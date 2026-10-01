// SPDX-License-Identifier: Apache-2.0

// Package narrate is the factory talking where people already are (Δ6): one issue or PR comment
// per event, posted by the factory App and never twice (R22), and the task's task_state messages
// in its room, once per clientSeq (ruling SK). A narration carries the factory's own words and
// ids only, never text a user wrote.
package narrate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// MaxNarrated is status.narrated's maxItems in the Task CRD: Post trims the oldest keys past it.
const MaxNarrated = 512

// roomPrefix marks a room message's key in status.narrated: room/<clientSeq>/<key>.
const roomPrefix = "room/"

// Event is one narration: its idempotency key and its markdown body.
type Event struct {
	Key  string
	Body string
}

// commenter is the part of the forge narration uses.
type commenter interface {
	RecentComments(ctx context.Context, number int) ([]forge.Comment, error)
	Comment(ctx context.Context, number int, body string) error
}

// Narrator posts as the factory App. Login is config.factoryLogin: a marker counts only in that
// login's comments, since anyone can comment on a public issue and would otherwise silence a
// narration by posting its marker first.
type Narrator struct {
	Forge commenter
	Login string
}

// Marker is the hidden line that makes a comment findable again (R22).
func Marker(scope, key string) string {
	return fmt.Sprintf("<!-- agent-factory scope=%s event=%s -->", scope, key)
}

// PostOnce posts e on issue or PR number unless one of its recent comments (the forge's last 50)
// by the factory already carries e's marker.
func (n Narrator) PostOnce(ctx context.Context, number int, scope string, e Event) error {
	if n.Login == "" {
		return errors.New("narrate: the factory's login is required to trust a marker")
	}
	marker := Marker(scope, e.Key)
	cs, err := n.Forge.RecentComments(ctx, number)
	if err != nil {
		return fmt.Errorf("narrate: %w", err)
	}
	for _, c := range cs {
		if strings.EqualFold(c.Author, n.Login) && strings.Contains(c.Body, marker) {
			return nil
		}
	}
	if err := n.Forge.Comment(ctx, number, e.Body+"\n\n"+marker); err != nil {
		return fmt.Errorf("narrate: %w", err)
	}
	return nil
}

// Post is PostOnce plus the task's own record, which spares the list call on a replay. The key is
// recorded only once the comment exists or is found; number 0 means nowhere to narrate (R28).
func (n Narrator) Post(ctx context.Context, t *v1alpha1.Task, number int, e Event) error {
	if number == 0 || slices.Contains(t.Status.Narrated, e.Key) {
		return nil
	}
	if err := n.PostOnce(ctx, number, t.Name, e); err != nil {
		return err
	}
	record(t, e.Key)
	return nil
}

// record appends key and trims the oldest keys past MaxNarrated. The list is a display record:
// a trimmed key can only cost a replay its shortcut, never a seq (roomSeq is the ledger).
func record(t *v1alpha1.Task, key string) {
	t.Status.Narrated = append(t.Status.Narrated, key)
	if over := len(t.Status.Narrated) - MaxNarrated; over > 0 {
		t.Status.Narrated = slices.Delete(t.Status.Narrated, 0, over)
	}
}

// roomPoster is the broker's system API as narration uses it (rooms.Client).
type roomPoster interface {
	TaskState(ctx context.Context, room, text string, clientSeq int64) error
}

