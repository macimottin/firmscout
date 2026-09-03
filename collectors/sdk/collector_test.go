package sdk_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/macimottin/firmscout/collectors/sdk"
	"github.com/macimottin/firmscout/internal/domain"
)

// fake is a collector whose behaviour each test dictates. It exists to exercise the
// limits wrapper, which is the part of the SDK that must hold even when the collector
// it wraps is badly behaved -- which is the whole reason the limits do not live
// inside the collector.
type fake struct {
	id       string
	extract  func(ctx context.Context, src domain.Source, art sdk.Artifact) ([]domain.CandidateRelease, error)
	warnings []string
}

func (f *fake) ID() string                  { return f.id }
func (f *fake) Version() string             { return "1" }
func (f *fake) Vendor() string              { return "test" }
func (f *fake) Supports(domain.Source) bool { return true }
func (f *fake) Extract(ctx context.Context, src domain.Source, art sdk.Artifact) ([]domain.CandidateRelease, error) {
	return f.extract(ctx, src, art)
}

func (f *fake) ExtractWithWarnings(ctx context.Context, src domain.Source, art sdk.Artifact) ([]domain.CandidateRelease, []string, error) {
	c, err := f.extract(ctx, src, art)
	return c, f.warnings, err
}

func candidates(n int) []domain.CandidateRelease {
	out := make([]domain.CandidateRelease, n)
	for i := range out {
		v, err := domain.NewVersionString("1.0." + string(rune('a'+i%26)))
		if err != nil {
			panic(err)
		}
		out[i] = domain.CandidateRelease{Version: v}
	}
	return out
}

func TestWithLimitsRejectsAnOversizedArtifact(t *testing.T) {
	inner := &fake{id: "test.oversize", extract: func(context.Context, domain.Source, sdk.Artifact) ([]domain.CandidateRelease, error) {
		t.Error("Extract was called for an artifact that exceeds the byte limit")
		return nil, nil
	}}
	c := sdk.WithLimits(inner, sdk.Limits{MaxArtifactBytes: 16})

	_, err := c.Extract(context.Background(), domain.Source{}, sdk.Artifact{Body: make([]byte, 17)})
	if err == nil {
		t.Fatal("an oversized artifact was accepted")
	}
	if !errors.Is(err, sdk.ErrArtifactTooLarge) {
		t.Errorf("error is not ErrArtifactTooLarge: %v", err)
	}
	// A limit breach is a policy outcome, not a transport failure.
	if !errors.Is(err, domain.ErrNotPermitted) {
		t.Errorf("error does not classify as a policy refusal: %v", err)
	}
}

