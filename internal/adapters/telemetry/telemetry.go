// Package telemetry wires OpenTelemetry for every FirmScout binary: a trace provider
// exporting over OTLP/gRPC, a meter provider exporting over both OTLP and Prometheus,
// W3C trace-context propagation, the named instruments of the metric catalogue in
// docs/architecture/observability.md §5, and a slog handler that stamps trace_id and
// span_id onto every record so logs and traces pivot to one another.
//
// # Graceful degradation
//
// Telemetry is an operational convenience, never a prerequisite for serving traffic.
// If no OTLP endpoint is configured, or the configured one is unreachable, Setup
// succeeds, logs a warning once, and the process runs with tracing disabled and
// metrics still available on /metrics through the Prometheus exporter, which needs no
// network at all. A missing collector must never be the reason a self-hoster's API is
// down.
//
// # No globals for instruments
//
// Setup installs the OpenTelemetry global providers because third-party
// instrumentation (otelhttp, the pgx tracer) resolves them that way, but FirmScout's
// own instruments live behind the application.Metrics port, handed to the packages
// that record through it. That is what lets two tests in the same binary have independent
// telemetry, and what keeps "which metrics does this package emit" answerable by
// reading its dependencies rather than grepping for a global.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/otlptranslator"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/macimottin/firmscout/internal/application"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
)

// Service names. observability.md §3 fixes these three; a resource carrying anything
// else will not match the dashboards' service filters.
const (
	ServiceAPI    = "firmscout-api"
	ServiceWorker = "firmscout-worker"
	ServiceCLI    = "firmscout-cli"
)

// Config describes one process's telemetry.
//
// Every field has a usable zero value except ServiceName: a resource without a
// service name produces signals nobody can attribute, so it is the one required
// input.
type Config struct {
	// ServiceName is one of ServiceAPI, ServiceWorker or ServiceCLI.
	ServiceName string
	// ServiceVersion is the build SHA or release tag.
	ServiceVersion string
	// Environment populates deployment.environment.name ("production", "staging",
	// "development").
	Environment string

	// OTLPEndpoint is the collector's gRPC address ("localhost:4317"). Empty means
	// "no collector": tracing is disabled and metrics are Prometheus-only. When
	// empty, OTEL_EXPORTER_OTLP_ENDPOINT is consulted so the standard environment
	// configuration still works.
	OTLPEndpoint string
	// OTLPInsecure disables transport credentials. True for a collector on the
	// local network or in docker compose; false for anything crossing a boundary.
	OTLPInsecure bool
	// OTLPHeaders are added to every export request (for a collector behind an
	// authenticating proxy). Never logged.
	OTLPHeaders map[string]string

	// SampleRatio is the head-based sampling probability. Zero or negative means
	// 1.0, which is the right default for local development; production sets a low
	// value and lets the collector's tail sampling keep the interesting traces
	// (observability.md §7).
	SampleRatio float64

	// MetricInterval is how often the OTLP metric reader exports. Zero means 60s.
	MetricInterval time.Duration
	// ShutdownTimeout bounds the flush on shutdown. Zero means 5s.
	ShutdownTimeout time.Duration

	// PrometheusRegisterer receives the Prometheus exporter's collector. When nil a
	// private registry is created, which is what keeps two providers in one test
	// binary from colliding on duplicate registration.
	PrometheusRegisterer prometheus.Registerer
	// PrometheusGatherer is the gatherer /metrics serves. When nil it is the
	// private registry created above, or the registerer if it happens to be one.
	PrometheusGatherer prometheus.Gatherer

	// Logger receives telemetry's own diagnostics, including the warning that says
	// tracing is disabled. Nil means slog.Default().
	Logger *slog.Logger

	// ResourceAttributes are extra resource attributes (host, region, instance id).
	ResourceAttributes []attribute.KeyValue
}

