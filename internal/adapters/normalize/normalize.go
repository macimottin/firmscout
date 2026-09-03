// Package normalize reduces fetched content to the canonical text that change
// detection hashes.
//
// Normalisation is what makes change detection meaningful. The worked example is the
// MikroTik changelog page (docs/architecture/update-pipeline.md): roughly 409 KB,
// server-rendered with Livewire and Alpine, "cache-control: private", no ETag. Hashing
// the whole document reports a change on every single check, because the framework's
// per-render identifiers move even when the visible content does not. Hashing only the
// elements matched by a section selector is stable across the page's volatile
// furniture and changes exactly when a changelog entry does.
package normalize

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/PuerkitoBio/goquery"
	"github.com/macimottin/firmscout/internal/application"
	"golang.org/x/net/html"
)

// ErrSelectorNoMatch reports that a non-empty section selector matched nothing.
//
// This is an error rather than an empty hash on purpose. A selector that stops matching
// -- because the vendor restructured the page -- would otherwise produce a stable hash
// of the empty string, and the source would report "unchanged" forever while releases
// went unnoticed. A silent false "unchanged" is the most expensive bug this package can
// have, so it is made loud.
var ErrSelectorNoMatch = errors.New("normalize: section selector matched no elements")

// defaultStripSelectors are removed from every HTML document before hashing. They are
// either non-content (script, style) or content FirmScout must not follow (iframe), and
// all of them carry per-render noise.
var defaultStripSelectors = []string{"script", "style", "noscript", "iframe", "svg", "template"}

// volatileAttributes are removed outright wherever they appear.
var volatileAttributes = map[string]bool{
	"nonce":        true, // CSP per-request nonce
	"integrity":    true, // subresource integrity digest, changes with every asset build
	"x-data":       true, // Alpine client-state blob
	"x-init":       true, // Alpine initialisation expression
	"csrf-token":   true,
	"data-csrf":    true,
	"data-nonce":   true,
	"data-turbo":   true,
	"data-reactid": true,
}

// volatileAttributePrefixes are attribute-name prefixes removed outright. "wire:" is
// Livewire's per-render component state, which is the specific reason the MikroTik page
// cannot be whole-page hashed.
var volatileAttributePrefixes = []string{"wire:"}

var (
	uuidPattern      = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hexPattern       = regexp.MustCompile(`(?i)^[0-9a-f]{8,}$`)
	reactIDPattern   = regexp.MustCompile(`^:r[0-9a-z]+:$`)
	scopedCSSPattern = regexp.MustCompile(`(?i)^data-v-[0-9a-f]{6,}$`)
	tokenPattern     = regexp.MustCompile(`^[A-Za-z0-9_:.-]+$`)
	// zeroWidth characters are dropped entirely: they are invisible, they appear and
	// disappear between renders, and they change a hash without changing the content.
	zeroWidth = map[rune]bool{'\u200b': true, '\u200c': true, '\u200d': true, '\ufeff': true}
)

// Normalizer implements application.Normalizer.
type Normalizer struct {
	stripSelectors []string
}

var _ application.Normalizer = (*Normalizer)(nil)

// Option configures a Normalizer.
type Option func(*Normalizer)

// WithDefaultStripSelectors replaces the built-in strip list.
func WithDefaultStripSelectors(sel []string) Option {
	return func(n *Normalizer) { n.stripSelectors = append([]string(nil), sel...) }
}

// New builds a Normalizer.
func New(opts ...Option) *Normalizer {
	n := &Normalizer{stripSelectors: append([]string(nil), defaultStripSelectors...)}
	for _, o := range opts {
		o(n)
	}
	return n
}

// Normalize returns the canonical text of the content and the SHA-256 hex digest of it.
//
// sectionSelector may be empty, in which case the whole normalised document is hashed.
// strip lists extra CSS selectors removed before normalisation -- cookie banners,
// advertisement containers, navigation chrome, personalisation panels.
func (n *Normalizer) Normalize(contentType string, body []byte, sectionSelector string, strip []string) ([]byte, string, error) {
	if isHTML(contentType) {
		return n.normalizeHTML(body, sectionSelector, strip)
	}
	// text/plain, JSON, XML and everything else: no HTML parsing, because parsing a
	// JSON document as HTML invents structure that is not there.
	normalized := []byte(collapseWhitespace(string(body)))
	return normalized, hashOf(normalized), nil
}

