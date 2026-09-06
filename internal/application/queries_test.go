package application_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/application/apptest"
	"github.com/macimottin/firmscout/internal/domain"
)

// recordingReleases is a ReleaseRepository that remembers the options it was asked
// with. The page bounds are the thing under test, and they are only observable at the
// port: the use case does not return the limit it chose.
type recordingReleases struct {
	lastOpts application.ReleaseListOptions
	calls    int

	// latest and byID are optional fixed answers for GetLatestRelease-style tests.
	// Unset (the zero value every other test in this file relies on), both methods
	// keep returning domain.ErrNotFound.
	latest domain.Release
	byID   map[string]domain.Release
}

func (r *recordingReleases) ListForProduct(ctx context.Context, productID string, opts application.ReleaseListOptions) ([]domain.Release, string, error) {
	r.lastOpts = opts
	r.calls++
	return nil, "", nil
}

func (r *recordingReleases) GetByID(ctx context.Context, id string) (domain.Release, error) {
	if rel, ok := r.byID[id]; ok {
		return rel, nil
	}
	return domain.Release{}, domain.ErrNotFound
}

func (r *recordingReleases) Insert(ctx context.Context, rel domain.Release, mappings []domain.ReleaseProductMapping) error {
	return nil
}

func (r *recordingReleases) FindDuplicate(ctx context.Context, productID, normalizedVersion, channel string) (string, error) {
	return "", nil
}

// ProductRefForRelease is not exercised by any test in this file (GetRelease has no
// use case here -- the handler calls the port directly), so it returns a fixed miss
// rather than a fabricated ref.
func (r *recordingReleases) ProductRefForRelease(ctx context.Context, releaseID string) (application.ReleaseProductRef, error) {
	return application.ReleaseProductRef{}, domain.ErrNotFound
}

func (r *recordingReleases) LatestForProduct(ctx context.Context, productID, channel string) (domain.Release, error) {
	if r.latest.ID != "" {
		return r.latest, nil
	}
	return domain.Release{}, domain.ErrNotFound
}

func (r *recordingReleases) ClearLatestFlag(ctx context.Context, productID, channel string) error {
	return nil
}

func (r *recordingReleases) TouchVerified(ctx context.Context, releaseID string, at time.Time) error {
	return nil
}

func (r *recordingReleases) RefreshProductSummary(ctx context.Context, productID string) error {
	return nil
}

// TestListReleasesEnforcesTheDocumentedPageBounds is the revert detector for the page
// size drift. api.md §2 documents "limit (default 20, max 100)" for
// GET /api/v1/products/{slug}/releases, and this use case is where that contract is
// enforced -- not the HTTP handler, which is only one of its callers.
//
// The old clamp read "if limit <= 0 || limit > MaxReleasePageSize { limit = 50 }", so a
// caller who asked for nothing got 50 and a caller who asked for 150 got 50 as well.
// Neither number appears in the documentation. It stayed invisible because the handler
// clamped first with its own default of 20, which is precisely the shape of drift that
// makes a handler the de-facto application layer.
func TestListReleasesEnforcesTheDocumentedPageBounds(t *testing.T) {
	tests := []struct {
		name string
		ask  int
		want int
	}{
		{"absent takes the documented default", 0, application.DefaultReleasePageSize},
		{"negative takes the documented default", -10, application.DefaultReleasePageSize},
		{"a request inside the bounds is honoured", 37, 37},
		{"the maximum itself is honoured", application.MaxReleasePageSize, application.MaxReleasePageSize},
		{"above the maximum clamps to the maximum", 150, application.MaxReleasePageSize},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			summaries := newFakeSummaries()
			summaries.add(application.ProductSummary{ProductID: "prd_1", ProductSlug: "ccr2004"})
			releases := &recordingReleases{}
			uc := application.NewListReleases(application.QueryDeps{
				Summaries: summaries,
				Releases:  releases,
				Events:    &apptest.Events{},
				Clock:     apptest.NewClock(time.Now()),
			})

			if _, err := uc.Execute(context.Background(), application.ReleaseHistoryQuery{
				Slug:  "ccr2004",
				Limit: tc.ask,
			}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if releases.lastOpts.Limit != tc.want {
				t.Errorf("Limit(%d) reached the repository as %d, want %d (api.md §2: default %d, max %d)",
					tc.ask, releases.lastOpts.Limit, tc.want,
					application.DefaultReleasePageSize, application.MaxReleasePageSize)
			}
		})
	}
}

