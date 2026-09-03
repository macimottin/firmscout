package telemetry

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"

	"github.com/macimottin/firmscout/internal/application"
)

// ScopeName is the instrumentation scope every FirmScout instrument is created under.
const ScopeName = "github.com/macimottin/firmscout"

// Meter is the OpenTelemetry implementation of the application.Metrics port.
//
// The port speaks in metric *names* rather than in typed instrument handles, which is
// what lets the application and every adapter record measurements without any of them
// importing an OpenTelemetry package. This type is where a name becomes a real
// instrument, exactly once, and where the units and descriptions from the catalogue in
// docs/architecture/observability.md §5 are attached.
//
// Instruments are cached and created lazily. The named catalogue is pre-created at
// construction so a freshly started process already exposes every metric a dashboard
// queries, rather than materialising each one only after the first request that
// happens to touch it.
type Meter struct {
	meter metric.Meter

	mu         sync.RWMutex
	counters   map[string]metric.Int64Counter
	histograms map[string]metric.Float64Histogram
	gauges     map[string]metric.Int64Gauge
}

var _ application.Metrics = (*Meter)(nil)

// instrumentKind describes how a catalogued metric is recorded.
type instrumentKind int

const (
	kindCounter instrumentKind = iota
	kindHistogram
	kindGauge
)

// instrumentSpec is the unit and description the exposition carries for one metric.
type instrumentSpec struct {
	kind    instrumentKind
	unit    string
	desc    string
	buckets []float64
}

// catalogue is the metric contract, keyed by the names declared in
// internal/application. Recording a name that is not here still works -- it becomes a
// counter or histogram with no unit -- but every name a dashboard queries should be
// listed, because the unit and the histogram buckets are part of what makes a panel
// readable.
var catalogue = map[string]instrumentSpec{
	application.MetricAPIRequestsTotal: {kindCounter, "{request}", "HTTP requests served by the public API.", nil},
	application.MetricAPIRequestDuration: {kindHistogram, "s", "End-to-end duration of an API request.",
		[]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}},
	application.MetricAPIPayloadSize: {kindHistogram, "By", "Request and response payload sizes.",
		[]float64{128, 512, 2048, 8192, 32768, 131072, 524288, 2097152}},
	application.MetricAPIRateLimitEvents: {kindCounter, "{event}", "Requests throttled by the in-process token bucket.", nil},
	application.MetricAPIQuotaViolations: {kindCounter, "{violation}", "Requests rejected for exceeding a persisted monthly quota.", nil},
	application.MetricAPICacheResults:    {kindCounter, "{result}", "Conditional-request cache hits and misses.", nil},

	application.MetricCollectorChecksTotal:    {kindCounter, "{check}", "Source checks by outcome.", nil},
	application.MetricCollectorExtractionFail: {kindCounter, "{failure}", "Extraction stage failures.", nil},
	application.MetricCollectorValidationFail: {kindCounter, "{failure}", "Validation gate failures.", nil},
	application.MetricCollectorPublications:   {kindCounter, "{publication}", "Successful publications.", nil},
	application.MetricCollectorDuplicates:     {kindCounter, "{candidate}", "Candidates rejected as exact duplicates.", nil},
	application.MetricCollectorDuration:       {kindHistogram, "s", "Per-stage collector execution time.", nil},

	application.MetricQueueDepth:            {kindGauge, "{message}", "Jobs pending or processing.", nil},
	application.MetricQueueOldestMessageAge: {kindGauge, "s", "Age of the oldest undelivered message.", nil},
	application.MetricQueueDeadLetterTotal:  {kindCounter, "{message}", "Jobs permanently abandoned.", nil},

	application.MetricAIAgentExecutions: {kindCounter, "{execution}", "AI agent executions by outcome.", nil},
	application.MetricAITokensTotal:     {kindCounter, "{token}", "Tokens consumed by AI agents.", nil},
	application.MetricAICostUSDTotal:    {kindCounter, "usd", "AI spend.", nil},
}

