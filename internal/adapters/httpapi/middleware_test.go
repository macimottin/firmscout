package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/application"
)

func hashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// RequestID
// ---------------------------------------------------------------------------

func TestRequestIDIsGeneratedAndEchoed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)

	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros")
	id := w.Header().Get(HeaderRequestID)
	if id == "" {
		t.Fatal("no X-Request-Id on the response")
	}

	second := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros")
	if second.Header().Get(HeaderRequestID) == id {
		t.Error("two requests were given the same generated id")
	}
}

func TestSuppliedRequestIDIsPreserved(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)

	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", func(r *http.Request) {
		r.Header.Set(HeaderRequestID, "01JCLIENTSUPPLIED0000")
	})
	if got := w.Header().Get(HeaderRequestID); got != "01JCLIENTSUPPLIED0000" {
		t.Errorf("X-Request-Id = %q, want the client's own id", got)
	}
}

// A request id is echoed in a header, written into logs and stamped on spans. A
// caller-controlled value containing a newline is a header-injection and log-forging
// vector, so an unsafe id is replaced rather than sanitised.
func TestUnsafeRequestIDIsReplacedRatherThanEchoed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)

	for _, bad := range []string{
		"has spaces",
		"has\nnewline",
		"<script>alert(1)</script>",
		strings.Repeat("x", maxRequestIDLength+1),
	} {
		w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", func(r *http.Request) {
			r.Header[HeaderRequestID] = []string{bad}
		})
		got := w.Header().Get(HeaderRequestID)
		if got == bad {
			t.Errorf("unsafe id %q was echoed verbatim", bad)
		}
		if got == "" {
			t.Errorf("no replacement id issued for %q", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Recover
// ---------------------------------------------------------------------------

func TestPanicBecomesAProblemDocumentAndTheProcessSurvives(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.summaries.getErr = nil

	// Replace one route's handler by panicking inside a dependency, which is the
	// realistic shape of the failure.
	h.summaries.bySlug["boom"] = application.ProductSummary{}
	panicking := &panicSummaries{}
	srv, err := NewServer(Deps{
		Summaries:  panicking,
		Vendors:    h.vendors,
		Releases:   h.releases,
		Logger:     discardLogger(),
		RateLimits: RateLimitConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	r := httptest.NewRequest(http.MethodGet, "/api/v1/products/anything", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != ProblemContentType {
		t.Errorf("Content-Type = %q, want %q", ct, ProblemContentType)
	}
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal: %v; body %s", err, w.Body.String())
	}
	if p.Type != TypeInternal {
		t.Errorf("type = %q, want %q", p.Type, TypeInternal)
	}
	if strings.Contains(w.Body.String(), "deliberate test panic") {
		t.Errorf("the panic value leaked to the client: %s", w.Body.String())
	}

	// And the server still serves.
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w2.Code != http.StatusOK {
		t.Errorf("after a panic, /healthz = %d, want 200", w2.Code)
	}
}

type panicSummaries struct{}

func (panicSummaries) Get(context.Context, string) (application.ProductSummary, error) {
	panic("deliberate test panic")
}
func (panicSummaries) Search(context.Context, string, int) ([]application.ProductSummary, error) {
	return nil, nil
}
func (panicSummaries) ListByVendor(context.Context, string, int, string) ([]application.ProductSummary, string, error) {
	return nil, "", nil
}

// ---------------------------------------------------------------------------
// APIKey
// ---------------------------------------------------------------------------

func TestAPIKeyResolution(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)
	h.apiKeys.add(hashKey("secret-key"), "key_1", application.APIConsumer{
		ID: "con_1", Name: "Acme", Plan: "professional", Status: "active", MonthlyQuota: 5000,
	})

	t.Run("bearer token resolves", func(t *testing.T) {
		w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", func(r *http.Request) {
			r.Header.Set(HeaderAuthorization, "Bearer secret-key")
		})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get(HeaderQuotaLimit); got != "5000" {
			t.Errorf("X-Quota-Limit = %q, want 5000", got)
		}
	})

	t.Run("x-api-key header resolves", func(t *testing.T) {
		w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", func(r *http.Request) {
			r.Header.Set(HeaderAPIKey, "secret-key")
		})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})

	t.Run("an unknown key is 401, not a silent downgrade", func(t *testing.T) {
		w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", func(r *http.Request) {
			r.Header.Set(HeaderAuthorization, "Bearer revoked-key")
		})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		var p Problem
		_ = json.Unmarshal(w.Body.Bytes(), &p)
		if p.Type != TypeUnauthenticated {
			t.Errorf("type = %q, want %q", p.Type, TypeUnauthenticated)
		}
	})

	t.Run("a suspended account is 403", func(t *testing.T) {
		h.apiKeys.add(hashKey("suspended"), "key_2", application.APIConsumer{ID: "con_2", Status: "suspended"})
		w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", func(r *http.Request) {
			r.Header.Set(HeaderAuthorization, "Bearer suspended")
		})
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
	})

	t.Run("an unreachable key store is 503, not 401", func(t *testing.T) {
		broken := newHarness(t)
		seedProduct(broken)
		broken.apiKeys.err = errors.New("dial tcp: connection refused")
		w := broken.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", func(r *http.Request) {
			r.Header.Set(HeaderAuthorization, "Bearer anything")
		})
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 -- a credential is not at fault when the store is down", w.Code)
		}
	})
}

