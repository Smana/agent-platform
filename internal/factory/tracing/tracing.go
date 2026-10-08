// SPDX-License-Identifier: Apache-2.0

// Package tracing gives each factory task one root span (SP3 ruling R46). Its ids are minted
// when the task is accepted and kept in the Task's status; every run of the task parents its
// harness span on them, and the span itself is built from them and exported when the task ends,
// at least once: a replay exports the identical span, which the collector keeps once per id. A
// restart or a new leader loses nothing. The span carries ids, the tier, the phase and a reason
// code only, never issue text: it enters the trace router with no allowlist (O-1 M3).
package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// ExportTimeout bounds one export (ruling ST2): it runs on the reconciler's single worker, so a
// collector that never answers must cost a stop or the kill switch at most this.
const ExportTimeout = 3 * time.Second

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
// It dials lazily, so an unreachable collector fails an export, never the factory's start. The
// SDK's retry is off: the reconciler retries on a later reconcile, off the worker's clock (ST2).
func NewOTLP(ctx context.Context, endpoint string) (*Exporter, error) {
	exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(endpoint), otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{Enabled: false}))
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

// Export sends t as one root span, within ExportTimeout. A malformed or all-zero id is refused
// (FromHex), not exported.
func (e *Exporter) Export(ctx context.Context, t Task) error {
	tid, err := trace.TraceIDFromHex(t.TraceID)
	if err != nil {
		return fmt.Errorf("task span trace id: %w", err)
	}
	sid, err := trace.SpanIDFromHex(t.SpanID)
	if err != nil {
		return fmt.Errorf("task span id: %w", err)
	}
	span, err := record(tid, sid, t)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, ExportTimeout)
	defer cancel()
	if err := e.exp.ExportSpans(ctx, []sdktrace.ReadOnlySpan{span}); err != nil {
		return fmt.Errorf("export task span: %w", err)
	}
	return nil
}

// record builds t as the SDK's own span, ended at t.End, without exporting it: a tracer provider
// of one span, whose ids are the task's and whose processor keeps the ended span for Export. The
// SDK's processors swallow export errors; Export needs them to mark the span exported.
func record(tid trace.TraceID, sid trace.SpanID, t Task) (sdktrace.ReadOnlySpan, error) {
	keep := &keeper{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithIDGenerator(fixedIDs{tid, sid}), sdktrace.WithSpanProcessor(keep),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "agent-factory"))))
	_, span := tp.Tracer("agent-factory").Start(context.Background(), "task", trace.WithTimestamp(t.Start),
		trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attribute.String("agent.task_id", t.TaskID),
			attribute.String("agent.tier", t.Tier), attribute.String("agent.task.phase", t.Phase),
			attribute.String("agent.task.reason", spanReason(t.Reason))))
	span.End(trace.WithTimestamp(t.End))
	if keep.span == nil {
		return nil, errors.New("task span: not recorded")
	}
	return keep.span, nil
}

// fixedIDs hands the SDK the ids minted at acceptance.
type fixedIDs struct {
	tid trace.TraceID
	sid trace.SpanID
}

func (f fixedIDs) NewIDs(context.Context) (trace.TraceID, trace.SpanID)  { return f.tid, f.sid }
func (f fixedIDs) NewSpanID(context.Context, trace.TraceID) trace.SpanID { return f.sid }

// keeper is the span processor that keeps the one ended span of record's provider.
type keeper struct{ span sdktrace.ReadOnlySpan }

func (k *keeper) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (k *keeper) OnEnd(s sdktrace.ReadOnlySpan)                   { k.span = s }
func (k *keeper) Shutdown(context.Context) error                  { return nil }
func (k *keeper) ForceFlush(context.Context) error                { return nil }

// Shutdown flushes and closes the exporter.
func (e *Exporter) Shutdown(ctx context.Context) error { return e.exp.Shutdown(ctx) }
