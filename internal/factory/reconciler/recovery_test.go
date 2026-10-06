// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	roomv1 "github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// losingClient fails the next status write once: the write that would record what loseWhen says.
func losingClient(t *testing.T, loseWhen func(*v1alpha1.Task) bool) client.Client {
	t.Helper()
	lose := true
	return fake.NewClientBuilder().WithScheme(scheme()).WithStatusSubresource(&v1alpha1.Task{}, &roomv1.Room{}).
		WithObjects(issueTask("3buqdlot", 7, "x")).
		WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, o client.Object, opts ...client.SubResourceUpdateOption) error {
			if tk, ok := o.(*v1alpha1.Task); ok && lose && loseWhen(tk) {
				lose = false
				return apierrors.NewConflict(schema.GroupResource{Resource: "tasks"}, o.GetName(), errors.New("stale"))
			}
			return cl.SubResource(sub).Update(ctx, o, opts...)
		}}).Build()
}

// chatAt is one noise message in the room's log, at seq.
func chatAt(seq int64) envelope.Event {
	return envelope.Event{Seq: seq, Type: envelope.Message,
		Payload: envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: "noise"})}
}

// R48: the run's id is derived from the task, so a reconcile whose status write was lost finds
// the claim it created instead of minting another. Here the claim went Failed with 900 tokens
// before the replay: the record is the claim's own facts — its start seq included — and nothing
// runs twice.
func TestLostWriteTerminalOrphanRecordedOnce(t *testing.T) {
	c := losingClient(t, func(tk *v1alpha1.Task) bool { return len(tk.Status.Runs) > 0 })
	g := newRig(t)
	g.c, g.r.Client = c, c
	// A room with events, so the run's start seq is one a replay must read back, not recompute.
	for i := range 2 {
		g.log.evs = append(g.log.evs, chatAt(int64(i+1)))
	}
	g.reconcile(t, "3buqdlot", 2) // Queued
	if _, err := g.r.Reconcile(t.Context(), reqFor("3buqdlot")); err == nil {
		t.Fatal("the write recording the run is lost")
	}
	if len(g.runs.runs) != 1 {
		t.Fatalf("setup: %d claims", len(g.runs.runs))
	}
	// The orphan failed with 900 tokens, and the room holds its end.
	r := g.runs.runs[rid(0)]
	r.Phase, r.Tokens = "Failed", 900
	g.runs.runs[rid(0)] = r
	g.log.end(rid(0), "Failed", "pod_lost")
	tk := g.reconcile(t, "3buqdlot", 2) // the replay records the claim; the task then ends on it
	if len(g.runs.runs) != 1 || len(tk.Status.Runs) != 1 || tk.Status.Runs[0].ID != rid(0) {
		t.Fatalf("one claim, one record: %d claims, %+v", len(g.runs.runs), tk.Status.Runs)
	}
	rec := tk.Status.Runs[0]
	if rec.Tokens != 900 || rec.StartSeq != 2 || rec.Phase != "Failed" || rec.Role != "implementer" {
		t.Fatalf("the claim's own facts: %+v", rec)
	}
	if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "pod_lost" || tk.Status.Usage.Tokens != 900 {
		t.Fatalf("%s %s %d", tk.Status.Phase, tk.Status.Reason, tk.Status.Usage.Tokens)
	}
}

// R48: the reviewer's claim was created and its record lost; the pull request merged meanwhile.
// The replay records the run from its claim, and the sequence still passes through lateReviews
// (the merge ends the task through AwaitingHuman) with the run and its tokens on record.
func TestLostWriteThenPRMergedStillRecords(t *testing.T) {
	c := losingClient(t, func(tk *v1alpha1.Task) bool { return len(tk.Status.Runs) > 1 })
	g := newRig(t)
	g.c, g.r.Client = c, c
	g.r.Triage = staticWith("pair")
	g.reconcile(t, "3buqdlot", 3) // the implementer rid(0) is started
	g.f.SetBranch("agent/3buqdlot", 12)
	g.f.SetPR(pr12At(head1))
	g.runs.set(rid(0), "Running")
	g.reconcile(t, "3buqdlot", 2) // the PR is found
	g.finish(rid(0), "Succeeded", "agent_finished")
	g.reconcile(t, "3buqdlot", 1) // → Queued for the reviewer
	if _, err := g.r.Reconcile(t.Context(), reqFor("3buqdlot")); err == nil {
		t.Fatal("the write recording the reviewer is lost")
	}
	// The reviewer ended with 300 tokens; the pull request merged while the write was lost.
	r := g.runs.runs[rid(1)]
	r.Phase, r.Tokens = "Succeeded", 300
	g.runs.runs[rid(1)] = r
	g.log.end(rid(1), "Succeeded", "agent_finished")
	g.f.SetPR(forge.PR{Number: 12, State: "MERGED", MergedBy: "Smana", HeadSHA: head1})
	// The replay records the orphan; its missing verdict requeues the task, and lateReviews ends
	// it on the merge.
	tk := g.reconcile(t, "3buqdlot", 4)
	if tk.Status.Phase != v1alpha1.PhaseDone || tk.Status.Reason != "merged" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
	if len(g.runs.specs) != 2 || len(tk.Status.Runs) != 2 || tk.Status.Runs[1].ID != rid(1) ||
		tk.Status.Runs[1].Tokens != 300 || tk.Status.Usage.Tokens != 300 {
		t.Fatalf("the record and its tokens: %d claims, %+v usage %d", len(g.runs.specs), tk.Status.Runs, tk.Status.Usage.Tokens)
	}
}

