package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

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
	if p.Type != TypeInvalidRequest {
		t.Errorf("type = %q, want %q", p.Type, TypeInvalidRequest)
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
	if len(gotQuery) != searchQueryMaxRunes {
		t.Errorf("query length = %d, want %d", len(gotQuery), searchQueryMaxRunes)
	}
	if gotLimit != searchLimitMax {
		t.Errorf("limit = %d, want %d", gotLimit, searchLimitMax)
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
