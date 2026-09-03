package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/macimottin/firmscout/internal/application"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	reg := prometheus.NewRegistry()
	return Config{
		ServiceName:          ServiceAPI,
		ServiceVersion:       "a1b2c3d",
		Environment:          "test",
		PrometheusRegisterer: reg,
		PrometheusGatherer:   reg,
		Logger:               NoopLogger(),
	}
}

// A missing collector is an operational inconvenience, never a reason the API cannot
// start. This is the single most important behaviour in the package: a self-hoster who
// has not deployed an OTel collector must still get a working FirmScout.
func TestSetupSucceedsWithNoCollectorConfigured(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	ctx := context.Background()
	shutdown, err := Setup(ctx, testConfig(t))
	if err != nil {
		t.Fatalf("Setup with no OTLP endpoint returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
}

func TestProviderRunsWithTracingDisabledWhenNoEndpointIsSet(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	p, err := New(context.Background(), testConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	if p.TracingEnabled() {
		t.Error("tracing reported as enabled with no endpoint configured")
	}
	// Metrics still work: the Prometheus reader needs no network at all.
	if p.Gatherer() == nil {
		t.Fatal("no Prometheus gatherer even though the exporter needs no collector")
	}
	p.Metrics().Counter(context.Background(), application.MetricAPIRequestsTotal, 1)

	// Starting a span through the no-op provider must not panic and must not record.
	_, span := p.TracerProvider().Tracer("t").Start(context.Background(), "noop")
	span.End()
}

// An unreachable collector is also not fatal: the gRPC exporter dials lazily, so the
// process starts now and connects when (if) the collector appears.
func TestAnUnreachableCollectorIsNotFatal(t *testing.T) {
	cfg := testConfig(t)
	// Port 1 is reserved and nothing listens on it.
	cfg.OTLPEndpoint = "127.0.0.1:1"
	cfg.OTLPInsecure = true
	cfg.MetricInterval = time.Hour // never fire during the test
	// Shutdown must be bounded even when the flush cannot reach anyone.
	cfg.ShutdownTimeout = time.Second

	p, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New with an unreachable collector returned an error: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	p.Metrics().Counter(context.Background(), application.MetricAPIRequestsTotal, 1,
		application.A("route", "/api/v1/search"))
	ctx, span := p.TracerProvider().Tracer("t").Start(context.Background(), "source.check")
	span.End()
	_ = ctx
}

func TestSetupRequiresAServiceName(t *testing.T) {
	if _, err := New(context.Background(), Config{Logger: NoopLogger()}); err == nil {
		t.Fatal("expected an error for a config with no service name")
	}
}

// The exposition names are a contract shared with the Grafana dashboards and the
// Prometheus alert rules. A suffixing translation strategy would silently turn
// firmscout_api_requests_total into firmscout_api_requests_total_total and blank every
// panel that queries it, which is a failure nobody notices until an incident.
func TestPrometheusExpositionUsesTheCatalogueNamesVerbatim(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	cfg := testConfig(t)
	p, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	ctx := context.Background()
	m := p.Metrics()
	counters := []string{
		application.MetricAPIRequestsTotal,
		application.MetricAPIRateLimitEvents,
		application.MetricAPIQuotaViolations,
		application.MetricAPICacheResults,
		application.MetricCollectorChecksTotal,
		application.MetricCollectorPublications,
		application.MetricQueueDeadLetterTotal,
	}
	for _, name := range counters {
		m.Counter(ctx, name, 1, application.A("route", "/api/v1/search"))
	}
	histograms := []string{
		application.MetricAPIRequestDuration,
		application.MetricAPIPayloadSize,
		application.MetricCollectorDuration,
	}
	for _, name := range histograms {
		m.Histogram(ctx, name, 0.5, application.A("route", "/api/v1/search"))
	}
	m.Gauge(ctx, application.MetricQueueDepth, 3, application.A("queue_name", "source.check.requested"))

	body := scrape(t, p.Gatherer())

	for _, name := range append(append([]string{}, counters...), histograms...) {
		if !strings.Contains(body, name) {
			t.Errorf("metric %s missing from the exposition", name)
		}
	}
	if !strings.Contains(body, application.MetricQueueDepth) {
		t.Errorf("metric %s missing from the exposition", application.MetricQueueDepth)
	}
	// The failure modes a naming strategy would introduce, asserted by name.
	for _, wrong := range []string{
		"firmscout_api_requests_total_total",
		"firmscout_api_request_duration_seconds_seconds",
		"firmscout_api_payload_size_bytes_bytes",
		"firmscout_queue_depth_messages",
	} {
		if strings.Contains(body, wrong) {
			t.Errorf("exposition contains re-suffixed name %q; the dashboards query the catalogue name", wrong)
		}
	}
}

func TestHistogramBucketsAreExposedForLatency(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	p, err := New(context.Background(), testConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	p.Metrics().Histogram(context.Background(), application.MetricAPIRequestDuration, 0.42,
		application.A("route", "/api/v1/search"), application.A("method", "GET"))

	body := scrape(t, p.Gatherer())
	// histogram_quantile in the alert rules needs the _bucket series.
	if !strings.Contains(body, "firmscout_api_request_duration_seconds_bucket") {
		t.Errorf("no histogram buckets exposed:\n%s", body)
	}
}

func scrape(t *testing.T, g prometheus.Gatherer) string {
	t.Helper()
	w := httptest.NewRecorder()
	promhttp.HandlerFor(g, promhttp.HandlerOpts{}).
		ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("scrape returned %d", w.Code)
	}
	return w.Body.String()
}

func TestResourceCarriesTheServiceIdentity(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	cfg := testConfig(t)
	p, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	// target_info carries the resource attributes into the Prometheus exposition.
	p.Metrics().Counter(context.Background(), application.MetricAPIRequestsTotal, 1)
	body := scrape(t, p.Gatherer())
	for _, want := range []string{"firmscout-api", "a1b2c3d", "test"} {
		if !strings.Contains(body, want) {
			t.Errorf("resource attribute %q not present in the exposition:\n%s", want, body)
		}
	}
}

// Logs and traces have to be pivotable to one another, or an on-call engineer who has
// found the log line still cannot see what the request actually did.
func TestSlogHandlerInjectsTraceAndSpanID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewCorrelationHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	tp := sdktrace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(context.Background()) }()
	ctx, span := tp.Tracer("test").Start(context.Background(), "release.publish")
	ctx = ContextWithRequestID(ctx, "req_01JEXAMPLE")

	logger.InfoContext(ctx, "release published")
	span.End()

	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &record); err != nil {
		t.Fatalf("unmarshal log record: %v; got %s", err, buf.String())
	}
	traceID, _ := record["trace_id"].(string)
	if traceID != span.SpanContext().TraceID().String() {
		t.Errorf("trace_id = %q, want %q", traceID, span.SpanContext().TraceID())
	}
	spanID, _ := record["span_id"].(string)
	if spanID != span.SpanContext().SpanID().String() {
		t.Errorf("span_id = %q, want %q", spanID, span.SpanContext().SpanID())
	}
	if record["request_id"] != "req_01JEXAMPLE" {
		t.Errorf("request_id = %v, want req_01JEXAMPLE", record["request_id"])
	}
}

