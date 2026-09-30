// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	roomv1 "github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/tracing"
	"github.com/Smana/agent-platform/internal/factory/triage"
)

type fakeSink struct {
	got  []tracing.Task
	down bool
}

func (f *fakeSink) Export(_ context.Context, t tracing.Task) error {
	if f.down {
		return errors.New("collector unreachable")
	}
	f.got = append(f.got, t)
	return nil
}

func stopTask(t *testing.T, g *rig, tk *v1alpha1.Task) {
	t.Helper()
	tk.Annotations = map[string]string{v1alpha1.AnnotationStop: "true"}
	if err := g.c.Update(context.Background(), tk); err != nil {
		t.Fatal(err)
	}
}

// R46: one root span per accepted task, its traceparent on every run, exported once at the end.
func TestEveryRunOfATaskSharesItsTrace(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "fix"))
	sink := &fakeSink{}
	g.r.Trace = sink
	tk := g.reconcile(t, "3buqdlot", 3)
	tr := tk.Status.Trace
	if tr == nil || len(tr.TraceID) != 32 || len(tr.SpanID) != 16 || tr.Exported {
		t.Fatalf("minted at acceptance: %+v", tr)
	}
	s := g.runs.specs["7f3cq2xz"]
	if s.Traceparent != tracing.Traceparent(tr.TraceID, tr.SpanID) || s.Tier != "standard" {
		t.Fatalf("the run carries the task's trace and tier: %q %q", s.Traceparent, s.Tier)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("the claim is one runs.Client creates: %v", err)
	}
	if len(sink.got) != 0 {
		t.Fatal("nothing is exported before the task ends")
	}
	stopTask(t, g, tk)
	tk = g.reconcile(t, "3buqdlot", 2)
	if tk.Status.Phase != v1alpha1.PhaseStopped || len(sink.got) != 1 || !tk.Status.Trace.Exported {
		t.Fatalf("%s: exported %d", tk.Status.Phase, len(sink.got))
	}
	got := sink.got[0]
	if got.TraceID != tr.TraceID || got.SpanID != tr.SpanID || got.TaskID != "3buqdlot" || got.Phase != v1alpha1.PhaseStopped ||
		got.Reason != "stopped_by_annotation" || got.Tier != "standard" ||
		!got.Start.Equal(now.Add(-time.Minute)) || !got.End.Equal(now) {
		t.Fatalf("%+v", got)
	}
	g.reconcile(t, "3buqdlot", 2)
	if len(sink.got) != 1 {
		t.Fatal("exported once")
	}
}

func TestNoSinkNoTaskTrace(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "fix"))
	tk := g.reconcile(t, "3buqdlot", 3)
	if tk.Status.Trace != nil || g.runs.specs["7f3cq2xz"].Traceparent != "" {
		t.Fatal("tracing off: no trace minted; each run starts its own, as task agent:run's do")
	}
}

type flakyTriage struct {
	triage.Triager
	fail bool
}

func (f *flakyTriage) Triage(ctx context.Context, t *v1alpha1.Task) (triage.Decision, error) {
	if f.fail {
		f.fail = false
		return triage.Decision{}, errors.New("triage unavailable")
	}
	return f.Triager.Triage(ctx, t)
}

// The ids are minted once: a task whose triage failed after acceptance keeps the ids its status
// already holds (the CRD refuses a change of them, and its runs are parented on them).
func TestTheTraceIsMintedOnce(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.r.Trace = &fakeSink{}
	g.r.Triage = &flakyTriage{Triager: triage.Static{Cfg: cfg()}, fail: true}
	if _, err := g.r.Reconcile(t.Context(), reqFor("3buqdlot")); err == nil {
		t.Fatal("the triage error is returned")
	}
	first := g.reconcile(t, "3buqdlot", 0).Status.Trace
	if first == nil {
		t.Fatal("minted with the acceptance, before triage")
	}
	if tk := g.reconcile(t, "3buqdlot", 3); *tk.Status.Trace != *first ||
		g.runs.specs["7f3cq2xz"].Traceparent != tracing.Traceparent(first.TraceID, first.SpanID) {
		t.Fatalf("re-minted: %+v, then %+v", first, tk.Status.Trace)
	}
}

