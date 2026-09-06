package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

func seedProduct(h *harness) application.ProductSummary {
	s := application.ProductSummary{
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
		LatestReleaseDate: func() domain.PartialDate {
			d, _ := domain.NewExactDate(2026, time.September, 2)
			return d
		}(),
		LastVerifiedAt: time.Date(2026, 9, 3, 18, 30, 0, 0, time.UTC),
	}
	h.summaries.put(s)
	return s
}

// The public website is a client of this same API and calls it with no credential. If
// this ever starts failing, the website goes dark.
func TestAnonymousProductFetchSucceeds(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)

	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got ProductDTO
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Slug != "mikrotik-routeros" || got.Vendor.Slug != "mikrotik" {
		t.Errorf("unexpected product: %+v", got)
	}
	if got.LatestRelease == nil || got.LatestRelease.ReleaseDate != "2026-09-02" {
		t.Errorf("latest release not rendered: %+v", got.LatestRelease)
	}
}

func TestNotFoundReturnsWellFormedProblemJSON(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	w := h.do(http.MethodGet, "/api/v1/products/does-not-exist", func(r *http.Request) {
		r.Header.Set(HeaderRequestID, "req_test123")
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != ProblemContentType {
		t.Errorf("Content-Type = %q, want %q", ct, ProblemContentType)
	}
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal problem: %v; body: %s", err, w.Body.String())
	}
	if p.Type != TypeNotFound {
		t.Errorf("type = %q, want %q", p.Type, TypeNotFound)
	}
	if p.Title == "" {
		t.Error("title must be populated")
	}
	if p.Status != http.StatusNotFound {
		t.Errorf("status member = %d, want 404", p.Status)
	}
	if p.Instance != "/api/v1/products/does-not-exist" {
		t.Errorf("instance = %q", p.Instance)
	}
	// The request id must be readable from the body alone, for a consumer that logs
	// bodies but not headers.
	if p.RequestID != "req_test123" {
		t.Errorf("requestId = %q, want req_test123", p.RequestID)
	}
	if w.Header().Get(HeaderRequestID) != "req_test123" {
		t.Errorf("X-Request-Id header = %q", w.Header().Get(HeaderRequestID))
	}
}

func TestInternalErrorNeverLeaksTheUnderlyingErrorText(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.summaries.getErr = errors.New("pq: relation \"product_summaries\" does not exist on host db-primary-7")

	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	body := w.Body.String()
	for _, secret := range []string{"pq:", "product_summaries", "db-primary-7"} {
		if strings.Contains(body, secret) {
			t.Errorf("internal detail %q leaked to the client: %s", secret, body)
		}
	}
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Type != TypeInternal {
		t.Errorf("type = %q, want %q", p.Type, TypeInternal)
	}
	if p.RequestID == "" {
		t.Error("a 500 must carry a request id, since it is the only handle support has")
	}
}

// The single most consequential ordering rule in the API: 9.99.99 sorts above 1.0.0
// under every string and semver comparison, but if 9.99.99 shipped in January and
// 1.0.0 shipped in September, September is the later release.
func TestReleasesAreNeverOrderedByVersionString(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)

	jan, _ := domain.NewExactDate(2026, time.January, 15)
	sep, _ := domain.NewExactDate(2026, time.September, 1)
	h.releases.put("prd_routeros", domain.Release{
		ID:              "rel_00000000000000000009",
		Version:         mustVersion(t, "9.99.99"),
		ReleaseType:     domain.ReleaseTypeEmbeddedOS,
		Channel:         "stable",
		ReleaseDate:     jan,
		FirstObservedAt: time.Date(2026, 1, 16, 0, 0, 0, 0, time.UTC),
	})
	h.releases.put("prd_routeros", domain.Release{
		ID:              "rel_00000000000000000001",
		Version:         mustVersion(t, "1.0.0"),
		ReleaseType:     domain.ReleaseTypeEmbeddedOS,
		Channel:         "stable",
		ReleaseDate:     sep,
		FirstObservedAt: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
	})

	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/releases")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var got ReleaseListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Releases) != 2 {
		t.Fatalf("releases = %d, want 2", len(got.Releases))
	}
	if got.Releases[0].RawVersion != "1.0.0" {
		t.Errorf("first release = %q dated %q, want 1.0.0 (September) -- the order came from a version string, not a date",
			got.Releases[0].RawVersion, got.Releases[0].ReleaseDate)
	}
	if got.Releases[1].RawVersion != "9.99.99" {
		t.Errorf("second release = %q, want 9.99.99", got.Releases[1].RawVersion)
	}
}

func TestUndatedReleasesSortLastRegardlessOfDirection(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)

	dated, _ := domain.NewExactDate(2026, time.March, 1)
	h.releases.put("prd_routeros", domain.Release{
		ID: "rel_undated0000000000", Version: mustVersion(t, "2.0"),
		ReleaseDate: domain.UnknownDate, FirstObservedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	})
	h.releases.put("prd_routeros", domain.Release{
		ID: "rel_dated00000000000000", Version: mustVersion(t, "1.0"),
		ReleaseDate: dated, FirstObservedAt: time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
	})

	for _, order := range []string{"desc", "asc"} {
		w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/releases?order="+order)
		var got ReleaseListResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(got.Releases) != 2 {
			t.Fatalf("order=%s: releases = %d, want 2", order, len(got.Releases))
		}
		// An undated observation is weaker evidence of recency than a dated one, so
		// it never claims the top of the list -- in either direction.
		if got.Releases[1].ReleaseDatePrecision != "unknown" {
			t.Errorf("order=%s: undated release is not last: %+v", order, got.Releases)
		}
	}
}

func TestReleasesRejectsSortByVersionExplicitly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)

	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/releases?sort=version")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var p Problem
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.Type != TypeInvalidParameter {
		t.Errorf("type = %q, want %q", p.Type, TypeInvalidParameter)
	}
	if !strings.Contains(p.Detail, "version") {
		t.Errorf("the detail should explain why version is not sortable: %q", p.Detail)
	}
}

func TestReleaseFiltersRejectAnUnknownVocabularyValue(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)

	for _, target := range []string{
		"/api/v1/products/mikrotik-routeros/releases?channel=stabel",
		"/api/v1/products/mikrotik-routeros/releases?releaseType=firmwear",
		"/api/v1/products/mikrotik-routeros/releases?order=sideways",
		"/api/v1/products/mikrotik-routeros/releases?limit=nope",
	} {
		w := h.do(http.MethodGet, target)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (a misspelled filter must not look like an empty result set)", target, w.Code)
		}
	}
}

