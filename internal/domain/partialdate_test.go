package domain_test

import (
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/domain"
)

func TestPartialDateRendersOnlyKnownPrecision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		make func() (domain.PartialDate, error)
		want string
	}{
		{"exact day", func() (domain.PartialDate, error) { return domain.NewExactDate(2026, time.September, 3) }, "2026-09-03"},
		{"month only", func() (domain.PartialDate, error) { return domain.NewMonthDate(2026, time.February) }, "2026-02"},
		{"year only", func() (domain.PartialDate, error) { return domain.NewYearDate(2026) }, "2026"},
		{"unknown", func() (domain.PartialDate, error) { return domain.UnknownDate, nil }, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := tc.make()
			if err != nil {
				t.Fatalf("construction failed: %v", err)
			}
			if got := d.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The central promise of this type: a month-precision date must not expose a day.
func TestMonthPrecisionHasNoDay(t *testing.T) {
	t.Parallel()
	d, err := domain.NewMonthDate(2026, time.February)
	if err != nil {
		t.Fatalf("NewMonthDate: %v", err)
	}
	if _, ok := d.ExactDay(); ok {
		t.Fatal("ExactDay() succeeded on a month-precision date; a day was invented")
	}
	if m, ok := d.Month(); !ok || m != time.February {
		t.Errorf("Month() = %v, %v; want February, true", m, ok)
	}
	if y, ok := d.Year(); !ok || y != 2026 {
		t.Errorf("Year() = %v, %v; want 2026, true", y, ok)
	}
}

func TestYearPrecisionExposesNeitherDayNorMonth(t *testing.T) {
	t.Parallel()
	d, err := domain.NewYearDate(2026)
	if err != nil {
		t.Fatalf("NewYearDate: %v", err)
	}
	if _, ok := d.ExactDay(); ok {
		t.Error("ExactDay() succeeded on a year-precision date")
	}
	if _, ok := d.Month(); ok {
		t.Error("Month() succeeded on a year-precision date; a month was invented")
	}
}

func TestUnknownDateExposesNothing(t *testing.T) {
	t.Parallel()
	var zero domain.PartialDate // the zero value must behave as unknown
	for _, d := range []domain.PartialDate{domain.UnknownDate, zero} {
		if d.Known() {
			t.Error("Known() = true for an unknown date")
		}
		if d.Precision() != domain.PrecisionUnknown {
			t.Errorf("Precision() = %q, want unknown", d.Precision())
		}
		if _, ok := d.Year(); ok {
			t.Error("Year() succeeded on an unknown date")
		}
		if got := d.String(); got != "" {
			t.Errorf("String() = %q, want empty", got)
		}
	}
}

func TestNewPartialDateRejectsNonCanonicalAnchors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		anchor    time.Time
		precision domain.DatePrecision
	}{
		{
			"month precision anchored off day 1",
			time.Date(2026, time.February, 17, 0, 0, 0, 0, time.UTC),
			domain.PrecisionMonthOnly,
		},
		{
			"year precision anchored off January",
			time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC),
			domain.PrecisionYearOnly,
		},
		{
			"unknown precision carrying a date",
			time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
			domain.PrecisionUnknown,
		},
		{
			"exact precision with no date",
			time.Time{},
			domain.PrecisionExactDay,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := domain.NewPartialDate(tc.anchor, tc.precision); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestNewExactDateRejectsImpossibleDates(t *testing.T) {
	t.Parallel()
	// time.Date silently normalises 31 February into 3 March. Rejecting rather than
	// accepting is the whole point.
	if _, err := domain.NewExactDate(2026, time.February, 31); err == nil {
		t.Fatal("31 February was accepted")
	}
}

func TestParsePartialDateInfersPrecisionFromShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in            string
		wantPrecision domain.DatePrecision
		wantString    string
	}{
		{"2026-09-03", domain.PrecisionExactDay, "2026-09-03"},
		{"2026-02", domain.PrecisionMonthOnly, "2026-02"},
		{"2026", domain.PrecisionYearOnly, "2026"},
		{"", domain.PrecisionUnknown, ""},
		{"  2026-09-03  ", domain.PrecisionExactDay, "2026-09-03"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			d, err := domain.ParsePartialDate(tc.in)
			if err != nil {
				t.Fatalf("ParsePartialDate(%q): %v", tc.in, err)
			}
			if d.Precision() != tc.wantPrecision {
				t.Errorf("precision = %q, want %q", d.Precision(), tc.wantPrecision)
			}
			if d.String() != tc.wantString {
				t.Errorf("String() = %q, want %q", d.String(), tc.wantString)
			}
		})
	}
}

