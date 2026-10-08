// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"errors"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/rooms"
)

// stored is the rig's task as its client has it, so postFacts can write its status.
func stored(t *testing.T, c client.Client) *v1alpha1.Task {
	t.Helper()
	var tk v1alpha1.Task
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "agent-system", Name: "3buqdlot"}, &tk); err != nil {
		t.Fatal(err)
	}
	return &tk
}

func TestFactsOfATaskWithARunAndAPR(t *testing.T) {
	task := awaiting()
	task.Spec.Repository, task.Spec.Issue, task.Spec.IssueAuthor = "Smana/cloud-native-ref", 2238, "dev1"
	task.Spec.Source.RequestedBy = "github:smana"
	task.Spec.Budget.TaskTokens = 1500000
	task.Status.Usage.Tokens = 189093
	task.Status.PullRequest = &v1alpha1.PullRequestRef{Number: 2239, URL: "https://github.com/Smana/cloud-native-ref/pull/2239",
		Author: "ogenki-agent-factory[bot]", Reviewers: []string{"smana"}}
	task.Status.Runs = []v1alpha1.RunRecord{{ID: "cf4ato2x", Role: "implementer", Trigger: "human"}}
	f := factsOf(task)
	if f.Phase != "AwaitingHuman" || f.Run.ID != "cf4ato2x" || f.Budget.UsedTokens != 189093 ||
		f.Issue.Number != 2238 || f.Issue.Author != "dev1" || f.Issue.LabelledBy != "smana" ||
		f.PR.Number != 2239 || f.PR.Author != "ogenki-agent-factory[bot]" || f.PR.Reviewers[0] != "smana" {
		t.Fatalf("facts %+v", f)
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("invalid: %v", err)
	}
}

// A task with no run, no cap, no issue and no PR has a phase alone; a RunLore task was labelled by
// nobody.
func TestFactsOfABareTask(t *testing.T) {
	task := issueTask("3buqdlot", 0, "x")
	task.Spec.Source.RequestedBy = "system:runlore"
	task.Status.Phase = v1alpha1.PhaseQueued
	if f := factsOf(task); f.Phase != "Queued" || f.Run != nil || f.Budget != nil || f.Issue != nil || f.PR != nil {
		t.Fatalf("facts %+v", f)
	}
	task.Spec.Issue = 7
	if f := factsOf(task); f.Issue == nil || f.Issue.LabelledBy != "" || f.Validate() != nil {
		t.Fatalf("facts %+v", f.Issue)
	}
}

func TestFactsArePostedOncePerChange(t *testing.T) {
	g := newRig(t, awaiting())
	tk := stored(t, g.c)
	ctx := t.Context()
	if err := g.r.postFacts(ctx, tk); err != nil {
		t.Fatal(err)
	}
	if g.r.factsDue(tk) {
		t.Fatal("the same facts are due again")
	}
	tk.Status.Phase = v1alpha1.PhaseReviewing
	if !g.r.factsDue(tk) {
		t.Fatal("a phase change is not due")
	}
	if err := g.r.postFacts(ctx, tk); err != nil {
		t.Fatal(err)
	}
	if got := g.log.factsSeqs; !slices.Equal(got, []int64{1, 2}) || g.log.facts[2].Phase != "Reviewing" {
		t.Fatalf("seqs %v, want [1 2]", got)
	}
	// Ruling SK: each seq is the task's before it is posted.
	if s := stored(t, g.c).Status; s.RoomSeq != 2 || s.Facts == nil || s.Facts.Seq != 2 {
		t.Fatalf("persisted %d %+v", s.RoomSeq, s.Facts)
	}
}

