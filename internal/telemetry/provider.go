package telemetry

import (
	"context"
	"fmt"
	"io"
	"os"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// tracerName identifies Relay's instrumentation scope.
const tracerName = "github.com/diegoaleyvag/relay"

// Mode selects where spans go.
type Mode string

const (
	// ModeOff creates spans but exports nothing (near-zero overhead).
	ModeOff Mode = "off"
	// ModeStdout exports pretty JSON spans to the configured Writer (or stderr).
	ModeStdout Mode = "stdout"
)

// Config configures the TracerProvider.
type Config struct {
	Mode           Mode
	Writer         io.Writer // stdout mode target; defaults to os.Stderr
	ServiceVersion string
}

// Provider bundles a TracerProvider and a scoped Tracer.
type Provider struct {
	tp     *sdktrace.TracerProvider
	Tracer trace.Tracer
}

// NewProvider builds a Provider. The returned shutdown flushes and stops it.
func NewProvider(_ context.Context, cfg Config) (*Provider, func(context.Context) error, error) {
	res := resource.NewSchemaless(
		attribute.String("service.name", "relay"),
		attribute.String("service.version", nonEmpty(cfg.ServiceVersion, "0.1.0")),
	)

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	}

	if cfg.Mode == ModeStdout {
		w := cfg.Writer
		if w == nil {
			w = os.Stderr
		}
		exp, err := stdouttrace.New(stdouttrace.WithWriter(w), stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, nil, fmt.Errorf("telemetry: stdout exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	p := &Provider{tp: tp, Tracer: tp.Tracer(tracerName)}
	return p, tp.Shutdown, nil
}

// NewWithExporter builds a Provider around a caller-supplied exporter, exporting
// synchronously. Used by tests to record spans.
func NewWithExporter(exp sdktrace.SpanExporter) *Provider {
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	return &Provider{tp: tp, Tracer: tp.Tracer(tracerName)}
}

// Shutdown flushes and stops the provider.
func (p *Provider) Shutdown(ctx context.Context) error { return p.tp.Shutdown(ctx) }

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