func TestSlogHandlerOmitsCorrelationWhenThereIsNone(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewCorrelationHandler(slog.NewJSONHandler(&buf, nil)))
	logger.Info("no span here")

	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &record); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"trace_id", "span_id", "request_id"} {
		if _, present := record[key]; present {
			// An empty trace_id is worse than an absent one: it looks like a trace
			// that exists and cannot be found.
			t.Errorf("%s present with no active span: %v", key, record)
		}
	}
}

func TestProviderSlogHandlerCarriesServiceAttributes(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	p, err := New(context.Background(), testConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	var buf bytes.Buffer
	p.Logger(&buf, slog.LevelInfo).Info("hello")

	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &record); err != nil {
		t.Fatalf("unmarshal: %v; got %s", err, buf.String())
	}
	if record["service.name"] != ServiceAPI {
		t.Errorf("service.name = %v, want %s", record["service.name"], ServiceAPI)
	}
	if record["service.version"] != "a1b2c3d" {
		t.Errorf("service.version = %v", record["service.version"])
	}
	if record["deployment.environment.name"] != "test" {
		t.Errorf("deployment.environment.name = %v", record["deployment.environment.name"])
	}
}

func TestNoopProviderRecordsWithoutASideEffect(t *testing.T) {
	p := Noop()
	ctx := context.Background()

	// Metrics() already returns application.Metrics, so the interface conformance is
	// a compile-time fact rather than something to restate here.
	m := p.Metrics()
	m.Counter(ctx, application.MetricAPIRequestsTotal, 1, application.A("route", "/x"))
	m.Histogram(ctx, application.MetricAPIRequestDuration, 1.5)
	m.Gauge(ctx, application.MetricQueueDepth, 7)

	if p.TracingEnabled() {
		t.Error("the no-op provider must not claim tracing is enabled")
	}
	if err := p.Shutdown(ctx); err != nil {
		t.Errorf("shutdown: %v", err)
	}
	// Shutting down twice is safe, because a deferred shutdown and an explicit one
	// during a signal handler both happening is normal.
	if err := p.Shutdown(ctx); err != nil {
		t.Errorf("second shutdown: %v", err)
	}
}

func TestNilMeterAndUnknownNamesAreSafe(t *testing.T) {
	ctx := context.Background()
	var nilMeter *Meter
	nilMeter.Counter(ctx, application.MetricAPIRequestsTotal, 1)
	nilMeter.Histogram(ctx, application.MetricAPIRequestDuration, 1)
	nilMeter.Gauge(ctx, application.MetricQueueDepth, 1)

	m := NopMeter()
	// A name outside the catalogue must not panic. Losing a metric is a small
	// problem; a panic on a metric call is a request that failed for observability's
	// sake, which is exactly backwards.
	m.Counter(ctx, "firmscout_experimental_thing_total", 1)
	m.Histogram(ctx, "firmscout_experimental_duration_seconds", 1)
	m.Gauge(ctx, "firmscout_experimental_depth", 1)
}

func TestMeterSatisfiesThePort(t *testing.T) {
	var _ application.Metrics = (*Meter)(nil)
	var _ application.Metrics = NopMeter()
	var _ application.Metrics = application.NopMetrics{}
}

func TestStripScheme(t *testing.T) {
	cases := map[string]string{
		"localhost:4317":             "localhost:4317",
		"http://localhost:4317":      "localhost:4317",
		"https://collector.internal": "collector.internal",
		"grpc://collector.svc:4317":  "collector.svc:4317",
		"":                           "",
		"http://":                    "http://",
	}
	for in, want := range cases {
		if got := stripScheme(in); got != want {
			t.Errorf("stripScheme(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConcurrentRecordingIsSafe(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	p, err := New(context.Background(), testConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	m := p.Metrics()
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			ctx := context.Background()
			for j := 0; j < 50; j++ {
				m.Counter(ctx, application.MetricAPIRequestsTotal, 1, application.A("route", "/x"))
				// A name outside the catalogue exercises the lazy-creation path,
				// which is where a data race on the instrument cache would live.
				m.Histogram(ctx, "firmscout_test_lazy_seconds", float64(j))
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
