// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"slices"
	"strings"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/narrate"
	"github.com/Smana/agent-platform/internal/factory/rooms"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// The team engine (§3). Runs are sequential and only the implementer writes (S4): a reviewer or
// tester starts from agent/<taskId> with the pull request as its task (R25) and holds no forge
// write. Its own room_verdict decides the next step; a human steers through GitHub only (R36).

// nextVerifier is the template's next reviewer or tester after role, or "" when none follows.
func (r *Reconciler) nextVerifier(t *v1alpha1.Task, after string) string {
	roles := r.Cfg.Templates[t.Spec.Template].Roles
	i := slices.Index(roles, after)
	if i < 0 {
		return ""
	}
	for _, role := range roles[i+1:] {
		if role == "reviewer" || role == "tester" {
			return role
		}
	}
	return ""
}

// verifierSpec: a read-only role starts from the task's branch with the pull request as its task (R25).
func (r *Reconciler) verifierSpec(t *v1alpha1.Task, role string) runs.Spec {
	s := r.implementerSpec(t, "")
	s.Role, s.TaskText, s.TaskURL, s.BaseRef = role, "", t.Status.PullRequest.URL, "agent/"+t.Name
	return s
}

// startVerifier starts a reviewer or tester run and records the pull request's head it was given:
// its approve counts for that commit only (F1). A replay after a lost status write adopts the run
// it started instead; an adopted run's head is unknown, so its approve never counts. A pull
// request merged or closed meanwhile goes to AwaitingHuman, which ends the task.
func (r *Reconciler) startVerifier(ctx context.Context, t *v1alpha1.Task, role string) error {
	if adopted, err := r.adopt(ctx, t); err != nil || adopted {
		return err
	}
	pr, err := r.Forge.PullRequest(ctx, t.Status.PullRequest.Number)
	if err != nil {
		return err
	}
	if pr.State != "OPEN" {
		r.to(t, v1alpha1.PhaseAwaitingHuman, "")
		return nil
	}
	if err := r.startRun(ctx, t, r.verifierSpec(t, role), "review"); err != nil {
		return err
	}
	current(t).HeadSHA = pr.HeadSHA
	return nil
}

// ready: the work is done and reviewed. Phase 7 routes this to AwaitingCI and the merge gate.
func (r *Reconciler) ready(_ context.Context, t *v1alpha1.Task) error {
	r.to(t, v1alpha1.PhaseAwaitingHuman, "")
	return nil
}

// maxRounds is the template's bound on review rounds: every revision a verdict asks for, and every
// review run that has to start again, uses one.
func (r *Reconciler) maxRounds(t *v1alpha1.Task) int32 {
	return r.Cfg.Templates[t.Spec.Template].MaxReviewRounds
}

// reviewing waits for the reviewer or tester run to end, then acts on its own verdict: an approve
// of the pull request's unmoved head goes to the next verifier or to ready; changes send the task
// back to Queued for a revision, until the rounds run out; anything else is no verdict.
func (r *Reconciler) reviewing(ctx context.Context, t *v1alpha1.Task) error {
	run, found, err := r.observe(ctx, t)
	if err != nil {
		return err
	}
	if !found {
		return r.end(ctx, t, v1alpha1.PhaseEscalated, r.lostReason(ctx, t))
	}
	if !runs.Terminal(run.Phase) {
		return nil
	}
	reason := r.finished(ctx, t, run)
	if reason == "" {
		return nil
	}
	cur := current(t)
	cur.Reason = reason
	v, why, err := r.verdict(ctx, t)
	if err != nil {
		return err
	}
	if why != "" {
		return r.noVerdict(ctx, t, why)
	}
	if v.Verdict == "approve" {
		// A pull request merged or closed meanwhile ends the task through ready's AwaitingHuman, or
		// startVerifier's.
		pr, err := r.Forge.PullRequest(ctx, t.Status.PullRequest.Number)
		if err != nil {
			return err
		}
		// Never approve a moved head: the verdict names the commit the run was given, which is
		// still the pull request's head. An adopted run's head is "", which no commit prefixes.
		if pr.HeadSHA != cur.HeadSHA || !strings.HasPrefix(cur.HeadSHA, v.Commit) {
			return r.noVerdict(ctx, t, "verdict_stale")
		}
		cur.Verdict, t.Status.Verdict = v.Verdict, v.Verdict
		if next := r.nextVerifier(t, cur.Role); next != "" {
			return r.startVerifier(ctx, t, next)
		}
		return r.ready(ctx, t)
	}
	cur.Verdict, t.Status.Verdict = v.Verdict, v.Verdict
	if t.Status.ReviewRounds >= r.maxRounds(t) {
		narrateOn(t, t.Status.PullRequest.Number, narrate.RoundsExhausted(t, v))
		return r.end(ctx, t, v1alpha1.PhaseEscalated, "review_rounds_exhausted")
	}
	t.Status.ReviewRounds++
	t.Status.NextTrigger = "review"
	r.to(t, v1alpha1.PhaseQueued, "")
	return nil
}

// verdict is the current run's own verdict, read from the room after the run's start and before its
// end (F3). why names what made it none: a log too long to read to its end gives none, since an
// older verdict must never pass for the newest (F2).
func (r *Reconciler) verdict(ctx context.Context, t *v1alpha1.Task) (rooms.Verdict, string, error) {
	cur := current(t)
	evs, complete, err := r.roomTail(ctx, t.Status.RoomRef, cur.StartSeq, func(e envelope.Event) bool { return e.RunID == cur.ID })
	if err != nil {
		return rooms.Verdict{}, "", err
	}
	if !complete {
		return rooms.Verdict{}, "room_log_too_long", nil
	}
	if end, ok := rooms.LastRunEnd(evs, cur.ID); ok {
		evs = slices.DeleteFunc(evs, func(e envelope.Event) bool { return e.Seq > end.Seq })
	}
	v, ok := rooms.LastVerdict(evs, cur.ID)
	if !ok {
		return rooms.Verdict{}, "verdict_missing", nil
	}
	return v, "", nil
}

// noVerdict fails closed: a review run that ended without a verdict the factory can act on is never
// an approve. A new run of the same role starts while review rounds remain; then the task escalates.
func (r *Reconciler) noVerdict(ctx context.Context, t *v1alpha1.Task, why string) error {
	cur := current(t)
	cur.Verdict, t.Status.Verdict = "none", "none"
	if t.Status.ReviewRounds >= r.maxRounds(t) {
		return r.end(ctx, t, v1alpha1.PhaseEscalated, "no_verdict")
	}
	t.Status.ReviewRounds++
	narrateLater(t, narrate.NoVerdict(t, cur.ID, why))
	return r.startVerifier(ctx, t, cur.Role)
}
