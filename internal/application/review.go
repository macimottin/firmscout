package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/macimottin/firmscout/internal/domain"
)

// subjectTypeReviewItem is the audit_events.subject_type an audit row about a review
// decision carries. It is a constant rather than a literal because the same string has
// to match on both sides of a query -- the audit row names the review item, and the
// detail view reads the trail back by that name -- and a typo in one of them produces a
// row nothing ever finds. The review item's own subject types are declared in ports.go,
// where the reviewer-facing surfaces can reach them.
const subjectTypeReviewItem = "review_item"

// actorSystem is recorded as the actor for decisions the pipeline made on its own. A
// conflict the authority ladder closed was not closed by a person, and saying that it
// was would make the audit trail lie about the one thing it exists to record.
const actorSystem = "system"

// ReviewDeps are the ports a human decision needs.
type ReviewDeps struct {
	Reviews    ReviewRepository
	Candidates CandidateRepository
	Conflicts  ConflictRepository
	Audit      AuditRepository
	// Publisher is the same use case the worker runs. A human accepting an item
	// publishes through it rather than through a second path, so a hand-approved
	// release and an automatically published one are the same fact produced the same
	// way.
	Publisher *PublishRelease
	// Releases is used for exactly one thing: refreshing product_summaries after a
	// source-conflict item is resolved. Accepting or rejecting a candidate already
	// refreshes the summary via Publisher (ingest.go); resolving a conflict through
	// this path previously did not, which left HasSourceConflict and the detail
	// behind it (ProductSummary.Conflict's doc comment promises they are "derived
	// fresh... on every refresh") permanently stale for any product whose conflict a
	// human resolved, rather than one an automatic re-evaluation closed on its own.
	// Nil is tolerated the way Conflicts already is, for tests that exercise no
	// conflict path.
	Releases ReleaseRepository
	Events   EventPublisher
	UoW      UnitOfWork
	Clock    Clock
	IDs      IDGenerator
}

// DecideReviewItem is a human accepting or rejecting a queued decision.
//
// Accept and Reject each run in one unit of work: the review item is closed, the
// candidate is published or rejected, any conflict the item covers is resolved, and the
// audit row is written, or none of it happens. A published release whose review item is
// still open, or a closed item pointing at a release that was rolled back, is worse than
// a failed decision a reviewer can retry.
type DecideReviewItem struct {
	deps ReviewDeps
}

// NewDecideReviewItem builds the use case.
func NewDecideReviewItem(d ReviewDeps) *DecideReviewItem {
	return &DecideReviewItem{deps: d}
}

// ReviewDecisionInput is one reviewer's decision.
type ReviewDecisionInput struct {
	ItemID string
	// Actor is who is deciding. FirmScout has no login, so this is a name the caller
	// asserted and the platform did not verify; it is recorded as such. See ADR-0021.
	Actor string
	// ActorAuthenticated must be false until FirmScout has authentication. It is a
	// parameter rather than a hardcoded false so that the day authentication exists,
	// the audit trail distinguishes rows written before it from rows written after.
	ActorAuthenticated bool
	// Reason is required on both accept and reject. A decision with no stated reason is
	// not an audit trail, it is a timestamp.
	Reason    string
	RequestID string
	TraceID   string
}

// ReviewDecisionResult reports what the decision did.
type ReviewDecisionResult struct {
	ItemID      string
	SubjectType string
	SubjectID   string
	// Decision is "accepted" or "rejected".
	Decision   string
	ReleaseID  string
	Published  bool
	ConflictID string
	DecidedAt  time.Time
}

// Decisions recorded in review_items.resolution and in the audit trail.
const (
	ReviewDecisionAccepted = "accepted"
	ReviewDecisionRejected = "rejected"
)

