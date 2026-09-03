package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// These are the most important tests in the package.
//
// Every other bug here produces a wrong status code or an awkward field name. A bug in
// date rendering produces a *plausible-looking fact that no vendor ever published* --
// "2026-02-01" for a release the vendor dated "February 2026" -- which a consumer's
// compliance report or patch schedule will act on without ever knowing it was invented.
// So these assert on the exact serialised bytes, not on a struct field.

func mustExact(t *testing.T, y int, m time.Month, d int) domain.PartialDate {
	t.Helper()
	pd, err := domain.NewExactDate(y, m, d)
	if err != nil {
		t.Fatalf("NewExactDate: %v", err)
	}
	return pd
}

func mustMonth(t *testing.T, y int, m time.Month) domain.PartialDate {
	t.Helper()
	pd, err := domain.NewMonthDate(y, m)
	if err != nil {
		t.Fatalf("NewMonthDate: %v", err)
	}
	return pd
}

func mustYear(t *testing.T, y int) domain.PartialDate {
	t.Helper()
	pd, err := domain.NewYearDate(y)
	if err != nil {
		t.Fatalf("NewYearDate: %v", err)
	}
	return pd
}

func mustVersion(t *testing.T, raw string) domain.VersionString {
	t.Helper()
	v, err := domain.NewVersionString(raw)
	if err != nil {
		t.Fatalf("NewVersionString(%q): %v", raw, err)
	}
	return v
}

func TestReleaseDateRendersAtExactlyTheKnownPrecision(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		date          domain.PartialDate
		wantDateKey   bool
		wantDate      string
		wantPrecision string
	}{
		{"exact day", mustExact(t, 2026, time.September, 2), true, "2026-09-02", "exact_day"},
		{"month only", mustMonth(t, 2026, time.February), true, "2026-02", "month_only"},
		{"year only", mustYear(t, 2026), true, "2026", "year_only"},
		{"unknown", domain.UnknownDate, false, "", "unknown"},
		{"zero value is unknown", domain.PartialDate{}, false, "", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dto := PresentRelease(domain.Release{
				ID:          "rel_01J8Z3K9QWERTYUIOPASDFGH",
				Version:     mustVersion(t, "7.24.2"),
				ReleaseType: domain.ReleaseTypeEmbeddedOS,
				Channel:     "stable",
				ReleaseDate: tc.date,
			}, nil)

			body, err := json.Marshal(dto)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			// The precision is present in every case, including unknown. A consumer
			// must never have to guess whether the absence of a date means "unknown"
			// or "the API forgot".
			precision, ok := got["releaseDatePrecision"]
			if !ok {
				t.Fatalf("releaseDatePrecision missing from %s", body)
			}
			if precision != tc.wantPrecision {
				t.Errorf("releaseDatePrecision = %v, want %q", precision, tc.wantPrecision)
			}

			value, present := got["releaseDate"]
			if present != tc.wantDateKey {
				t.Fatalf("releaseDate present = %v, want %v; body: %s", present, tc.wantDateKey, body)
			}
			if tc.wantDateKey && value != tc.wantDate {
				t.Errorf("releaseDate = %v, want %q", value, tc.wantDate)
			}
		})
	}
}

func TestMonthPrecisionSerialisesWithoutADay(t *testing.T) {
	t.Parallel()
	dto := PresentRelease(domain.Release{
		ID:          "rel_01J8Z3K9QWERTYUIOPASDFGH",
		Version:     mustVersion(t, "7.24.2"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS,
		ReleaseDate: mustMonth(t, 2026, time.February),
	}, nil)
	body, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, `"releaseDate":"2026-02"`) {
		t.Errorf("want \"releaseDate\":\"2026-02\" in %s", got)
	}
	if !strings.Contains(got, `"releaseDatePrecision":"month_only"`) {
		t.Errorf("want \"releaseDatePrecision\":\"month_only\" in %s", got)
	}
	// The anchor for month precision is the 1st. If any code path ever reads the
	// anchor and formats it as a full date, this is what catches it.
	if strings.Contains(got, "2026-02-01") {
		t.Errorf("a day the vendor never published leaked into the response: %s", got)
	}
}

func TestUnknownPrecisionOmitsTheDateEntirely(t *testing.T) {
	t.Parallel()
	dto := PresentRelease(domain.Release{
		ID:          "rel_01J8Z3K9QWERTYUIOPASDFGH",
		Version:     mustVersion(t, "1.0"),
		ReleaseType: domain.ReleaseTypeFirmware,
		ReleaseDate: domain.UnknownDate,
	}, nil)
	body, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(body)
	if strings.Contains(got, `"releaseDate"`) {
		t.Errorf("releaseDate key must be absent for unknown precision: %s", got)
	}
	if !strings.Contains(got, `"releaseDatePrecision":"unknown"`) {
		t.Errorf("precision must still be present: %s", got)
	}
	// The zero time must not appear either: an unknown date is not 1 January year 1.
	if strings.Contains(got, "0001-01-01") {
		t.Errorf("the zero time leaked into the response: %s", got)
	}
}

