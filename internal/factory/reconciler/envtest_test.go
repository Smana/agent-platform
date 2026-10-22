// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/forge/forgetest"
	"github.com/Smana/agent-platform/internal/factory/runs"
	"github.com/Smana/agent-platform/internal/factory/tracing"
	"github.com/Smana/agent-platform/internal/factory/triage"
)

// Against a real API server: the status subresource, resourceVersions, the AgentRun watch.
func TestEnvtestTaskToRun(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset: run `task test`, which sets it")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd"),
		filepath.Join("..", "testdata")}, ErrorIfCRDPathMissing: true}
	restCfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = env.Stop() }()
	s := scheme()
	runs.Scheme(s)
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{Scheme: s, Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatal(err)
	}
	c := mgr.GetClient()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for _, ns := range []string{"agent-system", runs.Namespace()} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	r := &Reconciler{Client: c, Namespace: "agent-system", Cfg: cfg(), Forge: forgetest.NewFake(), Runs: runs.Client{C: c},
		Rooms: &fakeLog{}, Triage: triage.Static{Cfg: cfg()}, Metrics: &fakeMetrics{},
		Now: time.Now, Nonce: func() string { return "n0nce234" }, Log: slog.New(slog.DiscardHandler),
		Trace: &fakeSink{}}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	defer func() { cancel(); <-done }()
	tk := issueTask("3buqdlot", 7, "x")
	tk.CreationTimestamp = metav1.Time{}
	if err := c.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(runs.GVK())
	var got v1alpha1.Task
	eventually(func() bool {
		if c.Get(ctx, types.NamespacedName{Namespace: runs.Namespace(), Name: runs.Name(runID("3buqdlot", 0))}, u) != nil {
			return false
		}
		return c.Get(ctx, client.ObjectKeyFromObject(tk), &got) == nil && got.Status.Phase == v1alpha1.PhaseImplementing &&
			u.GetLabels()[runs.LabelTask] == "3buqdlot"
	})
	if got.Status.Phase != v1alpha1.PhaseImplementing || u.GetLabels()[runs.LabelTask] != "3buqdlot" {
		t.Fatalf("no AgentRun, or the task never reached Implementing: %q %v", got.Status.Phase, u.GetLabels())
	}
	// R46: the API server keeps the minted trace, and the claim carries it with the tier.
	if tr := got.Status.Trace; tr == nil || u.GetAnnotations()[runs.AnnTraceparent] != tracing.Traceparent(tr.TraceID, tr.SpanID) ||
		u.GetLabels()[runs.LabelTier] != "standard" {
		t.Fatalf("trace %+v, claim %v %v", got.Status.Trace, u.GetAnnotations(), u.GetLabels())
	}
	// Every run of the task is parented on these ids: the CRD refuses a change of them.
	if err := refusedUpdate(ctx, c, client.ObjectKeyFromObject(tk), func(tk *v1alpha1.Task) { tk.Status.Trace.TraceID = strings.Repeat("c", 32) }); err == nil || !strings.Contains(err.Error(), "a task's trace ids never change") {
		t.Fatalf("a changed trace id is refused: %v", err)
	}
	// Review M3: nor removed, which would let a new mint through.
	if err := refusedUpdate(ctx, c, client.ObjectKeyFromObject(tk), func(tk *v1alpha1.Task) { tk.Status.Trace = nil }); err == nil || !strings.Contains(err.Error(), "a task's trace is never removed") {
		t.Fatalf("a removed trace is refused: %v", err)
	}

	// R49: settled usage stays settled — the omitempty bool's has() guards hold on a real API
	// server, where false is absent.
	if err := refusedUpdate(ctx, c, client.ObjectKeyFromObject(tk), func(tk *v1alpha1.Task) { tk.Status.UsageSettled = true }); err != nil {
		t.Fatal(err)
	}
	if err := refusedUpdate(ctx, c, client.ObjectKeyFromObject(tk), func(tk *v1alpha1.Task) { tk.Status.UsageSettled = false }); err == nil || !strings.Contains(err.Error(), "settled usage stays settled") {
		t.Fatalf("unsettling is refused: %v", err)
	}

	// A change of the run is a reconcile of its task: the AgentRun watch maps the label back.
	if err := unstructured.SetNestedField(u.Object, "Succeeded", "status", "phase"); err != nil {
		t.Fatal(err)
	}
	if err := c.Status().Update(ctx, u); err != nil {
		t.Fatal(err)
	}
	if !eventually(func() bool {
		return c.Get(ctx, client.ObjectKeyFromObject(tk), &got) == nil && len(got.Status.Runs) == 1 && got.Status.Runs[0].Phase == "Succeeded"
	}) {
		t.Fatalf("the run's phase never reached its task: %+v", got.Status.Runs)
	}
}

// eventually polls cond every 200 ms for up to 20 s. A real API server has no
// event to wait on from here, so this is the one bounded poll in these tests.
func eventually(cond func() bool) bool {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for {
		if cond() {
			return true
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			return cond()
		}
	}
}

// refusedUpdate applies change to a fresh copy of the task and updates its status. The controller
// writes the same status, so a conflict (409) is retried on a fresh Get, never taken for the answer:
// the caller sees the API server's validation error, or nil when the update went through.
func refusedUpdate(ctx context.Context, c client.Client, key client.ObjectKey, change func(*v1alpha1.Task)) error {
	var err error
	for range 20 {
		var cur v1alpha1.Task
		if err = c.Get(ctx, key, &cur); err != nil {
			return err
		}
		change(&cur)
		if err = c.Status().Update(ctx, &cur); !apierrors.IsConflict(err) {
			return err
		}
	}
	return err
}