// NewMeter creates the OpenTelemetry-backed metrics port and pre-creates every
// catalogued instrument.
func NewMeter(m metric.Meter) (*Meter, error) {
	out := &Meter{
		meter:      m,
		counters:   make(map[string]metric.Int64Counter),
		histograms: make(map[string]metric.Float64Histogram),
		gauges:     make(map[string]metric.Int64Gauge),
	}
	for name, spec := range catalogue {
		var err error
		switch spec.kind {
		case kindCounter:
			_, err = out.counter(name)
		case kindHistogram:
			_, err = out.histogram(name)
		case kindGauge:
			_, err = out.gauge(name)
		}
		if err != nil {
			return nil, fmt.Errorf("telemetry: create instrument %s: %w", name, err)
		}
	}
	return out, nil
}

// NopMeter returns a Meter whose instruments discard every measurement, so a call site
// under test exercises the same code path it will run in production and only the
// destination differs.
func NopMeter() *Meter {
	m, err := NewMeter(metricnoop.NewMeterProvider().Meter(ScopeName))
	if err != nil {
		// The no-op meter validates nothing and cannot fail.
		return nil
	}
	return m
}

// Counter increments a named counter.
func (m *Meter) Counter(ctx context.Context, name string, n int64, attrs ...application.Attr) {
	if m == nil {
		return
	}
	c, err := m.counter(name)
	if err != nil {
		return
	}
	c.Add(ctx, n, metric.WithAttributes(convert(attrs)...))
}

// Histogram records one observation.
func (m *Meter) Histogram(ctx context.Context, name string, value float64, attrs ...application.Attr) {
	if m == nil {
		return
	}
	h, err := m.histogram(name)
	if err != nil {
		return
	}
	h.Record(ctx, value, metric.WithAttributes(convert(attrs)...))
}

// Gauge records the current value of something that goes up and down.
func (m *Meter) Gauge(ctx context.Context, name string, value int64, attrs ...application.Attr) {
	if m == nil {
		return
	}
	g, err := m.gauge(name)
	if err != nil {
		return
	}
	g.Record(ctx, value, metric.WithAttributes(convert(attrs)...))
}

func (m *Meter) counter(name string) (metric.Int64Counter, error) {
	m.mu.RLock()
	c, ok := m.counters[name]
	m.mu.RUnlock()
	if ok {
		return c, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.counters[name]; ok {
		return c, nil
	}
	spec := catalogue[name]
	c, err := m.meter.Int64Counter(name, metric.WithUnit(spec.unit), metric.WithDescription(spec.desc))
	if err != nil {
		return nil, err
	}
	m.counters[name] = c
	return c, nil
}

func (m *Meter) histogram(name string) (metric.Float64Histogram, error) {
	m.mu.RLock()
	h, ok := m.histograms[name]
	m.mu.RUnlock()
	if ok {
		return h, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.histograms[name]; ok {
		return h, nil
	}
	spec := catalogue[name]
	opts := []metric.Float64HistogramOption{
		metric.WithUnit(spec.unit),
		metric.WithDescription(spec.desc),
	}
	if len(spec.buckets) > 0 {
		opts = append(opts, metric.WithExplicitBucketBoundaries(spec.buckets...))
	}
	h, err := m.meter.Float64Histogram(name, opts...)
	if err != nil {
		return nil, err
	}
	m.histograms[name] = h
	return h, nil
}

func (m *Meter) gauge(name string) (metric.Int64Gauge, error) {
	m.mu.RLock()
	g, ok := m.gauges[name]
	m.mu.RUnlock()
	if ok {
		return g, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if g, ok := m.gauges[name]; ok {
		return g, nil
	}
	spec := catalogue[name]
	g, err := m.meter.Int64Gauge(name, metric.WithUnit(spec.unit), metric.WithDescription(spec.desc))
	if err != nil {
		return nil, err
	}
	m.gauges[name] = g
	return g, nil
}

// convert maps the port's plain string attributes onto OpenTelemetry key-values.
//
// This is the only place the conversion happens, and it is deliberately dumb: it does
// not enrich, derive or infer an attribute. Whatever the caller decided was a safe,
// low-cardinality dimension is what gets recorded, so the cardinality budget is
// enforced where the labels are chosen rather than hidden behind a helper here.
func convert(attrs []application.Attr) []attribute.KeyValue {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, attribute.String(a.Key, a.Value))
	}
	return out
}