// Room posts text in the task's room as a task_state message, once per key. The broker keeps one
// message per clientSeq, so the seq is the ledger (ruling SK): a new key takes status.roomSeq + 1,
// and persist writes that to the Task's status before the post. After a restart the key is found
// with its seq, and the replay sends the same seq, which the broker answers without storing.
//
// A refusal keeps its type: errors.Is(err, rooms.ErrNoRoom) is a retry, and
// errors.Is(err, rooms.ErrNotPermitted) is expected until FR-1 enables the factory's
// systemPrincipals entry. Either way the seq stays taken, so the retry reuses it.
func Room(ctx context.Context, r roomPoster, t *v1alpha1.Task, key, text string, persist func(context.Context) error) error {
	switch {
	case t.Status.RoomRef == "":
		return fmt.Errorf("narrate: task %s has no room yet", t.Name)
	case persist == nil:
		return errors.New("narrate: a room message needs its seq persisted first")
	}
	seq, ok := roomSeq(t, key)
	if !ok {
		prevSeq, prevNarrated := t.Status.RoomSeq, slices.Clone(t.Status.Narrated)
		seq = t.Status.RoomSeq + 1
		t.Status.RoomSeq = seq
		record(t, roomPrefix+strconv.FormatInt(seq, 10)+"/"+key)
		if err := persist(ctx); err != nil {
			t.Status.RoomSeq, t.Status.Narrated = prevSeq, prevNarrated
			return fmt.Errorf("narrate: persist room seq %d: %w", seq, err)
		}
	}
	if err := r.TaskState(ctx, t.Status.RoomRef, text, seq); err != nil {
		return fmt.Errorf("narrate: task_state %d in room %s: %w", seq, t.Status.RoomRef, err)
	}
	return nil
}

// roomSeq finds the seq a room message key was given.
func roomSeq(t *v1alpha1.Task, key string) (int64, bool) {
	for _, k := range t.Status.Narrated {
		rest, ok := strings.CutPrefix(k, roomPrefix)
		if !ok {
			continue
		}
		n, k, ok := strings.Cut(rest, "/")
		if seq, err := strconv.ParseInt(n, 10, 64); ok && err == nil && k == key {
			return seq, true
		}
	}
	return 0, false
}

// Tokens is a token count as people read it: 300 k, 1.5 M.
func Tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1f M", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%d k", n/1000)
	}
	return strconv.FormatInt(n, 10)
}

// reasons reads the end, stop and refusal reasons of the factory, SP1 and SP2 (P15) as prose.
func reasons() map[string]string {
	return map[string]string{
		"agent_finished":        "the agent finished its work",
		"agent_error":           "the agent stopped on an error",
		"agent_stuck":           "the agent reported that it was stuck",
		"deadline":              "the run hit its wall-clock limit",
		"pod_lost":              "the sandbox was lost (spot reclaim or eviction)",
		"revoked":               "the run was stopped by hand",
		"deleted":               "the run's claim was deleted",
		"budget-run":            "the run spent its token budget",
		"budget-principal":      "the factory's daily token budget is spent",
		"budget-fleet":          "the agent fleet's daily token budget is spent",
		"budget-task":           "the task spent its token budget",
		"run_lost":              "the run disappeared",
		"no_pr":                 "the agent opened no pull request",
		"merged":                "the pull request was merged",
		"pr_closed":             "the pull request was closed",
		"text_too_long":         "the issue text is longer than the factory accepts (14 KiB)",
		"daily_task_cap":        "the factory has reached its daily task cap",
		"kill_switch":           "the factory's kill switch is engaged",
		"stopped_by_label":      "a maintainer applied factory/stop",
		"stopped_by_annotation": "an operator stopped it",
		"superseded":            "a new factory/ready replaced it",
		"unauthorised_labeller": "only a maintainer's factory/ready starts a task",
		"edited_after_label":    "the issue was edited after it was labelled",
		"task_active":           "a task for this issue is still running",
		"unsanitisable":         "the issue text could not be made safe for an agent to read; simplify its markup",
		"unauthorised_stopper":  "only a maintainer's factory/stop stops a task",
		"foreign_room":          "a room of the task's name exists and is not the factory's",
	}
}

// Reason is r as prose; a reason without one is shown as it is.
func Reason(r string) string {
	if s, ok := reasons()[r]; ok {
		return s
	}
	return r
}

