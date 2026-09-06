package collectors

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/macimottin/firmscout/collectors/sdk"
	"github.com/macimottin/firmscout/internal/domain"
)

// RSSAtom is the engine for RSS 2.0 and Atom feeds.
//
// It parses with encoding/xml from the standard library rather than a feed library. Go's
// decoder matches struct tags on an element's local name irrespective of namespace,
// which is what lets one item type serve both RSS's <item>/<pubDate> and Atom's
// namespaced <entry>/<published> without the engine branching on which dialect it was
// handed. The cost is two documented limitations rather than silent handling: a feed
// declaring a non-UTF-8 encoding fails with a clear error (converting it would need a
// charset dependency this project has not authorised), and an undeclared entity outside
// the HTML named set is an error rather than being accepted by relaxing the parser.
type RSSAtom struct {
	cfg    Config
	logger *slog.Logger
}

// maxFeedInputBytes is the engine's own ceiling on the XML it will parse, on top of the
// config's spec.fetch.max_bytes. Two limits rather than one, because the config is
// contributor-supplied and this one cannot be raised from a YAML file.
const maxFeedInputBytes = 4 << 20

// maxFeedEntries bounds how many entries one artifact may yield inside the engine,
// before the SDK's own MaxCandidates limit applies.
const maxFeedEntries = 5000

// Feed field names an rss_atom config may name in a field's selector. The vocabulary is
// closed: a selector outside it is a load-time error naming the whole list, because a
// selector that silently matches nothing is a field that is quietly always empty.
const (
	FeedFieldTitle       = "title"
	FeedFieldLink        = "link"
	FeedFieldGUID        = "guid"
	FeedFieldDescription = "description"
	FeedFieldContent     = "content"
	FeedFieldPubDate     = "pub_date"
	FeedFieldUpdated     = "updated"
	FeedFieldCategory    = "category"
	FeedFieldAuthor      = "author"
)

// feedFieldNames is the closed selector vocabulary a rss_atom field or
// evidence_excerpt may name, restated as a set for O(1) validation. It is declared here,
// next to the constants, rather than derived from them, so the two can never silently
// drift: an entry added to one and forgotten in the other is caught by
// TestRSSAtomRejectsUnknownSelector accepting or rejecting the wrong thing.
var feedFieldNames = map[string]bool{
	FeedFieldTitle: true, FeedFieldLink: true, FeedFieldGUID: true,
	FeedFieldDescription: true, FeedFieldContent: true, FeedFieldPubDate: true,
	FeedFieldUpdated: true, FeedFieldCategory: true, FeedFieldAuthor: true,
}