func TestWithLimitsAbandonsASlowExtraction(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	inner := &fake{id: "test.slow", extract: func(ctx context.Context, _ domain.Source, _ sdk.Artifact) ([]domain.CandidateRelease, error) {
		// Deliberately ignores ctx: the wrapper must not depend on the collector
		// cooperating, because a pathological regex or an infinite loop will not.
		<-release
		return nil, nil
	}}
	c := sdk.WithLimits(inner, sdk.Limits{ExtractTimeout: 20 * time.Millisecond})

	start := time.Now()
	_, err := c.Extract(context.Background(), domain.Source{}, sdk.Artifact{Body: []byte("x")})
	elapsed := time.Since(start)

	if !errors.Is(err, sdk.ErrExtractTimeout) {
		t.Fatalf("want ErrExtractTimeout, got %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("the wrapper waited %s for an uncooperative collector", elapsed)
	}
	if !strings.Contains(err.Error(), "test.slow") {
		t.Errorf("error does not name the collector: %v", err)
	}
}

func TestWithLimitsTruncatesAndReportsCandidateFloods(t *testing.T) {
	inner := &fake{id: "test.flood", extract: func(context.Context, domain.Source, sdk.Artifact) ([]domain.CandidateRelease, error) {
		return candidates(50), nil
	}}

	var gotID string
	var produced, kept int
	c := sdk.WithLimits(inner, sdk.Limits{
		MaxCandidates: 10,
		OnTruncate: func(id string, p, k int) {
			gotID, produced, kept = id, p, k
		},
	})

	out, err := c.Extract(context.Background(), domain.Source{}, sdk.Artifact{Body: []byte("x")})
	if err != nil {
		t.Fatalf("truncation must not be an error: %v", err)
	}
	if len(out) != 10 {
		t.Errorf("want 10 candidates after truncation, got %d", len(out))
	}
	if gotID != "test.flood" || produced != 50 || kept != 10 {
		t.Errorf("overflow not reported correctly: id=%q produced=%d kept=%d", gotID, produced, kept)
	}
}

func TestWithLimitsContainsAPanic(t *testing.T) {
	inner := &fake{id: "test.panic", extract: func(context.Context, domain.Source, sdk.Artifact) ([]domain.CandidateRelease, error) {
		panic("selector index out of range")
	}}
	c := sdk.WithLimits(inner, sdk.Limits{})

	_, err := c.Extract(context.Background(), domain.Source{}, sdk.Artifact{Body: []byte("x")})
	if err == nil {
		t.Fatal("a panicking collector produced no error")
	}
	if !strings.Contains(err.Error(), "panicked") || !strings.Contains(err.Error(), "test.panic") {
		t.Errorf("error does not identify the panicking collector: %v", err)
	}
}

func TestWithLimitsPassesThroughWhenWithinBudget(t *testing.T) {
	want := candidates(3)
	inner := &fake{id: "test.ok", extract: func(context.Context, domain.Source, sdk.Artifact) ([]domain.CandidateRelease, error) {
		return want, nil
	}}
	c := sdk.WithLimits(inner, sdk.DefaultLimits())

	got, err := c.Extract(context.Background(), domain.Source{}, sdk.Artifact{Body: []byte("x")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != len(want) {
		t.Errorf("want %d candidates, got %d", len(want), len(got))
	}
	if c.ID() != "test.ok" || c.Version() != "1" || c.Vendor() != "test" {
		t.Errorf("the wrapper does not pass identity through: %s/%s/%s", c.ID(), c.Version(), c.Vendor())
	}
	if !c.Supports(domain.Source{}) {
		t.Error("the wrapper does not pass Supports through")
	}
}

// TestWarningsUnwrapsTheLimitsWrapper: diagnostics must remain reachable through the
// wrapper, or a fixture harness could not assert why a candidate was skipped.
func TestWarningsUnwrapsTheLimitsWrapper(t *testing.T) {
	inner := &fake{
		id:       "test.warn",
		warnings: []string{"container 0: field \"version\" produced no value"},
		extract: func(context.Context, domain.Source, sdk.Artifact) ([]domain.CandidateRelease, error) {
			return nil, nil
		},
	}
	c := sdk.WithLimits(inner, sdk.DefaultLimits())

	_, warnings, err := sdk.Warnings(context.Background(), c, domain.Source{}, sdk.Artifact{Body: []byte("x")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("want 1 warning through the wrapper, got %d", len(warnings))
	}
}

// TestExcerptRoundTrip documents the convention the ingest use case relies on: a
// collector's excerpt travels in EvidenceID until the candidate is persisted.
func TestExcerptRoundTrip(t *testing.T) {
	c := sdk.WithExcerpt(domain.CandidateRelease{}, "  7.24.2 Long-term 2026-09-03  ")
	if got := sdk.ExcerptOf(c); got != "7.24.2 Long-term 2026-09-03" {
		t.Errorf("excerpt: got %q", got)
	}

	long := strings.Repeat("a", domain.MaxExcerptLength+100)
	c = sdk.WithExcerpt(domain.CandidateRelease{}, long)
	if len(sdk.ExcerptOf(c)) > domain.MaxExcerptLength {
		t.Errorf("excerpt was not truncated to the domain bound: %d bytes", len(sdk.ExcerptOf(c)))
	}
}
