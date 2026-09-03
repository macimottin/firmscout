package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/macimottin/firmscout/internal/domain"
)

// CheckSourceDeps are the ports the CheckSource use case needs.
type CheckSourceDeps struct {
	Sources   SourceRepository
	Fetcher   Fetcher
	Normalize Normalizer
	Artifacts ArtifactStore
	Queue     JobQueue
	Events    EventPublisher
	Clock     Clock
	IDs       IDGenerator
	Policy    domain.SchedulingPolicy
	UserAgent string
	// MaxBytes bounds any single fetch. A source config may lower it, never raise it.
	MaxBytes int64
	Timeout  time.Duration
}

// CheckSource is the watcher: it decides whether a source may have changed, using the
// cheapest signal available, and enqueues extraction only when something actually
// changed.
//
// This is the use case whose cost profile determines whether monitoring tens of
// thousands of sources is affordable. Almost every run should end in `unchanged` after
// a conditional request that transfers no body.
type CheckSource struct {
	deps CheckSourceDeps
}

// NewCheckSource builds the use case.
func NewCheckSource(d CheckSourceDeps) *CheckSource {
	if d.MaxBytes <= 0 {
		d.MaxBytes = 8 << 20 // 8 MiB
	}
	if d.Timeout <= 0 {
		d.Timeout = 30 * time.Second
	}
	if d.Policy.MinInterval <= 0 {
		d.Policy = domain.DefaultSchedulingPolicy()
	}
	return &CheckSource{deps: d}
}

// CheckSourceResult reports what the check found.
type CheckSourceResult struct {
	SourceID     string
	Outcome      domain.CheckOutcome
	ChangeSignal domain.ChangeSignal
	ArtifactID   string
	ContentHash  string
	NextCheckAt  time.Time
	Reason       string
	// ExtractionEnqueued reports whether the change was significant enough to run a
	// collector.
	ExtractionEnqueued bool
}

// Execute checks one source.
//
// The compliance gate is re-evaluated here, not only at dispatch, because a source's
// robots or terms status can change between the scheduler's query and this call. See
// ADR-0018.
func (uc *CheckSource) Execute(ctx context.Context, sourceID string) (CheckSourceResult, error) {
	now := uc.deps.Clock.Now()

	src, err := uc.deps.Sources.GetByID(ctx, sourceID)
	if err != nil {
		return CheckSourceResult{}, fmt.Errorf("load source: %w", err)
	}

	if !src.CompliancePermitsCollection() {
		return CheckSourceResult{
			SourceID: sourceID,
			Outcome:  domain.OutcomeManualReviewRequired,
			Reason:   "compliance status does not permit collection",
		}, fmt.Errorf("source %s: %w", sourceID, domain.ErrNotPermitted)
	}

	check := SourceCheck{
		ID:        uc.deps.IDs.NewID("chk"),
		SourceID:  src.ID,
		StartedAt: now,
	}

	maxBytes := uc.deps.MaxBytes
	res, fetchErr := uc.deps.Fetcher.Fetch(ctx, FetchRequest{
		URL:                 src.URL,
		ETag:                src.ETag,
		LastModified:        src.LastModifiedValue,
		ExpectedContentType: src.ExpectedContentType,
		MaxBytes:            maxBytes,
		Timeout:             uc.deps.Timeout,
		UserAgent:           uc.deps.UserAgent,
		RespectRobots:       true,
	})

	check.FinishedAt = uc.deps.Clock.Now()
	check.HTTPStatus = res.StatusCode
	check.ResponseETag = res.ETag
	check.ResponseLastModified = res.LastModified
	check.RedirectLocation = res.RedirectLocation
	check.BytesFetched = res.BytesRead
	check.Outcome = res.Outcome
	check.ChangeSignal = res.ChangeSignal

	if fetchErr != nil {
		check.ErrorMessage = fetchErr.Error()
		if check.Outcome == "" {
			check.Outcome = domain.OutcomeUnavailable
		}
	}

	out := CheckSourceResult{
		SourceID:     src.ID,
		Outcome:      check.Outcome,
		ChangeSignal: check.ChangeSignal,
	}

	// A conditional request that returned 304 is the cheap path: no body, no
	// normalisation, no hashing, no extraction.
	if check.Outcome == domain.OutcomeChanged && len(res.Body) > 0 {
		_, hash, nerr := uc.deps.Normalize.Normalize(
			res.ContentType, res.Body, src.Normalize.SectionSelector, src.Normalize.Strip)
		if nerr != nil {
			check.Outcome = domain.OutcomeParserFailed
			check.ErrorMessage = nerr.Error()
			out.Outcome = check.Outcome
			out.Reason = "normalisation failed: " + nerr.Error()
		} else {
			check.NormalizedContentHash = hash
			out.ContentHash = hash

			// The hash decides whether this is a real change. A byte-level diff in
			// the response is not, by itself, a reason to run a collector.
			if hash != "" && hash == src.NormalizedContentHash {
				check.Outcome = domain.OutcomeUnchanged
				check.ChangeSignal = domain.SignalSectionHash
				out.Outcome = domain.OutcomeUnchanged
				out.ChangeSignal = domain.SignalSectionHash
				out.Reason = "content changed but the monitored section did not"
			} else {
				// The artifact holds the RAW response, not the normalised text.
				// Normalisation exists to answer "did the part we care about
				// change?"; the extractor needs the original markup to run
				// selectors against, and a reviewer needs to see what the vendor
				// actually served. The blob is therefore addressed by the hash of
				// the raw bytes, while the source's change-detection state keeps
				// the normalised section hash.
				rawSum := sha256.Sum256(res.Body)
				rawHash := hex.EncodeToString(rawSum[:])
				artifactID, created, aerr := uc.deps.Artifacts.Put(ctx, rawHash, res.ContentType, res.Body)
				if aerr != nil {
					return out, fmt.Errorf("store artifact: %w", aerr)
				}
				check.ArtifactID = artifactID
				out.ArtifactID = artifactID
				if !created {
					// The same content was seen before under a different source or
					// an earlier check. Refresh its last-seen marker rather than
					// storing a second copy.
					_ = uc.deps.Artifacts.Touch(ctx, artifactID, now)
				}
				out.Reason = "monitored section changed"
			}
		}
	}

	if err := uc.applyOutcome(ctx, &src, check, now); err != nil {
		return out, err
	}
	out.NextCheckAt = src.NextCheckAt

	if err := uc.deps.Sources.RecordCheck(ctx, check); err != nil {
		return out, fmt.Errorf("record check: %w", err)
	}

	if err := uc.emitEvents(ctx, src, check, now); err != nil {
		return out, err
	}

	if check.Outcome == domain.OutcomeChanged && check.ArtifactID != "" {
		if err := uc.deps.Queue.Enqueue(ctx, Job{
			ID:             uc.deps.IDs.NewID("job"),
			Kind:           JobExtractCandidates,
			IdempotencyKey: "extract:" + src.ID + ":" + check.NormalizedContentHash,
			Payload: map[string]string{
				"source_id":   src.ID,
				"artifact_id": check.ArtifactID,
				"check_id":    check.ID,
			},
			RunAfter: now,
		}); err != nil {
			return out, fmt.Errorf("enqueue extraction: %w", err)
		}
		out.ExtractionEnqueued = true
	}

	out.Outcome = check.Outcome
	return out, nil
}

