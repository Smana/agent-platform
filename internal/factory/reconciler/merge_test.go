// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

// mergeRig is a docs-links task at its merge gate: PR 12 open at abc, its room having reported
// that head (R02: the decision can only speak about a commit a run of the task named).
func mergeRig(t *testing.T, phase string, objs ...client.Object) *rig {
	tk := awaiting()
	tk.Spec.PredictedClass, tk.Status.Phase = "docs-links", phase
	tk.Status.PullRequest.NodeID, tk.Status.PullRequest.HeadSHA = "PR_12", "abc"
	g := newRig(t, append([]client.Object{tk, roomOf("3buqdlot")}, objs...)...)
	g.r.Merger = g.f // one fake records both Apps' calls
	g.r.Cfg.AgentsLogin, g.r.Cfg.FactoryLogin = "ogenki-agents[bot]", "ogenki-agent-factory[bot]"
	// The live path, proven offline now and live after the wave (Task 10.7); the shadow test flips it.
	g.r.Cfg.Classes = map[string]config.Class{"docs-links": {Live: true}, "revert": {Live: true}}
	g.r.Cfg.Merge = config.Merge{RequiredChecks: required, VerifyChecks: required, PolicyBotLogin: "ogenki-merge-gate[bot]",
		MergerLogin: "ogenki-agent-merger[bot]", AutoMergesPerDay: 10, FixRuns: 2,
		VerifyFor: config.Duration{Duration: 30 * time.Minute}, RevertWindow: config.Duration{Duration: 168 * time.Hour},
		Breaker: config.Breaker{Window: 10, MaxReverts: 1}}
	g.f.SetPR(forge.PR{Number: 12, NodeID: "PR_12", State: "OPEN", Title: "docs: fix a link", Author: "ogenki-agents[bot]",
		HeadRef: "agent/3buqdlot", HeadSHA: "abc", HeadMessage: "docs: fix\n\nAgent-Run: " + rid(0)})
	g.log.handoff(rid(0), "abc")
	return g
}

// handoff is the room event an implementer run ends with, naming the commit it pushed: R02's
// reported head, as the broker stamps it.
func (l *fakeLog) handoff(run, commit string) {
	l.evs = append(l.evs, envelope.Event{Seq: int64(len(l.evs) + 1), RunID: run, Type: envelope.Handoff, Origin: envelope.OriginClient,
		Actor:   envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:" + run, Role: "implementer"},
		Payload: envelope.Must(envelope.HandoffPayload{FromRole: "implementer", ToRole: "factory", Commit: commit})})
}

// C2: a watched check that never ran on main (a path-filtered push workflow) is not pending forever.
func TestVerifyingIgnoresChecksThatNeverRanOnMain(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseVerifying)
	var tk v1alpha1.Task
	_ = g.c.Get(t.Context(), client.ObjectKey{Namespace: "agent-system", Name: "3buqdlot"}, &tk)
	merged := metav1.NewTime(now)
	tk.Status.PullRequest.MergeCommitSHA, tk.Status.PullRequest.AutoMerged, tk.Status.PullRequest.MergedAt = "m1", true, &merged
	_ = g.c.Status().Update(t.Context(), &tk)
	g.f.SetCommitChecks("m1", forge.Check{Name: required[0], State: "SUCCESS"}) // required[1] never runs on push
	g.r.Now = func() time.Time { return now.Add(31 * time.Minute) }
	if got := g.reconcile(t, "3buqdlot", 1); got.Status.Phase != v1alpha1.PhaseDone || got.Status.Reason != "merged_verified" {
		t.Fatalf("%s %s", got.Status.Phase, got.Status.Reason)
	}
	if MainState([]forge.Check{{Name: required[0], State: "PENDING"}}, required) != "PENDING" ||
		MainState([]forge.Check{{Name: required[1], State: "FAILURE"}}, required) != "FAILURE" {
		t.Fatal("a check that ran is still watched until it ends")
	}
}

