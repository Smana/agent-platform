// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
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
		HeadSHA: "abc", HeadMessage: "docs: fix\n\nAgent-Run: " + rid(0)})
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
