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
	"github.com/macimottin/firmscout/internal/domain"
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
		// invalid-api-key, not unauthorized: the caller sent a credential and it did
		// not resolve, which is "replace this key", not "send a key".
		if p.Type != TypeInvalidAPIKey {
			t.Errorf("type = %q, want %q", p.Type, TypeInvalidAPIKey)
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

// ---------------------------------------------------------------------------
// the problem catalogue
// ---------------------------------------------------------------------------

// The revert detector for ADR-0022.
//
// The URIs are written out as literals rather than composed from ProblemBase, because a
// test that built them the same way the code does would agree with the code no matter
// what the code said. These strings are the contract as docs/architecture/api.md §6
// publishes it, and if one of them has to change here, that is the version bump.
func TestProblemTypesAreCanonical(t *testing.T) {
	t.Parallel()

	cases := []struct {
		constant string
		title    string
		status   int
		build    func(*http.Request, string) Problem
	}{
		{"https://firmscout.dev/problems/not-found", "Resource not found", http.StatusNotFound, NotFound},
		{"https://firmscout.dev/problems/invalid-parameter", "Invalid parameter", http.StatusBadRequest, InvalidParameter},
		{"https://firmscout.dev/problems/validation-failed", "Request body validation failed", http.StatusBadRequest, ValidationFailed},
		{"https://firmscout.dev/problems/unauthorized", "Authentication required", http.StatusUnauthorized, Unauthorized},
		{"https://firmscout.dev/problems/invalid-api-key", "API key invalid or revoked", http.StatusUnauthorized, InvalidAPIKey},
		{"https://firmscout.dev/problems/forbidden", "Not available on this plan", http.StatusForbidden, Forbidden},
		{"https://firmscout.dev/problems/conflict", "Conflicting state", http.StatusConflict, Conflict},
		{"https://firmscout.dev/problems/rate-limited", "Too many requests", http.StatusTooManyRequests, RateLimited},
		{"https://firmscout.dev/problems/quota-exceeded", "Monthly quota exceeded", http.StatusTooManyRequests, QuotaExceeded},
		{"https://firmscout.dev/problems/service-unavailable", "Service temporarily unavailable", http.StatusServiceUnavailable, nil},
		{"https://firmscout.dev/problems/internal-error", "Internal server error", http.StatusInternalServerError, nil},
	}

	// Every Type* constant, so a new one added without a row here is caught.
	declared := map[string]bool{
		TypeNotFound: true, TypeInvalidParameter: true, TypeValidationFailed: true,
		TypeUnauthorized: true, TypeInvalidAPIKey: true, TypeForbidden: true,
		TypeConflict: true, TypeRateLimited: true, TypeQuotaExceeded: true,
		TypeInternal: true, TypeServiceUnavailable: true,
	}
	if len(declared) != len(cases) {
		t.Fatalf("the catalogue declares %d types and this table has %d rows", len(declared), len(cases))
	}

	r := httptest.NewRequest(http.MethodGet, "/api/v1/vendors", nil)
	for _, tc := range cases {
		if !declared[tc.constant] {
			t.Errorf("%s is documented in api.md §6 but no Type* constant holds it", tc.constant)
			continue
		}
		if tc.build == nil {
			// Internal and ServiceUnavailable take a cause, so they are checked below.
			continue
		}
		p := tc.build(r, "detail")
		if p.Type != tc.constant {
			t.Errorf("constructor produced type %q, want %q", p.Type, tc.constant)
		}
		if p.Title != tc.title {
			t.Errorf("%s: title = %q, want %q", tc.constant, p.Title, tc.title)
		}
		if p.Status != tc.status {
			t.Errorf("%s: status = %d, want %d", tc.constant, p.Status, tc.status)
		}
	}

	if p := Internal(r, errors.New("boom")); p.Type != "https://firmscout.dev/problems/internal-error" ||
		p.Status != http.StatusInternalServerError {
		t.Errorf("Internal = %q/%d", p.Type, p.Status)
	}
	if p := ServiceUnavailable(r, "detail", errors.New("boom")); p.Type != "https://firmscout.dev/problems/service-unavailable" ||
		p.Status != http.StatusServiceUnavailable {
		t.Errorf("ServiceUnavailable = %q/%d", p.Type, p.Status)
	}

	// The three renamed URIs must not survive anywhere in the catalogue. A constant
	// still holding one would mean the rename was reverted for that type only, which
	// is the shape a partial revert actually takes.
	for _, stale := range []string{
		"https://firmscout.dev/problems/invalid-request",
		"https://firmscout.dev/problems/unauthenticated",
		"https://firmscout.dev/problems/internal",
	} {
		if declared[stale] {
			t.Errorf("the pre-Phase-2 URI %q is still declared; see ADR-0022", stale)
		}
	}
}

// Both 401s are 401, and they are not the same problem. "Send a credential" and
// "replace the one you sent" are different instructions, and a consumer that cannot
// tell them apart retries a dead key forever.
func TestInvalidAPIKeyIsDistinctFromUnauthorized(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)
	h.apiKeys.add(hashKey("live-key"), "key_1",
		application.APIConsumer{ID: "con_1", Plan: "professional", Status: "active"})

	cases := []struct {
		name    string
		header  string
		value   string
		want    string
		harness func(*testing.T) *harness
	}{
		{
			name: "a presented key that does not resolve", header: HeaderAuthorization,
			value: "Bearer revoked-key", want: TypeInvalidAPIKey,
		},
		{
			name: "an X-API-Key that does not resolve", header: HeaderAPIKey,
			value: "revoked-key", want: TypeInvalidAPIKey,
		},
		{
			name: "an Authorization header that is not a bearer token", header: HeaderAuthorization,
			value: "Basic dXNlcjpwYXNz", want: TypeUnauthorized,
		},
		{
			name: "a bearer scheme with no token", header: HeaderAuthorization,
			value: "Bearer   ", want: TypeUnauthorized,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", func(r *http.Request) {
				r.Header.Set(tc.header, tc.value)
			})
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body %s", w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); ct != ProblemContentType {
				t.Errorf("Content-Type = %q, want %q", ct, ProblemContentType)
			}
			var p Problem
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if p.Type != tc.want {
				t.Errorf("type = %q, want %q", p.Type, tc.want)
			}
		})
	}

	// A deployment with no key store cannot check a credential at all. That is
	// unauthorized, not invalid-api-key: the key may be perfectly good, and telling the
	// caller to replace it would send them to fix something that is not broken.
	bare := newHarness(t, func(d *Deps, _ *harness) { d.APIKeys = nil })
	seedProduct(bare)
	w := bare.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", func(r *http.Request) {
		r.Header.Set(HeaderAuthorization, "Bearer anything")
	})
	var p Problem
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if w.Code != http.StatusUnauthorized || p.Type != TypeUnauthorized {
		t.Errorf("no key store: status = %d type = %q, want 401 %q", w.Code, p.Type, TypeUnauthorized)
	}
}

