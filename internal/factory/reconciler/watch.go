// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/narrate"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// maxHandled is the CRD's cap on status.handled.
const maxHandled = 512

// nextTrigger is why the next implementer run exists: what sent the task back to Queued, else
// initial for its first run and retry for any later one.
func nextTrigger(t *v1alpha1.Task) string {
	switch {
	case t.Status.NextTrigger != "":
		return t.Status.NextTrigger
	case len(t.Status.Runs) > 0:
		return "retry"
	}
	return "initial"
}

// nextImplementer builds the next implementer run. Before a pull request exists it is the fenced
// snapshot; after, the revise brief with the room's log and queued messages, and refs are the
// queued messages that brief quoted: the only ones the run may consume (F-A).
func (r *Reconciler) nextImplementer(ctx context.Context, t *v1alpha1.Task) (runs.Spec, []int64, string, error) {
	trigger := nextTrigger(t)
	if t.Status.PullRequest == nil {
		return r.implementerSpec(t, FirstBrief(t, r.Nonce())), nil, trigger, nil
	}
	// The finished run's handoff and verdict are after its start; brief.Build reads only those.
	evs, err := r.roomTail(ctx, t.Status.RoomRef, current(t).StartSeq, briefEvent)
	if err != nil {
		return runs.Spec{}, nil, "", err
	}
	q, err := r.Rooms.Queue(ctx, t.Status.RoomRef)
	if err != nil {
		return runs.Spec{}, nil, "", err
	}
	text, refs := ReviseBrief(t, evs, q, r.Nonce())
	return r.implementerSpec(t, text), refs, trigger, nil
}

// maxTailReads bounds roomTail: each EventsSince reads at most 10,000 events.
const maxTailReads = 10

// roomTail pages room's log after afterSeq to its end, keeping only the events keep selects. A
// single EventsSince stops at 10,000 events, so it would miss the newest of a long room.
func (r *Reconciler) roomTail(ctx context.Context, room string, after int64, keep func(envelope.Event) bool) ([]envelope.Event, error) {
	var out []envelope.Event
	for range maxTailReads {
		evs, cursor, err := r.Rooms.EventsSince(ctx, room, after)
		if err != nil {
			return nil, err
		}
		for _, e := range evs {
			if keep(e) {
				out = append(out, e)
			}
		}
		if len(evs) == 0 || cursor <= after {
			return out, nil
		}
		after = cursor
	}
	r.log().Warn("room log longer than the factory reads", "room", room, "after", after)
	return out, nil
}

// briefEvent is what brief.Build quotes from a room's log: handoffs and review verdicts.
func briefEvent(e envelope.Event) bool {
	if e.Type == envelope.Handoff {
		return true
	}
	var p struct {
		Kind envelope.MessageKind `json:"kind"`
	}
	return e.Type == envelope.Message && json.Unmarshal(e.Payload, &p) == nil && p.Kind == envelope.KindReviewVerdict
}

// lateReviews re-reads the pull request just before a revision starts. A maintainer's review
// submitted while the task waited in Queued joins this run: after it, the run's start would put
// the review before "since" for good. A dismissed review is skipped like any non-CHANGES_REQUESTED
// one. A pull request merged or closed meanwhile is not revised: the task goes back to
// AwaitingHuman, which ends it (done is true).
func (r *Reconciler) lateReviews(ctx context.Context, t *v1alpha1.Task) (bool, error) {
	pr, err := r.Forge.PullRequest(ctx, t.Status.PullRequest.Number)
	if err != nil {
		return false, err
	}
	if pr.State != "OPEN" {
		t.Status.NextTrigger = ""
		r.to(t, v1alpha1.PhaseAwaitingHuman, "")
		return true, nil
	}
	return false, r.queueReviews(ctx, t, pr, r.changesRequested(t, pr))
}

