package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/domain"
)

func TestSourceHealthTransitions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		from  domain.SourceHealth
		to    domain.SourceHealth
		allow bool
	}{
		{"discovered to pending review", domain.SourceDiscovered, domain.SourcePendingReview, true},
		{"pending review to active", domain.SourcePendingReview, domain.SourceActive, true},
		{"active to degraded", domain.SourceActive, domain.SourceDegraded, true},
		{"degraded back to active", domain.SourceDegraded, domain.SourceActive, true},
		{"broken back to active", domain.SourceBroken, domain.SourceActive, true},
		{"active straight to active", domain.SourceActive, domain.SourceActive, true},
		{"discovered straight to active", domain.SourceDiscovered, domain.SourceActive, false},
		{"retired to active", domain.SourceRetired, domain.SourceActive, false},
		{"retired to anything", domain.SourceRetired, domain.SourceDegraded, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := domain.Source{Health: tc.from}
			err := s.TransitionHealth(tc.to)
			if tc.allow && err != nil {
				t.Fatalf("transition %s -> %s rejected: %v", tc.from, tc.to, err)
			}
			if !tc.allow {
				if err == nil {
					t.Fatalf("transition %s -> %s was allowed", tc.from, tc.to)
				}
				if !errors.Is(err, domain.ErrInvalidTransition) {
					t.Errorf("error = %v, want ErrInvalidTransition", err)
				}
			}
		})
	}
}

// A retired source is terminal: nothing brings it back, which is what makes
// "retired sources are never checked again" a property of the type.
func TestRetiredSourceIsTerminal(t *testing.T) {
	t.Parallel()
	for _, to := range []domain.SourceHealth{
		domain.SourceActive, domain.SourceDegraded, domain.SourceBroken,
		domain.SourceDisabled, domain.SourcePendingReview,
	} {
		s := domain.Source{Health: domain.SourceRetired}
		if err := s.TransitionHealth(to); err == nil {
			t.Errorf("retired source transitioned to %s", to)
		}
	}
}

func TestCandidateTransitions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		from  domain.CandidateState
		to    domain.CandidateState
		allow bool
	}{
		{"discovered to extracted", domain.CandidateDiscovered, domain.CandidateExtracted, true},
		{"extracted to normalized", domain.CandidateExtracted, domain.CandidateNormalized, true},
		{"normalized to validation pending", domain.CandidateNormalized, domain.CandidateValidationPending, true},
		{"validation pending to validated", domain.CandidateValidationPending, domain.CandidateValidated, true},
		{"validated to published", domain.CandidateValidated, domain.CandidatePublished, true},
		{"review to published", domain.CandidateHumanReviewRequired, domain.CandidatePublished, true},
		{"published back to discovered", domain.CandidatePublished, domain.CandidateDiscovered, false},
		{"published back to validated", domain.CandidatePublished, domain.CandidateValidated, false},
		{"discovered straight to published", domain.CandidateDiscovered, domain.CandidatePublished, false},
		{"superseded to anything", domain.CandidateSuperseded, domain.CandidatePublished, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := domain.CandidateRelease{State: tc.from}
			err := c.TransitionTo(tc.to)
			if tc.allow && err != nil {
				t.Fatalf("transition %s -> %s rejected: %v", tc.from, tc.to, err)
			}
			if !tc.allow && err == nil {
				t.Fatalf("transition %s -> %s was allowed", tc.from, tc.to)
			}
		})
	}
}

func TestSourceDispatchableRespectsCompliance(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	base := domain.Source{
		Enabled:            true,
		Health:             domain.SourceActive,
		RobotsPolicyStatus: domain.RobotsAllowed,
		TermsReviewStatus:  domain.TermsApproved,
		NextCheckAt:        now.Add(-time.Minute),
	}
	if !base.Dispatchable(now) {
		t.Fatal("a compliant, enabled, due source must be dispatchable")
	}

	tests := []struct {
		name   string
		mutate func(*domain.Source)
	}{
		{"disabled", func(s *domain.Source) { s.Enabled = false }},
		{"robots disallowed", func(s *domain.Source) { s.RobotsPolicyStatus = domain.RobotsDisallowed }},
		{"robots unknown", func(s *domain.Source) { s.RobotsPolicyStatus = domain.RobotsUnknown }},
		{"terms pending", func(s *domain.Source) { s.TermsReviewStatus = domain.TermsPending }},
		{"terms prohibited", func(s *domain.Source) { s.TermsReviewStatus = domain.TermsProhibited }},
		{"health broken", func(s *domain.Source) { s.Health = domain.SourceBroken }},
		{"health retired", func(s *domain.Source) { s.Health = domain.SourceRetired }},
		{"not due yet", func(s *domain.Source) { s.NextCheckAt = now.Add(time.Hour) }},
		{"inside Retry-After", func(s *domain.Source) { s.RetryAfterUntil = now.Add(time.Hour) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := base
			tc.mutate(&s)
			if s.Dispatchable(now) {
				t.Errorf("source dispatchable despite: %s", tc.name)
			}
		})
	}
}

