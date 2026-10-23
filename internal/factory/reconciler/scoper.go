// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// scoperDue: the task's mode is scoper and its scoper has not ended (scoper plan S13). A resumed or
// retried scoper is due again; once status.scope is set, every later run is an implementer's.
func scoperDue(t *v1alpha1.Task) bool {
	return t.Spec.ScopeMode == string(config.ScopeScoper) && t.Status.Scope == nil
}

// scoperSpec is the scoper's claim: a triager run at the light tier's run caps whatever the task's
// tier (SO2), on the task's model until per-role models exist (SC10). The tier label is what the
// meter caps the run's USD by. Its spend counts toward the task's caps as every run's does.
func (r *Reconciler) scoperSpec(t *v1alpha1.Task, text string) runs.Spec {
	s := r.implementerSpec(t, text)
	light := r.Cfg.Tiers["light"]
	s.Role, s.Tier, s.MaxTokens, s.MaxMinutes = "triager", "light", light.RunTokens, light.RunMinutes
	return s
}

// startScoper starts the scoper run, or resumes a lost one with its own brief.
func (r *Reconciler) startScoper(ctx context.Context, t *v1alpha1.Task) error {
	text, ok, err := r.resumeBrief(ctx, t)
	if err != nil {
		return err
	}
	if !ok {
		text = ScoperBrief(t, r.Nonce())
	}
	return r.startRun(ctx, t, r.scoperSpec(t, text), nextTrigger(t))
}
