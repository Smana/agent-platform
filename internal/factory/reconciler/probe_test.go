// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	roomv1 "github.com/Smana/agent-platform/api/v1alpha1"
)

// Probe 1: the status write recording a new run conflicts (the poller's stop patch bumped the
// resourceVersion meanwhile). The stop then ends the task and leaves the run alive.
func TestProbeStopAfterALostRunRecordLeavesTheRunAlive(t *testing.T) {
	failNext := false
	c := fake.NewClientBuilder().WithScheme(scheme()).WithStatusSubresource(&v1alpha1.Task{}, &roomv1.Room{}).
		WithObjects(issueTask("3buqdlot", 7, "x")).
		WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, o client.Object, opts ...client.SubResourceUpdateOption) error {
			if tk, ok := o.(*v1alpha1.Task); ok && failNext && len(tk.Status.Runs) > 0 { // the write recording the run
				failNext = false
				return apierrors.NewConflict(schema.GroupResource{Resource: "tasks"}, o.GetName(), errors.New("stale"))
			}
			return cl.SubResource(sub).Update(ctx, o, opts...)
		}}).Build()
	g := newRig(t)
	g.c, g.r.Client = c, c
	g.reconcile(t, "3buqdlot", 2) // Queued
	failNext = true
	_, _ = g.r.Reconcile(t.Context(), reqFor("3buqdlot"))
	if len(g.runs.runs) != 1 {
		t.Fatalf("setup: %d runs", len(g.runs.runs))
	}
	var tk v1alpha1.Task
	_ = c.Get(t.Context(), keyFor("3buqdlot"), &tk)
	tk.Annotations = map[string]string{v1alpha1.AnnotationStop: "label"}
	if err := c.Update(t.Context(), &tk); err != nil {
		t.Fatal(err)
	}
	got := g.reconcile(t, "3buqdlot", 1)
	t.Logf("phase=%s runs-in-status=%d live-runs=%d", got.Status.Phase, len(got.Status.Runs), len(g.runs.runs))
	if got.Status.Phase == v1alpha1.PhaseStopped && len(g.runs.runs) != 0 {
		t.Errorf("DEFECT: task Stopped but run %v still exists", g.runs.runs)
	}
}

func reqFor(n string) ctrl.Request {
	return ctrl.Request{NamespacedName: keyFor(n)}
}

func keyFor(n string) types.NamespacedName {
	return types.NamespacedName{Namespace: "agent-system", Name: n}
}