// SC-14 after the decision (R52): GitHub checks the head at merge time, so a push that lands
// between the decision and the merge refuses it — nothing merges on the old decision, and the
// new head is decided again, reported head and trailer check included. There is no disarm:
// the amended path never armed anything on GitHub (external review R02, ruling R52).
func TestAHeadMovedAfterArmingIsDecidedAgain(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.f.SetChecks(12, green("SUCCESS"))
	g.f.RefuseMerge("PR_12", forge.ErrHeadMoved) // the push landed after the pull request was read
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseAwaitingCI || tk.Status.Reason != "head_moved" ||
		len(g.f.Armed()) != 0 || len(g.f.Disarmed()) != 0 || tk.Status.PullRequest.ArmedAt != nil {
		t.Fatalf("%s %s %v %v", tk.Status.Phase, tk.Status.Reason, g.f.Armed(), g.f.Disarmed())
	}
	g.f.SetPR(forge.PR{Number: 12, NodeID: "PR_12", State: "OPEN", Title: "docs: sneak", Author: "ogenki-agents[bot]",
		HeadSHA: "def", HeadMessage: "docs: sneak\n\nAgent-Run: zzzzzzzz"})
	g.log.handoff(rid(0), "def") // the room reports the new head: it is decided on its own merits
	if tk = g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "foreign_trailer" {
		t.Fatalf("the moved head is decided again: %s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// R52's other refusal: a merge GitHub will not allow as it stands goes to a human, and nothing
// is left armed behind it.
func TestAMergeThatIsNotMergeableGoesToAHuman(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.f.SetChecks(12, green("SUCCESS"))
	g.f.RefuseMerge("PR_12", forge.ErrNotMergeable)
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "not_mergeable" || len(g.f.Armed()) != 0 {
		t.Fatalf("%s %s %v", tk.Status.Phase, tk.Status.Reason, g.f.Armed())
	}
}

// R41 (review G5): a revert's effect on its class is narrated where the kill switch lives.
func TestARevertIsNarratedOnTheControlIssue(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseVerifying)
	g.r.Cfg.ControlIssue = 1
	var tk v1alpha1.Task
	_ = g.c.Get(t.Context(), client.ObjectKey{Namespace: "agent-system", Name: "3buqdlot"}, &tk)
	merged := metav1.NewTime(now)
	tk.Status.PullRequest.MergeCommitSHA, tk.Status.PullRequest.AutoMerged, tk.Status.PullRequest.MergedAt = "m1", true, &merged
	_ = g.c.Status().Update(t.Context(), &tk)
	red := green("SUCCESS")
	red.Runs[0].State = "FAILURE"
	g.f.SetCommitChecks("m1", red.Runs...)
	g.reconcile(t, "3buqdlot", 1)
	if c := g.f.Comments(1); len(c) != 1 || !strings.Contains(c[0], "`docs-links` goes to human review while 1 or more of its last 10 merges are reverts") {
		t.Fatalf("%q", c)
	}
}

// §6.4: a revert that never goes green is disarmed and handed to a maintainer, never left armed.
func TestAStalledRevertIsDisarmed(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseVerifying)
	var tk v1alpha1.Task
	_ = g.c.Get(t.Context(), client.ObjectKey{Namespace: "agent-system", Name: "3buqdlot"}, &tk)
	merged := metav1.NewTime(now)
	tk.Status.PullRequest.MergeCommitSHA, tk.Status.PullRequest.AutoMerged, tk.Status.PullRequest.MergedAt = "m1", true, &merged
	_ = g.c.Status().Update(t.Context(), &tk)
	red := green("SUCCESS")
	red.Runs[0].State = "FAILURE"
	g.f.SetCommitChecks("m1", red.Runs...)
	got := g.reconcile(t, "3buqdlot", 1) // Reverted; the fake's revert PR is #901
	g.f.SetPR(forge.PR{Number: got.Status.PullRequest.RevertNumber, NodeID: "PR_revert", State: "OPEN"})
	if got = g.reconcile(t, "3buqdlot", 1); got.Status.Phase != v1alpha1.PhaseReverted || len(g.f.Disarmed()) != 0 {
		t.Fatal("inside verifyFor the revert keeps its chance")
	}
	g.r.Now = func() time.Time { return now.Add(31 * time.Minute) }
	g.reconcile(t, "3buqdlot", 2)
	if d := g.f.Disarmed(); len(d) != 1 || d[0] != "PR_revert" ||
		!strings.Contains(strings.Join(g.f.Comments(12), "\n"), "a maintainer merges or closes it") {
		t.Fatalf("%v %q", d, g.f.Comments(12))
	}
}

// R32 (owner, 2026-09-27): before the wave the gate narrates what it would do and does nothing.
func TestShadowNarratesAndNeverArms(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.r.Cfg.Classes = map[string]config.Class{"docs-links": {Shadow: true}, "revert": {Shadow: true}}
	g.f.SetChecks(12, green("SUCCESS"))
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "shadow_would_arm" ||
		len(g.f.Armed()) != 0 || tk.Status.PullRequest.ArmedAt != nil {
		t.Fatalf("%s %s %v", tk.Status.Phase, tk.Status.Reason, g.f.Armed())
	}
	if c := strings.Join(g.f.Comments(12), "\n"); !strings.Contains(c, "would auto-merge: `docs-links`, checks green, verdict approve") {
		t.Fatalf("%q", c)
	}
	if n, _ := g.r.armedToday(t.Context()); n != 0 {
		t.Fatal("a shadow decision never counts against the daily cap")
	}
}

func TestMergedByLoginCaseIsIgnored(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.f.SetChecks(12, green("SUCCESS"))
	g.reconcile(t, "3buqdlot", 1)
	g.f.SetPR(forge.PR{Number: 12, NodeID: "PR_12", State: "MERGED", MergedBy: "Ogenki-agent-merger[bot]", MergeCommitSHA: "m1"})
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseVerifying || !tk.Status.PullRequest.AutoMerged {
		t.Fatalf("%s autoMerged=%v", tk.Status.Phase, tk.Status.PullRequest.AutoMerged)
	}
}

func TestArmMergeVerifyDone(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.f.SetChecks(12, green("SUCCESS"))
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseAutoMerging || len(g.f.Armed()) != 1 || tk.Status.PullRequest.ArmedAt == nil {
		t.Fatalf("%s %v", tk.Status.Phase, g.f.Armed())
	}
	if a := g.f.Armed(); a[0] != "PR_12 abc" {
		t.Fatalf("the merge is of the decided head: %q", a)
	}
	g.f.SetPR(forge.PR{Number: 12, NodeID: "PR_12", State: "MERGED", MergedBy: "ogenki-agent-merger[bot]", MergeCommitSHA: "m1"})
	tk = g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseVerifying || !tk.Status.PullRequest.AutoMerged {
		t.Fatal(tk.Status.Phase)
	}
	g.f.SetCommitChecks("m1", green("SUCCESS").Runs...)
	if tk = g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseVerifying {
		t.Fatal("main is watched for 30 minutes (§6.4)")
	}
	g.r.Now = func() time.Time { return now.Add(31 * time.Minute) }
	if tk = g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseDone || tk.Status.Reason != "merged_verified" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

func TestMainRedRevertsAndPausesTheClass(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseVerifying)
	var tk v1alpha1.Task
	_ = g.c.Get(t.Context(), client.ObjectKey{Namespace: "agent-system", Name: "3buqdlot"}, &tk)
	merged := metav1.NewTime(now)
	tk.Status.PullRequest.MergeCommitSHA, tk.Status.PullRequest.AutoMerged, tk.Status.PullRequest.MergedAt = "m1", true, &merged
	tk.Status.ConfigHash = g.r.Cfg.Hash
	_ = g.c.Status().Update(t.Context(), &tk)
	red := green("SUCCESS")
	red.Runs[0].State = "FAILURE"
	g.f.SetCommitChecks("m1", red.Runs...)
	got := g.reconcile(t, "3buqdlot", 1)
	if got.Status.Phase != v1alpha1.PhaseReverted || got.Status.Reason != "main_red" || len(g.f.Reverts()) != 1 ||
		!strings.HasPrefix(strings.SplitN(g.f.Reverts()[0], " ", 2)[1], `Revert "`) || len(g.f.Armed()) != 1 {
		t.Fatalf("%s %s %v %v", got.Status.Phase, got.Status.Reason, g.f.Reverts(), g.f.Armed())
	}
	paused, err := g.r.paused(t.Context(), "docs-links")
	if err != nil || !paused {
		t.Fatal("one revert pauses the class until the config changes (§6.4)")
	}
	g.r.Cfg.Hash = strings.Repeat("c", 64)
	if paused, _ := g.r.paused(t.Context(), "docs-links"); !paused {
		t.Fatal("R41: a config change no longer lifts a demotion while the revert is among the class's last 10 merges")
	}
}

// #41: a run that starts because CI went red says which check failed on which head.
func TestACIFixRunSaysWhyItStarted(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	red := green("SUCCESS")
	red.Runs[1].State = "FAILURE"
	g.f.SetChecks(12, red)
	g.reconcile(t, "3buqdlot", 2)
	c := strings.Join(g.f.Comments(7), "\n") // the issue, where the task's other narration lands
	for _, want := range []string{"CI is red on #12 at `abc`", "`Kubernetes validation`: failure", "a run starts to fix it (1/2)"} {
		if !strings.Contains(c, want) {
			t.Fatalf("missing %q in %q", want, c)
		}
	}
}

func TestCIRedGetsTwoFixRunsThenEscalates(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	red := green("SUCCESS")
	red.Runs[1].State = "FAILURE"
	g.f.SetChecks(12, red)
	for i := range 2 {
		id := rid(i + 1) // the deterministic ids of the fix runs (R48)
		tk := g.reconcile(t, "3buqdlot", 2)
		if tk.Status.Phase != v1alpha1.PhaseImplementing || tk.Status.FixRuns != int32(i+1) ||
			tk.Status.Runs[len(tk.Status.Runs)-1].Trigger != "ci" ||
			!strings.Contains(g.runs.specs[id].TaskText, "Kubernetes validation") {
			t.Fatalf("fix %d: %s %d", i, tk.Status.Phase, tk.Status.FixRuns)
		}
		g.runs.set(id, "Succeeded")
		g.log.end(id, "Succeeded", "agent_finished")
		g.reconcile(t, "3buqdlot", 1) // back to AwaitingCI
	}
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "ci_red" ||
		!strings.Contains(strings.Join(g.f.Comments(12), "\n"), "Kubernetes validation") {
		t.Fatalf("%s %s %q", tk.Status.Phase, tk.Status.Reason, g.f.Comments(12))
	}
}

// A cancelled required check is not a failure (#40): it is re-run, and no fix run is spent.
func TestCancelledCheckIsRerunNotFixed(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.r.Cfg.Merge.CheckReruns = 1
	c := green("SUCCESS")
	c.Runs[1].State, c.Runs[1].ID = "CANCELLED", 777
	g.f.SetChecks(12, c)
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseAwaitingCI || tk.Status.FixRuns != 0 || tk.Status.CheckReruns != 1 ||
		tk.Status.Reason != "ci_rerun" || !slices.Equal(g.f.Reruns(), []int64{777}) {
		t.Fatalf("%s fix=%d reruns=%d %q %v", tk.Status.Phase, tk.Status.FixRuns, tk.Status.CheckReruns, tk.Status.Reason, g.f.Reruns())
	}
}

// The cap bounds the re-runs: a check that keeps being cancelled goes to a human, not a loop.
func TestCancelledCheckRerunCapHandsOff(t *testing.T) {
	for name, cap := range map[string]int32{"cap reached": 1, "re-run off": 0} {
		t.Run(name, func(t *testing.T) {
			g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
			g.r.Cfg.Merge.CheckReruns = cap
			c := green("SUCCESS")
			c.Runs[1].State, c.Runs[1].ID = "CANCELLED", 777
			g.f.SetChecks(12, c)
			var tk *v1alpha1.Task
			for range 4 { // however often the check is cancelled again
				tk = g.reconcile(t, "3buqdlot", 1)
			}
			if tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "ci_cancelled" ||
				tk.Status.FixRuns != 0 || len(g.f.Reruns()) != int(cap) {
				t.Fatalf("%s %q fix=%d reruns=%v", tk.Status.Phase, tk.Status.Reason, tk.Status.FixRuns, g.f.Reruns())
			}
		})
	}
}

// A failure beside a cancellation is still a failure: the fix run starts, nothing is re-run.
func TestFailureBesideCancelledStillFixed(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.r.Cfg.Merge.CheckReruns = 1
	c := green("SUCCESS")
	c.Runs[0].State, c.Runs[1].State = "CANCELLED", "FAILURE"
	g.f.SetChecks(12, c)
	tk := g.reconcile(t, "3buqdlot", 2)
	if tk.Status.Phase != v1alpha1.PhaseImplementing || tk.Status.FixRuns != 1 || len(g.f.Reruns()) != 0 {
		t.Fatalf("%s fix=%d %v", tk.Status.Phase, tk.Status.FixRuns, g.f.Reruns())
	}
}

func TestAMaintainersRevertAfterDone(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseDone)
	var tk v1alpha1.Task
	_ = g.c.Get(t.Context(), client.ObjectKey{Namespace: "agent-system", Name: "3buqdlot"}, &tk)
	merged := metav1.NewTime(now.Add(-48 * time.Hour))
	tk.Status.PullRequest.AutoMerged, tk.Status.PullRequest.MergedAt = true, &merged
	_ = g.c.Status().Update(t.Context(), &tk)
	tk.Annotations = map[string]string{v1alpha1.AnnotationRevert: "label"}
	_ = g.c.Update(t.Context(), &tk)
	got := g.reconcile(t, "3buqdlot", 1)
	if got.Status.Phase != v1alpha1.PhaseReverted || got.Status.Reason != "revert_requested" {
		t.Fatalf("%s %s", got.Status.Phase, got.Status.Reason)
	}
}

