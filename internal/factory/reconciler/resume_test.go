// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/rooms"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// Disruption design §4: a run lost to its infrastructure resumes on its own, capped and inside the
// task's budget; a run that failed on its own still escalates.

// lose ends run id as the composition records a pod lost to its node: Failed with the AgentRun's
// reason, and the broker's pod_lost in the room.
func (g *rig) lose(id, reason string) {
	r := g.runs.runs[id]
	r.Phase, r.Reason = "Failed", reason
	g.runs.runs[id] = r
	g.log.end(id, "Failed", "pod_lost")
}

func TestALostImplementerResumesOnTheSameBranchAndRoom(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.reconcile(t, "3buqdlot", 3)
	g.lose(rid(0), runs.ReasonDisrupted)
	tk := g.reconcile(t, "3buqdlot", 2) // Implementing → Queued (resume) → Implementing
	s := g.runs.specs[rid(1)]
	if tk.Status.Phase != v1alpha1.PhaseImplementing || tk.Status.Resumes != 1 || tk.Status.Runs[1].Trigger != "resume" ||
		s.Branch != "agent/3buqdlot" || s.RoomRef != "3buqdlot" ||
		!strings.HasPrefix(s.TaskText, "The previous run of agent factory task 3buqdlot was interrupted") || !strings.Contains(s.TaskText, "TASK-DATA-") {
		t.Fatalf("%s resumes=%d %+v %+v", tk.Status.Phase, tk.Status.Resumes, tk.Status.Runs, s)
	}
	if c := strings.Join(g.f.Comments(7), "\n"); !strings.Contains(c, "the sandbox was lost (spot reclaim or eviction); resuming automatically (1/2)") {
		t.Fatalf("%q", c)
	}
	if !slices.Contains(g.metrics.recorded, "resumed Disrupted") {
		t.Fatalf("%q", g.metrics.recorded)
	}
}

func TestResumesStopAtTheCapThenEscalate(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.reconcile(t, "3buqdlot", 3)
	for i := range 2 {
		g.lose(rid(i), runs.ReasonPodLost)
		if tk := g.reconcile(t, "3buqdlot", 2); tk.Status.Resumes != int32(i+1) || tk.Status.Phase != v1alpha1.PhaseImplementing {
			t.Fatalf("resume %d: %s %d", i+1, tk.Status.Phase, tk.Status.Resumes)
		}
	}
	g.lose(rid(2), runs.ReasonPodLost)
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "resumes_exhausted" || len(g.runs.specs) != 3 {
		t.Fatalf("%s %s %d runs", tk.Status.Phase, tk.Status.Reason, len(g.runs.specs))
	}
	// The cap is per task, as the user guide says: a /factory retry runs again, but gives no resume back.
	g.f.SetComments(7, retryBy(77, "smana", -time.Minute))
	g.reconcile(t, "3buqdlot", 2)
	g.lose(rid(3), runs.ReasonPodLost)
	tk = g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "resumes_exhausted" || tk.Status.Resumes != 2 || len(g.runs.specs) != 4 {
		t.Fatalf("after a retry: %s %s resumes=%d %d runs", tk.Status.Phase, tk.Status.Reason, tk.Status.Resumes, len(g.runs.specs))
	}
}

func TestAFailedHarnessIsNeverResumed(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.reconcile(t, "3buqdlot", 3)
	g.lose(rid(0), "PodFailed") // the broker's pod_lost reads the same for a crash
	tk := g.reconcile(t, "3buqdlot", 2)
	if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Resumes != 0 || len(g.runs.specs) != 1 || tk.Status.Reason != "PodFailed" {
		t.Fatalf("%s %s %d %d runs", tk.Status.Phase, tk.Status.Reason, tk.Status.Resumes, len(g.runs.specs))
	}
	// The AgentRun's reason, not the broker's: a crash is never narrated as a reclaim.
	if c := strings.Join(g.f.Comments(7), "\n"); !strings.Contains(c, "the sandbox failed on its own") || strings.Contains(c, "spot reclaim") {
		t.Fatalf("%q", c)
	}
}