// Accept resolves the item and publishes its candidate, overriding the gate that routed
// it to review.
//
// This is the one path by which a candidate that failed a gate becomes a published
// release, and the override is recorded rather than silent: the gate verdicts stay in
// validation_results, the review item records who accepted it, and audit_events records
// the same decision with the reason and the request id.
func (uc *DecideReviewItem) Accept(ctx context.Context, in ReviewDecisionInput) (ReviewDecisionResult, error) {
	return uc.decide(ctx, in, ReviewDecisionAccepted)
}

// Reject resolves the item and moves its candidate to rejected, through the domain state
// machine rather than by writing the state directly.
func (uc *DecideReviewItem) Reject(ctx context.Context, in ReviewDecisionInput) (ReviewDecisionResult, error) {
	return uc.decide(ctx, in, ReviewDecisionRejected)
}

// decide is the shared body of Accept and Reject. They differ in exactly one step --
// what happens to the candidate -- and writing the transaction, the guards and the audit
// row twice would be two places for the atomicity rule to drift apart.
func (uc *DecideReviewItem) decide(ctx context.Context, in ReviewDecisionInput, decision string) (ReviewDecisionResult, error) {
	now := uc.deps.Clock.Now()

	if strings.TrimSpace(in.Actor) == "" {
		return ReviewDecisionResult{}, fmt.Errorf("review decision needs an actor: %w", domain.ErrValidation)
	}
	if strings.TrimSpace(in.Reason) == "" {
		return ReviewDecisionResult{}, fmt.Errorf("review decision needs a reason: %w", domain.ErrValidation)
	}

	item, err := uc.deps.Reviews.GetByID(ctx, in.ItemID)
	if err != nil {
		return ReviewDecisionResult{}, fmt.Errorf("load review item: %w", err)
	}
	// The repository's own WHERE clause is the race guard; this check exists so that a
	// reviewer who opens a page somebody else already decided gets an answer that names
	// the reason rather than a bare "no rows updated".
	if item.State == ReviewStateResolved || item.State == ReviewStateDismissed {
		return ReviewDecisionResult{}, fmt.Errorf("review item %s is already %s: %w", item.ID, item.State, domain.ErrConflict)
	}

	out := ReviewDecisionResult{
		ItemID:      item.ID,
		SubjectType: item.SubjectType,
		SubjectID:   item.SubjectID,
		Decision:    decision,
		DecidedAt:   now,
	}

	resolution := decision + ": " + strings.TrimSpace(in.Reason)
	beforeCandidateState, _ := uc.candidateState(ctx, item)
	afterCandidateState := beforeCandidateState

	err = uc.deps.UoW.Within(ctx, func(ctx context.Context) error {
		if err := uc.deps.Reviews.Resolve(ctx, item.ID, resolution, in.Actor, now); err != nil {
			return fmt.Errorf("resolve review item: %w", err)
		}

		if item.SubjectType == SubjectTypeCandidateRelease {
			switch decision {
			case ReviewDecisionAccepted:
				if uc.deps.Publisher == nil {
					return fmt.Errorf("accepting a candidate needs the publication use case: %w", domain.ErrValidation)
				}
				res, err := uc.deps.Publisher.Execute(ctx, item.SubjectID)
				if err != nil {
					return fmt.Errorf("publish accepted candidate: %w", err)
				}
				out.ReleaseID = res.ReleaseID
				out.Published = res.Published
				afterCandidateState = string(domain.CandidatePublished)
			default:
				candidate, err := uc.deps.Candidates.GetByID(ctx, item.SubjectID)
				if err != nil {
					return fmt.Errorf("load candidate: %w", err)
				}
				// Through the state machine rather than by writing the column: an
				// edge that does not exist must fail here, not become a row whose
				// state nothing can interpret.
				if err := candidate.TransitionTo(domain.CandidateRejected); err != nil {
					return fmt.Errorf("reject candidate: %w", err)
				}
				if err := uc.deps.Candidates.UpdateState(ctx, candidate.ID, domain.CandidateRejected, resolution); err != nil {
					return fmt.Errorf("update candidate state: %w", err)
				}
				afterCandidateState = string(domain.CandidateRejected)
			}
		}

		if conflictID := item.Payload[PayloadKeyConflictID]; conflictID != "" && uc.deps.Conflicts != nil {
			if err := uc.deps.Conflicts.ResolveConflict(ctx, conflictID, resolution, in.Actor, now); err != nil {
				return fmt.Errorf("resolve conflict: %w", err)
			}
			out.ConflictID = conflictID
			// The summary's HasSourceConflict and Conflict fields are only ever as
			// fresh as the last refresh; resolving the conflict here changes the fact
			// on disk but nothing re-derives the row from it otherwise. Without this,
			// a product whose conflict a human resolved keeps showing the disagreement
			// as open, with its stale channel/versions/timestamp, indefinitely -- the
			// same disagreement can never surface again to trigger a fresh refresh
			// unless a new one is independently detected on that exact channel.
			if uc.deps.Releases != nil && item.ProductID != "" {
				if err := uc.deps.Releases.RefreshProductSummary(ctx, item.ProductID); err != nil {
					return fmt.Errorf("refresh product summary: %w", err)
				}
			}
		}

		return uc.deps.Audit.Record(ctx, AuditEvent{
			ID:        uc.deps.IDs.NewID("aud"),
			ActorType: ActorTypeHuman,
			ActorID:   in.Actor,
			// False in this phase, always. See ADR-0021: the platform did not verify
			// this name, and a row that claimed otherwise would be the audit trail
			// lying about the thing it exists to record.
			ActorAuthenticated: in.ActorAuthenticated,
			Action:             auditActionFor(decision),
			SubjectType:        subjectTypeReviewItem,
			SubjectID:          item.ID,
			BeforeState: map[string]string{
				"review_state":    stateOrOpen(item.State),
				"candidate_state": beforeCandidateState,
			},
			AfterState: map[string]string{
				"review_state":    ReviewStateResolved,
				"candidate_state": afterCandidateState,
				"release_id":      out.ReleaseID,
			},
			Reason:     strings.TrimSpace(in.Reason),
			RequestID:  in.RequestID,
			TraceID:    in.TraceID,
			OccurredAt: now,
		})
	})
	if err != nil {
		return ReviewDecisionResult{}, err
	}

	// Published after the commit, not inside it: an event announcing a decision that
	// was rolled back is worse than an event that arrives a moment late.
	if err := uc.deps.Events.Publish(ctx,
		domain.NewEvent(domain.EventReviewItemResolved, now, subjectTypeReviewItem, item.ID).
			With("decision", decision).
			With("kind", item.Kind)); err != nil {
		return out, fmt.Errorf("publish event: %w", err)
	}

	return out, nil
}

// candidateState reads the candidate's state for the audit row's before-image. A
// candidate that cannot be loaded yields the empty string rather than an error: the
// decision is about the review item, and failing it because the before-image is
// incomplete would refuse a correct decision over a cosmetic field.
func (uc *DecideReviewItem) candidateState(ctx context.Context, item ReviewItem) (string, error) {
	if item.SubjectType != SubjectTypeCandidateRelease || uc.deps.Candidates == nil {
		return "", nil
	}
	candidate, err := uc.deps.Candidates.GetByID(ctx, item.SubjectID)
	if err != nil {
		return "", err
	}
	return string(candidate.State), nil
}

func auditActionFor(decision string) string {
	if decision == ReviewDecisionAccepted {
		return AuditActionReviewAccepted
	}
	return AuditActionReviewRejected
}

// stateOrOpen renders a review item's state for the audit before-image. An item stored
// without a state predates the state column being written and is open by definition;
// recording an empty string would leave the before-image unreadable.
func stateOrOpen(state string) string {
	if strings.TrimSpace(state) == "" {
		return ReviewStateOpen
	}
	return state
}