// R41 (review G5): 1 revert among the last 10 merges demotes; 10 clean merges after it lift it.
func TestRevertsAmongTheLastMergesDemoteTheClass(t *testing.T) {
	b := config.Breaker{Window: 10, MaxReverts: 1}
	at := func(i int, phase string) *v1alpha1.Task {
		m := metav1.NewTime(now.Add(time.Duration(i) * time.Hour))
		return &v1alpha1.Task{Status: v1alpha1.TaskStatus{Phase: phase, ConfigHash: strings.Repeat("h", i+1),
			PullRequest: &v1alpha1.PullRequestRef{MergedAt: &m}}}
	}
	merged := []*v1alpha1.Task{at(0, v1alpha1.PhaseReverted)}
	for i := 1; i <= 9; i++ {
		merged = append(merged, at(i, v1alpha1.PhaseDone))
	}
	if d, n := Demoted(merged, b); !d || n != 1 {
		t.Fatalf("1 revert in the last 10 merges demotes, whatever the config: %v %d", d, n)
	}
	if d, _ := Demoted(append(merged, at(10, v1alpha1.PhaseDone)), b); d {
		t.Fatal("the revert left the window: 10 clean merges lift the demotion")
	}
}

// The bypass race at the CI gate: a maintainer's merge seen by awaitingCI records when it merged,
// so the task carries a timestamp the breaker window can count.
func TestAHumanMergeCountsInTheBreakerWindow(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.f.SetPR(forge.PR{Number: 12, NodeID: "PR_12", State: "MERGED", MergedBy: "Smana", MergeCommitSHA: "m1"})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseDone || tk.Status.PullRequest.MergedAt == nil {
		t.Fatalf("%s %v", tk.Status.Phase, tk.Status.PullRequest.MergedAt)
	}
}

