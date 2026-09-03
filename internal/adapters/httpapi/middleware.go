package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// The middleware chain is written out here rather than assembled by a framework. It
// is a handful of ordinary http.Handler wrappers, and the ordering between them is
// load-bearing enough to be worth reading in one place:
//
//	RequestID   -- so everything below, including a panic, has a correlation id
//	Telemetry   -- otelhttp span + the API metrics, outside the recovery so a panic
//	               is counted as the 500 it becomes
//	Recover     -- a panic becomes a problem document, not a dropped connection
//	Logging     -- the access log, with the final status
//	 ... per route:
//	RouteTag    -- records the route *pattern* for metrics and logs
//	APIKey      -- optional; anonymous is a supported, first-class caller
//	Usage       -- above the gates, so a throttled request is still metered
//	RateLimit   -- short-window smoothing
//	Quota       -- durable monthly allowance
//	CacheHeaders-- ETag/Cache-Control and 304 handling
//	handler
//
// Usage sits above RateLimit and Quota on purpose: a request rejected by either gate
// is still a request the consumer made, and UsageRecord has a RateLimited field
// precisely so that shows up in the meter rather than vanishing.

// Header names used by the API. Spelled once so a typo cannot produce a header a
// consumer's client library will never look at.
const (
	HeaderRequestID           = "X-Request-Id"
	HeaderAPIKey              = "X-API-Key"
	HeaderRetryAfter          = "Retry-After"
	HeaderRateLimitLimit      = "X-RateLimit-Limit"
	HeaderRateLimitRemaining  = "X-RateLimit-Remaining"
	HeaderRateLimitReset      = "X-RateLimit-Reset"
	HeaderQuotaLimit          = "X-Quota-Limit"
	HeaderQuotaRemaining      = "X-Quota-Remaining"
	HeaderQuotaReset          = "X-Quota-Reset"
	HeaderETag                = "ETag"
	HeaderIfNoneMatch         = "If-None-Match"
	HeaderCacheControl        = "Cache-Control"
	HeaderAuthorization       = "Authorization"
	HeaderXForwardedFor       = "X-Forwarded-For"
	headerContentTypeKey      = "Content-Type"
	maxRequestIDLength        = 128
	slowRequestLogThresholdMS = 500
)

// TierAnonymous is the api_tier label and usage plan name for a caller with no key.
const TierAnonymous = "anonymous"

// ---------------------------------------------------------------------------
// context plumbing
// ---------------------------------------------------------------------------

type ctxKey int

const (
	ctxKeyLogger ctxKey = iota
	ctxKeyRoute
	ctxKeyCaller
)

// routeContext is filled in by the per-route wrapper and read by the middlewares that
// sit above the mux.
//
// It is a pointer placed in the context by the outermost middleware and mutated by
// the innermost one, because http.ServeMux resolves the route *after* the outer
// middlewares have already run, and r.Pattern is set on a request copy the outer
// wrappers never see. Getting this wrong is how a metric ends up labelled with a
// resolved path -- one time series per product slug, which is the cardinality
// incident observability.md §5 forbids by name.
type routeContext struct {
	Pattern string
	Class   EndpointClass
	Cache   string
}

func withRouteContext(ctx context.Context, rc *routeContext) context.Context {
	return context.WithValue(ctx, ctxKeyRoute, rc)
}

func routeContextFrom(ctx context.Context) *routeContext {
	rc, _ := ctx.Value(ctxKeyRoute).(*routeContext)
	return rc
}

// RoutePatternFromContext returns the matched route pattern, e.g.
// "/api/v1/products/{slug}", or "unmatched" before routing has happened.
func RoutePatternFromContext(ctx context.Context) string {
	if rc := routeContextFrom(ctx); rc != nil && rc.Pattern != "" {
		return rc.Pattern
	}
	return "unmatched"
}

// LoggerFromContext returns the request-scoped logger, falling back to the default.
func LoggerFromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// Caller is the authenticated identity behind a request, or the anonymous one.
type Caller struct {
	Consumer      application.APIConsumer
	APIKeyID      string
	Authenticated bool
}

