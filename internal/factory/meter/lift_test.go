// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/internal/factory/runs"
)

// R10 across a failover: a run that ended before the switch is lifted by L1, its late reading is charged
// (settled.X = d), then a failover: L2 sees settled != 0, R10 does not lift, and charges the
// pre-switch cost again.
func TestALiftedRunsLateChargeCarriesTheLiftAcrossAFailover(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	c := fake.NewClientBuilder().WithScheme(cmScheme()).Build()
	run := glmRun("aaaaaaaa", now.Add(-time.Hour))
	run.Phase, run.Tokens = "Succeeded", 1_000_000
	src := typed{"aaaaaaaa": glmUsage(1_000_000)}
	l1 := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{run}}, src, c, &now)
	if err := l1.Tick(t.Context()); err != nil { // R10: lifted at 1.4 M
		t.Fatal(err)
	}
	src["aaaaaaaa"] = glmUsage(1_050_000) // late reading: +70 k
	if err := l1.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	run.Tokens = 1_050_000 // L1 annotated it
	later := now.Add(time.Hour)
	l2 := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{run}}, src, c, &later)
	if err := l2.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	spent, _ := spentOf(t, c, "20261022")
	if spent[runs.PrincipalFactory] != 70_000 {
		t.Errorf("today %v, want the late 70 k only", spent)
	}
}

// The same, where the lift reading is short because one served model's series is absent
// on L1's first tick (a partial read), not because of late spend.
func TestALiftedRunsPartialFirstReadingIsNotChargedTwice(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	c := fake.NewClientBuilder().WithScheme(cmScheme()).Build()
	run := glmRun("aaaaaaaa", now.Add(-time.Hour))
	run.Phase, run.Tokens = "Succeeded", 1_100_000
	src := typed{"aaaaaaaa": byModel(map[string]ModelUsage{"glm-5.3": {Input: 1_000_000}})}
	l1 := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{run}}, src, c, &now)
	if err := l1.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	src["aaaaaaaa"] = byModel(map[string]ModelUsage{"glm-5.3": {Input: 1_000_000}, "claude-opus-5-5": {Input: 100_000}})
	if err := l1.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	s1, _ := spentOf(t, c, "20261022")
	later := now.Add(time.Hour)
	l2 := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{run}}, src, c, &later)
	if err := l2.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	s2, _ := spentOf(t, c, "20261022")
	if s1[runs.PrincipalFactory] != 400_000 || s2[runs.PrincipalFactory] != s1[runs.PrincipalFactory] {
		t.Errorf("after L1 %v, after the failover %v: want 400 k both, the lifted 1.4 M never charged", s1, s2)
	}
}