// The measured Dell case: technically ideal source, robots.txt says no.
func TestDisallowedRobotsBlocksCollectionRegardlessOfQuality(t *testing.T) {
	t.Parallel()
	s := domain.Source{
		Enabled:            true,
		Health:             domain.SourceActive,
		Official:           true,
		QualityClass:       domain.QualityOfficialManufacturer,
		RobotsPolicyStatus: domain.RobotsDisallowed,
		TermsReviewStatus:  domain.TermsApproved,
	}
	if s.CompliancePermitsCollection() {
		t.Fatal("an official source with disallowed robots must not be collectable")
	}
}

func TestHealthForOutcome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		current  domain.SourceHealth
		outcome  domain.CheckOutcome
		failures int
		want     domain.SourceHealth
	}{
		{"success recovers a degraded source", domain.SourceDegraded, domain.OutcomeUnchanged, 0, domain.SourceActive},
		{"success recovers a broken source", domain.SourceBroken, domain.OutcomeChanged, 0, domain.SourceActive},
		{"active stays active", domain.SourceActive, domain.OutcomeUnchanged, 0, domain.SourceActive},
		{"one failure only degrades", domain.SourceActive, domain.OutcomeUnavailable, 1, domain.SourceDegraded},
		{"repeated failures break", domain.SourceActive, domain.OutcomeUnavailable, 3, domain.SourceBroken},
		{"unauthorized is its own state", domain.SourceActive, domain.OutcomeUnauthorized, 0, domain.SourceAuthenticationRequired},
		{"rate limited is its own state", domain.SourceActive, domain.OutcomeRateLimited, 0, domain.SourceRateLimited},
		{"off-host redirect means relocated", domain.SourceActive, domain.OutcomeRedirected, 0, domain.SourceRelocated},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := domain.HealthForOutcome(tc.current, tc.outcome, tc.failures)
			if got != tc.want {
				t.Errorf("HealthForOutcome(%s, %s, %d) = %s, want %s",
					tc.current, tc.outcome, tc.failures, got, tc.want)
			}
		})
	}
}

func TestComputeDedupeKeyIsStableAndDiscriminating(t *testing.T) {
	t.Parallel()
	v1, _ := domain.NewVersionString("7.24.2")
	v2, _ := domain.NewVersionString("7.24.3")
	app := domain.Applicability{Channel: "stable"}

	a := domain.ComputeDedupeKey("routeros", v1, app)
	b := domain.ComputeDedupeKey("routeros", v1, app)
	if a != b {
		t.Fatal("dedupe key is not stable across calls; every check would create duplicates")
	}
	if a == domain.ComputeDedupeKey("routeros", v2, app) {
		t.Error("different versions produced the same dedupe key; releases would be swallowed")
	}
	if a == domain.ComputeDedupeKey("routeros", v1, domain.Applicability{Channel: "testing"}) {
		t.Error("different channels produced the same dedupe key")
	}
	if a == domain.ComputeDedupeKey("chateau", v1, app) {
		t.Error("different products produced the same dedupe key")
	}
}

// A vendor correcting a release date must update the existing candidate rather than
// create a second one. That holds only if the date plays no part in the key, so this
// builds two candidates that differ ONLY by date and asserts they collide.
func TestDedupeKeyIgnoresReleaseDate(t *testing.T) {
	t.Parallel()
	v, err := domain.NewVersionString("7.24.2")
	if err != nil {
		t.Fatalf("NewVersionString: %v", err)
	}
	app := domain.Applicability{Channel: "stable"}

	first, err := domain.NewExactDate(2026, time.August, 15)
	if err != nil {
		t.Fatalf("NewExactDate: %v", err)
	}
	corrected, err := domain.NewExactDate(2026, time.August, 18)
	if err != nil {
		t.Fatalf("NewExactDate: %v", err)
	}

	a := domain.CandidateRelease{
		ProductMatchHint: "routeros", Version: v, Applicability: app, ReleaseDate: first,
	}
	b := domain.CandidateRelease{
		ProductMatchHint: "routeros", Version: v, Applicability: app, ReleaseDate: corrected,
	}
	if a.ReleaseDate.String() == b.ReleaseDate.String() {
		t.Fatal("the two candidates must differ by date for this test to mean anything")
	}

	keyA := domain.ComputeDedupeKey(a.ProductMatchHint, a.Version, a.Applicability)
	keyB := domain.ComputeDedupeKey(b.ProductMatchHint, b.Version, b.Applicability)
	if keyA != keyB {
		t.Errorf("a corrected release date produced a different dedupe key (%s vs %s); "+
			"the correction would appear as a second release rather than updating the first", keyA, keyB)
	}
}
