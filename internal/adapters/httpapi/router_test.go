package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/yaml.v3"

	"github.com/macimottin/firmscout/internal/application"
)

func TestNewServerRequiresItsReadPorts(t *testing.T) {
	t.Parallel()
	full := func() Deps {
		return Deps{Summaries: newFakeSummaries(), Vendors: newFakeVendors(), Releases: newFakeReleases(), Logger: discardLogger()}
	}
	cases := map[string]func(*Deps){
		"summaries": func(d *Deps) { d.Summaries = nil },
		"vendors":   func(d *Deps) { d.Vendors = nil },
		"releases":  func(d *Deps) { d.Releases = nil },
	}
	for name, remove := range cases {
		d := full()
		remove(&d)
		if _, err := NewServer(d); err == nil {
			t.Errorf("missing %s: expected an error", name)
		}
	}

	srv, err := NewServer(full())
	if err != nil {
		t.Fatalf("NewServer with every required port: %v", err)
	}
	_ = srv.Close()
}

// Optional dependencies are optional. A deployment with no key store, no meter and no
// event publisher is a valid one -- it is what a self-hoster running the free public
// catalogue has -- and it must serve reads.
func TestServerWorksWithOnlyTheRequiredPorts(t *testing.T) {
	t.Parallel()
	summaries := newFakeSummaries()
	summaries.put(minimalSummary())
	srv, err := NewServer(Deps{
		Summaries:  summaries,
		Vendors:    newFakeVendors(),
		Releases:   newFakeReleases(),
		Logger:     discardLogger(),
		RateLimits: RateLimitConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/products/mikrotik-routeros", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}

	// A credential presented to a server with no key store is refused rather than
	// silently ignored, because ignoring it would grant anonymous access under a name.
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/products/mikrotik-routeros", nil)
	r.Header.Set(HeaderAuthorization, "Bearer anything")
	srv.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 when no key store is configured", w.Code)
	}
}

// A request in flight when the server shuts down must not panic on a closed
// after-response queue.
func TestConcurrentRequestsDuringShutdownDoNotPanic(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)
	h.apiKeys.add(hashKey("k"), "key_1", consumerFixture())

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 20; j++ {
				r := httptest.NewRequest(http.MethodGet, "/api/v1/products/mikrotik-routeros", nil)
				r.Header.Set(HeaderAuthorization, "Bearer k")
				h.ServeHTTP(httptest.NewRecorder(), r)
			}
		}()
	}
	close(start)
	// Close concurrently with in-flight traffic, which is what a rolling deploy does.
	go func() { _ = h.Close() }()
	wg.Wait()
	_ = h.Close()
}

func TestMetricsEndpointServesTheGatherer(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "firmscout_test_probe_total", Help: "probe"})
	reg.MustRegister(counter)
	counter.Inc()

	h := newHarness(t, func(d *Deps, _ *harness) { d.Gatherer = reg })

	w := h.do(http.MethodGet, "/metrics")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "firmscout_test_probe_total") {
		t.Errorf("the configured gatherer was not served:\n%s", w.Body.String())
	}
	// /metrics is an infrastructure endpoint, not part of the versioned API, so it
	// carries none of the API's caching or quota semantics.
	if w.Header().Get(HeaderETag) != "" {
		t.Error("/metrics must not be given a validator")
	}
	if w.Header().Get(HeaderQuotaLimit) != "" {
		t.Error("/metrics must not be metered")
	}
	// And it stays a valid Prometheus exposition.
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") && !strings.Contains(ct, "openmetrics") {
		t.Errorf("Content-Type = %q, want a Prometheus exposition type", ct)
	}
}

func TestRouteTableCoversTheDocumentedSurface(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)

	// Every route in docs/api/openapi.yaml plus the infrastructure endpoints. The
	// assertion is only that the route exists and is reachable: a 404 problem
	// document from the catch-all handler would mean the pattern is not registered.
	for _, target := range []string{
		"/api/v1/search?q=routeros",
		"/api/v1/vendors",
		"/api/v1/vendors/mikrotik",
		"/api/v1/products/mikrotik-routeros",
		"/api/v1/products/mikrotik-routeros/releases",
		"/api/v1/products/mikrotik-routeros/latest",
		"/api/v1/releases/rel_01J8Z3K9QWERTYUIOPASDFGH",
		"/healthz",
		"/readyz",
		"/metrics",
	} {
		w := h.do(http.MethodGet, target)
		if w.Code == http.StatusNotFound {
			var p Problem
			if err := json.Unmarshal(w.Body.Bytes(), &p); err == nil &&
				strings.Contains(p.Detail, "No endpoint matches") {
				t.Errorf("%s is not routed", target)
			}
		}
	}
}

