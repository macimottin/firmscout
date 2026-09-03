// Package httpapi is FirmScout's public HTTP API adapter.
//
// Routing is net/http.ServeMux with Go 1.22 method-and-pattern routes, and the
// middleware chain is written out explicitly in middleware.go. There is no framework
// and no third-party router: since Go 1.22 the standard library does method and
// wildcard routing, which was the only thing a router library was buying, and the
// remaining value -- middleware composition -- is a few dozen lines that the project
// would rather own than depend on. See ADR-0014.
//
// The adapter owns HTTP concerns only. Handlers parse and validate input, call a read
// port or a use case, and hand the result to a presenter; a decision about what the
// data means belongs in internal/domain or internal/application, never here.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// Deps is everything the API adapter needs, as application ports.
//
// It is a struct of interfaces rather than a set of package-level variables so that
// two servers can exist in one process (a test binary routinely has dozens) and so
// that reading this type tells you exactly what the HTTP layer can reach. Nothing in
// this package resolves a dependency any other way.
type Deps struct {
	// Summaries serves the precomputed product read model and search. Required.
	Summaries application.SummaryRepository
	// Vendors serves the vendor catalogue. Required.
	Vendors application.VendorRepository
	// Releases serves release history and the latest-observed release. Required.
	Releases application.ReleaseRepository

	// APIKeys resolves bearer tokens. Optional: without it, every request is
	// anonymous and a presented credential is refused.
	APIKeys application.APIKeyRepository
	// Usage meters keyed requests and answers the quota question. Optional.
	Usage application.UsageRecorder
	// Events receives the product analytics events search emits. Optional.
	Events application.EventPublisher

	// Clock and IDs are injected so tests are deterministic. Both default to the
	// real implementations.
	Clock application.Clock
	IDs   application.IDGenerator

	// Metrics is the port every measurement in this package is recorded through.
	// The composition root passes the telemetry adapter's implementation; nil means
	// application.NopMetrics, so an unconfigured deployment loses observability
	// rather than the request path.
	Metrics application.Metrics
	// Gatherer is what GET /metrics serves.
	Gatherer prometheus.Gatherer
	// TracerProvider and Propagator are used by the otelhttp instrumentation.
	TracerProvider trace.TracerProvider
	Propagator     propagation.TextMapPropagator

	// Logger receives the access log and every error. Pass a plain handler: the
	// server wraps it so that request_id, trace_id and span_id land on every record
	// it emits, and wrapping an already-correlating handler would duplicate them.
	Logger *slog.Logger

	// RateLimits holds the per-endpoint-class token bucket policies. The zero value
	// means DefaultRateLimits.
	RateLimits RateLimitConfig
	// TrustProxyHeaders makes the rate limiter believe X-Forwarded-For. Enable it
	// only when a proxy that overwrites the header is the sole ingress; otherwise
	// any caller can choose their own bucket by forging it.
	TrustProxyHeaders bool

	// Ready is the readiness gate /readyz runs, typically a database ping. Nil
	// means "always ready", which is only correct for a server with no downstream.
	Ready func(context.Context) error

	// BackgroundBuffer bounds the queue of after-response work (usage records,
	// analytics events, last-used touches). Zero means DefaultBackgroundBuffer.
	BackgroundBuffer int
}

// DefaultBackgroundBuffer bounds the after-response work queue.
const DefaultBackgroundBuffer = 1024

// BackgroundTaskTimeout bounds one piece of after-response work.
const BackgroundTaskTimeout = 10 * time.Second

// Server serves the API. Build it with NewServer and close it on shutdown.
type Server struct {
	summaries application.SummaryRepository
	vendors   application.VendorRepository
	releases  application.ReleaseRepository
	apiKeys   application.APIKeyRepository
	usage     application.UsageRecorder
	events    application.EventPublisher
	ids       application.IDGenerator
	clock     application.Clock

	metrics               application.Metrics
	gatherer              prometheus.Gatherer
	tracerProvider        trace.TracerProvider
	otelHTTPMeterProvider metric.MeterProvider
	propagator            propagation.TextMapPropagator
	logger                *slog.Logger

	rateLimits        RateLimitConfig
	limiter           *tokenBucketLimiter
	trustProxyHeaders bool
	ready             func(context.Context) error

	handler http.Handler

	work     chan func(context.Context)
	workDone chan struct{}
	closeMu  sync.Mutex
	closed   bool
}

