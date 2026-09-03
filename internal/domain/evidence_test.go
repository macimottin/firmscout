package domain_test

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/macimottin/firmscout/internal/domain"
)

// TruncateExcerpt must produce something Evidence.Validate accepts. The subtle failure
// is the ellipsis: it is three bytes in UTF-8, so reserving one byte for it overshoots
// the limit and the evidence insert fails at the far end of the pipeline.
func TestTruncateExcerptAlwaysSatisfiesValidation(t *testing.T) {
	t.Parallel()
	inputs := []string{
		strings.Repeat("a", domain.MaxExcerptLength-1),
		strings.Repeat("a", domain.MaxExcerptLength),
		strings.Repeat("a", domain.MaxExcerptLength+1),
		strings.Repeat("a", domain.MaxExcerptLength*3),
		// Multi-byte content: cutting on a byte boundary would split a rune.
		strings.Repeat("é", domain.MaxExcerptLength),
		strings.Repeat("firmware ", domain.MaxExcerptLength),
	}
	for i, in := range inputs {
		got := domain.TruncateExcerpt(in)
		if len(got) > domain.MaxExcerptLength {
			t.Errorf("input %d: truncated excerpt is %d bytes, over the %d limit",
				i, len(got), domain.MaxExcerptLength)
		}
		if !utf8.ValidString(got) {
			t.Errorf("input %d: truncation produced invalid UTF-8", i)
		}
		ev := domain.Evidence{
			SourceURL:       "https://example.test/releases",
			RetrievedAt:     time.Now(),
			Excerpt:         got,
			DiscoveryMethod: domain.DiscoveryDeterministic,
		}
		if err := ev.Validate(); err != nil {
			t.Errorf("input %d: evidence with a truncated excerpt failed validation: %v", i, err)
		}
	}
}

func TestTruncateExcerptMarksThatItCut(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", domain.MaxExcerptLength*2)
	got := domain.TruncateExcerpt(long)
	if !strings.HasSuffix(got, "…") {
		t.Error("a truncated excerpt does not show that it was cut")
	}
	short := "7.24.3 released 2026-09-02"
	if domain.TruncateExcerpt(short) != short {
		t.Error("a short excerpt was altered")
	}
}

// Evidence produced with AI help must name the model and prompt, or a future reviewer
// cannot tell which of two contradictory facts came from a model that has since changed.
func TestEvidenceRequiresAIProvenanceWhenAIWasUsed(t *testing.T) {
	t.Parallel()
	base := domain.Evidence{
		SourceURL:       "https://example.test/releases",
		RetrievedAt:     time.Now(),
		Excerpt:         "7.24.3",
		DiscoveryMethod: domain.DiscoveryAIAssisted,
	}
	if err := base.Validate(); err == nil {
		t.Fatal("AI-assisted evidence was accepted without a model identifier")
	}

	withModel := base
	withModel.AIModelID = "some-model"
	if err := withModel.Validate(); err == nil {
		t.Error("AI-assisted evidence was accepted without a prompt version")
	}

	complete := withModel
	complete.AIPromptVersion = "repair/2026-09-01"
	if err := complete.Validate(); err != nil {
		t.Errorf("complete AI provenance was rejected: %v", err)
	}
}

func TestEvidenceRequiresANonEmptyExcerpt(t *testing.T) {
	t.Parallel()
	ev := domain.Evidence{
		SourceURL:       "https://example.test/releases",
		RetrievedAt:     time.Now(),
		Excerpt:         "   ",
		DiscoveryMethod: domain.DiscoveryDeterministic,
	}
	if err := ev.Validate(); err == nil {
		t.Fatal("evidence with a blank excerpt was accepted; a fact with no quotable source is not evidence")
	}
}
