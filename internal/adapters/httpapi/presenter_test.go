package httpapi

import (
	"encoding/json"
	"net/http"
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

// ---------------------------------------------------------------------------
// the history window
// ---------------------------------------------------------------------------

// The plan decides, not the emptiness of `since`. A Professional caller and an
// anonymous one whose window happens to predate every release in the catalogue must not
// produce the same answer: only one of them is being told the truth about their tier.
func TestPresentHistoryWindowIsDecidedByThePlan(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	for _, plan := range []string{application.PlanAnonymous, application.PlanFree, "", "platinum-elite"} {
		since := application.HistoryWindow(plan, now)
		w := PresentHistoryWindow(plan, since, false)
		if !w.Windowed {
			t.Errorf("plan %q: windowed = false, want true", plan)
		}
		if w.Since == "" {
			t.Errorf("plan %q: since is absent, so the caller cannot tell where the window starts", plan)
		}
		if w.Detail == "" {
			t.Errorf("plan %q: detail is absent, so the flag is a dead end", plan)
		}
	}

	for _, plan := range []string{application.PlanProfessional, application.PlanEnterprise, application.PlanInternal} {
		w := PresentHistoryWindow(plan, application.HistoryWindow(plan, now), false)
		if w.Windowed {
			t.Errorf("plan %q: windowed = true for a plan entitled to the whole archive", plan)
		}
		// There is no boundary, so neither member is stated. An unwindowed response
		// that named a `since` would be describing a limit that does not exist.
		if w.Since != "" || w.Detail != "" {
			t.Errorf("plan %q: an unwindowed window stated a boundary: %+v", plan, w)
		}
	}
}

// The member is always present, in both states, and it renders as an object rather than
// a bare boolean so a `since` can be added to a response without a shape change.
func TestReleaseListAlwaysCarriesItsWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	windowed, err := json.Marshal(PresentReleases(nil, nil, "",
		PresentHistoryWindow(application.PlanAnonymous, application.HistoryWindow(application.PlanAnonymous, now), false)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(windowed), `"window":{"windowed":true,"since":"2025-09-05T12:00:00Z"`) {
		t.Errorf("windowed response: %s", windowed)
	}

	complete, err := json.Marshal(PresentReleases(nil, nil, "",
		PresentHistoryWindow(application.PlanProfessional, time.Time{}, false)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(complete), `"window":{"windowed":false}`) {
		t.Errorf("complete response: %s", complete)
	}
}

// ---------------------------------------------------------------------------
// hasSourceConflict
// ---------------------------------------------------------------------------

// The key carries no omitempty, and that is the point. An absent key reads as "no
// conflict", and the difference between "we checked and they agree" and "we did not
// check" is exactly what ADR-0020 exists to publish.
func TestHasSourceConflictIsNeverOmitted(t *testing.T) {
	t.Parallel()

	for _, conflicted := range []bool{true, false} {
		body, err := json.Marshal(PresentProduct(application.ProductSummary{
			ProductSlug: "mikrotik-routeros", ProductName: "RouterOS",
			VendorSlug: "mikrotik", VendorName: "MikroTik",
			HasSourceConflict: conflicted,
		}))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		want := `"hasSourceConflict":false`
		if conflicted {
			want = `"hasSourceConflict":true`
		}
		if !strings.Contains(string(body), want) {
			t.Errorf("conflicted=%v: want %s in %s", conflicted, want, body)
		}
	}
}

// ---------------------------------------------------------------------------
// release source and evidence
// ---------------------------------------------------------------------------

// releases.evidence_id is "NOT NULL REFERENCES evidence (id) ON DELETE RESTRICT"
// (database/migrations/00001_initial.sql): every release GetByID, LatestForProduct and
// ListForProduct return has real source and evidence data joined in. This is a
// contract test for that guarantee, not a happy-path check -- it exists to fail loudly
// if PresentRelease ever regresses to leaving dto.Source or dto.Evidence nil, which is
// exactly the bug that was crashing GET /releases/{id} in production: the frontend
// dereferences release.source.url and release.evidence unconditionally.
func TestPresentReleaseNeverOmitsSourceOrEvidence(t *testing.T) {
	t.Parallel()
	retrievedAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	r := domain.Release{
		ID:          "rel_01J8Z3K9QWERTYUIOPASDFGH",
		Version:     mustVersion(t, "7.24.2"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS,
		Channel:     "stable",
		EvidenceID:  "evd_01J8Z3K9QWERTYUIOPASDFGH",
		Source: domain.ReleaseSource{
			URL:      "https://mikrotik.com/download/changelogs",
			Kind:     domain.PublicKindHTML,
			Official: true,
		},
		Evidence: domain.ReleaseEvidence{
			Excerpt:     "7.24.2 (2026-09-02) - stable",
			RetrievedAt: retrievedAt,
		},
	}

	dto := PresentRelease(r, nil)
	if dto.Source == nil {
		t.Fatal("Source is nil; a published release always has real source data (evidence_id is NOT NULL)")
	}
	if dto.Source.URL != "https://mikrotik.com/download/changelogs" || dto.Source.Kind != "html" || !dto.Source.Official {
		t.Errorf("Source = %+v, want the release's real source unchanged", *dto.Source)
	}
	if dto.Evidence == nil {
		t.Fatal("Evidence is nil; a published release always has real evidence (evidence_id is NOT NULL)")
	}
	if dto.Evidence.Excerpt != r.Evidence.Excerpt {
		t.Errorf("Evidence.Excerpt = %q, want %q", dto.Evidence.Excerpt, r.Evidence.Excerpt)
	}
	if dto.Evidence.RetrievedAt == nil || !time.Time(*dto.Evidence.RetrievedAt).Equal(retrievedAt) {
		t.Errorf("Evidence.RetrievedAt = %v, want %v", dto.Evidence.RetrievedAt, retrievedAt)
	}

	body, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"source":{`) {
		t.Errorf("serialised release has no source object: %s", body)
	}
	if !strings.Contains(string(body), `"evidence":{`) {
		t.Errorf("serialised release has no evidence object: %s", body)
	}
}

// ---------------------------------------------------------------------------
// product officialSources
// ---------------------------------------------------------------------------

// A source registered for the vendor but never actually contributing a release to this
// product must not appear here -- ProductSummary.OfficialSources is already filtered to
// real contributors by RefreshProductSummary, so PresentProduct's job is only to render
// exactly what it was given, never to widen or narrow that list.
func TestPresentProductRendersRealOfficialSources(t *testing.T) {
	t.Parallel()

	empty := PresentProduct(application.ProductSummary{ProductSlug: "mikrotik-routeros", ProductName: "RouterOS"})
	if empty.OfficialSources == nil {
		t.Error("OfficialSources is nil for a product with none, want an empty non-nil slice")
	}
	if len(empty.OfficialSources) != 0 {
		t.Errorf("OfficialSources = %+v, want empty for a summary with none", empty.OfficialSources)
	}
	if body, err := json.Marshal(empty); err != nil {
		t.Fatalf("marshal: %v", err)
	} else if !strings.Contains(string(body), `"officialSources":[]`) {
		t.Errorf("empty officialSources must serialise as [], not null: %s", body)
	}

	populated := PresentProduct(application.ProductSummary{
		ProductSlug: "mikrotik-routeros", ProductName: "RouterOS",
		OfficialSources: []application.ProductSourceRef{
			{Slug: "changelogs", URL: "https://mikrotik.com/download/changelogs", Kind: "html", Official: true},
			{Slug: "newest-stable", URL: "https://upgrade.mikrotik.com/routeros/NEWESTa7.stable", Kind: "text", Official: true},
		},
	})
	if len(populated.OfficialSources) != 2 {
		t.Fatalf("OfficialSources = %+v, want 2 entries", populated.OfficialSources)
	}
	if got := populated.OfficialSources[0]; got.URL != "https://mikrotik.com/download/changelogs" || got.Kind != "html" || !got.Official {
		t.Errorf("OfficialSources[0] = %+v, want the summary's first source unchanged", got)
	}
}

// ---------------------------------------------------------------------------
// conflict detail
// ---------------------------------------------------------------------------

// hasSourceConflict and the additive conflict field must never disagree about whether
// a conflict is open: this is the pairing the task's own consistency reports keep
// finding drifted apart when it is computed twice. PresentProduct and
// PresentLatestRelease derive both from the same ProductSummary fields rather than
// recomputing either, so this test is really asserting that neither presenter ever
// starts doing that.
func TestConflictDetailAgreesWithHasSourceConflict(t *testing.T) {
	t.Parallel()

	t.Run("no open conflict", func(t *testing.T) {
		t.Parallel()
		dto := PresentProduct(application.ProductSummary{
			ProductSlug: "mikrotik-routeros", ProductName: "RouterOS",
			HasSourceConflict: false,
			Conflict:          nil,
		})
		if dto.HasSourceConflict {
			t.Error("HasSourceConflict = true, want false")
		}
		if dto.Conflict != nil {
			t.Errorf("Conflict = %+v, want nil when there is no open conflict", dto.Conflict)
		}
	})

	t.Run("open conflict with recorded detail", func(t *testing.T) {
		t.Parallel()
		detectedAt := time.Date(2026, 9, 3, 18, 30, 0, 0, time.UTC)
		dto := PresentProduct(application.ProductSummary{
			ProductSlug: "mikrotik-routeros", ProductName: "RouterOS",
			HasSourceConflict: true,
			Conflict: &application.ProductConflictSummary{
				Channel:     "stable",
				Versions:    []string{"7.24.1", "7.24.2"},
				SourceCount: 2,
				DetectedAt:  detectedAt,
			},
		})
		if !dto.HasSourceConflict {
			t.Error("HasSourceConflict = false, want true")
		}
		if dto.Conflict == nil {
			t.Fatal("Conflict = nil, want the recorded detail since HasSourceConflict is true")
		}
		if dto.Conflict.Channel != "stable" {
			t.Errorf("Conflict.Channel = %q, want %q", dto.Conflict.Channel, "stable")
		}
		if len(dto.Conflict.Versions) != 2 {
			t.Errorf("Conflict.Versions = %v, want 2 entries", dto.Conflict.Versions)
		}
		if dto.Conflict.SourceCount != 2 {
			t.Errorf("Conflict.SourceCount = %d, want 2", dto.Conflict.SourceCount)
		}
		if !time.Time(dto.Conflict.DetectedAt).Equal(detectedAt) {
			t.Errorf("Conflict.DetectedAt = %v, want %v", dto.Conflict.DetectedAt, detectedAt)
		}
	})

	// A precomputed row from before this field existed, or a conflict the detector has
	// not finished recording detail for, must render honestly rather than crash or
	// fabricate a detail object just to agree with the boolean.
	t.Run("conflict flagged with no detail captured yet", func(t *testing.T) {
		t.Parallel()
		dto := PresentProduct(application.ProductSummary{
			ProductSlug: "mikrotik-routeros", ProductName: "RouterOS",
			HasSourceConflict: true,
			Conflict:          nil,
		})
		if !dto.HasSourceConflict {
			t.Error("HasSourceConflict = false, want true")
		}
		if dto.Conflict != nil {
			t.Errorf("Conflict = %+v, want nil: this package never fabricates detail nothing computed", dto.Conflict)
		}
	})
}

// The same pairing, on the endpoint whose entire purpose is "what version should I be
// on" -- LatestReleaseResponse must carry the identical conflict detail as Product for
// the same underlying summary, via LatestReleaseResult.Conflict.
func TestLatestReleaseResponseCarriesConflictDetail(t *testing.T) {
	t.Parallel()
	detectedAt := time.Date(2026, 9, 3, 18, 30, 0, 0, time.UTC)
	summary := application.ProductSummary{
		ProductSlug: "mikrotik-routeros", ProductName: "RouterOS",
		VendorSlug: "mikrotik", VendorName: "MikroTik",
		HasSourceConflict: true,
		Conflict: &application.ProductConflictSummary{
			Channel: "stable", Versions: []string{"7.24.1", "7.24.2"}, SourceCount: 2, DetectedAt: detectedAt,
		},
	}
	result := application.LatestReleaseResult{
		Summary:     summary,
		Release:     domain.Release{ID: "rel_01J8Z3K9QWERTYUIOPASDFGH", Version: mustVersion(t, "7.24.2"), ReleaseType: domain.ReleaseTypeEmbeddedOS},
		HasConflict: summary.HasSourceConflict,
		Conflict:    summary.Conflict,
	}

	dto := PresentLatestRelease(result)
	if !dto.HasSourceConflict {
		t.Error("HasSourceConflict = false, want true")
	}
	if dto.Conflict == nil || dto.Conflict.Channel != "stable" || dto.Conflict.SourceCount != 2 {
		t.Errorf("Conflict = %+v, want the summary's detail", dto.Conflict)
	}
}

// ---------------------------------------------------------------------------
// review DTOs
// ---------------------------------------------------------------------------

// The review surface is not exempt from the date discipline. A reviewer deciding
// whether a day is plausible must be shown the same "we do not know the day" the public
// API shows, not a zero-filled one.
func TestReviewDTOsRenderDatesAtTheKnownPrecisionOnly(t *testing.T) {
	t.Parallel()
	feb, err := domain.NewMonthDate(2026, time.February)
	if err != nil {
		t.Fatalf("NewMonthDate: %v", err)
	}

	detail := PresentReviewItemDetail(application.ReviewItemDetail{
		Entry: application.ReviewQueueEntry{
			Item: application.ReviewItem{ID: "rev_1", State: application.ReviewStateOpen},
			Age:  90 * time.Minute,
		},
		Candidate: domain.CandidateRelease{
			ID: "cnd_1", Version: mustVersion(t, "7.24.2"), ReleaseDate: feb,
		},
		CandidateFound: true,
		ConflictFound:  true,
		Conflict:       domain.SourceConflict{ID: "cfl_1", State: domain.ConflictOpen},
		ConflictObservations: []domain.SourceObservation{
			{SourceID: "src_1", RawVersion: "7.24.1", ReleaseDate: domain.UnknownDate},
		},
	})

	body, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"releaseDate":"2026-02"`) {
		t.Errorf("month precision was not preserved: %s", body)
	}
	if strings.Contains(string(body), "2026-02-01") {
		t.Errorf("a day was fabricated from a month-precision date: %s", body)
	}
	// The undated observation renders no date at all and says its precision is
	// unknown, which is the pair of facts that stops a consumer inventing one.
	if !strings.Contains(string(body), `"releaseDatePrecision":"unknown"`) {
		t.Errorf("an undated observation lost its precision: %s", body)
	}
	if detail.Item.AgeSeconds != int64(90*time.Minute/time.Second) {
		t.Errorf("ageSeconds = %d", detail.Item.AgeSeconds)
	}
	// An empty participant list is still an array, so a client iterating it never has
	// to branch on null.
	if !strings.Contains(string(body), `"versions":[]`) {
		t.Errorf("conflict versions rendered as null rather than an empty array: %s", body)
	}
}

// The decision receipt says the actor was not verified. A client rendering an audit
// trail must be handed that fact, not expected to know it.
func TestReviewDecisionAlwaysReportsAnUnverifiedActor(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(PresentReviewDecision(application.ReviewDecisionResult{
		ItemID: "rev_1", Decision: application.ReviewDecisionAccepted,
		ReleaseID: "rel_1", Published: true,
		DecidedAt: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
	}, "alex@firmscout.dev"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"actorAuthenticated":false`) {
		t.Errorf("the receipt must state that the actor is unverified: %s", body)
	}
	if !strings.Contains(string(body), `"actor":"alex@firmscout.dev"`) {
		t.Errorf("the asserted name must be echoed verbatim: %s", body)
	}
}

// ---------------------------------------------------------------------------
// The device-first catalogue (ADR-0024)
// ---------------------------------------------------------------------------
//
// These belong beside the date tests for the same reason. A fleet manager reading a
// device page is deciding what to flash onto hardware in a rack. The failure this
// section guards against is not an awkward field name: it is a device page that reads
// as though FirmScout established which firmware image that exact model takes, when it
// did not. That is a fabricated fact, arriving by omission rather than by invention.

// deviceSummary is a registered hardware model: a product code, no release stream of
// its own, and one runs_os edge to the operating system where the releases live.
func deviceSummary() application.ProductSummary {
	return application.ProductSummary{
		ProductID:       "prd_crs328",
		VendorSlug:      "mikrotik",
		VendorName:      "MikroTik",
		ProductSlug:     "mikrotik-crs328-24p-4s-rm",
		ProductName:     "CRS328-24P-4S+RM",
		FamilyName:      "ARM 32bit",
		ModelIdentifier: "CRS328-24P-4S+RM",
		Aliases:         []string{"CRS328-24P-4S+RM"},
		CategorySlugs:   []string{"network-devices", "switches"},
		LifecycleStatus: "unknown",
		Runs: []application.ProductRunsRef{
			{Slug: "mikrotik-routeros", Name: "RouterOS", Kind: domain.RelationRunsOS},
		},
	}
}

// osSummary is the operating system the device points at: releases of its own, no model
// identifier, nothing it runs.
func osSummary() application.ProductSummary {
	return application.ProductSummary{
		ProductID:         "prd_routeros",
		VendorSlug:        "mikrotik",
		VendorName:        "MikroTik",
		ProductSlug:       "mikrotik-routeros",
		ProductName:       "RouterOS",
		CategorySlugs:     []string{"network_device_os"},
		LatestReleaseID:   "rel_01J8Z3K9QWERTYUIOPASDFGH",
		LatestRawVersion:  "7.24.2",
		LatestReleaseType: "embedded_os",
		LatestChannel:     "stable",
		ReleaseCount:      12,
	}
}

// The three device claims are three keys, and all three are always sent.
//
// An absent key is read as a claim in this API -- that is the rule hasSourceConflict
// already states -- and each of these would be read as the wrong one. A missing
// firmwareApplicability reads as "the releases here apply"; a missing modelIdentifier
// is indistinguishable from an API that forgot to send it.
func TestProductApplicabilityIsAlwaysPresent(t *testing.T) {
	t.Parallel()
	cases := map[string]application.ProductSummary{
		"operating system with its own releases": osSummary(),
		"device that runs one":                   deviceSummary(),
		"product with neither":                   {ProductSlug: "empty", ProductName: "Empty"},
	}
	for name, summary := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body, err := json.Marshal(PresentProduct(summary))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			for _, key := range []string{"modelIdentifier", "runs", "aliases", "firmwareApplicability"} {
				if _, ok := got[key]; !ok {
					t.Errorf("%s is absent from %s", key, body)
				}
			}
			// runs is an array in every case, never null: a consumer ranges over it
			// without a nil check, exactly as officialSources promises.
			if s := string(got["runs"]); !strings.HasPrefix(s, "[") {
				t.Errorf("runs = %s, want an array", s)
			}
		})
	}

	// The null is the point, not the absence: "this is not a hardware model" is a
	// claim the API makes, and it is different from having sent nothing.
	body, err := json.Marshal(PresentProduct(osSummary()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"modelIdentifier":null`) {
		t.Errorf("a software product did not render an explicit null modelIdentifier: %s", body)
	}
	if !strings.Contains(string(body), `"runs":[]`) {
		t.Errorf("a product that runs nothing did not render an empty array: %s", body)
	}
}

// hybridSummary is the product shape ADR-0024 names as the reason the model exists: a
// hardware model that ALSO publishes a release stream of its own, the rack server with
// a BIOS. It is a device -- model identifier, a runs_os edge -- with its own mapped
// release on top.
func hybridSummary() application.ProductSummary {
	s := deviceSummary()
	s.ProductSlug = "mikrotik-hap-be-lite"
	s.ProductName = "hAP be lite"
	s.ModelIdentifier = "C53UiG+5HaxD2HaxD"
	s.Aliases = []string{"C53UiG+5HaxD2HaxD", "hAP be lite"}
	s.ReleaseCount = 1
	s.LatestReleaseID = "rel_01J8Z3K9QWERTYUIOPASDFGH"
	s.LatestRawVersion = "1.4.0"
	s.LatestChannel = "stable"
	return s
}

// The two applicability claims are answered independently, and a mapping row can never
// retract the caveat.
//
// The fourth case is the defect this table was rewritten for. A product that is both a
// hardware model and its own release stream used to report {verified: true, basis:
// own_releases} and drop the runs_os caveat entirely -- its own BIOS-shaped release
// vouching for an operating-system applicability nobody had verified. Both claims are
// now on the response at once.
func TestApplicabilityAnswersItsTwoClaimsIndependently(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		summary  application.ProductSummary
		verified bool
		basis    string
		own      OwnReleasesDTO
	}{
		{
			"an operating system: its own releases, nothing it runs",
			osSummary(), true, ApplicabilityOwnReleases,
			OwnReleasesDTO{Mapped: true, ReleaseCount: 12},
		},
		{
			"a device: runs an operating system, no releases of its own",
			deviceSummary(), false, ApplicabilityRunsOSUnverified,
			OwnReleasesDTO{Mapped: false, ReleaseCount: 0},
		},
		{
			"a device that also publishes releases: BOTH claims, and the caveat stands",
			hybridSummary(), false, ApplicabilityRunsOSUnverified,
			OwnReleasesDTO{Mapped: true, ReleaseCount: 1},
		},
		{
			"nothing recorded",
			application.ProductSummary{ProductSlug: "empty"}, false, ApplicabilityNoneRecorded,
			OwnReleasesDTO{Mapped: false, ReleaseCount: 0},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := PresentProduct(tc.summary).FirmwareApplicability
			want := ApplicabilityDTO{Verified: tc.verified, Basis: tc.basis, OwnReleases: tc.own}
			if got != want {
				t.Errorf("firmwareApplicability = %+v, want %+v", got, want)
			}
		})
	}
}

// The both-at-once shape, asserted on the wire rather than on the struct, because the
// keys are what owner F4 renders and what a generated client types.
//
// A response that carries runs:[RouterOS] and a latestRelease of the device's own must
// say both things: these releases are mine, and which of RouterOS's releases fit this
// exact chassis is unverified. Reporting verified applicability here is the fabricated
// fact ADR-0024's whole contract exists to refuse -- it tells an operator the image on
// the page is the one for their box.
func TestAProductThatIsBothADeviceAndAReleaseStreamKeepsTheCaveat(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(PresentProduct(hybridSummary()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Runs          []map[string]any `json:"runs"`
		LatestRelease map[string]any   `json:"latestRelease"`
		Applicability struct {
			Verified    bool   `json:"verified"`
			Basis       string `json:"basis"`
			OwnReleases struct {
				Mapped       bool `json:"mapped"`
				ReleaseCount int  `json:"releaseCount"`
			} `json:"ownReleases"`
		} `json:"firmwareApplicability"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// The preconditions that make this the both-at-once shape rather than either half.
	if len(got.Runs) != 1 {
		t.Fatalf("runs = %v, want the operating system edge that makes this a device: %s", got.Runs, body)
	}
	if got.LatestRelease == nil {
		t.Fatalf("latestRelease is null; this product has a release of its own: %s", body)
	}

	if got.Applicability.Verified {
		t.Errorf("verified = true on a product whose runs_os applicability nobody established: %s", body)
	}
	if got.Applicability.Basis != ApplicabilityRunsOSUnverified {
		t.Errorf("basis = %q, want %q -- a release mapping retracted the caveat: %s",
			got.Applicability.Basis, ApplicabilityRunsOSUnverified, body)
	}
	if !got.Applicability.OwnReleases.Mapped || got.Applicability.OwnReleases.ReleaseCount != 1 {
		t.Errorf("ownReleases = %+v, want {mapped:true releaseCount:1} -- the other claim was dropped: %s",
			got.Applicability.OwnReleases, body)
	}
}

// Aliases reach the wire, always as an array.
//
// The model number is frequently the string that produced the search hit that brought
// the caller to this page (ADR-0024 makes model-number search run through a
// `model_number` alias rather than through a column), so a product response that omits
// its aliases makes the page contradict the search result the reader just clicked.
func TestProductCarriesItsAliases(t *testing.T) {
	t.Parallel()
	s := hapSummary()
	dto := PresentProduct(s)
	if len(dto.Aliases) != 2 || dto.Aliases[0] != "C53UiG+5HPaxD2HPaxD" || dto.Aliases[1] != "hAP ax3" {
		t.Fatalf("aliases = %q, want the summary's aliases in order", dto.Aliases)
	}

	body, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"aliases":["C53UiG+5HPaxD2HPaxD","hAP ax3"]`) {
		t.Errorf("aliases did not reach the wire in order: %s", body)
	}

	// Empty rather than null, and present rather than omitted: a consumer ranges over
	// it without a check, and "this product has no other names" is a claim the API
	// makes rather than one a missing key implies.
	empty, err := json.Marshal(PresentProduct(osSummary()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(empty), `"aliases":[]`) {
		t.Errorf("a product with no aliases did not render an empty array: %s", empty)
	}
}

// The device response names the operating system, links to it by a slug that resolves
// on this same route, and says out loud that per-model applicability is unverified.
func TestDeviceProductNamesTheOSItRunsAndCarriesTheCaveat(t *testing.T) {
	t.Parallel()
	dto := PresentProduct(deviceSummary())

	if dto.ModelIdentifier == nil || *dto.ModelIdentifier != "CRS328-24P-4S+RM" {
		t.Fatalf("modelIdentifier = %v, want the vendor's product code", dto.ModelIdentifier)
	}
	if len(dto.Runs) != 1 {
		t.Fatalf("runs = %+v, want one entry", dto.Runs)
	}
	if dto.Runs[0].Slug != "mikrotik-routeros" || dto.Runs[0].Name != "RouterOS" {
		t.Errorf("runs[0] = %+v, want the operating system's slug and name", dto.Runs[0])
	}
	if dto.FirmwareApplicability.Verified {
		t.Error("a device with no releases of its own reported verified applicability")
	}
	if dto.FirmwareApplicability.Basis != ApplicabilityRunsOSUnverified {
		t.Errorf("basis = %q, want %q", dto.FirmwareApplicability.Basis, ApplicabilityRunsOSUnverified)
	}
	// No version is claimed anywhere on the response. Inheriting the operating
	// system's newest release would tell an operator to flash an image FirmScout
	// never established this hardware takes.
	if dto.LatestRelease != nil {
		t.Errorf("a device with no mapped releases rendered a latest release: %+v", dto.LatestRelease)
	}
	// The family is a name and never a slug: there is no family route for one to
	// point at, and a link the API cannot honour is worse than plain text.
	if dto.Family == nil || dto.Family.Name != "ARM 32bit" {
		t.Fatalf("family = %+v, want the architecture family's name", dto.Family)
	}
	if dto.Family.Slug != "" {
		t.Errorf("family.slug = %q; there is no family route for it to resolve on", dto.Family.Slug)
	}
}

// An entry with no slug is dropped rather than rendered as a bare name, because
// ProductRef.Slug is omitempty and a runs entry a consumer cannot follow defeats the
// only purpose the array has.
func TestRunsDropsAnEntryWithNoSlug(t *testing.T) {
	t.Parallel()
	s := deviceSummary()
	s.Runs = append(s.Runs, application.ProductRunsRef{Name: "SwitchOS", Kind: domain.RelationRunsOS})

	dto := PresentProduct(s)
	if len(dto.Runs) != 1 {
		t.Fatalf("runs = %+v, want the slugless entry dropped", dto.Runs)
	}
	body, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "SwitchOS") {
		t.Errorf("a runs entry with no slug reached the wire: %s", body)
	}
}

// hapSummary is a device whose marketing name and product code differ, which is the
// usual case and the one that exercises the alias path: nobody types
// "C53UiG+5HPaxD2HPaxD" from memory, they paste it off the chassis.
func hapSummary() application.ProductSummary {
	return application.ProductSummary{
		ProductID:       "prd_hap_ax3",
		VendorSlug:      "mikrotik",
		VendorName:      "MikroTik",
		ProductSlug:     "mikrotik-hap-ax3",
		ProductName:     "hAP ax³",
		ModelIdentifier: "C53UiG+5HPaxD2HPaxD",
		Aliases:         []string{"C53UiG+5HPaxD2HPaxD", "hAP ax3"},
		CategorySlugs:   []string{"network-devices", "wireless-devices"},
		Runs: []application.ProductRunsRef{
			{Slug: "mikrotik-routeros", Name: "RouterOS", Kind: domain.RelationRunsOS},
		},
	}
}

// A fleet manager pastes the string stamped on the chassis. This is the whole product
// requirement, asserted at the wire format: the hit echoes the code back, so they can
// see that the row is the thing they hold.
func TestSearchResultEchoesModelIdentifier(t *testing.T) {
	t.Parallel()
	res := PresentSearchResults(
		[]application.ProductSummary{deviceSummary(), osSummary()},
		"CRS328-24P-4S+RM",
	)
	if len(res.Results) != 2 {
		t.Fatalf("got %d result(s), want 2", len(res.Results))
	}
	device := res.Results[0]
	if device.Slug != "mikrotik-crs328-24p-4s-rm" {
		t.Fatalf("first result = %+v, want the device", device)
	}
	if device.ModelIdentifier != "CRS328-24P-4S+RM" {
		t.Errorf("modelIdentifier = %q, want the code the caller pasted", device.ModelIdentifier)
	}
	// This device's product name IS its product code, so the name test wins before
	// the alias test is ever reached. That is the honest label: the query really did
	// hit the name. matchedOn describes which field the query plausibly hit, not
	// which registry row made the row findable.
	if device.MatchedOn != MatchedOnName {
		t.Errorf("matchedOn = %q, want %q -- this device's name is byte-identical to its code",
			device.MatchedOn, MatchedOnName)
	}

	// The usual case, where the code is nothing like the marketing name, reports the
	// alias it actually matched.
	viaAlias := PresentSearchResults([]application.ProductSummary{hapSummary()}, "C53UiG+5HPaxD2HPaxD")
	if len(viaAlias.Results) != 1 {
		t.Fatalf("got %d result(s), want 1", len(viaAlias.Results))
	}
	if viaAlias.Results[0].MatchedOn != MatchedOnAlias {
		t.Errorf("matchedOn = %q, want %q -- the product code is registered as an alias",
			viaAlias.Results[0].MatchedOn, MatchedOnAlias)
	}
	if viaAlias.Results[0].ModelIdentifier != "C53UiG+5HPaxD2HPaxD" {
		t.Errorf("modelIdentifier = %q, want the pasted code", viaAlias.Results[0].ModelIdentifier)
	}

	body, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Omitted, not null, for a software product: a compact hit descriptor should not
	// carry an explicit null on every row that is not hardware.
	var decoded struct {
		Results []map[string]json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded.Results[1]["modelIdentifier"]; ok {
		t.Errorf("a software product carried a modelIdentifier key: %s", body)
	}
}

// ---------------------------------------------------------------------------
// The same three claims, over HTTP
// ---------------------------------------------------------------------------

// The route is the existing one. A caller holding a model number does not know whether
// the slug they followed names a device or an operating system, so there is no separate
// device route to know about.
func TestDeviceIsServedByTheProductRoute(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.summaries.put(deviceSummary())

	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-crs328-24p-4s-rm")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["modelIdentifier"] != "CRS328-24P-4S+RM" {
		t.Errorf("modelIdentifier = %v, want the product code", got["modelIdentifier"])
	}
	applicability, ok := got["firmwareApplicability"].(map[string]any)
	if !ok {
		t.Fatalf("firmwareApplicability missing or not an object: %s", w.Body.String())
	}
	if applicability["verified"] != false || applicability["basis"] != ApplicabilityRunsOSUnverified {
		t.Errorf("firmwareApplicability = %v, want an unverified runs_os basis", applicability)
	}
	runs, ok := got["runs"].([]any)
	if !ok || len(runs) != 1 {
		t.Fatalf("runs = %v, want one entry", got["runs"])
	}
	if got["latestRelease"] != nil {
		t.Errorf("latestRelease = %v, want an explicit null", got["latestRelease"])
	}
}

// Searching a real model number returns the device, with the code echoed back. If the
// model identifier ever stops reaching the wire, this fails.
func TestSearchByModelNumberReturnsTheDevice(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.summaries.put(deviceSummary())
	h.summaries.search = []application.ProductSummary{deviceSummary()}

	w := h.do(http.MethodGet, "/api/v1/search?q=CRS328-24P-4S%2BRM")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	// The query reaches the repository unmangled: the plus sign is part of the model
	// number, not an encoded space.
	if h.summaries.lastQuery != "CRS328-24P-4S+RM" {
		t.Errorf("the repository was queried for %q, want the pasted product code", h.summaries.lastQuery)
	}
	var got SearchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Results) != 1 {
		t.Fatalf("got %d result(s), want the device; body: %s", len(got.Results), w.Body.String())
	}
	if got.Results[0].Slug != "mikrotik-crs328-24p-4s-rm" {
		t.Errorf("slug = %q, want the device", got.Results[0].Slug)
	}
	if got.Results[0].ModelIdentifier != "CRS328-24P-4S+RM" {
		t.Errorf("modelIdentifier = %q, want the code the caller pasted -- a fleet manager "+
			"must be able to see the row is the thing they hold", got.Results[0].ModelIdentifier)
	}
}

// "Latest for this device" resolves to nothing, and the API says so with a 404 rather
// than by borrowing the operating system's answer. The empty history page says the
// same thing in list form.
func TestDeviceHasNoLatestReleaseAndNoHistory(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.summaries.put(deviceSummary())
	h.summaries.put(osSummary())
	h.releases.setLatest("prd_routeros", "", domain.Release{
		ID:          "rel_01J8Z3K9QWERTYUIOPASDFGH",
		Version:     mustVersion(t, "7.24.2"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS,
		Channel:     "stable",
		ReleaseDate: mustExact(t, 2026, time.September, 2),
	})

	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-crs328-24p-4s-rm/latest")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal problem: %v", err)
	}
	if p.Status != http.StatusNotFound {
		t.Errorf("problem status = %d, want 404", p.Status)
	}
	// The operating system's own answer is untouched: the 404 is about this device,
	// not about a catalogue that has nothing.
	if w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/latest"); w.Code != http.StatusOK {
		t.Errorf("the operating system's latest release = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	hist := h.do(http.MethodGet, "/api/v1/products/mikrotik-crs328-24p-4s-rm/releases")
	if hist.Code != http.StatusOK {
		t.Fatalf("history status = %d, want 200; body: %s", hist.Code, hist.Body.String())
	}
	var page ReleaseListResponse
	if err := json.Unmarshal(hist.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal history: %v", err)
	}
	if len(page.Releases) != 0 {
		t.Errorf("a device with no mapped releases returned %d, want an empty page", len(page.Releases))
	}
	if page.Pagination.NextCursor != nil {
		t.Errorf("nextCursor = %v, want null", *page.Pagination.NextCursor)
	}
}
