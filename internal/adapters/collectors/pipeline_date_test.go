package collectors_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/macimottin/firmscout/internal/adapters/collectors"
	"github.com/macimottin/firmscout/internal/domain"
)

// This file is the revert detector for one specific way FirmScout could manufacture a
// date the source never published: normalising a parsed timestamp to UTC before reading
// its calendar components. A vendor that writes "12 Aug 2026 ... -0700" published the
// twelfth; converting that instant to UTC first and then asking for .Day() answers with
// OUR calendar, not theirs, and the answer is stored at exact_day precision where
// nothing downstream can tell it from a fact. Gate 5 only bounds plausibility, the
// database CHECK only enforces anchoring, and the presenter renders whatever it is
// given, so this package is the only place the discipline can be defended.
//
// The cases below are wall-clock/offset combinations, not arbitrary ones: each pairs a
// time of day with an offset whose sign carries the instant across midnight in UTC. A
// fix that reintroduces .UTC() on the parse path fails every one of them.

// zonedFeedItem renders a one-item RSS 2.0 feed carrying the given pubDate text.
func zonedFeedItem(pubDate string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel>
  <title>Test feed</title>
  <item>
    <title>Release 7.24 (FG-IR-26-163)</title>
    <link>https://example.invalid/psirt/FG-IR-26-163</link>
    <pubDate>` + pubDate + `</pubDate>
  </item>
</channel></rss>`)
}

// extractOneDate runs a collector over a single-item feed and returns that item's
// release date.
func extractOneDate(t *testing.T, c *collectors.RSSAtom, pubDate string) domain.PartialDate {
	t.Helper()
	candidates, warnings, err := c.ExtractWithWarnings(context.Background(), fixtureSource(c), xmlArtifact(zonedFeedItem(pubDate)))
	if err != nil {
		t.Fatalf("extract %q: %v", pubDate, err)
	}
	if len(candidates) != 1 {
		t.Fatalf("extract %q: want 1 candidate, got %d (warnings: %v)", pubDate, len(candidates), warnings)
	}
	return candidates[0].ReleaseDate
}

// newShippedFortinetCollector builds the real, checked-in fortinet.psirt-advisories
// collector. The defect this file guards was measured against that config and not
// against a synthetic one, and the shipped fixture corpus escapes it only by luck --
// every measured pubDate happens to be 00:00:00 with a negative offset, the one
// combination that survives a UTC normalisation intact. Testing the synthetic config
// alone would leave the shipped one unguarded on the day Fortinet publishes an
// afternoon advisory.
func newShippedFortinetCollector(t *testing.T) *collectors.RSSAtom {
	t.Helper()
	const fortinetConfigPath = "../../../collectors/config/fortinet/psirt-advisories.yaml"
	data, err := os.ReadFile(fortinetConfigPath)
	if err != nil {
		t.Fatalf("read %s: %v", fortinetConfigPath, err)
	}
	cfg, err := collectors.LoadConfig(data)
	if err != nil {
		t.Fatalf("load %s: %v", fortinetConfigPath, err)
	}
	c, err := collectors.NewRSSAtom(cfg, nil)
	if err != nil {
		t.Fatalf("build fortinet.psirt-advisories collector: %v", err)
	}
	return c
}

// TestDateExactDayKeepsTheDayTheSourcePublished runs the shipped Fortinet config, whose
// date_format is "Mon, 02 Jan 2006 15:04:05 -0700" at exact_day precision.
func TestDateExactDayKeepsTheDayTheSourcePublished(t *testing.T) {
	c := newShippedFortinetCollector(t)

	cases := []struct {
		name    string
		pubDate string
		want    string
	}{
		{
			name:    "midnight west of UTC is the shape the shipped fixtures happen to have",
			pubDate: "Wed, 12 Aug 2026 00:00:00 -0700",
			want:    "2026-08-12",
		},
		{
			name:    "afternoon west of UTC would roll forward a day under UTC normalisation",
			pubDate: "Wed, 12 Aug 2026 17:00:00 -0700",
			want:    "2026-08-12",
		},
		{
			name:    "early morning east of UTC would roll back a day under UTC normalisation",
			pubDate: "Wed, 12 Aug 2026 01:00:00 +0200",
			want:    "2026-08-12",
		},
		{
			name:    "the far eastern offsets roll back too",
			pubDate: "Thu, 13 Aug 2026 10:00:00 +1400",
			want:    "2026-08-13",
		},
		{
			name:    "a month boundary is a day boundary",
			pubDate: "Mon, 31 Aug 2026 22:00:00 +0200",
			want:    "2026-08-31",
		},
		{
			name:    "a year boundary is a day boundary",
			pubDate: "Thu, 01 Jan 2026 00:30:00 +0530",
			want:    "2026-01-01",
		},
		{
			name:    "UTC itself is unchanged",
			pubDate: "Wed, 12 Aug 2026 17:00:00 +0000",
			want:    "2026-08-12",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractOneDate(t, c, tc.pubDate)
			if got.Precision() != domain.PrecisionExactDay {
				t.Fatalf("precision: want %q, got %q", domain.PrecisionExactDay, got.Precision())
			}
			if got.String() != tc.want {
				t.Errorf("pubDate %q: the source published %s, FirmScout recorded %s", tc.pubDate, tc.want, got)
			}
		})
	}
}

// zonedDateConfig is a synthetic rss_atom config whose release_date layout and precision
// are supplied by the test. Reduced precision needs a config of its own because no
// shipped config pairs a zone-bearing layout with month_only or year_only, and that
// pairing is exactly what config validation accepts (layoutHasDay is false for
// "Jan 2006 -0700") and what the runtime must therefore handle honestly.
const zonedDateConfig = `
apiVersion: firmscout.dev/v1alpha1
kind: CollectorConfig
metadata:
  id: test.zoned-date
  vendor: test-vendor
  version: 1
spec:
  engine: rss_atom
  product_match:
    product: test-vendor-thing
  extract:
    release_container: item
    fields:
      version:
        selector: title
        regex: "([0-9]+\\.[0-9]+)"
        transform: trim
      release_notes_url:
        selector: link
        transform: trim
      release_date:
        selector: pub_date
        date_format: "%s"
        precision: %s
    release_type: embedded_os
    evidence_excerpt: title
`

func newZonedDateCollector(t *testing.T, layout, precision string) *collectors.RSSAtom {
	t.Helper()
	return newSyntheticRSSAtomCollector(t, fmt.Sprintf(zonedDateConfig, layout, precision))
}

// TestDateReducedPrecisionKeepsThePublishedMonthAndYear is the second half of the same
// defect. A layout carrying an offset but no day passes validation for month_only, and
// under a UTC normalisation "Aug 2026 +0200" -- midnight on the first of August in the
// source's zone -- lands on 31 July and is recorded as July at month precision.
func TestDateReducedPrecisionKeepsThePublishedMonthAndYear(t *testing.T) {
	cases := []struct {
		name      string
		layout    string
		precision string
		pubDate   string
		want      string
		wantPrec  domain.DatePrecision
	}{
		{
			name:      "month east of UTC would roll back a month under UTC normalisation",
			layout:    "Jan 2006 -0700",
			precision: "month_only",
			pubDate:   "Aug 2026 +0200",
			want:      "2026-08",
			wantPrec:  domain.PrecisionMonthOnly,
		},
		{
			name:      "month west of UTC stays put",
			layout:    "Jan 2006 -0700",
			precision: "month_only",
			pubDate:   "Aug 2026 -0700",
			want:      "2026-08",
			wantPrec:  domain.PrecisionMonthOnly,
		},
		{
			name:      "January east of UTC would roll back a year as well as a month",
			layout:    "Jan 2006 -0700",
			precision: "month_only",
			pubDate:   "Jan 2026 +0200",
			want:      "2026-01",
			wantPrec:  domain.PrecisionMonthOnly,
		},
		{
			name:      "year east of UTC would roll back a year under UTC normalisation",
			layout:    "2006 -0700",
			precision: "year_only",
			pubDate:   "2026 +0200",
			want:      "2026",
			wantPrec:  domain.PrecisionYearOnly,
		},
		{
			name:      "a zoneless month layout is unaffected and must stay so",
			layout:    "Jan 2006",
			precision: "month_only",
			pubDate:   "Aug 2026",
			want:      "2026-08",
			wantPrec:  domain.PrecisionMonthOnly,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newZonedDateCollector(t, tc.layout, tc.precision)
			got := extractOneDate(t, c, tc.pubDate)
			if got.Precision() != tc.wantPrec {
				t.Fatalf("precision: want %q, got %q", tc.wantPrec, got.Precision())
			}
			if day, ok := got.ExactDay(); ok {
				t.Errorf("ExactDay() returned %s: a day was invented from a reduced-precision source", day.Format("2006-01-02"))
			}
			if got.String() != tc.want {
				t.Errorf("date text %q with layout %q: the source published %s, FirmScout recorded %s",
					tc.pubDate, tc.layout, tc.want, got)
			}
		})
	}
}

// TestDateZonelessLayoutIsReadAsWrittenNotAsLocalTime pins the other half of the
// conflation the defect rested on. time.Parse with a zoneless layout yields a UTC
// time, which made the old .UTC() call look harmless; the correct reading of both cases
// is the same one -- take the calendar components as written -- so a future fix that
// reaches for time.Local instead must fail here regardless of the machine's zone.
func TestDateZonelessLayoutIsReadAsWritten(t *testing.T) {
	c := newZonedDateCollector(t, "2006-01-02 15:04:05", "exact_day")

	for _, pubDate := range []string{
		"2026-08-12 00:00:00",
		"2026-08-12 23:59:59",
		"2026-08-12 12:00:00",
	} {
		got := extractOneDate(t, c, pubDate)
		if got.String() != "2026-08-12" {
			t.Errorf("date text %q: want 2026-08-12, got %s", pubDate, got)
		}
	}
}

// TestDateEpochSecondsStaysUTC states the one rule that is deliberately different. An
// epoch carries no zone at all, so there is no source calendar to preserve; UTC is a
// choice, and it must be a stable one rather than the test machine's local zone.
func TestDateEpochSecondsStaysUTC(t *testing.T) {
	c := newZonedDateCollector(t, "epoch_seconds", "exact_day")

	// 1786496400 = 2026-08-12T01:00:00Z, which is still 11 August in every zone west
	// of UTC, so a fix that reached for time.Local would fail here on this machine.
	got := extractOneDate(t, c, "1786496400")
	if got.String() != "2026-08-12" {
		t.Errorf("epoch 1786496400: want 2026-08-12, got %s", got)
	}
}
