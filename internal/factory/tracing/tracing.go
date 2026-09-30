// SPDX-License-Identifier: Apache-2.0

// Package tracing gives each factory task one root span (SP3 ruling R46). Its ids are minted
// when the task is accepted and kept in the Task's status; every run of the task parents its
// harness span on them, and the span itself is built from them and exported once, when the task
// ends. A restart or a new leader loses nothing. The span carries ids, the tier, the phase and a
// reason code only, never issue text: it enters the trace router with no allowlist (O-1 M3).
package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// Mint returns a fresh W3C trace id and span id, hex-encoded, neither of them all zeros.
func Mint() (traceID, spanID string) { return mint(rand.Reader) }

// mint redraws an all-zero id: W3C calls it invalid, and runs.Spec refuses it (ruling SI).
func mint(src io.Reader) (traceID, spanID string) {
	var t trace.TraceID
	var s trace.SpanID
	for !t.IsValid() {
		_, _ = io.ReadFull(src, t[:])
	}
	for !s.IsValid() {
		_, _ = io.ReadFull(src, s[:])
	}
	return hex.EncodeToString(t[:]), hex.EncodeToString(s[:])
}

// Traceparent is the W3C header a run's harness parents its root span on (sampled).
func Traceparent(traceID, spanID string) string { return "00-" + traceID + "-" + spanID + "-01" }

// Task is one task's root span as the factory recorded it.
type Task struct {
	TraceID, SpanID, TaskID, Tier, Phase, Reason string
	Start, End                                   time.Time
}

// Sink exports finished task spans; the reconciler holds nil when tracing is off.
type Sink interface {
	Export(ctx context.Context, t Task) error
}

// Exporter turns a recorded Task into its span and exports it.
type Exporter struct{ exp sdktrace.SpanExporter }

// New exports task spans through exp.
func New(exp sdktrace.SpanExporter) *Exporter { return &Exporter{exp: exp} }

// NewOTLP exports over OTLP/gRPC to endpoint (host:port), plaintext: the collector's platform
// port is in-cluster, and the cluster's WireGuard encrypts pod traffic (observability plan O20).
// It dials lazily, so an unreachable collector fails an export, never the factory's start.
func NewOTLP(ctx context.Context, endpoint string) (*Exporter, error) {
	exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(endpoint), otlptracegrpc.WithInsecure())
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}
	return New(exp), nil
}

// reasonRE is a reason code: the reconciler's own, or a broker's run end reason.
var reasonRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// spanReason keeps a reason on the span only as a code; anything else is "other".
func spanReason(r string) string {
	if r == "" || reasonRE.MatchString(r) {
		return r
	}
	return "other"
}

// Export sends t as one root span. A malformed or all-zero id is refused (FromHex), not exported.
func (e *Exporter) Export(ctx context.Context, t Task) error {
	tid, err := trace.TraceIDFromHex(t.TraceID)
	if err != nil {
		return fmt.Errorf("task span trace id: %w", err)
	}
	sid, err := trace.SpanIDFromHex(t.SpanID)
	if err != nil {
		return fmt.Errorf("task span id: %w", err)
	}
	stub := tracetest.SpanStub{
		Name:        "task",
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}),
		SpanKind:    trace.SpanKindInternal,
		StartTime:   t.Start,
		EndTime:     t.End,
		Attributes: []attribute.KeyValue{attribute.String("agent.task_id", t.TaskID), attribute.String("agent.tier", t.Tier),
			attribute.String("agent.task.phase", t.Phase), attribute.String("agent.task.reason", spanReason(t.Reason))},
		Resource:             resource.NewSchemaless(attribute.String("service.name", "agent-factory")),
		InstrumentationScope: instrumentation.Scope{Name: "agent-factory"},
	}
	if err := e.exp.ExportSpans(ctx, tracetest.SpanStubs{stub}.Snapshots()); err != nil {
		return fmt.Errorf("export task span: %w", err)
	}
	return nil
}

// Shutdown flushes and closes the exporter.
func (e *Exporter) Shutdown(ctx context.Context) error { return e.exp.Shutdown(ctx) }
