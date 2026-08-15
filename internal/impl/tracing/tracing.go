// Package tracing wires Design §3.10 / Plan Phase 7's optional OpenTelemetry
// tracing: an agent.Middleware built from MAF-Go's own
// provider/otelprovider, activated only when a caller explicitly asks for it
// (see Setup). This is deliberately not part of impl/harness or
// domain/services' default wiring — Design §3.10 is explicit that tracing is
// "not required for v1 usage," so the entire SDK/exporter/TracerProvider
// setup this package does only runs when Setup is called with Config.Enabled
// true.
package tracing

import (
	"cmp"
	"context"
	"fmt"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/provider/otelprovider"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Config configures Setup.
type Config struct {
	// Enabled turns tracing on. The zero value (false) is the default for a
	// reason: Setup does nothing at all in that case — no OTel SDK is
	// initialized, no exporter is built, no process-wide TracerProvider is
	// registered (otel's built-in no-op implementation stays in effect) —
	// matching Design §3.10's "not required for v1 usage" and Plan Phase 7's
	// "zero overhead and zero new behavior" when unset.
	Enabled bool

	// ServiceName sets the exported "service.name" resource attribute.
	// Empty defaults to "canopy".
	ServiceName string
}

// Setup wires OpenTelemetry tracing per cfg. Callers should unconditionally
// `defer shutdown(ctx)` (with their own bounded ctx — see below) regardless
// of whether tracing ended up enabled: when cfg.Enabled is false, Setup
// returns a nil middleware and a no-op shutdown, so there is nothing for the
// caller to special-case.
//
// # Genuinely optional, by construction
//
// When cfg.Enabled is true, Setup builds an OTLP/HTTP span exporter via
// otlptracehttp.New with no explicit endpoint option, so the exporter falls
// back to the OTel SDK's own standard environment variables
// (OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_TRACES_ENDPOINT,
// OTEL_EXPORTER_OTLP_HEADERS, etc. — see otlptracehttp's own doc comment),
// defaulting to http://localhost:4318 when none is set, matching the OTel
// SDK spec's own default rather than Canopy inventing its own convention.
//
// Critically, otlptracehttp.New does not dial or otherwise contact the
// collector — the exporter issues one HTTP request per batch, lazily, and
// only once a span is actually exported — so a missing/unreachable collector
// cannot make Setup (and therefore Canopy's startup) fail or hang. Once
// running, spans are exported asynchronously by a background batch
// processor (sdktrace.WithBatcher); a delivery failure is reported to
// otel's internal error handler (logged, not propagated) and never blocks
// or fails an agent run. The one place an unreachable collector can still
// cost wall-clock time is the returned shutdown func, which flushes
// in-flight spans — callers should invoke it with a short-timeout context
// (cmd/canopy's main.go uses a few seconds) rather than context.Background,
// so process shutdown can't hang indefinitely on a dead collector either.
func Setup(ctx context.Context, cfg Config) (agent.Middleware, func(context.Context) error, error) {
	noopShutdown := func(context.Context) error { return nil }
	if !cfg.Enabled {
		return nil, noopShutdown, nil
	}

	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, noopShutdown, fmt.Errorf("tracing: building OTLP/HTTP exporter: %w", err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", cmp.Or(cfg.ServiceName, "canopy")),
	))
	if err != nil {
		return nil, noopShutdown, fmt.Errorf("tracing: building resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	mw := otelprovider.NewMiddleware(otelprovider.MiddlewareConfig{SourceName: "canopy"})
	return mw, tp.Shutdown, nil
}
