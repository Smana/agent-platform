// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/forge"
	"github.com/Smana/maku/internal/factory/steering"
)

// scopeAtTriage fixes the task's scope mode and steering note (scoper design SC2, SC3): the
// class's mode, off for an investigate task, and in any other mode the class's note on the
// default branch of a repository this factory owns (v2 D7: a fence it is not named in is not
// read, and says so). An absent note is no steering; an invalid one is not used, counted and logged, and the
// task proceeds (SC4), as does one that is never readable (a directory, a symlink). A retryable read error is returned: the triage is retried.
func (r *Reconciler) scopeAtTriage(ctx context.Context, t *v1alpha1.Task) error {
	mode := r.Cfg.ScopeFor(t.Spec.PredictedClass)
	if r.Cfg.Investigates(t.Spec.Template) {
		mode = config.ScopeOff // R38's investigate path is never scoped
	}
	t.Spec.ScopeMode, t.Spec.Steering = string(mode), nil
	path := steering.Path(t.Spec.PredictedClass)
	if mode == config.ScopeOff || path == "" {
		return nil
	}
	class := t.Spec.PredictedClass
	if r.Owned == nil || !r.Owned(t.Spec.Repository) {
		// Not fenced to this factory, or its fence cache went stale (an outage): no read and no
		// retry, the task proceeds unsteered, visibly.
		r.log().Warn("steering note not read: repository not owned", "task.id", t.Name, "repository", t.Spec.Repository, "class", class)
		record(ctx, func(ctx context.Context) { r.Metrics.SteeringRead(ctx, class, "unowned") })
		return nil
	}
	text, sha, found, err := r.gh(ctx).DefaultBranchFile(ctx, path)
	if err != nil && !errors.Is(err, forge.ErrNotAFile) {
		return err
	}
	read := "used"
	switch verr := steering.Validate(text); {
	case errors.Is(err, forge.ErrNotAFile): // never readable: retrying cannot help (SC4)
		read = "invalid"
		r.log().Warn("steering note not used", "task.id", t.Name, "path", path, "sha", sha, "err", err)
	case !found:
		read = "absent"
	case verr != nil:
		read = "invalid"
		r.log().Warn("steering note not used", "task.id", t.Name, "path", path, "sha", sha, "err", verr)
	default:
		t.Spec.Steering = &v1alpha1.Steering{Path: path, SHA: sha, Text: text}
	}
	record(ctx, func(ctx context.Context) { r.Metrics.SteeringRead(ctx, class, read) })
	return nil
}
