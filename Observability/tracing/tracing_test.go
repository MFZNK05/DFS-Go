package tracing_test

import (
	"context"
	"testing"

	"github.com/Faizan2005/DFS-Go/Observability/tracing"
	"go.opentelemetry.io/otel/trace"
)

// ---------------------------------------------------------------------------
// Init — no-op mode (empty endpoint)
// ---------------------------------------------------------------------------

func TestInitNoopDoesNotError(t *testing.T) {
	shutdown, err := tracing.Init("dfs-test", "")
	if err != nil {
		t.Fatalf("Init with empty endpoint should not error, got: %v", err)
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown function")
	}
	// Shutdown must not error.
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// StartSpan
// ---------------------------------------------------------------------------

func TestStartSpanReturnsSpan(t *testing.T) {
	tracing.Init("dfs-test", "")

	ctx := context.Background()
	ctx2, span := tracing.StartSpan(ctx, "TestOp")
	defer span.End()

	if ctx2 == nil {
		t.Fatal("expected non-nil context from StartSpan")
	}
	if span == nil {
		t.Fatal("expected non-nil span")
	}
}

func TestStartSpanContextCarriesSpan(t *testing.T) {
	tracing.Init("dfs-test", "")

	ctx, span := tracing.StartSpan(context.Background(), "TestOp")
	defer span.End()

	got := tracing.SpanFromContext(ctx)
	// In no-op mode both are non-nil no-op spans; they share the same interface.
	if got == nil {
		t.Fatal("SpanFromContext returned nil")
	}
}

// ---------------------------------------------------------------------------
// SpanFromContext
// ---------------------------------------------------------------------------

func TestSpanFromEmptyContextIsNoOp(t *testing.T) {
	tracing.Init("dfs-test", "")
	span := tracing.SpanFromContext(context.Background())
	// A no-op span is valid but not recording.
	if span == nil {
		t.Fatal("expected non-nil no-op span from empty context")
	}
}

// ---------------------------------------------------------------------------
// InjectToMap / ExtractFromMap — context propagation round-trip
// ---------------------------------------------------------------------------

func TestInjectExtractRoundTrip(t *testing.T) {
	tracing.Init("dfs-test", "")

	// Create a span and inject its context into a carrier map.
	ctx, span := tracing.StartSpan(context.Background(), "sender")
	defer span.End()

	carrier := map[string]string{}
	tracing.InjectToMap(ctx, carrier)

	// Extract on the receiver side.
	receiverCtx := tracing.ExtractFromMap(context.Background(), carrier)
	if receiverCtx == nil {
		t.Fatal("ExtractFromMap returned nil context")
	}

	// In no-op mode the span context is empty (no real IDs to propagate);
	// we just verify the call chain doesn't panic.
	_ = tracing.SpanFromContext(receiverCtx)
}

func TestInjectEmptyCarrierDoesNotPanic(t *testing.T) {
	tracing.Init("dfs-test", "")
	// Injecting an empty (no span) context should be a no-op.
	carrier := map[string]string{}
	tracing.InjectToMap(context.Background(), carrier)
}

func TestExtractEmptyCarrierDoesNotPanic(t *testing.T) {
	tracing.Init("dfs-test", "")
	ctx := tracing.ExtractFromMap(context.Background(), map[string]string{})
	if ctx == nil {
		t.Fatal("expected non-nil context")
	}
}

// ---------------------------------------------------------------------------
// AddEvent / RecordError
// ---------------------------------------------------------------------------

func TestAddEventDoesNotPanic(t *testing.T) {
	tracing.Init("dfs-test", "")
	ctx, span := tracing.StartSpan(context.Background(), "EventTest")
	defer span.End()
	// Must not panic.
	tracing.AddEvent(ctx, "file-stored", "key", "notes.pdf")
}

func TestRecordErrorNilDoesNotPanic(t *testing.T) {
	tracing.Init("dfs-test", "")
	ctx, span := tracing.StartSpan(context.Background(), "ErrTest")
	defer span.End()
	tracing.RecordError(ctx, nil)
}

func TestRecordErrorNonNilDoesNotPanic(t *testing.T) {
	tracing.Init("dfs-test", "")
	ctx, span := tracing.StartSpan(context.Background(), "ErrTest")
	defer span.End()
	tracing.RecordError(ctx, fmt_errorString("simulated error"))
}

// ---------------------------------------------------------------------------
// Nested spans inherit parent
// ---------------------------------------------------------------------------

func TestNestedSpanParentContext(t *testing.T) {
	tracing.Init("dfs-test", "")

	parentCtx, parentSpan := tracing.StartSpan(context.Background(), "parent")
	defer parentSpan.End()

	childCtx, childSpan := tracing.StartSpan(parentCtx, "child")
	defer childSpan.End()

	// In no-op mode both spans report the same invalid SpanContext.
	// The important thing is no panic and child context is derived from parent.
	if childCtx == nil {
		t.Fatal("child context is nil")
	}
}

// ---------------------------------------------------------------------------
// Span interface compliance
// ---------------------------------------------------------------------------

func TestStartSpanReturnsSatisfiesInterface(t *testing.T) {
	tracing.Init("dfs-test", "")
	_, span := tracing.StartSpan(context.Background(), "InterfaceCheck")
	defer span.End()
	// Compile-time: span implements trace.Span.
	var _ trace.Span = span
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type fmt_errorString string

func (e fmt_errorString) Error() string { return string(e) }
