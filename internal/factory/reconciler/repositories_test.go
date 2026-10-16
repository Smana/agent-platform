// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/forge/forgetest"
)

const (
	repoA = "Smana/cloud-native-ref"
	repoB = "Smana/ai-native-ref"
)

// twoRepos lists A and B with the rig's settings, and gives each its own GitHub (v2 D8).
func twoRepos(g *rig) (a, b *forgetest.Fake) {
	c := g.r.Cfg
	one := config.Repository{Maintainers: c.Maintainers, Classes: c.Classes, Merge: c.Merge}
	ra, rb := one, one
	ra.Name, rb.Name = repoA, repoB
	c.Repositories = []config.Repository{ra, rb}
	c.Repository, c.Maintainers, c.Classes, c.Merge = "", nil, nil, config.Merge{}
	a, b = forgetest.NewFake(), forgetest.NewFake()
	a.Now, b.Now = g.f.Now, g.f.Now
	g.r.Forge, g.r.Merger = nil, nil
	g.r.Clients = func(repository string) (TaskForge, forge.Merger, bool) {
		switch repository {
		case repoA:
			return a, a, true
		case repoB:
			return b, b, true
		}
		return nil, nil, false
	}
	return a, b
}

func taskOn(name, repo string) *v1alpha1.Task {
	t := issueTask(name, 7, "x")
	t.Spec.Repository = repo
	return t
}

// A task of B is narrated on B, never on A's issue of the same number.
func TestATaskSpeaksOnItsRepositoryOnly(t *testing.T) {
	g := newRig(t, taskOn("3buqdlot", repoB))
	a, b := twoRepos(g)
	tk := g.reconcile(t, "3buqdlot", 3)
	tk.Annotations = map[string]string{v1alpha1.AnnotationStop: "true"}
	if err := g.c.Update(t.Context(), tk); err != nil {
		t.Fatal(err)
	}
	g.reconcile(t, "3buqdlot", 2)
	if len(b.Comments(7)) == 0 || len(a.Comments(7)) != 0 {
		t.Fatalf("B's issue %d comments, A's %d", len(b.Comments(7)), len(a.Comments(7)))
	}
}

// A task on a repository the config does not list gets no GitHub connection: it errors, and no
// client acts for it.
func TestATaskOnAnUnlistedRepositoryActsNowhere(t *testing.T) {
	g := newRig(t, taskOn("3buqdlot", "Smana/elsewhere"))
	a, b := twoRepos(g)
	if _, err := g.r.Reconcile(t.Context(), reqFor("3buqdlot")); err == nil {
		t.Fatal("reconciled without a connection")
	}
	if len(a.Comments(7))+len(b.Comments(7)) != 0 {
		t.Fatal("a client acted for an unlisted repository")
	}
}

// The daily auto-merge cap and the breaker count the task's repository only: B's merges spend
// none of A's day and revert none of A's classes.
func TestTheMergeCapAndBreakerArePerRepository(t *testing.T) {
	armed := func(name, repo string, n int, reverted bool) *v1alpha1.Task {
		tk := taskOn(name, repo)
		tk.Spec.PredictedClass = "docs-links"
		at := metav1.NewTime(now)
		tk.Status.PullRequest = &v1alpha1.PullRequestRef{Number: n, ArmedAt: &at, MergedAt: &at}
		if reverted {
			tk.Status.Phase = v1alpha1.PhaseReverted
		}
		return tk
	}
	g := newRig(t, armed("aaaaaaaa", repoA, 1, true), armed("bbbbbbbb", repoA, 2, false), armed("cccccccc", repoB, 3, false))
	twoRepos(g)
	ctx := context.Background()
	for repo, want := range map[string]int{repoA: 2, repoB: 1} {
		bctx, err := g.r.bind(ctx, taskOn("x", repo))
		if err != nil {
			t.Fatal(err)
		}
		n, err := g.r.armedToday(bctx, g.r.Cfg.RepositoryFor(repo))
		if err != nil || n != want {
			t.Errorf("%s armed today: %d %v, want %d", repo, n, err, want)
		}
		paused, err := g.r.paused(bctx, g.r.Cfg.RepositoryFor(repo), "docs-links")
		if err != nil || paused != (repo == repoA) {
			t.Errorf("%s paused %v %v: A's revert must not demote B's class", repo, paused, err)
		}
	}
}
