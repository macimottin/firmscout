package domain_test

import (
	"testing"

	"github.com/macimottin/firmscout/internal/domain"
)

// The type must handle the version shapes real vendors actually publish, none of which
// are semantic versions.
func TestVersionStringAcceptsRealVendorShapes(t *testing.T) {
	t.Parallel()
	shapes := []string{
		"7.24.2",                   // MikroTik RouterOS
		"3.003.0015.001",           // brief example
		"CollabOS 2.1.B (2.1.121)", // brief example
		"2026.24.5",                // date-like
		"4.6.2.460046",             // Poly G7500
		"1.14.0-beta3",
		"A21",      // BIOS revision string
		"R1.0.7",   // proprietary build
		"20260903", // date as a build number
	}
	for _, s := range shapes {
		v, err := domain.NewVersionString(s)
		if err != nil {
			t.Errorf("NewVersionString(%q) failed: %v", s, err)
			continue
		}
		if v.Raw() != s {
			t.Errorf("Raw() = %q, want %q", v.Raw(), s)
		}
		if v.Normalized() == "" {
			t.Errorf("Normalized() empty for %q", s)
		}
	}
}

func TestVersionStringRejectsEmpty(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "   ", "\t\n"} {
		if _, err := domain.NewVersionString(s); err == nil {
			t.Errorf("NewVersionString(%q) succeeded; empty versions must be rejected", s)
		}
	}
}

func TestVersionEqualityUsesNormalizedForm(t *testing.T) {
	t.Parallel()
	a, _ := domain.NewVersionString("7.24.2")
	b, _ := domain.NewVersionString(" 7.24.2 ")
	c, _ := domain.NewVersionString("7.24.3")
	if !a.Equal(b) {
		t.Error("versions differing only by surrounding whitespace should be equal")
	}
	if a.Equal(c) {
		t.Error("different versions compared equal")
	}
}

func TestTokensSplitsNumericAndNonNumericRuns(t *testing.T) {
	t.Parallel()
	v, _ := domain.NewVersionString("CollabOS 2.1.B (2.1.121)")
	tokens := v.Tokens()
	if len(tokens) == 0 {
		t.Fatal("no tokens produced")
	}
	if tokens[0].Numeric {
		t.Errorf("first token %q should be non-numeric", tokens[0].Text)
	}
	var sawNumeric bool
	for _, tk := range tokens {
		if tk.Numeric && tk.Value > 0 {
			sawNumeric = true
		}
	}
	if !sawNumeric {
		t.Error("expected at least one numeric token with a parsed value")
	}
}

func TestAssessTransitionPlausibility(t *testing.T) {
	t.Parallel()
	mk := func(s string) domain.VersionString {
		v, err := domain.NewVersionString(s)
		if err != nil {
			t.Fatalf("NewVersionString(%q): %v", s, err)
		}
		return v
	}

	tests := []struct {
		name string
		prev string
		next string
		want domain.TransitionPlausibility
	}{
		{"normal patch bump", "7.24.2", "7.24.3", domain.TransitionPlausible},
		{"normal minor bump", "7.24.2", "7.25.0", domain.TransitionPlausible},
		{"identical", "7.24.2", "7.24.2", domain.TransitionIdentical},
		{"large regression", "7.24.2", "1.0.0", domain.TransitionSuspicious},
		{"implausible jump", "7.24.2", "99.0.0", domain.TransitionSuspicious},
		{"scheme change", "7.24.2", "CollabOS 2.1.B", domain.TransitionUncomparable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := domain.AssessTransition(mk(tc.prev), mk(tc.next))
			if got.Verdict != tc.want {
				t.Errorf("AssessTransition(%q -> %q) = %q (%s), want %q",
					tc.prev, tc.next, got.Verdict, got.Reason, tc.want)
			}
			if got.Reason == "" {
				t.Error("verdict carried no reason; a reviewer needs the reasoning")
			}
		})
	}
}

func TestAssessTransitionWithNoPreviousVersionIsPlausible(t *testing.T) {
	t.Parallel()
	next, _ := domain.NewVersionString("7.24.2")
	got := domain.AssessTransition(domain.VersionString{}, next)
	if got.Verdict != domain.TransitionPlausible {
		t.Errorf("first observed release judged %q; it must be plausible", got.Verdict)
	}
}

func TestNormalizeAliasCollapsesSeparators(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"RB5009UG+S+IN", "rb5009ug s in"},
		{"  RB5009UG+S+IN  ", "rb5009ug s in"},
		{"Poly G7500", "poly g7500"},
		{"G-7500", "g 7500"},
	}
	for _, tc := range tests {
		if got := domain.NormalizeAlias(tc.in); got != tc.want {
			t.Errorf("NormalizeAlias(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSlugifyAndValidSlug(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"MikroTik", "mikrotik"},
		{"Poly G7500", "poly-g7500"},
		{"Palo Alto Networks", "palo-alto-networks"},
		{"  Schneider  Electric ", "schneider-electric"},
	}
	for _, tc := range tests {
		got := domain.Slugify(tc.in)
		if got != tc.want {
			t.Errorf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if !domain.ValidSlug(got) {
			t.Errorf("Slugify produced an invalid slug: %q", got)
		}
	}
	for _, bad := range []string{"", "-leading", "trailing-", "Upper", "has space", "double--dash"} {
		if domain.ValidSlug(bad) {
			t.Errorf("ValidSlug(%q) = true, want false", bad)
		}
	}
}
