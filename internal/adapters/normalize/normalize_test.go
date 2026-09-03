package normalize

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

// mikrotikPage renders a stand-in for mikrotik.com/download/changelogs: a Livewire and
// Alpine page whose framework attributes, analytics identifiers and rotating furniture
// change on every render, wrapped around the changelog entries that actually matter.
func mikrotikPage(wireID, sessionToken, adSlot, visitorCount string, entries []string) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><title>Changelogs</title>`)
	b.WriteString(`<meta name="csrf-token" content="` + sessionToken + `">`)
	b.WriteString(`<script nonce="` + sessionToken + `">window.__ANALYTICS_ID="` + sessionToken + `";</script>`)
	b.WriteString(`<style>.changelog-header{color:#` + adSlot + `}</style>`)
	b.WriteString(`</head><body>`)
	b.WriteString(`<!-- rendered at 2026-09-03T12:00:00Z build ` + sessionToken + ` -->`)
	b.WriteString(`<nav class="site-nav"><a href="/">Home</a><a href="/download">Download</a></nav>`)
	b.WriteString(`<div class="cookie-banner">We use cookies. Region: ` + adSlot + `</div>`)
	b.WriteString(`<div class="ad-slot">Sponsored: ` + adSlot + `</div>`)
	b.WriteString(`<div wire:id="` + wireID + `" x-data="{open:false,token:'` + sessionToken + `'}">`)
	for _, e := range entries {
		b.WriteString(`<div class="changelog-header" id="entry-` + wireID + `" data-livewire-key="` + wireID + `">`)
		b.WriteString(e)
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)
	b.WriteString(`<footer>You are visitor #` + visitorCount + `. Page generated in ` + adSlot + `ms.</footer>`)
	b.WriteString(`<noscript>Enable JavaScript, ` + sessionToken + `</noscript>`)
	b.WriteString(`</body></html>`)
	return b.String()
}

var baseEntries = []string{
	`<span class="version">7.24.2</span><span class="channel">stable</span><span class="date">2026-08-14</span>`,
	`<span class="version">7.25beta3</span><span class="channel">beta</span><span class="date">2026-08-28</span>`,
}

