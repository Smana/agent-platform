// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/forge/forgetest"
	"github.com/Smana/agent-platform/internal/factory/killswitch"
	"github.com/Smana/agent-platform/internal/factory/taskid"
)

// twoRepositories: B has its own maintainer and label, and the control issue lives on A (v2 D1, D2).
func twoRepositories(p *IssuePoller) {
	p.Cfg.Repository, p.Cfg.Maintainers, p.Cfg.TriggerLabel = "", nil, ""
	p.Cfg.Repositories = []config.Repository{
		{Name: "Smana/a", Maintainers: []string{"Smana"}, TriggerLabel: "factory/ready"},
		{Name: "Smana/b", Maintainers: []string{"Other"}, TriggerLabel: "factory/go"},
	}
	p.Cfg.Control = config.Control{Repository: "Smana/a", Issue: 1}
}

// A poller serves its own entry: its label, its maintainers, its name on the task and the key.
func TestAPollerServesItsRepository(t *testing.T) {
	f := forgetest.NewFake()
	evs := []forge.LabelEvent{{Actor: "Other", Label: "factory/go", At: t0}}
	f.SetEvents(7, evs...)
	f.SetLabeled("factory/go", forge.Item{Number: 7})
	f.SetLabeled("factory/ready", forge.Item{Number: 8}) // A's label: not B's poller's to read
	f.SetIssue(forge.Issue{Number: 7, Title: "Fix", Body: "it"})
	p, c := poller(t, f)
	twoRepositories(p)
	p.Repository = "Smana/b"
	if err := p.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	ts := tasks(t, c)
	if len(ts) != 1 || ts[0].Spec.Repository != "Smana/b" || ts[0].Spec.Source.Ref != "Smana/b#7" ||
		ts[0].Name != taskid.Name(taskid.IssueKey("Smana/b", 7, 1)) {
		t.Fatalf("%+v", ts)
	}
	if got := f.Removed(7); len(got) == 0 || got[0] != "factory/go" {
		t.Fatalf("B's own label is consumed: %q", got)
	}
}

// A's maintainer is nobody on B (v2 D5's list half).
func TestAnotherRepositorysMaintainerIsRefused(t *testing.T) {
	f := forgetest.NewFake()
	f.SetEvents(7, forge.LabelEvent{Actor: "Smana", Label: "factory/go", At: t0})
	f.SetLabeled("factory/go", forge.Item{Number: 7})
	f.SetIssue(forge.Issue{Number: 7, Title: "Fix", Body: "it"})
	p, c := poller(t, f)
	twoRepositories(p)
	p.Repository = "Smana/b"
	_ = p.Poll(t.Context())
	if len(tasks(t, c)) != 0 {
		t.Fatal("no task from a labeller outside B's maintainers")
	}
}

// The control issue is read on its own repository only: issue #1 of B is just an issue.
func TestTheControlIssueIsOnItsRepository(t *testing.T) {
	for name, c := range map[string]struct {
		repo string
		want bool
	}{"on A": {"Smana/a", true}, "on B": {"Smana/b", false}} {
		t.Run(name, func(t *testing.T) {
			f := forgetest.NewFake()
			f.SetIssue(forge.Issue{Number: 1, Labels: []string{LabelStop}})
			p, _ := poller(t, f)
			twoRepositories(p)
			p.Repository = c.repo
			_ = p.Poll(context.Background())
			if on, _ := killswitch.Engaged(context.Background(), p.Client, "agent-system"); on != c.want {
				t.Fatalf("engaged %v, want %v", on, c.want)
			}
		})
	}
}

// Each repository's schedules start tasks on that repository (v2 D1).
func TestSchedulesAreStampedWithTheirRepository(t *testing.T) {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	now := time.Date(2026, 10, 5, 6, 20, 0, 0, time.UTC)
	cfg := &config.Config{Defaults: config.Defaults{DataClass: "public"}, Repositories: []config.Repository{
		{Name: "Smana/a", Schedules: []config.Schedule{{Name: "a-links", Cron: "0 6 * * 1", Class: "review", Text: "Fix A."}}},
		{Name: "Smana/b", Schedules: []config.Schedule{{Name: "b-links", Cron: "0 6 * * 1", Class: "review", Text: "Fix B."}}},
	}}
	sch := scheduler(forgetest.NewFake(), fake.NewClientBuilder().WithScheme(s), func(context.Context) bool { return false }, now, cfg)
	if err := sch.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	var l v1alpha1.TaskList
	_ = sch.Client.List(t.Context(), &l)
	got := map[string]string{}
	for _, tk := range l.Items {
		got[tk.Spec.Source.Ref] = tk.Spec.Repository
	}
	if len(got) != 2 || got["a-links"] != "Smana/a" || got["b-links"] != "Smana/b" {
		t.Fatalf("%v", got)
	}
}
