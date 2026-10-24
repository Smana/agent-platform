// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"strconv"
	"strings"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

// adoptedTask: the Task is another author's pull request the factory adopted (MC10), with no
// room and no run.
func adoptedTask(t *v1alpha1.Task) bool { return t.Spec.Source.Kind == "adopt" }

// receivedAdopted (R39): an adopted Task is never triaged. Its class is the poller's, it has no
// template, no room and no run: it records its pull request and goes to the merge gate, which
// decides everything again on the head it reads.
func (r *Reconciler) receivedAdopted(ctx context.Context, t *v1alpha1.Task) error {
	i := strings.LastIndex(t.Spec.Source.Ref, "#")
	n, err := strconv.Atoi(t.Spec.Source.Ref[i+1:])
	if i < 0 || err != nil || n <= 0 {
		return r.end(ctx, t, v1alpha1.PhaseRejected, "adopt_ref_invalid")
	}
	pr, err := r.gh(ctx).PullRequest(ctx, n)
	if err != nil {
		return err
	}
	if t.Spec.Template != "" {
		t.Spec.Template = ""
		status := t.Status // Update returns the server's status into t; keep ours
		if err := r.Client.Update(ctx, t); err != nil {
			return err
		}
		t.Status = status
	}
	t.Status.PullRequest = &v1alpha1.PullRequestRef{Number: n, URL: pr.URL, NodeID: pr.NodeID, HeadSHA: pr.HeadSHA, Author: pr.Author}
	t.Status.ConfigHash = r.Cfg.Hash
	r.to(t, v1alpha1.PhaseAwaitingCI, "")
	return nil
}

// adoptedReturns are the hand-offs a new head undoes without the gate's own return list: the
// shadow decision's. Every other reason is the gate's (reenter returns the transient ones) or
// final for the pull request (a diff, a fork, a veto, a branch).
var adoptedReturns = map[string]bool{"shadow_would_arm": true}

// awaitingHumanAdopted (R45): Renovate's pull request is never reminded nor stale-closed, and a
// maintainer's Request changes is theirs to act on with Renovate: no revision run starts. A
// transient hand-off returns through reenter on a new head, counted; a shadow decision returns on
// Renovate's next push, uncounted, since it is no hand-off.
func (r *Reconciler) awaitingHumanAdopted(ctx context.Context, t *v1alpha1.Task, pr forge.PR) error {
	if back, err := r.reenter(ctx, t, pr); err != nil {
		r.log().Warn("merge gate return unreadable", "task.id", t.Name, "reason", t.Status.Reason, "err", err)
	} else if back {
		return nil
	}
	if adoptedReturns[t.Status.Reason] && pr.HeadSHA != "" && pr.HeadSHA != t.Status.PullRequest.HeadSHA {
		r.to(t, v1alpha1.PhaseAwaitingCI, "adopted_head_moved")
		return nil
	}
	return nil
}
