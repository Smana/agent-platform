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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/forge"
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
	for _, ns := range []string{"agent-system", runs.Namespace} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	r := &Reconciler{Client: c, Namespace: "agent-system", Cfg: cfg(), Forge: forge.NewFake(), Runs: runs.Client{C: c},
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
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if c.Get(ctx, types.NamespacedName{Namespace: runs.Namespace, Name: runs.Name(runID("3buqdlot", 0))}, u) != nil {
			continue
		}
		if c.Get(ctx, client.ObjectKeyFromObject(tk), &got) == nil && got.Status.Phase == v1alpha1.PhaseImplementing &&
			u.GetLabels()[runs.LabelTask] == "3buqdlot" {
			break
		}
	}
	if got.Status.Phase != v1alpha1.PhaseImplementing || u.GetLabels()[runs.LabelTask] != "3buqdlot" {
		t.Fatalf("no AgentRun, or the task never reached Implementing: %q %v", got.Status.Phase, u.GetLabels())
	}
	// R46: the API server keeps the minted trace, and the claim carries it with the tier.
	if tr := got.Status.Trace; tr == nil || u.GetAnnotations()[runs.AnnTraceparent] != tracing.Traceparent(tr.TraceID, tr.SpanID) ||
		u.GetLabels()[runs.LabelTier] != "standard" {
		t.Fatalf("trace %+v, claim %v %v", got.Status.Trace, u.GetAnnotations(), u.GetLabels())
	}
	// Every run of the task is parented on these ids: the CRD refuses a change of them.
	moved := got.DeepCopy()
	moved.Status.Trace.TraceID = strings.Repeat("c", 32)
	if err := c.Status().Update(ctx, moved); err == nil || !strings.Contains(err.Error(), "a task's trace ids never change") {
		t.Fatalf("a changed trace id is refused: %v", err)
	}
	// Review M3: nor removed, which would let a new mint through.
	dropped := got.DeepCopy()
	dropped.Status.Trace = nil
	if err := c.Status().Update(ctx, dropped); err == nil || !strings.Contains(err.Error(), "a task's trace is never removed") {
		t.Fatalf("a removed trace is refused: %v", err)
	}

	// R49: settled usage stays settled — the omitempty bool's has() guards hold on a real API
	// server, where false is absent.
	settled := got.DeepCopy()
	settled.Status.UsageSettled = true
	if err := c.Status().Update(ctx, settled); err != nil {
		t.Fatal(err)
	}
	unsettled := settled.DeepCopy()
	unsettled.Status.UsageSettled = false
	if err := c.Status().Update(ctx, unsettled); err == nil || !strings.Contains(err.Error(), "settled usage stays settled") {
		t.Fatalf("unsettling is refused: %v", err)
	}

	// A change of the run is a reconcile of its task: the AgentRun watch maps the label back.
	if err := unstructured.SetNestedField(u.Object, "Succeeded", "status", "phase"); err != nil {
		t.Fatal(err)
	}
	if err := c.Status().Update(ctx, u); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if c.Get(ctx, client.ObjectKeyFromObject(tk), &got) == nil && len(got.Status.Runs) == 1 && got.Status.Runs[0].Phase == "Succeeded" {
			return
		}
	}
	t.Fatalf("the run's phase never reached its task: %+v", got.Status.Runs)
}
