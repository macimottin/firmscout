package collectors

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/macimottin/firmscout/collectors/sdk"
	"github.com/macimottin/firmscout/internal/domain"
)

// HTMLSelectors is the goquery-backed engine for server-rendered HTML pages.
//
// It executes no JavaScript. A source that renders its release data client-side into
// an empty shell cannot be handled by this engine at all, and the honest answer there
// is a vendor API or a feed, not a headless browser bolted onto a collector.
type HTMLSelectors struct {
	cfg    Config
	logger *slog.Logger
}

// NewHTMLSelectors builds the engine for one config.
//
// logger may be nil, in which case warnings are discarded by the collector itself and
// remain available through ExtractWithWarnings. Nothing here holds a repository, a
// clock or an HTTP client, because nothing here is given one.
func NewHTMLSelectors(cfg Config, logger *slog.Logger) (*HTMLSelectors, error) {
	if cfg.Spec.Engine != EngineHTMLSelectors {
		return nil, fieldErr("spec.engine", "config %s declares engine %q, not %q",
			cfg.Metadata.ID, cfg.Spec.Engine, EngineHTMLSelectors)
	}
	return &HTMLSelectors{cfg: cfg, logger: logger}, nil
}

// ID returns the config id, which is the identifier evidence rows reference.
func (h *HTMLSelectors) ID() string { return h.cfg.Metadata.ID }

// Version returns the config revision. It changes when, and only when, a change to the
// config could produce different candidates from the same artifact.
func (h *HTMLSelectors) Version() string { return h.cfg.CollectorVersion() }

// Vendor returns the vendor slug the config declares.
func (h *HTMLSelectors) Vendor() string { return h.cfg.Metadata.Vendor }

// Supports reports whether this collector handles src. Routing is by collector id:
// the source registry names the collector it wants, and a config-driven collector does
// not second-guess that with content sniffing. It is cheap and does no I/O.
func (h *HTMLSelectors) Supports(src domain.Source) bool {
	return src.CollectorID == h.cfg.Metadata.ID
}

// Extract turns an artifact into candidates, discarding warnings after logging them.
func (h *HTMLSelectors) Extract(ctx context.Context, src domain.Source, art sdk.Artifact) ([]domain.CandidateRelease, error) {
	candidates, warnings, err := h.ExtractWithWarnings(ctx, src, art)
	h.log(src, warnings)
	return candidates, err
}

// ExtractWithWarnings is Extract plus the reasons anything was skipped.
//
// Warnings are returned, never stored on the receiver: two goroutines extracting
// different artifacts through the same collector must not see each other's
// diagnostics, and a collector that accumulated state would stop being a pure
// function of (src, art).
func (h *HTMLSelectors) ExtractWithWarnings(ctx context.Context, src domain.Source, art sdk.Artifact) ([]domain.CandidateRelease, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(art.Body))
	if err != nil {
		return nil, nil, fmt.Errorf("collector %s: parse HTML: %w", h.ID(), err)
	}

	// Strip first: scripts, styles and framework-injected volatile attributes are
	// removed before anything is selected, so a stripped element can never be the
	// thing a field selector accidentally matched.
	for _, sel := range h.cfg.Spec.Normalize.stripSel {
		doc.FindMatcher(sel).Remove()
	}

	containers := h.containers(doc)
	var (
		out      []domain.CandidateRelease
		warnings []string
	)
	if containers.Length() == 0 {
		// Zero candidates from a successfully parsed page is a valid, common result:
		// a changelog with no entries the selector recognises. It is reported as a
		// warning, not an error, because the caller's repair signal is a run that
		// produced nothing where it used to produce entries -- a decision the
		// application layer makes from the source's history, which extraction cannot
		// see.
		warnings = append(warnings, fmt.Sprintf(
			"%s: release_container %q matched no elements%s; extracted 0 candidates",
			h.ID(), h.cfg.Spec.Extract.ReleaseContainer, h.sectionNote()))
		return nil, warnings, nil
	}

	containers.Each(func(i int, s *goquery.Selection) {
		if ctx.Err() != nil {
			return
		}
		candidate, warns, ok := buildCandidate(&h.cfg, i, &htmlContainer{sel: s, cfg: &h.cfg})
		warnings = append(warnings, warns...)
		if ok {
			out = append(out, candidate)
		}
	})
	if err := ctx.Err(); err != nil {
		return nil, warnings, err
	}
	return out, warnings, nil
}