// ---------------------------------------------------------------------------
// Vary
// ---------------------------------------------------------------------------

// The revert detector for the shared-cache rule.
//
// A cacheable response whose body depends on the caller's plan and carries no Vary is a
// response a shared cache may hand to a caller of a different tier. The 304 is asserted
// as well as the 200: a cache that stored the 200 without the header and then
// revalidated would still be free to reuse the entry for anyone.
func TestCacheableResponsesVaryOnCredentials(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)
	h.vendors.put(domain.Vendor{ID: "ven_mikrotik", Slug: "mikrotik", Name: "MikroTik"})
	h.releases.setLatest("prd_routeros", "", domain.Release{
		ID: "rel_01J8Z3K9QWERTYUIOPASDFGH", Version: mustVersion(t, "7.24.2"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS, Channel: "stable",
	})
	h.releases.put("prd_routeros", domain.Release{
		ID: "rel_01J8Z3K9QWERTYUIOPASDFGH", Version: mustVersion(t, "7.24.2"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS, Channel: "stable",
		FirstObservedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	})

	// Every cacheable route, which is every route that declares a Cache-Control policy.
	cacheable := map[string]string{
		"/api/v1/search?q=routeros":                     cacheSearch,
		"/api/v1/vendors":                               cacheCatalogue,
		"/api/v1/vendors/mikrotik":                      cacheCatalogue,
		"/api/v1/products/mikrotik-routeros":            cacheCatalogue,
		"/api/v1/products/mikrotik-routeros/releases":   cacheReleases,
		"/api/v1/products/mikrotik-routeros/latest":     cacheReleases,
		"/api/v1/releases/rel_01J8Z3K9QWERTYUIOPASDFGH": cacheImmutable,
	}
	for target, wantCache := range cacheable {
		first := h.do(http.MethodGet, target)
		if first.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body %s", target, first.Code, first.Body.String())
		}
		if cc := first.Header().Get(HeaderCacheControl); cc != wantCache {
			t.Errorf("%s: Cache-Control = %q, want %q", target, cc, wantCache)
		}
		if got := first.Header().Get(HeaderVary); got != varyCredentials {
			t.Errorf("%s: 200 Vary = %q, want %q -- a shared cache could serve one tier's body to another",
				target, got, varyCredentials)
		}
		// Both carriers, not just one: a Vary naming only Authorization leaves every
		// X-API-Key response interchangeable.
		for _, carrier := range []string{HeaderAuthorization, HeaderAPIKey} {
			if !strings.Contains(first.Header().Get(HeaderVary), carrier) {
				t.Errorf("%s: Vary does not name %s", target, carrier)
			}
		}

		etag := first.Header().Get(HeaderETag)
		if etag == "" {
			continue
		}
		second := h.do(http.MethodGet, target, func(r *http.Request) {
			r.Header.Set(HeaderIfNoneMatch, etag)
		})
		if second.Code != http.StatusNotModified {
			t.Fatalf("%s: conditional status = %d, want 304", target, second.Code)
		}
		if got := second.Header().Get(HeaderVary); got != varyCredentials {
			t.Errorf("%s: 304 Vary = %q, want %q", target, got, varyCredentials)
		}
	}
}