// The absolute redaction list forbids an API key or an Authorization header from
// appearing in a log record at any level, in any environment.
func TestTheAPIKeyNeverReachesTheLog(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	h := newHarness(t, func(d *Deps, _ *harness) {
		d.Logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	seedProduct(h)
	h.apiKeys.add(hashKey("super-secret-key-value"), "key_1",
		application.APIConsumer{ID: "con_1", Plan: "professional", Status: "active"})

	if w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", func(r *http.Request) {
		r.Header.Set(HeaderAuthorization, "Bearer super-secret-key-value")
	}); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	logged := buf.String()
	if logged == "" {
		t.Fatal("expected some log output at debug level")
	}
	for _, secret := range []string{"super-secret-key-value", "Bearer ", hashKey("super-secret-key-value")} {
		if strings.Contains(logged, secret) {
			t.Errorf("credential material %q appeared in the log: %s", secret, logged)
		}
	}
	// The route pattern is logged; the resolved path with the slug in it is not a
	// metric label, but it must at least not be the only thing identifying the route.
	if !strings.Contains(logged, "/api/v1/products/{slug}") {
		t.Errorf("the route pattern should be logged: %s", logged)
	}
}

// ---------------------------------------------------------------------------
// RateLimit
// ---------------------------------------------------------------------------

func TestRateLimitReturns429WithRetryAfterOnceTheBurstIsSpent(t *testing.T) {
	t.Parallel()
	const burst = 3
	h := newHarness(t, func(d *Deps, _ *harness) {
		d.RateLimits = RateLimitConfig{
			Anonymous: map[EndpointClass]Policy{
				ClassDetail: {Burst: burst, PerMinute: 60},
			},
		}
	})
	seedProduct(h)

	for i := 0; i < burst; i++ {
		w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros")
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, w.Code)
		}
		if w.Header().Get(HeaderRateLimitLimit) != strconv.Itoa(burst) {
			t.Errorf("X-RateLimit-Limit = %q, want %d", w.Header().Get(HeaderRateLimitLimit), burst)
		}
		wantRemaining := strconv.Itoa(burst - i - 1)
		if got := w.Header().Get(HeaderRateLimitRemaining); got != wantRemaining {
			t.Errorf("request %d: X-RateLimit-Remaining = %q, want %q", i+1, got, wantRemaining)
		}
	}

	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 after the burst is spent", w.Code)
	}
	retry := w.Header().Get(HeaderRetryAfter)
	if retry == "" {
		t.Fatal("no Retry-After on the 429")
	}
	if n, err := strconv.Atoi(retry); err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want a positive integer number of seconds", retry)
	}
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Type != TypeRateLimited {
		t.Errorf("type = %q, want %q", p.Type, TypeRateLimited)
	}

	// The bucket refills: after a minute of virtual time the caller is served again.
	h.clock.advance(time.Minute)
	if w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros"); w.Code != http.StatusOK {
		t.Errorf("after refill: status = %d, want 200", w.Code)
	}
}

