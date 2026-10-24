// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	roomv1 "github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/triage"
)

// neverTriage fails the test when the classifier is asked about an adopted Task.
type neverTriage struct{ t *testing.T }

func (n neverTriage) Triage(context.Context, *v1alpha1.Task) (triage.Decision, error) {
	n.t.Helper()
	n.t.Error("an adopted task is never triaged")
	return triage.Decision{}, errors.New("triaged")
}

// receivedRig is adoptRig before the gate: phase Received, no pull request recorded yet.
func receivedRig(t *testing.T) *rig {
	g := adoptRig(t)
	var tk v1alpha1.Task
	if err := g.c.Get(t.Context(), client.ObjectKey{Namespace: "agent-system", Name: adoptTask}, &tk); err != nil {
		t.Fatal(err)
	}
	tk.Spec.Template, tk.Spec.Issue, tk.Spec.DataClass = "x-never-used", 0, "internal"
	tk.Spec.Text = "Adopted pull request #30 from renovate[bot] (renovate/kyverno-3.x)."
	if err := g.c.Update(t.Context(), &tk); err != nil {
		t.Fatal(err)
	}
	tk.Status = v1alpha1.TaskStatus{Phase: v1alpha1.PhaseReceived}
	if err := g.c.Status().Update(t.Context(), &tk); err != nil {
		t.Fatal(err)
	}
	g.r.Triage = neverTriage{t}
	return g
}

