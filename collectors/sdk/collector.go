// Package sdk defines the contract every FirmScout collector satisfies -- whether it
// is generated from a YAML configuration (internal/adapters/collectors) or written by
// hand under collectors/vendors/<vendor>.
//
// # Extract is a pure function of (Source, Artifact)
//
// The single most important property of this package is what a Collector is NOT given.
// Extract receives a domain.Source, an Artifact, and a context.Context. It receives:
//
//   - no clock. It never calls time.Now(). The one legitimate "now" in extraction is
//     Artifact.RetrievedAt, stamped once at fetch time and carried as plain data.
//   - no repository, no database handle, no unit of work. Identity resolution and
//     duplicate detection belong to the application layer's validation gates; a
//     collector that could query them would be silently doing their job with none of
//     their review.
//   - no network. Every request a source needs has already happened by the time
//     Extract runs, so an Artifact is a complete, replayable input.
//
// That is what makes a fixture test meaningful rather than a snapshot that merely
// happens to pass today: an artifact recorded once, pinned next to its expected
// output, produces identical candidates on any machine, offline, years later. It is
// also why collectors are structurally incapable of publishing -- Extract returns
// []domain.CandidateRelease and there is no receiver in scope through which a release
// row could be written. See docs/architecture/collector-sdk.md §5 and §6.
//
// # Limits belong to the SDK, not to each collector
//
// A limit that lives only inside the thing being limited is not a limit. Byte budgets,
// extraction wall-clock budgets and candidate-count budgets are enforced by
// WithLimits, outside the collector, in code the collector cannot see or influence.
package sdk

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// Collector is the contract every collector implements.
//
// It is a type alias for application.Collector rather than a separate, structurally
// identical interface, so that the two can never drift apart: a change to the port the
// application layer depends on is a compile error here, not a subtle mismatch
// discovered when a collector fails to satisfy the registry.
//
// The methods, restated for readers who arrive here first:
//
//	ID()      stable identifier, e.g. "mikrotik.changelogs". Never changes once the
//	          collector has produced evidence -- evidence rows reference it, and a
//	          renamed id makes past extractions unexplainable. A rename is a new id
//	          plus an explicit migration, not an edit.
//	Version() behavioural version, bumped whenever a change could produce different
//	          candidates from the same artifact, and never bumped otherwise.
//	Vendor()  vendor slug for a vendor-specific collector, "" for a generic engine.
//	Supports() cheap, side-effect-free routing predicate. No I/O.
//	Extract()  pure; see the package comment.
type Collector = application.Collector

// Artifact is exactly what was retrieved from a source at a point in time: raw bytes
// plus the response metadata needed to reason about them. It carries no
// interpretation.
type Artifact = application.Artifact

// Registry resolves a source to the collector that handles it.
type Registry = application.CollectorRegistry

// Limits bounds what one collector run may consume.
//
// These are the three budgets docs/architecture/collector-sdk.md §4 assigns to the SDK
// wrapper rather than to collector authors, because a misbehaving or maliciously
// configured collector is exactly the case where the collector's own self-restraint is
// worth nothing.
type Limits struct {
	// MaxArtifactBytes rejects an oversized artifact before parsing rather than
	// truncating it. A truncated document is worse than no document: it parses, it
	// looks plausible, and it silently drops the entries that were cut off.
	MaxArtifactBytes int64

	// ExtractTimeout bounds extraction wall-clock time. The wrapper does not trust
	// Extract to observe ctx cooperatively; it runs Extract on a goroutine it is
	// willing to abandon.
	ExtractTimeout time.Duration

	// MaxCandidates bounds how many candidates one artifact may yield. Exceeding it
	// truncates rather than fails, because the first N candidates from a page that
	// grew unexpectedly are still useful evidence, and because a hard failure here
	// would let one malformed page stop a whole vendor's monitoring.
	MaxCandidates int

	// OnTruncate, when set, is called after truncation with the collector id, the
	// number produced and the number kept. It exists so an overflow is loud somewhere
	// (a log line, a metric, a review flag) instead of being silently swallowed;
	// leaving it nil is legal and means "truncate quietly".
	OnTruncate func(collectorID string, produced, kept int)
}

