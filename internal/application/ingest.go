package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/macimottin/firmscout/internal/domain"
)

// IngestDeps are the ports the extraction, validation and publication use cases need.
type IngestDeps struct {
	Sources    SourceRepository
	Products   ProductRepository
	Candidates CandidateRepository
	Releases   ReleaseRepository
	Evidence   EvidenceRepository
	Reviews    ReviewRepository
	// Conflicts holds what each source currently claims and the disagreements that
	// follow. When it is nil the conflict projection is simply not wired: no
	// observation is recorded, no conflict is opened, and gate 10 can then only ever
	// pass. That is a wiring defect rather than a policy, which is why it is stated
	// here rather than silently tolerated in the middle of validation.
	Conflicts ConflictRepository
	Audit     AuditRepository
	Artifacts ArtifactStore
	Registry  CollectorRegistry
	Queue     JobQueue
	Events    EventPublisher
	UoW       UnitOfWork
	Clock     Clock
	IDs       IDGenerator

	// ConfidenceThreshold is the minimum confidence for automatic publication from an
	// official source.
	ConfidenceThreshold float64
	// FutureDateTolerance bounds how far ahead a release date may be dated.
	FutureDateTolerance time.Duration
	// EarliestPlausibleDate rejects absurdly old dates.
	EarliestPlausibleDate time.Time
}

func (d *IngestDeps) defaults() {
	if d.ConfidenceThreshold <= 0 {
		d.ConfidenceThreshold = domain.DefaultConfidenceThreshold
	}
	if d.FutureDateTolerance <= 0 {
		d.FutureDateTolerance = 48 * time.Hour
	}
	if d.EarliestPlausibleDate.IsZero() {
		d.EarliestPlausibleDate = time.Date(1990, time.January, 1, 0, 0, 0, 0, time.UTC)
	}
}

// ---------------------------------------------------------------------------
// Extraction
// ---------------------------------------------------------------------------

// ExtractCandidates runs a collector over a stored artifact and records the candidates
// it produces.
//
// The collector receives no repository handle. It cannot write a release even if its
// author wanted it to; the only thing it can return is a slice of candidates. See
// ADR-0005.
type ExtractCandidates struct {
	deps IngestDeps
}

// NewExtractCandidates builds the use case.
func NewExtractCandidates(d IngestDeps) *ExtractCandidates {
	d.defaults()
	return &ExtractCandidates{deps: d}
}

// ExtractResult reports what extraction found.
type ExtractResult struct {
	SourceID      string
	CollectorID   string
	Extracted     int
	NewCandidates int
	Duplicates    int
	CandidateIDs  []string
}