// TestListReleasesPageBoundsMatchTheDocumentation pins the two constants themselves.
// The clamp above can be correct while pointing at the wrong numbers, and the numbers
// are the part api.md states.
func TestListReleasesPageBoundsMatchTheDocumentation(t *testing.T) {
	if application.DefaultReleasePageSize != 20 {
		t.Errorf("DefaultReleasePageSize = %d, want 20; api.md §2 documents a default of 20",
			application.DefaultReleasePageSize)
	}
	if application.MaxReleasePageSize != 100 {
		t.Errorf("MaxReleasePageSize = %d, want 100; api.md §2 documents a maximum of 100",
			application.MaxReleasePageSize)
	}
}

// TestListReleasesWindowKeepsReducedPrecisionDates is the in-memory half of the
// windowing fix. The database proves it against real SQL; this proves the use case
// hands the boundary down unchanged and that a fake nobody runs against PostgreSQL
// applies the same rule, so an application test cannot pass on a rule the adapter
// does not implement.
func TestListReleasesWindowKeepsReducedPrecisionDates(t *testing.T) {
	now, err := time.Parse(time.RFC3339, "2026-09-05T12:00:00Z")
	if err != nil {
		t.Fatalf("parse test time: %v", err)
	}
	clock := apptest.NewClock(now)
	since := application.HistoryWindow(application.PlanFree, now)
	if since.IsZero() {
		t.Fatal("the free plan is expected to be windowed; this test proves nothing otherwise")
	}

	// Dated only with the year the window opens in: the vendor may have shipped it in
	// December, so it is inside the window. Its anchor -- 1 January -- is not.
	yearOnly, err := domain.NewYearDate(since.Year())
	if err != nil {
		t.Fatalf("NewYearDate: %v", err)
	}
	if !yearOnly.Anchor().Before(since) {
		t.Fatalf("fixture is wrong: the anchor %s is not before the boundary %s",
			yearOnly.Anchor().Format(time.DateOnly), since.Format(time.DateOnly))
	}

	summaries := newFakeSummaries()
	summaries.add(application.ProductSummary{ProductID: "prd_1", ProductSlug: "ccr2004"})
	releases := apptest.NewReleases()
	rel := domain.Release{
		ID:              "rel_year_only",
		VendorID:        "ven_1",
		ReleaseDate:     yearOnly,
		FirstObservedAt: since.AddDate(-3, 0, 0),
	}
	if err := releases.Insert(context.Background(), rel, []domain.ReleaseProductMapping{{
		ID: "rmap_1", ReleaseID: rel.ID, ProductID: "prd_1",
	}}); err != nil {
		t.Fatalf("seed release: %v", err)
	}

	uc := application.NewListReleases(application.QueryDeps{
		Summaries: summaries,
		Releases:  releases,
		Events:    &apptest.Events{},
		Clock:     clock,
	})
	page, err := uc.Execute(context.Background(), application.ReleaseHistoryQuery{
		Slug: "ccr2004", Plan: application.PlanFree,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(page.Releases) != 1 {
		t.Fatalf("a release dated %q was dropped by a window opening %s; a reduced-precision date must be compared at the end of its period, not at its anchor",
			yearOnly, since.Format(time.DateOnly))
	}
}

// TestGetVendorRecordsTheView is the revert detector for the dead use case. GetVendor
// had no caller anywhere in the repository while the handler resolved a vendor slug
// inline, which meant the VendorViewed analytics event -- published nowhere else --
// had never once been emitted.
func TestGetVendorRecordsTheView(t *testing.T) {
	vendors := newFakeVendors()
	vendors.add(domain.Vendor{ID: "ven_mikrotik", Slug: "mikrotik", Name: "MikroTik"})
	events := &apptest.Events{}
	uc := application.NewGetVendor(application.QueryDeps{
		Vendors: vendors,
		Events:  events,
		Clock:   apptest.NewClock(time.Now()),
	})

	v, err := uc.Execute(context.Background(), "  MikroTik  ")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if v.ID != "ven_mikrotik" {
		t.Errorf("vendor = %q, want ven_mikrotik; the slug is trimmed and lowercased before it is resolved", v.ID)
	}
	if n := events.Count(domain.EventVendorViewed); n != 1 {
		t.Errorf("VendorViewed published %d times, want 1; it is the only signal FirmScout has about which vendors are read", n)
	}

	if _, err := uc.Execute(context.Background(), "Not A Slug"); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Execute with a malformed slug = %v, want domain.ErrValidation", err)
	}
	if _, err := uc.Execute(context.Background(), "unknown-vendor"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Execute with an unknown slug = %v, want domain.ErrNotFound", err)
	}
	if n := events.Count(domain.EventVendorViewed); n != 1 {
		t.Errorf("VendorViewed published %d times after two failed lookups, want 1; a view that did not happen must not be counted", n)
	}
}

// recordingSearch captures the text the use case actually sends to the trigram index.
type recordingSearch struct {
	*fakeSummaries
	lastQuery string
	lastLimit int
}

func (r *recordingSearch) Search(ctx context.Context, query string, limit int) ([]application.ProductSummary, error) {
	r.lastQuery = query
	r.lastLimit = limit
	return nil, nil
}

// TestSearchEnforcesItsDocumentedInputBounds is the revert detector for the same class
// of drift as the release page size: a documented rule that held only because the HTTP
// handler applied it before the use case saw the input.
//
// api.md §2 documents "q (required, 2-200 characters; a longer query is truncated to
// 200 bytes of UTF-8 on a character boundary, never rejected)". The use case counted
// bytes for the minimum, so a single three-byte ideograph passed the check its own
// doc comment says rejects single-character catalogue sweeps, and it sliced the
// maximum at a raw byte offset, which cuts a multi-byte rune in half and sends invalid
// UTF-8 to PostgreSQL.
func TestSearchEnforcesItsDocumentedInputBounds(t *testing.T) {
	newSearch := func() (*application.SearchProducts, *recordingSearch) {
		summaries := &recordingSearch{fakeSummaries: newFakeSummaries()}
		return application.NewSearchProducts(application.QueryDeps{
			Summaries: summaries,
			Events:    &apptest.Events{},
			Clock:     apptest.NewClock(time.Now()),
		}), summaries
	}

	// One ideograph is three bytes and one character. The minimum is characters.
	uc, _ := newSearch()
	if _, err := uc.Execute(context.Background(), application.SearchQuery{Text: "日"}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("a one-character query returned %v, want domain.ErrValidation; the minimum counts characters, not bytes", err)
	}
	// Two of them are two characters and must be accepted.
	uc, _ = newSearch()
	if _, err := uc.Execute(context.Background(), application.SearchQuery{Text: "日本"}); err != nil {
		t.Errorf("a two-character query returned %v, want it accepted", err)
	}

	// A long multi-byte query is truncated, never rejected, and never cut inside a
	// rune: invalid UTF-8 reaching the trigram index is a 500, not a search.
	uc, rec := newSearch()
	long := strings.Repeat("é", application.MaxSearchQueryLength) // two bytes per rune
	if _, err := uc.Execute(context.Background(), application.SearchQuery{Text: long}); err != nil {
		t.Fatalf("a long query returned %v, want it truncated and run", err)
	}
	if len(rec.lastQuery) > application.MaxSearchQueryLength {
		t.Errorf("query reached the repository as %d bytes, want at most %d",
			len(rec.lastQuery), application.MaxSearchQueryLength)
	}
	if !utf8.ValidString(rec.lastQuery) {
		t.Errorf("query reached the repository as invalid UTF-8 (%q); the cut split a rune", rec.lastQuery)
	}

	// The case a raw byte slice actually gets wrong: one ASCII byte followed by
	// two-byte runes puts a rune boundary on every odd offset, so cutting at the even
	// MaxSearchQueryLength lands in the middle of one.
	uc, rec = newSearch()
	odd := "x" + strings.Repeat("é", application.MaxSearchQueryLength)
	if _, err := uc.Execute(context.Background(), application.SearchQuery{Text: odd}); err != nil {
		t.Fatalf("a long query returned %v, want it truncated and run", err)
	}
	if !utf8.ValidString(rec.lastQuery) {
		t.Errorf("query reached the repository as invalid UTF-8 (%q); the cut split a rune", rec.lastQuery)
	}

	// The result cap is the documented one in both directions.
	for _, tc := range []struct{ ask, want int }{
		{0, application.DefaultSearchResults},
		{500, application.MaxSearchResults},
		{7, 7},
	} {
		uc, rec = newSearch()
		if _, err := uc.Execute(context.Background(), application.SearchQuery{Text: "ccr", Limit: tc.ask}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if rec.lastLimit != tc.want {
			t.Errorf("limit(%d) reached the repository as %d, want %d (api.md §2: default %d, max %d)",
				tc.ask, rec.lastLimit, tc.want, application.DefaultSearchResults, application.MaxSearchResults)
		}
	}
}

// TestReadUseCasesRejectAMalformedSlugRatherThanMissingIt pins the status-code contract
// api.md §6 states, at the layer that owns it.
//
// Both use cases used to trim and lowercase the slug and then hand it straight to the
// summary projection, so "Not_A_Slug" -- a string that cannot name anything -- came back
// as domain.ErrNotFound and would be answered 404. Only the HTTP handler's own
// pre-check made the public API answer 400, which meant the documented mapping was a
// property of one caller rather than of the use case. GetProduct already validated;
// these two now agree with it.
func TestReadUseCasesRejectAMalformedSlugRatherThanMissingIt(t *testing.T) {
	summaries := newFakeSummaries()
	summaries.add(application.ProductSummary{ProductID: "prd_1", ProductSlug: "ccr2004"})
	deps := application.QueryDeps{
		Summaries: summaries,
		Releases:  &recordingReleases{},
		Events:    &apptest.Events{},
		Clock:     apptest.NewClock(time.Now()),
	}

	malformed := []string{"Not_A_Slug", "has spaces", "trailing-", "--double"}

	latest := application.NewGetLatestRelease(deps)
	history := application.NewListReleases(deps)

	for _, slug := range malformed {
		if _, err := latest.Execute(context.Background(), slug, ""); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("GetLatestRelease(%q) = %v, want domain.ErrValidation; a slug that cannot name anything is a 400, not a 404", slug, err)
		}
		if _, err := history.Execute(context.Background(), application.ReleaseHistoryQuery{Slug: slug}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("ListReleases(%q) = %v, want domain.ErrValidation; a slug that cannot name anything is a 400, not a 404", slug, err)
		}
	}

	// A well-formed slug for a product that does not exist stays a not-found, which is
	// the distinction this test exists to keep: absent and malformed are two answers.
	if _, err := latest.Execute(context.Background(), "no-such-product", ""); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetLatestRelease on a well-formed unknown slug = %v, want domain.ErrNotFound", err)
	}
	if _, err := history.Execute(context.Background(), application.ReleaseHistoryQuery{Slug: "no-such-product"}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("ListReleases on a well-formed unknown slug = %v, want domain.ErrNotFound", err)
	}

	// And a well-formed slug that differs only in case or padding still resolves, so the
	// new check did not tighten the endpoint into rejecting callers it used to serve.
	if _, err := history.Execute(context.Background(), application.ReleaseHistoryQuery{Slug: "  CCR2004 "}); err != nil {
		t.Errorf("ListReleases(\"  CCR2004 \") = %v, want success; trimming and lowercasing must still happen before validation", err)
	}
}

