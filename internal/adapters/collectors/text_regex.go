package collectors

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/macimottin/firmscout/collectors/sdk"
	"github.com/macimottin/firmscout/internal/domain"
)

// TextRegex is the engine for plain-text sources: a version pointer file, a bare
// changelog, an inline block already isolated by normalisation.
//
// It uses Go's regexp package, which implements RE2. That choice is the safety
// property this engine rests on: RE2 has no backtracking, so matching time is linear
// in the input regardless of what the pattern looks like, and the catastrophic
// backtracking class of denial of service does not exist here at all. The cost is
// expressiveness -- no backreferences, no lookaround -- which is a trade this project
// makes deliberately, since a config is contributor-submitted data run against
// vendor-controlled input.
//
// Input size is bounded independently of the pattern, because linear time on an
// unbounded input is still unbounded.
type TextRegex struct {
	cfg    Config
	logger *slog.Logger
}

// maxTextInputBytes is the engine's own ceiling on how much text it will scan,
// applied on top of the config's spec.fetch.max_bytes. Two limits rather than one
// because the config is contributor-supplied and could name a very large value; this
// one cannot be raised from a config file.
const maxTextInputBytes = 4 << 20 // 4 MiB

// maxTextMatches bounds how many containers one artifact may yield inside the engine,
// before the SDK's own MaxCandidates limit applies. A pattern that matches the empty
// string against a megabyte of text would otherwise allocate one candidate per byte.
const maxTextMatches = 10000

// NewTextRegex builds the engine for one config.
func NewTextRegex(cfg Config, logger *slog.Logger) (*TextRegex, error) {
	if cfg.Spec.Engine != EngineTextRegex {
		return nil, fieldErr("spec.engine", "config %s declares engine %q, not %q",
			cfg.Metadata.ID, cfg.Spec.Engine, EngineTextRegex)
	}
	return &TextRegex{cfg: cfg, logger: logger}, nil
}

// ID returns the config id.
func (t *TextRegex) ID() string { return t.cfg.Metadata.ID }

// Version returns the config revision.
func (t *TextRegex) Version() string { return t.cfg.CollectorVersion() }

// Vendor returns the vendor slug the config declares.
func (t *TextRegex) Vendor() string { return t.cfg.Metadata.Vendor }

// Supports routes by collector id, cheaply and without I/O.
func (t *TextRegex) Supports(src domain.Source) bool {
	return src.CollectorID == t.cfg.Metadata.ID
}

// Extract turns an artifact into candidates, discarding warnings after logging them.
func (t *TextRegex) Extract(ctx context.Context, src domain.Source, art sdk.Artifact) ([]domain.CandidateRelease, error) {
	candidates, warnings, err := t.ExtractWithWarnings(ctx, src, art)
	t.log(src, warnings)
	return candidates, err
}

// ExtractWithWarnings is Extract plus the reasons anything was skipped.
func (t *TextRegex) ExtractWithWarnings(ctx context.Context, src domain.Source, art sdk.Artifact) ([]domain.CandidateRelease, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	var warnings []string
	text, truncated := boundInput(string(art.Body), t.inputLimit())
	if truncated {
		warnings = append(warnings, fmt.Sprintf(
			"%s: artifact of %d bytes truncated to %d before matching; a source this large is a signal the source changed shape",
			t.ID(), len(art.Body), len(text)))
	}

	// normalize.replace runs before matching, so a config can collapse whitespace or
	// strip a per-request nonce without the pattern having to tolerate it.
	for _, r := range t.cfg.Spec.Normalize.Replace {
		text = r.re.ReplaceAllString(text, r.With)
	}

	re := t.cfg.Spec.Extract.containerRe
	matches := re.FindAllStringSubmatchIndex(text, maxTextMatches)
	if len(matches) == 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%s: release_container pattern matched nothing; extracted 0 candidates", t.ID()))
		return nil, warnings, nil
	}
	if len(matches) == maxTextMatches {
		warnings = append(warnings, fmt.Sprintf(
			"%s: release_container matched the engine's ceiling of %d times; later matches ignored",
			t.ID(), maxTextMatches))
	}

	names := re.SubexpNames()
	out := make([]domain.CandidateRelease, 0, len(matches))
	for i, m := range matches {
		if err := ctx.Err(); err != nil {
			return nil, warnings, err
		}
		candidate, warns, ok := buildCandidate(&t.cfg, i, newTextContainer(t.cfg, text, names, m))
		warnings = append(warnings, warns...)
		if ok {
			out = append(out, candidate)
		}
	}
	return out, warnings, nil
}

// inputLimit is the smaller of the config's declared max_bytes and the engine ceiling.
func (t *TextRegex) inputLimit() int {
	limit := maxTextInputBytes
	if mb := t.cfg.Spec.Fetch.MaxBytes; mb > 0 && mb < int64(limit) {
		limit = int(mb)
	}
	return limit
}

func (t *TextRegex) log(src domain.Source, warnings []string) {
	if t.logger == nil || len(warnings) == 0 {
		return
	}
	for _, w := range warnings {
		t.logger.Warn("collector extraction warning",
			slog.String("collector_id", t.ID()),
			slog.String("collector_version", t.Version()),
			slog.String("source_id", src.ID),
			slog.String("warning", w))
	}
}

// boundInput truncates on a rune boundary, so a cut never produces the invalid UTF-8
// that would make a pattern behave differently from the same pattern on valid text.
func boundInput(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// textContainer adapts one regex match to the shared value pipeline.
type textContainer struct {
	cfg    Config
	whole  string
	groups map[string]string
	// present records which named groups actually participated in the match, which
	// is different from having matched the empty string.
	present map[string]bool
}

func newTextContainer(cfg Config, text string, names []string, match []int) *textContainer {
	c := &textContainer{
		cfg:     cfg,
		whole:   text[match[0]:match[1]],
		groups:  make(map[string]string, len(names)),
		present: make(map[string]bool, len(names)),
	}
	for gi, name := range names {
		if name == "" {
			continue
		}
		start, end := match[2*gi], match[2*gi+1]
		if start < 0 || end < 0 {
			continue
		}
		c.groups[name] = text[start:end]
		c.present[name] = true
	}
	return c
}

// value returns the whole match for a ":scope" selector, or the text of the named
// capture group the selector names. The group's existence was verified at config load
// time, so a miss here means the group did not participate in this particular match.
func (c *textContainer) value(f *FieldSpec) (string, bool) {
	if f.scope {
		return c.whole, true
	}
	v, ok := c.groups[f.group]
	return v, ok
}

func (c *textContainer) excerpt() string {
	spec := &c.cfg.Spec.Extract
	if !spec.excerptScope && spec.excerptGroup != "" {
		if v, ok := c.groups[spec.excerptGroup]; ok {
			return collapseWhitespace(v)
		}
	}
	return collapseWhitespace(strings.TrimSpace(c.whole))
}