// R41's lift, through the flow a demotion actually routes merges along: a demoted class' PRs sit
// in AwaitingHuman, and the maintainer's merges of them refill the breaker window until the
// revert leaves it. The rig's window is 2 for the test to be watchable: revert + 1 clean still
// demotes, revert + 2 clean lifts.
func TestAHumanMergeOfADemotedClassRefillsTheWindow(t *testing.T) {
	old := metav1.NewTime(now.Add(-3 * time.Hour))
	reverted := awaiting()
	reverted.Name, reverted.Spec.PredictedClass, reverted.Status.Phase = "1reverted", "docs-links", v1alpha1.PhaseReverted
	reverted.Status.PullRequest.Number, reverted.Status.PullRequest.AutoMerged = 9, true
	reverted.Status.PullRequest.MergedAt, reverted.Status.Runs = &old, nil
	next := awaiting()
	next.Name, next.Spec.PredictedClass = "2cleanmerge", "docs-links"
	next.Status.PullRequest.Number, next.Status.PullRequest.NodeID, next.Status.Runs = 13, "PR_13", nil
	g := mergeRig(t, v1alpha1.PhaseAwaitingHuman, reverted, next)
	g.r.Cfg.Merge.Breaker = config.Breaker{Window: 2, MaxReverts: 1}
	g.f.SetPR(forge.PR{Number: 12, NodeID: "PR_12", State: "MERGED", MergedBy: "Smana", MergeCommitSHA: "m1"})
	g.f.SetPR(forge.PR{Number: 13, NodeID: "PR_13", State: "MERGED", MergedBy: "octocat", MergeCommitSHA: "m2"})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseDone || tk.Status.PullRequest.MergedAt == nil {
		t.Fatalf("prEnded must timestamp the merge too: %s %v", tk.Status.Phase, tk.Status.PullRequest.MergedAt)
	}
	if paused, _ := g.r.paused(t.Context(), "docs-links"); !paused {
		t.Fatal("one clean merge beside the revert does not lift the demotion")
	}
	if tk := g.reconcile(t, "2cleanmerge", 1); tk.Status.Phase != v1alpha1.PhaseDone || tk.Status.PullRequest.MergedAt == nil {
		t.Fatalf("the second human merge: %s %v", tk.Status.Phase, tk.Status.PullRequest.MergedAt)
	}
	if paused, _ := g.r.paused(t.Context(), "docs-links"); paused {
		t.Fatal("R41: clean merges of the demoted class must refill the window until the revert leaves it")
	}
}