func TestSearchRejectsAQueryShorterThanTwoCharacters(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	for _, target := range []string{"/api/v1/search", "/api/v1/search?q=", "/api/v1/search?q=a", "/api/v1/search?q=%20%20"} {
		w := h.do(http.MethodGet, target)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, w.Code)
		}
	}
}

func TestSearchCapsTheQueryAndTheLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	long := strings.Repeat("x", 500)
	w := h.do(http.MethodGet, "/api/v1/search?q="+long+"&limit=5000")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	h.summaries.mu.Lock()
	gotQuery, gotLimit := h.summaries.lastQuery, h.summaries.lastLimit
	h.summaries.mu.Unlock()
	if len(gotQuery) != searchQueryMaxBytes {
		t.Errorf("query length = %d, want %d", len(gotQuery), searchQueryMaxBytes)
	}
	if gotLimit != searchLimitMax {
		t.Errorf("limit = %d, want %d", gotLimit, searchLimitMax)
	}
}

// The cap is a byte cap and it cuts on a rune boundary.
//
// SearchProducts.Execute truncates by bytes, so a handler that capped by runes would
// hand it a longer string and let it split a multi-byte character in half. The result is
// invalid UTF-8 reaching the trigram index, which PostgreSQL answers with an encoding
// error -- a 500 that any caller can trigger by searching in Japanese.
func TestSearchTruncatesMultiByteQueriesOnARuneBoundary(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// 300 three-byte runes: 900 bytes, and 200 is not a multiple of 3.
	long := strings.Repeat("設", 300)
	w := h.do(http.MethodGet, "/api/v1/search?q="+url.QueryEscape(long))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	h.summaries.mu.Lock()
	got := h.summaries.lastQuery
	h.summaries.mu.Unlock()

	if !utf8.ValidString(got) {
		t.Errorf("the search port was handed invalid UTF-8: %q", got)
	}
	if len(got) > searchQueryMaxBytes {
		t.Errorf("query = %d bytes, want at most %d", len(got), searchQueryMaxBytes)
	}
	// And it is not truncated further than it has to be: 66 whole runes fit in 200
	// bytes, 67 do not.
	if n := utf8.RuneCountInString(got); n != searchQueryMaxBytes/3 {
		t.Errorf("query = %d runes, want %d", n, searchQueryMaxBytes/3)
	}
}

func TestSearchEmitsAnalyticsEvents(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	if w := h.do(http.MethodGet, "/api/v1/search?q=routeros"); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	h.summaries.search = []application.ProductSummary{{ProductSlug: "mikrotik-routeros", ProductName: "RouterOS"}}
	if w := h.do(http.MethodGet, "/api/v1/search?q=routeros"); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	// Events are published after the response, so drain the worker before asserting.
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	names := h.events.names()
	var executed, noResults int
	for _, n := range names {
		switch n {
		case string(domain.EventSearchExecuted):
			executed++
		case string(domain.EventSearchReturnedNoResult):
			noResults++
		}
	}
	if executed != 2 {
		t.Errorf("SearchExecuted count = %d, want 2 (got %v)", executed, names)
	}
	if noResults != 1 {
		t.Errorf("SearchReturnedNoResults count = %d, want 1 (got %v)", noResults, names)
	}
}

func TestLatestReleaseResponse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)
	feb, _ := domain.NewMonthDate(2026, time.February)
	h.releases.setLatest("prd_routeros", "", domain.Release{
		ID:          "rel_01J8Z3K9QWERTYUIOPASDFGH",
		Version:     mustVersion(t, "7.24.2"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS,
		Channel:     "stable",
		ReleaseDate: feb,
	})

	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/latest")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"releaseDate":"2026-02"`) {
		t.Errorf("month precision not preserved through the latest endpoint: %s", body)
	}
	var got LatestReleaseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Vendor.Slug != "mikrotik" || got.Product.Slug != "mikrotik-routeros" {
		t.Errorf("unexpected envelope: %+v", got)
	}
}

// TestLatestReleaseAnswersItsOwnDefaultCallAcrossChannels is the handler-level guard for
// the defect that broke GET /products/{slug}/latest: every release a real product has
// carries a channel, and an omitted ?channel must therefore mean ANY channel rather than
// "the channel whose name is empty".
//
// Every other test in this file registers its latest release under channel "", so all of
// them passed while the endpoint's documented default call answered 404 against the real
// database for the one product in the pilot dataset that has releases. This one registers
// nothing under "" -- exactly the shape of the RouterOS rows -- so it fails if the fake
// or the handler ever goes back to reading "" as a channel name. It also pins the
// cross-channel choice to domain.LatestComparison, so the answer here and the product
// page's headline latestRelease cannot name different releases (ADR-0017: release date,
// then first-observed time, never the version string).
func TestLatestReleaseAnswersItsOwnDefaultCallAcrossChannels(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)
	older, _ := domain.NewExactDate(2024, time.May, 2)
	newer, _ := domain.NewExactDate(2026, time.February, 11)
	h.releases.setLatest("prd_routeros", "long_term", domain.Release{
		ID:          "rel_01J8Z3K9QWERTYUIOPASDFG1",
		Version:     mustVersion(t, "6.49.18"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS,
		Channel:     "long_term",
		ReleaseDate: older,
	})
	h.releases.setLatest("prd_routeros", "stable", domain.Release{
		ID:          "rel_01J8Z3K9QWERTYUIOPASDFG2",
		Version:     mustVersion(t, "7.24.2"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS,
		Channel:     "stable",
		ReleaseDate: newer,
	})

	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/latest")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /latest with no ?channel: status = %d, want 200; an omitted channel means any channel, "+
			"not the channel whose name is empty. Body: %s", w.Code, w.Body.String())
	}
	var got LatestReleaseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.LatestRelease.RawVersion != "7.24.2" {
		t.Errorf("latest across channels = %q, want the release domain.LatestComparison picks (7.24.2, the newer release date)",
			got.LatestRelease.RawVersion)
	}

	scoped := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/latest?channel=long_term")
	if scoped.Code != http.StatusOK {
		t.Fatalf("GET /latest?channel=long_term: status = %d, want 200; body: %s", scoped.Code, scoped.Body.String())
	}
	if err := json.Unmarshal(scoped.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.LatestRelease.RawVersion != "6.49.18" {
		t.Errorf("latest on long_term = %q, want 6.49.18; naming a channel must still restrict, or the fix over-reaches",
			got.LatestRelease.RawVersion)
	}
}