// Vary would needlessly fragment a cache it cannot protect, so it goes nowhere a
// response is not cacheable in the first place: not on a problem document, and not on
// the infrastructure endpoints, which are no-store and identical for everyone.
func TestVaryIsAbsentWhereNothingIsCached(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	for _, target := range []string{"/healthz", "/readyz", "/metrics"} {
		w := h.do(http.MethodGet, target)
		if got := w.Header().Get(HeaderVary); got != "" {
			t.Errorf("%s: Vary = %q on an uncacheable infrastructure endpoint", target, got)
		}
	}

	// A problem document already carries no-store; a validator or a Vary on one would
	// be describing a cache entry that must never exist.
	w := h.do(http.MethodGet, "/api/v1/products/does-not-exist")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if got := w.Header().Get(HeaderVary); got != "" {
		t.Errorf("Vary = %q on a problem document", got)
	}
	if w.Header().Get(HeaderETag) != "" {
		t.Error("a problem document must not carry a validator")
	}
}

// ---------------------------------------------------------------------------
// cache partitioning by credential
// ---------------------------------------------------------------------------

// The revert detector for T-13, and it starts by proving the premise rather than
// assuming it: one URL, two callers, two different bodies.
//
// Before this fix both of those responses carried "public, max-age=300" and relied on
// Vary to keep them apart. CloudFront -- which this architecture names -- honours Vary
// only for Accept-Encoding, so a URL-keyed shared cache would have served the paid
// archive to an anonymous caller or the empty anonymous page to a paying customer, and
// the entry would also have carried one consumer's X-RateLimit-Remaining and
// X-Quota-Remaining. security.md §4.10 says the mitigation in as many words: no response
// to an authenticated request is cached at the CDN at all.
func TestAuthenticatedResponsesAreNotStorable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)
	seedTwoYearsOfReleases(t, h)
	h.apiKeys.add(hashKey("paid"), "key_paid",
		application.APIConsumer{ID: "con_paid", Plan: application.PlanProfessional, Status: "active"})

	const target = "/api/v1/products/mikrotik-routeros/releases"
	anon := h.do(http.MethodGet, target)
	paid := h.do(http.MethodGet, target, func(r *http.Request) {
		r.Header.Set(HeaderAuthorization, "Bearer paid")
	})
	if anon.Code != http.StatusOK || paid.Code != http.StatusOK {
		t.Fatalf("status: anonymous = %d, keyed = %d", anon.Code, paid.Code)
	}
	if anon.Body.String() == paid.Body.String() {
		t.Fatal("the two callers received the same body, so this test is not exercising a tier-dependent response")
	}

	// The anonymous response is still public: the CDN has to earn its keep, and the
	// traffic that decides hit rate is the public website, which sends no credential.
	if cc := anon.Header().Get(HeaderCacheControl); cc != cacheReleases {
		t.Errorf("anonymous Cache-Control = %q, want %q", cc, cacheReleases)
	}
	// The keyed one is storable by nothing.
	cc := paid.Header().Get(HeaderCacheControl)
	if cc != cacheAuthenticated {
		t.Errorf("authenticated Cache-Control = %q, want %q", cc, cacheAuthenticated)
	}
	if !strings.Contains(cc, "no-store") || strings.Contains(cc, "public") {
		t.Errorf("authenticated Cache-Control = %q; a shared cache may store it", cc)
	}
}