// R42 (review G6): a red secret scan is a live credential in a public diff. No fix run: a human.
func TestARedSecretScanEscalatesWithoutAFixRun(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.r.Cfg.Merge.LeakScanCheck = "Security scanning"
	red := green("SUCCESS")
	red.Runs = append(red.Runs, forge.Check{Name: "Security scanning", State: "FAILURE"})
	g.f.SetChecks(12, red)
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "secret_scan_red" || tk.Status.FixRuns != 0 {
		t.Fatalf("%s %s %d", tk.Status.Phase, tk.Status.Reason, tk.Status.FixRuns)
	}
	if c := strings.Join(g.f.Comments(12), "\n"); !strings.Contains(c, "live credential") {
		t.Fatalf("%q", c)
	}
}

// gatePR is mergeRig's pull request at head h, its commit claiming run.
func gatePR(h, run string) forge.PR {
	return forge.PR{Number: 12, NodeID: "PR_12", State: "OPEN", Title: "docs: fix a link", Author: "ogenki-agents[bot]",
		HeadRef: "agent/3buqdlot", HeadSHA: h, HeadMessage: "docs: fix\n\nAgent-Run: " + run}
}

// patch edits the rig's task, spec and status.
func (g *rig) patch(t *testing.T, f func(*v1alpha1.Task)) {
	t.Helper()
	var tk v1alpha1.Task
	if err := g.c.Get(t.Context(), client.ObjectKey{Namespace: "agent-system", Name: "3buqdlot"}, &tk); err != nil {
		t.Fatal(err)
	}
	f(&tk)
	st := tk.Status
	if err := g.c.Update(t.Context(), &tk); err != nil {
		t.Fatal(err)
	}
	tk.Status = st
	if err := g.c.Status().Update(t.Context(), &tk); err != nil {
		t.Fatal(err)
	}
}

