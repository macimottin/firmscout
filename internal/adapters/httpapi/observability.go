package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/macimottin/firmscout/internal/application"
)

// Everything this adapter records goes through the application.Metrics port and the
// metric names declared in internal/application. The adapter never reaches into the
// telemetry adapter for an instrument: adapters do not depend on each other, and the
// composition root is what connects the two. internal/archtest enforces that.

// Metric label keys. Only low-cardinality dimensions appear here.
//
// # Cardinality is a budget
//
// A Prometheus label creates one time series per distinct value, forever. A product
// slug, a source id, a URL or an API key is unbounded or grows with the catalogue, so
// none of them may ever be a label: labelling this API's request counter by slug would
// mean one series per product in a catalogue the blueprint sizes in the hundreds of
// thousands, which degrades every query against the metric and can take down the
// Prometheus instance the rest of the organisation's dashboards depend on.
//
// The dimensions below are all small, enumerable sets known in advance. Per-request
// identity -- the slug, the path, the request id -- goes on the span and in the
// structured log, which are queried over a bounded window rather than indexed into a
// permanent series.
const (
	labelMethod     = "method"
	labelRoute      = "route"
	labelStatusCode = "status_code"
	labelAPITier    = "api_tier"
	labelDirection  = "direction"
	labelScope      = "scope"
	labelResult     = "result"
)

// Label values, spelled once.
const (
	directionRequest  = "request"
	directionResponse = "response"

	cacheResultHit  = "hit"
	cacheResultMiss = "miss"

	scopeAPIKey = "api_key"
	scopeIP     = "ip"
)

// recordRequest emits the three request-level instruments for one served request.
func (s *Server) recordRequest(ctx context.Context, method, route string, status int, tier string, d time.Duration, reqBytes, respBytes int64) {
	statusCode := statusCodeLabel(status)
	s.metrics.Counter(ctx, application.MetricAPIRequestsTotal, 1,
		application.A(labelMethod, method),
		application.A(labelRoute, route),
		application.A(labelStatusCode, statusCode),
		application.A(labelAPITier, tier),
	)
	s.metrics.Histogram(ctx, application.MetricAPIRequestDuration, d.Seconds(),
		application.A(labelMethod, method),
		application.A(labelRoute, route),
		application.A(labelStatusCode, statusCode),
	)
	if reqBytes > 0 {
		s.metrics.Histogram(ctx, application.MetricAPIPayloadSize, float64(reqBytes),
			application.A(labelRoute, route),
			application.A(labelDirection, directionRequest),
		)
	}
	s.metrics.Histogram(ctx, application.MetricAPIPayloadSize, float64(respBytes),
		application.A(labelRoute, route),
		application.A(labelDirection, directionResponse),
	)
}

// recordRateLimitEvent counts one request the token bucket refused. scope says whether
// the bucket was keyed by API key or by client address -- never which key or address.
func (s *Server) recordRateLimitEvent(ctx context.Context, route, scope string) {
	s.metrics.Counter(ctx, application.MetricAPIRateLimitEvents, 1,
		application.A(labelRoute, route),
		application.A(labelScope, scope),
	)
}

// recordQuotaViolation counts one request rejected for exhausting a monthly quota.
func (s *Server) recordQuotaViolation(ctx context.Context, route, tier string) {
	s.metrics.Counter(ctx, application.MetricAPIQuotaViolations, 1,
		application.A(labelRoute, route),
		application.A(labelAPITier, tier),
	)
}

// recordCacheResult counts a conditional-request hit or miss.
func (s *Server) recordCacheResult(ctx context.Context, route, result string) {
	s.metrics.Counter(ctx, application.MetricAPICacheResults, 1,
		application.A(labelRoute, route),
		application.A(labelResult, result),
	)
}

// statusCodeLabel renders a status code as a label value. The set of codes this API
// produces is a short, fixed list, so the exact code is a bounded dimension and is more
// useful on a dashboard than a collapsed class would be; StatusClass is available for
// callers that want the coarser cut.
func statusCodeLabel(status int) string {
	if status < 100 || status > 999 {
		return "unknown"
	}
	return string([]byte{byte('0' + status/100), byte('0' + (status/10)%10), byte('0' + status%10)})
}

// StatusClass renders a status code as its class ("2xx", "5xx"). Exported because the
// alert rules aggregate by class.
func StatusClass(status int) string {
	switch {
	case status >= 100 && status < 200:
		return "1xx"
	case status < 300:
		return "2xx"
	case status < 400:
		return "3xx"
	case status < 500:
		return "4xx"
	case status < 600:
		return "5xx"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// correlation
// ---------------------------------------------------------------------------

// requestIDAttributeKey is the span attribute the request id is stamped as, so a trace
// found from a log line can also be found from a request id a customer quotes.
const requestIDAttributeKey = attribute.Key("firmscout.request_id")

// annotateSpan stamps the request id onto the active span.
func annotateSpan(ctx context.Context, requestID string) {
	if requestID == "" {
		return
	}
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(requestIDAttributeKey.String(requestID))
	}
}

// correlationHandler stamps request_id, trace_id and span_id onto every record the
// server's logger emits, not only onto the access-log line.
//
// Doing it in a handler rather than at each call site is what makes the guarantee
// total: a log written from a handler, from the recovery middleware or from the
// problem writer all carry the same three identifiers, so a support conversation that
// starts with a request id reaches every line the request produced and the trace
// behind them.
//
// Wrapping is idempotent, so a logger that already correlates is not double-stamped.
type correlationHandler struct{ inner slog.Handler }

func withCorrelation(h slog.Handler) slog.Handler {
	if h == nil {
		return nil
	}
	if _, ok := h.(*correlationHandler); ok {
		return h
	}
	return &correlationHandler{inner: h}
}

func (h *correlationHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *correlationHandler) Handle(ctx context.Context, rec slog.Record) error {
	if id := RequestIDFromContext(ctx); id != "" {
		rec.AddAttrs(slog.String("request_id", id))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		rec.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, rec)
}

func (h *correlationHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &correlationHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *correlationHandler) WithGroup(name string) slog.Handler {
	return &correlationHandler{inner: h.inner.WithGroup(name)}
}

// ---------------------------------------------------------------------------
// request id context
// ---------------------------------------------------------------------------

type requestIDKey struct{}

// ContextWithRequestID returns a context carrying the request correlation id.
func ContextWithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFromContext returns the request correlation id, or "" when there is none.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// traceIDFromContext returns the W3C trace id of the active span, or "" when the
// request is not being traced.
//
// It is recorded on an audit row rather than only in a log line because the audit trail
// outlives the log retention window: a decision from six months ago is still queryable,
// and the trace id is what lets somebody line it up with whatever telemetry was kept.
func traceIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return sc.TraceID().String()
	}
	return ""
}

// RequestIDFromRequest is the convenience form for a handler that has the request.
func RequestIDFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	return RequestIDFromContext(r.Context())
}