// sortedFeedFieldNames renders the vocabulary for an error message. It is computed on
// demand rather than cached because it is only ever built once per rejected config, at
// load time, well off any hot path.
func sortedFeedFieldNames() []string {
	out := make([]string, 0, len(feedFieldNames))
	for k := range feedFieldNames {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Container kinds for spec.extract.release_container under the rss_atom engine.
const (
	FeedContainerAuto  = "auto"
	FeedContainerItem  = "item"
	FeedContainerEntry = "entry"
)

// NewRSSAtom builds the engine for one config.
func NewRSSAtom(cfg Config, logger *slog.Logger) (*RSSAtom, error) {
	if cfg.Spec.Engine != EngineRSSAtom {
		return nil, fieldErr("spec.engine", "config %s declares engine %q, not %q",
			cfg.Metadata.ID, cfg.Spec.Engine, EngineRSSAtom)
	}
	return &RSSAtom{cfg: cfg, logger: logger}, nil
}

// ID returns the config id, which is the identifier evidence rows reference.
func (f *RSSAtom) ID() string { return f.cfg.Metadata.ID }

// Version returns the config revision.
func (f *RSSAtom) Version() string { return f.cfg.CollectorVersion() }

// Vendor returns the vendor slug the config declares.
func (f *RSSAtom) Vendor() string { return f.cfg.Metadata.Vendor }

// Supports reports whether this collector handles src, routing by collector id exactly
// as the other two engines do: cheaply, with no I/O, and never by sniffing content.
func (f *RSSAtom) Supports(src domain.Source) bool {
	return src.CollectorID == f.cfg.Metadata.ID
}

// Extract turns an artifact into candidates, discarding warnings after logging them.
func (f *RSSAtom) Extract(ctx context.Context, src domain.Source, art sdk.Artifact) ([]domain.CandidateRelease, error) {
	candidates, warnings, err := f.ExtractWithWarnings(ctx, src, art)
	f.log(src, warnings)
	return candidates, err
}

// ExtractWithWarnings is Extract plus the reasons anything was skipped.
func (f *RSSAtom) ExtractWithWarnings(ctx context.Context, src domain.Source, art sdk.Artifact) ([]domain.CandidateRelease, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	body := art.Body
	truncated := false
	if limit := f.inputLimit(); len(body) > limit {
		// A truncated XML document usually fails to parse at all -- an unclosed tag
		// is not tolerant input the way plain text is -- so the warning below is
		// appended only if parsing still succeeds despite the cut. When it does not,
		// the parse error below is the honest report: "malformed", not "truncated
		// and silently partial".
		body = body[:limit]
		truncated = true
	}

	var doc feedDocument
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, nil, fmt.Errorf("collector %s: parse feed: %w", f.ID(), err)
	}

	var warnings []string
	if truncated {
		warnings = append(warnings, fmt.Sprintf(
			"%s: artifact of %d bytes truncated to %d before parsing; a source this large is a signal the source changed shape",
			f.ID(), len(art.Body), len(body)))
	}

	var (
		entries []feedEntry
		found   string
	)
	switch doc.XMLName.Local {
	case "rss":
		entries, found = doc.Channel.Items, FeedContainerItem
	case "feed":
		entries, found = doc.Entries, FeedContainerEntry
	default:
		// A document that parses as well-formed XML but is neither dialect this
		// engine understands is not an error: it is a source that changed shape,
		// and the repair signal is a run that produced nothing, not a job that
		// failed and gets retried.
		warnings = append(warnings, fmt.Sprintf(
			"%s: root element %q is neither <rss> (RSS 2.0) nor <feed> (Atom); extracted 0 candidates",
			f.ID(), doc.XMLName.Local))
		return nil, warnings, nil
	}

	if want := f.cfg.Spec.Extract.ReleaseContainer; want != FeedContainerAuto && want != found {
		warnings = append(warnings, fmt.Sprintf(
			"%s: release_container %q was declared but the document is %s, which carries %d %s element(s); extracted 0 candidates",
			f.ID(), want, dialectLabel(found), len(entries), found))
		return nil, warnings, nil
	}

	if len(entries) == 0 {
		// A feed with no items is a valid feed; nothing to extract is not a failure.
		warnings = append(warnings, fmt.Sprintf(
			"%s: feed parsed successfully but contains 0 %s elements", f.ID(), found))
		return nil, warnings, nil
	}
	if len(entries) > maxFeedEntries {
		warnings = append(warnings, fmt.Sprintf(
			"%s: feed contains %d entries, more than the engine's ceiling of %d; later entries ignored",
			f.ID(), len(entries), maxFeedEntries))
		entries = entries[:maxFeedEntries]
	}

	out := make([]domain.CandidateRelease, 0, len(entries))
	for i := range entries {
		if err := ctx.Err(); err != nil {
			return nil, warnings, err
		}
		candidate, warns, ok := buildCandidate(&f.cfg, i, &feedContainer{cfg: &f.cfg, entry: &entries[i]})
		warnings = append(warnings, warns...)
		if ok {
			out = append(out, candidate)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, warnings, err
	}
	return out, warnings, nil
}

// inputLimit is the smaller of the config's declared max_bytes and the engine ceiling.
func (f *RSSAtom) inputLimit() int {
	limit := maxFeedInputBytes
	if mb := f.cfg.Spec.Fetch.MaxBytes; mb > 0 && mb < int64(limit) {
		limit = int(mb)
	}
	return limit
}

func (f *RSSAtom) log(src domain.Source, warnings []string) {
	if f.logger == nil || len(warnings) == 0 {
		return
	}
	for _, w := range warnings {
		f.logger.Warn("collector extraction warning",
			slog.String("collector_id", f.ID()),
			slog.String("collector_version", f.Version()),
			slog.String("source_id", src.ID),
			slog.String("warning", w))
	}
}

// dialectLabel renders a container kind the way a human reads a warning, not the way a
// config file spells it.
func dialectLabel(container string) string {
	switch container {
	case FeedContainerItem:
		return "RSS 2.0"
	case FeedContainerEntry:
		return "Atom"
	default:
		return container
	}
}

// ---------------------------------------------------------------------------
// Feed document shape
// ---------------------------------------------------------------------------
//
// encoding/xml matches a struct field's tag against an element's LOCAL name, ignoring
// namespace, unless the tag itself carries a namespace. That is the one property this
// whole file rests on: one feedEntry serves RSS's <item> (bare elements, no namespace)
// and Atom's <entry> (elements namespaced to http://www.w3.org/2005/Atom) without this
// engine ever asking which dialect it was handed. <content:encoded> (RSS's namespaced
// module element) and Atom's <content> land in different fields below precisely because
// their local names ("encoded" and "content") differ -- the collision this technique
// would have with, say, two dialects both calling something <link> is resolved instead
// by feedLink capturing every attribute either dialect might set and letting linkText
// decide.

// feedDocument is the root of either dialect. XMLName records the root element's local
// name, which is how the engine tells RSS from Atom from anything else: item- and
// entry-level element names collide between the two dialects, but the document root
// does not.
type feedDocument struct {
	XMLName xml.Name
	Channel struct {
		Items []feedEntry `xml:"item"`
	} `xml:"channel"`
	Entries []feedEntry `xml:"entry"`
}

// feedEntry is one repeating unit: an RSS <item> or an Atom <entry>. Markup inside
// description/content-shaped fields is captured as character data and never parsed a
// second time -- a second parser inside this engine would be a second attack surface,
// and a config that needs a value out of embedded HTML uses the field's regex instead.
type feedEntry struct {
	Title          string         `xml:"title"`
	Links          []feedLink     `xml:"link"`
	GUID           string         `xml:"guid"`
	ID             string         `xml:"id"`
	Description    string         `xml:"description"`
	Summary        string         `xml:"summary"`
	Content        string         `xml:"content"`
	ContentEncoded string         `xml:"encoded"`
	PubDate        string         `xml:"pubDate"`
	Published      string         `xml:"published"`
	Updated        string         `xml:"updated"`
	Categories     []feedCategory `xml:"category"`
	Author         feedAuthor     `xml:"author"`
}

// feedLink holds every attribute either dialect might set on <link>. RSS's <link> is
// plain element text with neither attribute set; Atom's is (usually several) self-
// closing elements distinguished by rel.
type feedLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Text string `xml:",chardata"`
}

// linkText implements the §4.4 rule: RSS's element text, or Atom's first link with
// rel="alternate" or no rel at all -- never a "self" or "enclosure" link, which would
// point at the feed itself or an attachment rather than the item's page.
func linkText(links []feedLink) string {
	for _, l := range links {
		if l.Rel != "" && l.Rel != "alternate" {
			continue
		}
		if href := strings.TrimSpace(l.Href); href != "" {
			return href
		}
		if text := strings.TrimSpace(l.Text); text != "" {
			return text
		}
	}
	return ""
}

// feedCategory holds RSS's element text and Atom's term attribute; only one is ever
// populated per dialect.
type feedCategory struct {
	Term string `xml:"term,attr"`
	Text string `xml:",chardata"`
}

func categoryText(cats []feedCategory) string {
	if len(cats) == 0 {
		return ""
	}
	if text := strings.TrimSpace(cats[0].Text); text != "" {
		return text
	}
	return strings.TrimSpace(cats[0].Term)
}

// feedAuthor holds RSS's element text and Atom's nested <name>; only one is ever
// populated per dialect.
type feedAuthor struct {
	Name string `xml:"name"`
	Text string `xml:",chardata"`
}

func authorText(a feedAuthor) string {
	if name := strings.TrimSpace(a.Name); name != "" {
		return name
	}
	return strings.TrimSpace(a.Text)
}

// fieldText resolves one closed-vocabulary selector against this entry, exactly per
// collector-config-spec.md §4.4's resolution table. The default case is unreachable in
// production: validateField restricts a config's selector to this vocabulary before any
// artifact is ever fetched with it.
func (e *feedEntry) fieldText(name string) string {
	switch name {
	case FeedFieldTitle:
		return strings.TrimSpace(e.Title)
	case FeedFieldLink:
		return linkText(e.Links)
	case FeedFieldGUID:
		return firstNonEmpty(strings.TrimSpace(e.GUID), strings.TrimSpace(e.ID))
	case FeedFieldDescription:
		return firstNonEmpty(strings.TrimSpace(e.Description), strings.TrimSpace(e.Summary))
	case FeedFieldContent:
		return firstNonEmpty(strings.TrimSpace(e.ContentEncoded), strings.TrimSpace(e.Content))
	case FeedFieldPubDate:
		// Atom's <updated> is deliberately excluded here (D14): a pub_date selector
		// asks what the source calls its publication date, and a feed that only
		// carries <updated> must report honestly that nothing was found rather than
		// have this engine decide the two mean the same thing.
		return firstNonEmpty(strings.TrimSpace(e.PubDate), strings.TrimSpace(e.Published))
	case FeedFieldUpdated:
		return strings.TrimSpace(e.Updated)
	case FeedFieldCategory:
		return categoryText(e.Categories)
	case FeedFieldAuthor:
		return authorText(e.Author)
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// Value pipeline adapter
// ---------------------------------------------------------------------------

// feedContainer adapts one feed entry to the shared value pipeline (pipeline.go), which
// is what lets rss_atom agree with html_selectors and text_regex about precision,
// confidence, dedupe keys and unmapped labels rather than inventing its own opinion of
// any of them.
type feedContainer struct {
	cfg   *Config
	entry *feedEntry
}

// value returns (text, located). A located-but-empty value is impossible here in
// practice -- an rss_atom field's text is either present or it is not -- so located is
// simply whether the resolved text is non-empty. That is what makes a pub_date selector
// against an Atom entry carrying only <updated> report honestly that nothing was found.
func (c *feedContainer) value(f *FieldSpec) (string, bool, error) {
	if f.scope {
		return c.scopeText(), true, nil
	}
	v := c.entry.fieldText(f.Selector)
	// No error is possible: the feed-field vocabulary is closed and each name resolves
	// to exactly one value per entry, so FieldSpec.Multiple has nothing to govern here.
	return v, v != "", nil
}

// excerpt returns the evidence text for this entry.
func (c *feedContainer) excerpt() string {
	spec := &c.cfg.Spec.Extract
	if !spec.excerptScope && spec.excerptGroup != "" {
		if v := c.entry.fieldText(spec.excerptGroup); v != "" {
			return collapseWhitespace(v)
		}
	}
	return collapseWhitespace(c.scopeText())
}

// scopeText is the ":scope" join from collector-config-spec.md §4.4: title, link,
// pub_date and description, single-spaced, empty parts skipped, whitespace collapsed.
// The order is fixed and documented so an evidence excerpt is reproducible.
func (c *feedContainer) scopeText() string {
	fields := [...]string{
		c.entry.fieldText(FeedFieldTitle),
		c.entry.fieldText(FeedFieldLink),
		c.entry.fieldText(FeedFieldPubDate),
		c.entry.fieldText(FeedFieldDescription),
	}
	parts := make([]string, 0, len(fields))
	for _, v := range fields {
		if v != "" {
			parts = append(parts, v)
		}
	}
	return collapseWhitespace(strings.Join(parts, " "))
}