// Execute extracts candidates from an artifact and persists the new ones.
//
// Re-running this against the same artifact is safe: candidates are keyed by
// (source, dedupe key), so a repeated run updates rather than duplicates. That is what
// makes the job queue's at-least-once delivery acceptable.
func (uc *ExtractCandidates) Execute(ctx context.Context, sourceID, artifactID string) (ExtractResult, error) {
	now := uc.deps.Clock.Now()

	src, err := uc.deps.Sources.GetByID(ctx, sourceID)
	if err != nil {
		return ExtractResult{}, fmt.Errorf("load source: %w", err)
	}

	collector, err := uc.deps.Registry.For(src)
	if err != nil {
		return ExtractResult{}, fmt.Errorf("resolve collector for source %s: %w", sourceID, err)
	}

	body, err := readArtifact(ctx, uc.deps.Artifacts, artifactID)
	if err != nil {
		return ExtractResult{}, fmt.Errorf("read artifact: %w", err)
	}

	art := Artifact{
		ID:          artifactID,
		SourceID:    src.ID,
		ContentType: src.ExpectedContentType,
		Body:        body,
		RetrievedAt: now,
		URL:         src.URL,
	}

	candidates, err := collector.Extract(ctx, src, art)
	if err != nil {
		_ = uc.deps.Events.Publish(ctx, domain.NewEvent(domain.EventCollectorFailed, now, "source", src.ID).
			With("collector_id", collector.ID()).
			With("error", err.Error()))
		return ExtractResult{}, fmt.Errorf("extract: %w", err)
	}

	out := ExtractResult{SourceID: src.ID, CollectorID: collector.ID(), Extracted: len(candidates)}

	for i := range candidates {
		c := candidates[i]

		// Evidence first: a candidate without provenance is not persisted at all.
		ev := domain.Evidence{
			ID:               uc.deps.IDs.NewID("evd"),
			SourceID:         src.ID,
			SourceURL:        src.URL,
			SourceType:       src.SourceType,
			Official:         src.Official,
			RetrievedAt:      now,
			ArtifactID:       artifactID,
			Excerpt:          domain.TruncateExcerpt(firstNonEmpty(c.ProductMatchHint, c.Version.Raw())),
			RawValue:         c.Version.Raw(),
			NormalizedValue:  c.Version.Normalized(),
			CollectorID:      collector.ID(),
			CollectorVersion: collector.Version(),
			DiscoveryMethod:  domain.DiscoveryDeterministic,
			Confidence:       c.Confidence,
			CreatedAt:        now,
		}
		if c.EvidenceID != "" {
			// A collector may supply a richer excerpt; keep it.
			ev.Excerpt = domain.TruncateExcerpt(c.EvidenceID)
			c.EvidenceID = ""
		}
		if err := ev.Validate(); err != nil {
			return out, fmt.Errorf("candidate %d evidence: %w", i, err)
		}
		if err := uc.deps.Evidence.Insert(ctx, ev); err != nil {
			return out, fmt.Errorf("insert evidence: %w", err)
		}

		c.ID = uc.deps.IDs.NewID("cnd")
		c.SourceID = src.ID
		c.EvidenceID = ev.ID
		c.DiscoveredAt = now
		c.UpdatedAt = now
		if c.State == "" {
			c.State = domain.CandidateDiscovered
		}
		if c.DedupeKey == "" {
			c.DedupeKey = domain.ComputeDedupeKey(c.ProductMatchHint, c.Version, c.Applicability)
		}
		if err := c.TransitionTo(domain.CandidateExtracted); err != nil {
			return out, fmt.Errorf("candidate %d: %w", i, err)
		}

		if err := c.Validate(); err != nil {
			return out, fmt.Errorf("candidate %d: %w", i, err)
		}

		stored, created, err := uc.deps.Candidates.UpsertByDedupeKey(ctx, c)
		if err != nil {
			return out, fmt.Errorf("store candidate: %w", err)
		}
		out.CandidateIDs = append(out.CandidateIDs, stored.ID)
		if created {
			out.NewCandidates++
			if err := uc.deps.Events.Publish(ctx,
				domain.NewEvent(domain.EventCandidateCreated, now, "candidate", stored.ID).
					With("source_id", src.ID).
					With("version", c.Version.Raw())); err != nil {
				return out, fmt.Errorf("publish candidate event: %w", err)
			}
			if err := uc.deps.Queue.Enqueue(ctx, Job{
				ID:             uc.deps.IDs.NewID("job"),
				Kind:           JobValidateCandidate,
				IdempotencyKey: "validate:" + stored.ID,
				Payload:        map[string]string{"candidate_id": stored.ID},
				RunAfter:       now,
			}); err != nil {
				return out, fmt.Errorf("enqueue validation: %w", err)
			}
		} else {
			out.Duplicates++
		}
	}

	return out, nil
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// ValidateCandidate runs the ten deterministic gates and routes the candidate to
// publication, review or rejection.
type ValidateCandidate struct {
	deps IngestDeps
}

// NewValidateCandidate builds the use case.
func NewValidateCandidate(d IngestDeps) *ValidateCandidate {
	d.defaults()
	return &ValidateCandidate{deps: d}
}

// ValidateResult reports the verdict.
type ValidateResult struct {
	CandidateID string
	Decision    domain.GateOutcome
	Reason      string
	Verdict     domain.ValidationVerdict
	// PublicationEnqueued reports whether the candidate was cleared to publish.
	PublicationEnqueued bool
	// ReviewItemID names the open queue item covering this run's finding. It is set
	// whenever a human decision is outstanding -- which includes a candidate rejected
	// as a duplicate that nonetheless exposed a disagreement between sources, because
	// the disagreement still needs answering even though this candidate does not.
	ReviewItemID string
	// ConflictID names the open multi-source disagreement this candidate belongs to,
	// when there is one. It is set even for a candidate that was rejected or
	// published, because the disagreement is a fact about the product rather than
	// about this candidate's fate.
	ConflictID string
}

// Execute validates one candidate.
//
// Every write it makes runs in one unit of work, and it is safe to run twice on the same
// candidate. Both properties are load-bearing rather than tidy: the job queue delivers at
// least once, so a validation that files a review item, fails while linking it to the
// conflict and is then retried used to leave one orphaned item behind per attempt --
// exactly what the "one disagreement, one queue item" rule was written to prevent.
//
// Events are collected and published after the commit, for the reason PublishRelease
// gives: an event announcing a decision that was rolled back is worse than one that
// arrives a moment late.
func (uc *ValidateCandidate) Execute(ctx context.Context, candidateID string) (ValidateResult, error) {
	now := uc.deps.Clock.Now()

	c, err := uc.deps.Candidates.GetByID(ctx, candidateID)
	if err != nil {
		return ValidateResult{}, fmt.Errorf("load candidate: %w", err)
	}
	src, err := uc.deps.Sources.GetByID(ctx, c.SourceID)
	if err != nil {
		return ValidateResult{}, fmt.Errorf("load source: %w", err)
	}

	vctx, assessment, err := uc.buildContext(ctx, c, src, now)
	if err != nil {
		return ValidateResult{}, err
	}

	verdict := domain.RunValidationGates(c, vctx)

	out := ValidateResult{
		CandidateID: c.ID,
		Decision:    verdict.Decision,
		Reason:      verdict.Reason,
		Verdict:     verdict,
	}

	var pending []domain.Event

	err = uc.deps.UoW.Within(ctx, func(ctx context.Context) error {
		// A candidate that arrived without a product but resolved to exactly one gets
		// the match persisted here, so publication reads a settled fact rather than
		// repeating the resolution and risking a different answer.
		if c.ProductID == "" && vctx.ResolvedProductID != "" {
			c.ProductID = vctx.ResolvedProductID
			c.ProductMatchStatus = domain.MatchUnique
			if err := uc.deps.Candidates.SetResolvedProduct(ctx, c.ID, c.ProductID); err != nil {
				return fmt.Errorf("record product match: %w", err)
			}
		}

		if err := uc.deps.Candidates.RecordValidation(ctx, c.ID, verdict.Results); err != nil {
			return fmt.Errorf("record validation: %w", err)
		}

		// The observation and the conflict are recorded regardless of the verdict. A
		// candidate rejected as a duplicate is still what its source currently claims,
		// and a candidate routed to review is exactly the case a disagreement has to be
		// visible for.
		outcome, err := uc.reconcileConflict(ctx, c, assessment, now, &pending)
		if err != nil {
			return err
		}
		out.ConflictID = outcome.ConflictID

		// A duplicate is not a failure. The right response is to confirm the existing
		// release is still current rather than publish a second identical row.
		if vctx.DuplicateReleaseID != "" {
			if err := uc.deps.Releases.TouchVerified(ctx, vctx.DuplicateReleaseID, now); err != nil {
				return fmt.Errorf("refresh verification: %w", err)
			}
		}

		switch verdict.Decision {
		case domain.GatePassed:
			moved, err := uc.transition(ctx, &c, domain.CandidateValidated, "")
			if err != nil {
				return err
			}
			if moved {
				pending = append(pending, domain.NewEvent(domain.EventCandidateValidated, now, "candidate", c.ID))
			}
			if err := uc.deps.Queue.Enqueue(ctx, Job{
				ID:             uc.deps.IDs.NewID("job"),
				Kind:           JobPublishRelease,
				IdempotencyKey: "publish:" + c.ID,
				Payload:        map[string]string{"candidate_id": c.ID},
				RunAfter:       now,
			}); err != nil {
				return fmt.Errorf("enqueue publication: %w", err)
			}
			out.PublicationEnqueued = true

		case domain.GateReviewRequired:
			moved, err := uc.transition(ctx, &c, domain.CandidateHumanReviewRequired, verdict.Reason)
			if err != nil {
				return err
			}
			if moved {
				pending = append(pending,
					domain.NewEvent(domain.EventHumanReviewRequested, now, "candidate", c.ID).
						With("reason", verdict.Reason))
			}

		default:
			moved, err := uc.transition(ctx, &c, domain.CandidateRejected, verdict.Reason)
			if err != nil {
				return err
			}
			if moved {
				pending = append(pending,
					domain.NewEvent(domain.EventCandidateRejected, now, "candidate", c.ID).
						With("reason", verdict.Reason))
			}
		}

		// The queue item is filed outside the switch because the two conditions that
		// need one are independent. A candidate routed to review needs one because a
		// human must decide its fate; an unresolved disagreement needs one because
		// ADR-0020 says a disagreement reaches a human, and gate 4's short circuit means
		// the candidate that exposed it can perfectly well have been rejected as a
		// duplicate of an already-published release.
		if verdict.Decision == domain.GateReviewRequired || outcome.ConflictID != "" {
			itemID, err := uc.routeToReview(ctx, c, src, verdict, assessment, outcome, now, &pending)
			if err != nil {
				return err
			}
			out.ReviewItemID = itemID
		}
		return nil
	})
	if err != nil {
		return out, err
	}

	if len(pending) > 0 {
		if err := uc.deps.Events.Publish(ctx, pending...); err != nil {
			return out, fmt.Errorf("publish event: %w", err)
		}
	}
	return out, nil
}

// routeToReview keeps exactly one open queue item pointed at the decision a human
// actually has to make, and returns its id.
//
// Three rules meet here, and the previous shape satisfied only the first.
//
//  1. One disagreement, one queue item. A re-check of both sources must not file another
//     copy of the same conflict, or a queue whose value depends on staying short is
//     unusable within a week.
//
//  2. That item must describe the disagreement as it stands now. The old code reused any
//     item already linked to an open conflict without checking that its subject was still
//     one of the versions in dispute. When a source moved from 7.24.1 to 7.24.5 the item
//     went on naming 7.24.1, the new candidate was left in human_review_required with
//     nothing in the queue referencing it, and accepting the item published a version no
//     source claimed and that was older than both live claims.
//
//  3. Filing it must be idempotent. The job queue delivers at least once, and
//     Reviews.Create is a plain insert.
func (uc *ValidateCandidate) routeToReview(
	ctx context.Context,
	c domain.CandidateRelease,
	src domain.Source,
	verdict domain.ValidationVerdict,
	a conflictAssessment,
	outcome conflictOutcome,
	now time.Time,
	pending *[]domain.Event,
) (string, error) {
	desired := uc.desiredReviewItem(ctx, c, src, verdict, a, outcome, now)

	existing, found, err := uc.openReviewItemFor(ctx, outcome, desired)
	if err != nil {
		return "", err
	}

	if !found {
		desired.ID = uc.deps.IDs.NewID("rev")
		if err := uc.deps.Reviews.Create(ctx, desired); err != nil {
			return "", fmt.Errorf("create review item: %w", err)
		}
		if err := uc.linkReviewItem(ctx, outcome, desired.ID); err != nil {
			return "", err
		}
		return desired.ID, nil
	}

	// The description always follows the current dispute; the subject moves only when
	// the one it names has dropped out of it, so a reviewer's page does not change
	// underneath them on every scheduler cycle.
	superseded := ""
	if !stillInDispute(existing, desired, c, a) {
		// Repointing is only safe onto a subject that has no open item of its own.
		// review_items_open_subject_idx permits exactly one, so attempting the second
		// would turn this validation into a job that can never succeed -- strictly worse
		// than a queue carrying one extra row. Where the subject already has an item,
		// that item is the one to keep, and the stale one is left for its own source's
		// next check to move.
		replacement, hasReplacement, err := uc.openReviewItemForSubject(ctx, desired)
		if err != nil {
			return "", err
		}
		if hasReplacement && replacement.ID != existing.ID {
			existing = replacement
		} else {
			if existing.SubjectType == SubjectTypeCandidateRelease {
				superseded = existing.SubjectID
			}
			existing.Kind = desired.Kind
			existing.SubjectType = desired.SubjectType
			existing.SubjectID = desired.SubjectID
			existing.Title = desired.Title
		}
	}
	existing.VendorID = desired.VendorID
	existing.ProductID = desired.ProductID
	existing.Detail = desired.Detail
	existing.Payload = desired.Payload
	existing.PriorityScore = desired.PriorityScore
	existing.SLAClass = desired.SLAClass

	if err := uc.deps.Reviews.Retarget(ctx, existing); err != nil {
		return "", fmt.Errorf("retarget review item %s: %w", existing.ID, err)
	}
	if err := uc.linkReviewItem(ctx, outcome, existing.ID); err != nil {
		return "", err
	}
	if err := uc.releaseSuperseded(ctx, superseded, existing.ID, now, pending); err != nil {
		return "", err
	}
	return existing.ID, nil
}

// desiredReviewItem is the item this run would file if the queue were empty.
func (uc *ValidateCandidate) desiredReviewItem(
	ctx context.Context,
	c domain.CandidateRelease,
	src domain.Source,
	verdict domain.ValidationVerdict,
	a conflictAssessment,
	outcome conflictOutcome,
	now time.Time,
) ReviewItem {
	conflicted := a.Verdict.Status == domain.ConflictUnresolved
	priority := domain.ScoreReview(uc.reviewSignals(ctx, c, src, verdict, conflicted))

	item := ReviewItem{
		Kind:          reviewKindFor(verdict),
		SubjectType:   SubjectTypeCandidateRelease,
		SubjectID:     c.ID,
		VendorID:      src.VendorID,
		ProductID:     c.ProductID,
		Title:         "Candidate " + c.Version.Raw() + " needs review",
		Detail:        verdict.Reason,
		Payload:       reviewPayload(verdict, outcome, a),
		PriorityScore: priority.Score,
		SLAClass:      string(priority.SLA),
		State:         ReviewStateOpen,
		CreatedAt:     now,
	}

	if verdict.Decision == domain.GateReviewRequired {
		return item
	}

	// The candidate's own fate is settled -- it was rejected, most often as a duplicate
	// of a release that is already published -- so there is nothing here for a reviewer
	// to accept. The decision that remains is about the disagreement, and the item has to
	// say so: one whose subject is a rejected candidate would offer an accept button that
	// PublishRelease is bound to refuse.
	item.Kind = reviewKindMultiSourceConflict
	item.SubjectType = SubjectTypeSourceConflict
	item.SubjectID = outcome.ConflictID
	item.Title = "Sources disagree about " + strings.Join(
		domain.MergeParticipants(a.Subject.NormalizedVersion, a.Verdict.ConflictingVersions), " and ")
	item.Detail = a.Verdict.Reason
	return item
}

// openReviewItemFor finds the item this run should update rather than duplicate: the one
// already linked to the open conflict, or failing that the one already waiting on a
// decision about the same subject.
//
// A linked item that has since been closed is treated as absent. A human who resolved it
// answered the question it asked; a disagreement that is still open after that is a new
// question and deserves a new row rather than a reopened one.
func (uc *ValidateCandidate) openReviewItemFor(ctx context.Context, outcome conflictOutcome, desired ReviewItem) (ReviewItem, bool, error) {
	if id := outcome.ExistingReviewItemID; id != "" {
		item, err := uc.deps.Reviews.GetByID(ctx, id)
		switch {
		case err == nil:
			if item.State == ReviewStateOpen || item.State == ReviewStateInProgress {
				return item, true, nil
			}
		case isNotFound(err):
		default:
			return ReviewItem{}, false, fmt.Errorf("load linked review item %s: %w", id, err)
		}
	}

	return uc.openReviewItemForSubject(ctx, desired)
}

// openReviewItemForSubject returns the open item already naming a subject, if any.
func (uc *ValidateCandidate) openReviewItemForSubject(ctx context.Context, desired ReviewItem) (ReviewItem, bool, error) {
	item, err := uc.deps.Reviews.FindOpenBySubject(ctx, desired.SubjectType, desired.SubjectID)
	switch {
	case err == nil:
		return item, true, nil
	case isNotFound(err):
		return ReviewItem{}, false, nil
	default:
		return ReviewItem{}, false, fmt.Errorf("find open review item for %s %s: %w",
			desired.SubjectType, desired.SubjectID, err)
	}
}

func (uc *ValidateCandidate) linkReviewItem(ctx context.Context, outcome conflictOutcome, itemID string) error {
	if outcome.ConflictID == "" || outcome.ExistingReviewItemID == itemID {
		return nil
	}
	if err := uc.deps.Conflicts.LinkReviewItem(ctx, outcome.ConflictID, itemID); err != nil {
		return fmt.Errorf("link review item to conflict: %w", err)
	}
	return nil
}

// releaseSuperseded closes out a candidate the queue has just stopped naming.
//
// It is what makes "no candidate sits in human_review_required with nothing in the queue
// referencing it" true rather than aspirational. The candidate is not being judged: its
// own source has stopped reporting that version, so nobody is asserting it any more and
// there is nothing left for a human to decide about it. Leaving it parked would be a row
// no queue reaches and no reviewer can close.
func (uc *ValidateCandidate) releaseSuperseded(ctx context.Context, candidateID, itemID string, now time.Time, pending *[]domain.Event) error {
	if candidateID == "" {
		return nil
	}
	superseded, err := uc.deps.Candidates.GetByID(ctx, candidateID)
	switch {
	case err == nil:
	case isNotFound(err):
		return nil
	default:
		return fmt.Errorf("load superseded candidate %s: %w", candidateID, err)
	}
	if superseded.State != domain.CandidateHumanReviewRequired {
		return nil
	}

	reason := "its source no longer reports this version; review item " + itemID +
		" now tracks the current disagreement"
	if err := superseded.TransitionTo(domain.CandidateRejected); err != nil {
		return fmt.Errorf("release superseded candidate %s: %w", candidateID, err)
	}
	if err := uc.deps.Candidates.UpdateState(ctx, candidateID, domain.CandidateRejected, reason); err != nil {
		return fmt.Errorf("update superseded candidate state: %w", err)
	}
	*pending = append(*pending,
		domain.NewEvent(domain.EventCandidateRejected, now, "candidate", candidateID).
			With("reason", reason))
	return nil
}

// stillInDispute reports whether an open item's subject is still part of the
// disagreement, and so whether repointing it would take a reviewer's page away from a
// live question.
//
// A candidate subject counts while some eligible source's current observation still names
// it -- that is precisely the set domain.AssessSourceConflict compares against, read from
// the observations already loaded rather than queried again. A conflict subject stays
// accurate for as long as the conflict is open, but yields as soon as there is a
// candidate a reviewer could act on, because "accept this version" is a decision somebody
// can make and "the sources disagree" is only a description.
func stillInDispute(item, desired ReviewItem, c domain.CandidateRelease, a conflictAssessment) bool {
	if item.SubjectType != SubjectTypeCandidateRelease {
		return desired.SubjectType != SubjectTypeCandidateRelease
	}
	if item.SubjectID == c.ID {
		return true
	}
	for _, o := range a.Observations {
		if o.SourceID == c.SourceID || !o.Eligible || o.CandidateID == "" {
			continue
		}
		if o.CandidateID == item.SubjectID {
			return true
		}
	}
	return false
}

// transition walks the candidate to a state and reports whether it actually moved.
//
// The bool is what keeps the event stream honest under at-least-once delivery: a job
// delivered three times validates the same candidate three times, and announcing "human
// review requested" on each of them would describe three requests where there was one.
func (uc *ValidateCandidate) transition(ctx context.Context, c *domain.CandidateRelease, to domain.CandidateState, reason string) (bool, error) {
	// The candidate may arrive in `extracted`; move it through the intermediate
	// states so the recorded history is complete rather than jumping edges.
	steps := pathTo(c.State, to)
	for _, step := range steps {
		if err := c.TransitionTo(step); err != nil {
			return false, fmt.Errorf("candidate %s: %w", c.ID, err)
		}
		if err := uc.deps.Candidates.UpdateState(ctx, c.ID, step, reason); err != nil {
			return false, fmt.Errorf("update candidate state: %w", err)
		}
	}
	return len(steps) > 0, nil
}

// pathTo returns the sequence of states to walk from `from` to `to`, so a candidate
// that is still `extracted` reaches `validated` through the declared intermediate
// states rather than by an edge that does not exist.
func pathTo(from, to domain.CandidateState) []domain.CandidateState {
	if from == to {
		return nil
	}
	if from.CanTransitionTo(to) {
		return []domain.CandidateState{to}
	}
	chain := []domain.CandidateState{
		domain.CandidateNormalized,
		domain.CandidateValidationPending,
		to,
	}
	var out []domain.CandidateState
	cur := from
	for _, step := range chain {
		if cur == step {
			continue
		}
		if cur.CanTransitionTo(step) {
			out = append(out, step)
			cur = step
		}
	}
	return out
}

// conflictAssessment is what buildContext learned about other sources while assembling
// the validation context, carried forward so Execute can record it without asking the
// database the same question twice.
type conflictAssessment struct {
	// Subject is this candidate's observation, ready to record.
	Subject domain.SourceObservation
	// Previous is this source's stored observation, zero when it has none.
	Previous    domain.SourceObservation
	HasPrevious bool
	// Observations is every source's current claim about this product and channel, as
	// loaded. It is kept rather than discarded so the routing step can ask which
	// candidates are still in dispute without repeating the query -- and so that the
	// answer it gets is the same one the verdict was computed from.
	Observations []domain.SourceObservation
	Verdict      domain.ConflictVerdict
	// Recordable reports whether Subject is worth storing: an eligible source, a
	// resolved product, a non-empty version, and a later observation than Previous.
	Recordable bool
}

// conflictOutcome is what reconcileConflict did, so the routing branch can attach a
// review item to a conflict without querying for it again.
type conflictOutcome struct {
	ConflictID string
	// Opened reports that this call created the conflict rather than refreshing one.
	Opened bool
	// ExistingReviewItemID is the item already attached to an open conflict, if any.
	// Reusing it is what keeps one disagreement from producing one queue item per
	// check.
	ExistingReviewItemID string
}

func (uc *ValidateCandidate) buildContext(ctx context.Context, c domain.CandidateRelease, src domain.Source, now time.Time) (domain.ValidationContext, conflictAssessment, error) {
	vctx := domain.ValidationContext{
		Now:                   now,
		SourceHealth:          src.Health,
		SourceEligible:        src.CompliancePermitsCollection() && (src.Health == domain.SourceActive || src.Health == domain.SourceDegraded),
		SourceQuality:         src.QualityClass,
		SourceOfficial:        src.Official && src.QualityClass == domain.QualityOfficialManufacturer,
		EvidencePresent:       c.EvidenceID != "",
		ConfidenceThreshold:   uc.deps.ConfidenceThreshold,
		ApplicabilityKnown:    true,
		FutureDateTolerance:   uc.deps.FutureDateTolerance,
		EarliestPlausibleDate: uc.deps.EarliestPlausibleDate,
	}

	var assessment conflictAssessment

	// Product resolution.
	switch c.ProductMatchStatus {
	case domain.MatchUnique:
		vctx.ProductResolved = c.ProductID != ""
	case domain.MatchAmbiguous:
		vctx.ProductAmbiguous = true
	case domain.MatchNoMatch:
		vctx.ProductResolved = false
	default:
		if c.ProductID != "" {
			vctx.ProductResolved = true
			break
		}
		matches, err := uc.deps.Products.ResolveByAlias(ctx, src.VendorID, c.ProductMatchHint)
		if err != nil {
			return vctx, assessment, fmt.Errorf("resolve product: %w", err)
		}
		switch len(matches) {
		case 0:
			vctx.ProductResolved = false
		case 1:
			vctx.ProductResolved = true
			vctx.ResolvedProductID = matches[0].ID
		default:
			vctx.ProductAmbiguous = true
		}
	}

	if c.ProductID != "" {
		dup, err := uc.deps.Releases.FindDuplicate(ctx, c.ProductID, c.Version.Normalized(), c.Applicability.Channel)
		if err != nil {
			return vctx, assessment, fmt.Errorf("duplicate check: %w", err)
		}
		vctx.DuplicateReleaseID = dup

		// A CHANNEL-LESS candidate asks this for the newest release of ANY channel, not
		// for the newest channel-less one: an empty channel means "any" on this port
		// (see ReleaseRepository.LatestForProduct). That is a wider comparison than a
		// reader might assume, and it is deliberate here rather than merely tolerated --
		// PreviousVersion feeds a plausibility gate, and the most recent thing this
		// product actually published is the better baseline for "is this jump credible"
		// than the most recent thing published without a channel label.
		//
		// It cannot corrupt data. domain.LatestComparison still decides the flag,
		// ClearLatestFlag still scopes its UPDATE to one channel through the same
		// COALESCE, and release_mappings_latest_idx is per channel either way. The only
		// observable effect is that such a candidate may not take the channel-less
		// latest flag. No product in the pilot dataset mixes channelled and channel-less
		// mappings. If strict per-channel semantics are ever wanted for publication
		// specifically, that needs its own port method, not a change in one adapter.
		latest, err := uc.deps.Releases.LatestForProduct(ctx, c.ProductID, c.Applicability.Channel)
		switch {
		case err == nil:
			vctx.PreviousVersion = latest.Version
		case isNotFound(err):
			// No previous release: the first observation for a product is always
			// plausible.
		default:
			return vctx, assessment, fmt.Errorf("latest release: %w", err)
		}
	}

	// Gate 10 needs to know what every other source says. The product it is asked
	// about is the one already on the candidate, or the one just resolved from the
	// hint -- an observation attributed to no product is not evidence of anything, so
	// when neither exists the whole assessment is skipped.
	productID := c.ProductID
	if productID == "" {
		productID = vctx.ResolvedProductID
	}
	if productID == "" || uc.deps.Conflicts == nil {
		return vctx, assessment, nil
	}

	channel := c.Applicability.Channel
	assessment.Subject = domain.SourceObservation{
		SourceID:          src.ID,
		ProductID:         productID,
		Channel:           channel,
		RawVersion:        c.Version.Raw(),
		NormalizedVersion: c.Version.Normalized(),
		ReleaseDate:       c.ReleaseDate,
		CandidateID:       c.ID,
		EvidenceID:        c.EvidenceID,
		ObservedAt:        now,
		FirstObservedAt:   now,
		QualityClass:      src.QualityClass,
		Official:          src.Official,
		Eligible:          vctx.SourceEligible,
	}

	observations, err := uc.deps.Conflicts.ObservationsForProduct(ctx, productID, channel)
	if err != nil {
		return vctx, assessment, fmt.Errorf("load source observations: %w", err)
	}
	assessment.Observations = observations
	for _, o := range observations {
		if o.SourceID == src.ID {
			assessment.Previous = o
			assessment.HasPrevious = true
			assessment.Subject.FirstObservedAt = o.FirstObservedAt
			break
		}
	}

	// AssessSourceConflict ignores the subject's own row and every ineligible source,
	// so everything that was loaded is handed to it rather than filtered here twice.
	assessment.Verdict = domain.AssessSourceConflict(assessment.Subject, observations)
	vctx.ConflictingSourceVersions = assessment.Verdict.ConflictingVersions
	vctx.OutrankedSourceVersions = assessment.Verdict.OutrankedVersions

	assessment.Recordable = vctx.SourceEligible &&
		strings.TrimSpace(assessment.Subject.NormalizedVersion) != "" &&
		(!assessment.HasPrevious || domain.LaterObservation(assessment.Subject, assessment.Previous))

	return vctx, assessment, nil
}

// reconcileConflict records this source's observation and opens, refreshes or closes the
// conflict for the candidate's product and channel.
func (uc *ValidateCandidate) reconcileConflict(ctx context.Context, c domain.CandidateRelease, a conflictAssessment, now time.Time, pending *[]domain.Event) (conflictOutcome, error) {
	var out conflictOutcome
	if uc.deps.Conflicts == nil || a.Subject.ProductID == "" {
		return out, nil
	}

	if a.Recordable {
		if err := a.Subject.Validate(); err != nil {
			return out, fmt.Errorf("source observation: %w", err)
		}
		if err := uc.deps.Conflicts.RecordObservation(ctx, a.Subject); err != nil {
			return out, fmt.Errorf("record source observation: %w", err)
		}
	}

	// An ineligible source's claim is not evidence, so it may neither raise a conflict
	// nor settle one. domain.AssessSourceConflict already ignores ineligible *others*;
	// the subject's own eligibility is the caller's to check, and it is checked here
	// rather than left implicit because opening a conflict now files a queue item -- a
	// source under a takedown would otherwise generate work for a human out of a claim
	// FirmScout is not allowed to act on.
	if !a.Subject.Eligible {
		return out, nil
	}

	if a.Verdict.Status == domain.ConflictUnresolved {
		conflict := domain.SourceConflict{
			ID:            uc.deps.IDs.NewID("cfl"),
			ProductID:     a.Subject.ProductID,
			Channel:       a.Subject.Channel,
			State:         domain.ConflictOpen,
			AuthorityRank: a.Verdict.AuthorityRank,
			Versions:      domain.MergeParticipants(a.Subject.NormalizedVersion, a.Verdict.ConflictingVersions),
			SourceIDs:     domain.MergeParticipants(a.Subject.SourceID, a.Verdict.ConflictingSourceIDs),
			DetectedAt:    now,
			LastSeenAt:    now,
		}
		if err := conflict.Validate(); err != nil {
			return out, fmt.Errorf("source conflict: %w", err)
		}
		stored, created, err := uc.deps.Conflicts.UpsertOpenConflict(ctx, conflict)
		if err != nil {
			return out, fmt.Errorf("record source conflict: %w", err)
		}
		out.ConflictID = stored.ID
		out.Opened = created
		out.ExistingReviewItemID = stored.ReviewItemID
		if !created {
			return out, nil
		}
		*pending = append(*pending,
			domain.NewEvent(domain.EventSourceConflictDetected, now, "product", conflict.ProductID).
				With("channel", conflict.Channel).
				With("conflict_id", stored.ID).
				With("versions", strings.Join(conflict.Versions, ", ")))
		// Synchronously, not through JobRefreshSummary: the worker leases four job
		// kinds and that is not one of them, so an enqueued refresh would sit in the
		// table forever and has_source_conflict would never become true.
		if err := uc.refreshSummary(ctx, conflict.ProductID); err != nil {
			return out, err
		}
		return out, nil
	}

	resolution := "sources agree"
	if len(a.Verdict.OutrankedVersions) > 0 {
		resolution = "resolved by source authority"
	}
	closed, err := uc.deps.Conflicts.CloseOpenConflict(ctx, a.Subject.ProductID, a.Subject.Channel, resolution, actorSystem, now)
	if err != nil {
		return out, fmt.Errorf("close source conflict: %w", err)
	}
	if !closed {
		return out, nil
	}
	*pending = append(*pending,
		domain.NewEvent(domain.EventSourceConflictResolved, now, "product", a.Subject.ProductID).
			With("channel", a.Subject.Channel).
			With("resolution", resolution))
	return out, uc.refreshSummary(ctx, a.Subject.ProductID)
}

// refreshSummary rebuilds the product's precomputed row. A product with no summary row
// yet is not a validation failure -- the summary is built on first publication -- so
// domain.ErrNotFound is swallowed here rather than aborting the pipeline.
func (uc *ValidateCandidate) refreshSummary(ctx context.Context, productID string) error {
	if err := uc.deps.Releases.RefreshProductSummary(ctx, productID); err != nil && !isNotFound(err) {
		return fmt.Errorf("refresh summary: %w", err)
	}
	return nil
}

// reviewSignals gathers the facts domain.ScoreReview is allowed to use. A product that
// cannot be loaded contributes nothing rather than failing validation: a scoring input
// is not worth failing a pipeline over.
func (uc *ValidateCandidate) reviewSignals(ctx context.Context, c domain.CandidateRelease, src domain.Source, v domain.ValidationVerdict, conflicted bool) domain.ReviewSignals {
	s := domain.ReviewSignals{
		Gate:               failingGate(v),
		SourceQuality:      src.QualityClass,
		SourceOfficial:     src.Official,
		Confidence:         c.Confidence,
		ReleaseType:        effectiveReleaseType(c),
		ConflictUnresolved: conflicted,
	}
	if c.ProductID == "" {
		return s
	}
	product, err := uc.deps.Products.GetByID(ctx, c.ProductID)
	if err != nil {
		return s
	}
	s.ProductPopularity = product.PopularityScore
	s.ProductSecurityCritical = product.SecurityCritical
	return s
}

// ---------------------------------------------------------------------------
// Publication
// ---------------------------------------------------------------------------

// PublishRelease turns a validated candidate into a published fact.
//
// This is the only use case that writes to the releases table. Everything it does runs
// in one unit of work: insert the release, map it to products, move the latest-observed
// flag, transition the candidate, and refresh the product summary. Either all of it
// happens or none of it does, because a release visible without its mapping, or a
// summary pointing at a release that was rolled back, is worse than a failed job.
type PublishRelease struct {
	deps IngestDeps
}

// NewPublishRelease builds the use case.
func NewPublishRelease(d IngestDeps) *PublishRelease {
	d.defaults()
	return &PublishRelease{deps: d}
}

// PublishResult reports what was published.
type PublishResult struct {
	ReleaseID   string
	CandidateID string
	ProductID   string
	Version     string
	Published   bool
	Reason      string
}

// Execute publishes one validated candidate.
func (uc *PublishRelease) Execute(ctx context.Context, candidateID string) (PublishResult, error) {
	now := uc.deps.Clock.Now()

	c, err := uc.deps.Candidates.GetByID(ctx, candidateID)
	if err != nil {
		return PublishResult{}, fmt.Errorf("load candidate: %w", err)
	}
	out := PublishResult{CandidateID: c.ID, ProductID: c.ProductID, Version: c.Version.Raw()}

	if c.State == domain.CandidatePublished {
		// Idempotency: the job ran twice. This is not an error.
		out.ReleaseID = c.PublishedReleaseID
		out.Published = false
		out.Reason = "candidate was already published"
		return out, nil
	}
	if c.State != domain.CandidateValidated && c.State != domain.CandidateHumanReviewRequired {
		return out, fmt.Errorf("candidate %s in state %s: %w", c.ID, c.State, domain.ErrInvalidTransition)
	}
	if c.ProductID == "" {
		return out, fmt.Errorf("candidate %s has no resolved product: %w", c.ID, domain.ErrValidation)
	}

	src, err := uc.deps.Sources.GetByID(ctx, c.SourceID)
	if err != nil {
		return out, fmt.Errorf("load source: %w", err)
	}

	// ADR-0018's compliance gate covers dispatch and fetching. Nothing covered promotion
	// of what had already been collected, and the gap is not theoretical: a candidate
	// that fails a gate waits in the review queue for days, and if the vendor sends a
	// takedown in the meantime an operator records terms_review_status = 'prohibited'.
	// The dispatch gate correctly stops the next check; without this one, a reviewer
	// accepting the queued item still published that vendor's data after FirmScout was
	// told not to. Publication is the moment the data becomes FirmScout's public answer,
	// so it is the moment the permission has to hold.
	if !src.CompliancePermitsCollection() {
		out.Reason = "source " + src.ID + " may no longer be published from: robots policy " +
			string(src.RobotsPolicyStatus) + ", terms review " + string(src.TermsReviewStatus)
		return out, fmt.Errorf("candidate %s: %s: %w", c.ID, out.Reason, domain.ErrNotPermitted)
	}

	rel := domain.Release{
		ID:               uc.deps.IDs.NewID("rel"),
		VendorID:         src.VendorID,
		Version:          c.Version,
		ReleaseType:      effectiveReleaseType(c),
		Channel:          c.Applicability.Channel,
		ReleaseDate:      c.ReleaseDate,
		PublicationDate:  c.PublicationDate,
		FirstObservedAt:  c.DiscoveredAt,
		LastVerifiedAt:   now,
		PublishedAt:      now,
		ReleaseNotesURL:  c.ReleaseNotesURL,
		CandidateID:      c.ID,
		CollectorRunID:   c.CollectorRunID,
		EvidenceID:       c.EvidenceID,
		SourceConfidence: c.Confidence,
		CreatedAt:        now,
	}
	if rel.FirstObservedAt.IsZero() {
		rel.FirstObservedAt = now
	}
	if err := rel.Validate(); err != nil {
		return out, fmt.Errorf("release invariants: %w", err)
	}

	mapping := domain.ReleaseProductMapping{
		ID:               uc.deps.IDs.NewID("rpm"),
		ReleaseID:        rel.ID,
		ProductID:        c.ProductID,
		Applicability:    c.Applicability,
		IsLatestObserved: false,
		CreatedAt:        now,
	}

	// Decide whether this release becomes the latest observed for the product and
	// channel. This never compares version strings.
	latest, err := uc.deps.Releases.LatestForProduct(ctx, c.ProductID, c.Applicability.Channel)
	switch {
	case !rel.EligibleForLatest():
		// An advisory is not a version anybody upgrades to, and a withdrawn release is
		// a retracted fact. Neither may answer "what should I be running".
		mapping.IsLatestObserved = false
	case err == nil:
		mapping.IsLatestObserved = domain.LatestComparison(rel, latest) > 0
	case isNotFound(err):
		mapping.IsLatestObserved = true
	default:
		return out, fmt.Errorf("latest release: %w", err)
	}

	if err := mapping.Validate(); err != nil {
		return out, fmt.Errorf("mapping invariants: %w", err)
	}

	err = uc.deps.UoW.Within(ctx, func(ctx context.Context) error {
		if mapping.IsLatestObserved {
			if err := uc.deps.Releases.ClearLatestFlag(ctx, c.ProductID, c.Applicability.Channel); err != nil {
				return fmt.Errorf("clear latest flag: %w", err)
			}
		}
		if err := uc.deps.Releases.Insert(ctx, rel, []domain.ReleaseProductMapping{mapping}); err != nil {
			return fmt.Errorf("insert release: %w", err)
		}
		if err := uc.deps.Candidates.UpdateState(ctx, c.ID, domain.CandidatePublished, ""); err != nil {
			return fmt.Errorf("transition candidate: %w", err)
		}
		if err := uc.deps.Candidates.SetPublishedRelease(ctx, c.ID, rel.ID); err != nil {
			return fmt.Errorf("link candidate to release: %w", err)
		}
		if err := uc.deps.Releases.RefreshProductSummary(ctx, c.ProductID); err != nil {
			return fmt.Errorf("refresh summary: %w", err)
		}
		return nil
	})
	if err != nil {
		return out, err
	}

	if err := uc.deps.Events.Publish(ctx,
		domain.NewEvent(domain.EventReleasePublished, now, "release", rel.ID).
			With("product_id", c.ProductID).
			With("version", rel.Version.Raw()).
			With("release_type", string(rel.ReleaseType)).
			With("latest", boolString(mapping.IsLatestObserved))); err != nil {
		return out, fmt.Errorf("publish event: %w", err)
	}

	out.ReleaseID = rel.ID
	out.Published = true
	return out, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// Review item kinds this use case files, matching the review_items.kind CHECK
// constraint. They are constants because the routing code and the queue's kind filter
// have to agree on the spelling.
const (
	reviewKindProductMatchAmbiguous  = "product_match_ambiguous"
	reviewKindImplausibleTransition  = "candidate_implausible_transition"
	reviewKindMultiSourceConflict    = "multi_source_conflict"
	reviewKindCandidateLowConfidence = "candidate_low_confidence"
)

func reviewKindFor(v domain.ValidationVerdict) string {
	for _, r := range v.Results {
		if r.Outcome != domain.GateReviewRequired {
			continue
		}
		switch r.Gate {
		case domain.GateProductIdentity:
			return reviewKindProductMatchAmbiguous
		case domain.GateVersionTransition:
			return reviewKindImplausibleTransition
		case domain.GateMultiSourceAgree:
			return reviewKindMultiSourceConflict
		default:
			return reviewKindCandidateLowConfidence
		}
	}
	return reviewKindCandidateLowConfidence
}

// failingGate names the first gate that asked for a human, which is the one a reviewer
// has to answer. It is recorded on the review item because a queue that says a
// candidate needs review without saying which check said so is a queue nobody can act
// on.
func failingGate(v domain.ValidationVerdict) domain.ValidationGate {
	for _, r := range v.Results {
		if r.Outcome == domain.GateReviewRequired || r.Outcome == domain.GateRejected {
			return r.Gate
		}
	}
	return ""
}

// reviewPayload carries the identifiers a reviewer UI reads by name. The keys are
// constants for exactly that reason: a typo here is a panel that is silently always
// empty.
func reviewPayload(v domain.ValidationVerdict, outcome conflictOutcome, a conflictAssessment) map[string]string {
	payload := map[string]string{}
	if g := failingGate(v); g != "" {
		payload[PayloadKeyFailingGate] = string(g)
	}
	if outcome.ConflictID == "" {
		return payload
	}
	payload[PayloadKeyConflictID] = outcome.ConflictID
	payload[PayloadKeyConflictVersions] = strings.Join(
		domain.MergeParticipants(a.Subject.NormalizedVersion, a.Verdict.ConflictingVersions), ",")
	payload[PayloadKeyConflictSources] = strings.Join(
		domain.MergeParticipants(a.Subject.SourceID, a.Verdict.ConflictingSourceIDs), ",")
	if candidates := disputingCandidateIDs(a); len(candidates) > 0 {
		payload[PayloadKeyConflictCandidates] = strings.Join(candidates, ",")
	}
	return payload
}

// disputingCandidateIDs names every candidate currently in dispute: the one being
// validated plus the candidate behind each other eligible source's current observation.
//
// The item's subject can only name one of them. Listing the rest is what lets a reviewer
// -- or a consistency check -- get from any candidate parked in human_review_required
// back to the queue item that covers it.
func disputingCandidateIDs(a conflictAssessment) []string {
	ids := []string{a.Subject.CandidateID}
	for _, o := range a.Observations {
		if o.SourceID == a.Subject.SourceID || !o.Eligible {
			continue
		}
		ids = append(ids, o.CandidateID)
	}
	return domain.MergeParticipants(ids[0], ids[1:])
}

// effectiveReleaseType is the type a candidate is published as: what the collector
// determined, or the classification proposed for it when the collector could not tell.
// It never defaults to a type -- an undetermined type stays undetermined, and
// Release.Validate refuses to publish it.
func effectiveReleaseType(c domain.CandidateRelease) domain.ReleaseType {
	if c.ReleaseType != "" && c.ReleaseType != domain.ReleaseTypeUnknown {
		return c.ReleaseType
	}
	if c.ProposedReleaseType != "" && c.ProposedReleaseType != domain.ReleaseTypeUnknown {
		return c.ProposedReleaseType
	}
	return c.ReleaseType
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func readArtifact(ctx context.Context, store ArtifactStore, id string) ([]byte, error) {
	rc, err := store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	const maxArtifact = 32 << 20
	buf := make([]byte, 0, 64<<10)
	tmp := make([]byte, 32<<10)
	for {
		n, err := rc.Read(tmp)
		if n > 0 {
			if len(buf)+n > maxArtifact {
				return nil, fmt.Errorf("artifact %s exceeds the maximum readable size", id)
			}
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			if isEOF(err) {
				break
			}
			return nil, err
		}
	}
	return buf, nil
}
