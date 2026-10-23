// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"
	"time"

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
