// Package tracing wraps the OpenTelemetry SDK to provide distributed tracing
// for DFS-Go operations.
//
// When an OTLP endpoint is configured (e.g. a local Jaeger instance), spans
// are exported in real time. When the endpoint is empty (""), a no-op tracer
// is used — zero overhead, no external dependency required.
//
// Usage:
//
//	shutdown, err := tracing.Init("dfs-node", "http://localhost:4318")
//	if err != nil { log.Fatal(err) }
//	defer shutdown(context.Background())
//
//	// In StoreData:
//	ctx, span := tracing.StartSpan(ctx, "StoreData")
//	defer span.End()
//
// Context propagation across nodes:
//
//	// Sender — inject trace context into the outgoing message map:
//	carrier := map[string]string{}
//	tracing.InjectToMap(ctx, carrier)
//	msg.TraceCtx = carrier
//
//	// Receiver — restore the span context:
//	ctx = tracing.ExtractFromMap(ctx, msg.TraceCtx)
//	ctx, span := tracing.StartSpan(ctx, "handleQuorumWrite")
//	defer span.End()
package tracing

import (
	"context"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/Faizan2005/DFS-Go"

// Init initialises the global OpenTelemetry tracer provider.
//
// serviceName appears in every span as the service.name resource attribute.
// endpoint is an OTLP/HTTP endpoint (e.g. "http://localhost:4318"); when
// empty the no-op tracer is used and no goroutines are started.
//
// Returns a shutdown function that must be called on process exit to flush
// buffered spans. It is safe to call even when the no-op tracer is active.
func Init(serviceName, endpoint string) (shutdown func(context.Context) error, err error) {
	// Silence OTel's internal logger (it captures os.Stderr at init time).
	otel.SetLogger(logr.Discard())

	if endpoint == "" {
		// No-op: just set up propagators. Don't call SetTracerProvider with
		// the default provider — that triggers a spurious warning log.
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))
		return func(_ context.Context) error { return nil }, nil
	}

	// OTLP/HTTP exporter — sends spans to the endpoint in Protobuf format.
	exp, err := otlptracehttp.New(context.Background(),
		otlptracehttp.WithEndpointURL(endpoint),
		otlptracehttp.WithInsecure(), // plain HTTP; use WithTLSClientConfig for TLS
	)
	if err != nil {
		return nil, err
	}

	res, _ := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String(serviceName),
		),
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		// Always-on sampler for development; swap for ParentBased(TraceIDRatioBased(0.1))
		// in production to reduce overhead.
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, // W3C Trace-Context header
		propagation.Baggage{},
	))

	return func(ctx context.Context) error {
		return tp.Shutdown(ctx)
	}, nil
}

// StartSpan creates a new span as a child of any span already in ctx.
// The returned context carries the new span; callers must call span.End().
//
//	ctx, span := tracing.StartSpan(ctx, "StoreData", trace.WithAttributes(...))
//	defer span.End()
func StartSpan(ctx context.Context, spanName string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return otel.Tracer(tracerName).Start(ctx, spanName, opts...)
}

// SpanFromContext returns the current span stored in ctx.
// Returns a no-op span if ctx carries no span.
func SpanFromContext(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}

// mapCarrier adapts map[string]string to OTel's TextMapCarrier interface.
type mapCarrier map[string]string

func (m mapCarrier) Get(key string) string    { return m[key] }
func (m mapCarrier) Set(key, value string)    { m[key] = value }
func (m mapCarrier) Keys() []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// InjectToMap writes the W3C trace-context headers for the span in ctx into
// carrier. Call this before sending an RPC message to propagate the trace.
func InjectToMap(ctx context.Context, carrier map[string]string) {
	otel.GetTextMapPropagator().Inject(ctx, mapCarrier(carrier))
}

// ExtractFromMap restores a span context from carrier (previously populated by
// InjectToMap on the sending side). The returned context is a child of ctx
// with the remote span context attached; call StartSpan on it to continue
// the distributed trace.
func ExtractFromMap(ctx context.Context, carrier map[string]string) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, mapCarrier(carrier))
}

// AddEvent records a structured event on the current span (e.g. "file stored").
// key-value pairs must be alternating string keys and string values.
func AddEvent(ctx context.Context, name string, kvs ...string) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.AddEvent(name) // attrs can be added via StartSpan opts or here with attribute package
}

// RecordError marks the current span as errored and attaches the error.
func RecordError(ctx context.Context, err error) {
	if err == nil {
		return
	}
	trace.SpanFromContext(ctx).RecordError(err)
}
