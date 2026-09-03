package fetch

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// Fetcher defaults.
const (
	// DefaultUserAgent identifies FirmScout honestly and points at a contact. A crawler
	// that will not say who it is has no business asking a vendor for politeness.
	DefaultUserAgent = "FirmScout/0.1 (+https://github.com/macimottin/firmscout; firmware release monitoring; contact: github.com/macimottin/firmscout/issues)"
	// DefaultMaxBytes is the platform ceiling on a response body.
	DefaultMaxBytes = 16 << 20 // 16 MiB
	// DefaultTimeout is the overall per-request ceiling.
	DefaultTimeout = 30 * time.Second
	// DefaultPerHostConcurrency caps simultaneous in-flight requests to one host, so a
	// vendor with dozens of registered sources never receives a thundering herd.
	DefaultPerHostConcurrency = 2
	// DefaultMaxDecompressionRatio bounds decompressed:compressed. A body that expands
	// more than this beyond the ratio floor is a bomb, not a changelog.
	DefaultMaxDecompressionRatio = 100
	// decompressionRatioFloor is the decompressed size below which the ratio check is
	// not applied, so that a small, highly compressible page is not flagged.
	decompressionRatioFloor = 1 << 20 // 1 MiB
)

// contextKey is this package's private context key type.
type contextKey int

const redirectStateKey contextKey = iota

// redirectState records what a redirect chain did. It is carried in the request context
// because http.Client.CheckRedirect has no other channel back to the caller.
//
// CheckRedirect is invoked synchronously on the goroutine that called Do, so this needs
// no lock; it is never shared across requests.
type redirectState struct {
	originalHost string
	// allowOffHost permits the chain to leave the original host. It is set only for
	// robots.txt discovery, where following a redirect to a CDN is normal and where no
	// content is adopted as a source.
	allowOffHost bool
	offHost      bool
	location     string
	hops         int
}

func withRedirectState(ctx context.Context, st *redirectState) context.Context {
	return context.WithValue(ctx, redirectStateKey, st)
}

func redirectStateFrom(ctx context.Context) *redirectState {
	st, _ := ctx.Value(redirectStateKey).(*redirectState)
	return st
}

// Fetcher is the application.Fetcher adapter: a guarded, polite, conditional HTTP
// client.
type Fetcher struct {
	guard        *Guard
	client       *http.Client
	robots       *RobotsCache
	userAgent    string
	maxBytes     int64
	timeout      time.Duration
	maxRedirects int

	perHostConcurrency int
	minHostInterval    time.Duration
	maxRatio           int64
	blockOnUnknown     bool

	now   func() time.Time
	after func(d time.Duration) <-chan time.Time

	mu       sync.Mutex
	hostSem  map[string]chan struct{}
	hostNext map[string]time.Time
}

var _ application.Fetcher = (*Fetcher)(nil)

// Option configures a Fetcher.
type Option func(*Fetcher)

// WithUserAgent overrides the default User-Agent.
func WithUserAgent(ua string) Option { return func(f *Fetcher) { f.userAgent = ua } }

// WithMaxBytes sets the platform ceiling on a response body.
func WithMaxBytes(n int64) Option { return func(f *Fetcher) { f.maxBytes = n } }

// WithTimeout sets the default per-request timeout.
func WithTimeout(d time.Duration) Option { return func(f *Fetcher) { f.timeout = d } }

// WithPerHostConcurrency caps simultaneous requests to a single host.
func WithPerHostConcurrency(n int) Option {
	return func(f *Fetcher) {
		if n > 0 {
			f.perHostConcurrency = n
		}
	}
}

// WithMinHostInterval enforces a minimum gap between requests to the same host. Zero,
// the default, disables it; a source whose vendor documents a rate limit sets it so the
// limit is honoured client-side rather than discovered through 429s.
func WithMinHostInterval(d time.Duration) Option {
	return func(f *Fetcher) { f.minHostInterval = d }
}

// WithMaxDecompressionRatio bounds the decompressed:compressed ratio.
func WithMaxDecompressionRatio(n int64) Option {
	return func(f *Fetcher) { f.maxRatio = n }
}

// WithRobotsCache injects a robots policy cache, normally so a test can control its
// clock or its HTTP client.
func WithRobotsCache(rc *RobotsCache) Option { return func(f *Fetcher) { f.robots = rc } }

// WithClock injects a clock. Retry-After HTTP-date parsing and host pacing use it.
func WithClock(now func() time.Time) Option { return func(f *Fetcher) { f.now = now } }

