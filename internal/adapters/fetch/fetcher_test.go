package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// fixedNow is the clock every test that cares about time uses.
var fixedNow = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

// newTestFetcher builds a fetcher whose guard exempts exactly the test server's
// address. Every other denied range stays enforced, so a test cannot accidentally pass
// because the policy was switched off.
func newTestFetcher(t *testing.T, srv *httptest.Server, opts ...Option) *Fetcher {
	t.Helper()
	g := NewGuard(WithExemptDestinations(srv.Listener.Addr().String()))
	opts = append([]Option{WithClock(func() time.Time { return fixedNow })}, opts...)
	return New(g, opts...)
}

func fetchRequest(url string) application.FetchRequest {
	return application.FetchRequest{URL: url, Timeout: 5 * time.Second}
}

func TestFetchConditionalGet(t *testing.T) {
	t.Parallel()

	const etag = `W/"routeros-7.24.2"`
	const lastMod = "Wed, 03 Sep 2026 09:00:00 GMT"

	tests := []struct {
		name         string
		req          application.FetchRequest
		handler      http.HandlerFunc
		wantOutcome  domain.CheckOutcome
		wantSignal   domain.ChangeSignal
		wantBody     string
		wantEmptyBdy bool
	}{
		{
			name: "304 with etag is unchanged and reads no body",
			req:  application.FetchRequest{ETag: etag},
			handler: func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("If-None-Match"); got != etag {
					t.Errorf("If-None-Match = %q, want %q", got, etag)
				}
				w.WriteHeader(http.StatusNotModified)
			},
			wantOutcome:  domain.OutcomeUnchanged,
			wantSignal:   domain.SignalETag,
			wantEmptyBdy: true,
		},
		{
			name: "304 with only last-modified reports last_modified",
			req:  application.FetchRequest{LastModified: lastMod},
			handler: func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("If-Modified-Since"); got != lastMod {
					t.Errorf("If-Modified-Since = %q, want %q", got, lastMod)
				}
				if r.Header.Get("If-None-Match") != "" {
					t.Error("If-None-Match sent without a stored ETag")
				}
				w.WriteHeader(http.StatusNotModified)
			},
			wantOutcome:  domain.OutcomeUnchanged,
			wantSignal:   domain.SignalLastModified,
			wantEmptyBdy: true,
		},
		{
			name: "200 after a conditional request is changed",
			req:  application.FetchRequest{ETag: etag},
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `W/"routeros-7.25.1"`)
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				_, _ = w.Write([]byte("7.25.1"))
			},
			wantOutcome: domain.OutcomeChanged,
			wantSignal:  domain.SignalConditionalGet,
			wantBody:    "7.25.1",
		},
		{
			name: "200 with no validators available falls back to full comparison",
			req:  application.FetchRequest{},
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("7.25.1"))
			},
			wantOutcome: domain.OutcomeChanged,
			wantSignal:  domain.SignalFullCompare,
			wantBody:    "7.25.1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			f := newTestFetcher(t, srv)
			req := tc.req
			req.URL = srv.URL
			req.Timeout = 5 * time.Second

			res, err := f.Fetch(context.Background(), req)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if res.Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %q, want %q (err=%v)", res.Outcome, tc.wantOutcome, res.Err)
			}
			if res.ChangeSignal != tc.wantSignal {
				t.Errorf("signal = %q, want %q", res.ChangeSignal, tc.wantSignal)
			}
			if tc.wantEmptyBdy && len(res.Body) != 0 {
				t.Errorf("body = %q, want empty", res.Body)
			}
			if tc.wantBody != "" && string(res.Body) != tc.wantBody {
				t.Errorf("body = %q, want %q", res.Body, tc.wantBody)
			}
		})
	}
}

func TestFetchUnchangedPreservesStoredValidators(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv)
	res, err := f.Fetch(context.Background(), application.FetchRequest{
		URL: srv.URL, ETag: `"abc"`, LastModified: "Wed, 03 Sep 2026 09:00:00 GMT", Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ETag != `"abc"` {
		t.Errorf("ETag = %q; a 304 must not blank the stored validator", res.ETag)
	}
	if res.LastModified == "" {
		t.Error("LastModified was blanked by a 304")
	}
}

func TestFetchStatusMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		want    domain.CheckOutcome
		headers map[string]string
	}{
		{name: "401", status: http.StatusUnauthorized, want: domain.OutcomeUnauthorized},
		{name: "403", status: http.StatusForbidden, want: domain.OutcomeUnauthorized},
		{name: "404", status: http.StatusNotFound, want: domain.OutcomeUnavailable},
		{name: "410", status: http.StatusGone, want: domain.OutcomeUnavailable},
		{name: "429", status: http.StatusTooManyRequests, want: domain.OutcomeRateLimited},
		{name: "500", status: http.StatusInternalServerError, want: domain.OutcomeUnavailable},
		{name: "502", status: http.StatusBadGateway, want: domain.OutcomeUnavailable},
		{name: "503", status: http.StatusServiceUnavailable, want: domain.OutcomeUnavailable},
		{name: "418", status: http.StatusTeapot, want: domain.OutcomeUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			f := newTestFetcher(t, srv)
			res, err := f.Fetch(context.Background(), fetchRequest(srv.URL))
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != tc.want {
				t.Fatalf("status %d gave outcome %q, want %q", tc.status, res.Outcome, tc.want)
			}
			if res.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", res.StatusCode, tc.status)
			}
		})
	}
}

