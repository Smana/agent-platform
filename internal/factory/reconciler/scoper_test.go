// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/runs"
)

// scoperRig is a scoper-mode task with the review note, past its first run's start.
func scoperRig(t *testing.T) *rig {
	t.Helper()
	g := scopeRig(t, config.ScopeScoper)
	g.r.Cfg.Tiers["light"] = config.Tier{Model: "agent-default", RunTokens: 300_000, TaskTokens: 600_000, RunMinutes: 20}
	g.f.SetFile(".github/agent-factory/steering/review.md", reviewNote)
	g.reconcile(t, "3buqdlot", 3)
	return g
}

// Offline proof 4: mode scoper starts a triager run first, at the light tier's caps, with the
// scoping brief carrying the note and the snapshot.
func TestTheScoperRunsFirstAtTheLightCaps(t *testing.T) {
	g := scoperRig(t)
	s := g.runs.specs[rid(0)]
	if s.Role != "triager" || s.Tier != "light" || s.MaxTokens != 300_000 || s.MaxMinutes != 20 || s.Model != "agent-default" {
		t.Fatalf("the scoper's claim %+v", s)
	}
	for _, want := range []string{"You are the scoper", "STEERING-n0nce234", "TASK-DATA-n0nce234", "room_handoff", "one question", "never invent one"} {
		if !strings.Contains(s.TaskText, want) {
			t.Errorf("the scoping brief lacks %q", want)
		}
	}
	if s.Loop != nil {
		t.Fatal("the loop policy closes implementer runs only (part 2 R5)")
	}
}

// scopeHandoff appends run n's handoff carrying sc to the rig's room.
func scopeHandoff(g *rig, n int, sc envelope.Scope) {
	g.log.evs = append(g.log.evs, envelope.Event{Seq: int64(len(g.log.evs) + 1), RunID: rid(n), Type: envelope.Handoff,
		Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:" + rid(n), Role: "triager"},
		Payload: envelope.Must(envelope.HandoffPayload{FromRole: "triager", ToRole: "implementer", Summary: "Scoped.",
			Commit: strings.Repeat("c", 40), Scope: &sc})})
}

func readyScope() envelope.Scope {
	return envelope.Scope{State: "ready", Task: "Fix the link to runbook 07 and change nothing else.",
		Touches: []string{"docs/user-guide.md"}, Plan: []string{"Point the link at runbook 08."},
		Assumptions: []string{"Runbook 08 replaces 07."},
		Acceptance:  []envelope.Acceptance{{Check: "task docs (CI: validate)", Proves: "every relref resolves"}}}
}

func questionScope() envelope.Scope {
	sc := readyScope()
	sc.State, sc.Question = "question", "Which dashboard should the panel move to: agent-run or agent-factory?"
	sc.Assumptions = []string{"agent-run, where the per-run cost panels are."}
	return sc
}

// revoke and lose shape a claim as a person's revocation and a lost pod leave it.
func (f *fakeRuns) revoke(id, why string) { r := f.runs[id]; r.Revoked = why; f.runs[id] = r }
func (f *fakeRuns) lose(id string) {
	r := f.runs[id]
	r.Phase, r.Reason = "Failed", runs.ReasonPodLost
	f.runs[id] = r
}

// scoperRigOf is scoperRig on a task shaped by edit before its triage.
func scoperRigOf(t *testing.T, edit func(*v1alpha1.Task)) *rig {
	t.Helper()
	tk := issueTask("3buqdlot", 7, "# Fix the link\n\n## Acceptance check\nthe link resolves")
	edit(tk)
	g := newRig(t, tk)
	g.r.Cfg.Loops = map[string]config.Loop{"review": {Scope: config.ScopeScoper}}
	g.r.Cfg.Tiers["light"] = config.Tier{Model: "agent-default", RunTokens: 300_000, TaskTokens: 600_000, RunMinutes: 20}
	g.reconcile(t, "3buqdlot", 3)
	return g
}

// endScoper ends the scoper run with phase and reason, then reconciles once.
func endScoper(t *testing.T, g *rig, phase, reason string) *v1alpha1.Task {
	t.Helper()
	g.runs.set(rid(0), phase)
	g.log.end(rid(0), phase, reason)
	return g.reconcile(t, "3buqdlot", 1)
}

