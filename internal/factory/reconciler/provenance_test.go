// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"slices"
	"strings"
	"testing"

	"github.com/Smana/agent-platform/internal/factory/meter"
)

// R10: the record takes what the run ran on from its claim as each fact appears, never changes a
// fact once written, and gathers the models the meter saw serve it.
func TestARunRecordsWhatItRanOn(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.reconcile(t, "3buqdlot", 3)
	image := "ghcr.io/smana/agent-harness@sha256:" + strings.Repeat("c", 64)
	r := g.runs.runs[rid(0)]
	r.Phase, r.FactoryVersion, r.CompositionRevision = "Running", "v0.16.0", "xagentruns.cloud.ogenki.io-41b6efe"
	g.runs.runs[rid(0)] = r
	costs := fakeCosts{rid(0): {MicroUSD: 10, Served: []string{"glm-5.3"}}}
	g.r.Costs = costs
	p := g.reconcile(t, "3buqdlot", 1).Status.Runs[0].Provenance
	if p.FactoryVersion != "v0.16.0" || p.CompositionRevision != "xagentruns.cloud.ogenki.io-41b6efe" || p.HarnessImage != "" ||
		!slices.Equal(p.ServedModels, []string{"glm-5.3"}) {
		t.Fatalf("before the pod: %+v", p)
	}
	// The pod started; then a newer CompositionRevision re-binds the claim, and a second model serves.
	r.HarnessImage = image
	g.runs.runs[rid(0)] = r
	g.reconcile(t, "3buqdlot", 1)
	r.CompositionRevision, r.HarnessImage = "xagentruns.cloud.ogenki.io-9f00d1e", "ghcr.io/smana/agent-harness@sha256:"+strings.Repeat("d", 64)
	g.runs.runs[rid(0)] = r
	costs[rid(0)] = meter.Cost{MicroUSD: 20, Served: []string{"claude-opus-5-5", "glm-5.3"}}
	p = g.reconcile(t, "3buqdlot", 1).Status.Runs[0].Provenance
	if p.CompositionRevision != "xagentruns.cloud.ogenki.io-41b6efe" || p.HarnessImage != image ||
		!slices.Equal(p.ServedModels, []string{"claude-opus-5-5", "glm-5.3"}) {
		t.Fatalf("after: %+v", p)
	}
}

// An unpriced typed run still carries the models that served it.
func TestAnUnpricedRunStillRecordsWhatServedIt(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.reconcile(t, "3buqdlot", 3)
	g.r.Costs = fakeCosts{rid(0): {Why: "no price for model", Served: []string{"mystery-1"}}}
	p := g.reconcile(t, "3buqdlot", 1).Status.Runs[0].Provenance
	if !slices.Equal(p.ServedModels, []string{"mystery-1"}) {
		t.Fatalf("unpriced: %+v", p)
	}
}
