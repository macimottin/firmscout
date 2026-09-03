package domain

import (
	"strconv"
	"strings"
	"unicode"
)

// VersionString is a vendor version as published, together with the normalised form
// FirmScout matches on.
//
// The critical property of this type is what it does NOT provide: there is no Compare
// method, no Less method, and no way to sort a slice of VersionString by version. Real
// vendor versions are not semantic versions. The brief's own examples include
// "3.003.0015.001" and "CollabOS 2.1.B (2.1.121)"; MikroTik's RouterOS uses "7.24.2";
// some vendors use dates, some use BIOS revision strings, some use build numbers. Any
// ordering imposed on these is a guess, and a wrong "this one is newer" is not a
// cosmetic bug for a product whose entire value is being right about current versions.
//
// "Latest observed" is derived from release date, first-observed timestamp and channel.
// See ADR-0017.
//
// Tokenisation exists solely to answer a different, narrower question: is a transition
// from one version to another plausible? An implausible transition routes a candidate
// to human review; it never decides ordering.
type VersionString struct {
	raw        string
	normalized string
}

// NewVersionString builds a VersionString from a raw vendor value. The normalised form
// is the raw value trimmed and lowercased with internal whitespace collapsed; vendor
// specific normalisation beyond that belongs in the collector configuration, which can
// supply an explicit normalised value through NewVersionStringWithNormalized.
func NewVersionString(raw string) (VersionString, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return VersionString{}, invalid("version", "must not be empty")
	}
	if len(trimmed) > 200 {
		return VersionString{}, invalid("version", "exceeds 200 characters")
	}
	return VersionString{raw: trimmed, normalized: defaultNormalizeVersion(trimmed)}, nil
}

// NewVersionStringWithNormalized builds a VersionString where the collector has
// supplied its own normalised form, for vendors whose published string carries
// decoration that should not participate in matching.
func NewVersionStringWithNormalized(raw, normalized string) (VersionString, error) {
	v, err := NewVersionString(raw)
	if err != nil {
		return VersionString{}, err
	}
	n := strings.TrimSpace(normalized)
	if n == "" {
		return VersionString{}, invalid("normalized_version", "must not be empty")
	}
	v.normalized = n
	return v, nil
}

func defaultNormalizeVersion(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// Raw returns the version exactly as the vendor published it.
func (v VersionString) Raw() string { return v.raw }

// Normalized returns the form used for equality and duplicate detection.
func (v VersionString) Normalized() string { return v.normalized }

// String returns the raw form.
func (v VersionString) String() string { return v.raw }

// Equal reports whether two versions are the same release. Equality is defined on the
// normalised form. Equality is meaningful; ordering is not.
func (v VersionString) Equal(other VersionString) bool {
	return v.normalized == other.normalized
}

// IsZero reports whether the version was never set.
func (v VersionString) IsZero() bool { return v.raw == "" }

// Token is one lexical unit of a version string: either a run of digits or a run of
// non-digits.
type Token struct {
	Text    string
	Numeric bool
	Value   int64 // valid only when Numeric is true and the run fits in an int64
}

// Tokens splits a version into alternating numeric and non-numeric runs, ignoring
// separator characters. It is exported for plausibility analysis and for tests; it is
// not an ordering primitive.
func (v VersionString) Tokens() []Token {
	var out []Token
	var cur strings.Builder
	curNumeric := false
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		t := Token{Text: cur.String(), Numeric: curNumeric}
		if curNumeric {
			if n, err := strconv.ParseInt(t.Text, 10, 64); err == nil {
				t.Value = n
			} else {
				// A digit run too long for int64 is still a token; it simply has
				// no usable numeric value for plausibility purposes.
				t.Numeric = false
			}
		}
		out = append(out, t)
		cur.Reset()
	}
	for _, r := range v.normalized {
		switch {
		case unicode.IsDigit(r):
			if !curNumeric {
				flush()
				curNumeric = true
			}
			cur.WriteRune(r)
		case unicode.IsLetter(r):
			if curNumeric {
				flush()
				curNumeric = false
			}
			cur.WriteRune(r)
		default:
			flush()
			curNumeric = false
		}
	}
	flush()
	return out
}

