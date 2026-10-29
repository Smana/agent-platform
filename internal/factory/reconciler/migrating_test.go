// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	roomv1 "github.com/Smana/maku/api/v1alpha1"
	"github.com/Smana/maku/internal/factory/killswitch"
)

// A copy migrate-group has not finished is left alone (maku rebrand): with no status yet it
// reads as a new task, and under the window's kill switch a finished one would end Stopped.
func TestAMigratingTaskIsLeftAlone(t *testing.T) {
	tk := issueTask("3buqdlot", 7, "x")
	tk.Annotations = map[string]string{roomv1.AnnotationMigrating: "true"}
	stop := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: killswitch.ConfigMap, Namespace: "agent-system"}}
	g := newRig(t, tk, stop)
	got := g.reconcile(t, "3buqdlot", 2)
	if got.Status.Phase != "" || len(g.runs.specs) != 0 || len(g.f.Comments(7)) != 0 {
		t.Fatalf("phase %q, %d runs, %d comments: the migrating copy was reconciled",
			got.Status.Phase, len(g.runs.specs), len(g.f.Comments(7)))
	}
	// migrate-group writes the status while the guard still stands: the kill switch would stop an
	// Implementing task, so only the guard keeps it as it was.
	got.Status.Phase = v1alpha1.PhaseImplementing
	if err := g.c.Status().Update(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	held := g.reconcile(t, "3buqdlot", 1)
	if held.Status.Phase != v1alpha1.PhaseImplementing {
		t.Fatalf("phase %q while guarded, want Implementing kept", held.Status.Phase)
	}
	// Lifting the guard: the task reconciles from its migrated status, not from Received.
	delete(held.Annotations, roomv1.AnnotationMigrating)
	if err := g.c.Update(t.Context(), held); err != nil {
		t.Fatal(err)
	}
	if after := g.reconcile(t, "3buqdlot", 1); after.Status.Phase != v1alpha1.PhaseStopped {
		t.Fatalf("phase %q after the lift, want Stopped by the kill switch, from the migrated status", after.Status.Phase)
	}
}