// Search is the expensive endpoint and gets its own, tighter budget. A single global
// limit would have to be either too generous for search or too stingy for a product
// fetch the public website makes on every page view.
func TestRateLimitPoliciesArePerEndpointClass(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(d *Deps, _ *harness) {
		d.RateLimits = RateLimitConfig{
			Anonymous: map[EndpointClass]Policy{
				ClassSearch: {Burst: 1, PerMinute: 60},
				ClassDetail: {Burst: 10, PerMinute: 600},
			},
		}
	})
	seedProduct(h)

	if w := h.do(http.MethodGet, "/api/v1/search?q=routeros"); w.Code != http.StatusOK {
		t.Fatalf("first search: status = %d", w.Code)
	}
	if w := h.do(http.MethodGet, "/api/v1/search?q=routeros"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second search: status = %d, want 429", w.Code)
	}
	// The detail bucket is untouched by search having been exhausted.
	if w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros"); w.Code != http.StatusOK {
		t.Errorf("product fetch: status = %d, want 200 -- classes must not share a bucket", w.Code)
	}
}

func TestSystemEndpointsAreNeverRateLimited(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(d *Deps, _ *harness) {
		d.RateLimits = RateLimitConfig{Anonymous: map[EndpointClass]Policy{
			ClassDetail: {Burst: 1, PerMinute: 1},
		}}
	})
	for i := 0; i < 5; i++ {
		if w := h.do(http.MethodGet, "/healthz"); w.Code != http.StatusOK {
			t.Fatalf("probe %d: status = %d -- a probe that 429s during an incident makes it worse", i, w.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// Quota
// ---------------------------------------------------------------------------

func TestQuotaExceededIs429WithItsOwnProblemType(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)
	h.apiKeys.add(hashKey("k"), "key_1", application.APIConsumer{
		ID: "con_1", Plan: "free", Status: "active", MonthlyQuota: 100,
	})
	h.usage.consumed = 100

	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", func(r *http.Request) {
		r.Header.Set(HeaderAuthorization, "Bearer k")
	})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The type is what tells a consumer to buy more quota rather than back off for
	// thirty seconds; both gates return 429, and only the type distinguishes them.
	if p.Type != TypeQuotaExceeded {
		t.Errorf("type = %q, want %q", p.Type, TypeQuotaExceeded)
	}
	if w.Header().Get(HeaderQuotaRemaining) != "0" {
		t.Errorf("X-Quota-Remaining = %q, want 0", w.Header().Get(HeaderQuotaRemaining))
	}
	if w.Header().Get(HeaderRetryAfter) == "" {
		t.Error("a quota 429 still carries Retry-After")
	}
	if !strings.Contains(p.Detail, "billing period") {
		t.Errorf("the detail should say the retry window is a billing period: %q", p.Detail)
	}
}

// A metering outage is FirmScout's problem, not the paying customer's.
func TestQuotaFailsOpenWhenTheMeterIsUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)
	h.apiKeys.add(hashKey("k"), "key_1", application.APIConsumer{
		ID: "con_1", Plan: "free", Status: "active", MonthlyQuota: 100,
	})
	h.usage.err = errors.New("connection refused")

	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", func(r *http.Request) {
		r.Header.Set(HeaderAuthorization, "Bearer k")
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestAnonymousRequestsHaveNoQuotaHeaders(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)

	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros")
	if got := w.Header().Get(HeaderQuotaLimit); got != "" {
		t.Errorf("X-Quota-Limit = %q, want absent -- there is nothing to bill", got)
	}
}

// ---------------------------------------------------------------------------
// Usage
// ---------------------------------------------------------------------------

func TestUsageIsRecordedAfterTheResponse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)
	h.apiKeys.add(hashKey("k"), "key_1", application.APIConsumer{
		ID: "con_1", Plan: "professional", Status: "active",
	})

	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", func(r *http.Request) {
		r.Header.Set(HeaderAuthorization, "Bearer k")
		r.Header.Set(HeaderRequestID, "req_meter1")
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	records := h.usage.recorded()
	if len(records) != 1 {
		t.Fatalf("usage records = %d, want 1", len(records))
	}
	rec := records[0]
	if rec.ConsumerID != "con_1" || rec.APIKeyID != "key_1" {
		t.Errorf("unexpected identity on the record: %+v", rec)
	}
	// The endpoint is the route pattern, not the resolved path: a meter keyed by
	// path would be unusable for aggregation and would carry the slug.
	if rec.Endpoint != "/api/v1/products/{slug}" {
		t.Errorf("endpoint = %q, want the route pattern", rec.Endpoint)
	}
	if rec.StatusCode != http.StatusOK || rec.QuotaWeight != 1 {
		t.Errorf("unexpected record: %+v", rec)
	}
	// Derived from the request id, so a retry collapses onto the same row.
	if !strings.Contains(rec.IdempotencyKey, "req_meter1") {
		t.Errorf("idempotency key = %q, want it derived from the request id", rec.IdempotencyKey)
	}
	if rec.RateLimited {
		t.Error("a served request is not rate limited")
	}
}

