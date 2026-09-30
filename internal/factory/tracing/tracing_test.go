// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"bytes"
	"context"
	"errors"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// R46: the task span is built from the ids minted at acceptance, and carries metadata only.
func TestTheTaskSpanKeepsItsMintedIds(t *testing.T) {
	tr, sp := Mint()
	if !regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`).MatchString(Traceparent(tr, sp)) {
		t.Fatalf("traceparent %q", Traceparent(tr, sp))
	}
	if again, _ := Mint(); again == tr {
		t.Fatal("every task gets its own trace")
	}
	mem := tracetest.NewInMemoryExporter()
	start := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	task := Task{TraceID: tr, SpanID: sp, TaskID: "3buqdlot", Tier: "standard", Phase: "Done", Reason: "merged", Start: start, End: start.Add(time.Hour)}
	if err := New(mem).Export(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	got := mem.GetSpans()
	if len(got) != 1 || got[0].Name != "task" || got[0].SpanContext.TraceID().String() != tr ||
		got[0].SpanContext.SpanID().String() != sp || got[0].Parent.IsValid() || !got[0].StartTime.Equal(start) ||
		!got[0].EndTime.Equal(start.Add(time.Hour)) || !got[0].SpanContext.IsSampled() {
		t.Fatalf("%+v", got)
	}
	attrs := map[string]string{}
	for _, kv := range got[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	if attrs["agent.task_id"] != "3buqdlot" || attrs["agent.tier"] != "standard" || attrs["agent.task.phase"] != "Done" ||
		attrs["agent.task.reason"] != "merged" || len(attrs) != 4 {
		t.Fatalf("metadata only: %v", attrs)
	}
	// O-1 M3: the span reaches traces/router with no allowlist, so nothing else rides on it.
	if len(got[0].Events) != 0 || len(got[0].Links) != 0 || got[0].Status.Description != "" {
		t.Fatalf("no events, links or status text: %+v", got[0])
	}
	if res := got[0].Resource.Attributes(); len(res) != 1 || res[0].Key != "service.name" || res[0].Value.AsString() != "agent-factory" {
		t.Fatalf("resource %v", res)
	}
	for _, bad := range []Task{
		{TraceID: "nothex", SpanID: sp},
		{TraceID: tr, SpanID: "nothex"},
		{TraceID: "00000000000000000000000000000000", SpanID: sp},
		{TraceID: tr, SpanID: "0000000000000000"},
	} {
		if err := New(mem).Export(context.Background(), bad); err == nil {
			t.Errorf("%+v: a malformed or invalid id is refused", bad)
		}
	}
	if len(mem.GetSpans()) != 1 {
		t.Fatal("a refused span is not exported")
	}
}

// A reason reaches the span only as a short snake_case code: the reconciler's own, or a
// broker's run end reason. Anything else is "other" (O-1 M3).
func TestASpanReasonIsACode(t *testing.T) {
	for reason, want := range map[string]string{
		"stopped_by_annotation":       "stopped_by_annotation",
		"merged":                      "merged",
		"":                            "",
		"IGNORE ALL RULES":            "other",
		"secret: ghp_0123456789":      "other",
		"line\nbreak":                 "other",
		"two words":                   "other",
		"x" + strings.Repeat("y", 63): "x" + strings.Repeat("y", 63),
		"x" + strings.Repeat("y", 64): "other",
	} {
		mem := tracetest.NewInMemoryExporter()
		tr, sp := Mint()
		if err := New(mem).Export(context.Background(), Task{TraceID: tr, SpanID: sp, Reason: reason}); err != nil {
			t.Fatal(err)
		}
		for _, kv := range mem.GetSpans()[0].Attributes {
			if kv.Key == "agent.task.reason" && kv.Value.AsString() != want {
				t.Errorf("reason %q: got %q, want %q", reason, kv.Value.AsString(), want)
			}
		}
	}
}

type failingExporter struct{ *tracetest.InMemoryExporter }

func (failingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return errors.New("collector unreachable")
}

// The reconciler marks a span exported only on success: a failed export is an error.
func TestAFailedExportIsAnError(t *testing.T) {
	tr, sp := Mint()
	if err := New(failingExporter{}).Export(context.Background(), Task{TraceID: tr, SpanID: sp}); err == nil {
		t.Fatal("a failed export is reported")
	}
}

// blackHole accepts connections and never answers: the collector outage that stalls a caller
// longest, since the TCP connect succeeds.
func blackHole(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var held []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
		for _, c := range held {
			_ = c.Close()
		}
	})
	return ln.Addr().String()
}

// Ruling ST2: an export gives up within ExportTimeout, the SDK's retry off, whatever the collector does.
func TestAnExportNeverOutlastsItsTimeout(t *testing.T) {
	exp, err := NewOTLP(t.Context(), blackHole(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = exp.Shutdown(ctx)
	}()
	tr, sp := Mint()
	start := time.Now()
	err = exp.Export(t.Context(), Task{TraceID: tr, SpanID: sp, Start: start, End: start})
	if took := time.Since(start); err == nil || took > ExportTimeout+time.Second {
		t.Fatalf("export: %v after %s, want an error within %s", err, took, ExportTimeout)
	}
	if ExportTimeout > 3*time.Second {
		t.Fatalf("ExportTimeout %s: ST2 caps it at 3 s", ExportTimeout)
	}
}

// SI: an all-zero id is invalid W3C, and runs.Spec refuses a traceparent that carries one.
func TestMintNeverReturnsAZeroId(t *testing.T) {
	draws := [][]byte{make([]byte, 16), bytes.Repeat([]byte{0xab}, 16), make([]byte, 8), bytes.Repeat([]byte{0xab}, 8)}
	src := bytes.NewReader(bytes.Join(draws, nil))
	tr, sp := mint(src)
	if tr != "abababababababababababababababab" || sp != "abababababababab" {
		t.Fatalf("zero draws are redrawn: %s %s", tr, sp)
	}
}
