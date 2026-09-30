// SPDX-License-Identifier: Apache-2.0

// Package triage decides a task's team and budget once (§2). Phase 1 uses the config's defaults;
// phase 4 adds the C7 classifier and the class matrix behind the same interface.
package triage

import (
	"context"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
)

// Decision is what triage fixes on a task: its template, predicted class, data class and budget.
type Decision struct {
	Template, PredictedClass, DataClass string
	Budget                              v1alpha1.Budget
	Classification                      v1alpha1.Classification
}

// Triager decides once per task.
type Triager interface {
	Triage(ctx context.Context, t *v1alpha1.Task) (Decision, error)
}

// BudgetFor is the tier's caps and model (§6.2, R11).
func BudgetFor(cfg *config.Config, tier string) v1alpha1.Budget {
	c := cfg.Tiers[tier]
	return v1alpha1.Budget{Tier: tier, Model: c.Model, RunTokens: c.RunTokens, TaskTokens: c.TaskTokens, RunMinutes: c.RunMinutes}
}

// Static is phase 1's triage: the config's defaults for every task.
type Static struct{ Cfg *config.Config }

// Triage returns the defaults; a task's own data class wins over the default one.
func (s Static) Triage(_ context.Context, t *v1alpha1.Task) (Decision, error) {
	d := s.Cfg.Defaults
	dataClass := t.Spec.DataClass
	if dataClass == "" {
		dataClass = d.DataClass
	}
	return Decision{Template: d.Template, PredictedClass: d.PredictedClass, DataClass: dataClass,
		Budget:         BudgetFor(s.Cfg, d.Tier),
		Classification: v1alpha1.Classification{Tier: d.Tier, Classifier: "static", Fallback: "static"}}, nil
}
