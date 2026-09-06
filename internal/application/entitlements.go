package application

import (
	"time"

	"github.com/macimottin/firmscout/internal/domain"
)

// Plan names, matching the api_consumers.plan CHECK constraint. "anonymous" is a real
// plan here rather than an absence: the public website calls this API without a key and
// is a first-class caller (api.md §1).
const (
	PlanAnonymous    = "anonymous"
	PlanFree         = "free"
	PlanProfessional = "professional"
	PlanEnterprise   = "enterprise"
	PlanInternal     = "internal"
)

// HistoryWindowMonths is how much release history anonymous and free callers receive.
// It is the number api.md §2 already publishes; it lives here so the endpoint and the
// contract cannot drift.
const HistoryWindowMonths = 12

// HistoryWindow returns the earliest release a plan may see, or the zero time for a plan
// entitled to the complete archive.
//
// An unrecognised plan is windowed. Failing closed here under-serves a caller whose plan
// name is misspelled, which is recoverable; failing open would hand the paid archive to
// anyone who sent a plan string nobody implemented.
func HistoryWindow(plan string, now time.Time) time.Time {
	switch plan {
	case PlanProfessional, PlanEnterprise, PlanInternal:
		return time.Time{}
	default:
		// PlanAnonymous, PlanFree, and anything unrecognised.
		return now.AddDate(0, -HistoryWindowMonths, 0)
	}
}

// HistoryWindowed reports whether a plan's history is windowed at all, which is what the
// response tells the caller so a complete archive and a truncated one are distinguishable.
func HistoryWindowed(plan string) bool {
	switch plan {
	case PlanProfessional, PlanEnterprise, PlanInternal:
		return false
	default:
		return true
	}
}

// WithinHistoryWindow reports whether a release falls inside the window a plan is
// entitled to, and is the ONE definition of that rule. The PostgreSQL adapter spells it
// in SQL because a window has to be applied by the query rather than by filtering a page
// the query already returned; everything else -- the API's latestOutsideWindow flag, the
// in-memory fakes, any future adapter -- calls this. The rule previously lived in four
// places and three of them were wrong at once, in different directions, which is the
// reason it now lives in one.
//
// A dated release is compared at the END of the period its precision denotes, never at
// its anchor. release_date is anchored to the earliest day the precision could mean --
// day 1 for a month, 1 January for a year, which is what the schema's CHECK constraints
// enforce -- so comparing the anchor would drop a release dated only "2025" out of a
// window opening in September, even though the vendor may have shipped it in December.
// That is a hidden release, and hiding one a caller paid to see is a silent correctness
// failure; showing one that might be a few days early is a disclosure the tier already
// permits. The error is deliberately biased towards over-inclusion.
//
// The boundary is reduced to its UTC calendar day because PeriodEnd returns a UTC
// midnight. Comparing a midnight against an instant carrying a time of day would put a
// release dated on the boundary day itself on one side for the query and the other for
// the flag, so the page and the response's own description of the page would contradict
// each other. An undated release is compared at full instant precision against
// first-observed time, because there is no period to take the end of.
func WithinHistoryWindow(releaseDate domain.PartialDate, firstObservedAt, since time.Time) bool {
	if since.IsZero() {
		return true
	}
	if releaseDate.Known() {
		return !releaseDate.PeriodEnd().Before(UTCDayOf(since))
	}
	return !firstObservedAt.Before(since)
}

// UTCDayOf reduces an instant to the UTC calendar day it falls in, which is what
// `(($n::timestamptz) AT TIME ZONE 'UTC')::date` does inside the window query. It is
// spelled out rather than inlined because it is the half of the comparison that has to
// match the SQL, and a reader checking one against the other should find the same two
// words -- UTC, day -- on both sides. A bare ::date cast would resolve in the session's
// TimeZone instead, making the answer depend on where the database happens to run.
func UTCDayOf(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
