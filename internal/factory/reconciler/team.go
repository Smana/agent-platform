// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/narrate"
	"github.com/Smana/agent-platform/internal/factory/rooms"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// The team engine (§3). Runs are sequential and only the implementer writes (S4): a reviewer or
// tester starts from agent/<taskId> with the pull request as its task (R25) and holds no forge
// write. Its own room_verdict decides the next step; a human steers through GitHub only (R36).
// Every run, a verifier's included, starts in Queued: the caps and the human-driver rule (C4) stay
// in one place.

// roomLogPatience is how long after a review run's end an unreadable room is waited for before the
// task escalates: the verdict is there, and only the room holds it.
const roomLogPatience = 30 * time.Minute

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

// otherTier is the reviewer's rule (§3): run where the implementer did not.
func otherTier(tier string) string {
	if tier == "frontier" {
		return "standard"
	}
	return "frontier"
}

// verifierSpec: a read-only role starts from the task's branch with the pull request as its task (R25).
func (r *Reconciler) verifierSpec(t *v1alpha1.Task, role string) runs.Spec {
	s := r.implementerSpec(t, "")
	s.Role, s.TaskText, s.TaskURL, s.BaseRef = role, "", t.Status.PullRequest.URL, "agent/"+t.Name
	if role == "reviewer" { // "on a different tier from the implementer where possible" (§3); R47
		s.Tier = otherTier(t.Spec.Budget.Tier)
		s.Model = r.Cfg.Tiers[s.Tier].Model
	}
	return s
}

// requestVerifier sends the task back to Queued for a reviewer or tester run.
func (r *Reconciler) requestVerifier(t *v1alpha1.Task, role string) {
	t.Status.NextRole = role
	r.to(t, v1alpha1.PhaseQueued, "")
}

// startVerifier starts the NextRole run from Queued, past the caps, C4 and lateReviews, on pr as
// queued checked it (open), and records its head: the run's approve counts for that commit only
// (F1). The verifiers of one chain all review one head: when the head moved since the run before,
// the chain starts again from the template's first verifier, so a ready task holds every
// verifier's approve of the same head. When a chain begins the run before is the implementer, whose
// record has no head, and NextRole is the first verifier already.
func (r *Reconciler) startVerifier(ctx context.Context, t *v1alpha1.Task, pr forge.PR) error {
	role := t.Status.NextRole
	if current(t).HeadSHA != pr.HeadSHA {
		role = r.nextVerifier(t, "implementer")
	}
	s := r.verifierSpec(t, role)
	s.Head = pr.HeadSHA // rides the claim (AnnHead): a replay that records the run knows its head
	if err := r.startRun(ctx, t, s, "review"); err != nil {
		return err
	}
	t.Status.NextRole = ""
	return nil
}

// ready: the work is done and reviewed; CI and the merge gate decide the rest (§4).
func (r *Reconciler) ready(_ context.Context, t *v1alpha1.Task) error {
	r.to(t, v1alpha1.PhaseAwaitingCI, "")
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
	if err := r.boundPending(ctx, t, run); err != nil {
		return err
	}
	if !runs.Terminal(run.Phase) {
		if s, err := r.stuck(ctx, t, run); err != nil || s {
			if err != nil {
				return err
			}
			return r.end(ctx, t, v1alpha1.PhaseEscalated, "stuck")
		}
		return nil
	}
	reason := r.finished(ctx, t, run)
	if reason == "" {
		return nil
	}
	cur := current(t)
	cur.Reason = reason
	r.interventions(ctx, t)
	v, why, err := r.verdict(ctx, t)
	if err != nil {
		return r.roomUnreadable(ctx, t, err)
	}
	if why != "" {
		if r.resumable(t, run) {
			r.resume(ctx, t, run)
			return nil
		}
		return r.noVerdict(ctx, t, why)
	}
	if v.Verdict == "approve" {
		// A pull request merged or closed meanwhile ends the task through ready's AwaitingCI, or
		// the next Queued's lateReviews.
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
			r.requestVerifier(t, next)
			return nil
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

// roomUnreadable: the run ended and its verdict is in a room the factory cannot read. The task
// says so while it retries; a room the broker has no log for escalates at once, and any other
// failure roomLogPatience after the run's end. Never an approve.
func (r *Reconciler) roomUnreadable(ctx context.Context, t *v1alpha1.Task, err error) error {
	if errors.Is(err, rooms.ErrNoRoom) || r.Now().Sub(current(t).Finished.Time) >= roomLogPatience {
		r.log().Warn("review verdict unreadable", "task.id", t.Name, "run.id", current(t).ID, "err", err)
		return r.end(ctx, t, v1alpha1.PhaseEscalated, "room_log_unreadable")
	}
	t.Status.Reason = "waiting_room_log"
	return err
}

// verdict is the current run's own verdict, read from the room after the run's start and before its
// first end (F3, M6). why names what made it none: a log too long to read to its end gives none,
// since an older verdict must never pass for the newest (F2).
func (r *Reconciler) verdict(ctx context.Context, t *v1alpha1.Task) (rooms.Verdict, string, error) {
	cur := current(t)
	evs, complete, err := r.roomTail(ctx, t.Status.RoomRef, cur.StartSeq, func(e envelope.Event) bool { return e.RunID == cur.ID })
	if err != nil {
		return rooms.Verdict{}, "", err
	}
	if !complete {
		return rooms.Verdict{}, "room_log_too_long", nil
	}
	if end, ok := rooms.FirstRunEnd(evs, cur.ID); ok {
		evs = slices.DeleteFunc(evs, func(e envelope.Event) bool { return e.Seq > end.Seq })
	}
	v, ok := rooms.LastVerdict(evs, cur.ID)
	if !ok {
		return rooms.Verdict{}, "verdict_missing", nil
	}
	return v, "", nil
}

// noVerdict fails closed: a review run that ended without a verdict the factory can act on is never
// an approve. A new run of the same role is queued while review rounds remain; then the task
// escalates.
func (r *Reconciler) noVerdict(ctx context.Context, t *v1alpha1.Task, why string) error {
	cur := current(t)
	cur.Verdict, t.Status.Verdict = "none", "none"
	if t.Status.ReviewRounds >= r.maxRounds(t) {
		return r.end(ctx, t, v1alpha1.PhaseEscalated, "no_verdict")
	}
	t.Status.ReviewRounds++
	narrateLater(t, narrate.NoVerdict(t, cur.ID, why))
	r.requestVerifier(t, cur.Role)
	return nil
}