// The task cap is enforced on this path although budgets.enforceTask is false: a resume needs a
// whole RunTokens left under TaskTokens (standard tier: 1.5 M of 3 M).
func TestAResumeNeedsARunsWorthOfTheTaskBudget(t *testing.T) {
	for name, c := range map[string]struct {
		used      int64
		want, why string
	}{
		"a run's worth left":      {1_500_000, v1alpha1.PhaseImplementing, ""},
		"less than a run's worth": {1_500_001, v1alpha1.PhaseEscalated, "resume_budget"},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, issueTask("3buqdlot", 7, "x"))
			g.reconcile(t, "3buqdlot", 3)
			r := g.runs.runs[rid(0)]
			r.Tokens = c.used
			g.runs.runs[rid(0)] = r
			g.lose(rid(0), runs.ReasonDisrupted)
			if tk := g.reconcile(t, "3buqdlot", 2); tk.Status.Phase != c.want || c.why != "" && tk.Status.Reason != c.why {
				t.Fatalf("%s %s %+v resumes=%d", tk.Status.Phase, tk.Status.Reason, tk.Status.Usage, tk.Status.Resumes)
			}
		})
	}
}

func TestALostTriagerRunsTheTriagerAgain(t *testing.T) {
	g := investigateRig(t)
	g.reconcile(t, "3buqdlot", 3)
	g.lose(rid(0), runs.ReasonDisrupted)
	tk := g.reconcile(t, "3buqdlot", 2)
	if s := g.runs.specs[rid(1)]; tk.Status.Phase != v1alpha1.PhaseImplementing || s.Role != "triager" || tk.Status.Resumes != 1 ||
		!strings.HasPrefix(s.TaskText, "The previous run") {
		t.Fatalf("%s %d %+v", tk.Status.Phase, tk.Status.Resumes, s)
	}
}

// #20's latch is per run: a resumed run is a new record, unseen, so one never admitted is still
// bounded although the run it replaces had started.
func TestAResumedRunThatNeverStartsIsStillBounded(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.reconcile(t, "3buqdlot", 3)
	g.runs.set(rid(0), "Running")
	g.reconcile(t, "3buqdlot", 1)
	g.lose(rid(0), runs.ReasonDisrupted)
	g.reconcile(t, "3buqdlot", 2)
	g.r.Now = func() time.Time { return now.Add(31 * time.Minute) }
	tk := g.reconcile(t, "3buqdlot", 1)
	if _, ok := g.runs.runs[rid(1)]; ok || tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "run_unschedulable" ||
		tk.Status.Runs[0].Phase != "Failed" {
		t.Fatalf("claim kept %t, %s %s %+v", ok, tk.Status.Phase, tk.Status.Reason, tk.Status.Runs)
	}
}

// A resumed run carries on the run it replaces: a resumed revision still goes back to the maintainer.
func TestAResumedRunCarriesOnItsCause(t *testing.T) {
	tk := &v1alpha1.Task{Status: v1alpha1.TaskStatus{Runs: []v1alpha1.RunRecord{{Trigger: "initial"}, {Trigger: "human"}, {Trigger: "resume"}, {Trigger: "resume"}}}}
	if got := cause(tk); got != "human" {
		t.Fatalf("cause = %q, want human", got)
	}
	if got := cause(&v1alpha1.Task{}); got != "initial" {
		t.Fatalf("cause of no run = %q", got)
	}
}

// R6: the notice must not push the longest first brief past AgentRun's 16 KiB task.text.
func TestAResumedFirstBriefFitsTheTaskText(t *testing.T) {
	tk := issueTask("3buqdlot", 7, strings.Repeat("x", config.MaxTextCeiling))
	if n := len(resumed(tk, "resume") + FirstBrief(tk, "n0nce234")); n > 16384 {
		t.Fatalf("a resumed brief of a %d-byte issue is %d bytes, over 16384", config.MaxTextCeiling, n)
	}
}

func TestALostReviewerRunsAgainWithoutARound(t *testing.T) {
	g := pairRig(t)
	g.lose(rid(1), runs.ReasonDisrupted)
	tk := g.reconcile(t, "3buqdlot", 2)
	if tk.Status.Phase != v1alpha1.PhaseReviewing || tk.Status.ReviewRounds != 0 || tk.Status.Resumes != 1 ||
		g.runs.specs[rid(2)].Role != "reviewer" || tk.Status.Runs[2].HeadSHA != head1 {
		t.Fatalf("%s rounds=%d resumes=%d %+v", tk.Status.Phase, tk.Status.ReviewRounds, tk.Status.Resumes, tk.Status.Runs)
	}
	if issue := strings.Join(g.f.Comments(7), "\n"); !strings.Contains(issue, "The new review run uses no review round.") {
		t.Fatalf("%s", issue)
	}
}