func (c Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// Provider owns the configured telemetry pipeline. Construct it with New, hand its
// Metrics and Gatherer to the packages that need them, and call Shutdown on exit.
type Provider struct {
	cfg      Config
	res      *resource.Resource
	tracerP  trace.TracerProvider
	meterP   metric.MeterProvider
	prop     propagation.TextMapPropagator
	gatherer prometheus.Gatherer
	metrics  *Meter

	// tracingEnabled reports whether spans actually leave the process. It is false
	// when no OTLP endpoint was configured or the exporter could not be built.
	tracingEnabled bool

	shutdownFns []func(context.Context) error
}

// New builds the telemetry pipeline without touching any OpenTelemetry global.
//
// It returns an error only for a configuration mistake the operator can fix (a
// missing service name, an instrument that cannot be created). A collector that is
// absent or unreachable is not such a mistake: it degrades to tracing-disabled and is
// reported through the logger.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.ServiceName == "" {
		return nil, errors.New("telemetry: ServiceName is required")
	}
	log := cfg.logger()
	switch cfg.ServiceName {
	case ServiceAPI, ServiceWorker, ServiceCLI:
	default:
		// Not fatal: a self-hoster may run a differently named binary. It is worth
		// saying out loud, because the dashboards filter on the three known names.
		log.Warn("telemetry: unrecognised service name; dashboards filter on the documented names",
			slog.String("service.name", cfg.ServiceName))
	}

	res, err := buildResource(cfg)
	if err != nil {
		return nil, err
	}

	p := &Provider{
		cfg:     cfg,
		res:     res,
		tracerP: tracenoop.NewTracerProvider(),
		meterP:  metricnoop.NewMeterProvider(),
		prop: propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{},
		),
	}

	endpoint := cfg.OTLPEndpoint
	if endpoint == "" {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}

	// --- traces -----------------------------------------------------------
	if endpoint == "" {
		log.Info("telemetry: no OTLP endpoint configured, tracing disabled")
	} else if tp, err := p.newTracerProvider(ctx, endpoint); err != nil {
		// Deliberately non-fatal. See the package comment.
		log.Warn("telemetry: OTLP trace exporter unavailable, continuing without tracing",
			slog.String("error", err.Error()))
	} else {
		p.tracerP = tp
		p.tracingEnabled = true
	}

	// --- metrics ----------------------------------------------------------
	// The Prometheus reader is always present so /metrics answers locally even with
	// no collector anywhere in the deployment.
	readers, gatherer, err := p.newMetricReaders(ctx, endpoint, log)
	if err != nil {
		return nil, err
	}
	p.gatherer = gatherer
	opts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	for _, r := range readers {
		opts = append(opts, sdkmetric.WithReader(r))
	}
	mp := sdkmetric.NewMeterProvider(opts...)
	p.meterP = mp
	p.shutdownFns = append(p.shutdownFns, mp.Shutdown)

	m, err := NewMeter(mp.Meter(ScopeName))
	if err != nil {
		return nil, err
	}
	p.metrics = m
	return p, nil
}

// Setup is the one-call wiring for a binary's main function: it builds the pipeline,
// installs it as the OpenTelemetry global providers and propagator so that otelhttp
// and the pgx tracer find it, and returns the shutdown function to defer.
//
// A binary that needs the metrics port or the Prometheus gatherer (the API does,
// because it serves /metrics and records the API counters) should call New instead
// and keep the *Provider.
func Setup(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	p, err := New(ctx, cfg)
	if err != nil {
		return func(context.Context) error { return nil }, err
	}
	p.InstallGlobals()
	return p.Shutdown, nil
}

// InstallGlobals publishes this provider through the OpenTelemetry global accessors.
// Instrumentation libraries resolve their providers that way and cannot be handed one
// explicitly in every case, which is the only reason globals are touched at all.
func (p *Provider) InstallGlobals() {
	otel.SetTracerProvider(p.tracerP)
	otel.SetMeterProvider(p.meterP)
	otel.SetTextMapPropagator(p.prop)
	// An unreachable collector produces a steady trickle of export errors. They are
	// worth seeing, but at DEBUG: at ERROR they would drown the log of a system that
	// is, by design, working fine without a collector.
	log := p.cfg.logger()
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Debug("telemetry: exporter error", slog.String("error", err.Error()))
	}))
}

