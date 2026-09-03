package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

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