// Default limits. They are deliberately generous: they exist to bound catastrophe, not
// to second-guess a well-formed source. Per-source values come from the collector
// config's spec.fetch block.
const (
	DefaultMaxArtifactBytes = int64(2 << 20) // 2 MiB, the collector-config default
	DefaultExtractTimeout   = 15 * time.Second
	DefaultMaxCandidates    = 500
)

// DefaultLimits returns the limits applied when a caller supplies none.
func DefaultLimits() Limits {
	return Limits{
		MaxArtifactBytes: DefaultMaxArtifactBytes,
		ExtractTimeout:   DefaultExtractTimeout,
		MaxCandidates:    DefaultMaxCandidates,
	}
}

// withDefaults fills in any unset (zero or negative) budget. A zero value means "not
// configured", never "no limit"; there is deliberately no way to express "unlimited".
func (l Limits) withDefaults() Limits {
	if l.MaxArtifactBytes <= 0 {
		l.MaxArtifactBytes = DefaultMaxArtifactBytes
	}
	if l.ExtractTimeout <= 0 {
		l.ExtractTimeout = DefaultExtractTimeout
	}
	if l.MaxCandidates <= 0 {
		l.MaxCandidates = DefaultMaxCandidates
	}
	return l
}

// Limit errors. They wrap domain.ErrNotPermitted so a caller can classify a limit
// breach as a policy outcome rather than a transport failure.
var (
	// ErrArtifactTooLarge reports that the artifact exceeded Limits.MaxArtifactBytes.
	ErrArtifactTooLarge = fmt.Errorf("collector: artifact exceeds the configured size limit: %w", domain.ErrNotPermitted)
	// ErrExtractTimeout reports that extraction exceeded Limits.ExtractTimeout.
	ErrExtractTimeout = fmt.Errorf("collector: extraction exceeded its wall-clock limit: %w", domain.ErrNotPermitted)
)

// WithLimits wraps c so that every Extract call is bounded by l.
//
// The wrapper is a Collector, so it is substitutable everywhere the unwrapped
// collector was, and the wrapped collector cannot tell it is being limited: there is
// no callback it can register, no field it can set, and no way to widen its own
// budget.
func WithLimits(c Collector, l Limits) Collector {
	return &limited{inner: c, limits: l.withDefaults()}
}

// limited is the enforcing wrapper. It is unexported because the only supported way to
// obtain one is WithLimits, which guarantees the defaults have been applied.
type limited struct {
	inner  Collector
	limits Limits
}

func (w *limited) ID() string                    { return w.inner.ID() }
func (w *limited) Version() string               { return w.inner.Version() }
func (w *limited) Vendor() string                { return w.inner.Vendor() }
func (w *limited) Supports(s domain.Source) bool { return w.inner.Supports(s) }

// Unwrap returns the collector being limited, for callers that legitimately need the
// concrete implementation (a fixture harness reading warnings, for example). It does
// not remove the limits from the wrapper.
func (w *limited) Unwrap() Collector { return w.inner }

// Extract enforces all three budgets.
func (w *limited) Extract(ctx context.Context, src domain.Source, art Artifact) ([]domain.CandidateRelease, error) {
	if int64(len(art.Body)) > w.limits.MaxArtifactBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds %d", ErrArtifactTooLarge, len(art.Body), w.limits.MaxArtifactBytes)
	}

	ctx, cancel := context.WithTimeout(ctx, w.limits.ExtractTimeout)
	defer cancel()

	type result struct {
		candidates []domain.CandidateRelease
		err        error
	}
	// Buffered so the goroutine can always finish and be collected even when the
	// wrapper has already abandoned it on a deadline. An unbuffered channel here
	// would turn every timeout into a permanently blocked goroutine.
	done := make(chan result, 1)
	go func() {
		defer func() {
			// A panicking collector must not take the worker down with it. A
			// collector runs against attacker-influenceable input; a nil map or an
			// out-of-range slice index in an engine is a bug to fix, not a reason to
			// lose the process.
			if r := recover(); r != nil {
				done <- result{err: fmt.Errorf("collector %s panicked during extraction: %v", w.inner.ID(), r)}
			}
		}()
		c, err := w.inner.Extract(ctx, src, art)
		done <- result{candidates: c, err: err}
	}()

	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %s after %s", ErrExtractTimeout, w.inner.ID(), w.limits.ExtractTimeout)
		}
		return nil, ctx.Err()
	case res := <-done:
		if res.err != nil {
			return nil, res.err
		}
		return w.cap(res.candidates), nil
	}
}