// SC9, offline proof 4: ready starts the implementer, whose first brief carries the scope.
func TestAReadyScopeStartsTheImplementer(t *testing.T) {
	g := scoperRig(t)
	scopeHandoff(g, 0, readyScope())
	tk := endScoper(t, g, "Succeeded", "agent_finished")
	if tk.Status.Phase != v1alpha1.PhaseQueued || tk.Status.Scope == nil || tk.Status.Scope.State != "ready" ||
		tk.Status.Scope.Seq == 0 || tk.Status.NextTrigger != "initial" {
		t.Fatalf("%s %+v %q", tk.Status.Phase, tk.Status.Scope, tk.Status.NextTrigger)
	}
	tk = g.reconcile(t, "3buqdlot", 1)
	s := g.runs.specs[rid(1)]
	if s.Role != "implementer" || tk.Status.Runs[1].Trigger != "initial" || s.Tier != "standard" {
		t.Fatalf("the implementer %+v, trigger %q", s, tk.Status.Runs[1].Trigger)
	}
	if !strings.Contains(s.TaskText, "SCOPE-DATA-n0nce234") || !strings.Contains(s.TaskText, "Point the link at runbook 08.") {
		t.Fatalf("the implementer's brief lacks the scope:\n%s", s.TaskText)
	}
	if !slices.Contains(g.metrics.recorded, "scoper review ready") {
		t.Fatalf("metrics %v", g.metrics.recorded)
	}
}

// SC11, SO3 (a): no scope, the caps or a stuck run start the implementer unscoped.
func TestAScoperWithoutAScopeFailsOpen(t *testing.T) {
	for name, c := range map[string]struct{ phase, reason, state string }{
		"finished without one": {"Succeeded", "agent_finished", "missing"},
		"its run cap":          {"BudgetExhausted", "budget-run", "failed"},
		"an agent error":       {"Failed", "agent_error", "failed"},
	} {
		t.Run(name, func(t *testing.T) {
			g := scoperRig(t)
			tk := endScoper(t, g, c.phase, c.reason)
			if tk.Status.Phase != v1alpha1.PhaseQueued || tk.Status.Scope == nil || tk.Status.Scope.State != c.state {
				t.Fatalf("%s %+v", tk.Status.Phase, tk.Status.Scope)
			}
			g.reconcile(t, "3buqdlot", 1)
			if s := g.runs.specs[rid(1)]; s.Role != "implementer" || !strings.Contains(s.TaskText, "STEERING-") ||
				strings.Contains(s.TaskText, "SCOPE-DATA-") {
				t.Fatalf("an unscoped implementer with steering: %+v", s)
			}
		})
	}
}

// SC11: a person's stop escalates as any run's; the run's loss to its infrastructure resumes it.
func TestAStoppedScoperEscalatesAndALostOneResumes(t *testing.T) {
	g := scoperRig(t)
	g.runs.revoke(rid(0), "manual")
	if tk := endScoper(t, g, "Failed", "manual"); tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Scope != nil {
		t.Fatalf("%s %+v", tk.Status.Phase, tk.Status.Scope)
	}
	g = scoperRig(t)
	g.runs.lose(rid(0))
	g.log.end(rid(0), "Failed", "pod_lost")
	tk := g.reconcile(t, "3buqdlot", 2)
	if s := g.runs.specs[rid(1)]; s.Role != "triager" || tk.Status.Scope != nil || !strings.Contains(s.TaskText, "interrupted by the platform") {
		t.Fatalf("the resumed scoper %+v", s)
	}
}

// SC8: question gives Escalated, needs_author, one comment quoting the question and the first
// assumption for a public task, the label, and no Ended comment.
func TestAQuestionBouncesToTheAuthor(t *testing.T) {
	g := scoperRig(t)
	before := len(g.f.Comments(7))
	scopeHandoff(g, 0, questionScope())
	tk := endScoper(t, g, "Succeeded", "agent_finished")
	if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "needs_author" || tk.Status.Scope.State != "question" {
		t.Fatalf("%s %s %+v", tk.Status.Phase, tk.Status.Reason, tk.Status.Scope)
	}
	c := g.f.Comments(7)[before:]
	if len(c) != 1 || !strings.Contains(c[0], "Which dashboard should the panel move to") ||
		!strings.Contains(c[0], "Assumption: agent-run") || !strings.Contains(c[0], "/factory retry") ||
		!strings.Contains(c[0], "factory/ready") || strings.Contains(c[0], "Tokens used") {
		t.Fatalf("comments %q", c)
	}
	if !slices.Equal(g.f.Added(7), []string{"factory/needs-author"}) || !slices.Contains(g.metrics.recorded, "scoper review question") {
		t.Fatalf("labels %v metrics %v", g.f.Added(7), g.metrics.recorded)
	}
}

