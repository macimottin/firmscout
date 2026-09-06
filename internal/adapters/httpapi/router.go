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

	// ReviewQueue and ReviewItems serve the internal reviewer surface's read side, and
	// Review performs its decisions. All three are optional: without them the
	// /internal/review routes are not registered at all.
	ReviewQueue *application.ListReviewQueue
	ReviewItems *application.GetReviewItem
	Review      *application.DecideReviewItem
	// ReviewAPIEnabled must be true, in addition to the three dependencies above being
	// present, before any /internal route exists. Two switches on this server, because
	// this surface performs unauthenticated writes and a single accidental wiring
	// should not be enough to expose it.
	//
	// They are not the only two that bear on this surface. apps/web's
	// FIRMSCOUT_REVIEW_UI_ENABLED gates a read-only viewer of the same data on the
	// public site, and it is set on a different host by a different operator; api.md §11
	// names all three in one table for that reason. See ADR-0021 and its amendment.
	ReviewAPIEnabled bool

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

	reviewQueue *application.ListReviewQueue
	reviewItems *application.GetReviewItem
	review      *application.DecideReviewItem

	// The read-side use cases, built in NewServer over the ports above. Every public
	// read handler that carries a rule -- what "latest" means, how deep a plan's
	// history goes, how a search is bounded -- goes through one of these rather than
	// reaching for a repository, so the rule has one implementation and the adapter
	// keeps to parsing, validating and presenting.
	searchProducts *application.SearchProducts
	getProduct     *application.GetProduct
	getVendor      *application.GetVendor
	getLatest      *application.GetLatestRelease
	listReleases   *application.ListReleases

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

	// Two switches on this server, and both of them have to be on. The flag alone leaves
	// the routes unregistered because there is nothing to serve them with; the
	// dependencies alone leave them unregistered because an operator has not said the
	// surface may exist. A single mistaken wiring on this host is therefore not enough
	// to publish an unauthenticated write endpoint.
	//
	// What that does not cover, and what reading the count as complete would have cost
	// the first real deployment: a first-party client on another host that calls these
	// endpoints on a visitor's behalf needs no switch here at all. It never reached one
	// -- nothing has ever been deployed -- and the client's write path is gone. See
	// api.md §11 for all three switches, and ADR-0021.
	if d.ReviewAPIEnabled && d.ReviewQueue != nil && d.ReviewItems != nil && d.Review != nil {
		s.reviewQueue, s.reviewItems, s.review = d.ReviewQueue, d.ReviewItems, d.Review
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

	s.buildQueries()
	s.handler = s.buildHandler()
	return s, nil
}

// buildQueries assembles the read-side use cases over the ports this adapter was given.
//
// The QueryDeps is built here rather than accepted from the composition root, and that
// is the whole point: there is then no wiring anyone can forget. A Deps that satisfies
// NewServer necessarily produces working use cases, so "the read path was not wired"
// cannot be a deployment state. The two ports the composition root would otherwise
// supply are supplied here instead, because this adapter needs different behaviour from
// them than a CLI or a worker does:
//
//   - Events is the background sink, so an analytics write never lands on a customer's
//     latency and never fails a read. A synchronous publisher would put a log write --
//     later a queue write -- inside the request.
//   - Clock is the server's, which is the injected one in a test and the wall clock in
//     production, so a use case's idea of "now" and a middleware's cannot differ.
//
// QueryDeps.Products is left nil deliberately: no read use case this adapter calls
// touches the product repository (they read the precomputed summary projection, ADR-0013),
// and passing a port nothing uses would imply this surface can reach further than it can.
func (s *Server) buildQueries() {
	deps := application.QueryDeps{
		Summaries: s.summaries,
		Vendors:   s.vendors,
		Releases:  s.releases,
		Events:    backgroundEvents{s},
		Clock:     serverClock{s},
	}
	s.searchProducts = application.NewSearchProducts(deps)
	s.getProduct = application.NewGetProduct(deps)
	s.getVendor = application.NewGetVendor(deps)
	s.getLatest = application.NewGetLatestRelease(deps)
	s.listReleases = application.NewListReleases(deps)
}

// backgroundEvents adapts the after-response worker to application.EventPublisher, so a
// use case can publish without knowing that this adapter defers the write.
//
// It never reports failure. The alternative -- returning the publisher's error -- would
// turn a lost analytics event into a failed product page, and a search that answered
// correctly but 500ed because a counter could not be written is the wrong trade. A
// dropped event is logged where it is dropped.
type backgroundEvents struct{ srv *Server }