func TestAThrottledRequestIsStillMetered(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(d *Deps, _ *harness) {
		d.RateLimits = RateLimitConfig{Keyed: map[EndpointClass]Policy{
			ClassDetail: {Burst: 1, PerMinute: 60},
		}}
	})
	seedProduct(h)
	h.apiKeys.add(hashKey("k"), "key_1", application.APIConsumer{
		ID: "con_1", Plan: "free", Status: "active",
	})

	auth := func(r *http.Request) { r.Header.Set(HeaderAuthorization, "Bearer k") }
	_ = h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", auth)
	w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", auth)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	records := h.usage.recorded()
	if len(records) != 2 {
		t.Fatalf("usage records = %d, want 2 -- a rejected request is still a request the consumer made", len(records))
	}
	if !records[1].RateLimited {
		t.Error("the throttled record should be flagged RateLimited")
	}
}

func TestAnonymousRequestsAreNotMetered(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)

	_ = h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros")
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := len(h.usage.recorded()); got != 0 {
		t.Errorf("usage records = %d, want 0 -- there is no consumer to bill", got)
	}
}

// ---------------------------------------------------------------------------
// CacheHeaders
// ---------------------------------------------------------------------------

func TestETagAndConditionalRequest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)

	first := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros")
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d", first.Code)
	}
	etag := first.Header().Get(HeaderETag)
	if etag == "" {
		t.Fatal("no ETag on a cacheable GET")
	}
	if !strings.HasPrefix(etag, `"`) || strings.HasPrefix(etag, `W/`) {
		t.Errorf("ETag = %q, want a strong validator", etag)
	}
	if cc := first.Header().Get(HeaderCacheControl); cc != cacheCatalogue {
		t.Errorf("Cache-Control = %q, want %q", cc, cacheCatalogue)
	}

	second := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", func(r *http.Request) {
		r.Header.Set(HeaderIfNoneMatch, etag)
	})
	if second.Code != http.StatusNotModified {
		t.Fatalf("conditional request: status = %d, want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Errorf("a 304 must carry no body, got %d bytes", second.Body.Len())
	}
	if second.Header().Get(HeaderETag) != etag {
		t.Errorf("the 304 should repeat the validator")
	}

	// A stale validator gets the full body back.
	third := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", func(r *http.Request) {
		r.Header.Set(HeaderIfNoneMatch, `"stale"`)
	})
	if third.Code != http.StatusOK || third.Body.Len() == 0 {
		t.Errorf("stale validator: status = %d, body %d bytes, want 200 with a body", third.Code, third.Body.Len())
	}
}

func TestProblemResponsesAreNeverCached(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	w := h.do(http.MethodGet, "/api/v1/products/missing")
	if cc := w.Header().Get(HeaderCacheControl); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a problem document", cc)
	}
	if w.Header().Get(HeaderETag) != "" {
		t.Error("a problem document must not carry a validator")
	}
}

