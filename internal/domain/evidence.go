package domain

import (
	"strings"
	"time"
	"unicode/utf8"
)

// DiscoveryMethod records how a fact came to FirmScout's attention. It travels with
// every piece of evidence so that a reader can tell a deterministic extraction from an
// AI-assisted proposal from a community submission.
type DiscoveryMethod string

const (
	DiscoveryDeterministic DiscoveryMethod = "deterministic"
	DiscoveryAIAssisted    DiscoveryMethod = "ai_assisted"
	DiscoveryManual        DiscoveryMethod = "manual"
	DiscoveryCommunity     DiscoveryMethod = "community"
)

// ValidDiscoveryMethod reports whether m is a declared discovery method.
func ValidDiscoveryMethod(m DiscoveryMethod) bool {
	switch m {
	case DiscoveryDeterministic, DiscoveryAIAssisted, DiscoveryManual, DiscoveryCommunity:
		return true
	}
	return false
}

// Evidence is the provenance record for a fact: where it came from, when it was
// retrieved, what the source actually said, and what produced the interpretation.
//
// Every published release carries one. It is the difference between a catalogue and a
// rumour.
type Evidence struct {
	ID       string
	SourceID string
	// SourceURL is the exact URL the value was read from, which may be more specific
	// than the source's registered URL.
	SourceURL  string
	SourceType SourceType
	Official   bool

	RetrievedAt time.Time
	ArtifactID  string
	ContentHash string

	// Excerpt is a concise quotation sufficient to verify the extraction. FirmScout
	// links to release notes rather than reproducing them; this is a verification
	// aid, not a copy.
	Excerpt string

	RawValue        string
	NormalizedValue string

	CollectorID      string
	CollectorVersion string
	DiscoveryMethod  DiscoveryMethod

	// AIModelID and AIPromptVersion are required when DiscoveryMethod is
	// DiscoveryAIAssisted, so an AI-influenced fact can always be traced to the model
	// and prompt that produced it.
	AIModelID       string
	AIPromptVersion string

	Confidence float64
	CreatedAt  time.Time
}

// MaxExcerptLength bounds stored excerpts. The limit exists for a legal reason as much
// as a storage one: a short quotation used to verify a fact is a different thing from
// a reproduction of a vendor's release notes.
const MaxExcerptLength = 4000

// Validate checks the invariants that make evidence trustworthy.
func (e Evidence) Validate() error {
	if strings.TrimSpace(e.SourceURL) == "" {
		return invalid("evidence.source_url", "must not be empty")
	}
	if e.RetrievedAt.IsZero() {
		return invalid("evidence.retrieved_at", "must be set")
	}
	if strings.TrimSpace(e.Excerpt) == "" {
		return invalid("evidence.excerpt", "must not be empty; a fact without a quotable source is not evidence")
	}
	if len(e.Excerpt) > MaxExcerptLength {
		return invalid("evidence.excerpt", "exceeds the maximum excerpt length")
	}
	if !ValidDiscoveryMethod(e.DiscoveryMethod) {
		return invalid("evidence.discovery_method", string(e.DiscoveryMethod)+" is not a known discovery method")
	}
	if e.DiscoveryMethod == DiscoveryAIAssisted {
		if strings.TrimSpace(e.AIModelID) == "" {
			return invalid("evidence.ai_model_id", "required when the discovery method is ai_assisted")
		}
		if strings.TrimSpace(e.AIPromptVersion) == "" {
			return invalid("evidence.ai_prompt_version", "required when the discovery method is ai_assisted")
		}
	}
	if e.Confidence < 0 || e.Confidence > 1 {
		return invalid("evidence.confidence", "must be between 0 and 1")
	}
	return nil
}

// TruncateExcerpt shortens an excerpt to at most MaxExcerptLength bytes, appending an
// ellipsis so a reader can tell the quotation was cut.
//
// The result is guaranteed to satisfy Evidence.Validate. Two details make that true and
// are easy to get wrong: the ellipsis is three bytes in UTF-8, not one, so the budget
// must account for its encoded length rather than its rune count; and cutting a byte
// slice can land in the middle of a multi-byte rune, which would store invalid UTF-8 in
// a column that a reviewer later has to read.
func TruncateExcerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= MaxExcerptLength {
		return s
	}

	const ellipsis = "…"
	budget := MaxExcerptLength - len(ellipsis)
	cut := s[:budget]

	// Step back to a rune boundary so the excerpt never ends mid-character.
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + ellipsis
}
