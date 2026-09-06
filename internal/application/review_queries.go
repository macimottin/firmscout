package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/macimottin/firmscout/internal/domain"
)

// ReviewQueryDeps are the ports the reviewer's read side needs. They are read-only
// ports: nothing in this file writes, which is what lets the queue be served from a
// replica later without revisiting the question.
type ReviewQueryDeps struct {
	Reviews    ReviewRepository
	Candidates CandidateRepository
	Evidence   EvidenceRepository
	Sources    SourceRepository
	Products   ProductRepository
	Vendors    VendorRepository
	Conflicts  ConflictRepository
	Audit      AuditRepository
	Clock      Clock
}

// ReviewQueueQuery selects part of the queue. Slugs rather than ids, because a URL and a
// human both speak slugs.
type ReviewQueueQuery struct {
	States      []string
	Kinds       []string
	SLAClasses  []string
	VendorSlug  string
	ProductSlug string
	Limit       int
	Cursor      string
}

// Queue page bounds. These are the contract api.md §11 documents for the internal
// review queue -- "limit (default 50, max 200)" -- and this is where they are enforced,
// for every caller and not only for an HTTP request.
const (
	DefaultReviewPageSize = 50
	MaxReviewPageSize     = 200
)

// maxAuditEventsInDetail bounds how much decision history one item's detail carries. A
// review item is resolved once; a long trail here would mean something already went
// wrong with D5's "at most one item per open conflict" rule.
const maxAuditEventsInDetail = 20

// validReviewKinds mirrors the review_items.kind CHECK constraint
// (database/migrations/00001_initial.sql). It is declared here, not in ports.go, because
// filtering the queue is the only place that needs to reject an unrecognised value
// rather than simply storing or displaying one.
var validReviewKinds = map[string]bool{
	"candidate_low_confidence":         true,
	"candidate_implausible_transition": true,
	"product_match_ambiguous":          true,
	"multi_source_conflict":            true,
	"ai_proposal":                      true,
	"source_broken":                    true,
	"source_relocated":                 true,
	"community_correction":             true,
	"new_source_proposal":              true,
	"terms_review_required":            true,
}

// validReviewStates mirrors the review_items.state CHECK constraint.
var validReviewStates = map[string]bool{
	ReviewStateOpen:       true,
	ReviewStateInProgress: true,
	ReviewStateResolved:   true,
	ReviewStateDismissed:  true,
}

// ReviewQueueEntry is one row of the queue with the names a human needs to recognise it.
type ReviewQueueEntry struct {
	Item        ReviewItem
	VendorSlug  string
	VendorName  string
	ProductSlug string
	ProductName string
	// Age is how long the item has been waiting, computed at read time. It is not part
	// of the score (see domain.ScoreReview) but it is what a maintainer looks at first.
	Age time.Duration
}

// queueAge is how long an item has been waiting, floored at zero.
//
// The floor is not paranoia. created_at is written from the application's clock and read
// back against the reader's, and those are the same clock only until the queue is served
// from a replica, the reader runs on another host, or the host's clock is corrected
// backwards -- all three of which happen. A negative duration is never a true statement
// about how long somebody has been waiting, and rendering one ("-3m0s" in the CLI's age
// column) reads as a bug in whatever is displaying it rather than as clock skew.
func queueAge(now, createdAt time.Time) time.Duration {
	if age := now.Sub(createdAt); age > 0 {
		return age
	}
	return 0
}

// ReviewQueuePage is a page of the queue.
type ReviewQueuePage struct {
	Entries    []ReviewQueueEntry
	NextCursor string
}

// ListReviewQueue serves the queue a human reads.
type ListReviewQueue struct {
	deps ReviewQueryDeps
}

// NewListReviewQueue builds the use case.
func NewListReviewQueue(d ReviewQueryDeps) *ListReviewQueue { return &ListReviewQueue{deps: d} }

