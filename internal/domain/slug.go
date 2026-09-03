package domain

import (
	"regexp"
	"strings"
	"unicode"
)

// slugPattern is the shape enforced by the database CHECK constraints on vendor,
// product, family and category slugs. Slugs are public identifiers: they appear in
// URLs and in API responses, so they are immutable after creation.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidSlug reports whether s is an acceptable public slug.
func ValidSlug(s string) bool {
	return slugPattern.MatchString(s)
}

// Slugify converts a human-readable name into a candidate slug. It is a convenience
// for tooling and registry authoring, not an authority: a slug that Slugify would not
// produce is still valid if it matches ValidSlug, because slugs are chosen
// deliberately and never rewritten.
func Slugify(s string) string {
	var b strings.Builder
	lastDash := true // suppress a leading dash
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case unicode.IsSpace(r), r == '-', r == '_', r == '.', r == '/', r == '+':
			if !lastDash && b.Len() > 0 {
				b.WriteRune('-')
				lastDash = true
			}
		default:
			// Drop anything else rather than transliterating. A wrong
			// transliteration in a public URL is worse than a shorter slug.
		}
	}
	return strings.Trim(b.String(), "-")
}

// NormalizeAlias produces the comparison form used for alias matching: lowercase,
// with runs of separator characters collapsed to a single space and surrounding
// whitespace removed. It deliberately preserves digits and letters only, so that
// "RB5009UG+S+IN", "rb5009ug s in" and "RB5009UG+S+IN " all normalise alike.
func NormalizeAlias(s string) string {
	var b strings.Builder
	lastSpace := true
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
			lastSpace = false
		default:
			if !lastSpace && b.Len() > 0 {
				b.WriteRune(' ')
				lastSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}
