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
	return v1alpha1.Budget{Tier: tier, Model: c.Model, RunTokens: c.RunTokens, TaskTokens: c.TaskTokens, RunMinutes: c.RunMinutes,
		RunMicroUSD: config.MicroUSD(c.RunUSD), TaskMicroUSD: config.MicroUSD(c.TaskUSD)}
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

// sponsoredRuns is how many whole runs a ceiling holds: implementer, tester and reviewer. runFits
// admits a run only while the task's cap still holds a whole run cap, so a run cap equal to the
// ceiling would starve the second run.
const sponsoredRuns = 3

// Ceiling caps a budget under an api task's max_cost (F4, R5, C4): the task at the ceiling, each
// run at a third of it, in USD and in tokens at the reference rate, so an unpriced run keeps a
// ceiling too. It only ever lowers the class's caps, and never to 0, which reads as unset (I-2).
func Ceiling(b v1alpha1.Budget, maxMicroUSD int64) v1alpha1.Budget {
	if maxMicroUSD <= 0 {
		return b
	}
	taskTokens := int64(float64(maxMicroUSD) / config.RefUSDPerMToken)
	b.TaskMicroUSD = capAt(b.TaskMicroUSD, maxMicroUSD)
	b.RunMicroUSD = capAt(b.RunMicroUSD, max(1, maxMicroUSD/sponsoredRuns))
	b.TaskTokens = capAt(b.TaskTokens, max(1, taskTokens))
	b.RunTokens = capAt(b.RunTokens, max(1, taskTokens/sponsoredRuns))
	return b
}

// capAt is v under c: c when v is unset (0) or above it.
func capAt(v, c int64) int64 {
	if v == 0 || v > c {
		return c
	}
	return v
}
