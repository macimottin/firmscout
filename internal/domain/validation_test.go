package domain_test

import (
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/domain"
)

func baseCandidate(t *testing.T) domain.CandidateRelease {
	t.Helper()
	v, err := domain.NewVersionString("7.24.3")
	if err != nil {
		t.Fatalf("NewVersionString: %v", err)
	}
	d, err := domain.NewExactDate(2026, time.September, 2)
	if err != nil {
		t.Fatalf("NewExactDate: %v", err)
	}
	return domain.CandidateRelease{
		ID:                 "cnd_test",
		SourceID:           "src_test",
		EvidenceID:         "evd_test",
		ProductID:          "prd_test",
		ProductMatchStatus: domain.MatchUnique,
		Version:            v,
		ReleaseType:        domain.ReleaseTypeEmbeddedOS,
		ReleaseDate:        d,
		Confidence:         0.95,
		DedupeKey:          "abc123",
		State:              domain.CandidateValidationPending,
	}
}

func baseContext(t *testing.T) domain.ValidationContext {
	t.Helper()
	prev, err := domain.NewVersionString("7.24.2")
	if err != nil {
		t.Fatalf("NewVersionString: %v", err)
	}
	return domain.ValidationContext{
		Now:                   time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC),
		ProductResolved:       true,
		SourceEligible:        true,
		SourceOfficial:        true,
		SourceQuality:         domain.QualityOfficialManufacturer,
		PreviousVersion:       prev,
		EvidencePresent:       true,
		ConfidenceThreshold:   domain.DefaultConfidenceThreshold,
		ApplicabilityKnown:    true,
		FutureDateTolerance:   48 * time.Hour,
		EarliestPlausibleDate: time.Date(1990, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestHappyPathPublishes(t *testing.T) {
	t.Parallel()
	v := domain.RunValidationGates(baseCandidate(t), baseContext(t))
	if !v.Publishable() {
		t.Fatalf("expected publishable, got %q: %s", v.Decision, v.Reason)
	}
	if len(v.Results) != len(domain.GateOrder()) {
		t.Errorf("ran %d gates, want %d", len(v.Results), len(domain.GateOrder()))
	}
	for _, r := range v.Results {
		if r.Detail == "" {
			t.Errorf("gate %q recorded no detail; a reviewer needs the reasoning", r.Gate)
		}
	}
}

func TestGatesRejectAndShortCircuit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*domain.CandidateRelease, *domain.ValidationContext)
	}{
		{"no product match", func(_ *domain.CandidateRelease, ctx *domain.ValidationContext) {
			ctx.ProductResolved = false
		}},
		{"source not eligible", func(_ *domain.CandidateRelease, ctx *domain.ValidationContext) {
			ctx.SourceEligible = false
		}},
		{"exact duplicate", func(_ *domain.CandidateRelease, ctx *domain.ValidationContext) {
			ctx.DuplicateReleaseID = "rel_existing"
		}},
		{"no evidence", func(_ *domain.CandidateRelease, ctx *domain.ValidationContext) {
			ctx.EvidencePresent = false
		}},
		{"date before the vendor existed", func(c *domain.CandidateRelease, _ *domain.ValidationContext) {
			d, _ := domain.NewExactDate(1971, time.January, 1)
			c.ReleaseDate = d
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, ctx := baseCandidate(t), baseContext(t)
			tc.mutate(&c, &ctx)
			v := domain.RunValidationGates(c, ctx)
			if v.Decision != domain.GateRejected {
				t.Fatalf("decision = %q, want rejected (%s)", v.Decision, v.Reason)
			}
			if v.Reason == "" {
				t.Error("rejection carried no reason")
			}
		})
	}
}

