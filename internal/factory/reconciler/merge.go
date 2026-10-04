// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/narrate"
)

// awaitingCI is §4's merge gate: §5.1's rule decides the pull request, and a decided "arm"
// merges it now at the head that was decided (R52). A maintainer's Request changes is honoured
// here as it is in AwaitingHuman (Δ5): the gate's wait must not silence a review.
func (r *Reconciler) awaitingCI(ctx context.Context, t *v1alpha1.Task) error {
	n := t.Status.PullRequest.Number
	pr, err := r.Forge.PullRequest(ctx, n)
	if err != nil {
		return err
	}
	switch pr.State {
	case "MERGED": // a maintainer merged it through the bypass…
		if pr.MergedBy != r.Cfg.Merge.MergerLogin {
			t.Status.PullRequest.MergedBy, t.Status.PullRequest.MergeCommitSHA = pr.MergedBy, pr.MergeCommitSHA
			class := t.Spec.PredictedClass
			record(ctx, func(ctx context.Context) { r.Metrics.PROutcome(ctx, class, "human_merged") })
			return r.end(ctx, t, v1alpha1.PhaseDone, "merged")
		}
		// …or our own merge landed but its write was lost: it verifies like the auto-merge it is.
		r.to(t, v1alpha1.PhaseAutoMerging, "")
		return r.autoMerging(ctx, t)
	case "CLOSED":
		class := t.Spec.PredictedClass
		record(ctx, func(ctx context.Context) { r.Metrics.PROutcome(ctx, class, "closed") })
		return r.end(ctx, t, v1alpha1.PhaseClosed, "pr_closed")
	}
	if rvs := r.changesRequested(t, pr); len(rvs) > 0 {
		return r.revise(ctx, t, pr, rvs)
	}
	checks, err := r.Merger.PullRequestChecks(ctx, n)
	if err != nil {
		return err
	}
	paused, err := r.paused(ctx, t.Spec.PredictedClass)
	if err != nil {
		return err
	}
	today, err := r.armedToday(ctx)
	if err != nil {
		return err
	}
	in := ArmInputs{PR: pr, Checks: checks, Task: t, Cfg: r.Cfg, ArmedToday: today, Paused: paused}
	if CIState(checks, r.Cfg.Merge.RequiredChecks) == "SUCCESS" {
		// R02/R03: the rule can only speak about a commit the room reported and the verifiers
		// approved. Neither read matters while CI decides on its own, so they wait for green.
		in.ReportedHead, err = r.reportedHead(ctx, t)
		if err != nil {
			return err
		}
		in.Verifiers = verifiers(t)
	}
	d := DecideArm(in)
	if (d.Verdict == "arm" || d.Verdict == "shadow") && (t.Spec.PredictedClass == "docs-links" || t.Spec.PredictedClass == "revert") {
		// forge.Files is read late: only a decision that would reach the LinksOnly gate of an
		// entry class pays its 2N+1 API calls. The first pass passed that gate vacuously; this
		// second pass decides on the real diff.
		base, head, ferr := r.Merger.Files(ctx, "main", pr.HeadSHA)
		if ferr != nil {
			return ferr
		}
		in.Files.Base, in.Files.Head = base, head
		d = DecideArm(in)
	}
	switch d.Verdict {
	case "wait":
		t.Status.Reason = d.Reason
		return nil
	case "ci_red":
		return r.fixCI(ctx, t, pr, checks)
	case "human":
		if d.Matched != "" && d.Matched != t.Spec.PredictedClass {
			pred, matched := t.Spec.PredictedClass, d.Matched
			record(ctx, func(ctx context.Context) { r.Metrics.ClassMismatch(ctx, pred, matched) })
		}
		r.to(t, v1alpha1.PhaseAwaitingHuman, d.Reason)
		narrateLater(t, narrate.WaitingForHuman(t, d.Reason))
		return nil
	case "shadow": // R32: the decision is recorded and narrated; a maintainer merges or closes
		r.to(t, v1alpha1.PhaseAwaitingHuman, d.Reason)
		narrateOn(t, pr.Number, narrate.WouldArm(t, d.Matched))
		return nil
	}
	// The decided head, pinned: GitHub checks it at merge time, so a push that landed since the
	// read refuses the merge (409) and the new head is decided again — never merged on this
	// decision (SC-14). A merge GitHub does not allow as it stands (405) is a human's to fix.
	if err := r.Merger.Merge(ctx, pr.NodeID, pr.HeadSHA); err != nil {
		switch {
		case errors.Is(err, forge.ErrHeadMoved):
			r.to(t, v1alpha1.PhaseAwaitingCI, "head_moved")
			return nil
		case errors.Is(err, forge.ErrNotMergeable):
			r.to(t, v1alpha1.PhaseAwaitingHuman, "not_mergeable")
			narrateLater(t, narrate.WaitingForHuman(t, "not_mergeable"))
			return nil
		default:
			return err
		}
	}
	armed := metav1.NewTime(r.Now())
	t.Status.PullRequest.ArmedAt, t.Status.PullRequest.HeadSHA = &armed, pr.HeadSHA
	r.to(t, v1alpha1.PhaseAutoMerging, "")
	narrateOn(t, pr.Number, narrate.Armed(t))
	return nil
}

