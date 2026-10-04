// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/taskid"
)

func TestLastSlot(t *testing.T) {
	now := time.Date(2026, 10, 5, 6, 20, 0, 0, time.UTC) // a Monday
	slot, ok := LastSlot("0 6 * * 1", now)
	if !ok || !slot.Equal(time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC)) {
		t.Fatal(slot)
	}
	if _, ok := LastSlot("nonsense", now); ok {
		t.Fatal("an unparsable expression names no slot")
	}
	// Feb 29 only comes around every four years: a lookback of eight days names nothing between.
	if _, ok := LastSlot("0 0 29 2 *", time.Date(2026, 10, 5, 6, 20, 0, 0, time.UTC)); ok {
		t.Fatal("an expression that never fired in the lookback names no slot")
	}
}

func schedulerCfg() *config.Config {
	return &config.Config{Repository: "Smana/cloud-native-ref", Defaults: config.Defaults{DataClass: "public"},
		Schedules: []config.Schedule{
			{Name: "link-rot", Cron: "0 6 * * 1", Class: "docs-links", Text: "Fix broken external links."},
			{Name: "renovate-red", Cron: "0 6 * * 1", Class: "review", Text: "Fix red Renovate PRs.", Probe: "renovate-red"},
		}}
}

func scheduler(f *forge.Fake, c *fake.ClientBuilder, stopped func(context.Context) bool,
	now time.Time, cfg *config.Config) *Scheduler {
	return &Scheduler{Forge: f, Merger: f, Client: c.Build(), Namespace: "agent-system", Now: func() time.Time { return now },
		Stopped: stopped, Errors: &counter{}, Cfg: cfg}
}

func TestSchedulerCreatesOneTaskPerSlot(t *testing.T) {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s)
	now := time.Date(2026, 10, 5, 6, 20, 0, 0, time.UTC)
	f := forge.NewFake()
	sch := scheduler(f, c, func(context.Context) bool { return false }, now, schedulerCfg())
	for i := 0; i < 2; i++ {
		if err := sch.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var l v1alpha1.TaskList
	_ = sch.Client.List(context.Background(), &l)
	if len(l.Items) != 1 {
		t.Fatalf("one task: the probe found no red Renovate PR, and a slot fires once; got %d", len(l.Items))
	}
	tk := l.Items[0]
	if tk.Name != taskid.Name(taskid.ScheduleKey("link-rot", time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC))) ||
		tk.Spec.Source.Kind != "schedule" || tk.Spec.Source.Trust != "trusted" || tk.Spec.PredictedClass != "docs-links" || tk.Spec.Issue != 0 {
		t.Fatalf("%+v", tk.Spec)
	}
	f.SetOpenPRs(forge.PRSummary{Number: 40, Author: "renovate[bot]", Created: now.Add(-48 * time.Hour)})
	f.SetChecks(40, forge.Checks{Runs: []forge.Check{{Name: "Kubernetes validation ☸", State: "FAILURE"}}})
	_ = sch.Tick(context.Background())
	_ = sch.Client.List(context.Background(), &l)
	if len(l.Items) != 2 {
		t.Fatal("a red Renovate PR older than 24 h starts renovate-red")
	}
	for _, it := range l.Items {
		if it.Spec.Source.Kind == "schedule" && strings.HasPrefix(it.Spec.Text, "Fix red") &&
			!strings.Contains(it.Spec.Text, "Red Renovate PRs: #40") {
			t.Fatalf("the probe's finding rides in the text: %q", it.Spec.Text)
		}
	}
}

// A factory down for a day does not replay a week: only a slot of the last hour fires.
func TestSchedulerIgnoresOldSlots(t *testing.T) {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	f := forge.NewFake()
	// Tuesday 06:20: the Monday slot is a day old, and only a slot of the last hour fires.
	sch := scheduler(f, fake.NewClientBuilder().WithScheme(s),
		func(context.Context) bool { return false }, time.Date(2026, 10, 13, 6, 20, 0, 0, time.UTC),
		&config.Config{Repository: "Smana/cloud-native-ref", Defaults: config.Defaults{DataClass: "public"},
			Schedules: []config.Schedule{{Name: "link-rot", Cron: "0 6 * * 1", Class: "docs-links", Text: "Fix broken external links."}}})
	if err := sch.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	var l v1alpha1.TaskList
	_ = sch.Client.List(context.Background(), &l)
	if len(l.Items) != 0 {
		t.Fatalf("a week-old slot does not replay: %d tasks", len(l.Items))
	}
}

// The stop object pauses intake, and schedules are intake.
func TestSchedulerStoppedPauses(t *testing.T) {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	f := forge.NewFake()
	sch := scheduler(f, fake.NewClientBuilder().WithScheme(s),
		func(context.Context) bool { return true }, time.Date(2026, 10, 5, 6, 20, 0, 0, time.UTC), schedulerCfg())
	if err := sch.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	var l v1alpha1.TaskList
	_ = sch.Client.List(context.Background(), &l)
	if len(l.Items) != 0 {
		t.Fatalf("the stop holds intake: %d tasks", len(l.Items))
	}
}