// Execute returns a page of the queue.
//
// An unrecognised state, kind or SLA class is rejected rather than silently producing
// an empty page: a reviewer who mistypes a filter must be told, not shown a queue that
// looks caught up. Resolving VendorSlug and ProductSlug to ids is different: a filter
// naming a vendor or product that does not (or no longer) exist simply matches nothing,
// because narrowing to a non-existent scope is a legitimate way to get zero results.
func (uc *ListReviewQueue) Execute(ctx context.Context, q ReviewQueueQuery) (ReviewQueuePage, error) {
	for _, s := range q.States {
		if !validReviewStates[s] {
			return ReviewQueuePage{}, fmt.Errorf("unknown review state %q: %w", s, domain.ErrValidation)
		}
	}
	for _, k := range q.Kinds {
		if !validReviewKinds[k] {
			return ReviewQueuePage{}, fmt.Errorf("unknown review kind %q: %w", k, domain.ErrValidation)
		}
	}
	for _, c := range q.SLAClasses {
		if !domain.ValidSLAClass(domain.SLAClass(c)) {
			return ReviewQueuePage{}, fmt.Errorf("unknown SLA class %q: %w", c, domain.ErrValidation)
		}
	}

	// Out of range in each direction is answered differently on purpose, matching
	// ListReleases: absent or negative means "give me the documented default", while
	// too large means "give me as much as you will allow". The single-branch clamp this
	// replaced handed a caller asking for 500 a page of 50 -- smaller than the maximum
	// they asked to approach, and a number api.md never promised for that input. It was
	// invisible through HTTP only because parseLimit clamps to the maximum first, which
	// is precisely the shape that lets a use case's real contract drift from its
	// documented one unnoticed.
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultReviewPageSize
	}
	if limit > MaxReviewPageSize {
		limit = MaxReviewPageSize
	}

	filter := ReviewQueueFilter{
		States:     q.States,
		Kinds:      q.Kinds,
		SLAClasses: q.SLAClasses,
		Limit:      limit,
		Cursor:     q.Cursor,
	}

	if slug := strings.TrimSpace(strings.ToLower(q.VendorSlug)); slug != "" {
		v, err := uc.deps.Vendors.GetBySlug(ctx, slug)
		switch {
		case err == nil:
			filter.VendorID = v.ID
		case isNotFound(err):
			return ReviewQueuePage{}, nil
		default:
			return ReviewQueuePage{}, fmt.Errorf("resolve vendor slug: %w", err)
		}
	}
	if slug := strings.TrimSpace(strings.ToLower(q.ProductSlug)); slug != "" {
		p, err := uc.deps.Products.GetBySlug(ctx, slug)
		switch {
		case err == nil:
			filter.ProductID = p.ID
		case isNotFound(err):
			return ReviewQueuePage{}, nil
		default:
			return ReviewQueuePage{}, fmt.Errorf("resolve product slug: %w", err)
		}
	}

	items, next, err := uc.deps.Reviews.List(ctx, filter)
	if err != nil {
		return ReviewQueuePage{}, fmt.Errorf("list review queue: %w", err)
	}

	now := uc.deps.Clock.Now()
	entries := make([]ReviewQueueEntry, 0, len(items))
	for _, item := range items {
		entry, err := resolveQueueEntry(ctx, uc.deps, item, now)
		if err != nil {
			return ReviewQueuePage{}, err
		}
		entries = append(entries, entry)
	}

	return ReviewQueuePage{Entries: entries, NextCursor: next}, nil
}

// resolveQueueEntry attaches the vendor and product names a human needs to recognise an
// item without a second lookup, and computes its age. It is shared by the list and
// detail use cases so the two surfaces cannot drift on what "resolved" means.
//
// A vendor or product that no longer resolves is not an error: review_items.vendor_id
// and .product_id are ON DELETE SET NULL, and an item queued against an entity that was
// since removed from the catalogue is still a decision someone must close out.
func resolveQueueEntry(ctx context.Context, deps ReviewQueryDeps, item ReviewItem, now time.Time) (ReviewQueueEntry, error) {
	entry := ReviewQueueEntry{Item: item, Age: queueAge(now, item.CreatedAt)}

	if item.VendorID != "" {
		v, err := deps.Vendors.GetByID(ctx, item.VendorID)
		switch {
		case err == nil:
			entry.VendorSlug = v.Slug
			entry.VendorName = v.Name
		case isNotFound(err):
		default:
			return ReviewQueueEntry{}, fmt.Errorf("resolve vendor %s: %w", item.VendorID, err)
		}
	}
	if item.ProductID != "" {
		p, err := deps.Products.GetByID(ctx, item.ProductID)
		switch {
		case err == nil:
			entry.ProductSlug = p.Slug
			entry.ProductName = p.Name
		case isNotFound(err):
		default:
			return ReviewQueueEntry{}, fmt.Errorf("resolve product %s: %w", item.ProductID, err)
		}
	}
	return entry, nil
}