func TestLatestReleaseIsNotFoundWhenTheProductHasNoRelease(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)

	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/latest")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestGetReleaseValidatesTheIDShape(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.releases.put("prd_routeros", domain.Release{
		ID:          "rel_01J8Z3K9QWERTYUIOPASDFGH",
		Version:     mustVersion(t, "7.24.2"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS,
	})

	if w := h.do(http.MethodGet, "/api/v1/releases/not-a-release-id"); w.Code != http.StatusBadRequest {
		t.Errorf("malformed id: status = %d, want 400", w.Code)
	}
	if w := h.do(http.MethodGet, "/api/v1/releases/rel_ZZZZZZZZZZZZZZZZZZZZ"); w.Code != http.StatusNotFound {
		t.Errorf("well-formed unknown id: status = %d, want 404", w.Code)
	}
	w := h.do(http.MethodGet, "/api/v1/releases/rel_01J8Z3K9QWERTYUIOPASDFGH")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if cc := w.Header().Get(HeaderCacheControl); cc != cacheImmutable {
		t.Errorf("Cache-Control = %q, want %q (a published release is immutable)", cc, cacheImmutable)
	}
}

// This is the test that would have caught the production crash: the release detail
// page dereferences release.source.url and release.evidence unconditionally, but
// PresentRelease used to leave both nil, because releases.evidence_id being NOT NULL
// was treated as an optional relationship instead of a guarantee. A response with no
// "source" key here is the bug, not a documented absence -- every real release fetched
// through GetByID carries real source and evidence data.
func TestGetReleaseCarriesRealSourceAndEvidence(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	retrievedAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	h.releases.put("prd_routeros", domain.Release{
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
	})

	w := h.do(http.MethodGet, "/api/v1/releases/rel_01J8Z3K9QWERTYUIOPASDFGH")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"source":{`) {
		t.Fatalf(`response has no "source" object -- this is the crash bug: %s`, body)
	}
	if !strings.Contains(body, `"evidence":{`) {
		t.Fatalf(`response has no "evidence" object -- this is the crash bug: %s`, body)
	}
	var got ReleaseDTO
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Source == nil {
		t.Fatal("Source is nil")
	}
	if got.Source.URL != "https://mikrotik.com/download/changelogs" || got.Source.Kind != "html" || !got.Source.Official {
		t.Errorf("Source = %+v, unexpected", *got.Source)
	}
	if got.Evidence == nil {
		t.Fatal("Evidence is nil")
	}
	if got.Evidence.Excerpt != "7.24.2 (2026-09-02) - stable" {
		t.Errorf("Evidence.Excerpt = %q, unexpected", got.Evidence.Excerpt)
	}
}

// TestGetReleaseCarriesARealProduct is the revert detector for a second, previously
// undiagnosed crash on the same page: handleGetRelease always called
// PresentRelease(release, nil) because GET /releases/{id} carries no product context
// of its own the way a product-scoped read already has, so `product` was always
// omitted even after Source/Evidence were fixed, and the release detail page
// dereferences release.product.slug with no guard. Found by loading the real page
// against the real dev database, not by reading a diagram -- see
// docs/architecture/consistency-report.md's "Closed since the last report" for how.
func TestGetReleaseCarriesARealProduct(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.releases.put("prd_routeros", domain.Release{
		ID:          "rel_01J8Z3K9QWERTYUIOPASDFGH",
		Version:     mustVersion(t, "7.24.2"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS,
		Channel:     "stable",
		EvidenceID:  "evd_01J8Z3K9QWERTYUIOPASDFGH",
	})

	w := h.do(http.MethodGet, "/api/v1/releases/rel_01J8Z3K9QWERTYUIOPASDFGH")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var got ReleaseDTO
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Product == nil {
		t.Fatal(`Product is nil -- the release page dereferences release.product.slug with no guard, so an omitted key here is a 500, not a rendering gap`)
	}
	if got.Product.Slug != "prd_routeros" {
		t.Errorf("Product.Slug = %q, want the release's real mapped product %q", got.Product.Slug, "prd_routeros")
	}
}

func TestVendorEndpoints(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.vendors.put(domain.Vendor{Slug: "mikrotik", Name: "MikroTik", HomepageURL: "https://mikrotik.com"})

	w := h.do(http.MethodGet, "/api/v1/vendors")
	if w.Code != http.StatusOK {
		t.Fatalf("list: status = %d", w.Code)
	}
	var list VendorListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list.Vendors) != 1 || list.Vendors[0].Slug != "mikrotik" {
		t.Errorf("unexpected list: %+v", list)
	}

	if w := h.do(http.MethodGet, "/api/v1/vendors/mikrotik"); w.Code != http.StatusOK {
		t.Errorf("detail: status = %d", w.Code)
	}
	if w := h.do(http.MethodGet, "/api/v1/vendors/nobody"); w.Code != http.StatusNotFound {
		t.Errorf("missing vendor: status = %d, want 404", w.Code)
	}
	// A slug that cannot exist is a client mistake, named as such.
	if w := h.do(http.MethodGet, "/api/v1/vendors/Not_A_Slug"); w.Code != http.StatusBadRequest {
		t.Errorf("malformed slug: status = %d, want 400", w.Code)
	}
}

// Liveness must not consult a dependency: a liveness probe that fails during a
// database incident restarts every instance and turns a degraded read path into a
// crash loop.
func TestHealthzIsUnconditional(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(d *Deps, _ *harness) {
		d.Ready = func(context.Context) error { return errors.New("database down") }
	})

	w := h.do(http.MethodGet, "/healthz")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with a failing readiness gate", w.Code)
	}
	var hs HealthStatus
	if err := json.Unmarshal(w.Body.Bytes(), &hs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if hs.Status != StatusOK {
		t.Errorf("status = %q, want ok", hs.Status)
	}
}

func TestReadyzReportsTheDependency(t *testing.T) {
	t.Parallel()
	down := errors.New("dial tcp: connection refused")
	h := newHarness(t, func(d *Deps, _ *harness) {
		d.Ready = func(context.Context) error { return down }
	})

	w := h.do(http.MethodGet, "/readyz")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != ProblemContentType {
		t.Errorf("Content-Type = %q, want %q", ct, ProblemContentType)
	}
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Type != TypeServiceUnavailable {
		t.Errorf("type = %q, want %q", p.Type, TypeServiceUnavailable)
	}
	if strings.Contains(w.Body.String(), "connection refused") {
		t.Errorf("readiness leaked the underlying error: %s", w.Body.String())
	}

	ready := newHarness(t)
	if w := ready.do(http.MethodGet, "/readyz"); w.Code != http.StatusOK {
		t.Errorf("with no gate configured: status = %d, want 200", w.Code)
	}
}

func TestUnroutedPathAnswersWithAProblemDocument(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	w := h.do(http.MethodGet, "/api/v1/nonsense")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != ProblemContentType {
		t.Errorf("Content-Type = %q, want %q -- every failure uses one media type", ct, ProblemContentType)
	}
	if w.Header().Get(HeaderRequestID) == "" {
		t.Error("even an unrouted request gets a correlation id")
	}
}

func TestMethodNotMatchedFallsThroughToTheProblemHandler(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)

	w := h.do(http.MethodPost, "/api/v1/products/mikrotik-routeros")
	if w.Code != http.StatusMethodNotAllowed && w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 405 or 404", w.Code)
	}
}

// ---------------------------------------------------------------------------
// tier-based history windowing
// ---------------------------------------------------------------------------

// seedTwoYearsOfReleases puts one release inside a 12-month window and one well outside
// it, so a windowed caller and an unwindowed one cannot get the same answer.
func seedTwoYearsOfReleases(t *testing.T, h *harness) {
	t.Helper()
	recent, err := domain.NewExactDate(2026, time.June, 1)
	if err != nil {
		t.Fatalf("NewExactDate: %v", err)
	}
	ancient, err := domain.NewExactDate(2023, time.February, 14)
	if err != nil {
		t.Fatalf("NewExactDate: %v", err)
	}
	h.releases.put("prd_routeros", domain.Release{
		ID: "rel_recent0000000000000", Version: mustVersion(t, "7.24.0"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS, Channel: "stable",
		ReleaseDate: recent, FirstObservedAt: time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
	})
	h.releases.put("prd_routeros", domain.Release{
		ID: "rel_ancient000000000000", Version: mustVersion(t, "6.49.0"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS, Channel: "stable",
		ReleaseDate: ancient, FirstObservedAt: time.Date(2023, 2, 15, 0, 0, 0, 0, time.UTC),
	})
}

func decodeReleaseList(t *testing.T, w *httptest.ResponseRecorder) ReleaseListResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var got ReleaseListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body %s", err, w.Body.String())
	}
	return got
}

// The revert detector for tier-based history windowing.
//
// It asserts three separate things, because any one of them alone can pass while the
// window is broken: the repository was handed a Since (a filter applied to a returned
// page would break cursors and would not show up here), the older release is actually
// absent, and the response says out loud that it was windowed.
func TestReleasesWindowedForAnonymousAndFree(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		plan  string
		token string
	}{
		{name: "anonymous", plan: application.PlanAnonymous},
		{name: "free", plan: application.PlanFree, token: "free-key"},
		// An unrecognised plan is windowed too: failing closed under-serves a caller
		// whose plan name is misspelled, which is recoverable; failing open would hand
		// the paid archive to anyone who sent a plan string nobody implemented.
		{name: "unrecognised plan", plan: "platinum-elite", token: "odd-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			seedProduct(h)
			seedTwoYearsOfReleases(t, h)
			if tc.token != "" {
				h.apiKeys.add(hashKey(tc.token), "key_"+tc.name,
					application.APIConsumer{ID: "con_" + tc.name, Plan: tc.plan, Status: "active"})
			}

			w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/releases", func(r *http.Request) {
				if tc.token != "" {
					r.Header.Set(HeaderAuthorization, "Bearer "+tc.token)
				}
			})
			got := decodeReleaseList(t, w)

			// The window reached the query, not a filter over the page it returned.
			h.releases.mu.Lock()
			since := h.releases.lastOpts.Since
			h.releases.mu.Unlock()
			want := application.HistoryWindow(tc.plan, h.clock.Now())
			if !since.Equal(want) {
				t.Errorf("ListForProduct was handed Since = %v, want %v", since, want)
			}
			if since.IsZero() {
				t.Fatal("no window was applied at all for a windowed plan")
			}

			if len(got.Releases) != 1 || got.Releases[0].ID != "rel_recent0000000000000" {
				t.Errorf("a release older than the window was served: %+v", got.Releases)
			}
			// And the response is honest about it. A consumer who cannot tell a
			// windowed history from a complete one has been misled by omission.
			if !got.Window.Windowed {
				t.Error("window.windowed = false on a windowed response")
			}
			if got.Window.Since == "" {
				t.Error("window.since is absent; the caller cannot tell where the window starts")
			}
			if !strings.Contains(got.Window.Detail, "Professional") {
				t.Errorf("window.detail = %q, want it to name the plan that lifts the window", got.Window.Detail)
			}
			if _, err := time.Parse(time.RFC3339, got.Window.Since); err != nil {
				t.Errorf("window.since = %q is not RFC 3339: %v", got.Window.Since, err)
			}
		})
	}
}

func TestReleasesUnwindowedForProfessional(t *testing.T) {
	t.Parallel()

	for _, plan := range []string{application.PlanProfessional, application.PlanEnterprise, application.PlanInternal} {
		t.Run(plan, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			seedProduct(h)
			seedTwoYearsOfReleases(t, h)
			h.apiKeys.add(hashKey("paid"), "key_paid",
				application.APIConsumer{ID: "con_paid", Plan: plan, Status: "active"})

			w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/releases", func(r *http.Request) {
				r.Header.Set(HeaderAuthorization, "Bearer paid")
			})
			got := decodeReleaseList(t, w)

			h.releases.mu.Lock()
			since := h.releases.lastOpts.Since
			h.releases.mu.Unlock()
			if !since.IsZero() {
				t.Errorf("a paid plan was windowed at %v", since)
			}
			if len(got.Releases) != 2 {
				t.Errorf("releases = %d, want the complete archive: %+v", len(got.Releases), got.Releases)
			}
			if got.Window.Windowed {
				t.Error("window.windowed = true for a plan entitled to the whole archive")
			}
			// There is no boundary to state, so neither member is sent.
			if got.Window.Since != "" || got.Window.Detail != "" {
				t.Errorf("an unwindowed response stated a boundary: %+v", got.Window)
			}
		})
	}
}

// The same request, two callers, two different history spans. This is the property the
// window exists for, asserted end to end in one test so a revert cannot pass by fixing
// one half.
func TestTheSameRequestReturnsDifferentSpansPerTier(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)
	seedTwoYearsOfReleases(t, h)
	h.apiKeys.add(hashKey("paid"), "key_paid",
		application.APIConsumer{ID: "con_paid", Plan: application.PlanProfessional, Status: "active"})

	const target = "/api/v1/products/mikrotik-routeros/releases"
	anon := decodeReleaseList(t, h.do(http.MethodGet, target))
	paid := decodeReleaseList(t, h.do(http.MethodGet, target, func(r *http.Request) {
		r.Header.Set(HeaderAuthorization, "Bearer paid")
	}))

	if len(anon.Releases) >= len(paid.Releases) {
		t.Fatalf("anonymous got %d releases and Professional got %d; the window is not applied",
			len(anon.Releases), len(paid.Releases))
	}
	if anon.Window.Windowed == paid.Window.Windowed {
		t.Errorf("both tiers reported window.windowed = %v", anon.Window.Windowed)
	}
	// The 12-month figure is not restated here as a literal: it is
	// application.HistoryWindowMonths, and a test that hardcoded 12 would keep passing
	// after somebody changed the constant.
	wantSince := h.clock.Now().AddDate(0, -application.HistoryWindowMonths, 0)
	if got, err := time.Parse(time.RFC3339, anon.Window.Since); err != nil {
		t.Fatalf("window.since: %v", err)
	} else if !got.Equal(wantSince.UTC().Truncate(time.Second)) {
		t.Errorf("window.since = %v, want %v (%d months)", got, wantSince, application.HistoryWindowMonths)
	}
}

// ---------------------------------------------------------------------------
// conflict surfacing
// ---------------------------------------------------------------------------

// The key is always present, in both states. An absent key would read as "no conflict",
// and "we checked and they agree" is a different claim from "we did not check" --
// which is the whole reason ADR-0020 records a disagreement rather than arbitrating it.
func TestProductExposesHasSourceConflict(t *testing.T) {
	t.Parallel()

	for _, conflicted := range []bool{true, false} {
		h := newHarness(t)
		s := seedProduct(h)
		s.HasSourceConflict = conflicted
		h.summaries.put(s)

		w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		var got ProductDTO
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.HasSourceConflict != conflicted {
			t.Errorf("hasSourceConflict = %v, want %v", got.HasSourceConflict, conflicted)
		}
		if !strings.Contains(w.Body.String(), `"hasSourceConflict":`) {
			t.Errorf("the key must be present even when false: %s", w.Body.String())
		}
	}
}

// ---------------------------------------------------------------------------
// the read path goes through the read use cases
// ---------------------------------------------------------------------------

// The drift detector for the page bounds.
//
// api.md §2 documents "limit (default 20, max 100)" for release history. When the
// handler owned its own clamp and the use case owned another, ?limit=200 produced 100
// rows on the served path and 50 through the use case -- one documented contract, two
// implementations, and only one of them reachable. The bounds are now the application's
// own constants, so this asserts both that the documented numbers are what the query is
// asked for and that nobody has re-forked the constant.
func TestReleaseLimitIsClampedToTheDocumentedMaximum(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)
	seedTwoYearsOfReleases(t, h)

	if releaseLimitMax != application.MaxReleasePageSize {
		t.Errorf("releaseLimitMax = %d and application.MaxReleasePageSize = %d; the bound has been forked",
			releaseLimitMax, application.MaxReleasePageSize)
	}
	// The default needs the same pin as the maximum. It had none, so setting this
	// constant to 50 left the entire suite green while the endpoint returned 50
	// releases against the 20 api.md §2 and openapi.yaml both publish -- the exact
	// drift the release page bounds were consolidated to end, surviving in the file
	// that added the consolidation test.
	if releaseLimitDefault != application.DefaultReleasePageSize {
		t.Errorf("releaseLimitDefault = %d and application.DefaultReleasePageSize = %d; the documented default has been forked",
			releaseLimitDefault, application.DefaultReleasePageSize)
	}
	if searchLimitDefault != application.DefaultSearchResults || searchLimitMax != application.MaxSearchResults {
		t.Errorf("the search bounds have been forked from the application's: %d/%d vs %d/%d",
			searchLimitDefault, searchLimitMax, application.DefaultSearchResults, application.MaxSearchResults)
	}

	for _, tc := range []struct {
		query string
		want  int
	}{
		{query: "", want: releaseLimitDefault},
		{query: "?limit=200", want: releaseLimitMax},
		{query: "?limit=5", want: 5},
	} {
		if w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/releases"+tc.query); w.Code != http.StatusOK {
			t.Fatalf("%q: status = %d; body %s", tc.query, w.Code, w.Body.String())
		}
		h.releases.mu.Lock()
		got := h.releases.lastOpts.Limit
		h.releases.mu.Unlock()
		if got != tc.want {
			t.Errorf("%q: the query was asked for %d rows, want %d (api.md §2: default 20, max 100)",
				tc.query, got, tc.want)
		}
	}
}

// Search is answered by the use case, not by this adapter reaching for the search port.
//
// The observable difference is what the analytics event carries: SearchProducts records
// the result count on every search and the query text only when the search found
// nothing, because a zero-result search is a product request while a successful one is
// somebody's browsing history. A handler that ran the search itself put the raw query on
// every event, so this fails the moment that code comes back.
func TestSearchAnalyticsComeFromTheUseCase(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.summaries.search = []application.ProductSummary{{ProductSlug: "mikrotik-routeros", ProductName: "RouterOS"}}

	if w := h.do(http.MethodGet, "/api/v1/search?q=routeros"); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	executed := h.events.withName(domain.EventSearchExecuted)
	if len(executed) != 1 {
		t.Fatalf("search_executed events = %d, want 1", len(executed))
	}
	if got := executed[0].Attributes["result_count"]; got != "1" {
		t.Errorf("result_count = %q, want \"1\"", got)
	}
	if got, ok := executed[0].Attributes["query"]; ok {
		t.Errorf("a successful search recorded the query text %q; only a zero-result search does", got)
	}
}

// One product view per product request, published once, by the use case.
func TestProductViewIsPublishedOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)

	if w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros"); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	viewed := h.events.withName(domain.EventProductViewed)
	if len(viewed) != 1 {
		t.Fatalf("product_viewed events = %d, want exactly 1", len(viewed))
	}
	if viewed[0].SubjectID != "prd_routeros" || viewed[0].ProductSlug != "mikrotik-routeros" {
		t.Errorf("unexpected event: %+v", viewed[0])
	}
}

// TestVendorViewIsPublishedOnce is the revert detector for the wiring, not for the rule.
//
// application.GetVendor publishes domain.EventVendorViewed and is the only publisher of
// it, but for the whole of Phase 2 handleGetVendor called VendorRepository.GetBySlug
// directly, so the use case had zero callers and the event was never emitted by a running
// system. Unit tests on the use case could not see that: a use case nobody calls still
// passes its own tests. Only an assertion made through the router can, which is why this
// one drives the HTTP endpoint and then asserts on the published event.
func TestVendorViewIsPublishedOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.vendors.put(domain.Vendor{ID: "vnd_mikrotik", Slug: "mikrotik", Name: "MikroTik"})

	if w := h.do(http.MethodGet, "/api/v1/vendors/mikrotik"); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	// A 404 and a 400 are not views and must not be counted.
	if w := h.do(http.MethodGet, "/api/v1/vendors/nobody"); w.Code != http.StatusNotFound {
		t.Fatalf("missing vendor: status = %d, want 404", w.Code)
	}
	if w := h.do(http.MethodGet, "/api/v1/vendors/Not_A_Slug"); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed slug: status = %d, want 400", w.Code)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	viewed := h.events.withName(domain.EventVendorViewed)
	if len(viewed) != 1 {
		t.Fatalf("vendor_viewed events = %d, want exactly 1; the handler must go through application.GetVendor, which is the only publisher of this event", len(viewed))
	}
	if viewed[0].SubjectID != "vnd_mikrotik" {
		t.Errorf("subject = %q, want the vendor id vnd_mikrotik", viewed[0].SubjectID)
	}
	if got := viewed[0].Attributes["vendor_slug"]; got != "mikrotik" {
		t.Errorf("vendor_slug = %q, want \"mikrotik\"", got)
	}
}

// ---------------------------------------------------------------------------
// /latest tells the truth about a contested answer
// ---------------------------------------------------------------------------

// The endpoint whose whole purpose is answering "what version should I be on?" must not
// hand back a contested version with nothing to check. The flag is present in both
// states: an absent key reads as "no conflict", which is a different claim from "we did
// not check" (ADR-0020).
func TestLatestReleaseReportsAnOpenSourceConflict(t *testing.T) {
	t.Parallel()

	for _, conflicted := range []bool{true, false} {
		h := newHarness(t)
		s := seedProduct(h)
		s.HasSourceConflict = conflicted
		h.summaries.put(s)
		h.releases.setLatest("prd_routeros", "", domain.Release{
			ID: "rel_01J8Z3K9QWERTYUIOPASDFGH", Version: mustVersion(t, "7.24.2"),
			ReleaseType: domain.ReleaseTypeEmbeddedOS, Channel: "stable",
		})

		w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/latest")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"hasSourceConflict":`) {
			t.Errorf("the key must be present even when false: %s", w.Body.String())
		}
		var got LatestReleaseResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.HasSourceConflict != conflicted {
			t.Errorf("hasSourceConflict = %v, want %v", got.HasSourceConflict, conflicted)
		}
	}
}

