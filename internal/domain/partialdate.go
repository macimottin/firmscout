package domain

import (
	"fmt"
	"strings"
	"time"
)

// DatePrecision records how much of a release date a vendor actually published.
//
// This type exists because vendors frequently date a release "February 2026" or
// "2026" and nothing more. Storing an invented day would manufacture precision the
// source never provided, which the evidence-first principle forbids. See ADR-0017.
type DatePrecision string

const (
	// PrecisionExactDay means the vendor published a specific calendar day.
	PrecisionExactDay DatePrecision = "exact_day"
	// PrecisionMonthOnly means the vendor published a month and year only.
	PrecisionMonthOnly DatePrecision = "month_only"
	// PrecisionYearOnly means the vendor published a year only.
	PrecisionYearOnly DatePrecision = "year_only"
	// PrecisionUnknown means no release date could be determined at all.
	PrecisionUnknown DatePrecision = "unknown"
)

// ValidDatePrecision reports whether p is one of the four declared precisions.
func ValidDatePrecision(p DatePrecision) bool {
	switch p {
	case PrecisionExactDay, PrecisionMonthOnly, PrecisionYearOnly, PrecisionUnknown:
		return true
	}
	return false
}

// PartialDate is a date together with the precision at which it is known.
//
// The zero value is a valid unknown date. The type deliberately exposes no Day()
// accessor: a month-precision date has no day, and offering one would invite callers
// to read the canonical anchor as if it were real data.
type PartialDate struct {
	// anchor is the canonical representation: day 1 for month precision, 1 January
	// for year precision, the zero time for unknown. It is unexported so it can only
	// be set through a constructor that enforces the anchoring rule.
	anchor    time.Time
	precision DatePrecision
}

// UnknownDate is the PartialDate carrying no date information.
var UnknownDate = PartialDate{precision: PrecisionUnknown}

// NewExactDate returns a PartialDate known to the day.
func NewExactDate(year int, month time.Month, day int) (PartialDate, error) {
	if year < 1970 || year > 2200 {
		return PartialDate{}, invalid("release_date", fmt.Sprintf("year %d out of range", year))
	}
	t := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	// time.Date normalises out-of-range components silently; reject rather than accept
	// a date the caller did not mean.
	if t.Year() != year || t.Month() != month || t.Day() != day {
		return PartialDate{}, invalid("release_date", fmt.Sprintf("%04d-%02d-%02d is not a real date", year, month, day))
	}
	return PartialDate{anchor: t, precision: PrecisionExactDay}, nil
}

// NewMonthDate returns a PartialDate known to the month.
func NewMonthDate(year int, month time.Month) (PartialDate, error) {
	if year < 1970 || year > 2200 {
		return PartialDate{}, invalid("release_date", fmt.Sprintf("year %d out of range", year))
	}
	if month < time.January || month > time.December {
		return PartialDate{}, invalid("release_date", fmt.Sprintf("month %d out of range", int(month)))
	}
	return PartialDate{
		anchor:    time.Date(year, month, 1, 0, 0, 0, 0, time.UTC),
		precision: PrecisionMonthOnly,
	}, nil
}

// NewYearDate returns a PartialDate known to the year.
func NewYearDate(year int) (PartialDate, error) {
	if year < 1970 || year > 2200 {
		return PartialDate{}, invalid("release_date", fmt.Sprintf("year %d out of range", year))
	}
	return PartialDate{
		anchor:    time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC),
		precision: PrecisionYearOnly,
	}, nil
}

// NewPartialDate reconstructs a PartialDate from a stored anchor and precision. It is
// the inverse of Anchor and Precision and is used by persistence adapters. It
// re-validates the anchoring rule, so a corrupted row cannot silently become a date
// with false precision.
func NewPartialDate(anchor time.Time, precision DatePrecision) (PartialDate, error) {
	if !ValidDatePrecision(precision) {
		return PartialDate{}, invalid("release_date_precision", string(precision)+" is not a known precision")
	}
	if precision == PrecisionUnknown {
		if !anchor.IsZero() {
			return PartialDate{}, invalid("release_date", "unknown precision requires no date")
		}
		return UnknownDate, nil
	}
	if anchor.IsZero() {
		return PartialDate{}, invalid("release_date", "precision "+string(precision)+" requires a date")
	}
	a := anchor.UTC()
	switch precision {
	case PrecisionMonthOnly:
		if a.Day() != 1 {
			return PartialDate{}, invalid("release_date", "month precision requires an anchor on day 1")
		}
	case PrecisionYearOnly:
		if a.Day() != 1 || a.Month() != time.January {
			return PartialDate{}, invalid("release_date", "year precision requires an anchor on 1 January")
		}
	}
	return PartialDate{
		anchor:    time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC),
		precision: precision,
	}, nil
}

// Precision reports how much of the date is known.
func (d PartialDate) Precision() DatePrecision {
	if d.precision == "" {
		return PrecisionUnknown
	}
	return d.precision
}