// N1: a hand-off for a policy-bot status not yet passing is transient. Once policy-bot passes the
// same head the task returns to the gate, which decides it on its own merits, and says why.
func TestAPolicyHandOffReturnsToTheGate(t *testing.T) {
	absent := green("SUCCESS")
	absent.Statuses = nil
	for reason, checks := range map[string]forge.Checks{"policy_pending": green("PENDING"), "policy_absent": absent} {
		t.Run(reason, func(t *testing.T) {
			g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
			g.f.SetChecks(12, checks)
			if tk := g.reconcile(t, "3buqdlot", 2); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != reason {
				t.Fatalf("while the policy has not passed a human holds it: %s %s", tk.Status.Phase, tk.Status.Reason)
			}
			g.f.SetChecks(12, green("SUCCESS"))
			tk := g.reconcile(t, "3buqdlot", 1)
			if tk.Status.Phase != v1alpha1.PhaseAwaitingCI || tk.Status.Reason != "reentered_policy" || tk.Status.GateReentries != 1 {
				t.Fatalf("%s %s %d", tk.Status.Phase, tk.Status.Reason, tk.Status.GateReentries)
			}
			if c := strings.Join(g.f.Comments(7), "\n"); !strings.Contains(c, "goes back to the merge gate: policy-bot now passes") {
				t.Fatalf("the return is narrated: %q", c)
			}
			if tk = g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAutoMerging || len(g.f.Armed()) != 1 || g.f.Armed()[0] != "PR_12 abc" {
				t.Fatalf("%s %v", tk.Status.Phase, g.f.Armed())
			}
		})
	}
}