// The vendor's own designation is passed through, and it is null when the vendor never
// made one. The newest release is not automatically the safest, so the two are separate
// members and one is never substituted for the other.
func TestLatestReleaseCarriesTheVendorRecommendation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	s := seedProduct(h)
	h.releases.setLatest("prd_routeros", "", domain.Release{
		ID: "rel_01J8Z3K9QWERTYUIOPASDFGH", Version: mustVersion(t, "7.24.2"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS, Channel: "stable",
	})

	// No designation: the member is present and null.
	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/latest")
	if !strings.Contains(w.Body.String(), `"recommendedRelease":null`) {
		t.Errorf(`want "recommendedRelease":null when the vendor designated none: %s`, w.Body.String())
	}

	// A designation, and it is an older release than the newest one.
	recommended := true
	h.releases.put("prd_routeros", domain.Release{
		ID: "rel_recommended000000", Version: mustVersion(t, "7.22.4"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS, Channel: "long_term", Recommended: &recommended,
	})
	s.RecommendedReleaseID = "rel_recommended000000"
	h.summaries.put(s)

	w = h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/latest")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", w.Code, w.Body.String())
	}
	var got LatestReleaseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.RecommendedRelease == nil || got.RecommendedRelease.ID != "rel_recommended000000" {
		t.Fatalf("recommendedRelease = %+v, want the vendor's designated release", got.RecommendedRelease)
	}
	if got.LatestRelease.ID == got.RecommendedRelease.ID {
		t.Error("the newest release was substituted for the vendor's recommendation")
	}
}

