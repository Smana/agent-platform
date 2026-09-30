// SPDX-License-Identifier: Apache-2.0

package triage

import (
	"reflect"
	"testing"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
)

func cfg() *config.Config {
	return &config.Config{Defaults: config.Defaults{Template: "solo", Tier: "standard", DataClass: "public", PredictedClass: "review"},
		Tiers: map[string]config.Tier{"standard": {Model: "agent-default", RunTokens: 1_500_000, TaskTokens: 3_000_000, RunMinutes: 45}}}
}

func TestStaticUsesTheDefaults(t *testing.T) {
	d, err := Static{Cfg: cfg()}.Triage(t.Context(), &v1alpha1.Task{})
	if err != nil {
		t.Fatal(err)
	}
	want := v1alpha1.Budget{Tier: "standard", Model: "agent-default", RunTokens: 1_500_000, TaskTokens: 3_000_000, RunMinutes: 45}
	if d.Template != "solo" || d.PredictedClass != "review" || d.DataClass != "public" || d.Budget != want ||
		!reflect.DeepEqual(d.Classification, v1alpha1.Classification{Tier: "standard", Classifier: "static", Fallback: "static"}) {
		t.Fatalf("%+v", d)
	}
}

// The task's own data class wins over the default.
func TestStaticKeepsTheTasksDataClass(t *testing.T) {
	d, _ := Static{Cfg: cfg()}.Triage(t.Context(), &v1alpha1.Task{Spec: v1alpha1.TaskSpec{DataClass: "internal"}})
	if d.DataClass != "internal" {
		t.Fatal(d.DataClass)
	}
}