// changesRequested are the maintainers' "Request changes" reviews on the task's own pull request
// since its last run started, not yet acted on, oldest id first (Δ5). Anyone else's review is
// advice to a maintainer, not input; so is a review its author later approved over. Every one is
// returned, not the newest: two maintainers' reviews in one round are both the next run's input.
func (r *Reconciler) changesRequested(t *v1alpha1.Task, pr forge.PR) []forge.Review {
	if pr.HeadRef != "agent/"+t.Name { // only the task's branch is the factory's to revise
		return nil
	}
	since := current(t).Started
	approvedAt := map[string]int{} // a login's latest approval, by index; logins fold case
	for i, rv := range pr.Reviews {
		if rv.State == "APPROVED" {
			approvedAt[strings.ToLower(rv.Author)] = i
		}
	}
	var out []forge.Review
	for i, rv := range pr.Reviews {
		last, approved := approvedAt[strings.ToLower(rv.Author)]
		superseded := approved && last > i
		if rv.State == "CHANGES_REQUESTED" && r.Cfg.IsMaintainer(rv.Author) && !superseded &&
			!slices.Contains(t.Status.Handled, rv.ID) && (since == nil || rv.At.After(since.Time)) {
			out = append(out, rv)
		}
	}
	slices.SortFunc(out, func(a, b forge.Review) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// revise queues the reviews in the room (queueReviews) and sends the task back to Queued for a
// new implementer run: the caps and the human-driver rule (C4) stay in one place.
func (r *Reconciler) revise(ctx context.Context, t *v1alpha1.Task, pr forge.PR, rvs []forge.Review) error {
	if err := r.queueReviews(ctx, t, pr, rvs); err != nil {
		return err
	}
	record(ctx, func(ctx context.Context) { r.Metrics.Intervention(ctx, "request_changes") })
	t.Status.NextTrigger = "human"
	r.to(t, v1alpha1.PhaseQueued, "")
	narrateLater(t, narrate.Revising(t, rvs[len(rvs)-1].Author))
	return nil
}

// queueReviews enqueues each review in the room, as a sanitised ReviewMessage on the review
// stream keyed by its GitHub id, and records it handled. The broker stores an id once, whatever
// the order, so a replay is safe. A review dismissed after this still reaches the run its queue
// row feeds: the system API cannot remove a queued message.
func (r *Reconciler) queueReviews(ctx context.Context, t *v1alpha1.Task, pr forge.PR, rvs []forge.Review) error {
	for _, rv := range rvs {
		if err := r.Rooms.Enqueue(ctx, t.Status.RoomRef, "review", ReviewMessage(pr, rv), rv.ID); err != nil {
			return err
		}
	}
	for _, rv := range rvs {
		markHandled(t, rv.ID)
	}
	return nil
}

// markHandled records a review or comment id as acted on, keeping the newest maxHandled.
func markHandled(t *v1alpha1.Task, id int64) {
	t.Status.Handled = append(t.Status.Handled, id)
	if over := len(t.Status.Handled) - maxHandled; over > 0 {
		t.Status.Handled = slices.Delete(t.Status.Handled, 0, over)
	}
}

// cmdRetry asks for a fresh run of an escalated task (§6.3).
const cmdRetry = "/factory retry"

// command is the newest maintainer comment carrying verb alone on a line, on the task's issue or
// on pr (the zero PR when the task has none), posted since the task entered its phase and not yet
// acted on. Anyone else's command is dropped without an answer (T1), so it cannot make the
// factory talk. An edited comment never counts: its text may not be its author's.
func (r *Reconciler) command(ctx context.Context, t *v1alpha1.Task, pr forge.PR, verb string) (forge.Comment, bool, error) {
	all := slices.Clone(pr.Comments)
	if t.Spec.Issue > 0 {
		cs, err := r.Forge.RecentComments(ctx, t.Spec.Issue)
		if err != nil {
			return forge.Comment{}, false, err
		}
		all = append(all, cs...)
	}
	var found forge.Comment
	ok := false
	for _, c := range all {
		if c.Edited || !r.Cfg.IsMaintainer(c.Author) || slices.Contains(t.Status.Handled, c.ID) || !commandLine(c.Body, verb) ||
			(t.Status.PhaseSince != nil && !c.At.After(t.Status.PhaseSince.Time)) {
			continue
		}
		if !ok || c.At.After(found.At) {
			found, ok = c, true
		}
	}
	return found, ok, nil
}

// commandLine: some line of body is exactly verb, from its first character; trailing blanks
// aside. A quote ("> /factory retry") or a longer word ("/factory retrying") is not the command.
func commandLine(body, verb string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimRight(line, " \t\r") == verb {
			return true
		}
	}
	return false
}

// escalated waits for a maintainer (§6.3): a pull request merged or closed meanwhile ends the
// task; a /factory retry sends it back for a fresh run, through Queued and its caps. The command
// is marked handled in the same status write as the move, and no run is created here, so a replay
// after a lost write repeats the move, and queued's adopt never starts a second run.
func (r *Reconciler) escalated(ctx context.Context, t *v1alpha1.Task) error {
	var pr forge.PR
	if t.Status.PullRequest != nil {
		var err error
		if pr, err = r.Forge.PullRequest(ctx, t.Status.PullRequest.Number); err != nil {
			return err
		}
		if r.prEnded(ctx, t, pr) {
			return nil
		}
	}
	c, ok, err := r.command(ctx, t, pr, cmdRetry)
	if err != nil || !ok {
		return err
	}
	markHandled(t, c.ID)
	t.Status.Retries++
	t.Status.NextTrigger = "retry"
	record(ctx, func(ctx context.Context) { r.Metrics.Intervention(ctx, "retry") })
	next := v1alpha1.PhaseQueued
	if t.Status.RoomRef == "" { // escalated before it had a room (foreign_room): check the room again
		next = v1alpha1.PhaseTriaged
	}
	r.to(t, next, "")
	narrateLater(t, narrate.Retrying(t, c.Author))
	return nil
}

// labelStale marks a pull request the factory closed for want of a review (§6.3).
const labelStale = "factory/stale"

// quietSince is when a maintainer last touched the pull request: the task's entry into
// AwaitingHuman, or a later review or comment of theirs on it.
func (r *Reconciler) quietSince(t *v1alpha1.Task, pr forge.PR) time.Time {
	since := t.Status.PhaseSince.Time
	for _, rv := range pr.Reviews {
		if r.Cfg.IsMaintainer(rv.Author) && rv.At.After(since) {
			since = rv.At
		}
	}
	for _, c := range pr.Comments {
		if r.Cfg.IsMaintainer(c.Author) && c.At.After(since) {
			since = c.At
		}
	}
	return since
}

// remind nudges the maintainers once a pull request has waited RemindAfter for them, and closes
// it with factory/stale after StaleAfter (§6.3). Only maintainers' silence counts: a review or a
// comment of theirs starts the wait again. The close comes only after that spell's reminder was
// posted, and only on the task's own branch. The reminder goes through the outbox, written before
// it is posted, so a replay posts it once. The label goes on before the close, so a replay after
// a lost status write still reads the closed pull request as stale (prEnded).
func (r *Reconciler) remind(ctx context.Context, t *v1alpha1.Task, pr forge.PR) error {
	if t.Status.PhaseSince == nil || pr.HeadRef != "agent/"+t.Name {
		return nil
	}
	since := r.quietSince(t, pr)
	waited := r.Now().Sub(since)
	if waited < RemindAfter {
		return nil
	}
	reminder := narrate.Reminder(t, r.Cfg.Maintainers, since)
	if waited < StaleAfter || !slices.Contains(t.Status.Narrated, reminder.Key) {
		narrateLater(t, reminder)
		return nil
	}
	if err := r.Forge.AddLabels(ctx, pr.Number, labelStale); err != nil {
		return err
	}
	if err := r.Forge.ClosePR(ctx, pr.Number); err != nil {
		return err
	}
	class := t.Spec.PredictedClass
	record(ctx, func(ctx context.Context) { r.Metrics.PROutcome(ctx, class, "closed") })
	return r.end(ctx, t, v1alpha1.PhaseClosed, "stale")
}