// NewServer wires the routes and the middleware chain.
func NewServer(d Deps) (*Server, error) {
	if d.Summaries == nil {
		return nil, errors.New("httpapi: Deps.Summaries is required")
	}
	if d.Vendors == nil {
		return nil, errors.New("httpapi: Deps.Vendors is required")
	}
	if d.Releases == nil {
		return nil, errors.New("httpapi: Deps.Releases is required")
	}

	s := &Server{
		summaries:         d.Summaries,
		vendors:           d.Vendors,
		releases:          d.Releases,
		apiKeys:           d.APIKeys,
		usage:             d.Usage,
		events:            d.Events,
		ids:               d.IDs,
		clock:             d.Clock,
		rateLimits:        d.RateLimits,
		trustProxyHeaders: d.TrustProxyHeaders,
		ready:             d.Ready,
	}

	if s.rateLimits.Anonymous == nil && s.rateLimits.Keyed == nil && !s.rateLimits.Disabled {
		s.rateLimits = DefaultRateLimits()
	}
	s.limiter = newTokenBucketLimiter(s.rateLimits.MaxTrackedKeys, s.now)

	s.metrics = d.Metrics
	if s.metrics == nil {
		s.metrics = application.NopMetrics{}
	}

	s.gatherer = d.Gatherer

	s.tracerProvider = d.TracerProvider
	if s.tracerProvider == nil {
		s.tracerProvider = tracenoop.NewTracerProvider()
	}

	s.propagator = d.Propagator
	if s.propagator == nil {
		s.propagator = propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{})
	}

	// otelhttp would emit its own http.server.* metric family. FirmScout's API
	// metrics are the catalogue in observability.md §5, and running two overlapping
	// families is how a dashboard ends up disagreeing with an alert, so otelhttp is
	// given a no-op meter and used for tracing only.
	s.otelHTTPMeterProvider = metricnoop.NewMeterProvider()

	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// Wrapping is idempotent, so a logger that already correlates is left alone.
	s.logger = slog.New(withCorrelation(logger.Handler()))

	buffer := d.BackgroundBuffer
	if buffer <= 0 {
		buffer = DefaultBackgroundBuffer
	}
	s.work = make(chan func(context.Context), buffer)
	s.workDone = make(chan struct{})
	go s.runBackground()

	s.handler = s.buildHandler()
	return s, nil
}

// route describes one endpoint's non-handler properties: which rate-limit class it
// belongs to and how it may be cached.
type route struct {
	pattern string
	class   EndpointClass
	cache   string
	handler http.HandlerFunc
}

// Cache-Control policies, from the per-endpoint table in docs/architecture/api.md §2.
const (
	cacheSearch    = "public, max-age=60"
	cacheCatalogue = "public, max-age=3600"
	cacheReleases  = "public, max-age=300"
	// A published release is immutable, so its representation can be cached for as
	// long as a cache is willing to keep it.
	cacheImmutable = "public, max-age=31536000, immutable"
	cacheNone      = ""
)

func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()

	apiRoutes := []route{
		{"GET /api/v1/search", ClassSearch, cacheSearch, s.handleSearch},
		{"GET /api/v1/vendors", ClassList, cacheCatalogue, s.handleListVendors},
		{"GET /api/v1/vendors/{slug}", ClassDetail, cacheCatalogue, s.handleGetVendor},
		{"GET /api/v1/products/{slug}", ClassDetail, cacheCatalogue, s.handleGetProduct},
		{"GET /api/v1/products/{slug}/releases", ClassList, cacheReleases, s.handleListReleases},
		{"GET /api/v1/products/{slug}/latest", ClassDetail, cacheReleases, s.handleGetLatest},
		{"GET /api/v1/releases/{id}", ClassDetail, cacheImmutable, s.handleGetRelease},
	}
	for _, rt := range apiRoutes {
		mux.Handle(rt.pattern, s.apiChain(rt))
	}

	systemRoutes := []route{
		{"GET /healthz", ClassSystem, cacheNone, s.handleHealthz},
		{"GET /readyz", ClassSystem, cacheNone, s.handleReadyz},
		{"GET /metrics", ClassSystem, cacheNone, s.handleMetrics},
	}
	for _, rt := range systemRoutes {
		mux.Handle(rt.pattern, s.systemChain(rt))
	}

	// A request that matches no route still needs a request id, a problem document
	// and a metric, so the catch-all goes through the global chain like everything
	// else.
	mux.Handle("/", s.tagRoute(route{pattern: "unmatched", class: ClassDetail}, http.HandlerFunc(s.handleNotFound)))

	// Outermost first.
	var h http.Handler = mux
	h = s.Logging(h)
	h = s.Recover(h)
	h = s.Telemetry(h)
	h = s.RequestID(h)
	return h
}