// AllowOnUnknownRobots makes an unreadable robots.txt permissive instead of blocking.
//
// The default is to block, which is the conservative reading of RFC 9309 §2.3.1.4: if
// the rules cannot be read, permission has not been given. An operator who would rather
// keep collecting through a vendor's robots.txt outage sets this deliberately.
func AllowOnUnknownRobots() Option { return func(f *Fetcher) { f.blockOnUnknown = false } }

// New builds a Fetcher around a Guard. Passing a nil guard installs the default policy.
func New(guard *Guard, opts ...Option) *Fetcher {
	if guard == nil {
		guard = NewGuard()
	}
	f := &Fetcher{
		guard:              guard,
		userAgent:          DefaultUserAgent,
		maxBytes:           DefaultMaxBytes,
		timeout:            DefaultTimeout,
		maxRedirects:       guard.MaxRedirects(),
		perHostConcurrency: DefaultPerHostConcurrency,
		maxRatio:           DefaultMaxDecompressionRatio,
		blockOnUnknown:     true,
		now:                time.Now,
		after:              time.After,
		hostSem:            make(map[string]chan struct{}),
		hostNext:           make(map[string]time.Time),
	}
	for _, o := range opts {
		o(f)
	}
	f.client = &http.Client{
		Transport:     guard.Transport(),
		CheckRedirect: f.checkRedirect,
		// No Client.Timeout: the deadline is a context so that it covers body reads
		// and is visible to the caller's own cancellation.
	}
	if f.robots == nil {
		f.robots = NewRobotsCache(f.client, f.userAgent, WithRobotsClock(f.now))
	}
	return f
}

// checkRedirect caps the chain, re-validates every hop against the URL policy, and
// records whether the chain left the originally requested host.
//
// The address policy does not depend on this function: the dialer control hook fires
// again for every connection the chain makes, so a redirect to a denied address is
// refused at connect(2) whatever this returns. What this adds is the compliance
// boundary -- a redirect must not silently move a source to a host whose robots policy
// and terms nobody has reviewed.
func (f *Fetcher) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > f.maxRedirects {
		return fmt.Errorf("%w: chain exceeded %d hops", ErrTooManyRedirects, f.maxRedirects)
	}
	if _, err := f.guard.ValidateURL(req.URL.String()); err != nil {
		return err
	}
	st := redirectStateFrom(req.Context())
	if st == nil {
		return nil
	}
	st.hops = len(via)
	st.location = req.URL.String()
	if st.originalHost != "" && !strings.EqualFold(req.URL.Hostname(), st.originalHost) {
		st.offHost = true
		if !st.allowOffHost {
			// Stop and hand back the 3xx: the off-host body is never fetched, so an
			// unreviewed host never becomes a de facto source.
			return http.ErrUseLastResponse
		}
	}
	return nil
}