func TestGatesRouteToReviewRatherThanRejecting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*domain.CandidateRelease, *domain.ValidationContext)
	}{
		{"ambiguous product match", func(_ *domain.CandidateRelease, ctx *domain.ValidationContext) {
			ctx.ProductAmbiguous = true
		}},
		{"implausible version transition", func(c *domain.CandidateRelease, _ *domain.ValidationContext) {
			v, _ := domain.NewVersionString("1.0.0")
			c.Version = v
		}},
		{"confidence below threshold", func(c *domain.CandidateRelease, _ *domain.ValidationContext) {
			c.Confidence = 0.4
		}},
		{"non-official source", func(_ *domain.CandidateRelease, ctx *domain.ValidationContext) {
			ctx.SourceOfficial = false
		}},
		{"unknown hardware revision", func(_ *domain.CandidateRelease, ctx *domain.ValidationContext) {
			ctx.ApplicabilityKnown = false
		}},
		{"sources disagree", func(_ *domain.CandidateRelease, ctx *domain.ValidationContext) {
			ctx.ConflictingSourceVersions = []string{"7.25.0"}
		}},
		{"date too far in the future", func(c *domain.CandidateRelease, _ *domain.ValidationContext) {
			d, _ := domain.NewExactDate(2027, time.January, 1)
			c.ReleaseDate = d
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, ctx := baseCandidate(t), baseContext(t)
			tc.mutate(&c, &ctx)
			v := domain.RunValidationGates(c, ctx)
			if v.Decision != domain.GateReviewRequired {
				t.Fatalf("decision = %q, want review_required (%s)", v.Decision, v.Reason)
			}
			if v.Publishable() {
				t.Error("a candidate needing review reported itself publishable")
			}
		})
	}
}

// A community source never publishes automatically, however confident the extraction.
func TestCommunitySourceAlwaysReviews(t *testing.T) {
	t.Parallel()
	c, ctx := baseCandidate(t), baseContext(t)
	c.Confidence = 1.0
	ctx.SourceOfficial = false
	ctx.SourceQuality = domain.QualityTrustedCommunity
	if v := domain.RunValidationGates(c, ctx); v.Publishable() {
		t.Fatal("a community source published without review")
	}
}

// An unknown release date is normal and must not block publication; inventing a day is
// what would be wrong.
func TestUnknownDateDoesNotBlockPublication(t *testing.T) {
	t.Parallel()
	c, ctx := baseCandidate(t), baseContext(t)
	c.ReleaseDate = domain.UnknownDate
	if v := domain.RunValidationGates(c, ctx); !v.Publishable() {
		t.Fatalf("an undated release was blocked: %s", v.Reason)
	}
}

func TestReleaseRequiresEvidenceAndDeterminedType(t *testing.T) {
	t.Parallel()
	v, _ := domain.NewVersionString("7.24.3")
	base := domain.Release{
		VendorID:    "ven_mikrotik",
		Version:     v,
		ReleaseType: domain.ReleaseTypeEmbeddedOS,
		EvidenceID:  "evd_1",
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid release rejected: %v", err)
	}

	noEvidence := base
	noEvidence.EvidenceID = ""
	if err := noEvidence.Validate(); err == nil {
		t.Error("a release without evidence was accepted")
	}

	unknownType := base
	unknownType.ReleaseType = domain.ReleaseTypeUnknown
	if err := unknownType.Validate(); err == nil {
		t.Error("a release with an undetermined type was accepted")
	}

	inconsistent := base
	inconsistent.Withdrawn = true
	if err := inconsistent.Validate(); err == nil {
		t.Error("a withdrawn release without a withdrawal timestamp was accepted")
	}
}

func TestLatestComparisonNeverUsesVersionOrdering(t *testing.T) {
	t.Parallel()
	older, _ := domain.NewVersionString("9.99.99")
	newer, _ := domain.NewVersionString("1.0.0")
	oldDate, _ := domain.NewExactDate(2026, time.January, 1)
	newDate, _ := domain.NewExactDate(2026, time.September, 1)

	a := domain.Release{Version: older, ReleaseDate: oldDate}
	b := domain.Release{Version: newer, ReleaseDate: newDate}

	// Version 1.0.0 looks "smaller" but was released later, so it is the later
	// observation. Sorting by version string would get this backwards.
	if domain.LatestComparison(a, b) >= 0 {
		t.Fatal("latest comparison used version ordering rather than release date")
	}
}

func TestLatestComparisonPrefersDatedOverUndated(t *testing.T) {
	t.Parallel()
	v, _ := domain.NewVersionString("7.24.3")
	d, _ := domain.NewExactDate(2026, time.September, 1)
	dated := domain.Release{Version: v, ReleaseDate: d}
	undated := domain.Release{Version: v, ReleaseDate: domain.UnknownDate}
	if domain.LatestComparison(dated, undated) <= 0 {
		t.Error("an undated observation outranked a dated one")
	}
}