// TransitionPlausibility is the verdict of comparing a candidate version against the
// most recently observed one for the same product and channel.
type TransitionPlausibility string

const (
	// TransitionPlausible means nothing about the shape of the change is suspicious.
	TransitionPlausible TransitionPlausibility = "plausible"
	// TransitionIdentical means the versions are equal after normalisation.
	TransitionIdentical TransitionPlausibility = "identical"
	// TransitionSuspicious means the change should be reviewed by a human before
	// publication. It is never grounds for silent rejection.
	TransitionSuspicious TransitionPlausibility = "suspicious"
	// TransitionUncomparable means the two versions have no shared structure, so no
	// plausibility judgement is possible. Like suspicious, it routes to review.
	TransitionUncomparable TransitionPlausibility = "uncomparable"
)

// PlausibilityReport explains a verdict so a reviewer sees the reasoning rather than a
// bare label.
type PlausibilityReport struct {
	Verdict TransitionPlausibility
	Reason  string
}

// AssessTransition judges whether moving from prev to v looks like a normal vendor
// release progression.
//
// It answers "does this need a human to look at it?", never "which of these is newer".
// A verdict of TransitionSuspicious routes the candidate to the review queue; it does
// not reject it, because vendors do legitimately renumber their products.
func AssessTransition(prev, v VersionString) PlausibilityReport {
	if prev.IsZero() {
		return PlausibilityReport{TransitionPlausible, "no previously observed version for this product and channel"}
	}
	if prev.Equal(v) {
		return PlausibilityReport{TransitionIdentical, "identical to the previously observed version"}
	}

	pt, vt := prev.Tokens(), v.Tokens()
	if len(pt) == 0 || len(vt) == 0 {
		return PlausibilityReport{TransitionUncomparable, "one of the versions contains no recognisable tokens"}
	}

	// A change in the non-numeric skeleton usually means the vendor changed its
	// numbering scheme, which is exactly the case a human should confirm.
	if skeleton(pt) != skeleton(vt) {
		return PlausibilityReport{
			TransitionUncomparable,
			"version shape changed from " + skeletonDisplay(pt) + " to " + skeletonDisplay(vt),
		}
	}

	// Compare the first numeric token, which is the component vendors change least
	// often and most meaningfully.
	pn, pok := firstNumeric(pt)
	vn, vok := firstNumeric(vt)
	if !pok || !vok {
		return PlausibilityReport{TransitionPlausible, "no numeric component to compare"}
	}
	switch {
	case vn < pn:
		return PlausibilityReport{
			TransitionSuspicious,
			"leading numeric component decreased from " + strconv.FormatInt(pn, 10) + " to " + strconv.FormatInt(vn, 10),
		}
	case vn > pn+5:
		return PlausibilityReport{
			TransitionSuspicious,
			"leading numeric component jumped from " + strconv.FormatInt(pn, 10) + " to " + strconv.FormatInt(vn, 10),
		}
	default:
		return PlausibilityReport{TransitionPlausible, "consistent with the previously observed version"}
	}
}

// skeleton returns the non-numeric structure of a token list, which is what changes
// when a vendor switches numbering scheme.
func skeleton(tokens []Token) string {
	var b strings.Builder
	for _, t := range tokens {
		if t.Numeric {
			b.WriteByte('#')
		} else {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

func skeletonDisplay(tokens []Token) string {
	s := skeleton(tokens)
	if s == "" {
		return "(empty)"
	}
	return s
}

func firstNumeric(tokens []Token) (int64, bool) {
	for _, t := range tokens {
		if t.Numeric {
			return t.Value, true
		}
	}
	return 0, false
}
