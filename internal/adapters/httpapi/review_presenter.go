package httpapi

import (
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// This file is the wire format of the internal review surface. It is a separate file
// from presenter.go for the same reason the routes are under /internal/ rather than
// /api/v1/: these shapes are not part of the public contract, they are deliberately
// absent from docs/api/openapi.yaml, and a client generator pointed at that document
// must never produce bindings for an off-by-default unauthenticated surface. See
// ADR-0021.
//
// Every date on every DTO below goes through RenderPartialDate. Nothing in this package
// constructs a date any other way, and the review surface is not an exception: a
// reviewer looking at "2026-02" and deciding whether the day is plausible must be shown
// the same "we do not know the day" the public API shows.

// ReviewItemDTO is one row of the queue.
//
// AgeSeconds is a number rather than a rendered duration because the client decides how
// to phrase "three days"; CreatedAt is sent alongside it so a client that would rather
// compute the age itself against its own clock can.
type ReviewItemDTO struct {
	ID            string      `json:"id"`
	Kind          string      `json:"kind"`
	State         string      `json:"state"`
	SLAClass      string      `json:"slaClass"`
	PriorityScore int         `json:"priorityScore"`
	Title         string      `json:"title"`
	Detail        string      `json:"detail,omitempty"`
	Subject       SubjectRef  `json:"subject"`
	Vendor        *VendorRef  `json:"vendor,omitempty"`
	Product       *ProductRef `json:"product,omitempty"`
	AgeSeconds    int64       `json:"ageSeconds"`
	CreatedAt     *Timestamp  `json:"createdAt,omitempty"`
	UpdatedAt     *Timestamp  `json:"updatedAt,omitempty"`
	ResolvedAt    *Timestamp  `json:"resolvedAt,omitempty"`
	ResolvedBy    string      `json:"resolvedBy,omitempty"`
	Resolution    string      `json:"resolution,omitempty"`
}

// SubjectRef names what an item is about. The type is carried alongside the id because
// review_items.subject_type is a vocabulary, not a prefix convention: a client that
// guessed the type from the id shape would break the first time a subject type without
// a prefixed id is queued.
type SubjectRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// ReviewQueueResponse is a page of the queue.
type ReviewQueueResponse struct {
	Items      []ReviewItemDTO `json:"items"`
	Pagination Pagination      `json:"pagination"`
}

// GateResultDTO is one recorded gate verdict.
//
// Order is sent because the gates are a sequence and their numbering is the vocabulary
// a maintainer uses to talk about them ("gate 10 routed it"); sorting the array on the
// client by anything else would renumber the conversation.
type GateResultDTO struct {
	Gate        string     `json:"gate"`
	Order       int        `json:"order"`
	Outcome     string     `json:"outcome"`
	Detail      string     `json:"detail,omitempty"`
	EvaluatedBy string     `json:"evaluatedBy"`
	EvaluatedAt *Timestamp `json:"evaluatedAt,omitempty"`
}

// ReviewCandidateDTO is the candidate under review.
type ReviewCandidateDTO struct {
	ID                   string  `json:"id"`
	RawVersion           string  `json:"rawVersion"`
	NormalizedVersion    string  `json:"normalizedVersion"`
	ReleaseType          string  `json:"releaseType"`
	Channel              string  `json:"channel,omitempty"`
	ReleaseDate          string  `json:"releaseDate,omitempty"`
	ReleaseDatePrecision string  `json:"releaseDatePrecision"`
	Confidence           float64 `json:"confidence"`
	State                string  `json:"state"`
	ProductMatchHint     string  `json:"productMatchHint,omitempty"`
	ReleaseNotesURL      string  `json:"releaseNotesUrl,omitempty"`
}

// ReviewSourceDTO is where the candidate came from. It carries the quality class as
// well as the official flag, because the authority ladder that decides whether a
// disagreement is a conflict at all reads the class, not the flag (ADR-0020), and a
// reviewer shown only "official: false" cannot tell an authorised portal from an
// anonymous forum post.
type ReviewSourceDTO struct {
	ID           string `json:"id"`
	Slug         string `json:"slug"`
	URL          string `json:"url"`
	Official     bool   `json:"official"`
	QualityClass string `json:"qualityClass"`
	Health       string `json:"health"`
}

// ConflictObservationDTO is what one source currently claims.
//
// Eligible is sent even though an ineligible source's claim never counts towards a
// conflict: a reviewer looking at two versions and one row marked ineligible is being
// told why the disagreement is one-sided, which is exactly the question they would
// otherwise open a database client to answer.
type ConflictObservationDTO struct {
	SourceID             string     `json:"sourceId"`
	QualityClass         string     `json:"qualityClass"`
	Official             bool       `json:"official"`
	Eligible             bool       `json:"eligible"`
	RawVersion           string     `json:"rawVersion"`
	ReleaseDate          string     `json:"releaseDate,omitempty"`
	ReleaseDatePrecision string     `json:"releaseDatePrecision"`
	ObservedAt           *Timestamp `json:"observedAt,omitempty"`
}

// ConflictDTO is the disagreement an item belongs to, with today's claims rather than
// the ones recorded when it was opened.
type ConflictDTO struct {
	ID            string                   `json:"id"`
	Channel       string                   `json:"channel,omitempty"`
	State         string                   `json:"state"`
	AuthorityRank int                      `json:"authorityRank"`
	Versions      []string                 `json:"versions"`
	Observations  []ConflictObservationDTO `json:"observations"`
	DetectedAt    *Timestamp               `json:"detectedAt,omitempty"`
}

// AuditEventDTO is one recorded decision.
//
// ActorAuthenticated is always false in this phase and is sent anyway. A client
// rendering an audit trail must not be able to present an asserted name as a verified
// one, and the only way to guarantee that is to hand it the fact rather than expecting
// it to know. See ADR-0021.
type AuditEventDTO struct {
	Actor              string     `json:"actor"`
	ActorType          string     `json:"actorType"`
	ActorAuthenticated bool       `json:"actorAuthenticated"`
	Action             string     `json:"action"`
	Reason             string     `json:"reason,omitempty"`
	OccurredAt         *Timestamp `json:"occurredAt,omitempty"`
}

// ReviewItemDetailResponse is everything a reviewer needs to decide one item.
//
// Candidate, Evidence, Source and Conflict are pointers without omitempty, so a missing
// part renders as an explicit null. A key that simply vanished would leave a reviewer
// unable to tell "the evidence row was pruned" from "this client version does not read
// evidence yet", and the first of those is a fact about the decision they are making.
type ReviewItemDetailResponse struct {
	Item      ReviewItemDTO       `json:"item"`
	Candidate *ReviewCandidateDTO `json:"candidate"`
	Gates     []GateResultDTO     `json:"gates"`
	Evidence  *EvidenceDTO        `json:"evidence"`
	Source    *ReviewSourceDTO    `json:"source"`
	Conflict  *ConflictDTO        `json:"conflict"`
	Audit     []AuditEventDTO     `json:"audit"`
}

// ReviewDecisionRequest is the body of an accept or a reject.
//
// A reason is the only member, and it is required. A decision with no stated reason is
// not an audit trail, it is a timestamp.
type ReviewDecisionRequest struct {
	Reason string `json:"reason"`
}

// ReviewDecisionResponse reports what a decision did.
type ReviewDecisionResponse struct {
	ItemID   string `json:"itemId"`
	Decision string `json:"decision"`
	Actor    string `json:"actor"`
	// ActorAuthenticated is always false in this phase and is sent anyway, so a client
	// rendering the audit trail cannot present an asserted name as a verified one.
	ActorAuthenticated bool       `json:"actorAuthenticated"`
	ReleaseID          string     `json:"releaseId,omitempty"`
	Published          bool       `json:"published"`
	ConflictID         string     `json:"conflictId,omitempty"`
	DecidedAt          *Timestamp `json:"decidedAt,omitempty"`
}

// PresentReviewItem renders one queue row.
func PresentReviewItem(e application.ReviewQueueEntry) ReviewItemDTO {
	item := e.Item
	dto := ReviewItemDTO{
		ID:            item.ID,
		Kind:          item.Kind,
		State:         item.State,
		SLAClass:      item.SLAClass,
		PriorityScore: item.PriorityScore,
		Title:         item.Title,
		Detail:        item.Detail,
		Subject:       SubjectRef{Type: item.SubjectType, ID: item.SubjectID},
		AgeSeconds:    int64(e.Age / time.Second),
		CreatedAt:     timestamp(item.CreatedAt),
		UpdatedAt:     timestamp(item.UpdatedAt),
		ResolvedAt:    timestamp(item.ResolvedAt),
		ResolvedBy:    item.ResolvedBy,
		Resolution:    item.Resolution,
	}
	// An item queued against a vendor or product that has since been removed from the
	// catalogue keeps its id but resolves to no name (review_items.vendor_id is ON
	// DELETE SET NULL). Rendering an empty {slug:"",name:""} would offer a reviewer a
	// link that goes nowhere; omitting the member says "we could not name it".
	if e.VendorSlug != "" || e.VendorName != "" {
		dto.Vendor = &VendorRef{Slug: e.VendorSlug, Name: e.VendorName}
	}
	if e.ProductSlug != "" || e.ProductName != "" {
		dto.Product = &ProductRef{Slug: e.ProductSlug, Name: e.ProductName}
	}
	return dto
}

// PresentReviewQueue renders a page of the queue.
func PresentReviewQueue(p application.ReviewQueuePage) ReviewQueueResponse {
	out := ReviewQueueResponse{
		Items:      make([]ReviewItemDTO, 0, len(p.Entries)),
		Pagination: pagination(p.NextCursor),
	}
	for _, e := range p.Entries {
		out.Items = append(out.Items, PresentReviewItem(e))
	}
	return out
}

// PresentReviewItemDetail renders one item in full.
func PresentReviewItemDetail(d application.ReviewItemDetail) ReviewItemDetailResponse {
	out := ReviewItemDetailResponse{
		Item:  PresentReviewItem(d.Entry),
		Gates: make([]GateResultDTO, 0, len(d.Gates)),
		Audit: make([]AuditEventDTO, 0, len(d.Audit)),
	}
	for _, g := range d.Gates {
		out.Gates = append(out.Gates, GateResultDTO{
			Gate:        string(g.Gate),
			Order:       g.Order,
			Outcome:     string(g.Outcome),
			Detail:      g.Detail,
			EvaluatedBy: g.EvaluatedBy,
			EvaluatedAt: timestamp(g.EvaluatedAt),
		})
	}
	if d.CandidateFound {
		out.Candidate = presentReviewCandidate(d.Candidate)
	}
	if d.EvidenceFound {
		out.Evidence = &EvidenceDTO{
			RetrievedAt: timestamp(d.Evidence.RetrievedAt),
			Excerpt:     d.Evidence.Excerpt,
		}
	}
	if d.SourceFound {
		out.Source = &ReviewSourceDTO{
			ID:           d.Source.ID,
			Slug:         d.Source.Slug,
			URL:          d.Source.URL,
			Official:     d.Source.Official,
			QualityClass: string(d.Source.QualityClass),
			Health:       string(d.Source.Health),
		}
	}
	if d.ConflictFound {
		out.Conflict = presentConflict(d.Conflict, d.ConflictObservations)
	}
	for _, e := range d.Audit {
		out.Audit = append(out.Audit, AuditEventDTO{
			Actor:              e.ActorID,
			ActorType:          e.ActorType,
			ActorAuthenticated: e.ActorAuthenticated,
			Action:             e.Action,
			Reason:             e.Reason,
			OccurredAt:         timestamp(e.OccurredAt),
		})
	}
	return out
}

func presentReviewCandidate(c domain.CandidateRelease) *ReviewCandidateDTO {
	date, precision := RenderPartialDate(c.ReleaseDate)
	return &ReviewCandidateDTO{
		ID:                   c.ID,
		RawVersion:           c.Version.Raw(),
		NormalizedVersion:    c.Version.Normalized(),
		ReleaseType:          string(c.ReleaseType),
		Channel:              c.Applicability.Channel,
		ReleaseDate:          date,
		ReleaseDatePrecision: precision,
		Confidence:           c.Confidence,
		State:                string(c.State),
		ProductMatchHint:     c.ProductMatchHint,
		ReleaseNotesURL:      c.ReleaseNotesURL,
	}
}

func presentConflict(c domain.SourceConflict, obs []domain.SourceObservation) *ConflictDTO {
	dto := &ConflictDTO{
		ID:            c.ID,
		Channel:       c.Channel,
		State:         string(c.State),
		AuthorityRank: c.AuthorityRank,
		// A non-nil empty slice, so a client iterating `versions` never has to branch
		// on null. The value is meaningful even when the array is empty: a conflict
		// whose participants were cleared is a repair signal, not an absent field.
		Versions:     append([]string{}, c.Versions...),
		Observations: make([]ConflictObservationDTO, 0, len(obs)),
		DetectedAt:   timestamp(c.DetectedAt),
	}
	for _, o := range obs {
		date, precision := RenderPartialDate(o.ReleaseDate)
		dto.Observations = append(dto.Observations, ConflictObservationDTO{
			SourceID:             o.SourceID,
			QualityClass:         string(o.QualityClass),
			Official:             o.Official,
			Eligible:             o.Eligible,
			RawVersion:           o.RawVersion,
			ReleaseDate:          date,
			ReleaseDatePrecision: precision,
			ObservedAt:           timestamp(o.ObservedAt),
		})
	}
	return dto
}

// PresentReviewDecision renders the outcome of an accept or a reject.
//
// The actor is echoed from the request rather than read back from the audit row,
// because the response is a receipt for what this call did: a reviewer who mistyped
// their own name must see the name that was recorded, not a tidied version of it.
func PresentReviewDecision(r application.ReviewDecisionResult, actor string) ReviewDecisionResponse {
	return ReviewDecisionResponse{
		ItemID:   r.ItemID,
		Decision: r.Decision,
		Actor:    actor,
		// Hardcoded false, matching what the use case recorded. See ADR-0021: the
		// platform did not verify this name and the response must not imply it did.
		ActorAuthenticated: false,
		ReleaseID:          r.ReleaseID,
		Published:          r.Published,
		ConflictID:         r.ConflictID,
		DecidedAt:          timestamp(r.DecidedAt),
	}
}
