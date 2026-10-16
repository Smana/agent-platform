// SPDX-License-Identifier: Apache-2.0

package triage

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/forge/forgetest"
)

func TestMatrix(t *testing.T) { // §2
	for _, c := range []struct{ class, source, tier, want string }{
		{"docs-links", "issue", "light", "solo"}, {"docs-links", "issue", "standard", "solo"}, {"docs-links", "issue", "frontier", "pair"},
		{"docs", "issue", "light", "pair"}, {"review", "issue", "standard", "pair"}, {"tests", "schedule", "frontier", "trio"},
		{"review", "runlore", "light", "investigate"}, {"review", "runlore", "frontier", "investigate"},
	} {
		if got := Matrix(c.class, c.source, c.tier); got != c.want {
			t.Errorf("%s/%s/%s = %s, want %s", c.class, c.source, c.tier, got, c.want)
		}
	}
}

func TestControlIsDeterministicAndAboutTenPercent(t *testing.T) {
	n := 0
	for i := 0; i < 2000; i++ {
		id := string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + "234567"
		// SA4000 bars the literal Control(id,10) != Control(id,10); the two bindings are the
		// check: one task id must always land the same way.
		if a, b := Control(id, 10), Control(id, 10); a != b {
			t.Fatal("deterministic")
		}
		if Control(id, 10) {
			n++
		}
	}
	if n < 120 || n > 280 {
		t.Fatalf("%d of 2000 in the control group", n)
	}
	if Control("3buqdlot", 0) {
		t.Fatal("0 % means none")
	}
}

type fakeC7 struct {
	resp C7Response
	err  error
	got  C7Request
}

func (f *fakeC7) Classify(_ context.Context, r C7Request) (C7Response, error) {
	f.got = r
	return f.resp, f.err
}

func testCfg() *config.Config {
	return &config.Config{Maintainers: []string{"Smana"},
		Tiers: map[string]config.Tier{
			"light":    {Model: "agent-default", RunTokens: 300_000, TaskTokens: 600_000, RunMinutes: 20},
			"standard": {Model: "agent-default", RunTokens: 1_500_000, TaskTokens: 3_000_000, RunMinutes: 45},
			"frontier": {Model: "agent-default", RunTokens: 4_000_000, TaskTokens: 8_000_000, RunMinutes: 90}},
		Triage:  config.Triage{ControlPercent: 0},
		Classes: map[string]config.Class{"docs-links": {Live: true}, "revert": {Live: true}, "docs": {}, "tests": {}, "dashboards": {}}}
}

func issueTask() *v1alpha1.Task {
	return &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "3buqdlot"},
		Spec: v1alpha1.TaskSpec{Source: v1alpha1.Source{Kind: "issue"}, Issue: 7, Text: "fix a link", DataClass: "public"}}
}

func TestClassifyUsesAMaintainersClassLabel(t *testing.T) {
	f := forgetest.NewFake()
	f.SetIssue(forge.Issue{Number: 7, Labels: []string{"class:docs-links"}})
	f.SetEvents(7, forge.LabelEvent{Actor: "Smana", Label: "class:docs-links", At: time.Now()})
	c7 := &fakeC7{resp: C7Response{Tier: "light", Confidence: 0.91, Classifier: "semantic-router", Fallback: "none",
		Shadow: []C7Shadow{{Classifier: "jev", Tier: "standard", Confidence: 0.6}}}}
	d, err := Classify{Cfg: testCfg(), C: c7, Forge: f}.Triage(context.Background(), issueTask())
	if err != nil {
		t.Fatal(err)
	}
	if d.PredictedClass != "docs-links" || d.Template != "solo" || d.Budget.Tier != "light" || d.Budget.RunTokens != 300_000 ||
		d.Classification.Classifier != "semantic-router" || d.Classification.Confidence != "0.91" || len(d.Classification.Shadow) != 1 {
		t.Fatalf("%+v", d)
	}
	if c7.got.Ref != "3buqdlot" || c7.got.DataClass != "public" || c7.got.Text != "fix a link" {
		t.Fatalf("C7 request %+v", c7.got)
	}
}

func TestClassifyFallsBackAndIgnoresStrangersLabels(t *testing.T) {
	f := forgetest.NewFake()
	f.SetIssue(forge.Issue{Number: 7, Labels: []string{"class:docs-links"}})
	f.SetEvents(7, forge.LabelEvent{Actor: "someone", Label: "class:docs-links", At: time.Now()})
	d, err := Classify{Cfg: testCfg(), C: &fakeC7{err: errors.New("timeout")}, Forge: f}.Triage(context.Background(), issueTask())
	if err != nil {
		t.Fatal(err)
	}
	if d.PredictedClass != "review" || d.Template != "pair" || d.Budget.Tier != "standard" ||
		d.Classification.Fallback != "static" || d.Classification.Classifier != "static" {
		t.Fatalf("the classifier never blocks (C7) and labels are not trust anchors (§8): %+v", d)
	}
}

