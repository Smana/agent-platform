// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/runs"
)

// searchRig is a rig whose review class (the rig's default) has search switched on at triage.
func searchRig(t *testing.T, row config.Loop) (*rig, *v1alpha1.Task) {
	t.Helper()
	g := newRig(t, issueTask("3buqdlot", 7, "# Fix the link"))
	g.r.Cfg.Loops = map[string]config.Loop{"review": row}
	tk := g.reconcile(t, "3buqdlot", 3)
	if !tk.Spec.Search {
		t.Fatal("triage did not record spec.search")
	}
	return g, tk
}

// v3 feature 5 (ST9, offline proof 5): triage writes spec.search; the implementer's claim carries
// spec.loop.search alone when the class's loop is off.
func TestASearchRowReachesTheImplementersClaimAlone(t *testing.T) {
	g, _ := searchRig(t, config.Loop{Search: true})
	s := g.runs.specs[rid(0)]
	if !s.Search || s.Loop != nil {
		t.Fatalf("claim search %v loop %+v, want search alone", s.Search, s.Loop)
	}
	loop, _, _ := unstructured.NestedMap(runs.Build(s).Object, "spec", "loop")
	if !reflect.DeepEqual(loop, map[string]any{"search": true}) {
		t.Fatalf("spec.loop %v, want {search: true}", loop)
	}
}

// With the loop on too, one spec.loop holds both; every other role's claim has no search.
func TestSearchJoinsTheLoopOnImplementerClaimsOnly(t *testing.T) {
	steps := int32(40)
	g, tk := searchRig(t, config.Loop{Search: true, StallSteps: &steps})
	loop, _, _ := unstructured.NestedMap(runs.Build(g.runs.specs[rid(0)]).Object, "spec", "loop")
	if loop["search"] != true || loop["stallSteps"] != int64(40) {
		t.Fatalf("spec.loop %v", loop)
	}
	for _, role := range []string{"reviewer", "tester", "triager"} {
		s := g.r.implementerSpec(tk, "text")
		s.Role = role
		if err := g.r.startRun(t.Context(), tk, s, "verifier"); err != nil {
			t.Fatal(err)
		}
		if got := g.runs.specs[runID(tk.Name, len(tk.Status.Runs)-1)]; got.Search {
			t.Fatalf("%s claim carries search", role)
		}
	}
}

// No row: the task has no spec.search and its claim no spec.loop (D4).
func TestNoSearchRowIsTodaysClaim(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "# Fix the link"))
	tk := g.reconcile(t, "3buqdlot", 3)
	if tk.Spec.Search || g.runs.specs[rid(0)].Search {
		t.Fatal("search on with no row")
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(runs.Build(g.runs.specs[rid(0)]).Object, "spec", "loop"); found {
		t.Fatal("spec.loop written with no row")
	}
}