// R49: the implementer ended at 1000 tokens and the reviewer started; the meter's late reading
// raises the implementer to 1300, and after the task ends the reviewer from 200 to 260. Every
// record is refreshed while the task moves, and the settle records the total once, after the
// window: 1560.
func TestLateUsageSettles(t *testing.T) {
	g := pairRig(t)
	usage := func(n int, tokens int64) {
		r := g.runs.runs[rid(n)]
		r.Tokens = tokens
		g.runs.runs[rid(n)] = r
	}
	usage(0, 1000)
	usage(1, 200)
	usage(0, 1300) // the meter's late reading of the ended implementer
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Usage.Tokens != 1500 {
		t.Fatalf("every record is refreshed: %d", tk.Status.Usage.Tokens)
	}
	g.log.verdict(rid(1), "approve", head1[:7], "LGTM")
	g.finish(rid(1), "Succeeded", "agent_finished")
	g.reconcile(t, "3buqdlot", 2) // → AwaitingHuman
	g.f.SetPR(forge.PR{Number: 12, State: "MERGED", MergedBy: "Smana", HeadSHA: head1})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseDone {
		t.Fatal(tk.Status.Phase)
	}
	if n := strings.Count(strings.Join(g.metrics.recorded, "|"), "task_tokens"); n != 0 {
		t.Fatalf("task_tokens recorded %d times before the settle: %q", n, g.metrics.recorded)
	}
	usage(1, 260) // the meter's late reading of the ended reviewer, after the task ended
	window := 2*g.r.Cfg.Poll.Meter.Duration + 30*time.Second
	g.r.Now = func() time.Time { return now.Add(window - time.Second) }
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Usage.Tokens != 1560 || tk.Status.UsageSettled {
		t.Fatalf("refreshed, not settled: %d %+v", tk.Status.Usage.Tokens, tk.Status.UsageSettled)
	}
	g.r.Now = func() time.Time { return now.Add(window) }
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Usage.Tokens != 1560 || !tk.Status.UsageSettled {
		t.Fatalf("settled: %d %+v", tk.Status.Usage.Tokens, tk.Status.UsageSettled)
	}
	var recorded []string
	for _, m := range g.metrics.recorded {
		if strings.HasPrefix(m, "task_tokens") {
			recorded = append(recorded, m)
		}
	}
	if strings.Join(recorded, "|") != "task_tokens 1560 standard pair review" {
		t.Fatalf("TaskTokens recorded once, with the settled total: %q", g.metrics.recorded)
	}
	g.r.Now = func() time.Time { return now.Add(window + time.Minute) }
	if tk := g.reconcile(t, "3buqdlot", 1); !tk.Status.UsageSettled {
		t.Fatalf("still settled: %+v", tk.Status.UsageSettled)
	}
	if n := strings.Count(strings.Join(g.metrics.recorded, "|"), "task_tokens"); n != 1 {
		t.Fatalf("settled once: %q", g.metrics.recorded)
	}
}