func TestFetchRetryAfter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{name: "delay seconds", header: "120", want: 2 * time.Minute},
		{name: "delay seconds zero", header: "0", want: 0},
		{name: "http date in the future", header: fixedNow.Add(90 * time.Second).UTC().Format(http.TimeFormat), want: 90 * time.Second},
		{name: "http date in the past", header: fixedNow.Add(-time.Hour).UTC().Format(http.TimeFormat), want: 0},
		{name: "absent", header: "", want: 0},
		{name: "unparseable", header: "soon", want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer srv.Close()

			f := newTestFetcher(t, srv)
			res, err := f.Fetch(context.Background(), fetchRequest(srv.URL))
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != domain.OutcomeRateLimited {
				t.Fatalf("outcome = %q, want rate_limited", res.Outcome)
			}
			if res.RetryAfter != tc.want {
				t.Fatalf("RetryAfter = %v, want %v", res.RetryAfter, tc.want)
			}
		})
	}
}

func TestFetchSizeLimit(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("A"), 4096))
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv)
	res, err := f.Fetch(context.Background(), application.FetchRequest{URL: srv.URL, MaxBytes: 512, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != domain.OutcomeSuspiciousContent {
		t.Fatalf("outcome = %q, want suspicious_content (err=%v)", res.Outcome, res.Err)
	}
	if len(res.Body) != 0 {
		t.Fatalf("an over-limit body must not be delivered truncated; got %d bytes", len(res.Body))
	}
}

func TestFetchSizeLimitExactBoundary(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("A"), 512))
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv)
	res, err := f.Fetch(context.Background(), application.FetchRequest{URL: srv.URL, MaxBytes: 512, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != domain.OutcomeChanged {
		t.Fatalf("a body exactly at the limit was rejected: %q (%v)", res.Outcome, res.Err)
	}
	if res.BytesRead != 512 {
		t.Errorf("BytesRead = %d, want 512", res.BytesRead)
	}
}

func TestFetchDecompressionIsCapped(t *testing.T) {
	t.Parallel()
	var payload bytes.Buffer
	zw := gzip.NewWriter(&payload)
	if _, err := zw.Write(bytes.Repeat([]byte("A"), 1<<20)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	compressed := payload.Bytes()
	if len(compressed) > 64<<10 {
		t.Fatalf("test fixture is not compressible enough: %d bytes", len(compressed))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Error("fetcher did not negotiate gzip explicitly")
		}
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressed)
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv)
	res, err := f.Fetch(context.Background(), application.FetchRequest{URL: srv.URL, MaxBytes: 4096, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != domain.OutcomeSuspiciousContent {
		t.Fatalf("outcome = %q, want suspicious_content: a 1 MiB body decompressed under a 4 KiB cap", res.Outcome)
	}
}

func TestFetchDecompressesWithinTheCap(t *testing.T) {
	t.Parallel()
	var payload bytes.Buffer
	zw := gzip.NewWriter(&payload)
	_, _ = zw.Write([]byte("RouterOS 7.25.1"))
	_ = zw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(payload.Bytes())
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv)
	res, err := f.Fetch(context.Background(), fetchRequest(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != domain.OutcomeChanged {
		t.Fatalf("outcome = %q (%v)", res.Outcome, res.Err)
	}
	if string(res.Body) != "RouterOS 7.25.1" {
		t.Fatalf("body = %q", res.Body)
	}
}

func TestFetchRejectsNestedContentEncoding(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip, gzip")
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv)
	res, err := f.Fetch(context.Background(), fetchRequest(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != domain.OutcomeSuspiciousContent {
		t.Fatalf("outcome = %q, want suspicious_content", res.Outcome)
	}
}

func TestFetchContentTypeValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		expected string
		served   string
		want     domain.CheckOutcome
	}{
		{name: "match", expected: "application/json", served: "application/json", want: domain.OutcomeChanged},
		{name: "match ignoring parameters", expected: "application/json", served: "application/json; charset=utf-8", want: domain.OutcomeChanged},
		{name: "expected carries parameters", expected: "text/html; charset=utf-8", served: "text/html", want: domain.OutcomeChanged},
		{name: "mismatch", expected: "application/json", served: "text/html; charset=utf-8", want: domain.OutcomeParserFailed},
		{name: "login page instead of json", expected: "application/json", served: "text/html", want: domain.OutcomeParserFailed},
		{name: "unset expectation accepts anything", expected: "", served: "application/octet-stream", want: domain.OutcomeChanged},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.served)
				_, _ = w.Write([]byte(`{"version":"7.25.1"}`))
			}))
			defer srv.Close()

			f := newTestFetcher(t, srv)
			res, err := f.Fetch(context.Background(), application.FetchRequest{
				URL: srv.URL, ExpectedContentType: tc.expected, Timeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q (err=%v)", res.Outcome, tc.want, res.Err)
			}
		})
	}
}