func TestParsePartialDateRejectsAmbiguousFormats(t *testing.T) {
	t.Parallel()
	// Guessing at an ambiguous format is how wrong dates enter a catalogue.
	for _, in := range []string{"03/09/2026", "Sept 2026", "2026-9-3", "20260903"} {
		if _, err := domain.ParsePartialDate(in); err == nil {
			t.Errorf("ParsePartialDate(%q) succeeded; ambiguous formats must be rejected", in)
		}
	}
}

func TestPartialDateRoundTripsThroughPersistenceShape(t *testing.T) {
	t.Parallel()
	original, err := domain.NewMonthDate(2026, time.February)
	if err != nil {
		t.Fatalf("NewMonthDate: %v", err)
	}
	restored, err := domain.NewPartialDate(original.Anchor(), original.Precision())
	if err != nil {
		t.Fatalf("NewPartialDate: %v", err)
	}
	if restored.String() != original.String() || restored.Precision() != original.Precision() {
		t.Errorf("round trip changed the value: %q/%q -> %q/%q",
			original.String(), original.Precision(), restored.String(), restored.Precision())
	}
}

// TestPeriodEndIsTheLastDayThePrecisionCouldMean is the revert detector for the
// windowing fix. If PeriodEnd ever returns the anchor again -- which is what the
// release-history window compared against before -- a release the vendor dated only
// "2025" is hidden from every window opening after 1 January 2025, and a release dated
// "2025-09" from every window opening after the 1st of that month.
func TestPeriodEndIsTheLastDayThePrecisionCouldMean(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		make func() (domain.PartialDate, error)
		want string
	}{
		{"exact day is itself", func() (domain.PartialDate, error) { return domain.NewExactDate(2025, time.September, 5) }, "2025-09-05"},
		{"month ends on its last day", func() (domain.PartialDate, error) { return domain.NewMonthDate(2025, time.September) }, "2025-09-30"},
		{"a 31-day month", func() (domain.PartialDate, error) { return domain.NewMonthDate(2025, time.January) }, "2025-01-31"},
		{"february in a common year", func() (domain.PartialDate, error) { return domain.NewMonthDate(2025, time.February) }, "2025-02-28"},
		{"february in a leap year", func() (domain.PartialDate, error) { return domain.NewMonthDate(2024, time.February) }, "2024-02-29"},
		{"year ends on 31 december", func() (domain.PartialDate, error) { return domain.NewYearDate(2025) }, "2025-12-31"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := tc.make()
			if err != nil {
				t.Fatalf("build date: %v", err)
			}
			got := d.PeriodEnd().Format("2006-01-02")
			if got != tc.want {
				t.Errorf("PeriodEnd() = %s, want %s", got, tc.want)
			}
			// The bound is never earlier than the anchor, and for a reduced
			// precision it is strictly later: that gap is the whole point.
			if d.PeriodEnd().Before(d.Anchor()) {
				t.Errorf("PeriodEnd() %s is before Anchor() %s", got, d.Anchor().Format("2006-01-02"))
			}
			if d.Precision() != domain.PrecisionExactDay && !d.PeriodEnd().After(d.Anchor()) {
				t.Errorf("PeriodEnd() = Anchor() = %s at %s precision; the period has no width, so a window would hide it",
					got, d.Precision())
			}
		})
	}

	// An unknown date denotes no period at all, so it has no end. The window falls
	// back to first_observed_at for exactly this case.
	if !domain.UnknownDate.PeriodEnd().IsZero() {
		t.Errorf("UnknownDate.PeriodEnd() = %v, want the zero time", domain.UnknownDate.PeriodEnd())
	}
}