// Fetch performs one guarded conditional retrieval.
//
// It returns a non-nil error only when the request URL itself is rejected, which is a
// registry problem the caller should surface. Every other failure is classified: the
// outcome is always a valid domain.CheckOutcome and the cause is in FetchResult.Err.
func (f *Fetcher) Fetch(ctx context.Context, req application.FetchRequest) (application.FetchResult, error) {
	res := application.FetchResult{}

	u, err := f.guard.ValidateURL(req.URL)
	if err != nil {
		res.Err = err
		if errors.Is(err, ErrBlockedAddress) {
			res.Outcome = domain.OutcomeSuspiciousContent
		} else {
			res.Outcome = domain.OutcomeManualReviewRequired
		}
		return res, err
	}

	userAgent := strings.TrimSpace(req.UserAgent)
	if userAgent == "" {
		userAgent = f.userAgent
	}
	maxBytes := req.MaxBytes
	if maxBytes <= 0 || maxBytes > f.maxBytes {
		maxBytes = f.maxBytes
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = f.timeout
	}

	if req.RespectRobots {
		allowed, known, crawlDelay, rerr := f.robots.Allowed(ctx, userAgent, u.String())
		switch {
		case known && !allowed:
			res.Outcome = domain.OutcomeManualReviewRequired
			res.Err = fmt.Errorf("fetch: robots.txt disallows %s for %q", u.Path, productToken(userAgent))
			return res, nil
		case !known && f.blockOnUnknown:
			res.Outcome = domain.OutcomeManualReviewRequired
			res.Err = fmt.Errorf("fetch: robots.txt policy for %s is unknown: %w", u.Host, rerr)
			return res, nil
		case known && crawlDelay > 0:
			// A declared Crawl-delay is a rate limit the host asked for. Honouring it
			// client-side is the whole point of reading it.
			if err := f.pace(ctx, u.Host, crawlDelay); err != nil {
				res.Outcome = domain.OutcomeUnavailable
				res.Err = err
				return res, nil
			}
		}
	}

	release, err := f.acquireHost(ctx, u.Host)
	if err != nil {
		res.Outcome = domain.OutcomeUnavailable
		res.Err = err
		return res, nil
	}
	defer release()

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	st := &redirectState{originalHost: u.Hostname()}
	httpReq, err := http.NewRequestWithContext(withRedirectState(reqCtx, st), http.MethodGet, u.String(), nil)
	if err != nil {
		res.Outcome = domain.OutcomeUnavailable
		res.Err = err
		return res, nil
	}
	httpReq.Header.Set("User-Agent", userAgent)
	httpReq.Header.Set("Accept", "*/*")
	// Compression is negotiated explicitly rather than left to the transport, because
	// transparent decompression hides the ratio and makes a decompression bomb
	// invisible until it has already been allocated.
	httpReq.Header.Set("Accept-Encoding", "gzip")
	if req.ETag != "" {
		httpReq.Header.Set("If-None-Match", req.ETag)
	}
	if req.LastModified != "" {
		httpReq.Header.Set("If-Modified-Since", req.LastModified)
	}

	resp, err := f.client.Do(httpReq)
	if err != nil {
		res.Err = err
		switch {
		case errors.Is(err, ErrBlockedAddress), errors.Is(err, ErrBlockedHost), errors.Is(err, ErrBlockedPort), errors.Is(err, ErrBlockedScheme):
			// A registered source that resolves into a denied range is either a
			// misconfiguration or an attack. Either way a human decides, and it is
			// never retried as a transient fault.
			res.Outcome = domain.OutcomeSuspiciousContent
		default:
			res.Outcome = domain.OutcomeUnavailable
		}
		if st.location != "" {
			res.RedirectLocation = st.location
			res.OffRegisteredHost = st.offHost
		}
		return res, nil
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	res.StatusCode = resp.StatusCode
	res.ETag = resp.Header.Get("ETag")
	res.LastModified = resp.Header.Get("Last-Modified")
	res.ContentType = resp.Header.Get("Content-Type")
	if st.location != "" {
		res.RedirectLocation = st.location
	}

	// A chain that left the registered host is reported as a relocation regardless of
	// what the final status was, because adopting the new host is a compliance decision
	// and not the fetcher's to make.
	if st.offHost {
		res.Outcome = domain.OutcomeRedirected
		res.OffRegisteredHost = true
		if res.RedirectLocation == "" {
			res.RedirectLocation = resolveLocation(resp)
		}
		return res, nil
	}

	switch {
	case resp.StatusCode == http.StatusNotModified:
		// The cheap path the economics depend on: no body is read at all.
		res.Outcome = domain.OutcomeUnchanged
		if req.ETag != "" {
			res.ChangeSignal = domain.SignalETag
		} else if req.LastModified != "" {
			res.ChangeSignal = domain.SignalLastModified
		} else {
			res.ChangeSignal = domain.SignalConditionalGet
		}
		// A 304 does not repeat the validators; keep the ones we sent so the caller's
		// stored state does not get blanked.
		if res.ETag == "" {
			res.ETag = req.ETag
		}
		if res.LastModified == "" {
			res.LastModified = req.LastModified
		}
		return res, nil

	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if err := validateContentType(req.ExpectedContentType, res.ContentType); err != nil {
			res.Outcome = domain.OutcomeParserFailed
			res.Err = err
			return res, nil
		}
		body, n, err := f.readBody(resp, maxBytes)
		if err != nil {
			if errors.Is(err, errBodyTooLarge) || errors.Is(err, errBadEncoding) {
				res.Outcome = domain.OutcomeSuspiciousContent
			} else {
				res.Outcome = domain.OutcomeUnavailable
			}
			res.BytesRead = n
			res.Err = err
			return res, nil
		}
		res.Body = body
		res.BytesRead = n
		res.Outcome = domain.OutcomeChanged
		if req.ETag != "" || req.LastModified != "" {
			res.ChangeSignal = domain.SignalConditionalGet
		} else {
			res.ChangeSignal = domain.SignalFullCompare
		}
		return res, nil

	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		// FirmScout never works around authentication. This is a terminal, honest
		// state, not a transient failure to retry with a different user agent.
		res.Outcome = domain.OutcomeUnauthorized
		res.Err = fmt.Errorf("fetch: source requires credentials (status %d)", resp.StatusCode)
		return res, nil

	case resp.StatusCode == http.StatusTooManyRequests:
		res.Outcome = domain.OutcomeRateLimited
		res.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), f.now())
		res.Err = fmt.Errorf("fetch: rate limited by host (status 429)")
		return res, nil

	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusGone:
		res.Outcome = domain.OutcomeUnavailable
		res.Err = fmt.Errorf("fetch: source not present (status %d)", resp.StatusCode)
		return res, nil

	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// Reached only when the chain stopped without leaving the host, for example a
		// 3xx carrying no Location header.
		res.Outcome = domain.OutcomeRedirected
		if res.RedirectLocation == "" {
			res.RedirectLocation = resolveLocation(resp)
		}
		return res, nil

	default:
		res.Outcome = domain.OutcomeUnavailable
		res.Err = fmt.Errorf("fetch: unexpected status %d", resp.StatusCode)
		if ra := parseRetryAfter(resp.Header.Get("Retry-After"), f.now()); ra > 0 {
			// A 503 with Retry-After is the host telling us when to come back.
			res.RetryAfter = ra
		}
		return res, nil
	}
}

