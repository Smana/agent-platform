// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"

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
		t.Status.Handled = append(t.Status.Handled, rv.ID)
	}
	if over := len(t.Status.Handled) - maxHandled; over > 0 {
		t.Status.Handled = slices.Delete(t.Status.Handled, 0, over)
	}
	return nil
}