func TestEtagMatches(t *testing.T) {
	t.Parallel()
	const etag = `"abc"`
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"*", true},
		{`"abc"`, true},
		{`W/"abc"`, true},
		{`"other", "abc"`, true},
		{`"other"`, false},
	}
	for _, tc := range cases {
		if got := etagMatches(tc.header, etag); got != tc.want {
			t.Errorf("etagMatches(%q, %q) = %v, want %v", tc.header, etag, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// metrics wiring
// ---------------------------------------------------------------------------

// recordingMetrics captures what the middleware records so the labels can be asserted
// on without a Prometheus scrape.
type recordingMetrics struct {
	mu       sync.Mutex
	counters []recordedMeasurement
	hists    []recordedMeasurement
}

type recordedMeasurement struct {
	name  string
	attrs map[string]string
}

func (m *recordingMetrics) Counter(_ context.Context, name string, _ int64, attrs ...application.Attr) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counters = append(m.counters, recordedMeasurement{name, attrMap(attrs)})
}

func (m *recordingMetrics) Histogram(_ context.Context, name string, _ float64, attrs ...application.Attr) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hists = append(m.hists, recordedMeasurement{name, attrMap(attrs)})
}

func (m *recordingMetrics) Gauge(context.Context, string, int64, ...application.Attr) {}

func (m *recordingMetrics) find(name string) []recordedMeasurement {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []recordedMeasurement
	for _, r := range append(append([]recordedMeasurement{}, m.counters...), m.hists...) {
		if r.name == name {
			out = append(out, r)
		}
	}
	return out
}

func attrMap(attrs []application.Attr) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, a := range attrs {
		out[a.Key] = a.Value
	}
	return out
}

// Cardinality is a budget: a slug, a path, a URL or a key must never become a label.
func TestMetricsAreLabelledOnlyWithBoundedDimensions(t *testing.T) {
	t.Parallel()
	rec := &recordingMetrics{}
	h := newHarness(t, func(d *Deps, _ *harness) { d.Metrics = rec })
	seedProduct(h)

	if w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros"); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	requests := rec.find(application.MetricAPIRequestsTotal)
	if len(requests) != 1 {
		t.Fatalf("%s recorded %d times, want 1", application.MetricAPIRequestsTotal, len(requests))
	}
	got := requests[0].attrs
	want := map[string]string{
		labelMethod:     http.MethodGet,
		labelRoute:      "/api/v1/products/{slug}",
		labelStatusCode: "200",
		labelAPITier:    TierAnonymous,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("label %s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("unexpected extra labels: %v", got)
	}
	for _, v := range got {
		if strings.Contains(v, "mikrotik-routeros") {
			t.Errorf("a product slug reached a metric label: %v", got)
		}
	}

	if n := len(rec.find(application.MetricAPIRequestDuration)); n != 1 {
		t.Errorf("%s recorded %d times, want 1", application.MetricAPIRequestDuration, n)
	}
	if n := len(rec.find(application.MetricAPIPayloadSize)); n != 1 {
		t.Errorf("%s recorded %d times, want 1 (response only, no request body)", application.MetricAPIPayloadSize, n)
	}
	if n := len(rec.find(application.MetricAPICacheResults)); n != 1 {
		t.Errorf("%s recorded %d times, want 1", application.MetricAPICacheResults, n)
	}
}

func TestRateLimitAndQuotaEventsAreCounted(t *testing.T) {
	t.Parallel()
	rec := &recordingMetrics{}
	h := newHarness(t, func(d *Deps, _ *harness) {
		d.Metrics = rec
		d.RateLimits = RateLimitConfig{Anonymous: map[EndpointClass]Policy{
			ClassDetail: {Burst: 1, PerMinute: 60},
		}}
	})
	seedProduct(h)

	_ = h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros")
	_ = h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros")

	events := rec.find(application.MetricAPIRateLimitEvents)
	if len(events) != 1 {
		t.Fatalf("%s recorded %d times, want 1", application.MetricAPIRateLimitEvents, len(events))
	}
	if events[0].attrs[labelScope] != scopeIP {
		t.Errorf("scope = %q, want %q for an anonymous caller", events[0].attrs[labelScope], scopeIP)
	}

	quotaHarness := newHarness(t, func(d *Deps, _ *harness) { d.Metrics = rec })
	seedProduct(quotaHarness)
	quotaHarness.apiKeys.add(hashKey("k"), "key_1", application.APIConsumer{
		ID: "con_1", Plan: "free", Status: "active", MonthlyQuota: 10,
	})
	quotaHarness.usage.consumed = 10
	_ = quotaHarness.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", func(r *http.Request) {
		r.Header.Set(HeaderAuthorization, "Bearer k")
	})
	if n := len(rec.find(application.MetricAPIQuotaViolations)); n != 1 {
		t.Errorf("%s recorded %d times, want 1", application.MetricAPIQuotaViolations, n)
	}
}
