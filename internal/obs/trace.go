package obs

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// ShutdownFunc flushes and stops the tracer provider.
type ShutdownFunc func(context.Context) error

// SetupTracing installs the global tracer provider and propagator: OTLP
// over HTTP to endpoint when it is set, a no-op otherwise. It reports
// whether tracing is on, so the start line says so rather than leaving
// it to be guessed. The ShutdownFunc restores the previous global
// provider (E-11: tests restore global state).
func SetupTracing(ctx context.Context, endpoint, service string) (bool, ShutdownFunc, error) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	restore := func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	}
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if endpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return false, func(context.Context) error { restore(); return nil }, nil
	}
	// The exporter logs and ignores a malformed URL; refuse it here.
	if u, err := url.Parse(endpoint); err != nil || u.Scheme == "" || u.Host == "" {
		restore()
		return false, nil, errors.New("otlp exporter: USSP_OTLP_URL must be an absolute URL")
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		restore()
		return false, nil, fmt.Errorf("otlp exporter: %w", err)
	}
	res := resource.NewSchemaless(
		attribute.String("service.name", service),
		attribute.String("service.version", Version),
	)
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	return true, func(ctx context.Context) error {
		defer restore()
		return tp.Shutdown(ctx)
	}, nil
}