// Publish queues the events. The context is the request's and is deliberately unused:
// the work runs after the response, by which time that context is cancelled.
func (e backgroundEvents) Publish(_ context.Context, events ...domain.Event) error {
	e.srv.publish(events...)
	return nil
}

// serverClock is the server's clock as an application.Clock, so an injected test clock
// reaches the use cases too.
type serverClock struct{ srv *Server }

func (c serverClock) Now() time.Time { return c.srv.now() }

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

	// cacheAuthenticated replaces the route's policy on any response to a caller who
	// presented a credential, whatever the route says. "private" is the intent and
	// "no-store" is the instruction, because a CDN that ignores one may still honour
	// the other; the reasoning is in CacheHeaders and in security.md §4.10 (T-13).
	cacheAuthenticated = "private, no-store"
)

// apiRoutes is the public, versioned surface. It is a method rather than a literal
// inside buildHandler so a contract test can walk it and compare it with
// docs/api/openapi.yaml in both directions: a handler with no OpenAPI entry, and an
// OpenAPI entry with no handler, are both failures (api.md §10).
func (s *Server) apiRoutes() []route {
	return []route{
		{"GET /api/v1/search", ClassSearch, cacheSearch, s.handleSearch},
		{"GET /api/v1/vendors", ClassList, cacheCatalogue, s.handleListVendors},
		{"GET /api/v1/vendors/{slug}", ClassDetail, cacheCatalogue, s.handleGetVendor},
		{"GET /api/v1/products/{slug}", ClassDetail, cacheCatalogue, s.handleGetProduct},
		{"GET /api/v1/products/{slug}/releases", ClassList, cacheReleases, s.handleListReleases},
		{"GET /api/v1/products/{slug}/latest", ClassDetail, cacheReleases, s.handleGetLatest},
		{"GET /api/v1/releases/{id}", ClassDetail, cacheImmutable, s.handleGetRelease},
	}
}

// systemRoutes are the infrastructure endpoints. They sit outside /api/v1 because they
// are not part of the versioned contract, and they are documented in openapi.yaml with
// a server override that says so.
func (s *Server) systemRoutes() []route {
	return []route{
		{"GET /healthz", ClassSystem, cacheNone, s.handleHealthz},
		{"GET /readyz", ClassSystem, cacheNone, s.handleReadyz},
		{"GET /metrics", ClassSystem, cacheNone, s.handleMetrics},
	}
}

// internalRoutes is the reviewer surface, and it is empty unless both of this server's switches in
// NewServer were on. Returning nothing rather than registering routes that answer 403
// is the point: a path that does not exist cannot be probed for its error message, and
// the catch-all answers it with the same 404 problem document any other unrouted path
// gets.
//
// Every one of these carries cacheNone and none of them passes through CacheHeaders, so
// no ETag is issued and nothing between here and the reviewer may store a moderation
// queue. They are also outside the APIKey, Usage and Quota middlewares: there is no
// credential to resolve and nothing to bill, and metering an internal surface against a
// customer's quota would be charging them for work they did not ask for.
func (s *Server) internalRoutes() []route {
	if s.reviewQueue == nil || s.reviewItems == nil || s.review == nil {
		return nil
	}
	return []route{
		{"GET /internal/review/items", ClassList, cacheNone, s.handleListReviewItems},
		{"GET /internal/review/items/{id}", ClassDetail, cacheNone, s.handleGetReviewItem},
		{"POST /internal/review/items/{id}/accept", ClassDetail, cacheNone, s.handleAcceptReviewItem},
		{"POST /internal/review/items/{id}/reject", ClassDetail, cacheNone, s.handleRejectReviewItem},
	}
}

func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()

	for _, rt := range s.apiRoutes() {
		mux.Handle(rt.pattern, s.apiChain(rt))
	}
	for _, rt := range s.systemRoutes() {
		mux.Handle(rt.pattern, s.systemChain(rt))
	}
	for _, rt := range s.internalRoutes() {
		mux.Handle(rt.pattern, s.internalChain(rt))
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

// internalChain is the per-route middleware for the reviewer surface.
//
// It is deliberately the shortest chain in the file. No APIKey: there is no credential
// on this surface, and running the middleware would answer 401 to a reviewer who
// happened to have a key in their client. No Usage and no Quota: nothing here is
// billable, and an internal decision must not consume a customer's allowance. No
// CacheHeaders: a moderation queue gets no validator and no shared cache entry.
//
// RateLimit stays, because an unauthenticated endpoint that publishes releases is worth
// bounding even when it is only reachable from a private network.
func (s *Server) internalChain(rt route) http.Handler {
	var h http.Handler = rt.handler
	h = s.RateLimit(h)
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