// cap truncates an over-long candidate slice and reports the overflow.
func (w *limited) cap(candidates []domain.CandidateRelease) []domain.CandidateRelease {
	if len(candidates) <= w.limits.MaxCandidates {
		return candidates
	}
	produced := len(candidates)
	candidates = candidates[:w.limits.MaxCandidates]
	if w.limits.OnTruncate != nil {
		w.limits.OnTruncate(w.inner.ID(), produced, len(candidates))
	}
	return candidates
}

// ExcerptOf returns the evidence excerpt a collector attached to a candidate.
//
// The excerpt travels in CandidateRelease.EvidenceID until the candidate is persisted:
// the domain type has no Excerpt field, and application.ExtractCandidates reads
// EvidenceID as "a richer excerpt supplied by the collector" when building the
// Evidence row, clearing it before the candidate is stored. This helper exists so that
// the convention is written down in one place and read through a named function rather
// than being rediscovered from a field whose name says something else.
func ExcerptOf(c domain.CandidateRelease) string { return c.EvidenceID }

// WithExcerpt attaches an evidence excerpt to a candidate, bounded to the length the
// domain enforces. A config cannot opt out of the bound: it is applied here, not read
// from the config, because a short quotation used to verify a fact is a legally
// different thing from a reproduction of a vendor's release notes.
//
// It deliberately does not call domain.TruncateExcerpt. That helper cuts to
// MaxExcerptLength-1 bytes and then appends a three-byte ellipsis, so its result can
// be 4002 bytes -- which domain.Evidence.Validate, checking len(Excerpt) >
// MaxExcerptLength, then rejects. A collector that emitted a long excerpt would fail
// the whole extraction at evidence-insert time. Bounding it here keeps the result at
// or under the limit, so the application layer's own TruncateExcerpt call is a no-op.
func WithExcerpt(c domain.CandidateRelease, excerpt string) domain.CandidateRelease {
	c.EvidenceID = BoundExcerpt(excerpt)
	return c
}

// excerptEllipsis marks a quotation that was cut.
const excerptEllipsis = "\u2026"

// BoundExcerpt trims an excerpt to at most domain.MaxExcerptLength bytes, cutting on
// a rune boundary so the result is always valid UTF-8.
func BoundExcerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= domain.MaxExcerptLength {
		return s
	}
	cut := domain.MaxExcerptLength - len(excerptEllipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + excerptEllipsis
}

// WarningReporter is implemented by collectors that can explain what they chose not to
// extract: a container with no version, an unmapped channel label, an unparsable date.
//
// It is a separate, optional interface rather than a change to Extract because the
// warnings are diagnostics, not output: nothing downstream branches on them, and a
// collector that never warns is not deficient. The fixture harness uses it to assert
// that a deliberately malformed fixture is skipped for the stated reason rather than
// silently producing nothing.
//
// Implementations must keep ExtractWithWarnings as pure as Extract: warnings are
// returned as a value, never accumulated in a field, so that concurrent extraction
// against the same collector instance stays race-free.
type WarningReporter interface {
	ExtractWithWarnings(ctx context.Context, src domain.Source, art Artifact) ([]domain.CandidateRelease, []string, error)
}

// Warnings runs c and returns its warnings when it implements WarningReporter,
// unwrapping a WithLimits wrapper to find one. Callers that only want candidates
// should call Extract directly.
//
// Note that when c is a limits wrapper, the inner collector is called directly here,
// so the limits are NOT applied to this call. It is a diagnostic path for tests and
// tooling, not the production extraction path.
func Warnings(ctx context.Context, c Collector, src domain.Source, art Artifact) ([]domain.CandidateRelease, []string, error) {
	target := c
	for {
		if wr, ok := target.(WarningReporter); ok {
			return wr.ExtractWithWarnings(ctx, src, art)
		}
		u, ok := target.(interface{ Unwrap() Collector })
		if !ok {
			break
		}
		target = u.Unwrap()
	}
	candidates, err := c.Extract(ctx, src, art)
	return candidates, nil, err
}
