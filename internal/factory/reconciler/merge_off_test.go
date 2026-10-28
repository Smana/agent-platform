// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/forge"
)

// #57: with every class off the factory may run without the merger key. A pull request at the CI
// gate goes to a maintainer at once, instead of erroring on the absent merger at every reconcile.
func TestMergeGateOffHandsTheTaskToAHuman(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.r.Cfg.Classes = map[string]config.Class{"docs-links": {}, "revert": {}}
	g.r.Merger = forge.Absent{}
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "merge_gate_off" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
}