// TestGetLatestReleaseThreadsConflictDetailFromTheSummary pins that
// LatestReleaseResult.Conflict is copied straight from ProductSummary.Conflict rather
// than recomputed -- the only way to guarantee it can never say something different
// from HasConflict, which comes from the same summary field.
func TestGetLatestReleaseThreadsConflictDetailFromTheSummary(t *testing.T) {
	detectedAt := time.Date(2026, 9, 3, 18, 30, 0, 0, time.UTC)

	t.Run("no open conflict", func(t *testing.T) {
		summaries := newFakeSummaries()
		summaries.add(application.ProductSummary{
			ProductID: "prd_1", ProductSlug: "ccr2004", HasSourceConflict: false,
		})
		releases := &recordingReleases{latest: domain.Release{ID: "rel_1", Version: mustVersion(t, "7.24.2"), ReleaseType: domain.ReleaseTypeEmbeddedOS}}
		uc := application.NewGetLatestRelease(application.QueryDeps{
			Summaries: summaries, Releases: releases, Events: &apptest.Events{}, Clock: apptest.NewClock(time.Now()),
		})

		out, err := uc.Execute(context.Background(), "ccr2004", "")
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if out.HasConflict {
			t.Error("HasConflict = true, want false")
		}
		if out.Conflict != nil {
			t.Errorf("Conflict = %+v, want nil", out.Conflict)
		}
	})

	t.Run("open conflict with detail", func(t *testing.T) {
		conflict := &application.ProductConflictSummary{
			Channel: "stable", Versions: []string{"7.24.1", "7.24.2"}, SourceCount: 2, DetectedAt: detectedAt,
		}
		summaries := newFakeSummaries()
		summaries.add(application.ProductSummary{
			ProductID: "prd_1", ProductSlug: "ccr2004", HasSourceConflict: true, Conflict: conflict,
		})
		releases := &recordingReleases{latest: domain.Release{ID: "rel_1", Version: mustVersion(t, "7.24.2"), ReleaseType: domain.ReleaseTypeEmbeddedOS}}
		uc := application.NewGetLatestRelease(application.QueryDeps{
			Summaries: summaries, Releases: releases, Events: &apptest.Events{}, Clock: apptest.NewClock(time.Now()),
		})

		out, err := uc.Execute(context.Background(), "ccr2004", "")
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !out.HasConflict {
			t.Error("HasConflict = false, want true")
		}
		if out.Conflict != conflict {
			t.Errorf("Conflict = %+v, want the exact summary value %+v (copied, not recomputed)", out.Conflict, conflict)
		}
	})
}

