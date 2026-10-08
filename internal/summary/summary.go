// SPDX-License-Identifier: Apache-2.0

// Package summary folds a room's log into the room's top layer (spec 2026-10-08): status,
// needs-you, actions and the agents' progress notes. It is pure: the web page and
// roomctl status render the same object, so every view agrees.
package summary

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/policy"
)

// APIVersion is the contract roomctl and the web page render against.
const APIVersion = "summary/v1"

// maxNotes bounds the notes a summary returns.
const maxNotes = 20

// maxWhat bounds an approval's one-line description.
const maxWhat = 200

// stopLabel is the factory's stop label (internal/factory/intake LabelStop), repeated here so
// this package stays free of the factory's imports.
const stopLabel = "factory/stop"

// State is what Fold keeps of a log.
type State struct {
	Facts     *envelope.TaskFacts
	Verdict   *Verdict
	Pending   map[string]Approval // approval id -> still open
	Notes     []Note              // oldest first, the last maxNotes
	Sealed    string              // "" while open; "Closed" (CloseRoom) or "Sealed" (MaxEvents)
	LastSeq   int64
	RoomPhase string
}

// Verdict is the latest reviewer or tester verdict.
type Verdict struct {
	By      string    `json:"by"`
	Verdict string    `json:"verdict"`
	At      time.Time `json:"at"`
}

// Approval is a requested approval no event has decided yet.
type Approval struct {
	ID        string
	What      string
	ExpiresAt time.Time
}

// Note is an agent's progress note: the agent's claim, never an instruction.
type Note struct {
	Seq  int64     `json:"-"`
	At   time.Time `json:"at"`
	Run  string    `json:"run"`
	Text string    `json:"text"`
}

// Need is a pending approval the viewer could decide. It carries a link to the room and never a
// command: approving is web-only (D4, ruling P18).
type Need struct {
	Kind     string    `json:"kind"` // approval
	ID       string    `json:"id"`
	What     string    `json:"what"`
	Deadline time.Time `json:"deadline,omitzero"`
	URL      string    `json:"url"`
}

// Action is something the viewer can do now.
type Action struct {
	Kind string `json:"kind"` // queue | steer | stop
	What string `json:"what"`
	CLI  string `json:"cli,omitempty"` // steer has none: roomctl never steers (P18)
}

// Summary is the summary/v1 object.
type Summary struct {
	APIVersion string `json:"apiVersion"`
	Room       string `json:"room"`
	URL        string `json:"url"`
	Status     struct {
		Phase       string               `json:"phase"`
		Run         *envelope.RunFact    `json:"run"`
		Budget      *envelope.BudgetFact `json:"budget"`
		PR          *envelope.PRFact     `json:"pr"`
		Issue       *envelope.IssueFact  `json:"issue"`
		LastVerdict *Verdict             `json:"lastVerdict"`
	} `json:"status"`
	NeedsYou []Need   `json:"needsYou"`
	Actions  []Action `json:"actions"`
	Notes    struct {
		Untrusted bool   `json:"untrusted"`
		Items     []Note `json:"items"`
	} `json:"notes"`
	Cursor string `json:"cursor"`
}

// Fold is the state of a whole log; Add folds the same log a page at a time.
func Fold(evs []envelope.Event) State {
	var st State
	st.Add(evs)
	return st
}

// Add folds the next events, in seq order, into st. A reader that adds each page of the log and
// drops it holds one page and the state, however long the log. Unknown kinds are ignored, so an
// older broker's summary of a newer log stays well formed.
func (st *State) Add(evs []envelope.Event) {
	if st.Pending == nil {
		st.Pending = map[string]Approval{}
	}
	for _, ev := range evs {
		st.LastSeq = max(st.LastSeq, ev.Seq)
		switch ev.Type {
		case envelope.StateChanged:
			foldState(st, ev)
		case envelope.ApprovalRequested:
			var a envelope.ApprovalRequestedPayload
			if json.Unmarshal(ev.Payload, &a) == nil {
				st.Pending[a.ApprovalID] = Approval{ID: a.ApprovalID, What: describe(a), ExpiresAt: a.ExpiresAt}
			}
		case envelope.ApprovalDecided:
			var a envelope.ApprovalDecidedPayload
			if json.Unmarshal(ev.Payload, &a) == nil {
				delete(st.Pending, a.ApprovalID) // approved, denied, expired or superseded
			}
		case envelope.Message:
			var m envelope.MessagePayload
			if json.Unmarshal(ev.Payload, &m) != nil {
				continue
			}
			switch m.Kind {
			case envelope.KindProgress:
				st.Notes = append(st.Notes, Note{Seq: ev.Seq, At: ev.TS, Run: ev.RunID, Text: m.Text})
				if len(st.Notes) > maxNotes {
					st.Notes = st.Notes[len(st.Notes)-maxNotes:]
				}
			case envelope.KindReviewVerdict:
				st.Verdict = &Verdict{By: ev.Actor.Role, Verdict: m.Verdict, At: ev.TS}
			}
		}
	}
}