// Tier returns the api_tier metric label. It is a plan name or "anonymous" -- never
// a consumer id, which would be an unbounded label.
func (c Caller) Tier() string {
	if !c.Authenticated || c.Consumer.Plan == "" {
		return TierAnonymous
	}
	return c.Consumer.Plan
}

// CallerFromContext returns the caller. The zero value is the anonymous caller, which
// is a supported identity on every read endpoint: the public website calls this same
// API without a key.
func CallerFromContext(ctx context.Context) Caller {
	c, _ := ctx.Value(ctxKeyCaller).(Caller)
	return c
}

// ---------------------------------------------------------------------------
// response recording
// ---------------------------------------------------------------------------

// responseRecorder observes the status code and byte count without buffering.
type responseRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
}

func newResponseRecorder(w http.ResponseWriter) *responseRecorder {
	return &responseRecorder{ResponseWriter: w, status: http.StatusOK}
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.wrote {
		return
	}
	r.status = status
	r.wrote = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, so flushing and
// deadline control still work through the chain.
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush forwards to the underlying writer when it supports it.
func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// bufferingRecorder holds the whole response so an ETag can be computed over it and a
// 304 substituted. Only used on cacheable GETs, whose bodies are small by
// construction (a page of releases, a product summary) -- it is never wrapped around
// /metrics or anything streaming.
type bufferingRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
	wrote  bool
}

func newBufferingRecorder() *bufferingRecorder {
	return &bufferingRecorder{header: make(http.Header), status: http.StatusOK}
}

func (b *bufferingRecorder) Header() http.Header { return b.header }

func (b *bufferingRecorder) WriteHeader(status int) {
	if b.wrote {
		return
	}
	b.status = status
	b.wrote = true
}

func (b *bufferingRecorder) Write(p []byte) (int, error) {
	if !b.wrote {
		b.WriteHeader(http.StatusOK)
	}
	return b.body.Write(p)
}

// ---------------------------------------------------------------------------
// RequestID
// ---------------------------------------------------------------------------

// RequestID assigns or adopts the correlation id and echoes it on the response.
//
// A client-supplied id is adopted only if it is syntactically safe. A request id ends
// up in a response header, in structured logs and on span attributes, so accepting an
// arbitrary caller-controlled string is a header-injection and log-forging vector;
// anything that fails the check is replaced rather than sanitised, because a silently
// altered id is worse for correlation than an obviously different one.
func (s *Server) RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if !validRequestID(id) {
			id = s.newRequestID()
		}
		ctx := ContextWithRequestID(r.Context(), id)
		ctx = withRouteContext(ctx, &routeContext{})
		ctx = context.WithValue(ctx, ctxKeyLogger, s.logger)
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

func (s *Server) newRequestID() string {
	if s.ids != nil {
		if id := s.ids.NewID("req"); validRequestID(id) {
			return id
		}
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail on any supported platform; if it somehow does,
		// a time-based id still correlates a single request's own log lines.
		return "req_" + strconv.FormatInt(s.now().UnixNano(), 36)
	}
	return "req_" + hex.EncodeToString(buf[:])
}

// ---------------------------------------------------------------------------
// Telemetry
// ---------------------------------------------------------------------------

// Telemetry wraps the chain in an otelhttp server span and records the API metrics.
//
// The metric labels are the route pattern, the method, the status code and the api
// tier, and nothing else. No slug, no path, no key, no source id: each of those is
// unbounded or grows with the catalogue, and a Prometheus label is a permanent time
// series per distinct value. Per-request identity goes on the span and in the log,
// where it is queried over a bounded window instead of indexed forever.
func (s *Server) Telemetry(next http.Handler) http.Handler {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		annotateSpan(r.Context(), RequestIDFromContext(r.Context()))

		rec := newResponseRecorder(w)
		next.ServeHTTP(rec, r)

		ctx := r.Context()
		s.recordRequest(ctx,
			r.Method,
			RoutePatternFromContext(ctx),
			rec.status,
			CallerFromContext(ctx).Tier(),
			s.now().Sub(start),
			max64(r.ContentLength, 0),
			rec.written,
		)
	})
	return otelhttp.NewHandler(inner, "http.server.request",
		otelhttp.WithTracerProvider(s.tracerProvider),
		otelhttp.WithMeterProvider(s.otelHTTPMeterProvider),
		otelhttp.WithPropagators(s.propagator),
	)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------

