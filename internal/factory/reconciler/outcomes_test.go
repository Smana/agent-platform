// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

func tenMerges() []Merge {
	ms := make([]Merge, 10)
	for i := range ms {
		ms[i] = Merge{Number: 100 + i, At: now.Add(-time.Duration(i) * time.Hour)}
	}
	return ms
}

// MC8, M5: one follow-up or one regression in 10 demotes with maxFollowUps 1; with 0, as before,
// neither does; an eleventh clean merge pushes it out of the window.
func TestFollowUpsAndRegressionsDemote(t *testing.T) {
	on, off := config.Breaker{Window: 10, MaxReverts: 1, MaxFollowUps: 1}, config.Breaker{Window: 10, MaxReverts: 1}
	for _, bad := range []func(*Merge){func(m *Merge) { m.FollowUp = true }, func(m *Merge) { m.Regression = true }} {
		ms := tenMerges()
		bad(&ms[9])
		if d, _ := Demoted(ms, on); !d {
			t.Fatal("one bad outcome in 10 demotes")
		}
		if d, _ := Demoted(ms, off); d {
			t.Fatal("maxFollowUps 0 counts none")
		}
		if d, _ := Demoted(append(ms, Merge{Number: 99, At: now.Add(time.Hour)}), on); d {
			t.Fatal("an eleventh clean merge pushes it out")
		}
	}
}