// TestMikroTikSectionHashIsStable is the worked example from
// docs/architecture/update-pipeline.md. Two renders of the same content differ in every
// volatile way a Livewire/Alpine page differs, and the section hash must not move.
func TestMikroTikSectionHashIsStable(t *testing.T) {
	t.Parallel()
	n := New()

	renderA := mikrotikPage("wire-8fa31c02", "b91d77e4a0", "aa11bb", "1042812", baseEntries)
	renderB := mikrotikPage("wire-4d90ee71", "0c3fa8b291", "ff99cc", "1042901", baseEntries)

	const selector = "div.changelog-header"
	const strippable = "nav.site-nav, div.cookie-banner, div.ad-slot, footer"

	normA, hashA, err := n.Normalize("text/html; charset=utf-8", []byte(renderA), selector, strings.Split(strippable, ", "))
	if err != nil {
		t.Fatalf("Normalize(A): %v", err)
	}
	normB, hashB, err := n.Normalize("text/html; charset=utf-8", []byte(renderB), selector, strings.Split(strippable, ", "))
	if err != nil {
		t.Fatalf("Normalize(B): %v", err)
	}

	if hashA != hashB {
		t.Fatalf("section hash moved between two renders of identical content:\nA=%s\n  %q\nB=%s\n  %q", hashA, normA, hashB, normB)
	}
	if !strings.Contains(string(normA), "7.24.2") || !strings.Contains(string(normA), "2026-08-28") {
		t.Fatalf("normalised section lost the content that matters: %q", normA)
	}
	if strings.Contains(string(normA), "Sponsored") || strings.Contains(string(normA), "visitor #") {
		t.Fatalf("normalised section leaked page furniture: %q", normA)
	}

	// The test is only meaningful if whole-page hashing really would have moved, which
	// is the entire reason the section selector exists.
	_, wholeA, err := n.Normalize("text/html", []byte(renderA), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, wholeB, err := n.Normalize("text/html", []byte(renderB), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if wholeA == wholeB {
		t.Fatal("whole-page hashes matched; the fixture does not reproduce the MikroTik problem")
	}
}

// TestMikroTikSectionHashMovesOnRealChange is the other half: a genuine new release must
// change the hash, or the source reports "unchanged" forever.
func TestMikroTikSectionHashMovesOnRealChange(t *testing.T) {
	t.Parallel()
	n := New()
	const selector = "div.changelog-header"

	before := mikrotikPage("wire-8fa31c02", "b91d77e4a0", "aa11bb", "1042812", baseEntries)
	newEntry := `<span class="version">7.25.1</span><span class="channel">stable</span><span class="date">2026-09-02</span>`
	after := mikrotikPage("wire-8fa31c02", "b91d77e4a0", "aa11bb", "1042812", append([]string{newEntry}, baseEntries...))

	_, hashBefore, err := n.Normalize("text/html", []byte(before), selector, nil)
	if err != nil {
		t.Fatal(err)
	}
	normAfter, hashAfter, err := n.Normalize("text/html", []byte(after), selector, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hashBefore == hashAfter {
		t.Fatal("a new changelog entry did not change the section hash")
	}
	if !strings.Contains(string(normAfter), "7.25.1") {
		t.Fatalf("the new entry is missing from the normalised text: %q", normAfter)
	}

	// An edit inside an existing entry must also register.
	edited := mikrotikPage("wire-8fa31c02", "b91d77e4a0", "aa11bb", "1042812", []string{
		`<span class="version">7.24.3</span><span class="channel">stable</span><span class="date">2026-08-14</span>`,
		baseEntries[1],
	})
	_, hashEdited, err := n.Normalize("text/html", []byte(edited), selector, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hashEdited == hashBefore {
		t.Fatal("an edit inside the section did not change the hash")
	}
}

func TestNormalizeStripsScriptAndStyleFromTheHash(t *testing.T) {
	t.Parallel()
	n := New()

	withScript := `<html><body><p>RouterOS 7.25.1</p>` +
		`<script>var t="` + strings.Repeat("x", 40) + `";</script>` +
		`<style>body{background:#` + "abc123" + `}</style>` +
		`<noscript>fallback noise</noscript>` +
		`<iframe src="https://ads.example/frame"></iframe>` +
		`<svg><title>icon</title></svg>` +
		`<!-- build 8fa31c02 --></body></html>`
	plain := `<html><body><p>RouterOS 7.25.1</p></body></html>`

	normA, hashA, err := n.Normalize("text/html", []byte(withScript), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, hashB, err := n.Normalize("text/html", []byte(plain), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if hashA != hashB {
		t.Fatalf("script/style/comment content reached the hash: %q", normA)
	}
	for _, leaked := range []string{"var t", "background", "fallback noise", "build 8fa31c02", "icon"} {
		if strings.Contains(string(normA), leaked) {
			t.Errorf("normalised text contains %q", leaked)
		}
	}
}

func TestNormalizeStripSelectors(t *testing.T) {
	t.Parallel()
	n := New()
	page := `<html><body><div class="banner">Accept cookies</div><main>RouterOS 7.25.1</main></body></html>`

	_, withBanner, err := n.Normalize("text/html", []byte(page), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	norm, stripped, err := n.Normalize("text/html", []byte(page), "", []string{"div.banner", "", "   "})
	if err != nil {
		t.Fatal(err)
	}
	if withBanner == stripped {
		t.Fatal("the strip selector had no effect")
	}
	if string(norm) != "RouterOS 7.25.1" {
		t.Fatalf("normalised = %q", norm)
	}
}

func TestNormalizeSectionSelectorMissIsAnError(t *testing.T) {
	t.Parallel()
	n := New()
	_, _, err := n.Normalize("text/html", []byte(`<html><body><p>hi</p></body></html>`), "div.changelog-header", nil)
	if !errors.Is(err, ErrSelectorNoMatch) {
		t.Fatalf("err = %v, want ErrSelectorNoMatch: a selector that stops matching must be loud, not a stable empty hash", err)
	}
}

func TestNormalizeSectionBoundariesAreHashed(t *testing.T) {
	t.Parallel()
	n := New()
	a := `<html><body><i class="s">ab</i><i class="s">c</i></body></html>`
	b := `<html><body><i class="s">a</i><i class="s">bc</i></body></html>`

	_, hashA, err := n.Normalize("text/html", []byte(a), "i.s", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, hashB, err := n.Normalize("text/html", []byte(b), "i.s", nil)
	if err != nil {
		t.Fatal(err)
	}
	if hashA == hashB {
		t.Fatal("different section splits collided; the join separator is not hashed")
	}
}

func TestNormalizeNonHTML(t *testing.T) {
	t.Parallel()
	n := New()

	tests := []struct {
		name        string
		contentType string
		body        string
		want        string
	}{
		{name: "json", contentType: "application/json", body: "{\n  \"version\":  \"7.25.1\"\n}\n", want: `{ "version": "7.25.1" }`},
		{name: "plain text", contentType: "text/plain; charset=utf-8", body: "  7.25.1  \r\n", want: "7.25.1"},
		{name: "html-looking text is not parsed", contentType: "text/plain", body: "<p>7.25.1</p>", want: "<p>7.25.1</p>"},
		{name: "xml", contentType: "application/xml", body: "<rss>\n<item>7.25.1</item>\n</rss>", want: "<rss> <item>7.25.1</item> </rss>"},
		{name: "unknown", contentType: "", body: "a\t\tb", want: "a b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			norm, hash, err := n.Normalize(tc.contentType, []byte(tc.body), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if string(norm) != tc.want {
				t.Fatalf("normalised = %q, want %q", norm, tc.want)
			}
			if len(hash) != 64 {
				t.Fatalf("hash %q is not a sha-256 hex digest", hash)
			}
		})
	}
}

func TestNormalizeIsDeterministicAndSHA256(t *testing.T) {
	t.Parallel()
	n := New()
	body := []byte("  RouterOS   7.25.1  ")
	norm, h1, err := n.Normalize("text/plain", body, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, h2, err := n.Normalize("text/plain", body, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatal("normalisation is not deterministic")
	}
	sum := sha256.Sum256(norm)
	if want := hex.EncodeToString(sum[:]); h1 != want {
		t.Fatalf("hash = %s, want sha-256 of the normalised bytes %s", h1, want)
	}
	if string(norm) != "RouterOS 7.25.1" {
		t.Fatalf("normalised = %q", norm)
	}
}

func TestCollapseWhitespace(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{in: "  a   b  ", want: "a b"},
		{in: "a\n\n\tb", want: "a b"},
		{in: "a b", want: "a b"},
		{in: "a\u200bb", want: "ab"},
		{in: "\ufeffRouterOS", want: "RouterOS"},
		{in: "", want: ""},
		{in: "   ", want: ""},
	}
	for _, tc := range tests {
		if got := collapseWhitespace(tc.in); got != tc.want {
			t.Errorf("collapseWhitespace(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLooksGenerated(t *testing.T) {
	t.Parallel()
	generated := []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"8fa31c02",
		"deadbeefcafebabe",
		":r1a:",
		"wire9f3a1b7c",
		"comp-4f2a9d1e",
	}
	authored := []string{
		"main",
		"changelog",
		"nav",
		"v7",
		"download-page",
		"header",
	}
	for _, v := range generated {
		if !looksGenerated(v) {
			t.Errorf("looksGenerated(%q) = false, want true", v)
		}
	}
	for _, v := range authored {
		if looksGenerated(v) {
			t.Errorf("looksGenerated(%q) = true, want false", v)
		}
	}
}

func TestVolatileAttributesAreRemoved(t *testing.T) {
	t.Parallel()
	// Attribute cleaning is verified through the parsed tree rather than the hash,
	// because the normalised form is text; see the note on cleanNode.
	page := fmt.Sprintf(`<html><body><div id="entry-8fa31c02" wire:id="%s" x-data="{a:1}" nonce="%s" integrity="sha384-abc" data-v-9f3a1b7c="" class="changelog-header" data-testid="entry">x</div></body></html>`,
		"9f3a1b7c", "b91d77e4a0")

	kept, removed := attributesAfterCleaning(t, page, "div")
	for _, name := range []string{"wire:id", "x-data", "nonce", "integrity", "data-v-9f3a1b7c", "id"} {
		if !removed[name] {
			t.Errorf("attribute %q survived cleaning (kept: %v)", name, kept)
		}
	}
	if !kept["class"] {
		t.Error("class was removed; only volatile attributes should be")
	}
	if !kept["data-testid"] {
		t.Error("a stable data attribute was removed")
	}
}

// attributesAfterCleaning parses a document, runs the cleaner, and reports which
// attributes survived on the first element matching sel.
func attributesAfterCleaning(t *testing.T, page, sel string) (kept, removed map[string]bool) {
	t.Helper()
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader([]byte(page)))
	if err != nil {
		t.Fatal(err)
	}
	before := map[string]bool{}
	node := doc.Find(sel).Nodes[0]
	for _, a := range node.Attr {
		before[attrName(a)] = true
	}
	for _, root := range doc.Nodes {
		cleanNode(root)
	}
	kept = map[string]bool{}
	for _, a := range node.Attr {
		kept[attrName(a)] = true
	}
	removed = map[string]bool{}
	for name := range before {
		if !kept[name] {
			removed[name] = true
		}
	}
	return kept, removed
}

func attrName(a html.Attribute) string {
	if a.Namespace != "" {
		return strings.ToLower(a.Namespace + ":" + a.Key)
	}
	return strings.ToLower(a.Key)
}

// TestPathologicallyNestedHTMLIsRejectedNotFatal covers T-04 (resource exhaustion): a
// hostile document that is nested far deeper than any real page must produce an error
// the caller can map to parser_failed, not a stack overflow. The 512-element limit comes
// from golang.org/x/net/html itself; this test pins the behaviour so that a dependency
// change which removed it would be caught here rather than in production.
func TestPathologicallyNestedHTMLIsRejectedNotFatal(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("<div>", 100000) + "7.25.1" + strings.Repeat("</div>", 100000)
	_, _, err := New().Normalize("text/html", []byte(body), "", nil)
	if err == nil {
		t.Fatal("a 100000-deep document was accepted; nesting is unbounded")
	}
	if !strings.Contains(err.Error(), "parsing html") {
		t.Fatalf("err = %v, want a parse error", err)
	}
}
