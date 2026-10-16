// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// v2 D5: a labeller of B must be in B's maintainers *and* hold write on B on GitHub. A maintainer
// GitHub no longer lists as a writer is refused once, the label consumed; a lookup that fails
// leaves the label for the next poll, and nothing starts.
func TestLabelIntakeNeedsWriteOnTheRepository(t *testing.T) {
	for name, tc := range map[string]struct {
		write    bool
		err      error
		task     bool
		consumed bool
	}{
		"a maintainer with write":     {true, nil, true, true},
		"a maintainer with read only": {false, nil, false, true},
		"GitHub does not answer":      {false, errors.New("github down"), false, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := forgetest.NewFake()
			f.SetEvents(7, forge.LabelEvent{Actor: "Other", Label: "factory/go", At: t0})
			f.SetLabeled("factory/go", forge.Item{Number: 7})
			f.SetIssue(forge.Issue{Number: 7, Title: "Fix", Body: "it"})
			p, c := poller(t, f)
			twoRepositories(p)
			p.Repository = "Smana/b"
			var asked []string
			p.CanWrite = func(_ context.Context, login string) (bool, error) {
				asked = append(asked, login)
				return tc.write, tc.err
			}
			_ = p.Poll(t.Context())
			if got := len(tasks(t, c)) == 1; got != tc.task {
				t.Fatalf("task %v, want %v", got, tc.task)
			}
			if got := len(f.Removed(7)) > 0; got != tc.consumed {
				t.Fatalf("label consumed %v, want %v", got, tc.consumed)
			}
			if len(asked) != 1 || asked[0] != "Other" {
				t.Fatalf("asked GitHub about %q", asked)
			}
		})
	}
}

// Without the GitHub check wired, no label starts work (fail closed).
func TestNoWriteCheckStartsNothing(t *testing.T) {
	f := forgetest.NewFake()
	labelled(f, 7, "Smana")
	p, c := poller(t, f)
	p.CanWrite = nil
	_ = p.Poll(t.Context())
	if len(tasks(t, c)) != 0 {
		t.Fatal("a task without the write check")
	}
}

// Issue #7 of A and issue #7 of B are two issues: A's live task never refuses B's label.
func TestTheSameIssueNumberOnTwoRepositories(t *testing.T) {
	f := forgetest.NewFake()
	f.SetEvents(7, forge.LabelEvent{Actor: "Other", Label: "factory/go", At: t0})
	f.SetLabeled("factory/go", forge.Item{Number: 7})
	f.SetIssue(forge.Issue{Number: 7, Title: "Fix", Body: "it"})
	onA := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "aaaaaaaa", Namespace: "agent-system",
		Labels: map[string]string{v1alpha1.LabelIssue: "7"}}, Spec: v1alpha1.TaskSpec{Repository: "Smana/a", Issue: 7}}
	p, c := poller(t, f, onA)
	twoRepositories(p)
	p.Repository = "Smana/b"
	if err := p.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ts := tasks(t, c); len(ts) != 2 {
		t.Fatalf("B's task is refused by A's: %d tasks", len(ts))
	}
}

// v2 D6: RunLore's task names the repository the config gives it, not the first by accident.
func TestRunLoreFilesOnItsConfiguredRepository(t *testing.T) {
	h, c, _ := runlore(t)
	h.Cfg.Repository = ""
	h.Cfg.Repositories = []config.Repository{{Name: "Smana/a"}, {Name: "Smana/b"}}
	h.Cfg.RunLore.Repository = "Smana/b"
	if w := send(h, "s3cret", finding); w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var l v1alpha1.TaskList
	_ = c.List(t.Context(), &l)
	if len(l.Items) != 1 || l.Items[0].Spec.Repository != "Smana/b" {
		t.Fatalf("%+v", l.Items)
	}
}

// v2 D8: a schedule's probe reads the open pull requests of its own repository.
func TestAProbeReadsItsRepository(t *testing.T) {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	now := time.Date(2026, 10, 5, 6, 20, 0, 0, time.UTC)
	probe := config.Schedule{Name: "b-red", Cron: "0 6 * * 1", Class: "review", Text: "Fix.", Probe: "renovate-red", ProbeAuthor: "renovate[bot]"}
	cfg := &config.Config{Defaults: config.Defaults{DataClass: "public"}, Repositories: []config.Repository{
		{Name: "Smana/a", Classes: map[string]config.Class{"docs-links": {Shadow: true}}},
		{Name: "Smana/b", Classes: map[string]config.Class{"docs-links": {Shadow: true}}, Schedules: []config.Schedule{probe}},
	}}
	onA, onB := forgetest.NewFake(), forgetest.NewFake()
	red := forge.PRSummary{Number: 9, Author: "renovate[bot]", Created: now.Add(-48 * time.Hour)}
	onA.SetOpenPRs(red) // A's red PR is not B's
	onA.SetChecks(9, forge.Checks{Runs: []forge.Check{{Name: "ci", State: "FAILURE"}}})
	sch := scheduler(nil, fake.NewClientBuilder().WithScheme(s), func(context.Context) bool { return false }, now, cfg)
	sch.Forge, sch.Merger = nil, nil
	sch.Clients = func(repo string) (ScheduleForge, ScheduleMerger, bool) {
		switch repo {
		case "Smana/a":
			return onA, onA, true
		case "Smana/b":
			return onB, onB, true
		}
		return nil, nil, false
	}
	if err := sch.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	var l v1alpha1.TaskList
	_ = sch.Client.List(t.Context(), &l)
	if len(l.Items) != 0 {
		t.Fatalf("B's probe found A's red pull request: %+v", l.Items)
	}
}
