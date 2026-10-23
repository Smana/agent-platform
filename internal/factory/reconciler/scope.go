// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/steering"
)

// scopeAtTriage fixes the task's scope mode and steering note (scoper design SC2, SC3): the
// class's mode, off for an investigate task, and in any other mode the class's note on the
// default branch of a repository this factory owns (v2 D7: a fence it is not named in is not
// read). An absent note is no steering; an invalid one is not used, counted and logged, and the
// task proceeds (SC4). A read error is returned: the triage is retried.
func (r *Reconciler) scopeAtTriage(ctx context.Context, t *v1alpha1.Task) error {
	mode := r.Cfg.ScopeFor(t.Spec.PredictedClass)
	if roles := r.Cfg.Templates[t.Spec.Template].Roles; len(roles) > 0 && roles[0] == "triager" {
		mode = config.ScopeOff // R38's investigate path is never scoped
	}
	t.Spec.ScopeMode, t.Spec.Steering = string(mode), nil
	path := steering.Path(t.Spec.PredictedClass)
	if mode == config.ScopeOff || path == "" || r.Owned == nil || !r.Owned(t.Spec.Repository) {
		return nil
	}
	text, sha, found, err := r.gh(ctx).DefaultBranchFile(ctx, path)
	if err != nil {
		return err
	}
	read := "used"
	switch verr := steering.Validate(text); {
	case !found:
		read = "absent"
	case verr != nil:
		read = "invalid"
		r.log().Warn("steering note not used", "task.id", t.Name, "path", path, "sha", sha, "err", verr)
	default:
		t.Spec.Steering = &v1alpha1.Steering{Path: path, SHA: sha, Text: text}
	}
	class := t.Spec.PredictedClass
	record(ctx, func(ctx context.Context) { r.Metrics.SteeringRead(ctx, class, read) })
	return nil
}