// A verdict recorded before the loss still counts: the review is done, nothing re-runs.
func TestALostReviewersVerdictStillCounts(t *testing.T) {
	g := pairRig(t)
	g.log.verdict(rid(1), "approve", head1[:7], "lgtm")
	g.lose(rid(1), runs.ReasonDisrupted)
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAwaitingCI || tk.Status.Resumes != 0 {
		t.Fatalf("%s %d", tk.Status.Phase, tk.Status.Resumes)
	}
}

// Past the cap a lost reviewer is a run without a verdict, as before: it spends a round.
func TestPastTheCapALostReviewerSpendsARound(t *testing.T) {
	g := pairRig(t)
	var tk v1alpha1.Task
	if err := g.c.Get(t.Context(), types.NamespacedName{Namespace: "agent-system", Name: "3buqdlot"}, &tk); err != nil {
		t.Fatal(err)
	}
	tk.Status.Resumes = 2
	if err := g.c.Status().Update(t.Context(), &tk); err != nil {
		t.Fatal(err)
	}
	g.lose(rid(1), runs.ReasonDisrupted)
	if got := g.reconcile(t, "3buqdlot", 2); got.Status.ReviewRounds != 1 || got.Status.Resumes != 2 || got.Status.Verdict != "none" {
		t.Fatalf("rounds=%d resumes=%d verdict=%s", got.Status.ReviewRounds, got.Status.Resumes, got.Status.Verdict)
	}
}

// Review I1: a resumed revision carries the review it revises. The room after the lost run's own
// start holds no verdict: the resumed run is briefed with the lost run's brief.
func TestAResumedRevisionCarriesTheReviewItRevises(t *testing.T) {
	g := pairRig(t)
	g.log.verdict(rid(1), "changes", head1, "Add a test for the new link.")
	g.finish(rid(1), "Succeeded", "agent_finished")
	g.reconcile(t, "3buqdlot", 2) // → the revision rid(2)
	g.lose(rid(2), runs.ReasonDisrupted)
	tk := g.reconcile(t, "3buqdlot", 2)
	s := g.runs.specs[rid(3)]
	if tk.Status.Phase != v1alpha1.PhaseImplementing || tk.Status.Runs[3].Trigger != "resume" || tk.Status.ReviewRounds != 1 ||
		!strings.HasPrefix(s.TaskText, ResumeNotice(tk)) || !strings.Contains(s.TaskText, "Add a test for the new link.") {
		t.Fatalf("%s rounds=%d %+v\n%s", tk.Status.Phase, tk.Status.ReviewRounds, tk.Status.Runs, s.TaskText)
	}
	// Lost again: one notice, never two.
	g.lose(rid(3), runs.ReasonPodLost)
	g.reconcile(t, "3buqdlot", 2)
	if s := g.runs.specs[rid(4)]; strings.Count(s.TaskText, "was interrupted by the platform") != 1 || !strings.Contains(s.TaskText, "Add a test for the new link.") {
		t.Fatalf("%s", s.TaskText)
	}
}

// Review I1: with the lost run's claim gone the brief is rebuilt, reading the room from the start
// of the run before the resumed chain, where the verdict is.
func TestAResumedRevisionWithoutItsClaimRereadsTheReview(t *testing.T) {
	g := pairRig(t)
	g.log.verdict(rid(1), "changes", head1, "Add a test for the new link.")
	g.finish(rid(1), "Succeeded", "agent_finished")
	g.reconcile(t, "3buqdlot", 2)
	g.lose(rid(2), runs.ReasonDisrupted)
	g.reconcile(t, "3buqdlot", 1) // → Queued (resume)
	_ = g.runs.Delete(t.Context(), rid(2))
	tk := g.reconcile(t, "3buqdlot", 1)
	if s := g.runs.specs[rid(3)]; tk.Status.Phase != v1alpha1.PhaseImplementing || !strings.HasPrefix(s.TaskText, ResumeNotice(tk)) ||
		!strings.Contains(s.TaskText, "Add a test for the new link.") {
		t.Fatalf("%s\n%s", tk.Status.Phase, s.TaskText)
	}
}