func (n *Normalizer) normalizeHTML(body []byte, sectionSelector string, strip []string) ([]byte, string, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("normalize: parsing html: %w", err)
	}

	for _, sel := range n.stripSelectors {
		doc.Find(sel).Remove()
	}
	for _, sel := range strip {
		sel = strings.TrimSpace(sel)
		if sel == "" {
			continue
		}
		// An unparseable selector is a collector-config error. Removing nothing is the
		// safe response: it may add noise to the hash, but it cannot silently drop
		// content that matters.
		doc.Find(sel).Remove()
	}

	for _, root := range doc.Nodes {
		cleanNode(root)
	}

	if sectionSelector == "" {
		normalized := []byte(collapseWhitespace(doc.Text()))
		return normalized, hashOf(normalized), nil
	}

	sections := doc.Find(sectionSelector)
	if sections.Length() == 0 {
		return nil, "", fmt.Errorf("%w: %q", ErrSelectorNoMatch, sectionSelector)
	}
	parts := make([]string, 0, sections.Length())
	sections.Each(func(_ int, s *goquery.Selection) {
		if t := collapseWhitespace(s.Text()); t != "" {
			parts = append(parts, t)
		}
	})
	// Sections are joined with a newline rather than a space so that the boundary
	// between two sections is part of the hashed bytes: ["ab","c"] and ["a","bc"] are
	// different documents and must not collide.
	normalized := []byte(strings.Join(parts, "\n"))
	return normalized, hashOf(normalized), nil
}

// cleanNode removes comment nodes and volatile attributes from the tree rooted at n.
//
// Attribute cleaning does not change the hash while the normalised form is text, and it
// is done anyway: the attributes named here are the exact ones that make a markup-level
// comparison useless, and a future full-comparison mechanism (the bottom rung of the
// watcher ladder) reads this same normalised tree.
func cleanNode(n *html.Node) {
	for child := n.FirstChild; child != nil; {
		next := child.NextSibling
		switch child.Type {
		case html.CommentNode:
			// Comments carry build ids, render timestamps and cache markers.
			n.RemoveChild(child)
		default:
			cleanNode(child)
		}
		child = next
	}
	if n.Type == html.ElementNode {
		n.Attr = filterAttributes(n.Attr)
	}
}

func filterAttributes(attrs []html.Attribute) []html.Attribute {
	if len(attrs) == 0 {
		return attrs
	}
	kept := attrs[:0]
	for _, a := range attrs {
		if isVolatileAttribute(a) {
			continue
		}
		kept = append(kept, a)
	}
	return kept
}

func isVolatileAttribute(a html.Attribute) bool {
	name := strings.ToLower(a.Key)
	if a.Namespace != "" {
		name = strings.ToLower(a.Namespace) + ":" + name
	}
	if volatileAttributes[name] {
		return true
	}
	for _, p := range volatileAttributePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	if name == "id" {
		return looksGenerated(a.Val)
	}
	if strings.HasPrefix(name, "data-") {
		return scopedCSSPattern.MatchString(name) || looksGenerated(a.Val)
	}
	return false
}

// looksGenerated reports whether a value looks machine-generated per render rather than
// authored: a UUID, a hex digest, a React useId token, or a long alphanumeric string
// mixing letters and digits.
func looksGenerated(v string) bool {
	v = strings.TrimSpace(v)
	// React's useId tokens are short by design (":r1a:"), so they are recognised before
	// the length floor that filters out authored names such as "nav" and "main".
	if reactIDPattern.MatchString(v) {
		return true
	}
	if len(v) < 8 {
		return false
	}
	if uuidPattern.MatchString(v) || hexPattern.MatchString(v) {
		return true
	}
	if !tokenPattern.MatchString(v) {
		return false
	}
	var digits, letters int
	for _, r := range v {
		switch {
		case unicode.IsDigit(r):
			digits++
		case unicode.IsLetter(r):
			letters++
		}
	}
	return digits >= 3 && letters >= 3
}

// collapseWhitespace reduces every run of whitespace to a single space and trims the
// result. Non-breaking spaces are whitespace here, and zero-width characters are
// dropped entirely: both appear and disappear between renders of identical content.
func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pendingSpace := false
	wroteAny := false
	for _, r := range s {
		if zeroWidth[r] {
			continue
		}
		if unicode.IsSpace(r) {
			pendingSpace = wroteAny
			continue
		}
		if pendingSpace {
			b.WriteByte(' ')
			pendingSpace = false
		}
		b.WriteRune(r)
		wroteAny = true
	}
	return b.String()
}

func isHTML(contentType string) bool {
	mt := contentType
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	switch strings.ToLower(strings.TrimSpace(mt)) {
	case "text/html", "application/xhtml+xml":
		return true
	}
	return false
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