var (
	errBodyTooLarge = errors.New("fetch: response body exceeds the configured limit")
	errBadEncoding  = errors.New("fetch: unsupported or nested content encoding")
)

// readBody reads at most maxBytes of decompressed content. Over-limit is an error and
// never a silent truncation: half a changelog parses cleanly and produces a wrong
// answer, which is the one failure mode FirmScout must not have.
func (f *Fetcher) readBody(resp *http.Response, maxBytes int64) ([]byte, int64, error) {
	counted := &countingReader{r: resp.Body}
	var reader io.Reader = counted

	switch enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(counted)
		if err != nil {
			return nil, 0, fmt.Errorf("fetch: gzip stream is not readable: %w", err)
		}
		defer func() { _ = zr.Close() }()
		reader = zr
	default:
		// Nested or unknown encodings are rejected outright rather than unwrapped;
		// unwrapping is exactly how a bomb gets a second multiplier.
		return nil, 0, fmt.Errorf("%w: %q", errBadEncoding, enc)
	}

	// Read one byte past the limit so that "exactly at the limit" and "over" are
	// distinguishable.
	body, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	n := int64(len(body))
	if err != nil {
		return nil, n, err
	}
	if n > maxBytes {
		return nil, n, fmt.Errorf("%w: read more than %d bytes", errBodyTooLarge, maxBytes)
	}
	if f.maxRatio > 0 && n >= decompressionRatioFloor && counted.n > 0 && n/counted.n > f.maxRatio {
		return nil, n, fmt.Errorf("%w: %d bytes expanded from %d (ratio %d:1)",
			errBodyTooLarge, n, counted.n, n/counted.n)
	}
	return body, n, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// validateContentType compares media types only, ignoring parameters: a vendor adding
// "; charset=utf-8" is not a parser failure.
func validateContentType(expected, actual string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return nil
	}
	want := mediaType(expected)
	got := mediaType(actual)
	if got == "" {
		return fmt.Errorf("fetch: expected content type %q, response declared none", want)
	}
	if want != got {
		return fmt.Errorf("fetch: expected content type %q, got %q", want, got)
	}
	return nil
}

func mediaType(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if mt, _, err := mime.ParseMediaType(v); err == nil {
		return strings.ToLower(mt)
	}
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(strings.TrimSpace(v))
}

// parseRetryAfter accepts both forms RFC 9110 §10.2.3 permits: delay-seconds and an
// HTTP-date. A host that answers in dates is honoured as exactly as one that answers in
// seconds.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
		return 0
	}
	return 0
}

func resolveLocation(resp *http.Response) string {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return ""
	}
	if resp.Request != nil && resp.Request.URL != nil {
		if u, err := resp.Request.URL.Parse(loc); err == nil {
			return u.String()
		}
	}
	return loc
}

// acquireHost takes a per-host slot and applies the minimum inter-request interval. It
// returns the release function.
func (f *Fetcher) acquireHost(ctx context.Context, host string) (func(), error) {
	f.mu.Lock()
	sem, ok := f.hostSem[host]
	if !ok {
		sem = make(chan struct{}, f.perHostConcurrency)
		f.hostSem[host] = sem
	}
	f.mu.Unlock()

	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	release := func() { <-sem }

	if err := f.pace(ctx, host, f.minHostInterval); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// pace reserves the next permitted send time for a host and waits for it. The slot is
// reserved under the lock and waited for outside it, so N callers queue rather than all
// waiting for the same instant and then firing together.
func (f *Fetcher) pace(ctx context.Context, host string, interval time.Duration) error {
	if interval <= 0 {
		return nil
	}
	now := f.now()
	f.mu.Lock()
	next := f.hostNext[host]
	if next.Before(now) {
		next = now
	}
	f.hostNext[host] = next.Add(interval)
	f.mu.Unlock()

	wait := next.Sub(now)
	if wait <= 0 {
		return nil
	}
	select {
	case <-f.after(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