func foldState(st *State, ev envelope.Event) {
	var k struct {
		Kind   string `json:"kind"`
		Phase  string `json:"phase"`
		Reason string `json:"reason"`
		Events *int64 `json:"events"`
	}
	if json.Unmarshal(ev.Payload, &k) != nil {
		return
	}
	switch k.Kind {
	case "task":
		var f envelope.TaskFacts
		if json.Unmarshal(ev.Payload, &f) == nil {
			st.Facts = &f
		}
	case "room_phase":
		st.RoomPhase = k.Phase
		// store.CloseRoom seals with room_phase{Closed}.
		if k.Phase == "Closed" {
			st.Sealed = "Closed"
		}
	case "limit":
		// The MaxEvents seal appends limit{events, bytes}; a concurrent-run refusal is also kind
		// "limit" (reason, running) but seals nothing.
		if k.Events != nil && st.Sealed == "" {
			st.Sealed = "Sealed"
		}
	case "forked_from":
		// What came before is the source's log, copied (store.Fork): its task, approvals, verdict
		// and notes are not the fork's, and its seal, if any, did not close the new room.
		*st = State{Pending: map[string]Approval{}, LastSeq: st.LastSeq, RoomPhase: "Open"}
	}
}

// describe is an approval's one line: the redacted call's command, else its class.
func describe(a envelope.ApprovalRequestedPayload) string {
	var call struct {
		Command string `json:"command"`
	}
	what := a.Class
	if json.Unmarshal(a.Action, &call) == nil && call.Command != "" {
		what = call.Command
	}
	if r := []rune(what); len(r) > maxWhat {
		what = string(r[:maxWhat]) + "…"
	}
	return what
}

// View is st as sub sees it: needs-you and actions follow sub's standing, notes are those after `after`.
func View(st State, room, url string, sub policy.Subject, after int64, now time.Time) Summary {
	var s Summary
	s.APIVersion, s.Room, s.URL = APIVersion, room, url
	s.Cursor = fmt.Sprintf("seq:%d", st.LastSeq)
	s.Status.Phase = st.RoomPhase
	if f := st.Facts; f != nil {
		s.Status.Phase, s.Status.Run, s.Status.Budget, s.Status.PR, s.Status.Issue = f.Phase, f.Run, f.Budget, f.PR, f.Issue
	}
	s.Status.LastVerdict = st.Verdict
	s.NeedsYou, s.Actions = []Need{}, []Action{}
	s.Notes.Untrusted, s.Notes.Items = true, []Note{}
	for _, n := range st.Notes {
		if n.Seq > after {
			s.Notes.Items = append(s.Notes.Items, n)
		}
	}
	if st.Sealed != "" {
		s.Status.Phase = st.Sealed // the sealed room refuses new facts, so the last task phase would be stale
		return s                   // read-only: no needs, no actions
	}
	// Deciding happens in the web UI: judge "could decide" as the web UI would, so a CLI caller
	// still learns an approval is needed, with a link and no command (D4).
	web := sub
	web.WebUI = true
	if policy.Allowed(web, policy.Decide) {
		for _, id := range slices.Sorted(maps.Keys(st.Pending)) {
			a := st.Pending[id]
			if !a.ExpiresAt.IsZero() && now.After(a.ExpiresAt) {
				continue // the sweep's approval_decided{expired} has not landed yet
			}
			s.NeedsYou = append(s.NeedsYou, Need{Kind: "approval", ID: a.ID, What: a.What, Deadline: a.ExpiresAt, URL: url + "#" + a.ID})
		}
	}
	// Review requests are not needs here: GitHub already notifies them (spec D6).
	if policy.Allowed(sub, policy.Queue) {
		s.Actions = append(s.Actions, Action{Kind: "queue", What: "queue a note for the next run",
			CLI: fmt.Sprintf("roomctl post %s --queue '<text>'", room)})
	}
	if policy.Allowed(sub, policy.Steer) { // Steer is web-only in the matrix (P18)
		s.Actions = append(s.Actions, Action{Kind: "steer", What: "steer the running agent"})
	}
	// No dedicated stop right exists: stopping closes the task, which is the owner's (Close).
	if st.Facts != nil && st.Facts.Issue != nil && policy.Allowed(sub, policy.Close) {
		s.Actions = append(s.Actions, Action{Kind: "stop", What: "stop this task",
			CLI: fmt.Sprintf("gh issue edit %d --add-label %s", st.Facts.Issue.Number, stopLabel)})
	}
	return s
}