// N1's bound: a policy-bot that flaps between pending and success re-enters the gate at most
// maxGateReentries times, then a human keeps the pull request.
func TestAFlappingPolicyCannotLoop(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	entries := 0
	for range 10 {
		g.f.SetChecks(12, green("PENDING"))
		g.reconcile(t, "3buqdlot", 1)
		g.f.SetChecks(12, green("SUCCESS"))
		if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase == v1alpha1.PhaseAwaitingCI {
			entries++
		}
	}
	tk := g.reconcile(t, "3buqdlot", 3)
	if entries != maxGateReentries || tk.Status.GateReentries != maxGateReentries || tk.Status.Phase != v1alpha1.PhaseAwaitingHuman ||
		tk.Status.Reason != "policy_pending" || len(g.f.Armed()) != 0 {
		t.Fatalf("%d entries, %d recorded, %s %s, %v", entries, tk.Status.GateReentries, tk.Status.Phase, tk.Status.Reason, g.f.Armed())
	}
}

// N1: the room reporting the head the hand-off was for proves the factory pushed it: the task
// returns to the gate. A run's push lands before its handoff event does.
func TestAnUnreportedHeadReturnsOnceTheRoomReportsIt(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.f.SetPR(gatePR("def", rid(0)))
	g.f.SetChecks(12, green("SUCCESS"))
	if tk := g.reconcile(t, "3buqdlot", 2); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "head_unreported" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
	g.log.handoff(rid(0), "def")
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseAwaitingCI || tk.Status.Reason != "reentered_head_reported" || tk.Status.GateReentries != 1 {
		t.Fatalf("%s %s %d", tk.Status.Phase, tk.Status.Reason, tk.Status.GateReentries)
	}
	if tk = g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAutoMerging || g.f.Armed()[0] != "PR_12 def" {
		t.Fatalf("%s %v", tk.Status.Phase, g.f.Armed())
	}
}

