// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/killswitch"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// Spend per identity SI8: a task leaving Escalated adds the time since its phaseSince to its
// payer's waits, the payer of the work that waited: the labeller, not the maintainer whose retry
// ended the wait; the fleet with per-person caps off.
func TestAWaitCountsForTheTasksPayer(t *testing.T) {
	for enabled, payer := range map[bool]string{true: "github:smana", false: runs.PrincipalFactory} {
		g := newRig(t, escalatedTask(false), roomOf("3buqdlot"))
		g.r.Cfg.Budgets = personBudgets(enabled, true)
		g.r.Cfg.Maintainers = append(g.r.Cfg.Maintainers, "Alice")
		g.f.SetComments(7, retryBy(77, "Alice", time.Minute))
		if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseQueued {
			t.Fatalf("enabled %v: %s", enabled, tk.Status.Phase)
		}
		if want := "wait Escalated 600 " + payer; !slices.Contains(g.metrics.bound, want) {
			t.Fatalf("enabled %v: %q, want %q", enabled, g.metrics.bound, want)
		}
		g.reconcile(t, "3buqdlot", 1) // Queued → Implementing: no wait for a human
		if n := len(slices.DeleteFunc(slices.Clone(g.metrics.bound), func(s string) bool { return !strings.HasPrefix(s, "wait ") })); n != 1 {
			t.Fatalf("enabled %v: %d waits, want the one: %q", enabled, n, g.metrics.bound)
		}
	}
}

// SI4: a stop's revocations name each run's payer, as the ledger charged it, whatever the task's
// payer is now.
func TestAStopsRevocationsNameEachRunsPayer(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.r.Cfg.Budgets = personBudgets(true, true)
	g.reconcile(t, "3buqdlot", 3)
	g.runs.set(rid(0), "Running")
	g.runs.runs["aaaaaaa2"] = runs.Run{ID: "aaaaaaa2", TaskID: "3buqdlot", Phase: "Pending", Principal: runs.PrincipalFactory,
		ChargedTo: "github:bob"}
	_ = g.c.Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: killswitch.ConfigMap, Namespace: "agent-system"}})
	g.reconcile(t, "3buqdlot", 1)
	got := slices.DeleteFunc(slices.Clone(g.metrics.bound), func(s string) bool { return !strings.HasPrefix(s, "revoked manual") })
	slices.Sort(got)
	if want := []string{"revoked manual github:bob", "revoked manual github:smana"}; !slices.Equal(got, want) {
		t.Fatalf("%q, want %q", got, want)
	}
}