// TestGetProductPassesThroughOfficialSourcesAndConflict pins that GetProduct is a
// straight read of the precomputed summary for these two fields: it neither widens nor
// narrows OfficialSources, and it neither invents nor drops Conflict.
func TestGetProductPassesThroughOfficialSourcesAndConflict(t *testing.T) {
	sources := []application.ProductSourceRef{
		{Slug: "changelogs", URL: "https://mikrotik.com/download/changelogs", Kind: "html_page", Official: true},
	}
	conflict := &application.ProductConflictSummary{Channel: "stable", Versions: []string{"7.24.1", "7.24.2"}, SourceCount: 2}
	summaries := newFakeSummaries()
	summaries.add(application.ProductSummary{
		ProductID: "prd_1", ProductSlug: "ccr2004",
		OfficialSources:   sources,
		HasSourceConflict: true,
		Conflict:          conflict,
	})
	uc := application.NewGetProduct(application.QueryDeps{
		Summaries: summaries, Events: &apptest.Events{}, Clock: apptest.NewClock(time.Now()),
	})

	out, err := uc.Execute(context.Background(), "ccr2004")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(out.OfficialSources) != 1 || out.OfficialSources[0] != sources[0] {
		t.Errorf("OfficialSources = %+v, want %+v unchanged", out.OfficialSources, sources)
	}
	if out.Conflict != conflict {
		t.Errorf("Conflict = %+v, want the exact summary value %+v", out.Conflict, conflict)
	}
}