// Offline proof 5, v2 G1's shape: with every Task deleted, the labels alone keep the class
// demoted: a human merge of the class (factory/merged) and its follow-up's label.
func TestADemotionOnFollowUpSurvivesEveryTaskDeleted(t *testing.T) {
	g := declaredRig(t)
	g.r.Cfg.Merge.Breaker = config.Breaker{Window: 10, MaxReverts: 1, MaxFollowUps: 1}
	g.f.SetPR(forge.PR{Number: 40, State: "MERGED", MergedBy: "Smana", MergedAt: now.Add(-48 * time.Hour),
		Labels: []string{MergedLabel("dead-links")}})
	g.f.SetPR(forge.PR{Number: 41, State: "MERGED", MergedBy: "Smana", MergedAt: now.Add(-24 * time.Hour),
		Labels: []string{FollowUpOfLabel(40)}})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Reason != "class_paused" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
	g = declaredRig(t)
	g.r.Cfg.Merge.Breaker = config.Breaker{Window: 10, MaxReverts: 1, MaxFollowUps: 1}
	g.f.SetPR(forge.PR{Number: 40, State: "MERGED", MergedBy: "renovate[bot]", MergedAt: now.Add(-48 * time.Hour),
		Labels: []string{MergedLabel("dead-links"), RegressionLabel}})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Reason != "class_paused" {
		t.Fatalf("a regression label demotes: %s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

func followUpBreaker(g *rig) {
	g.r.Cfg.Merge.Breaker = config.Breaker{Window: 10, MaxReverts: 1, MaxFollowUps: 1}
}

// A fix in review is not yet a fix: an OPEN follow-up-of pull request demotes nothing.
func TestAnOpenFollowUpDoesNotDemote(t *testing.T) {
	g := declaredRig(t)
	followUpBreaker(g)
	g.f.SetPR(forge.PR{Number: 40, State: "MERGED", MergedBy: "Smana", MergedAt: now.Add(-48 * time.Hour),
		Labels: []string{MergedLabel("dead-links")}})
	g.f.SetPR(forge.PR{Number: 41, State: "OPEN", Labels: []string{FollowUpOfLabel(40)}})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAutoMerging {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// A merge carrying both labels is one ledger entry: a duplicate would take two window slots and
// push an older follow-up out.
func TestAMergeWithBothLabelsIsOneLedgerEntry(t *testing.T) {
	g := declaredRig(t)
	followUpBreaker(g)
	for i := range 9 {
		g.f.SetPR(forge.PR{Number: 50 + i, State: "MERGED", MergedAt: now.Add(-time.Duration(i+1) * time.Hour),
			Labels: []string{MergedLabel("dead-links"), AutoMergedLabel("dead-links")}})
	}
	g.f.SetPR(forge.PR{Number: 40, State: "MERGED", MergedAt: now.Add(-72 * time.Hour), Labels: []string{MergedLabel("dead-links")}})
	g.f.SetPR(forge.PR{Number: 41, State: "MERGED", MergedAt: now.Add(-71 * time.Hour), Labels: []string{FollowUpOfLabel(40)}})
	var tk v1alpha1.Task
	if err := g.c.Get(t.Context(), keyFor("3buqdlot"), &tk); err != nil {
		t.Fatal(err)
	}
	byClass, err := g.r.githubLedger(t.Context(), g.r.repo(&tk))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(byClass["dead-links"]); n != 10 {
		t.Fatalf("ten merges, one entry each: %d", n)
	}
	if d, _ := Demoted(byClass["dead-links"], g.r.repo(&tk).Merge.Breaker); !d {
		t.Fatal("the tenth merge's follow-up is inside the window")
	}
}

// An unread outcome is an unread ledger: each new read that fails leaves the task unarmed.
func TestAFailedOutcomeReadArmsNothing(t *testing.T) {
	for name, label := range map[string]string{"merged": MergedLabel("dead-links"), "regression": RegressionLabel,
		"follow-up": FollowUpOfLabel(40)} {
		t.Run(name, func(t *testing.T) {
			g := declaredRig(t)
			followUpBreaker(g)
			g.f.SetPR(forge.PR{Number: 40, State: "MERGED", MergedAt: now.Add(-48 * time.Hour), Labels: []string{MergedLabel("dead-links")}})
			g.f.FailLabelReadsFor(label, errors.New("github down"))
			if _, err := g.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "agent-system", Name: "3buqdlot"}}); err == nil {
				t.Fatal("an unread outcome is an error the reconcile retries")
			}
			if len(g.f.Armed()) != 0 {
				t.Fatalf("%v", g.f.Armed())
			}
		})
	}
}

// A full page of labelled pull requests whose oldest is still inside the window may hide newer
// merges: the ledger is incomplete, so nothing arms (fail closed) and a maintainer decides.
func TestAFullPageInsideTheWindowIsTruncated(t *testing.T) {
	// All merged days ago (never today); the higher the number, the more recent the merge, except
	// the oldest created, whose merge time is the argument.
	fill := func(g *rig, oldestMergedAgo time.Duration) {
		for i := range ledgerPage {
			ago := 100*time.Hour + time.Duration(ledgerPage-i)*3*time.Hour
			if i == 0 {
				ago = oldestMergedAgo
			}
			g.f.SetPR(forge.PR{Number: 200 + i, State: "MERGED", MergedAt: now.Add(-ago), Labels: []string{MergedLabel("dead-links")}})
		}
	}
	g := declaredRig(t)
	fill(g, 90*time.Hour) // the oldest created merged most recently: older pages could hold more
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "ledger_truncated" ||
		len(g.f.Armed()) != 0 {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
	g = declaredRig(t)
	fill(g, 500*time.Hour) // the oldest merged longest ago: the page holds the window
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAutoMerging {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// The regression read is repo-wide: a full page whose oldest was created after a windowed merge
// may have dropped that merge's label.
func TestAFullRegressionPageThatMissesTheWindowIsTruncated(t *testing.T) {
	g := declaredRig(t)
	followUpBreaker(g)
	g.f.SetPR(forge.PR{Number: 40, State: "MERGED", MergedAt: now.Add(-48 * time.Hour), Labels: []string{MergedLabel("dead-links")}})
	for i := range ledgerPage {
		g.f.SetPR(forge.PR{Number: 300 + i, State: "MERGED", MergedAt: now.Add(-72 * time.Hour), Labels: []string{RegressionLabel}})
	}
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Reason != "ledger_truncated" || len(g.f.Armed()) != 0 {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
}
