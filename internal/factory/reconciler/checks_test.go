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

// v3 feature 3 (RC8, offline proof 7): a switched-on class's public implementer, tester and reviewer claims
// carry spec.loop.checks and the repository's egress; every other claim is today's.
func TestASwitchedOnClassChecksItsPublicImplementerAndReviewer(t *testing.T) {
	g := newRig(t)
	repo := g.r.Cfg.Repos()[0]
	repo.Classes = map[string]config.Class{"docs-links": {Shadow: true}, "revert": {}, "docs": {Checks: true}, "tests": {}}
	repo.Checks = config.Checks{Egress: []string{"golang", "pypi"}}
	g.r.Cfg.Repositories = []config.Repository{repo}
	task := func(class, dataClass string) *v1alpha1.Task {
		tk := issueTask("3buqdlot", 7, "fix")
		tk.Spec.PredictedClass, tk.Spec.DataClass = class, dataClass
		tk.Status.PullRequest = &v1alpha1.PullRequestRef{Number: 12, URL: "https://github.com/Smana/cloud-native-ref/pull/12"}
		return tk
	}
	on := []runs.Spec{g.r.implementerSpec(task("docs", "public"), "brief"), g.r.verifierSpec(task("docs", "public"), "reviewer"),
		g.r.testerSpec(task("docs", "public"), "brief")}
	for _, s := range on {
		if !s.Checks || !reflect.DeepEqual(s.EgressProfiles, []string{"golang", "pypi"}) {
			t.Fatalf("%s: checks %v egress %v, want on with [golang pypi]", s.Role, s.Checks, s.EgressProfiles)
		}
	}
	off := []runs.Spec{g.r.testerSpec(task("docs", "internal"), "brief"), g.r.implementerSpec(task("docs", "internal"), "brief"),
		g.r.implementerSpec(task("tests", "public"), "brief"), g.r.implementerSpec(task("review", "public"), "brief")}
	for _, s := range off {
		u := runs.Build(s)
		for _, key := range []string{"loop", "egress"} {
			if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", key); found {
				t.Fatalf("%s %s: spec.%s written on a run without checks", s.Role, s.DataClass, key)
			}
		}
	}
}

// Checks ride beside the task's loop policy: one spec.loop holds both, and a checks-only claim
// (no loop policy, or a reviewer) writes only checks.
func TestChecksJoinTheLoopPolicyInOneSpecLoop(t *testing.T) {
	s := runs.Spec{Checks: true, Loop: &runs.Loop{StallSteps: 80, Continuations: 2}}
	loop, _, _ := unstructured.NestedMap(runs.Build(s).Object, "spec", "loop")
	if loop["checks"] != true || loop["stallSteps"] != int64(80) || loop["continuations"] != int64(2) {
		t.Fatalf("loop %v", loop)
	}
	loop, _, _ = unstructured.NestedMap(runs.Build(runs.Spec{Checks: true}).Object, "spec", "loop")
	if !reflect.DeepEqual(loop, map[string]any{"checks": true}) {
		t.Fatalf("checks-only loop %v", loop)
	}
}