// containers returns every release container, in document order, scoped to
// normalize.section_selector when one is declared.
//
// Scoping matters for more than tidiness: the section selector is what makes change
// detection stable on a page with no ETag, and extracting from outside it would mean
// the collector reads content whose churn the hash deliberately ignores.
func (h *HTMLSelectors) containers(doc *goquery.Document) *goquery.Selection {
	containerSel := h.cfg.Spec.Extract.containerSel
	all := doc.FindMatcher(containerSel)

	sectionSel := h.cfg.Spec.Normalize.sectionSel
	if sectionSel == nil {
		return all
	}

	// A container is in scope when it is a section itself -- the MikroTik case, where
	// one changelog entry is both the hashed section and the container -- or a
	// descendant of one. ClosestMatcher searches self-then-ancestors, which is
	// exactly that predicate, and filtering the document-wide selection preserves
	// document order, so candidate order is the page's order.
	return all.FilterFunction(func(_ int, s *goquery.Selection) bool {
		return s.ClosestMatcher(sectionSel).Length() > 0
	})
}

func (h *HTMLSelectors) sectionNote() string {
	if h.cfg.Spec.Normalize.SectionSelector == "" {
		return ""
	}
	return fmt.Sprintf(" within section_selector %q", h.cfg.Spec.Normalize.SectionSelector)
}

func (h *HTMLSelectors) log(src domain.Source, warnings []string) {
	if h.logger == nil || len(warnings) == 0 {
		return
	}
	for _, w := range warnings {
		h.logger.Warn("collector extraction warning",
			slog.String("collector_id", h.ID()),
			slog.String("collector_version", h.Version()),
			slog.String("source_id", src.ID),
			slog.String("warning", w))
	}
}

// htmlContainer adapts one matched element to the shared value pipeline.
type htmlContainer struct {
	sel *goquery.Selection
	cfg *Config
}

// value locates a field's raw text within this container.
func (c *htmlContainer) value(f *FieldSpec) (string, bool, error) {
	target := c.sel
	if !f.scope {
		matches := c.sel.FindMatcher(f.sel)
		switch {
		case matches.Length() == 0:
			return "", false, nil
		case matches.Length() > 1 && f.Multiple == MultipleError:
			// The page said several things and the config declared no way to choose.
			// Taking the first would be a guess by document order, which is how four
			// MikroTik long-term releases were once recorded as stable: their entries
			// carry a Stable badge and a Long-term badge, and Stable is rendered
			// first. Naming what matched is what makes that diagnosable in one read
			// of the log instead of one afternoon of bisecting the catalogue.
			return "", false, fmt.Errorf("selector %q matched %d elements (%s); "+
				"set multiple: %s to take the first deliberately",
				f.Selector, matches.Length(), matchSummary(matches), MultipleFirst)
		}
		target = matches.First()
	}
	if f.Attribute != "" {
		v, ok := target.Attr(f.Attribute)
		return v, ok, nil
	}
	return target.Text(), true, nil
}

// matchSummary renders what a multi-match selector actually found, so the error names
// the ambiguity rather than only its arity.
func matchSummary(sel *goquery.Selection) string {
	const max = 4
	var seen []string
	sel.EachWithBreak(func(i int, s *goquery.Selection) bool {
		seen = append(seen, strconv.Quote(collapseWhitespace(s.Text())))
		return i < max-1
	})
	out := strings.Join(seen, ", ")
	if sel.Length() > len(seen) {
		out += ", ..."
	}
	return out
}

// excerpt returns the evidence text for this container. Its length bound is applied by
// sdk.WithExcerpt, not by the config: a config cannot opt out of quoting briefly.
func (c *htmlContainer) excerpt() string {
	spec := &c.cfg.Spec.Extract
	if spec.excerptScope || spec.excerptSel == nil {
		return collapseWhitespace(c.sel.Text())
	}
	found := c.sel.FindMatcher(spec.excerptSel)
	if found.Length() == 0 {
		return collapseWhitespace(c.sel.Text())
	}
	return collapseWhitespace(found.First().Text())
}