// C7: an internal task's question stays in the room behind a link.
func TestAnInternalQuestionStaysInTheRoom(t *testing.T) {
	g := scoperRigOf(t, func(tk *v1alpha1.Task) { tk.Spec.DataClass = "internal" })
	before := len(g.f.Comments(7))
	scopeHandoff(g, 0, questionScope())
	endScoper(t, g, "Succeeded", "agent_finished")
	c := g.f.Comments(7)[before:]
	if len(c) != 1 || strings.Contains(c[0], "Which dashboard") || !strings.Contains(c[0], "/r/3buqdlot") {
		t.Fatalf("comments %q", c)
	}
}

// SC8: a task without an issue (a schedule) never bounces: its question becomes an assumption.
func TestAScheduleNeverBounces(t *testing.T) {
	g := scoperRigOf(t, func(tk *v1alpha1.Task) { tk.Spec.Issue, tk.Spec.Source.Kind = 0, "schedule" })
	scopeHandoff(g, 0, questionScope())
	if got := endScoper(t, g, "Succeeded", "agent_finished"); got.Status.Phase != v1alpha1.PhaseQueued || got.Status.Scope.State != "assumed" {
		t.Fatalf("%s %+v", got.Status.Phase, got.Status.Scope)
	}
}

// R02: the scoper's handoff, whose commit is the base it read, is never a reported head.
func TestAScopersCommitIsNoReportedHead(t *testing.T) {
	g := scoperRig(t)
	scopeHandoff(g, 0, readyScope())
	tk := endScoper(t, g, "Succeeded", "agent_finished")
	if impl := implementerRuns(tk); impl[rid(0)] {
		t.Fatal("the scoper's run counts as an implementer's")
	}
}

// A scope counts only on the scoper run's handoff to the implementer: one to any other role is
// ignored, and the task proceeds unscoped.
func TestAScopeOnAnotherRolesHandoffIsIgnored(t *testing.T) {
	g := scoperRig(t)
	scopeHandoff(g, 0, readyScope())
	var p envelope.HandoffPayload
	last := &g.log.evs[len(g.log.evs)-1]
	_ = json.Unmarshal(last.Payload, &p)
	p.ToRole = "reviewer"
	last.Payload = envelope.Must(p)
	if tk := endScoper(t, g, "Succeeded", "agent_finished"); tk.Status.Scope == nil || tk.Status.Scope.State != "missing" {
		t.Fatalf("%+v", tk.Status.Scope)
	}
}

// A resumed scoper pushed nothing: its notice does not send it to a branch.
func TestAResumedScopersNoticeNamesNoBranch(t *testing.T) {
	g := scoperRig(t)
	g.runs.lose(rid(0))
	g.log.end(rid(0), "Failed", "pod_lost")
	g.reconcile(t, "3buqdlot", 2)
	if s := g.runs.specs[rid(1)]; !strings.Contains(s.TaskText, "interrupted by the platform") || strings.Contains(s.TaskText, "pushed") {
		t.Fatalf("%q", s.TaskText)
	}
}

// SC11: a stuck scoper's claim is deleted and the implementer starts unscoped.
func TestAStuckScoperFailsOpen(t *testing.T) {
	g := scoperRig(t)
	g.runs.set(rid(0), "Running")
	g.reconcile(t, "3buqdlot", 1) // the stuck clock starts at the first Running sight
	g.r.Now = func() time.Time { return now.Add(StuckAfter + time.Minute) }
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseQueued || tk.Status.Scope == nil || tk.Status.Scope.State != "failed" || len(g.runs.runs) != 0 {
		t.Fatalf("%s %+v %v", tk.Status.Phase, tk.Status.Scope, g.runs.runs)
	}
}

// SC11: a scoper whose claim vanished (not a person's delete) fails open.
func TestAVanishedScoperFailsOpen(t *testing.T) {
	g := scoperRig(t)
	delete(g.runs.runs, rid(0))
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseQueued || tk.Status.Scope == nil || tk.Status.Scope.State != "failed" {
		t.Fatalf("%s %+v", tk.Status.Phase, tk.Status.Scope)
	}
}

// S14: capacity and budget ends are not the scoper's failure: they escalate as any run's.
func TestCapacityAndBudgetEndsEscalateTheScoper(t *testing.T) {
	for _, reason := range []string{"run_unschedulable", "resume_budget"} {
		t.Run(reason, func(t *testing.T) {
			g := scoperRig(t)
			if tk := endScoper(t, g, "Failed", reason); tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Scope != nil {
				t.Fatalf("%s %+v", tk.Status.Phase, tk.Status.Scope)
			}
		})
	}
}

func taskKey(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "agent-system", Name: name}
}

// bounced is a scoper rig whose scoper asked its question.
func bounced(t *testing.T) *rig {
	t.Helper()
	g := scoperRig(t)
	scopeHandoff(g, 0, questionScope())
	endScoper(t, g, "Succeeded", "agent_finished")
	return g
}