func TestNoResponseEverContainsAFabricatedDay(t *testing.T) {
	t.Parallel()
	// Every precision a vendor could have published, through every DTO that renders
	// a date. The assertion is a negative one, which is the only kind that can catch
	// a zero-filled day: the rendered value must never be longer than the precision
	// justifies.
	maxLenForPrecision := map[string]int{
		"exact_day":  10,
		"month_only": 7,
		"year_only":  4,
		"unknown":    0,
	}
	dates := []domain.PartialDate{
		mustExact(t, 2026, time.September, 2),
		mustMonth(t, 2026, time.February),
		mustYear(t, 2026),
		domain.UnknownDate,
	}
	for _, d := range dates {
		release := PresentRelease(domain.Release{
			ID:      "rel_x",
			Version: mustVersion(t, "1.0"),
			// ReleaseDate carries the precision under test.
			ReleaseDate: d,
		}, nil)
		if want := maxLenForPrecision[release.ReleaseDatePrecision]; len(release.ReleaseDate) != want {
			t.Errorf("release %s: rendered %q (len %d), want length %d",
				release.ReleaseDatePrecision, release.ReleaseDate, len(release.ReleaseDate), want)
		}

		product := PresentProduct(application.ProductSummary{
			ProductSlug:       "p",
			LatestReleaseID:   "rel_x",
			LatestRawVersion:  "1.0",
			LatestReleaseDate: d,
		})
		if product.LatestRelease == nil {
			t.Fatal("latestRelease must be present when the summary has one")
		}
		if want := maxLenForPrecision[product.LatestRelease.ReleaseDatePrecision]; len(product.LatestRelease.ReleaseDate) != want {
			t.Errorf("product summary %s: rendered %q, want length %d",
				product.LatestRelease.ReleaseDatePrecision, product.LatestRelease.ReleaseDate, want)
		}
	}
}

func TestRecommendedSerialisesAsNullWhenTheVendorNeverSaid(t *testing.T) {
	t.Parallel()
	base := domain.Release{
		ID:          "rel_01J8Z3K9QWERTYUIOPASDFGH",
		Version:     mustVersion(t, "7.24.2"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS,
		ReleaseDate: mustExact(t, 2026, time.September, 2),
	}

	t.Run("unset renders null", func(t *testing.T) {
		body, err := json.Marshal(PresentRelease(base, nil))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(body), `"recommended":null`) {
			t.Errorf(`want "recommended":null in %s`, body)
		}
		// false would claim the vendor said "not recommended", which is a different
		// and unsupported assertion.
		if strings.Contains(string(body), `"recommended":false`) {
			t.Errorf("an absent designation was rendered as an explicit no: %s", body)
		}
	})

	for _, tc := range []struct {
		name, want string
		value      bool
	}{
		{"explicit yes", `"recommended":true`, true},
		{"explicit no", `"recommended":false`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			v := tc.value
			r.Recommended = &v
			body, err := json.Marshal(PresentRelease(r, nil))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Errorf("want %s in %s", tc.want, body)
			}
		})
	}
}

func TestPaginationRendersNullCursorRatherThanOmittingIt(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(PresentVendors(nil, ""))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"nextCursor":null`) {
		t.Errorf(`want "nextCursor":null in %s`, body)
	}
	// An empty page must still be an array, not null, so a consumer can iterate it
	// without a nil check.
	if !strings.Contains(string(body), `"vendors":[]`) {
		t.Errorf(`want "vendors":[] in %s`, body)
	}

	body, err = json.Marshal(PresentVendors(nil, "opaque-cursor"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"nextCursor":"opaque-cursor"`) {
		t.Errorf("cursor not passed through: %s", body)
	}
}

func TestTimestampsRenderAsRFC3339UTC(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 3, 18, 30, 0, 123456789, time.FixedZone("CEST", 2*60*60))
	body, err := json.Marshal(PresentRelease(domain.Release{
		ID:              "rel_x",
		Version:         mustVersion(t, "1.0"),
		FirstObservedAt: at,
	}, nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"firstObservedAt":"2026-09-03T16:30:00Z"`) {
		t.Errorf("want UTC RFC 3339 at second precision in %s", body)
	}
}

func TestSearchResultsReportWhichFieldMatched(t *testing.T) {
	t.Parallel()
	summaries := []application.ProductSummary{
		{ProductSlug: "mikrotik-routeros", ProductName: "RouterOS", VendorSlug: "mikrotik", VendorName: "MikroTik"},
		{ProductSlug: "mikrotik-rb4011", ProductName: "RB4011iGS+", VendorSlug: "mikrotik", VendorName: "MikroTik",
			Aliases: []string{"RB4011"}},
	}
	got := PresentSearchResults(summaries, "rb4011")
	if len(got.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(got.Results))
	}
	if got.Results[0].MatchedOn != MatchedOnName {
		t.Errorf("first matchedOn = %q, want name (falls back when nothing matches)", got.Results[0].MatchedOn)
	}
	if got.Results[1].MatchedOn != MatchedOnName {
		// The slug itself contains rb4011, so name is the honest answer here.
		t.Errorf("second matchedOn = %q, want name", got.Results[1].MatchedOn)
	}

	got = PresentSearchResults(summaries, "mikrotik")
	if got.Results[0].MatchedOn != MatchedOnName {
		t.Errorf("matchedOn = %q, want name (slug contains the query)", got.Results[0].MatchedOn)
	}
}

func TestProductFamilyIsNullWhenTheProductHasNone(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(PresentProduct(application.ProductSummary{
		ProductSlug: "mikrotik-routeros",
		ProductName: "RouterOS",
		VendorSlug:  "mikrotik",
		VendorName:  "MikroTik",
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"family":null`) {
		t.Errorf(`want "family":null in %s`, body)
	}
	if !strings.Contains(string(body), `"latestRelease":null`) {
		t.Errorf(`want "latestRelease":null for a product with no release: %s`, body)
	}
}
