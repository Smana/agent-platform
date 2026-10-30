// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/forge"
)

// G1 (v2 live auto-merge, D1): a rebuild deletes every Task, and with them the breaker's Task
// view. The labels on GitHub are what survives: an auto-merge of the class and a revert PR of it
// demote the class on a factory that has no Task of either.
func TestARebuiltFactoryReadsADemotedClassFromGitHub(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reverted bool
		want     string
	}{
		{"a labelled revert of a labelled merge demotes", true, "class_paused"},
		{"a labelled merge alone does not", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
			g.f.SetChecks(12, green("SUCCESS"))
			g.f.SetPR(forge.PR{Number: 9, State: "MERGED", MergedBy: "ogenki-agent-merger[bot]", MergedAt: now.Add(-48 * time.Hour),
				Labels: []string{AutoMergedLabel("docs-links")}})
			if tc.reverted {
				g.f.SetPR(forge.PR{Number: 30, State: "MERGED", MergedAt: now.Add(-47 * time.Hour), Labels: []string{RevertOfLabel(9)}})
			}
			tk := g.reconcile(t, "3buqdlot", 1)
			if tc.want == "" {
				if tk.Status.Phase != v1alpha1.PhaseAutoMerging {
					t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
				}
				return
			}
			if tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != tc.want || len(g.f.Armed()) != 0 {
				t.Fatalf("%s %s %v", tk.Status.Phase, tk.Status.Reason, g.f.Armed())
			}
		})
	}
}

// Human path AM6: a revert PR carrying factory/drill does not count as a revert of what it
// reverts, on GitHub's side (a rebuild) as on the Task's, when the merger or a maintainer added
// the label: verified as factory/revert is. Anyone else's, or an unverified one, counts.
func TestADrillRevertDoesNotTripTheBreaker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		actors []string // who added factory/drill to the revert PR, oldest first
		paused bool
	}{
		{"the merger's copy", []string{"ogenki-agent-merger[bot]"}, false},
		{"a maintainer's", []string{"Smana"}, false},
		{"a non-maintainer's", []string{"mallory"}, true},
		{"a non-maintainer's re-add after a maintainer's", []string{"Smana", "mallory"}, true},
		{"no event read", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
			g.f.SetChecks(12, green("SUCCESS"))
			g.f.SetPR(forge.PR{Number: 9, State: "MERGED", MergedBy: "ogenki-agent-merger[bot]", MergedAt: now.Add(-48 * time.Hour),
				Labels: []string{AutoMergedLabel("docs-links")}})
			g.f.SetPR(forge.PR{Number: 30, State: "MERGED", MergedAt: now.Add(-47 * time.Hour), Labels: []string{RevertOfLabel(9), DrillLabel}})
			var evs []forge.LabelEvent
			for i, a := range tc.actors {
				evs = append(evs, forge.LabelEvent{Actor: a, Label: DrillLabel, At: now.Add(time.Duration(i-47) * time.Hour)})
			}
			g.f.SetEvents(30, evs...)
			got := g.reconcile(t, "3buqdlot", 1)
			if paused := got.Status.Reason == "class_paused"; paused != tc.paused ||
				(!paused && got.Status.Phase != v1alpha1.PhaseAutoMerging) {
				t.Fatalf("%s %s", got.Status.Phase, got.Status.Reason)
			}
		})
	}
}