// apiChain is the per-route middleware for a public API endpoint.
func (s *Server) apiChain(rt route) http.Handler {
	var h http.Handler = rt.handler
	h = s.CacheHeaders(h)
	h = s.Quota(h)
	h = s.RateLimit(h)
	h = s.Usage(h)
	h = s.APIKey(h)
	return s.tagRoute(rt, h)
}

// systemChain is the per-route middleware for an infrastructure endpoint. Liveness,
// readiness and metrics scrapes are neither authenticated, metered nor rate limited:
// a probe that gets a 429 during an incident makes the incident worse.
func (s *Server) systemChain(rt route) http.Handler {
	return s.tagRoute(rt, rt.handler)
}

// tagRoute records the matched pattern on the shared routeContext so the middlewares
// above the mux can label metrics and logs with it, and stamps the same pattern onto
// the server span.
//
// Naming the span after the pattern rather than the path is what keeps a trace view
// groupable: "/api/v1/products/{slug}" is one operation, while
// "/api/v1/products/mikrotik-routeros" is one operation per product in the catalogue.
func (s *Server) tagRoute(rt route, next http.Handler) http.Handler {
	routePath := rt.pattern
	if i := strings.IndexByte(routePath, ' '); i >= 0 {
		routePath = routePath[i+1:]
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rc := routeContextFrom(r.Context()); rc != nil {
			// The metric and log dimension is the path pattern alone; the method is
			// already its own label, and combining them would double the series count
			// for no extra information.
			rc.Pattern = routePath
			rc.Class = rt.class
			rc.Cache = rt.cache
		}
		if span := trace.SpanFromContext(r.Context()); span.IsRecording() {
			span.SetName(rt.pattern)
			span.SetAttributes(semconv.HTTPRoute(routePath))
		}
		next.ServeHTTP(w, r)
	})
}

// ServeHTTP makes the Server an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// Handler returns the fully wrapped handler, for a caller that wants to mount it or
// pass it to http.Server directly.
func (s *Server) Handler() http.Handler { return s.handler }

// ---------------------------------------------------------------------------
// after-response work
// ---------------------------------------------------------------------------

// runAfterResponse queues fn onto the background worker.
//
// The queue is bounded and the enqueue never blocks: if after-response work has
// backed up, the work is dropped with a log line rather than adding latency to a
// response that has already been written. Metering that is a few records short is a
// smaller problem than an API that stalls because its meter is slow.
func (s *Server) runAfterResponse(fn func(context.Context)) {
	if fn == nil {
		return
	}
	// The lock is held across the send, not merely across the closed check. Checking
	// and then sending would leave a window in which Close runs in between and the
	// send lands on a closed channel, which panics -- an in-flight request crashing
	// the shutdown path is exactly the kind of bug that only shows up in production.
	// The send is non-blocking, so holding the lock cannot deadlock against Close.
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.work <- fn:
	default:
		s.logger.Warn("after-response work queue full, dropping task")
	}
}

func (s *Server) runBackground() {
	defer close(s.workDone)
	for fn := range s.work {
		func() {
			// A fresh context: the request's is already cancelled by the time this
			// runs, which is precisely why the work could not be done inline.
			ctx, cancel := context.WithTimeout(context.Background(), BackgroundTaskTimeout)
			defer cancel()
			defer func() {
				if v := recover(); v != nil {
					s.logger.Error("panic in after-response work", slog.Any("panic", v))
				}
			}()
			fn(ctx)
		}()
	}
}

// publish queues analytics events for after-response delivery.
func (s *Server) publish(events ...domain.Event) {
	if s.events == nil || len(events) == 0 {
		return
	}
	s.runAfterResponse(func(ctx context.Context) {
		if err := s.events.Publish(ctx, events...); err != nil {
			s.logger.Warn("publishing analytics events failed", slog.String("error", err.Error()))
		}
	})
}

// Close drains the after-response queue and stops the worker. It is safe to call more
// than once, and it is what a test uses to make metering assertions deterministic.
func (s *Server) Close() error {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return nil
	}
	s.closed = true
	close(s.work)
	s.closeMu.Unlock()
	<-s.workDone
	return nil
}

// now reads the injected clock, falling back to the wall clock.
func (s *Server) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

// handleMetrics serves the Prometheus exposition.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	g := s.gatherer
	if g == nil {
		g = prometheus.DefaultGatherer
	}
	promhttp.HandlerFor(g, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
		ErrorLog:      slogErrorLog{s.logger},
	}).ServeHTTP(w, r)
}

// slogErrorLog adapts *slog.Logger to promhttp's tiny logger interface.
type slogErrorLog struct{ log *slog.Logger }

func (l slogErrorLog) Println(v ...any) { l.log.Error("prometheus handler", slog.Any("detail", v)) }