// Review I1 and I3 (M-B, M-C): the queued review a lost revision consumed is quoted again, and a
// maintainer's revision resumed still goes back to the maintainer.
func TestAResumedMaintainerRevisionKeepsItsReviewAndItsCause(t *testing.T) {
	g := newRig(t, awaiting(), roomOf("3buqdlot"))
	latest := changes(901, "Smana", "Use the relative link.", 5*time.Minute)
	latest.Comments = []forge.ReviewComment{{Path: "docs/a.md", Line: 3, Body: "here"}}
	g.f.SetPR(pr12(latest))
	g.reconcile(t, "3buqdlot", 2) // → Queued (human) → the revision rid(1), which consumes 901
	g.lose(rid(1), runs.ReasonPodLost)
	tk := g.reconcile(t, "3buqdlot", 2)
	if s := g.runs.specs[rid(2)]; tk.Status.Phase != v1alpha1.PhaseImplementing || tk.Status.Runs[2].Trigger != "resume" ||
		!strings.Contains(s.TaskText, "docs/a.md:3") || !strings.Contains(s.TaskText, "Use the relative link.") {
		t.Fatalf("%s %+v\n%s", tk.Status.Phase, tk.Status.Runs, s.TaskText)
	}
	if !slices.Contains(g.metrics.recorded, "resumed PodLost") {
		t.Fatalf("%q", g.metrics.recorded)
	}
	g.finish(rid(2), "Succeeded", "agent_finished")
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman {
		t.Fatalf("a resumed maintainer revision goes back to the maintainer: %s", tk.Status.Phase)
	}
}

// Review I3 (M-A): a lost tester re-runs as the tester, without a round.
func TestALostTesterRunsAgainAsTheTester(t *testing.T) {
	g := teamRig(t, "trio", nil) // rid(1) is the tester
	g.lose(rid(1), runs.ReasonDisrupted)
	tk := g.reconcile(t, "3buqdlot", 2)
	if tk.Status.Phase != v1alpha1.PhaseReviewing || g.runs.specs[rid(2)].Role != "tester" || tk.Status.ReviewRounds != 0 || tk.Status.Resumes != 1 {
		t.Fatalf("%s %s rounds=%d resumes=%d", tk.Status.Phase, g.runs.specs[rid(2)].Role, tk.Status.ReviewRounds, tk.Status.Resumes)
	}
}

// Review M1: an implementer that handed off before its sandbox was lost finished its work. It is
// read as Succeeded, never resumed into a second run of finished work.
func TestALostImplementerThatHandedOffIsNotResumed(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.r.Triage = staticWith("pair")
	g.reconcile(t, "3buqdlot", 3)
	g.f.SetBranch("agent/3buqdlot", 12)
	g.f.SetPR(pr12At(head1))
	g.log.handoff(rid(0), head1)
	g.lose(rid(0), runs.ReasonPodLost)
	tk := g.reconcile(t, "3buqdlot", 2)
	if tk.Status.Phase != v1alpha1.PhaseReviewing || tk.Status.Resumes != 0 || g.runs.specs[rid(1)].Role != "reviewer" {
		t.Fatalf("%s resumes=%d %+v", tk.Status.Phase, tk.Status.Resumes, tk.Status.Runs)
	}
}

// Review M3: when the room records no end, the record keeps the AgentRun's reason.
func TestALostRunWithoutARoomEndKeepsTheAgentRunsReason(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.reconcile(t, "3buqdlot", 3)
	r := g.runs.runs[rid(0)]
	r.Phase, r.Reason = "Failed", runs.ReasonDisrupted
	g.runs.runs[rid(0)] = r
	g.reconcile(t, "3buqdlot", 1) // waits for the room's end
	g.r.Now = func() time.Time { return now.Add(runEndGrace) }
	tk := g.reconcile(t, "3buqdlot", 2)
	if tk.Status.Runs[0].Reason != runs.ReasonDisrupted || tk.Status.Resumes != 1 {
		t.Fatalf("%+v resumes=%d", tk.Status.Runs[0], tk.Status.Resumes)
	}
}

// Only the AgentRun's Disrupted or PodLost on a Failed run, never a revoked one, is a loss to resume.
func TestInfraLost(t *testing.T) {
	for _, c := range []struct {
		run  runs.Run
		want bool
	}{
		{runs.Run{Phase: "Failed", Reason: runs.ReasonDisrupted}, true},
		{runs.Run{Phase: "Failed", Reason: runs.ReasonPodLost}, true},
		{runs.Run{Phase: "Failed", Reason: runs.ReasonPodFailed}, false},
		{runs.Run{Phase: "Failed", Reason: runs.ReasonDisrupted, Revoked: "manual"}, false},
		{runs.Run{Phase: "Succeeded", Reason: runs.ReasonPodLost}, false},
	} {
		if got := infraLost(c.run); got != c.want {
			t.Errorf("infraLost(%+v) = %t", c.run, got)
		}
	}
}