// SC8 (b): /factory retry proceeds on the assumptions, with no second scoper run.
func TestARetryProceedsOnTheAssumption(t *testing.T) {
	g := bounced(t)
	g.r.Now = func() time.Time { return now.Add(time.Hour) } // the command must postdate the escalation
	g.f.SetComments(7, retryBy(77, "smana", -30*time.Minute))
	tk := g.reconcile(t, "3buqdlot", 2)
	s := g.runs.specs[rid(1)]
	if tk.Status.Scope.State != "assumed" || s.Role != "implementer" || !strings.Contains(s.TaskText, "first assumption") {
		t.Fatalf("%+v %+v", tk.Status.Scope, s)
	}
	if !slices.Equal(g.f.Removed(7), []string{"factory/needs-author"}) || !slices.Contains(g.metrics.recorded, "bounce review retry") {
		t.Fatalf("removed %v metrics %v", g.f.Removed(7), g.metrics.recorded)
	}
}

// SC8 (a) and (c): a superseding re-label and the expiry each count their exit and drop the label.
func TestTheOtherExitsAreCounted(t *testing.T) {
	g := bounced(t)
	var tk v1alpha1.Task
	_ = g.c.Get(t.Context(), taskKey("3buqdlot"), &tk)
	tk.Annotations = map[string]string{v1alpha1.AnnotationStop: "superseded"}
	_ = g.c.Update(t.Context(), &tk)
	if got := g.reconcile(t, "3buqdlot", 1); got.Status.Phase != v1alpha1.PhaseStopped || got.Status.Scope.State != "question" {
		t.Fatalf("%s %+v", got.Status.Phase, got.Status.Scope)
	}
	if !slices.Contains(g.metrics.recorded, "bounce review relabel") || !slices.Equal(g.f.Removed(7), []string{"factory/needs-author"}) {
		t.Fatalf("metrics %v removed %v", g.metrics.recorded, g.f.Removed(7))
	}
	g = bounced(t)
	g.r.Cfg.Caps.EscalatedHours = new(168)
	g.r.Now = func() time.Time { return now.Add(200 * time.Hour) }
	if got := g.reconcile(t, "3buqdlot", 1); got.Status.Reason != "escalation_expired" || !slices.Contains(g.metrics.recorded, "bounce review expired") ||
		!slices.Equal(g.f.Removed(7), []string{"factory/needs-author"}) {
		t.Fatalf("%s %v removed %v", got.Status.Reason, g.metrics.recorded, g.f.Removed(7))
	}
}

// A stop that is no re-label (the annotation, the kill switch) still drops the label, and counts
// no exit: only the three exits are counted.
func TestAPlainStopDropsTheLabelAndCountsNothing(t *testing.T) {
	g := bounced(t)
	var tk v1alpha1.Task
	_ = g.c.Get(t.Context(), taskKey("3buqdlot"), &tk)
	tk.Annotations = map[string]string{v1alpha1.AnnotationStop: "true"}
	_ = g.c.Update(t.Context(), &tk)
	if got := g.reconcile(t, "3buqdlot", 1); got.Status.Phase != v1alpha1.PhaseStopped {
		t.Fatalf("%s", got.Status.Phase)
	}
	if !slices.Equal(g.f.Removed(7), []string{"factory/needs-author"}) {
		t.Fatalf("removed %v", g.f.Removed(7))
	}
	for _, m := range g.metrics.recorded {
		if strings.HasPrefix(m, "bounce ") {
			t.Fatalf("an exit was counted: %v", g.metrics.recorded)
		}
	}
}

// A maintainer who cannot write does not end the bounce either.
func TestAMaintainerWithoutWriteDoesNotEndTheBounce(t *testing.T) {
	g := bounced(t)
	g.r.Now = func() time.Time { return now.Add(time.Hour) }
	g.r.CanWrite = func(context.Context, string, string) (bool, error) { return false, nil }
	g.f.SetComments(7, retryBy(77, "smana", -30*time.Minute))
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Scope.State != "question" || len(g.f.Removed(7)) != 0 {
		t.Fatalf("%s %+v %v", tk.Status.Phase, tk.Status.Scope, g.f.Removed(7))
	}
}

// An answer from anyone else does nothing: a retry by a non-maintainer leaves the bounce as it is.
func TestOnlyAMaintainersRetryProceeds(t *testing.T) {
	g := bounced(t)
	g.r.Now = func() time.Time { return now.Add(time.Hour) }
	g.f.SetComments(7, retryBy(77, "octocat", -30*time.Minute))
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Scope.State != "question" || len(g.f.Removed(7)) != 0 {
		t.Fatalf("%s %+v %v", tk.Status.Phase, tk.Status.Scope, g.f.Removed(7))
	}
}