// The rule is the caller's, not the route's, on every cacheable endpoint including the
// immutable one. A per-endpoint exception has to be got right again on every endpoint
// added later, and it fails silently when it is wrong -- and even an immutable body
// ships with that caller's own rate-limit and quota counters.
func TestNoCacheableRouteIsPublicForAKeyedCaller(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedProduct(h)
	h.vendors.put(domain.Vendor{ID: "ven_mikrotik", Slug: "mikrotik", Name: "MikroTik"})
	h.releases.setLatest("prd_routeros", "", domain.Release{
		ID: "rel_01J8Z3K9QWERTYUIOPASDFGH", Version: mustVersion(t, "7.24.2"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS, Channel: "stable",
	})
	h.releases.put("prd_routeros", domain.Release{
		ID: "rel_01J8Z3K9QWERTYUIOPASDFGH", Version: mustVersion(t, "7.24.2"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS, Channel: "stable",
		FirstObservedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	})
	h.apiKeys.add(hashKey("k"), "key_1", consumerFixture())

	for _, target := range []string{
		"/api/v1/search?q=routeros",
		"/api/v1/vendors",
		"/api/v1/vendors/mikrotik",
		"/api/v1/products/mikrotik-routeros",
		"/api/v1/products/mikrotik-routeros/releases",
		"/api/v1/products/mikrotik-routeros/latest",
		"/api/v1/releases/rel_01J8Z3K9QWERTYUIOPASDFGH",
	} {
		w := h.do(http.MethodGet, target, func(r *http.Request) {
			r.Header.Set(HeaderAuthorization, "Bearer k")
		})
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d; body %s", target, w.Code, w.Body.String())
		}
		if cc := w.Header().Get(HeaderCacheControl); cc != cacheAuthenticated {
			t.Errorf("%s: Cache-Control = %q for a keyed caller, want %q", target, cc, cacheAuthenticated)
		}
		// Vary stays as the second line of defence, for a layer that ignores no-store.
		if got := w.Header().Get(HeaderVary); got != varyCredentials {
			t.Errorf("%s: Vary = %q, want %q", target, got, varyCredentials)
		}
		// The validator is still issued: the caller may hold their own copy and ask
		// whether it is current. What must not happen is a *shared* copy.
		if w.Header().Get(HeaderETag) == "" {
			t.Errorf("%s: a keyed caller lost its validator", target)
		}
	}

	// The same key on the same URL still gets a 304 from its own validator, and that
	// 304 repeats the policy rather than reverting to the route's.
	first := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", func(r *http.Request) {
		r.Header.Set(HeaderAuthorization, "Bearer k")
	})
	second := h.do(http.MethodGet, "/api/v1/products/mikrotik-routeros", func(r *http.Request) {
		r.Header.Set(HeaderAuthorization, "Bearer k")
		r.Header.Set(HeaderIfNoneMatch, first.Header().Get(HeaderETag))
	})
	if second.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", second.Code)
	}
	if cc := second.Header().Get(HeaderCacheControl); cc != cacheAuthenticated {
		t.Errorf("304 Cache-Control = %q, want %q", cc, cacheAuthenticated)
	}
}
