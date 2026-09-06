package domain_test

import (
	"strings"
	"testing"

	"github.com/macimottin/firmscout/internal/domain"
)

func TestScoreReviewIsTheSumOfItsNamedTerms(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		signals domain.ReviewSignals
		want    int
	}{
		{
			name:    "base only",
			signals: domain.ReviewSignals{},
			want:    domain.ReviewScoreBase,
		},
		{
			name: "authority and confidence",
			signals: domain.ReviewSignals{
				SourceQuality: domain.QualityOfficialManufacturer,
				Confidence:    0.9,
			},
			want: domain.ReviewScoreBase + 50 + 90,
		},
		{
			name: "popularity is capped",
			signals: domain.ReviewSignals{
				ProductPopularity: 5000,
			},
			want: domain.ReviewScoreBase + domain.ReviewPopularityCap,
		},
		{
			name: "a negative popularity contributes nothing rather than subtracting",
			signals: domain.ReviewSignals{
				ProductPopularity: -20,
			},
			want: domain.ReviewScoreBase,
		},
		{
			name: "confidence outside the unit interval is clamped, not trusted",
			signals: domain.ReviewSignals{
				Confidence: 4.2,
			},
			want: domain.ReviewScoreBase + domain.ReviewConfidenceWeight,
		},
		{
			name: "everything at once",
			signals: domain.ReviewSignals{
				SourceQuality:           domain.QualityAuthorizedPortal,
				Confidence:              0.5,
				ProductPopularity:       80,
				ProductSecurityCritical: true,
				ConflictUnresolved:      true,
				ReleaseType:             domain.ReleaseTypeSecurityUpdate,
			},
			want: domain.ReviewScoreBase + 40 + 50 + 80 +
				domain.ReviewScoreSecurityCritical +
				domain.ReviewScoreUnresolvedConflict +
				domain.ReviewScoreSecurityReleaseType,
		},
		{
			name: "an advisory counts as a security-related release type",
			signals: domain.ReviewSignals{
				ReleaseType: domain.ReleaseTypeAdvisory,
			},
			want: domain.ReviewScoreBase + domain.ReviewScoreSecurityReleaseType,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := domain.ScoreReview(tc.signals)
			if got.Score != tc.want {
				t.Errorf("score = %d, want %d (reasons: %v)", got.Score, tc.want, got.Reasons)
			}
			if len(got.Reasons) == 0 {
				t.Error("the queue was handed an unexplained integer")
			}
		})
	}
}

func TestScoreReviewBands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		signals domain.ReviewSignals
		want    domain.SLAClass
	}{
		{
			name: "security-critical with an unresolved conflict is urgent",
			signals: domain.ReviewSignals{
				ProductSecurityCritical: true,
				ConflictUnresolved:      true,
			},
			want: domain.SLAUrgent,
		},
		{
			name: "security-critical at the popularity boundary is urgent",
			signals: domain.ReviewSignals{
				ProductSecurityCritical: true,
				ProductPopularity:       domain.ReviewHighPopularity,
			},
			want: domain.SLAUrgent,
		},
		{
			name: "security-critical one below the boundary is only high",
			signals: domain.ReviewSignals{
				ProductSecurityCritical: true,
				ProductPopularity:       domain.ReviewHighPopularity - 1,
			},
			want: domain.SLAHigh,
		},
		{
			name: "an unresolved conflict on an ordinary product is high",
			signals: domain.ReviewSignals{
				ConflictUnresolved: true,
			},
			want: domain.SLAHigh,
		},
		{
			name: "a popular but not security-critical product is not urgent",
			signals: domain.ReviewSignals{
				ProductPopularity: 500,
				SourceQuality:     domain.QualityOfficialManufacturer,
			},
			want: domain.SLAStandard,
		},
		{
			name: "an authorised portal is standard",
			signals: domain.ReviewSignals{
				SourceQuality: domain.QualityAuthorizedPortal,
			},
			want: domain.SLAStandard,
		},
		{
			name: "a vendor repository sits below the standard band",
			signals: domain.ReviewSignals{
				SourceQuality: domain.QualityVendorRepository,
			},
			want: domain.SLALow,
		},
		{
			name:    "an unclassified source is low",
			signals: domain.ReviewSignals{},
			want:    domain.SLALow,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := domain.ScoreReview(tc.signals)
			if got.SLA != tc.want {
				t.Errorf("sla = %q, want %q", got.SLA, tc.want)
			}
			if !domain.ValidSLAClass(got.SLA) {
				t.Errorf("sla %q is not a declared class", got.SLA)
			}
		})
	}
}

// A queue that cannot explain its own order is an unexplained integer, so every term
// that moved the score has to be named.
func TestScoreReviewNamesEveryTermThatContributed(t *testing.T) {
	t.Parallel()
	p := domain.ScoreReview(domain.ReviewSignals{
		SourceQuality:           domain.QualityOfficialManufacturer,
		Confidence:              0.75,
		ProductPopularity:       30,
		ProductSecurityCritical: true,
		ConflictUnresolved:      true,
		ReleaseType:             domain.ReleaseTypeAdvisory,
	})
	joined := strings.Join(p.Reasons, " | ")
	for _, want := range []string{
		"base priority",
		"official_manufacturer",
		"confidence",
		"popularity",
		"security-critical",
		"unresolved multi-source conflict",
		"advisory",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("reasons %q do not name %q", joined, want)
		}
	}
}

// Age is deliberately not a term: the queue index already orders the oldest item first
// within a priority, and a decaying score would need a re-scoring job to reproduce that.
// Two identical items therefore score identically no matter when they were filed.
func TestScoreReviewIsIndependentOfAge(t *testing.T) {
	t.Parallel()
	s := domain.ReviewSignals{
		SourceQuality:     domain.QualityOfficialManufacturer,
		Confidence:        0.9,
		ProductPopularity: 10,
	}
	if a, b := domain.ScoreReview(s), domain.ScoreReview(s); a.Score != b.Score {
		t.Fatalf("scoring is not a pure function of its signals: %d vs %d", a.Score, b.Score)
	}
}