// reportedHead is the commit of the latest handoff or final room event whose broker-stamped
// actor is one of the task's implementer runs (R02's input to DecideArm). A log that could not
// be read to its end reports nothing: the newest event may be past what was read, and deciding
// on a stale newest-commit is exactly what R02 refuses.
func (r *Reconciler) reportedHead(ctx context.Context, t *v1alpha1.Task) (string, error) {
	impl := make(map[string]bool, len(t.Status.Runs))
	for _, rec := range t.Status.Runs {
		if rec.Role == "implementer" {
			impl[rec.ID] = true
		}
	}
	evs, complete, err := r.roomTail(ctx, t.Status.RoomRef, 0, briefEvent)
	if err != nil {
		return "", err
	}
	if !complete {
		return "", nil
	}
	head := ""
	for _, e := range evs {
		if !impl[e.RunID] {
			continue
		}
		if c := eventCommit(e); c != "" {
			head = c
		}
	}
	return head, nil
}

// eventCommit is the commit a handoff or a review verdict names, "" when it names none.
func eventCommit(e envelope.Event) string {
	if e.Type == envelope.Handoff {
		var p envelope.HandoffPayload
		if json.Unmarshal(e.Payload, &p) == nil {
			return p.Commit
		}
		return ""
	}
	var p envelope.MessagePayload
	if json.Unmarshal(e.Payload, &p) == nil && p.Kind == envelope.KindReviewVerdict {
		return p.Commit
	}
	return ""
}

// verifiers are the task's reviewer and tester run records, each carrying the head it was
// given (R03's input to DecideArm).
func verifiers(t *v1alpha1.Task) []v1alpha1.RunRecord {
	var out []v1alpha1.RunRecord
	for _, rec := range t.Status.Runs {
		if rec.Role == "reviewer" || rec.Role == "tester" {
			out = append(out, rec)
		}
	}
	return out
}

func failing(c forge.Checks, required []string) []string {
	var out []string
	for _, x := range c.Runs {
		if x.State == "FAILURE" && slices.Contains(required, x.Name) {
			out = append(out, x.Name)
		}
	}
	return out
}

// fixCI: two fix runs, then escalate with the failure on the PR (§6.3).
func (r *Reconciler) fixCI(ctx context.Context, t *v1alpha1.Task, pr forge.PR, c forge.Checks) error {
	names := failing(c, r.Cfg.Merge.RequiredChecks)
	if t.Status.FixRuns >= r.Cfg.Merge.FixRuns {
		narrateOn(t, pr.Number, narrate.CIExhausted(t, names))
		return r.end(ctx, t, v1alpha1.PhaseEscalated, "ci_red")
	}
	t.Status.FixRuns++
	msg := fmt.Sprintf("CI failed on %s: %s. Read the failing jobs with `gh pr checks %d` and fix them on the same branch.",
		pr.HeadSHA, strings.Join(names, ", "), pr.Number)
	if err := r.Rooms.Enqueue(ctx, t.Status.RoomRef, "ci", msg, int64(t.Status.FixRuns)); err != nil {
		return err
	}
	t.Status.NextTrigger = "ci"
	r.to(t, v1alpha1.PhaseQueued, "")
	return nil
}