// The two 404s on this endpoint are different answers and must read differently: a slug
// nobody has heard of, and a product whose channel has no release.
func TestLatestReleaseDistinguishesItsTwoNotFounds(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)

	missingProduct := h.do(http.MethodGet, "/api/v1/products/no-such-product/latest")
	noRelease := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/latest")
	if missingProduct.Code != http.StatusNotFound || noRelease.Code != http.StatusNotFound {
		t.Fatalf("status: unknown product = %d, no release = %d", missingProduct.Code, noRelease.Code)
	}

	// A third answer, distinct from both: the caller DID name a channel and that
	// channel has nothing. Only then may the detail blame a channel -- an omitted
	// ?channel means any channel, so telling a caller who named none that "this
	// product has no published release on that channel" points them at a filter they
	// never applied.
	noChannelNamed := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/latest")
	channelNamed := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/latest?channel=testing")
	if channelNamed.Code != http.StatusNotFound {
		t.Fatalf("named-channel miss: status = %d, want 404", channelNamed.Code)
	}
	var noneNamed, named Problem
	_ = json.Unmarshal(noChannelNamed.Body.Bytes(), &noneNamed)
	_ = json.Unmarshal(channelNamed.Body.Bytes(), &named)
	if strings.Contains(noneNamed.Detail, "channel") {
		t.Errorf("detail for a call that named no channel blames one: %q", noneNamed.Detail)
	}
	if !strings.Contains(named.Detail, "channel") {
		t.Errorf("detail for a call that named a channel does not say so: %q", named.Detail)
	}
	var a, b Problem
	_ = json.Unmarshal(missingProduct.Body.Bytes(), &a)
	_ = json.Unmarshal(noRelease.Body.Bytes(), &b)
	if !strings.Contains(a.Detail, "vendor") && !strings.Contains(a.Detail, "product") {
		t.Errorf("unknown product detail = %q", a.Detail)
	}
	// This used to require the word "channel" here. It was asserting the endpoint's
	// misdiagnosis: `noRelease` names no channel, so a detail blaming one points the
	// caller at a filter they never applied. What actually has to hold is that the
	// detail says the product has no release -- checked against the release-shaped
	// half of the sentence, and separated from the unknown-product 404 below.
	if !strings.Contains(b.Detail, "release") {
		t.Errorf("no-release detail = %q, want it to say the product has no release", b.Detail)
	}
	if a.Detail == b.Detail {
		t.Errorf("both 404s read the same: %q", a.Detail)
	}
}