// minimalSummary is the smallest product a router test needs: a slug to route to.
func minimalSummary() application.ProductSummary {
	return application.ProductSummary{
		ProductSlug: "mikrotik-routeros",
		ProductName: "RouterOS",
		VendorSlug:  "mikrotik",
		VendorName:  "MikroTik",
	}
}

func consumerFixture() application.APIConsumer {
	return application.APIConsumer{ID: "con_1", Plan: "professional", Status: "active"}
}

// ---------------------------------------------------------------------------
// handler <-> OpenAPI parity
// ---------------------------------------------------------------------------

// openAPIPath is the repository-relative location of the machine-readable contract.
const openAPIPath = "../../../docs/api/openapi.yaml"

// apiPathPrefix is the prefix openapi.yaml's `servers` entries carry and its path keys
// therefore do not. The infrastructure endpoints declare a path-level server override
// without it, which is why they are matched unprefixed.
const apiPathPrefix = "/api/v1"

// openAPIDocument is the slice of the contract this test reads: the path keys, the
// methods under each, and whether the path declares its own server (which is how the
// document says an endpoint sits outside /api/v1).
type openAPIDocument struct {
	Paths map[string]map[string]any `yaml:"paths"`
}

func loadOpenAPI(t *testing.T) openAPIDocument {
	t.Helper()
	raw, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatalf("read %s: %v", openAPIPath, err)
	}
	var doc openAPIDocument
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", openAPIPath, err)
	}
	if len(doc.Paths) == 0 {
		t.Fatalf("%s declares no paths", openAPIPath)
	}
	return doc
}

// The contract test api.md §10 promises: a handler with no OpenAPI entry, or an
// OpenAPI entry with no handler, fails the build.
//
// It exists because openapi.yaml is maintained by hand alongside this package, and a
// hand-maintained contract with nothing checking it is a comment. This is the same
// "a rule not enforced is a comment" discipline internal/archtest applies to package
// dependencies, applied to the API surface.
func TestEveryRouteIsDocumentedAndEveryDocumentedRouteExists(t *testing.T) {
	t.Parallel()
	h := newHarness(t, enableReviewAPI)

	// What the server actually registers, as "METHOD path" with the /api/v1 prefix
	// stripped so it lines up with the document's keys.
	registered := map[string]bool{}
	for _, rt := range append(h.apiRoutes(), h.systemRoutes()...) {
		method, path, ok := strings.Cut(rt.pattern, " ")
		if !ok {
			t.Fatalf("route pattern %q has no method", rt.pattern)
		}
		registered[method+" "+strings.TrimPrefix(path, apiPathPrefix)] = true
	}

	documented := map[string]bool{}
	for path, item := range loadOpenAPI(t).Paths {
		for key := range item {
			// `servers`, `parameters`, `summary` and the like sit beside the
			// operations; only an HTTP method names one.
			method := strings.ToUpper(key)
			switch method {
			case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
				http.MethodDelete, http.MethodHead, http.MethodOptions:
				documented[method+" "+path] = true
			}
		}
	}

	for route := range registered {
		if !documented[route] {
			t.Errorf("%s is registered by the router and absent from %s", route, openAPIPath)
		}
	}
	for route := range documented {
		if !registered[route] {
			t.Errorf("%s is documented in %s and registered by no handler", route, openAPIPath)
		}
	}
}

// The internal review surface is deliberately absent from the public contract.
//
// openapi.yaml is what a client generator is pointed at, and it must not produce
// bindings for an off-by-default, unauthenticated moderation surface. This asserts the
// omission on purpose, so that adding an /internal path to the document is a test
// failure rather than a quiet decision. See ADR-0021 and api.md §11.
func TestInternalRoutesAreNotInThePublicContract(t *testing.T) {
	t.Parallel()
	h := newHarness(t, enableReviewAPI)

	if len(h.internalRoutes()) == 0 {
		t.Fatal("the review-enabled harness registered no internal routes; the rest of this test proves nothing")
	}
	for path := range loadOpenAPI(t).Paths {
		if strings.HasPrefix(path, "/internal") {
			t.Errorf("%s documents %s; the internal surface must stay out of the public contract", openAPIPath, path)
		}
	}
}

// The route table and the document agree on shapes as well as on paths: a path
// parameter the document declares with a pattern must be the same wildcard the router
// matches, or a generated client builds URLs the server rejects.
func TestPathParametersMatchTheRouterWildcards(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	documented := map[string]bool{}
	for path := range loadOpenAPI(t).Paths {
		documented[path] = true
	}
	for _, rt := range h.apiRoutes() {
		_, path, _ := strings.Cut(rt.pattern, " ")
		path = strings.TrimPrefix(path, apiPathPrefix)
		if strings.Contains(path, "{") && !documented[path] {
			t.Errorf("the router matches %q, which %s does not declare -- a wildcard was renamed on one side only",
				path, openAPIPath)
		}
	}
}