func TestAFailedFactsPostReusesItsSeq(t *testing.T) {
	g := newRig(t, awaiting())
	tk := stored(t, g.c)
	g.log.failFacts = errors.New("broker down")
	if err := g.r.postFacts(t.Context(), tk); err == nil {
		t.Fatal("the failure is swallowed")
	}
	g.log.failFacts = nil
	if err := g.r.postFacts(t.Context(), tk); err != nil {
		t.Fatal(err)
	}
	if got := g.log.factsSeqs; !slices.Equal(got, []int64{1, 1}) || tk.Status.RoomSeq != 1 {
		t.Fatalf("seqs %v roomSeq %d: a retry must resend seq 1", got, tk.Status.RoomSeq)
	}
}

// No log for the room yet, or no permission for the factory yet (FR-1), is a wait as it is for the
// snapshot: no error, and the facts stay due under the seq they took. So is a sealed room, which
// would otherwise hold the task in error backoff for good.
func TestARefusedFactsPostWaits(t *testing.T) {
	for name, refuse := range map[string]func(*fakeLog){
		"no room log":   func(l *fakeLog) { l.noRoom = true },
		"not permitted": func(l *fakeLog) { l.noPermit = true },
		"sealed":        func(l *fakeLog) { l.failFacts = &rooms.APIError{Status: 410, Reason: "sealed"} },
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, awaiting())
			tk := stored(t, g.c)
			refuse(g.log)
			if err := g.r.postFacts(t.Context(), tk); err != nil {
				t.Fatal(err)
			}
			if !g.r.factsDue(tk) || tk.Status.Facts.Seq != 1 || stored(t, g.c).Status.RoomSeq != 1 {
				t.Fatalf("due %v, ledger %+v", g.r.factsDue(tk), tk.Status.Facts)
			}
		})
	}
}

// Every written phase reaches the room as facts, after the write; the snapshot stays the room's one
// task_state message.
func TestEveryPhaseReachesTheRoom(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	tk := g.reconcile(t, "3buqdlot", 3) // Received → Triaged → Queued → Implementing
	var phases []string
	for _, s := range g.log.factsSeqs {
		phases = append(phases, g.log.facts[s].Phase)
	}
	last := g.log.facts[tk.Status.Facts.Seq]
	if !slices.Equal(phases, []string{"Queued", "Implementing"}) || !tk.Status.Facts.Posted || tk.Status.Facts.Seq != tk.Status.RoomSeq ||
		last.Run == nil || last.Run.ID != rid(0) || last.Issue.Number != 7 || last.Issue.LabelledBy != "Smana" || len(g.log.states) != 1 {
		t.Fatalf("phases %v, ledger %+v, facts %+v, %d task_state", phases, tk.Status.Facts, last, len(g.log.states))
	}
	g.reconcile(t, "3buqdlot", 1)
	if len(g.log.factsSeqs) != 2 {
		t.Fatalf("unchanged facts were posted again: %v", g.log.factsSeqs)
	}
}

// An ended task keeps reconciling until the room has its last facts. A failed post never holds the
// end: it is returned, and retried under the same seq.
func TestAnEndedTaskWritesItsLastFacts(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	tk := g.reconcile(t, "3buqdlot", 2) // Queued
	tk.Annotations = map[string]string{v1alpha1.AnnotationStop: "true"}
	if err := g.c.Update(t.Context(), tk); err != nil {
		t.Fatal(err)
	}
	g.log.failFacts = errors.New("broker down")
	if _, err := g.r.Reconcile(t.Context(), reqFor("3buqdlot")); err == nil {
		t.Fatal("the failed post is not returned")
	}
	if s := stored(t, g.c).Status; s.Phase != v1alpha1.PhaseStopped || s.Facts.Posted || s.Facts.Seq != 2 {
		t.Fatalf("%s %+v", s.Phase, s.Facts)
	}
	g.log.failFacts = nil
	g.reconcile(t, "3buqdlot", 2)
	if got := g.log.factsSeqs; !slices.Equal(got, []int64{1, 2, 2}) || g.log.facts[2].Phase != "Stopped" {
		t.Fatalf("seqs %v, last %+v", got, g.log.facts[2])
	}
}