// applyOutcome updates the source's change-detection state, health and next check
// time, then persists it.
func (uc *CheckSource) applyOutcome(ctx context.Context, src *domain.Source, check SourceCheck, now time.Time) error {
	src.LastCheckedAt = now

	switch check.Outcome {
	case domain.OutcomeUnchanged:
		src.ConsecutiveUnchanged++
		src.ConsecutiveFailures = 0
		src.LastSuccessAt = now
		if check.ResponseETag != "" {
			src.ETag = check.ResponseETag
		}
		if check.ResponseLastModified != "" {
			src.LastModifiedValue = check.ResponseLastModified
		}
	case domain.OutcomeChanged:
		src.ConsecutiveUnchanged = 0
		src.ConsecutiveFailures = 0
		src.LastSuccessAt = now
		src.LastChangedAt = now
		src.ETag = check.ResponseETag
		src.LastModifiedValue = check.ResponseLastModified
		if check.NormalizedContentHash != "" {
			src.NormalizedContentHash = check.NormalizedContentHash
		}
	default:
		src.ConsecutiveFailures++
	}

	nextHealth := domain.HealthForOutcome(src.Health, check.Outcome, src.ConsecutiveFailures)
	if nextHealth != src.Health {
		// A rejected transition is not fatal to the check; it means the state
		// machine forbids the move (for example a retired source), and the current
		// state stands.
		_ = src.TransitionHealth(nextHealth)
	}

	decision := domain.NextInterval(domain.ScheduleInput{
		Policy:               uc.deps.Policy,
		Now:                  now,
		Outcome:              check.Outcome,
		Health:               src.Health,
		ConsecutiveUnchanged: src.ConsecutiveUnchanged,
		ConsecutiveFailures:  src.ConsecutiveFailures,
		LastChangedAt:        src.LastChangedAt,
		ConfiguredInterval:   time.Duration(src.CheckFrequencySeconds) * time.Second,
	})
	src.NextCheckAt = decision.NextAt

	if err := uc.deps.Sources.UpdateCheckState(ctx, *src); err != nil {
		return fmt.Errorf("update source state: %w", err)
	}
	return nil
}

func (uc *CheckSource) emitEvents(ctx context.Context, src domain.Source, check SourceCheck, now time.Time) error {
	evts := []domain.Event{
		domain.NewEvent(domain.EventSourceChecked, now, "source", src.ID).
			With("outcome", string(check.Outcome)).
			With("change_signal", string(check.ChangeSignal)),
	}
	switch check.Outcome {
	case domain.OutcomeChanged:
		evts = append(evts, domain.NewEvent(domain.EventSourceChanged, now, "source", src.ID))
	case domain.OutcomeUnchanged:
		evts = append(evts, domain.NewEvent(domain.EventSourceUnchanged, now, "source", src.ID))
	default:
		evts = append(evts, domain.NewEvent(domain.EventSourceFailed, now, "source", src.ID).
			With("outcome", string(check.Outcome)))
		if src.Health == domain.SourceBroken {
			evts = append(evts, domain.NewEvent(domain.EventSourceBroken, now, "source", src.ID))
		}
	}
	if err := uc.deps.Events.Publish(ctx, evts...); err != nil {
		return fmt.Errorf("publish events: %w", err)
	}
	return nil
}

// ErrSourceNotDispatchable reports that a source was checked when policy forbade it.
var ErrSourceNotDispatchable = errors.New("application: source is not dispatchable")