// Re-review (#25): a maintainer's review posted while a revision runs, which then loses its
// sandbox, reaches the resumed run. lateReviews queues it and marks it handled on the way back to
// Queued; the replayed brief quotes it after the lost run's own and the resumed run consumes it.
// The resume stays a resume, not a revision.
func TestAReviewPostedDuringALostRunReachesTheResumedRun(t *testing.T) {
	g := newRig(t, awaiting(), roomOf("3buqdlot"))
	first := changes(901, "Smana", "Use the relative link.", 5*time.Minute)
	g.f.SetPR(pr12(first))
	g.reconcile(t, "3buqdlot", 2) // → Queued (human) → the revision rid(1), which consumes 901
	g.f.SetPR(pr12(first, changes(902, "Smana", "Also fix the title.", -time.Minute)))
	g.lose(rid(1), runs.ReasonPodLost)
	tk := g.reconcile(t, "3buqdlot", 2)
	s := g.runs.specs[rid(2)]
	q, err := g.log.Queue(t.Context(), "3buqdlot")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status.Runs[2].Trigger != "resume" || !strings.HasPrefix(s.TaskText, ResumeNotice(tk)) ||
		!strings.Contains(s.TaskText, "Use the relative link.") || !strings.Contains(s.TaskText, "Also fix the title.") || len(q) != 0 {
		t.Fatalf("%+v queued=%d\n%s", tk.Status.Runs, len(q), s.TaskText)
	}
	if n := len(s.TaskText); n > reviseCap {
		t.Fatalf("the resumed brief is %d bytes, over reviseCap", n)
	}
}

// Past the task cap no reviewer is narrated into a review run the cap then refuses. Lost and
// enforced, the task escalates at once and spends no round; in shadow, it re-runs as before. One
// that ended on its own without a verdict is no resume: resume_budget never names it, and queued()
// escalates it after lateReviews (#30), with no "new review run" narrated.
func TestAReviewerWithoutAVerdictAtTheTaskCap(t *testing.T) {
	for name, c := range map[string]struct {
		enforce, lost, narrated bool
		used                    int64
		phase, reason           string
		rounds                  int32
		runs                    int
	}{
		"enforced":               {true, true, false, 1_500_001, v1alpha1.PhaseEscalated, "resume_budget", 0, 2},
		"shadow":                 {false, true, true, 1_500_001, v1alpha1.PhaseReviewing, "", 1, 3},
		"ended on its own":       {true, false, false, 1_500_001, v1alpha1.PhaseEscalated, "budget-task", 1, 2},
		"ended on its own, fits": {true, false, true, 1_500_000, v1alpha1.PhaseReviewing, "", 1, 3},
	} {
		t.Run(name, func(t *testing.T) {
			g := pairRig(t)
			g.r.Cfg.Budgets.EnforceTask = c.enforce
			r := g.runs.runs[rid(1)]
			r.Tokens = c.used // standard tier: 1.5 M of 3 M leaves exactly a run's worth
			g.runs.runs[rid(1)] = r
			if c.lost {
				g.lose(rid(1), runs.ReasonDisrupted)
			} else {
				g.finish(rid(1), "Succeeded", "agent_finished")
			}
			tk := g.reconcile(t, "3buqdlot", 2)
			if tk.Status.Phase != c.phase || tk.Status.Reason != c.reason || tk.Status.ReviewRounds != c.rounds || len(g.runs.specs) != c.runs {
				t.Fatalf("%s %q rounds=%d %d runs", tk.Status.Phase, tk.Status.Reason, tk.Status.ReviewRounds, len(g.runs.specs))
			}
			if issue := strings.Join(g.f.Comments(7), "\n"); strings.Contains(issue, "A new review run starts") != c.narrated {
				t.Fatalf("narrated a new review run: %t, want %t: %s", !c.narrated, c.narrated, issue)
			}
		})
	}
}

// R6 on the resume path: messages queued since the lost run are quoted only while the whole brief
// stays within reviseCap; with no room they stay queued, unquoted and unconsumed.
func TestWithQueuedKeepsTheBriefUnderTheCap(t *testing.T) {
	q := []rooms.Queued{{Ref: 902, Author: "Smana", Text: strings.Repeat("y", 1024)}}
	got, refs := withQueued(strings.Repeat("x", 1024), q, "n0nce234")
	if !slices.Equal(refs, []int64{902}) || len(got) > reviseCap || !strings.Contains(got, "QUEUED-DATA-n0nce234") {
		t.Fatalf("room left: refs=%v len=%d", refs, len(got))
	}
	full := strings.Repeat("x", reviseCap-200)
	if got, refs := withQueued(full, q, "n0nce234"); refs != nil || got != full {
		t.Fatalf("no room: refs=%v len=%d", refs, len(got))
	}
}