// D1: the daily cap counts today's labelled merges of every class, which a rebuild keeps.
func TestTheDailyCapCountsTodaysLabelledMerges(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.r.Cfg.Merge.AutoMergesPerDay = 1
	g.f.SetChecks(12, green("SUCCESS"))
	g.f.SetPR(forge.PR{Number: 5, State: "MERGED", MergedAt: now.Add(-time.Hour), Labels: []string{AutoMergedLabel("revert")}})
	g.f.SetPR(forge.PR{Number: 4, State: "MERGED", MergedAt: now.Add(-30 * time.Hour), Labels: []string{AutoMergedLabel("docs-links")}})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "auto_merge_cap" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// The ledger is the breaker's input: unread, nothing merges on a guess.
func TestAnUnreadLedgerDecidesNothing(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.f.SetChecks(12, green("SUCCESS"))
	g.f.FailLabelReads(errors.New("github down"))
	if _, err := g.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "agent-system", Name: "3buqdlot"}}); err == nil {
		t.Fatal("an unread ledger is an error the reconcile retries")
	}
	if len(g.f.Armed()) != 0 {
		t.Fatalf("%v", g.f.Armed())
	}
	g.f.FailLabelReads(nil)
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAutoMerging {
		t.Fatalf("a later read decides: %s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// D1: the merge the factory saw land is labelled with its class, through the merger App.
func TestAnAutoMergeIsLabelledWithItsClass(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.f.SetChecks(12, green("SUCCESS"))
	g.reconcile(t, "3buqdlot", 1)
	if slices.Contains(g.f.Added(12), AutoMergedLabel("docs-links")) {
		t.Fatal("labelled before GitHub reports the merge")
	}
	g.f.SetPR(forge.PR{Number: 12, NodeID: "PR_12", State: "MERGED", MergedBy: "ogenki-agent-merger[bot]", MergeCommitSHA: "m1"})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseVerifying {
		t.Fatal(tk.Status.Phase)
	}
	if a := g.f.Added(12); !slices.Equal(a, []string{"factory/auto-merged:docs-links"}) {
		t.Fatalf("%q", a)
	}
}

// D1: a human's merge is not an auto-merge, and is never labelled as one.
func TestAHumanMergeIsNotLabelled(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAutoMerging)
	g.f.SetPR(forge.PR{Number: 12, NodeID: "PR_12", State: "MERGED", MergedBy: "Smana", MergeCommitSHA: "m1"})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseDone {
		t.Fatal(tk.Status.Phase)
	}
	if a := g.f.Added(12); len(a) != 0 {
		t.Fatalf("%q", a)
	}
}

// D1: the revert PR names what it reverts in a label, once, whatever its state.
func TestARevertPRIsLabelledWithWhatItReverts(t *testing.T) {
	g := revertedRig(t)
	if a := g.f.Added(901); !slices.Equal(a, []string{"factory/revert-of:#12"}) {
		t.Fatalf("%q", a)
	}
	g.f.SetPR(forge.PR{Number: 901, NodeID: "PR_revert", State: "MERGED", Labels: []string{RevertOfLabel(12)}})
	g.reconcile(t, "3buqdlot", 1)
	if a := g.f.Added(901); len(a) != 1 {
		t.Fatalf("labelled once: %q", a)
	}
}

// revertedRig is the docs-links task auto-merged, main red, reverted by #901 (open), and that
// revert PR read once by the revert watch.
func revertedRig(t *testing.T) *rig {
	t.Helper()
	g := mergeRig(t, v1alpha1.PhaseVerifying)
	var tk v1alpha1.Task
	_ = g.c.Get(t.Context(), client.ObjectKey{Namespace: "agent-system", Name: "3buqdlot"}, &tk)
	merged := metav1.NewTime(now)
	tk.Status.PullRequest.MergeCommitSHA, tk.Status.PullRequest.AutoMerged, tk.Status.PullRequest.MergedAt = "m1", true, &merged
	_ = g.c.Status().Update(t.Context(), &tk)
	red := green("SUCCESS")
	red.Runs[0].State = "FAILURE"
	g.f.SetCommitChecks("m1", red.Runs...)
	got := g.reconcile(t, "3buqdlot", 1)
	g.f.SetPR(forge.PR{Number: got.Status.PullRequest.RevertNumber, NodeID: "PR_revert", State: "OPEN"})
	g.reconcile(t, "3buqdlot", 1)
	return g
}

// D2: the shadow record carries the head it judged, so a later human merge can be compared with
// it (D3), and a new head is a new decision.
func TestTheShadowRecordCarriesItsHead(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.r.Cfg.Classes = map[string]config.Class{"docs-links": {Shadow: true}, "revert": {Shadow: true}}
	g.f.SetChecks(12, green("SUCCESS"))
	g.reconcile(t, "3buqdlot", 1)
	if c := strings.Join(g.f.Comments(12), "\n"); !strings.Contains(c, "event=would-arm-1 head=abc paused=false capped=false -->") {
		t.Fatalf("%q", c)
	}
}

// D2: every decision the gate acts on is counted by class, verdict and reason; a wait is not one.
func TestTheGateCountsItsDecisions(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.r.Cfg.Classes = map[string]config.Class{"docs-links": {Shadow: true}, "revert": {Shadow: true}}
	pending := green("SUCCESS")
	pending.Runs[0].State = "PENDING"
	g.f.SetChecks(12, pending)
	g.reconcile(t, "3buqdlot", 1)
	if slices.ContainsFunc(g.metrics.recorded, func(s string) bool { return strings.HasPrefix(s, "merge_decision ") }) {
		t.Fatalf("a wait is no decision: %q", g.metrics.recorded)
	}
	g.f.SetChecks(12, green("SUCCESS"))
	g.reconcile(t, "3buqdlot", 1)
	if !slices.Contains(g.metrics.recorded, "merge_decision docs-links shadow shadow_would_arm") {
		t.Fatalf("%q", g.metrics.recorded)
	}
}

// D1's breaker over both views: a class's merges are deduplicated by pull request, a revert
// read from either side counts, and the window is the newest b.Window merges.
func TestDemotedUnitesTheTaskAndGitHubViews(t *testing.T) {
	b := config.Breaker{Window: 2, MaxReverts: 1}
	at := func(n, h int, reverted bool) Merge {
		return Merge{Number: n, At: now.Add(time.Duration(h) * time.Hour), Reverted: reverted}
	}
	tasks := []Merge{at(9, 0, false), at(10, 1, false)}
	gh := []Merge{at(9, 0, true)}
	if d, n := Demoted(uniteMerges(tasks, gh), b); !d || n != 1 {
		t.Fatalf("the revert GitHub knows of counts: %v %d", d, n)
	}
	if d, _ := Demoted(uniteMerges(append(tasks, at(11, 2, false)), gh), b); d {
		t.Fatal("two clean merges after it push it out of the window")
	}
}

// Human path AM7: with recoverAfter, a tripped class resumes once its recoverAfter newest merges
// are all newer than the newest bad one, clean and mature; immature ones count neither way, and a
// new bad merge trips it again.
func TestATrippedClassRecoversOnCleanMatureMerges(t *testing.T) {
	b := config.Breaker{Window: 10, MaxReverts: 1, RecoverAfter: 3}
	at := func(i int, reverted, mature bool) Merge {
		return Merge{Number: i + 1, At: now.Add(time.Duration(i) * time.Hour), Reverted: reverted, Mature: mature}
	}
	bad := []Merge{at(0, true, true)}
	if d, _ := Demoted(bad, b); !d {
		t.Fatal("one revert trips")
	}
	two := append(slices.Clone(bad), at(1, false, true), at(2, false, true))
	if d, _ := Demoted(two, b); !d {
		t.Fatal("two clean merges are not three")
	}
	immature := append(slices.Clone(two), at(3, false, false))
	if d, _ := Demoted(immature, b); !d {
		t.Fatal("an immature merge does not count")
	}
	threeImmature := append(slices.Clone(bad), at(1, false, false), at(2, false, false), at(3, false, false))
	if d, _ := Demoted(threeImmature, b); !d {
		t.Fatal("three clean immature merges do not recover")
	}
	three := append(slices.Clone(two), at(3, false, true))
	if d, _ := Demoted(three, b); d {
		t.Fatal("three clean mature merges after the bad one recover")
	}
	if d, _ := Demoted(append(slices.Clone(three), at(4, false, false)), b); d {
		t.Fatal("a newer immature merge is skipped, not blocking (R-I8)")
	}
	again := append(slices.Clone(three), at(4, true, true))
	if d, _ := Demoted(again, b); !d {
		t.Fatal("a new bad merge trips again")
	}
	if d, _ := Demoted(append(slices.Clone(three), at(4, true, false)), b); !d {
		t.Fatal("a new bad merge trips again, mature or not")
	}
	followUp := append(slices.Clone(two), Merge{Number: 4, At: now.Add(3 * time.Hour), FollowUp: true, Mature: true})
	if d, _ := Demoted(followUp, b); !d {
		t.Fatal("a follow-up is a bad merge")
	}
	if d, _ := Demoted(three, config.Breaker{Window: 10, MaxReverts: 1}); !d {
		t.Fatal("recoverAfter 0 keeps today's rule")
	}
}