// Known reports whether any date information is present.
func (d PartialDate) Known() bool { return d.Precision() != PrecisionUnknown }

// Anchor returns the canonical stored date, or the zero time when the date is
// unknown. Persistence adapters store this value alongside Precision.
//
// Callers must not interpret the day component unless Precision is PrecisionExactDay,
// and must not interpret the month component unless Precision is PrecisionExactDay or
// PrecisionMonthOnly.
func (d PartialDate) Anchor() time.Time { return d.anchor }

// Year returns the year and whether it is known.
func (d PartialDate) Year() (int, bool) {
	if !d.Known() {
		return 0, false
	}
	return d.anchor.Year(), true
}

// Month returns the month and whether it is known to at least month precision.
func (d PartialDate) Month() (time.Month, bool) {
	switch d.Precision() {
	case PrecisionExactDay, PrecisionMonthOnly:
		return d.anchor.Month(), true
	}
	return 0, false
}

// ExactDay returns the full date and whether it is known to the day. This is the only
// way to obtain a day component, and it is impossible to call successfully on a date
// that does not have one.
func (d PartialDate) ExactDay() (time.Time, bool) {
	if d.Precision() != PrecisionExactDay {
		return time.Time{}, false
	}
	return d.anchor, true
}

// PeriodEnd returns the last calendar day the published precision could denote: the
// day itself at exact-day precision, the final day of the month at month precision,
// 31 December at year precision, and the zero time when the date is unknown.
//
// It is the counterpart to Anchor, and it exists because Anchor alone cannot answer a
// range question honestly. Anchor is the *earliest* day a reduced-precision date could
// mean, so comparing it against the opening boundary of a window hides releases: a
// release the vendor dated only "2025" anchors at 1 January and vanishes from a window
// opening in September, even though the vendor may well have shipped it in December.
// Asking instead whether the period *ends* on or after the boundary asks the question
// the caller actually means -- could any day this date could denote fall inside the
// window? -- and errs towards showing a release rather than concealing one.
//
// The returned day is a comparison bound, not data. It must never be displayed, stored
// or presented as the release date: at month or year precision its day component is
// FirmScout's arithmetic, not the vendor's claim, and that is exactly the manufactured
// precision ADR-0017 forbids. The type still exposes no Day() accessor for that reason.
func (d PartialDate) PeriodEnd() time.Time {
	switch d.Precision() {
	case PrecisionExactDay:
		return d.anchor
	case PrecisionMonthOnly:
		// The anchor is guaranteed to be day 1, so this lands on the last day of the
		// same month for every month length, February in a leap year included.
		return d.anchor.AddDate(0, 1, -1)
	case PrecisionYearOnly:
		return d.anchor.AddDate(1, 0, -1)
	default:
		return time.Time{}
	}
}

// String renders the date at exactly the precision it is known to: YYYY-MM-DD, YYYY-MM,
// YYYY, or the empty string. This is the representation the API presenter serves, so a
// consumer cannot parse a day that was never published.
func (d PartialDate) String() string {
	switch d.Precision() {
	case PrecisionExactDay:
		return d.anchor.Format("2006-01-02")
	case PrecisionMonthOnly:
		return d.anchor.Format("2006-01")
	case PrecisionYearOnly:
		return d.anchor.Format("2006")
	default:
		return ""
	}
}

// Before reports whether d is strictly earlier than other, comparing anchors. Dates of
// differing precision are comparable but the result is coarse by nature: a
// month-precision February 2026 compares as 1 February 2026.
func (d PartialDate) Before(other PartialDate) bool {
	if !d.Known() || !other.Known() {
		return false
	}
	return d.anchor.Before(other.anchor)
}

// AfterInstant reports whether the anchor is later than t. It is used to detect
// implausibly future-dated releases.
func (d PartialDate) AfterInstant(t time.Time) bool {
	if !d.Known() {
		return false
	}
	return d.anchor.After(t)
}

// ParsePartialDate parses a date string at the precision its shape implies:
// "2026-09-03" gives day precision, "2026-09" month precision, "2026" year precision,
// and "" the unknown date. It accepts nothing else, because guessing at an ambiguous
// format is how wrong dates enter a catalogue.
func ParsePartialDate(s string) (PartialDate, error) {
	s = strings.TrimSpace(s)
	switch len(s) {
	case 0:
		return UnknownDate, nil
	case 4:
		t, err := time.Parse("2006", s)
		if err != nil {
			return PartialDate{}, invalid("release_date", "cannot parse year: "+s)
		}
		return NewYearDate(t.Year())
	case 7:
		t, err := time.Parse("2006-01", s)
		if err != nil {
			return PartialDate{}, invalid("release_date", "cannot parse year-month: "+s)
		}
		return NewMonthDate(t.Year(), t.Month())
	case 10:
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			return PartialDate{}, invalid("release_date", "cannot parse date: "+s)
		}
		return NewExactDate(t.Year(), t.Month(), t.Day())
	default:
		return PartialDate{}, invalid("release_date", "unrecognised date shape: "+s)
	}
}