// autoMerging watches the merge the decision issued. R52 armed nothing on GitHub — the merge
// either landed or did not — so there is no disarm path: an open pull request whose head moved
// never merged, and the new head is decided again from AwaitingCI.
func (r *Reconciler) autoMerging(ctx context.Context, t *v1alpha1.Task) error {
	pr, err := r.Forge.PullRequest(ctx, t.Status.PullRequest.Number)
	if err != nil {
		return err
	}
	ref := t.Status.PullRequest
	class := t.Spec.PredictedClass
	switch {
	case pr.State == "MERGED":
		now := metav1.NewTime(r.Now())
		ref.MergedBy, ref.MergeCommitSHA, ref.MergedAt = pr.MergedBy, pr.MergeCommitSHA, &now
		ref.AutoMerged = pr.MergedBy == r.Cfg.Merge.MergerLogin // AutoMerged: merged by the merger App (R52)
		if !ref.AutoMerged {
			record(ctx, func(ctx context.Context) { r.Metrics.PROutcome(ctx, class, "human_merged") })
			return r.end(ctx, t, v1alpha1.PhaseDone, "merged")
		}
		record(ctx, func(ctx context.Context) { r.Metrics.PROutcome(ctx, class, "auto_merged") })
		r.to(t, v1alpha1.PhaseVerifying, "")
	case pr.State == "CLOSED":
		record(ctx, func(ctx context.Context) { r.Metrics.PROutcome(ctx, class, "closed") })
		return r.end(ctx, t, v1alpha1.PhaseClosed, "pr_closed")
	case pr.HeadSHA != ref.HeadSHA:
		r.to(t, v1alpha1.PhaseAwaitingCI, "head_moved")
	}
	return nil
}

// MainState is §6.4's watch of main's CI on the merge commit: FAILURE if a watched check that ran
// failed, PENDING while one that ran is still running, else SUCCESS. A watched check that never
// ran is not failing: a path-filtered push workflow never reports on a docs merge.
func MainState(runs []forge.Check, watched []string) string {
	state := "SUCCESS"
	for _, c := range runs {
		if !slices.Contains(watched, c.Name) {
			continue
		}
		switch c.State {
		case "FAILURE":
			return "FAILURE"
		case "PENDING":
			state = "PENDING"
		}
	}
	return state
}

// verifying watches main's CI on the merge commit for merge.verifyFor (§6.4). Red, or a
// maintainer's factory/revert, reverts; not red for the whole window, with nothing still running,
// is done.
func (r *Reconciler) verifying(ctx context.Context, t *v1alpha1.Task) error {
	if t.Annotations[v1alpha1.AnnotationRevert] != "" {
		return r.revert(ctx, t, "revert_requested")
	}
	ref := t.Status.PullRequest
	runs, err := r.Merger.CommitChecks(ctx, ref.MergeCommitSHA)
	if err != nil {
		return err
	}
	switch MainState(runs, r.Cfg.Merge.VerifyChecks) {
	case "FAILURE":
		return r.revert(ctx, t, "main_red")
	case "SUCCESS":
		if ref.MergedAt != nil && r.Now().Sub(ref.MergedAt.Time) >= r.Cfg.Merge.VerifyFor.Duration {
			return r.end(ctx, t, v1alpha1.PhaseDone, "merged_verified")
		}
	}
	return nil
}

// revertable: a Done, auto-merged task a maintainer asked to revert inside the window.
func (r *Reconciler) revertable(t *v1alpha1.Task) bool {
	ref := t.Status.PullRequest
	return t.Status.Phase == v1alpha1.PhaseDone && t.Annotations[v1alpha1.AnnotationRevert] != "" &&
		ref != nil && ref.AutoMerged && ref.RevertNumber == 0 && ref.MergedAt != nil &&
		r.Now().Sub(ref.MergedAt.Time) <= r.Cfg.Merge.RevertWindow.Duration
}

