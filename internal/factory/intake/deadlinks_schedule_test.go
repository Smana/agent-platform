// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/forge/forgetest"
)

func deadLinksScheduler(t *testing.T, cl config.Class, objs ...v1alpha1.Task) *Scheduler {
	t.Helper()
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s)
	for i := range objs {
		c = c.WithObjects(&objs[i])
	}
	now := time.Date(2026, 10, 22, 5, 0, 0, 0, time.UTC)
	f := forgetest.NewFake()
	art := []byte(`[{"file":"docs/a.md","line":1,"url":"https://example.com/gone","status":"404"}]`)
	f.SetRuns([]forge.WorkflowRun{{ID: 2, Event: "schedule", Branch: "main", Conclusion: "success"},
		{ID: 1, Event: "schedule", Branch: "main", Conclusion: "success"}}, map[int64][]byte{1: art, 2: art})
	f.SetMainFiles(map[string]string{"docs/a.md": "[a](https://example.com/gone)"})
	cfg := &config.Config{Repository: "Smana/cloud-native-ref", Maintainers: []string{"Smana"}, Defaults: config.Defaults{DataClass: "public"},
		Classes:   map[string]config.Class{"dead-links": cl},
		Schedules: []config.Schedule{{Name: "dead-links", Cron: "0 5 * * *", Class: "dead-links", Text: "Fix the dead links below.", Probe: "dead-links"}}}
	return scheduler(f, c, func(context.Context) bool { return false }, now, cfg)
}

func scheduledTasks(t *testing.T, s *Scheduler) []v1alpha1.Task {
	var l v1alpha1.TaskList
	if err := s.Client.List(t.Context(), &l); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

// MC14 and L9: a class off has no intake; a class in shadow gets one task of its findings, and
// none more while that task is open.
func TestTheDeadLinksSchedule(t *testing.T) {
	cl := linksClass()
	sch := deadLinksScheduler(t, cl)
	if err := sch.Tick(t.Context()); err != nil || len(scheduledTasks(t, sch)) != 0 {
		t.Fatalf("a class off starts nothing: %v", err)
	}
	cl.Shadow = true
	sch = deadLinksScheduler(t, cl)
	if err := sch.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	ts := scheduledTasks(t, sch)
	if len(ts) != 1 || len(ts[0].Spec.Findings) != 1 || ts[0].Spec.Findings[0].URL != "https://example.com/gone" ||
		ts[0].Spec.PredictedClass != "dead-links" {
		t.Fatalf("%+v", ts)
	}
	open := v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "abcdefgh", Namespace: "agent-system"},
		Spec:   v1alpha1.TaskSpec{Source: v1alpha1.Source{Kind: "schedule", Ref: "dead-links"}},
		Status: v1alpha1.TaskStatus{Phase: v1alpha1.PhaseAwaitingHuman}}
	sch = deadLinksScheduler(t, cl, open)
	if err := sch.Tick(t.Context()); err != nil || len(scheduledTasks(t, sch)) != 1 {
		t.Fatalf("an open task of the schedule holds the next one (L9): %v", err)
	}
}

// A refused run (malformed JSON) starts no task, and is counted and logged, not a tick error.
func TestAMalformedArtifactStartsNothingAndIsCounted(t *testing.T) {
	cl := linksClass()
	cl.Shadow = true
	sch := deadLinksScheduler(t, cl)
	f := forgetest.NewFake()
	f.SetRuns([]forge.WorkflowRun{{ID: 2, Event: "schedule", Branch: "main", Conclusion: "success"},
		{ID: 1, Event: "schedule", Branch: "main", Conclusion: "success"}}, map[int64][]byte{1: []byte(`[`), 2: []byte(`[]`)})
	sch.Forge, sch.Merger = f, f
	if err := sch.Tick(t.Context()); err != nil || len(scheduledTasks(t, sch)) != 0 {
		t.Fatalf("a malformed artifact starts nothing: %v", err)
	}
	if c := sch.Errors.(*counter); len(c.sources) != 1 || c.sources[0] != "dead-links" {
		t.Fatalf("counted: %v", c.sources)
	}
}

// The same run pair is weighed once, however many ticks; a new run is weighed again. A refusal is
// counted once per pair.
func TestTheSameRunPairIsEvaluatedOnce(t *testing.T) {
	cl := linksClass()
	cl.Shadow = true
	sch := deadLinksScheduler(t, cl)
	f := &countingMerger{Fake: forgetest.NewFake()}
	runs := func(ids ...int64) {
		var rs []forge.WorkflowRun
		arts := map[int64][]byte{}
		for _, id := range ids {
			rs = append(rs, forge.WorkflowRun{ID: id, Event: "schedule", Branch: "main", Conclusion: "success"})
			arts[id] = []byte(`[`) // malformed: refused, so no task ends the loop early
		}
		f.SetRuns(rs, arts)
	}
	sch.Forge, sch.Merger = f, f
	runs(2, 1)
	for range 3 {
		_ = sch.Tick(t.Context())
	}
	if f.artifacts != 1 || len(sch.Errors.(*counter).sources) != 1 {
		t.Fatalf("one evaluation and one count for three ticks: %d reads, %v", f.artifacts, sch.Errors.(*counter).sources)
	}
	runs(3, 2)
	_ = sch.Tick(t.Context())
	if f.artifacts != 2 || len(sch.Errors.(*counter).sources) != 2 {
		t.Fatalf("a new run is weighed again: %d reads, %v", f.artifacts, sch.Errors.(*counter).sources)
	}
}

type countingMerger struct {
	*forgetest.Fake
	artifacts int
}

func (c *countingMerger) RunArtifact(ctx context.Context, id int64, name, file string) ([]byte, error) {
	c.artifacts++
	return c.Fake.RunArtifact(ctx, id, name, file)
}
