// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
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
		Now: time.Now, NewRunID: func() string { return "7f3cq2xz" }, Nonce: func() string { return "n0nce234" }, Log: slog.New(slog.DiscardHandler)}
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
		if c.Get(ctx, types.NamespacedName{Namespace: runs.Namespace, Name: runs.Name("7f3cq2xz")}, u) != nil {
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