func TestControlGroupRunsAtFrontier(t *testing.T) {
	cfg := testCfg()
	cfg.Triage.ControlPercent = 100
	d, _ := Classify{Cfg: cfg, C: &fakeC7{resp: C7Response{Tier: "light", Classifier: "semantic-router", Fallback: "none"}},
		Forge: forgetest.NewFake()}.Triage(context.Background(), &v1alpha1.Task{Spec: v1alpha1.TaskSpec{Source: v1alpha1.Source{Kind: "schedule"}, DataClass: "public"}})
	if !d.Classification.Control || d.Classification.Tier != "light" || d.Budget.Tier != "frontier" {
		t.Fatalf("the classifier's answer is recorded as-is; the budget acts at frontier (OD-14): %+v", d)
	}
}

// A class label is read against the task's repository: its classes and its maintainers (v2 D1).
func TestClassifyReadsTheTasksRepository(t *testing.T) {
	cfg := testCfg()
	a := config.Repository{Name: "Smana/a", Maintainers: []string{"Smana"}, Classes: cfg.Classes}
	b := config.Repository{Name: "Smana/b", Maintainers: []string{"Other"}, Classes: map[string]config.Class{"revert": {}}}
	cfg.Maintainers, cfg.Classes, cfg.Repositories = nil, nil, []config.Repository{a, b}
	for name, c := range map[string]struct{ repo, actor, want string }{
		"A's class by A's maintainer": {"Smana/a", "Smana", "docs-links"},
		"A's maintainer on B":         {"Smana/b", "Smana", "review"},
		"a class B does not declare":  {"Smana/b", "Other", "review"},
	} {
		t.Run(name, func(t *testing.T) {
			f := forgetest.NewFake()
			f.SetIssue(forge.Issue{Number: 7, Labels: []string{"class:docs-links"}})
			f.SetEvents(7, forge.LabelEvent{Actor: c.actor, Label: "class:docs-links", At: time.Now()})
			tk := issueTask()
			tk.Spec.Repository = c.repo
			d, err := Classify{Cfg: cfg, C: &fakeC7{resp: C7Response{Tier: "light", Confidence: 0.9}}, Forge: f}.Triage(t.Context(), tk)
			if err != nil {
				t.Fatal(err)
			}
			if d.PredictedClass != c.want {
				t.Fatalf("class %q, want %q", d.PredictedClass, c.want)
			}
		})
	}
}

// v2 D8: the class label is read on the task's repository: B's issue #7 is not A's.
func TestClassifyReadsTheIssueOnItsRepository(t *testing.T) {
	cfg := testCfg()
	a := config.Repository{Name: "Smana/a", Maintainers: []string{"Smana"}, Classes: cfg.Classes}
	b := config.Repository{Name: "Smana/b", Maintainers: []string{"Smana"}, Classes: cfg.Classes}
	cfg.Maintainers, cfg.Classes, cfg.Repositories = nil, nil, []config.Repository{a, b}
	onA, onB := forgetest.NewFake(), forgetest.NewFake()
	onA.SetIssue(forge.Issue{Number: 7, Labels: []string{"class:docs-links"}})
	onA.SetEvents(7, forge.LabelEvent{Actor: "Smana", Label: "class:docs-links", At: time.Now()})
	onB.SetIssue(forge.Issue{Number: 7})
	clients := func(repo string) (ClassForge, bool) {
		switch repo {
		case "Smana/a":
			return onA, true
		case "Smana/b":
			return onB, true
		}
		return nil, false
	}
	tk := issueTask()
	tk.Spec.Repository = "Smana/b"
	d, err := Classify{Cfg: cfg, C: &fakeC7{resp: C7Response{Tier: "light", Confidence: 0.9}}, Clients: clients}.Triage(t.Context(), tk)
	if err != nil || d.PredictedClass != "review" {
		t.Fatalf("B's issue carries no class: %q %v", d.PredictedClass, err)
	}
	tk.Spec.Repository = "Smana/elsewhere"
	if d, err := (Classify{Cfg: cfg, C: &fakeC7{resp: C7Response{Tier: "light", Confidence: 0.9}}, Clients: clients}).Triage(t.Context(), tk); err != nil ||
		d.PredictedClass != "review" {
		t.Fatalf("an unknown repository reads no label: %q %v", d.PredictedClass, err)
	}
}