// Logging writes the structured access log.
//
// It logs the route *pattern*, never the resolved path, and it logs no header, no
// query string and no body. The Authorization header, the X-API-Key header and any
// request body are on observability.md §6's absolute redaction list; the cheapest way
// to honour a redaction list is to never put the value anywhere near the logger in
// the first place, which is why nothing here reads them.
//
// trace_id, span_id and request_id are added by the correlation handler the server
// wraps its logger in, so they appear on every record this logger emits and not only
// on the access log line.
func (s *Server) Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		rec := newResponseRecorder(w)
		next.ServeHTTP(rec, r)

		ctx := r.Context()
		durationMS := s.now().Sub(start).Milliseconds()
		attrs := []any{
			slog.String("method", r.Method),
			slog.String("route", RoutePatternFromContext(ctx)),
			slog.Int("status", rec.status),
			slog.Int64("duration_ms", durationMS),
			slog.Int64("bytes_out", rec.written),
			slog.String("api_tier", CallerFromContext(ctx).Tier()),
		}
		log := LoggerFromContext(ctx)
		switch {
		case rec.status >= 500:
			log.ErrorContext(ctx, "http request", attrs...)
		case rec.status >= 400:
			log.WarnContext(ctx, "http request", attrs...)
		case durationMS >= slowRequestLogThresholdMS:
			log.InfoContext(ctx, "http request", attrs...)
		default:
			// A fast, successful request is fully described by the metrics. Logging
			// every one of them would bury the requests worth reading.
			log.DebugContext(ctx, "http request", attrs...)
		}
	})
}

// ---------------------------------------------------------------------------
// Recover
// ---------------------------------------------------------------------------

// Recover turns a panic into a 500 problem document and an ERROR log, and leaves the
// process running.
//
// http.Server would already recover a panic to keep the process alive, but it does so
// by closing the connection without a response, which a consumer sees as a transport
// error with no request id to quote. Recovering here produces the same problem
// document any other failure produces.
func (s *Server) Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := newResponseRecorder(w)
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
				panic(v)
			}
			ctx := r.Context()
			LoggerFromContext(ctx).ErrorContext(ctx, "recovered panic in handler",
				slog.Any("panic", v),
				slog.String("route", RoutePatternFromContext(ctx)),
				slog.String("stack", string(debug.Stack())),
			)
			if rec.wrote {
				// The status line is already on the wire; there is no way to turn
				// this into a problem document now. The log above is the record.
				return
			}
			WriteProblem(rec, r, Internal(r, errPanic))
		}()
		next.ServeHTTP(rec, r)
	})
}

var errPanic = errors.New("handler panicked")

// ---------------------------------------------------------------------------
// APIKey
// ---------------------------------------------------------------------------