// Metrics returns the application.Metrics implementation the composition root passes
// to the HTTP adapter, the worker and the use cases. It is never nil.
//
// The return type is the port, not *Meter, because that is what every consumer should
// hold: an adapter that took a *Meter would be depending on this adapter, which the
// dependency rule forbids and internal/archtest enforces.
func (p *Provider) Metrics() application.Metrics {
	if p == nil || p.metrics == nil {
		return application.NopMetrics{}
	}
	return p.metrics
}

// Gatherer returns the Prometheus gatherer that /metrics serves, or nil when the
// exporter could not be registered.
func (p *Provider) Gatherer() prometheus.Gatherer {
	if p == nil {
		return nil
	}
	return p.gatherer
}

// TracerProvider returns the configured trace provider, which is a no-op provider
// when tracing is disabled.
func (p *Provider) TracerProvider() trace.TracerProvider { return p.tracerP }

// MeterProvider returns the configured meter provider.
func (p *Provider) MeterProvider() metric.MeterProvider { return p.meterP }

// Propagator returns the W3C trace-context propagator.
func (p *Provider) Propagator() propagation.TextMapPropagator { return p.prop }

// TracingEnabled reports whether spans are exported anywhere.
func (p *Provider) TracingEnabled() bool { return p != nil && p.tracingEnabled }

// Shutdown flushes and stops every exporter. It is safe to call more than once.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	timeout := p.cfg.ShutdownTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	fns := p.shutdownFns
	p.shutdownFns = nil
	var errs []error
	for i := len(fns) - 1; i >= 0; i-- {
		if err := fns[i](ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// construction helpers
// ---------------------------------------------------------------------------

func buildResource(cfg Config) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{semconv.ServiceName(cfg.ServiceName)}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	if cfg.Environment != "" {
		attrs = append(attrs, semconv.DeploymentEnvironmentNameKey.String(cfg.Environment))
	}
	attrs = append(attrs, cfg.ResourceAttributes...)

	// Merging with the SDK default picks up telemetry.sdk.* and anything set through
	// OTEL_RESOURCE_ATTRIBUTES. The semconv import is pinned to the same version the
	// SDK's own default uses so the two schema URLs agree; a mismatch here is the
	// classic cause of resource.Merge returning ErrSchemaURLConflict.
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL, attrs...))
	if err != nil {
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}
	return res, nil
}

func (p *Provider) newTracerProvider(ctx context.Context, endpoint string) (trace.TracerProvider, error) {
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(stripScheme(endpoint))}
	if p.cfg.OTLPInsecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	if len(p.cfg.OTLPHeaders) > 0 {
		opts = append(opts, otlptracegrpc.WithHeaders(p.cfg.OTLPHeaders))
	}
	// The gRPC exporter dials lazily, so this call does not fail merely because the
	// collector is down -- which is exactly the behaviour the degradation rule wants:
	// start now, connect when the collector appears.
	exp, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, err
	}

	ratio := p.cfg.SampleRatio
	var sampler sdktrace.Sampler
	switch {
	case ratio <= 0 || ratio >= 1:
		sampler = sdktrace.AlwaysSample()
	default:
		sampler = sdktrace.TraceIDRatioBased(ratio)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(p.res),
		sdktrace.WithBatcher(exp),
		sdktrace.WithSampler(sdktrace.ParentBased(sampler)),
	)
	p.shutdownFns = append(p.shutdownFns, tp.Shutdown)
	return tp, nil
}