// revert opens the merger-authored revert PR and arms it: the policy's `revert` rule carries
// docs-links' paths and caps, so GitHub merges it once its checks are green (§6.4). Human
// merges are never reverted: only an auto-merged task reaches here, so before the wave (R32)
// no revert is ever opened.
func (r *Reconciler) revert(ctx context.Context, t *v1alpha1.Task, why string) error {
	ref := t.Status.PullRequest
	if ref.RevertNumber > 0 { // a lost write's replay: the revert exists; end, never open a second
		return r.end(ctx, t, v1alpha1.PhaseReverted, why)
	}
	pr, err := r.Forge.PullRequest(ctx, ref.Number)
	if err != nil {
		return err
	}
	rv, err := r.Merger.RevertPR(ctx, ref.NodeID, fmt.Sprintf("Revert %q", pr.Title),
		fmt.Sprintf("Reverts #%d (agent factory task `%s`): %s.", ref.Number, t.Name, narrate.Reason(why)))
	if err != nil {
		return err
	}
	ref.RevertNumber = rv.Number
	if r.Cfg.Classes["revert"].Live { // not live (R32): the revert PR opens and a maintainer merges it
		if err := r.Merger.EnableAutoMerge(ctx, rv.NodeID, ""); err != nil { // the merger's own revert-* branch
			return err
		}
	}
	class := t.Spec.PredictedClass
	record(ctx, func(ctx context.Context) { r.Metrics.PROutcome(ctx, class, "reverted") })
	narrateOn(t, ref.Number, narrate.RevertOpened(t, rv.Number, why))
	return r.end(ctx, t, v1alpha1.PhaseReverted, why)
}

// revertPending: a Reverted task whose revert PR has not been seen merged, closed or stalled.
func (r *Reconciler) revertPending(t *v1alpha1.Task) bool {
	ref := t.Status.PullRequest
	return t.Status.Phase == v1alpha1.PhaseReverted && ref != nil && ref.RevertNumber > 0 &&
		!slices.ContainsFunc(t.Status.Narrated, func(k string) bool { return strings.HasPrefix(k, "revert-end-") })
}

// revertWatch: a revert opened while main is red for another reason never goes green, and must
// not stay armed indefinitely. Merged or closed ends the watch; still open after verifyFor is
// disarmed and handed to a maintainer (§6.4). The narration key ends the watch, once.
func (r *Reconciler) revertWatch(ctx context.Context, t *v1alpha1.Task) error {
	ref := t.Status.PullRequest
	pr, err := r.Forge.PullRequest(ctx, ref.RevertNumber)
	if err != nil {
		return err
	}
	switch {
	case pr.State == "MERGED" || pr.State == "CLOSED":
		t.Status.Narrated = append(t.Status.Narrated, "revert-end-"+strings.ToLower(pr.State))
		return nil
	case t.Status.PhaseSince != nil && r.Now().Sub(t.Status.PhaseSince.Time) >= r.Cfg.Merge.VerifyFor.Duration:
		if err := r.Merger.DisableAutoMerge(ctx, pr.NodeID); err != nil {
			return err
		}
		return r.narrator().Post(ctx, t, ref.Number, narrate.RevertStalled(t, ref.RevertNumber))
	}
	return nil
}

// paused: the circuit breaker. One revert of a class under the current config stops arming
// for that class until the config changes (§6.4).
func (r *Reconciler) paused(ctx context.Context, class string) (bool, error) {
	n, err := r.countTasks(ctx, func(o *v1alpha1.Task) bool {
		return o.Status.Phase == v1alpha1.PhaseReverted && o.Spec.PredictedClass == class && o.Status.ConfigHash == r.Cfg.Hash
	})
	return n > 0, err
}

// armedToday counts the merges the factory issued today: Arming is recorded whichever way the
// merge itself went, and §5.1's cap counts the decisions, not their outcomes.
func (r *Reconciler) armedToday(ctx context.Context) (int, error) {
	return r.countTasks(ctx, func(o *v1alpha1.Task) bool {
		ref := o.Status.PullRequest
		return ref != nil && ref.ArmedAt != nil && sameUTCDay(ref.ArmedAt.Time, r.Now())
	})
}