// APIKey resolves an optional API key into a Caller.
//
// Authentication is optional by design: every read endpoint accepts anonymous
// requests, because the public website is a client of this same API. A request with
// no credential therefore proceeds as the anonymous caller rather than being
// rejected.
//
// A credential that is *presented* but does not resolve is a different matter and is
// rejected with 401. Silently downgrading a revoked key to anonymous would leave a
// customer debugging mysterious rate limits instead of reading an error that names
// the problem.
func (s *Server) APIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := extractAPIKey(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		if s.apiKeys == nil {
			WriteProblem(w, r, Unauthenticated(r, "API key authentication is not configured on this deployment."))
			return
		}

		// Keys are stored as SHA-256 hashes; the plaintext is shown once at creation
		// and never persisted, so the lookup is by digest. The plaintext token is not
		// logged, not put on a span, and not carried past this line.
		sum := sha256.Sum256([]byte(token))
		hash := hex.EncodeToString(sum[:])

		consumer, keyID, err := s.apiKeys.ResolveByHash(r.Context(), hash)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			WriteProblem(w, r, Unauthenticated(r, "The API key is not valid or has been revoked."))
			return
		case err != nil:
			// The key store is unreachable. This is retryable and is not the
			// caller's fault, so it is a 503 rather than a 401 that would send them
			// hunting for a credential problem that does not exist.
			WriteProblem(w, r, ServiceUnavailable(r, "Authentication is temporarily unavailable. Retry shortly.", err))
			return
		}
		if consumer.Status != "" && consumer.Status != "active" {
			WriteProblem(w, r, Forbidden(r, "This API key's account is not active."))
			return
		}

		caller := Caller{Consumer: consumer, APIKeyID: keyID, Authenticated: true}
		ctx := context.WithValue(r.Context(), ctxKeyCaller, caller)

		// last_used_at is a write, and a write on the read path of every keyed
		// request is a write nobody asked for. It goes to the background worker.
		at := s.now()
		s.runAfterResponse(func(bg context.Context) {
			if err := s.apiKeys.TouchLastUsed(bg, keyID, at); err != nil {
				s.logger.Debug("touch api key last_used_at failed", slog.String("error", err.Error()))
			}
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// extractAPIKey reads the credential from either accepted carrier. It returns ok
// false when no credential was presented at all, which is the anonymous path.
func extractAPIKey(r *http.Request) (string, bool) {
	if h := r.Header.Get(HeaderAuthorization); h != "" {
		const prefix = "bearer "
		if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
			if token := strings.TrimSpace(h[len(prefix):]); token != "" {
				return token, true
			}
		}
		// A present but unparseable Authorization header is a presented credential:
		// returning an empty token here makes it fail resolution and produce a 401,
		// which is the right answer for "Authorization: Basic ...".
		return "", true
	}
	if k := strings.TrimSpace(r.Header.Get(HeaderAPIKey)); k != "" {
		return k, true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// RateLimit
// ---------------------------------------------------------------------------

// RateLimit applies the per-endpoint-class token bucket, keyed by API key when there
// is one and by client address otherwise.
//
// See RateLimitConfig for the honest account of what a per-instance bucket does and
// does not bound across a fleet.
func (s *Server) RateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.rateLimits.Disabled {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		rc := routeContextFrom(ctx)
		class := ClassDetail
		if rc != nil && rc.Class != "" {
			class = rc.Class
		}
		if class == ClassSystem {
			next.ServeHTTP(w, r)
			return
		}

		caller := CallerFromContext(ctx)
		scope := scopeIP
		bucketKey := "ip:" + s.clientIP(r)
		if caller.Authenticated {
			scope = scopeAPIKey
			bucketKey = "key:" + caller.Consumer.ID
		}
		policy, ok := s.rateLimits.policyFor(caller.Authenticated, class)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		if caller.Authenticated && caller.Consumer.RateLimitPerMin > 0 {
			// A consumer's contracted per-minute rate overrides the class default,
			// while the class still decides the burst shape.
			policy.PerMinute = float64(caller.Consumer.RateLimitPerMin)
			if policy.Burst > caller.Consumer.RateLimitPerMin {
				policy.Burst = caller.Consumer.RateLimitPerMin
			}
		}

		d := s.limiter.allow(bucketKey+"|"+string(class), policy)

		// The contract puts these headers on every response, not only on the 429, so
		// a consumer can see how close they are before they hit the wall.
		h := w.Header()
		h.Set(HeaderRateLimitLimit, strconv.Itoa(d.Limit))
		h.Set(HeaderRateLimitRemaining, strconv.Itoa(d.Remaining))
		h.Set(HeaderRateLimitReset, strconv.FormatInt(d.Reset.Unix(), 10))

		if !d.Allowed {
			s.recordRateLimitEvent(ctx, RoutePatternFromContext(ctx), scope)
			h.Set(HeaderRetryAfter, strconv.Itoa(int(d.RetryAfter/time.Second)))
			WriteProblem(w, r, RateLimited(r,
				"Too many requests on this endpoint class. Retry after the number of seconds in the Retry-After header."))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP resolves the address the bucket is keyed by.
//
// X-Forwarded-For is trusted only when the deployment says a trusted proxy sets it.
// Trusting it unconditionally would let any caller pick their own bucket key by
// forging the header, which is not a rate limiter at all.
func (s *Server) clientIP(r *http.Request) string {
	if s.trustProxyHeaders {
		if xff := r.Header.Get(HeaderXForwardedFor); xff != "" {
			// The left-most entry is the original client as recorded by the first
			// trusted proxy in the chain.
			if i := strings.IndexByte(xff, ','); i >= 0 {
				xff = xff[:i]
			}
			if ip := strings.TrimSpace(xff); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// Quota
// ---------------------------------------------------------------------------

// Quota enforces the durable monthly allowance for authenticated consumers.
//
// Anonymous callers have no quota concept -- there is nothing to bill and nobody to
// bill it to -- so they pass straight through and rely on the rate limiter alone.
func (s *Server) Quota(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		caller := CallerFromContext(ctx)
		if !caller.Authenticated || caller.Consumer.MonthlyQuota <= 0 || s.usage == nil {
			next.ServeHTTP(w, r)
			return
		}

		periodStart := monthStart(s.now())
		consumed, err := s.usage.QuotaConsumed(ctx, caller.Consumer.ID, periodStart)
		if err != nil {
			// Fail open. The meter being unavailable is FirmScout's problem, not the
			// paying customer's, and the durable counter will catch up: usage is
			// recorded from the same records this reads, so nothing is lost, only
			// delayed. Failing closed here would turn a metering outage into a
			// customer-visible outage.
			s.logger.WarnContext(ctx, "quota lookup failed, allowing request",
				slog.String("error", err.Error()))
			next.ServeHTTP(w, r)
			return
		}

		reset := periodStart.AddDate(0, 1, 0)
		remaining := caller.Consumer.MonthlyQuota - consumed
		if remaining < 0 {
			remaining = 0
		}
		h := w.Header()
		h.Set(HeaderQuotaLimit, strconv.FormatInt(caller.Consumer.MonthlyQuota, 10))
		h.Set(HeaderQuotaRemaining, strconv.FormatInt(remaining, 10))
		h.Set(HeaderQuotaReset, strconv.FormatInt(reset.Unix(), 10))

		if consumed >= caller.Consumer.MonthlyQuota {
			route := RoutePatternFromContext(ctx)
			s.recordQuotaViolation(ctx, route, caller.Tier())
			retryAfter := int(reset.Sub(s.now()) / time.Second)
			if retryAfter < 1 {
				retryAfter = 1
			}
			h.Set(HeaderRetryAfter, strconv.Itoa(retryAfter))
			s.publish(domain.NewEvent(domain.EventAPIQuotaExceeded, s.now(), "consumer", caller.Consumer.ID).
				With("plan", caller.Consumer.Plan).
				With("endpoint", route))
			// The detail says out loud that Retry-After is a month, because a raw
			// header value of 1.8 million seconds invites a consumer to assume a bug.
			WriteProblem(w, r, QuotaExceeded(r,
				"The monthly quota for this API key is exhausted. It resets at the start of the next billing period, which is what the Retry-After header reflects; a larger plan raises the limit sooner."))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// ---------------------------------------------------------------------------
// Usage
// ---------------------------------------------------------------------------

// Usage meters an authenticated consumer's request.
//
// The record is built on the response path but written on the background worker: the
// customer's latency does not pay for the meter. The idempotency key is derived from
// the request id, so a retried write -- from the worker, or from a redelivery once
// this becomes a queue -- collapses onto the same row instead of billing twice.
func (s *Server) Usage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		rec := newResponseRecorder(w)
		next.ServeHTTP(rec, r)

		ctx := r.Context()
		caller := CallerFromContext(ctx)
		if !caller.Authenticated || s.usage == nil {
			return
		}
		rc := routeContextFrom(ctx)
		class := ClassDetail
		if rc != nil && rc.Class != "" {
			class = rc.Class
		}
		finished := s.now()
		record := application.UsageRecord{
			IdempotencyKey: "usage:" + RequestIDFromContext(ctx),
			ConsumerID:     caller.Consumer.ID,
			APIKeyID:       caller.APIKeyID,
			Endpoint:       RoutePatternFromContext(ctx),
			Method:         r.Method,
			StatusCode:     rec.status,
			QuotaWeight:    quotaWeight(class),
			DurationMS:     int(finished.Sub(start).Milliseconds()),
			BytesOut:       rec.written,
			RateLimited:    rec.status == http.StatusTooManyRequests,
			OccurredAt:     finished,
		}
		if s.ids != nil {
			record.ID = s.ids.NewID("use")
		}
		s.runAfterResponse(func(bg context.Context) {
			if err := s.usage.Record(bg, record); err != nil {
				s.logger.Error("recording usage failed", slog.String("error", err.Error()),
					slog.String("endpoint", record.Endpoint))
			}
		})
	})
}

// quotaWeight is how much of the monthly allowance one call to this class costs. Every
// MVP endpoint is weight 1; the function exists so the bulk lookup endpoint, which is
// weight N, has somewhere to go that is not an if-statement in the middleware.
func quotaWeight(EndpointClass) int { return 1 }

// ---------------------------------------------------------------------------
// CacheHeaders
// ---------------------------------------------------------------------------

// CacheHeaders sets Cache-Control and a strong ETag on cacheable GETs, and answers a
// matching If-None-Match with 304.
//
// The ETag is the hash of the exact bytes served, which makes it a strong validator:
// two responses with the same ETag are byte-identical, so a conditional request can
// be answered without re-reading anything downstream.
//
// Note for when tiered responses arrive: the plan is for /releases to window history
// by plan. The moment a response body depends on the caller's tier, this must also
// emit `Vary: Authorization`, or a shared cache will hand one tier's response to
// another. Today every response here is identical for every caller, so no Vary is
// correct and the responses stay cacheable at the edge.
func (s *Server) CacheHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		rc := routeContextFrom(ctx)
		if r.Method != http.MethodGet || rc == nil || rc.Cache == "" {
			next.ServeHTTP(w, r)
			return
		}

		buf := newBufferingRecorder()
		next.ServeHTTP(buf, r)

		dst := w.Header()
		for k, v := range buf.header {
			dst[k] = v
		}

		if buf.status != http.StatusOK {
			// Errors carry their own no-store policy from WriteProblem; do not
			// overwrite it, and never attach a validator to a problem document.
			w.WriteHeader(buf.status)
			_, _ = w.Write(buf.body.Bytes())
			return
		}

		sum := sha256.Sum256(buf.body.Bytes())
		etag := `"` + hex.EncodeToString(sum[:]) + `"`
		dst.Set(HeaderETag, etag)
		dst.Set(HeaderCacheControl, rc.Cache)

		route := RoutePatternFromContext(ctx)
		if etagMatches(r.Header.Get(HeaderIfNoneMatch), etag) {
			s.recordCacheResult(ctx, route, cacheResultHit)
			// RFC 9110: a 304 carries no body, and Content-Length must not describe
			// one that is not being sent.
			dst.Del(headerContentTypeKey)
			dst.Del("Content-Length")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		s.recordCacheResult(ctx, route, cacheResultMiss)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf.body.Bytes())
	})
}

// etagMatches implements the If-None-Match comparison. It accepts "*", a list of
// candidates, and the weak form W/"..." matching its strong counterpart, which is the
// weak comparison RFC 9110 prescribes for If-None-Match.
func etagMatches(ifNoneMatch, etag string) bool {
	ifNoneMatch = strings.TrimSpace(ifNoneMatch)
	if ifNoneMatch == "" {
		return false
	}
	if ifNoneMatch == "*" {
		return true
	}
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}
