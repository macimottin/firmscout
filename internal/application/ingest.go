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
	Artifacts  ArtifactStore
	Registry   CollectorRegistry
	Queue      JobQueue
	Events     EventPublisher
	UoW        UnitOfWork
	Clock      Clock
	IDs        IDGenerator

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
	// ReviewItemID is set when a human decision is required.
	ReviewItemID string
}

// Execute validates one candidate.
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

	vctx, err := uc.buildContext(ctx, c, src, now)
	if err != nil {
		return ValidateResult{}, err
	}

	verdict := domain.RunValidationGates(c, vctx)

	// A candidate that arrived without a product but resolved to exactly one gets the
	// match persisted here, so publication reads a settled fact rather than repeating
	// the resolution and risking a different answer.
	if c.ProductID == "" && vctx.ResolvedProductID != "" {
		c.ProductID = vctx.ResolvedProductID
		c.ProductMatchStatus = domain.MatchUnique
		if err := uc.deps.Candidates.SetResolvedProduct(ctx, c.ID, c.ProductID); err != nil {
			return ValidateResult{}, fmt.Errorf("record product match: %w", err)
		}
	}

	if err := uc.deps.Candidates.RecordValidation(ctx, c.ID, verdict.Results); err != nil {
		return ValidateResult{}, fmt.Errorf("record validation: %w", err)
	}

	out := ValidateResult{
		CandidateID: c.ID,
		Decision:    verdict.Decision,
		Reason:      verdict.Reason,
		Verdict:     verdict,
	}

	// A duplicate is not a failure. The right response is to confirm the existing
	// release is still current rather than publish a second identical row.
	if vctx.DuplicateReleaseID != "" {
		if err := uc.deps.Releases.TouchVerified(ctx, vctx.DuplicateReleaseID, now); err != nil {
			return out, fmt.Errorf("refresh verification: %w", err)
		}
	}

	switch verdict.Decision {
	case domain.GatePassed:
		if err := uc.transition(ctx, &c, domain.CandidateValidated, ""); err != nil {
			return out, err
		}
		if err := uc.deps.Events.Publish(ctx,
			domain.NewEvent(domain.EventCandidateValidated, now, "candidate", c.ID)); err != nil {
			return out, fmt.Errorf("publish event: %w", err)
		}
		if err := uc.deps.Queue.Enqueue(ctx, Job{
			ID:             uc.deps.IDs.NewID("job"),
			Kind:           JobPublishRelease,
			IdempotencyKey: "publish:" + c.ID,
			Payload:        map[string]string{"candidate_id": c.ID},
			RunAfter:       now,
		}); err != nil {
			return out, fmt.Errorf("enqueue publication: %w", err)
		}
		out.PublicationEnqueued = true

	case domain.GateReviewRequired:
		if err := uc.transition(ctx, &c, domain.CandidateHumanReviewRequired, verdict.Reason); err != nil {
			return out, err
		}
		item := ReviewItem{
			ID:            uc.deps.IDs.NewID("rev"),
			Kind:          reviewKindFor(verdict),
			SubjectType:   "candidate_release",
			SubjectID:     c.ID,
			VendorID:      src.VendorID,
			ProductID:     c.ProductID,
			Title:         "Candidate " + c.Version.Raw() + " needs review",
			Detail:        verdict.Reason,
			PriorityScore: reviewPriority(src, c),
			SLAClass:      "standard",
			CreatedAt:     now,
		}
		if err := uc.deps.Reviews.Create(ctx, item); err != nil {
			return out, fmt.Errorf("create review item: %w", err)
		}
		out.ReviewItemID = item.ID
		if err := uc.deps.Events.Publish(ctx,
			domain.NewEvent(domain.EventHumanReviewRequested, now, "candidate", c.ID).
				With("reason", verdict.Reason)); err != nil {
			return out, fmt.Errorf("publish event: %w", err)
		}

	default:
		if err := uc.transition(ctx, &c, domain.CandidateRejected, verdict.Reason); err != nil {
			return out, err
		}
		if err := uc.deps.Events.Publish(ctx,
			domain.NewEvent(domain.EventCandidateRejected, now, "candidate", c.ID).
				With("reason", verdict.Reason)); err != nil {
			return out, fmt.Errorf("publish event: %w", err)
		}
	}

	return out, nil
}

func (uc *ValidateCandidate) transition(ctx context.Context, c *domain.CandidateRelease, to domain.CandidateState, reason string) error {
	// The candidate may arrive in `extracted`; move it through the intermediate
	// states so the recorded history is complete rather than jumping edges.
	for _, step := range pathTo(c.State, to) {
		if err := c.TransitionTo(step); err != nil {
			return fmt.Errorf("candidate %s: %w", c.ID, err)
		}
		if err := uc.deps.Candidates.UpdateState(ctx, c.ID, step, reason); err != nil {
			return fmt.Errorf("update candidate state: %w", err)
		}
	}
	return nil
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

func (uc *ValidateCandidate) buildContext(ctx context.Context, c domain.CandidateRelease, src domain.Source, now time.Time) (domain.ValidationContext, error) {
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
			return vctx, fmt.Errorf("resolve product: %w", err)
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
			return vctx, fmt.Errorf("duplicate check: %w", err)
		}
		vctx.DuplicateReleaseID = dup

		latest, err := uc.deps.Releases.LatestForProduct(ctx, c.ProductID, c.Applicability.Channel)
		switch {
		case err == nil:
			vctx.PreviousVersion = latest.Version
		case isNotFound(err):
			// No previous release: the first observation for a product is always
			// plausible.
		default:
			return vctx, fmt.Errorf("latest release: %w", err)
		}
	}

	return vctx, nil
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

	releaseType := c.ReleaseType
	if releaseType == "" || releaseType == domain.ReleaseTypeUnknown {
		if c.ProposedReleaseType != "" && c.ProposedReleaseType != domain.ReleaseTypeUnknown {
			releaseType = c.ProposedReleaseType
		}
	}

	rel := domain.Release{
		ID:               uc.deps.IDs.NewID("rel"),
		VendorID:         src.VendorID,
		Version:          c.Version,
		ReleaseType:      releaseType,
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

func reviewKindFor(v domain.ValidationVerdict) string {
	for _, r := range v.Results {
		if r.Outcome != domain.GateReviewRequired {
			continue
		}
		switch r.Gate {
		case domain.GateProductIdentity:
			return "product_match_ambiguous"
		case domain.GateVersionTransition:
			return "candidate_implausible_transition"
		case domain.GateMultiSourceAgree:
			return "multi_source_conflict"
		default:
			return "candidate_low_confidence"
		}
	}
	return "candidate_low_confidence"
}

func reviewPriority(src domain.Source, c domain.CandidateRelease) int {
	score := 100
	if src.Official {
		score += 50
	}
	score += int(c.Confidence * 100)
	return score
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