func TestFetchOffHostRedirectIsFlaggedAndNotFollowed(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://downloads.example.com/routeros/", http.StatusMovedPermanently)
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv)
	res, err := f.Fetch(context.Background(), fetchRequest(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != domain.OutcomeRedirected {
		t.Fatalf("outcome = %q, want redirected (err=%v)", res.Outcome, res.Err)
	}
	if !res.OffRegisteredHost {
		t.Error("OffRegisteredHost = false; the chain left the registered host")
	}
	if !strings.Contains(res.RedirectLocation, "downloads.example.com") {
		t.Errorf("RedirectLocation = %q", res.RedirectLocation)
	}
	if len(res.Body) != 0 {
		t.Error("the off-host body was fetched; an unreviewed host must not become a source")
	}
}

func TestFetchSameHostRedirectIsFollowed(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/new", http.StatusFound)
	})
	mux.HandleFunc("/new", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("7.25.1"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := newTestFetcher(t, srv)
	res, err := f.Fetch(context.Background(), fetchRequest(srv.URL+"/old"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != domain.OutcomeChanged {
		t.Fatalf("outcome = %q, want changed (err=%v)", res.Outcome, res.Err)
	}
	if res.OffRegisteredHost {
		t.Error("a same-host redirect was reported as off-host")
	}
	if string(res.Body) != "7.25.1" {
		t.Errorf("body = %q", res.Body)
	}
}

func TestFetchRejectsBlockedURLWithoutARequest(t *testing.T) {
	t.Parallel()
	f := New(NewGuard())
	res, err := f.Fetch(context.Background(), fetchRequest("http://169.254.169.254/latest/meta-data/iam/security-credentials/"))
	if err == nil {
		t.Fatal("Fetch accepted the metadata endpoint")
	}
	if res.Outcome != domain.OutcomeSuspiciousContent {
		t.Fatalf("outcome = %q, want suspicious_content", res.Outcome)
	}
	if !errors.Is(res.Err, ErrBlockedAddress) {
		t.Fatalf("res.Err = %v, want ErrBlockedAddress", res.Err)
	}
}

func TestFetchSendsDescriptiveUserAgent(t *testing.T) {
	t.Parallel()
	var got string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Get("User-Agent")
		mu.Unlock()
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv)
	if _, err := f.Fetch(context.Background(), fetchRequest(srv.URL)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	ua := got
	mu.Unlock()
	if !strings.HasPrefix(ua, "FirmScout/") {
		t.Fatalf("User-Agent = %q, want a FirmScout identifier", ua)
	}
	if !strings.Contains(ua, "http") {
		t.Fatalf("User-Agent = %q, want a contact URL", ua)
	}

	res, err := f.Fetch(context.Background(), application.FetchRequest{URL: srv.URL, UserAgent: "FirmScoutTest/1.0 (+https://example.invalid)", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != domain.OutcomeChanged {
		t.Fatalf("outcome = %q", res.Outcome)
	}
	mu.Lock()
	ua = got
	mu.Unlock()
	if ua != "FirmScoutTest/1.0 (+https://example.invalid)" {
		t.Fatalf("request user agent was not honoured: %q", ua)
	}
}

func TestFetchPerHostConcurrencyIsCapped(t *testing.T) {
	t.Parallel()
	var inFlight, peak int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&inFlight, 1)
		for {
			p := atomic.LoadInt64(&peak)
			if n <= p || atomic.CompareAndSwapInt64(&peak, p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv, WithPerHostConcurrency(2))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.Fetch(context.Background(), fetchRequest(srv.URL)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if p := atomic.LoadInt64(&peak); p > 2 {
		t.Fatalf("peak concurrency to one host was %d, want at most 2", p)
	}
}

func TestFetchTimeoutIsEnforced(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	f := newTestFetcher(t, srv)
	start := time.Now()
	res, err := f.Fetch(context.Background(), application.FetchRequest{URL: srv.URL, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != domain.OutcomeUnavailable {
		t.Fatalf("outcome = %q, want unavailable", res.Outcome)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout took %v to fire", elapsed)
	}
}

func TestParseRetryAfterUnits(t *testing.T) {
	t.Parallel()
	now := fixedNow
	tests := []struct {
		in   string
		want time.Duration
	}{
		{in: "", want: 0},
		{in: "30", want: 30 * time.Second},
		{in: " 30 ", want: 30 * time.Second},
		{in: "-5", want: 0},
		{in: strconv.Itoa(3600), want: time.Hour},
		{in: now.Add(45 * time.Second).UTC().Format(http.TimeFormat), want: 45 * time.Second},
		{in: "Wed, 03 Sep 2026 11:00:00 GMT", want: 0},
		{in: "tomorrow", want: 0},
	}
	for _, tc := range tests {
		if got := parseRetryAfter(tc.in, now); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