// ---------------------------------------------------------------------------
// /latest and /releases stop contradicting each other
// ---------------------------------------------------------------------------

// seedDiscontinuedProduct is the normal state of the hardware this catalogue tracks: a
// product whose newest release is years old. Its summary points at that release, which is
// what the public site reads.
func seedDiscontinuedProduct(t *testing.T, h *harness) {
	t.Helper()
	ancient, err := domain.NewExactDate(2023, time.February, 14)
	if err != nil {
		t.Fatalf("NewExactDate: %v", err)
	}
	s := seedProduct(h)
	s.LatestReleaseID = "rel_ancient000000000000"
	s.LatestRawVersion = "6.49.0"
	s.LatestReleaseDate = ancient
	h.summaries.put(s)

	rel := domain.Release{
		ID: "rel_ancient000000000000", Version: mustVersion(t, "6.49.0"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS, Channel: "stable",
		ReleaseDate: ancient, FirstObservedAt: time.Date(2023, 2, 15, 0, 0, 0, 0, time.UTC),
	}
	h.releases.put("prd_routeros", rel)
	h.releases.setLatest("prd_routeros", "", rel)
}

// The revert detector for the /latest-versus-/releases contradiction.
//
// An anonymous caller asking a discontinued product for its latest release gets an
// answer -- the window bounds history depth, never the current fact, and answering "no
// published release" for a product that has one would be false. The history page is
// therefore empty for the same caller, and it says why: window.latestOutsideWindow, and
// a detail that names the other endpoint. Without that, the catalogue answers one
// question two ways and leaves the consumer to guess which to believe.
func TestLatestIsNotWindowedAndTheHistoryPageSaysSo(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedDiscontinuedProduct(t, h)

	latest := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/latest")
	if latest.Code != http.StatusOK {
		t.Fatalf("anonymous /latest: status = %d, want 200; body %s", latest.Code, latest.Body.String())
	}
	var got LatestReleaseResponse
	if err := json.Unmarshal(latest.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.LatestRelease.RawVersion != "6.49.0" {
		t.Errorf("latest release = %q, want the product's newest observed release", got.LatestRelease.RawVersion)
	}

	history := decodeReleaseList(t, h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/releases"))
	if len(history.Releases) != 0 {
		t.Fatalf("the window did not apply to history: %+v", history.Releases)
	}
	if !history.Window.Windowed {
		t.Fatal("window.windowed = false on a windowed response")
	}
	if !history.Window.LatestOutsideWindow {
		t.Error("an empty page whose product has a newer-than-nothing release did not say the latest is outside the window")
	}
	if !strings.Contains(history.Window.Detail, "latest") {
		t.Errorf("window.detail = %q, want it to name the endpoint that still answers", history.Window.Detail)
	}
}

// A plan with no window has no boundary, so nothing can fall outside it and the member
// stays the bare {"windowed": false} the contract promises.
func TestLatestOutsideWindowIsAbsentForAnUnwindowedPlan(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedDiscontinuedProduct(t, h)
	h.apiKeys.add(hashKey("paid"), "key_paid",
		application.APIConsumer{ID: "con_paid", Plan: application.PlanProfessional, Status: "active"})

	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/releases", func(r *http.Request) {
		r.Header.Set(HeaderAuthorization, "Bearer paid")
	})
	got := decodeReleaseList(t, w)
	if len(got.Releases) != 1 {
		t.Errorf("releases = %d, want the complete archive", len(got.Releases))
	}
	if got.Window.LatestOutsideWindow {
		t.Error("an unwindowed response claimed its latest release is outside a window it does not have")
	}
	if !strings.Contains(w.Body.String(), `"window":{"windowed":false}`) {
		t.Errorf("an unwindowed window stated more than it knows: %s", w.Body.String())
	}
}

// The honest rule for a reduced-precision date at a window boundary.
//
// A release the vendor dated only "2025" is anchored at 1 January 2025, and that anchor
// is padding, not data (ADR-0017): the vendor may have shipped it in December. Claiming
// it is outside a window that opens in September 2025 would assert a day nobody
// published, so the flag compares the end of the period the published precision denotes
// (domain.PartialDate.PeriodEnd) and stays silent when it cannot tell.
//
// It must give the same verdict as the query that builds the page, which compares that
// same period end against the boundary reduced to a UTC calendar day. The boundary here
// therefore carries a time of day on purpose: it is the shape HistoryWindow actually
// produces (now minus twelve months, at whatever hour the request arrived), and it is
// the input under which a flag that compared PeriodEnd against the raw instant would
// contradict the page for a release dated on the boundary day itself.
func TestLatestOutsideWindowNeverAssertsAPrecisionTheVendorDidNotPublish(t *testing.T) {
	t.Parallel()
	since := time.Date(2025, 9, 5, 12, 0, 0, 0, time.UTC)

	year2025, err := domain.NewYearDate(2025)
	if err != nil {
		t.Fatalf("NewYearDate: %v", err)
	}
	year2023, err := domain.NewYearDate(2023)
	if err != nil {
		t.Fatalf("NewYearDate: %v", err)
	}
	sep2025, err := domain.NewMonthDate(2025, time.September)
	if err != nil {
		t.Fatalf("NewMonthDate: %v", err)
	}
	aug2025, err := domain.NewMonthDate(2025, time.August)
	if err != nil {
		t.Fatalf("NewMonthDate: %v", err)
	}
	boundaryDay, err := domain.NewExactDate(2025, time.September, 5)
	if err != nil {
		t.Fatalf("NewExactDate: %v", err)
	}
	dayBefore, err := domain.NewExactDate(2025, time.September, 4)
	if err != nil {
		t.Fatalf("NewExactDate: %v", err)
	}

	cases := []struct {
		name string
		date domain.PartialDate
		want bool
	}{
		{"year precision covering the boundary", year2025, false},
		{"year precision entirely before it", year2023, true},
		{"month precision covering the boundary", sep2025, false},
		{"month precision entirely before it", aug2025, true},
		{"the boundary day itself", boundaryDay, false},
		{"the day before", dayBefore, true},
		{"no date at all", domain.UnknownDate, false},
	}
	for _, tc := range cases {
		summary := application.ProductSummary{LatestReleaseID: "rel_x", LatestReleaseDate: tc.date}
		if got := latestOutsideWindow(summary, since); got != tc.want {
			t.Errorf("%s: latestOutsideWindow = %v, want %v", tc.name, got, tc.want)
		}
	}

	// The contradiction case, pinned separately because it is the one a plausible
	// simplification reintroduces: a boundary late in the day, and a release dated that
	// same day. The query returns that release -- its period end is the boundary's UTC
	// day, and the predicate is >= -- so the response may not simultaneously assert that
	// it cannot appear.
	lateBoundary := time.Date(2025, 9, 5, 23, 59, 59, 0, time.UTC)
	if latestOutsideWindow(application.ProductSummary{LatestReleaseID: "rel_x", LatestReleaseDate: boundaryDay}, lateBoundary) {
		t.Error("flagged the latest release as outside a window the query includes it in; the flag and the page must reduce the boundary to a UTC day the same way")
	}

	// No window, nothing to be outside of; and a product with no release at all makes
	// no claim either.
	if latestOutsideWindow(application.ProductSummary{LatestReleaseID: "rel_x", LatestReleaseDate: year2023}, time.Time{}) {
		t.Error("claimed a release is outside a window that does not exist")
	}
	if latestOutsideWindow(application.ProductSummary{LatestReleaseDate: year2023}, since) {
		t.Error("claimed a latest release the summary does not have")
	}
}

// ---------------------------------------------------------------------------
// filtered pages and cursors
// ---------------------------------------------------------------------------

// channel and releaseType are applied to the page the query returned, so a filtered page
// can be empty while nextCursor is non-null. That is what the code does and what
// openapi.yaml and api.md now say; the contract previously implied the opposite, and a
// client that stopped on an empty page would have concluded there are no matching
// releases when there may be many.
//
// This pins the documented behaviour in both directions: if the filter is later pushed
// into the query, the contract has to change with it and this test is where that shows.
func TestAFilteredPageCanBeEmptyWithANonNullCursor(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)
	h.releases.next = "cursor_opaque"
	h.releases.put("prd_routeros", domain.Release{
		ID: "rel_stable0000000000000", Version: mustVersion(t, "7.24.2"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS, Channel: "stable",
		FirstObservedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	})

	got := decodeReleaseList(t, h.do(http.MethodGet,
		"/api/v1/products/mikrotik-routeros/releases?channel=long_term"))
	if len(got.Releases) != 0 {
		t.Fatalf("releases = %+v, want none matching the filter", got.Releases)
	}
	if got.Pagination.NextCursor == nil {
		t.Fatal("the cursor was dropped; a client cannot page past a filtered-empty page")
	}
	if *got.Pagination.NextCursor != "cursor_opaque" {
		t.Errorf("nextCursor = %q, want the cursor the query minted", *got.Pagination.NextCursor)
	}
}

// TestWindowKeepsAReducedPrecisionReleaseAtTheBoundary is the HTTP-layer revert detector
// for the window rule, and it exists because there was none.
//
// The rule was corrected in the SQL adapter and in the latestOutsideWindow flag while
// this package's test double went on comparing the stored anchor, so every windowing
// test here asserted the pre-fix behaviour: reinstating the anchor comparison left the
// whole httpapi suite green. The double now delegates to application.WithinHistoryWindow
// so it cannot diverge again, but delegation only removes the disagreement -- it does
// not, by itself, exercise the case where the two rules differ. This test does.
//
// The harness clock is 2026-09-03T18:30Z, so an anonymous caller's window opens on
// 2025-09-03. A release the vendor dated only "2025-09" anchors at the 1st and its period
// ends on the 30th. Comparing the period end includes it; comparing the anchor drops it.
// Dropping it produces the state api.md §3.3a says cannot occur: an empty page whose own
// window member reports latestOutsideWindow = false, so the response asserts there is
// nothing hidden while hiding something.
func TestWindowKeepsAReducedPrecisionReleaseAtTheBoundary(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	monthOnly, err := domain.NewMonthDate(2025, time.September)
	if err != nil {
		t.Fatalf("NewMonthDate: %v", err)
	}
	s := seedProduct(h)
	s.LatestReleaseID = "rel_monthonly00000000000"
	s.LatestRawVersion = "7.19.0"
	s.LatestReleaseDate = monthOnly
	h.summaries.put(s)

	rel := domain.Release{
		ID: "rel_monthonly00000000000", Version: mustVersion(t, "7.19.0"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS, Channel: "stable",
		ReleaseDate: monthOnly, FirstObservedAt: time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC),
	}
	h.releases.put("prd_routeros", rel)
	h.releases.setLatest("prd_routeros", "", rel)

	got := decodeReleaseList(t, h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros/releases"))

	if len(got.Releases) != 1 {
		t.Fatalf("a release dated only 2025-09 fell out of a window opening 2025-09-03: got %d releases, want 1. "+
			"The window is being compared at the stored anchor (the 1st) instead of the end of the period the "+
			"precision denotes (the 30th), which hides a release the vendor may have shipped after the boundary",
			len(got.Releases))
	}
	if got.Window.LatestOutsideWindow {
		t.Error("window.latestOutsideWindow = true for a release the page itself returned; " +
			"the flag and the page disagree about the same release")
	}
	// The rendered date must still carry only the precision the vendor published. The
	// period end is a comparison bound, never data (domain.PartialDate.PeriodEnd).
	if d := got.Releases[0].ReleaseDate; d != "2025-09" {
		t.Errorf("releaseDate = %q, want %q: comparing at the period end must not leak a day into the response", d, "2025-09")
	}
}