// ReviewItemDetail is everything a reviewer needs to decide one item without opening a
// database client: the candidate, the gate verdicts that routed it, the evidence excerpt,
// the source it came from, the conflict it belongs to and what each source claims, and
// the decisions already recorded against it.
//
// Each optional part carries its own found flag rather than being a pointer, because
// "the evidence row is missing" and "the evidence row is empty" are different facts and a
// reviewer must be able to tell them apart.
type ReviewItemDetail struct {
	Entry ReviewQueueEntry

	Candidate      domain.CandidateRelease
	CandidateFound bool

	// Gates are the recorded gate verdicts in gate order. Without them the queue shows
	// that a candidate needs review but not which check said so.
	Gates []domain.GateResult

	Evidence      domain.Evidence
	EvidenceFound bool

	Source      domain.Source
	SourceFound bool

	Conflict      domain.SourceConflict
	ConflictFound bool
	// ConflictObservations is what each source currently claims for the product and
	// channel in dispute, loaded live rather than from the conflict row: a reviewer
	// deciding today needs today's claims.
	ConflictObservations []domain.SourceObservation

	Audit []AuditEvent
}

// GetReviewItem serves one queue item in full.
type GetReviewItem struct {
	deps ReviewQueryDeps
}

// NewGetReviewItem builds the use case.
func NewGetReviewItem(d ReviewQueryDeps) *GetReviewItem { return &GetReviewItem{deps: d} }

// Execute returns one item's detail, or domain.ErrNotFound.
//
// A missing part is never an error: a candidate whose evidence row was pruned, or a
// conflict that was resolved between the list and the detail request, still produces a
// usable page. Only the review item itself is required.
func (uc *GetReviewItem) Execute(ctx context.Context, id string) (ReviewItemDetail, error) {
	item, err := uc.deps.Reviews.GetByID(ctx, id)
	if err != nil {
		return ReviewItemDetail{}, err
	}

	now := uc.deps.Clock.Now()
	entry, err := resolveQueueEntry(ctx, uc.deps, item, now)
	if err != nil {
		return ReviewItemDetail{}, err
	}
	detail := ReviewItemDetail{Entry: entry}

	if item.SubjectType == SubjectTypeCandidateRelease {
		cand, err := uc.deps.Candidates.GetByID(ctx, item.SubjectID)
		switch {
		case err == nil:
			detail.Candidate = cand
			detail.CandidateFound = true
		case isNotFound(err):
		default:
			return ReviewItemDetail{}, fmt.Errorf("load candidate %s: %w", item.SubjectID, err)
		}

		gates, err := uc.deps.Candidates.ListValidationResults(ctx, item.SubjectID)
		switch {
		case err == nil:
			detail.Gates = gates
		case isNotFound(err):
		default:
			return ReviewItemDetail{}, fmt.Errorf("load gate results for %s: %w", item.SubjectID, err)
		}

		if detail.CandidateFound && detail.Candidate.EvidenceID != "" {
			ev, err := uc.deps.Evidence.GetByID(ctx, detail.Candidate.EvidenceID)
			switch {
			case err == nil:
				detail.Evidence = ev
				detail.EvidenceFound = true
			case isNotFound(err):
			default:
				return ReviewItemDetail{}, fmt.Errorf("load evidence %s: %w", detail.Candidate.EvidenceID, err)
			}
		}

		if detail.CandidateFound && detail.Candidate.SourceID != "" {
			src, err := uc.deps.Sources.GetByID(ctx, detail.Candidate.SourceID)
			switch {
			case err == nil:
				detail.Source = src
				detail.SourceFound = true
			case isNotFound(err):
			default:
				return ReviewItemDetail{}, fmt.Errorf("load source %s: %w", detail.Candidate.SourceID, err)
			}
		}
	}

	if conflictID := item.Payload[PayloadKeyConflictID]; conflictID != "" {
		conflict, err := uc.deps.Conflicts.GetConflict(ctx, conflictID)
		switch {
		case err == nil:
			detail.Conflict = conflict
			detail.ConflictFound = true
			obs, err := uc.deps.Conflicts.ObservationsForProduct(ctx, conflict.ProductID, conflict.Channel)
			if err != nil {
				return ReviewItemDetail{}, fmt.Errorf("load conflict observations for %s: %w", conflictID, err)
			}
			detail.ConflictObservations = obs
		case isNotFound(err):
		default:
			return ReviewItemDetail{}, fmt.Errorf("load conflict %s: %w", conflictID, err)
		}
	}

	audit, err := uc.deps.Audit.ListForSubject(ctx, subjectTypeReviewItem, item.ID, maxAuditEventsInDetail)
	if err != nil {
		return ReviewItemDetail{}, fmt.Errorf("load audit trail for %s: %w", item.ID, err)
	}
	detail.Audit = audit

	return detail, nil
}