// Started announces a run with what a human needs: its id, branch, budget, where to watch it
// and how to stop it. roomsURL is config.roomsURL, which the config holds to https://<host>.
func Started(t *v1alpha1.Task, s runs.Spec, roomsURL string) Event {
	body := fmt.Sprintf("Agent factory task `%s` started run `%s` (%s) on branch `%s`.\n\n"+
		"- Budget: %s tokens, %d minutes (tier %s)\n"+
		"- Watch: %s/r/%s (tailnet only)\n"+
		"- Stop: apply the label `factory/stop`",
		t.Name, s.RunID, s.Role, s.Branch, Tokens(s.MaxTokens), s.MaxMinutes, t.Spec.Budget.Tier,
		strings.TrimSuffix(roomsURL, "/"), t.Status.RoomRef)
	return Event{Key: "run-" + s.RunID + "-started", Body: body}
}

// Revising says a maintainer's review sent the task back for another run (Δ5), once per round.
func Revising(t *v1alpha1.Task, reviewer string) Event {
	return Event{Key: fmt.Sprintf("revise-%d", len(t.Status.Runs)),
		Body: fmt.Sprintf("Agent factory task `%s` is revising after @%s's review: the next run starts on the same branch, "+
			"with the review in its brief.", t.Name, reviewer)}
}

// PROpened announces the task's pull request.
func PROpened(t *v1alpha1.Task, number int, url, runID string) Event {
	return Event{Key: "pr-opened", Body: fmt.Sprintf("Run `%s` of task `%s` opened #%d: %s", runID, t.Name, number, url)}
}

func headlines() map[string]string {
	return map[string]string{
		v1alpha1.PhaseDone: "is done", v1alpha1.PhaseNoOp: "made no change", v1alpha1.PhaseEscalated: "needs a maintainer",
		v1alpha1.PhaseRejected: "was not accepted", v1alpha1.PhaseClosed: "was closed", v1alpha1.PhaseStopped: "was stopped",
		v1alpha1.PhaseReverted: "was reverted",
	}
}

// hints: phase 2 replaces the Escalated one with /factory retry.
func hints() map[string]string {
	return map[string]string{
		v1alpha1.PhaseEscalated: "Re-apply `factory/ready` to start a new task, or push to the branch yourself.",
		v1alpha1.PhaseNoOp:      "If there is work to do, add detail to the issue and re-apply `factory/ready`.",
		v1alpha1.PhaseRejected:  "Fix what is described above and re-apply `factory/ready`.",
	}
}

// Ended announces the task's end, why, and what a human can do next. Its key carries the phase
// and the run count, so a task that ends, is retried and ends again narrates both.
func Ended(t *v1alpha1.Task, phase, reason string) Event {
	var b strings.Builder
	fmt.Fprintf(&b, "Agent factory task `%s` %s", t.Name, headlines()[phase])
	if why := Reason(reason); why != "" {
		fmt.Fprintf(&b, ": %s", why)
	}
	b.WriteString(".")
	if pr := t.Status.PullRequest; pr != nil && pr.MergedBy != "" && phase == v1alpha1.PhaseDone {
		fmt.Fprintf(&b, " Merged by @%s.", strings.TrimSuffix(pr.MergedBy, "[bot]"))
	}
	if h := hints()[phase]; h != "" {
		b.WriteString("\n\n" + h)
	}
	fmt.Fprintf(&b, "\n\nTokens used: %s.", Tokens(t.Status.Usage.Tokens))
	return Event{Key: fmt.Sprintf("end-%s-%d", strings.ToLower(phase), len(t.Status.Runs)), Body: b.String()}
}

// StopIgnored answers a factory/stop the poller will not act on, once per label.
func StopIgnored(number int, reason string, at time.Time) Event {
	return Event{Key: fmt.Sprintf("stop-ignored-%s-%d", reason, at.Unix()),
		Body: fmt.Sprintf("The agent factory ignored `factory/stop` on #%d: %s.", number, Reason(reason))}
}

// Refused answers a factory/ready the poller will not act on; keyed by the label's time, so each
// label is answered once.
func Refused(number int, reason string, at time.Time) Event {
	return Event{Key: fmt.Sprintf("refused-%s-%d", reason, at.Unix()),
		Body: fmt.Sprintf("The agent factory did not start a task for #%d: %s.", number, Reason(reason))}
}