// R39: received moves an adopted Task to AwaitingCI with its pull request, without triage, a
// room, a run, or a place in the daily task cap.
func TestAnAdoptedTaskGoesStraightToTheGate(t *testing.T) {
	g := receivedRig(t)
	g.r.Cfg.Caps.TasksPerDay = 1 // other tasks already fill the day
	g.r.Cfg.Budgets.PerPerson.Enabled = true
	other := issueTask("5otheraa", 8, "x")
	other.Status.Phase = v1alpha1.PhaseImplementing
	if err := g.c.Create(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	tk := g.reconcile(t, adoptTask, 1)
	pr := tk.Status.PullRequest
	if tk.Status.Phase != v1alpha1.PhaseAwaitingCI || pr == nil || pr.Number != 30 || pr.NodeID != "PR_30" || pr.HeadSHA != adoptHead ||
		pr.Author != bot || tk.Status.ConfigHash == "" || tk.Spec.Template != "" {
		t.Fatalf("%s %+v template %q", tk.Status.Phase, pr, tk.Spec.Template)
	}
	var rooms roomv1.RoomList
	if err := g.c.List(t.Context(), &rooms); err != nil || len(rooms.Items) != 0 || len(g.runs.specs) != 0 || tk.Status.RoomRef != "" {
		t.Fatalf("no room, no run: %v %d %d", err, len(rooms.Items), len(g.runs.specs))
	}
	// L15: Renovate is no principal, and no per-person series follows it.
	if p := tk.Payer(); p != "" {
		t.Fatalf("an adopted task is charged to nobody: %q", p)
	}
}

// The text cap stays: an oversized adopted text is still refused.
func TestAnAdoptedTaskKeepsTheTextCap(t *testing.T) {
	g := receivedRig(t)
	g.r.Cfg.Caps.MaxTextBytes = 5
	if tk := g.reconcile(t, adoptTask, 1); tk.Status.Phase != v1alpha1.PhaseRejected || tk.Status.Reason != "text_too_long" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// A pull request closed before the first read is still recorded and sent to the gate, which reads
// CLOSED and closes the Task on the next pass.
func TestAnAdoptedTaskWhosePRClosedBeforeTheFirstReadGoesToTheGateThenCloses(t *testing.T) {
	g := receivedRig(t)
	g.editPR(func(p *forge.PR) { p.State = "CLOSED" })
	tk := g.reconcile(t, adoptTask, 1)
	if tk.Status.Phase != v1alpha1.PhaseAwaitingCI { // the gate reads CLOSED and closes it
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
	if tk = g.reconcile(t, adoptTask, 1); tk.Status.Phase != v1alpha1.PhaseClosed {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// R45: in AwaitingHuman an adopted Task never reminds nor stale-closes Renovate's PR, never
// revises on a maintainer's Request changes, and a new head (Renovate's rebase) returns it to
// AwaitingCI; a merge by Renovate ends it Done "merged" and, with a monitor declared, under watch.
func TestTheAdoptedLifecycle(t *testing.T) {
	t.Run("no reminder, no stale close", func(t *testing.T) {
		g := adoptRig(t)
		if tk := g.reconcile(t, adoptTask, 1); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman {
			t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
		}
		g.r.Now = func() time.Time { return now.Add(15 * 24 * time.Hour) }
		for range 3 {
			g.reconcile(t, adoptTask, 1)
		}
		tk := g.reconcile(t, adoptTask, 1)
		if tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || len(g.f.Comments(30)) != 1 || !strings.Contains(g.f.Comments(30)[0], "event=would-arm-0 head="+adoptHead) {
			t.Fatalf("%s %s comments %q", tk.Status.Phase, tk.Status.Reason, g.f.Comments(30))
		}
		if p, _ := g.f.PullRequest(t.Context(), 30); p.State != "OPEN" || len(p.Labels) != 0 {
			t.Fatalf("Renovate's PR is not the factory's to close: %+v", p)
		}
	})
	t.Run("a request for changes starts no revision", func(t *testing.T) {
		g := adoptRig(t)
		g.review(30, 7, "Smana", "CHANGES_REQUESTED")
		g.reconcile(t, adoptTask, 1)
		tk := g.reconcile(t, adoptTask, 2)
		if tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "changes_requested" || len(g.runs.specs) != 0 || len(tk.Status.Runs) != 0 {
			t.Fatalf("%s %s runs %d", tk.Status.Phase, tk.Status.Reason, len(g.runs.specs))
		}
	})
	t.Run("a new head returns to the gate", func(t *testing.T) {
		g := adoptRig(t)
		g.reconcile(t, adoptTask, 1)
		g.cleanHead(newHead)
		tk := g.reconcile(t, adoptTask, 1)
		if tk.Status.Phase != v1alpha1.PhaseAwaitingCI || tk.Status.GateReentries != 0 {
			t.Fatalf("%s %s %d", tk.Status.Phase, tk.Status.Reason, tk.Status.GateReentries)
		}
		if tk = g.reconcile(t, adoptTask, 1); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.PullRequest.HeadSHA != newHead {
			t.Fatalf("decided again on the new head: %s %s", tk.Status.Phase, tk.Status.PullRequest.HeadSHA)
		}
	})
	t.Run("a merge by Renovate ends it, under watch", func(t *testing.T) {
		g := adoptRig(t)
		cl := g.r.Cfg.Classes["deps-patch"]
		cl.Monitor = &config.Monitor{FollowUpWindow: config.Duration{Duration: 168 * time.Hour}}
		g.r.Cfg.Classes["deps-patch"] = cl
		g.reconcile(t, adoptTask, 1)
		g.editPR(func(p *forge.PR) {
			p.State, p.MergedBy, p.MergedAt, p.MergeCommitSHA = "MERGED", bot, now, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		})
		tk := g.reconcile(t, adoptTask, 1)
		if tk.Status.Phase != v1alpha1.PhaseDone || tk.Status.Reason != "merged" || tk.Status.PullRequest.MergedBy != bot ||
			!g.r.monitorDue(tk) {
			t.Fatalf("%s %s %+v", tk.Status.Phase, tk.Status.Reason, tk.Status.PullRequest)
		}
	})
}

// R46: a shadow decision on an adopted PR posts the marker the tally reads, and no verdict claim.
func TestWouldArmAdopted(t *testing.T) {
	g := adoptRig(t)
	if tk := g.reconcile(t, adoptTask, 1); tk.Status.Reason != "shadow_would_arm" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
	c := strings.Join(g.f.Comments(30), "\n")
	if !strings.Contains(c, "event=would-arm-0 head="+adoptHead+" -->") || !strings.Contains(c, "would auto-merge: `deps-patch`") ||
		strings.Contains(c, "verdict approve") || !strings.Contains(c, "The class is in shadow: nothing is merged by the factory.") {
		t.Fatalf("%q", c)
	}
}

// adoptedCrowd is n adopted Tasks of Renovate's in the given phase, created today.
func adoptedCrowd(n int, phase string) []client.Object {
	var out []client.Object
	for i := range n {
		o := issueTask("7adopt"+string(rune('a'+i))+"z", 0, "x")
		o.Spec.Template, o.Spec.Source.Kind, o.Spec.Source.RequestedBy = "", "adopt", "github:renovate[bot]"
		o.Status.Phase = phase
		out = append(out, o)
	}
	return out
}

// R39: an adopted Task counts toward none of the factory's caps: ten of them, in each phase that
// counts, still leave an issue Task its place in tasksPerDay, activeTasks and awaitingHumanWIP.
func TestAdoptedTasksFillNoCap(t *testing.T) {
	t.Run("tasksPerDay", func(t *testing.T) {
		g := newRig(t, append(adoptedCrowd(10, v1alpha1.PhaseAwaitingCI), issueTask("3buqdlot", 7, "x"))...)
		g.r.Cfg.Caps.TasksPerDay = 1
		if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseTriaged {
			t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
		}
	})
	t.Run("activeTasks", func(t *testing.T) {
		g := newRig(t, append(adoptedCrowd(10, v1alpha1.PhaseAwaitingCI), issueTask("3buqdlot", 7, "x"))...)
		g.r.Cfg.Caps.ActiveTasks = 1
		if tk := g.reconcile(t, "3buqdlot", 3); tk.Status.Phase != v1alpha1.PhaseImplementing {
			t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
		}
	})
	t.Run("awaitingHumanWIP", func(t *testing.T) {
		g := newRig(t, append(adoptedCrowd(10, v1alpha1.PhaseAwaitingHuman), issueTask("3buqdlot", 7, "x"))...)
		g.r.Cfg.Caps.AwaitingHumanWIP = 1
		if tk := g.reconcile(t, "3buqdlot", 3); tk.Status.Phase != v1alpha1.PhaseImplementing {
			t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
		}
	})
}