func (p *Provider) newMetricReaders(ctx context.Context, endpoint string, log *slog.Logger) ([]sdkmetric.Reader, prometheus.Gatherer, error) {
	registerer := p.cfg.PrometheusRegisterer
	gatherer := p.cfg.PrometheusGatherer
	if registerer == nil {
		reg := prometheus.NewRegistry()
		registerer = reg
		if gatherer == nil {
			gatherer = reg
		}
	} else if gatherer == nil {
		if g, ok := registerer.(prometheus.Gatherer); ok {
			gatherer = g
		}
	}

	// NoTranslation keeps the exposition name identical to the instrument name. The
	// catalogue in observability.md §5 already spells the names the Prometheus way
	// (`_total` on counters, `_seconds`/`_bytes` on histograms), so any suffixing
	// strategy would produce `firmscout_api_requests_total_total` and silently break
	// every dashboard query and alert rule that names the metric.
	promExp, err := promexporter.New(
		promexporter.WithRegisterer(registerer),
		promexporter.WithTranslationStrategy(otlptranslator.NoTranslation),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("telemetry: prometheus exporter: %w", err)
	}
	readers := []sdkmetric.Reader{promExp}

	if endpoint != "" {
		opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(stripScheme(endpoint))}
		if p.cfg.OTLPInsecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		if len(p.cfg.OTLPHeaders) > 0 {
			opts = append(opts, otlpmetricgrpc.WithHeaders(p.cfg.OTLPHeaders))
		}
		if exp, err := otlpmetricgrpc.New(ctx, opts...); err != nil {
			log.Warn("telemetry: OTLP metric exporter unavailable, metrics remain available on /metrics",
				slog.String("error", err.Error()))
		} else {
			interval := p.cfg.MetricInterval
			if interval <= 0 {
				interval = 60 * time.Second
			}
			readers = append(readers, sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(interval)))
		}
	}
	return readers, gatherer, nil
}

// stripScheme accepts either "host:4317" or "http://host:4317" for the endpoint,
// because both spellings appear in the wild and otlptracegrpc.WithEndpoint wants the
// bare authority.
func stripScheme(endpoint string) string {
	for _, prefix := range []string{"http://", "https://", "grpc://"} {
		if len(endpoint) > len(prefix) && endpoint[:len(prefix)] == prefix {
			return endpoint[len(prefix):]
		}
	}
	return endpoint
}

// ---------------------------------------------------------------------------
// logging
// ---------------------------------------------------------------------------

// SlogHandler returns a JSON slog handler that writes to w, carries this process's
// service attributes on every record, and injects trace_id, span_id and request_id
// from the record's context.
func (p *Provider) SlogHandler(w io.Writer, level slog.Leveler) slog.Handler {
	base := []slog.Attr{slog.String("service.name", p.cfg.ServiceName)}
	if p.cfg.ServiceVersion != "" {
		base = append(base, slog.String("service.version", p.cfg.ServiceVersion))
	}
	if p.cfg.Environment != "" {
		base = append(base, slog.String("deployment.environment.name", p.cfg.Environment))
	}
	inner := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return NewCorrelationHandler(inner.WithAttrs(base))
}

// Logger returns a *slog.Logger built on SlogHandler.
func (p *Provider) Logger(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(p.SlogHandler(w, level))
}

// correlationHandler stamps the active span's identifiers and the ambient request id
// onto every record.
//
// This is what makes "the customer gave me a request id" a one-step pivot into Tempo:
// the same three identifiers appear on the log line, on the span, and in the response
// header, so none of them has to be re-derived.
type correlationHandler struct{ inner slog.Handler }

// NewCorrelationHandler wraps inner so that every record it handles carries trace_id,
// span_id and request_id when they are present on the record's context.
func NewCorrelationHandler(inner slog.Handler) slog.Handler {
	return &correlationHandler{inner: inner}
}

func (h *correlationHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *correlationHandler) Handle(ctx context.Context, rec slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		rec.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	if id := RequestIDFromContext(ctx); id != "" {
		rec.AddAttrs(slog.String("request_id", id))
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
// request id propagation
// ---------------------------------------------------------------------------

type requestIDKey struct{}

// ContextWithRequestID returns a context carrying the request correlation id.
//
// The key lives here rather than in the HTTP adapter because the request id is a
// correlation concern shared by logs, spans and the queue hop (observability.md §4),
// and a worker reading an id back off a job row needs to put it somewhere the log
// handler will find it without importing the HTTP package.
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

// RequestIDAttributeKey is the span attribute the request id is stamped as.
const RequestIDAttributeKey = attribute.Key("firmscout.request_id")

// AnnotateSpan stamps the request id on the active span, so a trace found from a log
// line can also be found from a request id.
func AnnotateSpan(ctx context.Context, requestID string) {
	if requestID == "" {
		return
	}
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(RequestIDAttributeKey.String(requestID))
	}
}