// N1: verdict_stale is a head the factory moved (R02 holds before it) with no verdict of it. A
// human has nothing to judge there: the gate queues a review of that head, which leads back to
// it, and no hand-off is narrated. The return counts against the cap; past it, a human.
func TestAStaleVerdictIsReviewedAgain(t *testing.T) {
	stale := func(t *testing.T, reentries int32) *rig {
		g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
		g.patch(t, func(tk *v1alpha1.Task) {
			tk.Spec.Template = "pair"
			tk.Status.Verdict, tk.Status.GateReentries = "approve", reentries
			tk.Status.Runs = append(tk.Status.Runs,
				v1alpha1.RunRecord{ID: rid(1), Role: "reviewer", Trigger: "review", Phase: "Succeeded", Verdict: "approve", HeadSHA: "old"},
				v1alpha1.RunRecord{ID: rid(2), Role: "implementer", Trigger: "ci", Phase: "Succeeded"})
		})
		g.log.handoff(rid(2), "abc") // a fix run, before this change, went from its push straight to the gate
		g.f.SetPR(gatePR("abc", rid(2)))
		g.f.SetChecks(12, green("SUCCESS"))
		return g
	}
	g := stale(t, 0)
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseQueued || tk.Status.NextRole != "reviewer" || tk.Status.Reason != "rereview_verdict_stale" ||
		tk.Status.GateReentries != 1 {
		t.Fatalf("%s %s %s %d", tk.Status.Phase, tk.Status.NextRole, tk.Status.Reason, tk.Status.GateReentries)
	}
	if c := strings.Join(g.f.Comments(7), "\n"); strings.Contains(c, "waits for a maintainer") || !strings.Contains(c, "reviewed again") {
		t.Fatalf("%q", c)
	}
	if tk = g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseReviewing || current(tk).Role != "reviewer" || current(tk).HeadSHA != "abc" {
		t.Fatalf("%s %+v", tk.Status.Phase, tk.Status.Runs)
	}
	g = stale(t, maxGateReentries)
	if tk = g.reconcile(t, "3buqdlot", 2); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "verdict_stale" {
		t.Fatalf("past the cap a human keeps it: %s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// N1 review: a return the factory cannot read (the room log, the checks) is no return, and never
// blocks the reminder and the stale close of a pull request a human holds.
func TestAReturnThatCannotBeReadStillReminds(t *testing.T) {
	tk := staleTask(15 * 24 * time.Hour)
	tk.Status.Reason, tk.Status.PullRequest.HeadSHA = "head_unreported", "abc"
	g := newRig(t, tk)
	p := pr12()
	p.HeadSHA = "abc"
	g.f.SetPR(p)
	g.log.failEvents = errors.New("broker unavailable")
	g.remindedAndAged(t)
	if got := g.reconcile(t, "3buqdlot", 1); got.Status.Phase != v1alpha1.PhaseClosed || got.Status.Reason != "stale" {
		t.Fatalf("%s %s", got.Status.Phase, got.Status.Reason)
	}
}

// N1's constraint: only a transient hand-off of a head nobody else touched returns to the gate.
// A human's push, a maintainer's review or comment, or a substantive reason keeps the human's.
func TestASubstantiveHandOffStaysWithTheHuman(t *testing.T) {
	later := now.Add(time.Minute)
	for name, tc := range map[string]struct {
		handOff func(g *rig)
		after   func(g *rig)
	}{
		"human push": {
			after: func(g *rig) { g.f.SetPR(gatePR("def", rid(0))) }, // the trailer is a claim anyone can write
		},
		"maintainer review": {
			after: func(g *rig) {
				p := gatePR("abc", rid(0))
				p.Reviews = []forge.Review{{ID: 7, Author: "Smana", State: "COMMENTED", Body: "looking", At: later}}
				g.f.SetPR(p)
			},
		},
		"maintainer comment": {
			after: func(g *rig) {
				p := gatePR("abc", rid(0))
				p.Comments = []forge.Comment{{ID: 8, Author: "Smana", Body: "hold on", At: later}}
				g.f.SetPR(p)
			},
		},
		"substantive reason": {
			handOff: func(g *rig) { g.r.Cfg.Merge.AutoMergesPerDay = 0 }, // auto_merge_cap
			after:   func(g *rig) { g.r.Cfg.Merge.AutoMergesPerDay = 10 },
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
			g.f.SetPR(gatePR("abc", rid(0)))
			g.f.SetChecks(12, green("PENDING"))
			if tc.handOff != nil {
				g.f.SetChecks(12, green("SUCCESS"))
				tc.handOff(g)
			}
			if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman {
				t.Fatal(tk.Status.Phase)
			}
			tc.after(g)
			g.f.SetChecks(12, green("SUCCESS"))
			if tk := g.reconcile(t, "3buqdlot", 2); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.GateReentries != 0 ||
				len(g.f.Armed()) != 0 {
				t.Fatalf("%s %s %d %v", tk.Status.Phase, tk.Status.Reason, tk.Status.GateReentries, g.f.Armed())
			}
		})
	}
}