// A rejected task was never accepted: it has no span.
func TestARejectedTaskHasNoTrace(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, strings.Repeat("x", 14337)))
	sink := &fakeSink{}
	g.r.Trace = sink
	tk := g.reconcile(t, "3buqdlot", 2)
	if tk.Status.Phase != v1alpha1.PhaseRejected || tk.Status.Trace != nil || len(sink.got) != 0 {
		t.Fatalf("%s %+v %d", tk.Status.Phase, tk.Status.Trace, len(sink.got))
	}
}

// The span is the task's, not a reconcile's: a terminal write that conflicts exports nothing (its
// replay may end the task otherwise), and the replay exports it once.
func TestTheSpanIsExportedOnlyOnceItsEndIsWritten(t *testing.T) {
	conflict := false
	c := fake.NewClientBuilder().WithScheme(scheme()).WithStatusSubresource(&v1alpha1.Task{}, &roomv1.Room{}).
		WithObjects(issueTask("3buqdlot", 7, "x")).
		WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, o client.Object, opts ...client.SubResourceUpdateOption) error {
			if conflict {
				conflict = false
				return apierrors.NewConflict(schema.GroupResource{Resource: "tasks"}, o.GetName(), errors.New("stale"))
			}
			return cl.SubResource(sub).Update(ctx, o, opts...)
		}}).Build()
	g := newRig(t)
	g.c, g.r.Client = c, c
	sink := &fakeSink{}
	g.r.Trace = sink
	stopTask(t, g, g.reconcile(t, "3buqdlot", 3))
	conflict = true
	if _, err := g.r.Reconcile(t.Context(), reqFor("3buqdlot")); err == nil {
		t.Fatal("the conflict is returned")
	}
	if len(sink.got) != 0 {
		t.Fatal("an unwritten end exports nothing")
	}
	if tk := g.reconcile(t, "3buqdlot", 2); !tk.Status.Trace.Exported || len(sink.got) != 1 {
		t.Fatalf("exported %d", len(sink.got))
	}
}

// Best effort: a collector outage never holds the task, and the span, with the end time the task
// recorded, goes out on a later reconcile. The retry is stamped with the persisted end, not the
// retry's clock.
func TestALostExportIsRetriedWithTheRecordedEnd(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	sink := &fakeSink{down: true}
	g.r.Trace = sink
	stopTask(t, g, g.reconcile(t, "3buqdlot", 3))
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseStopped || tk.Status.Trace.Exported || len(tk.Status.Outbox) != 0 {
		t.Fatalf("the task ends and narrates regardless: %s %+v", tk.Status.Phase, tk.Status.Trace)
	}
	sink.down = false
	g.r.Now = func() time.Time { return now.Add(time.Hour) }
	tk = g.reconcile(t, "3buqdlot", 2)
	if !tk.Status.Trace.Exported || len(sink.got) != 1 || !sink.got[0].End.Equal(now) {
		t.Fatalf("%+v %+v", tk.Status.Trace, sink.got)
	}
}

// O-1 M3: the factory's spans enter traces/router with no allowlist. Nothing an issue author
// wrote — its title, body or a comment — ever reaches a span attribute, event or status.
func TestNoIssueTextReachesTheSpan(t *testing.T) {
	const canary = "CANARY7Q"
	tk := issueTask("3buqdlot", 7, "# "+canary+" title\n\n"+canary+" body, then a comment: "+canary)
	tk.Spec.Source.Ref = "Smana/cloud-native-ref#7"
	g := newRig(t, tk)
	mem := tracetest.NewInMemoryExporter()
	g.r.Trace = tracing.New(mem)
	g.reconcile(t, "3buqdlot", 3)
	g.runs.set("7f3cq2xz", "Failed")
	g.log.end("7f3cq2xz", "Failed", canary+" the agent said")
	g.reconcile(t, "3buqdlot", 1) // Escalated, with the room's reason
	stopTask(t, g, g.reconcile(t, "3buqdlot", 0))
	g.reconcile(t, "3buqdlot", 2)
	spans := mem.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("%d spans", len(spans))
	}
	s := spans[0]
	texts := []string{s.Name, s.Status.Description}
	for _, kv := range append(s.Attributes, s.Resource.Attributes()...) {
		texts = append(texts, string(kv.Key), kv.Value.String())
	}
	for _, e := range s.Events {
		texts = append(texts, e.Name)
		for _, kv := range e.Attributes {
			texts = append(texts, kv.Value.String())
		}
	}
	for _, x := range texts {
		if strings.Contains(x, canary) {
			t.Fatalf("issue text on the span: %q", x)
		}
	}
}
