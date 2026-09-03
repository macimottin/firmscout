package telemetry

import (
	"context"
	"io"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Noop returns a Provider that exports nothing, needs no collector, opens no socket
// and starts no goroutine.
//
// It exists so a test -- or a one-shot CLI invocation where a batch exporter's
// background goroutine would outlive the useful life of the process -- can be handed a
// real *Provider rather than a nil one, and so call sites need no "if telemetry !=
// nil" branch that would then be untested in production.
//
// Its Metrics() still satisfies the full application.Metrics port, so a handler that
// records a measurement under test exercises exactly the code path it will run in
// production; only the destination differs.
func Noop() *Provider {
	reg := prometheus.NewRegistry()
	mp := metricnoop.NewMeterProvider()
	m, err := NewMeter(mp.Meter(ScopeName))
	if err != nil {
		// Unreachable: the no-op meter validates nothing and returns no error.
		m = nil
	}
	return &Provider{
		cfg:      Config{ServiceName: ServiceAPI, Logger: slog.New(discardHandler{})},
		tracerP:  tracenoop.NewTracerProvider(),
		meterP:   mp,
		prop:     propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
		gatherer: reg,
		metrics:  m,
	}
}

// NoopLogger returns a logger that discards everything. Handy in tests that assert on
// behaviour rather than on log output.
func NoopLogger() *slog.Logger { return slog.New(discardHandler{}) }

// DiscardHandler returns a slog.Handler that drops every record.
func DiscardHandler() slog.Handler { return discardHandler{} }

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (discardHandler) WithAttrs([]slog.Attr) slog.Handler        { return discardHandler{} }
func (discardHandler) WithGroup(string) slog.Handler             { return discardHandler{} }

// JSONLogger builds a correlation-stamped JSON logger without a Provider, for a
// process that wants FirmScout's log shape before (or without) telemetry set-up.
func JSONLogger(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(NewCorrelationHandler(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})))
}