// A run Pending past caps.maxPendingMinutes never ran, so deleting it loses no usage: the task
// escalates as run_unschedulable and the slot it held frees for the next task.
func TestPendingRunEscalatesAndFreesSlot(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"), issueTask("4buqdlot", 8, "x"))
	for _, id := range []string{"aaaaaaa2", "aaaaaaa3", "aaaaaaa4"} {
		_ = g.runs.Create(t.Context(), runs.Spec{RunID: id, TaskID: "other", Principal: runs.PrincipalFactory})
		g.runs.set(id, "Running")
	}
	g.reconcile(t, "3buqdlot", 3) // → Implementing, its run Pending: the four slots are taken
	if tk := g.reconcile(t, "4buqdlot", 3); tk.Status.Phase != v1alpha1.PhaseQueued || tk.Status.Reason != "waiting_run_slot" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
	g.r.Now = func() time.Time { return now.Add(31 * time.Minute) }
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "run_unschedulable" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
	if _, pending := g.runs.runs[rid(0)]; pending {
		t.Fatal("the Pending claim is deleted")
	}
	if tk := g.reconcile(t, "4buqdlot", 2); tk.Status.Phase != v1alpha1.PhaseImplementing {
		t.Fatalf("the next task gets the slot: %s %s", tk.Status.Phase, tk.Status.Reason)
	}
	if issue := strings.Join(g.f.Comments(7), "\n"); !strings.Contains(issue, "never admitted the task's run") ||
		!strings.Contains(issue, "/factory retry") {
		t.Fatalf("the escalation says why and how to restart: %s", issue)
	}
}

// A run that started is never run_unschedulable, and the Pending bound never deletes it (aws-0,
// 2026-10-04): the composition reports a started run as Pending whenever its Sandbox is not
// Ready and not yet Finished — the harness exited and the sidecars drain — so 37 minutes in, the
// bound deleted a finished run that had opened its PR. Its record saw it Running, or its claim
// carries usage: either says it was admitted.
func TestStartedRunIsNeverUnschedulable(t *testing.T) {
	for name, c := range map[string]struct {
		running bool  // the factory saw the claim Running before it read Pending again
		tokens  int64 // the meter's reading on the claim
	}{
		"seen Running, then Pending while its pod drains": {running: true},
		"never seen Running, metered":                     {tokens: 294_000},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, issueTask("3buqdlot", 7, "x"))
			g.reconcile(t, "3buqdlot", 3)
			if c.running {
				g.runs.set(rid(0), "Running")
				g.f.SetBranch("agent/3buqdlot", 12)
				g.f.SetPR(forge.PR{Number: 12, URL: "https://github.com/Smana/cloud-native-ref/pull/12", State: "OPEN", NodeID: "PR_1", HeadSHA: "abc"})
				g.reconcile(t, "3buqdlot", 1)
			}
			run := g.runs.runs[rid(0)]
			run.Phase, run.Tokens = "Pending", c.tokens
			g.runs.runs[rid(0)] = run
			g.r.Now = func() time.Time { return now.Add(37 * time.Minute) }
			tk := g.reconcile(t, "3buqdlot", 1)
			if _, ok := g.runs.runs[rid(0)]; !ok || tk.Status.Phase != v1alpha1.PhaseImplementing {
				t.Fatalf("the started run is kept: claim %t, %s %s", ok, tk.Status.Phase, tk.Status.Reason)
			}
			if issue := strings.Join(g.f.Comments(7), "\n"); strings.Contains(issue, "never admitted") {
				t.Fatalf("narrated as unschedulable: %s", issue)
			}
			if !c.running {
				return
			}
			g.runs.set(rid(0), "Succeeded") // the pod completed: the composition latches Succeeded
			g.log.end(rid(0), "Succeeded", "agent_finished")
			if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAwaitingCI {
				t.Fatalf("the PR goes on, as any finished run's: %s %s", tk.Status.Phase, tk.Status.Reason)
			}
		})
	}
}

// A later run of a task starts unseen: run 0 ran, and run 1 never admitted is still bounded.
func TestLaterRunOfAStartedTaskIsStillBounded(t *testing.T) {
	g := pairRig(t) // rid(0) Succeeded, the reviewer rid(1) created and Pending
	g.r.Now = func() time.Time { return now.Add(31 * time.Minute) }
	tk := g.reconcile(t, "3buqdlot", 1)
	if _, ok := g.runs.runs[rid(1)]; ok || tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "run_unschedulable" {
		t.Fatalf("claim kept %t, %s %s", ok, tk.Status.Phase, tk.Status.Reason)
	}
}

// A claim deleted out of band after its run was seen Running, past the Pending bound: the room's
// reason, never run_unschedulable, and no second run.
func TestRunDeletedAfterItRan(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.reconcile(t, "3buqdlot", 3)
	g.runs.set(rid(0), "Running")
	g.reconcile(t, "3buqdlot", 1)
	g.r.Now = func() time.Time { return now.Add(37 * time.Minute) }
	_ = g.runs.Delete(t.Context(), rid(0))
	g.log.end(rid(0), "Revoked", "deleted")
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "deleted" || len(g.runs.runs) != 0 {
		t.Fatalf("%s %s, %d claims", tk.Status.Phase, tk.Status.Reason, len(g.runs.runs))
	}
}
