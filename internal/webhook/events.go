// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/summary"
)

// types are R19's eleven events, in the design's order.
var types = []string{"room.task_received", "room.task_triaged", "room.run_started", "room.run_ended",
	"room.waiting_for_human", "room.pr_opened", "room.verdict_posted", "room.merged", "room.reverted",
	"room.budget_reached", "room.ended"}

// Types lists the events a subscriber may ask for.
func Types() []string { return slices.Clone(types) }

// limits are the budget limits a run or a task can reach: the meter's, the person's and the
// family's. A revoked run's reason is annotation text, so a "budget-" prefix alone proves nothing.
var limits = []string{"budget-run", "budget-task", "budget-principal", "budget-fleet", "budget-person", "budget-family"}

// Limits lists the values an event's limit may hold.
func Limits() []string { return slices.Clone(limits) }

// States lists the values an event's state may hold: the six coarse states.
func States() []string {
	return []string{summary.StateQueued, summary.StateRunning, summary.StateWaiting, summary.StateDone,
		summary.StateFailed, summary.StateStopped}
}

// Reasons lists the values an event's reason may hold: the wait reasons the summary has prose
// for, the run ends that escalate a task, the broker's approval, and the three escalations the
// factory raises on a room it cannot use. They repeat the factory's strings: this package
// imports nothing of the factory's reconciler, so a new factory reason needs a line here.
func Reasons() []string {
	out := summary.Reasons()
	out = append(out, "approval", "manual", "revoked", "deleted", "principal_unknown",
		"budget-run", "budget-principal", "budget-fleet", "budget-person",
		"foreign_room", "room_log_unreadable", "rooms_crd_outdated")
	slices.Sort(out)
	return slices.Compact(out)
}

// Clip is v when kind's vocabulary ("reason", "limit" or "state") holds it, "" for "", else
// "other": no value outside a closed list leaves the broker or reaches a Slack thread.
func Clip(kind, v string) string {
	var list []string
	switch kind {
	case "reason":
		list = Reasons()
	case "limit":
		list = limits
	case "state":
		list = States()
	}
	if v == "" || slices.Contains(list, v) {
		return v
	}
	return "other"
}

// Tracker turns one room's log into one subscriber's events (R19). It is per subscriber and
// room (M1): what it has sent is that subscriber's, so two subscribers at different cursors never
// share it. The emitter warms it by replaying the room up to the subscriber's cursor.
type Tracker struct {
	seq    int64  // the last seq read: an entry at or below it is a replay and means nothing
	phase  string // the last task phase seen: an event fires on a change only (M2)
	pr     int    // the last pull request number seen, for merged, reverted and verdict_posted
	prSent bool
	ended  bool
	origin string // the last origin the task facts named: every event after it repeats it (RA10)
}

// Events is what ev means to a subscriber of room, whose repository is repo: none, one or a few
// events, each carrying ids, states and codes only. A message is never decoded (M3).
func (t *Tracker) Events(room, repo string, ev envelope.Event) []Event {
	out := t.events(room, repo, ev)
	for i := range out {
		out[i].Origin = t.origin
	}
	return out
}

func (t *Tracker) events(room, repo string, ev envelope.Event) []Event {
	if ev.Seq <= t.seq {
		return nil
	}
	t.seq = ev.Seq
	if ev.Type == envelope.ApprovalRequested {
		return []Event{{Type: "room.waiting_for_human", Room: room, Reason: "approval"}}
	}
	if ev.Type != envelope.StateChanged {
		return nil
	}
	var k struct {
		Kind   string `json:"kind"`
		Phase  string `json:"phase"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal(ev.Payload, &k) != nil {
		return nil
	}
	switch k.Kind {
	case "task":
		var f envelope.TaskFacts
		if json.Unmarshal(ev.Payload, &f) != nil {
			return nil
		}
		return t.task(room, repo, f)
	case "run_phase":
		return runEvents(room, ev.RunID, k.Phase, k.Reason)
	case "verdict_posted":
		return []Event{{Type: "room.verdict_posted", Room: room, PR: t.pr}}
	}
	return nil
}

// task is a task_state entry's events: the first pull request, then the phase's own event on a
// change of phase, the budget its reason names, and the first coarse end.
func (t *Tracker) task(room, repo string, f envelope.TaskFacts) []Event {
	if f.Origin != "" {
		t.origin = f.Origin
	}
	var out []Event
	if f.PR != nil && f.PR.Number > 0 {
		t.pr = f.PR.Number
		if !t.prSent {
			t.prSent = true
			out = append(out, Event{Type: "room.pr_opened", Room: room, PR: t.pr})
		}
	}
	if f.Phase == t.phase {
		return out // the factory rewrites a phase's facts (budget, run, PR): no new event
	}
	t.phase = f.Phase
	switch f.Phase {
	case "Received":
		out = append(out, Event{Type: "room.task_received", Room: room, Repo: repo})
	case "Triaged":
		out = append(out, Event{Type: "room.task_triaged", Room: room, Repo: repo})
	case "AwaitingHuman", "Escalated":
		out = append(out, Event{Type: "room.waiting_for_human", Room: room, Reason: Clip("reason", f.Reason)})
	case "Merged":
		out = append(out, Event{Type: "room.merged", Room: room, PR: t.pr})
	case "Reverted":
		out = append(out, Event{Type: "room.reverted", Room: room, PR: t.pr})
	}
	if strings.HasPrefix(f.Reason, "budget-") {
		out = append(out, Event{Type: "room.budget_reached", Room: room, Limit: Clip("limit", f.Reason)})
	}
	s := summary.Coarse(f.Phase, "", false)
	if !t.ended && (s == summary.StateDone || s == summary.StateFailed || s == summary.StateStopped) {
		t.ended = true
		out = append(out, Event{Type: "room.ended", Room: room, State: s})
	}
	return out
}

// runEvents is a run_phase entry's events: its start, or its end and the budget it reached.
// The terminal phases repeat runwatch.Terminal's: this package stays free of the k8s imports.
func runEvents(room, run, phase, reason string) []Event {
	switch phase {
	case "Running":
		return []Event{{Type: "room.run_started", Room: room, Run: run}}
	case "Succeeded", "Failed", "BudgetExhausted", "Revoked":
		out := []Event{{Type: "room.run_ended", Room: room, Run: run}}
		if strings.HasPrefix(reason, "budget-") {
			out = append(out, Event{Type: "room.budget_reached", Room: room, Limit: Clip("limit", reason)})
		}
		return out
	}
	return nil
}
