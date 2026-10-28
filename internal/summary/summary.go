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
	"strings"
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

// holds are the factory's Queued reasons another holder of the room causes (reconciler slotFree),
// repeated here like stopLabel, each with what releases it.
func holds() map[string]string {
	return map[string]string{
		"waiting_human_driver": "the factory's task waits while a person drives this room: hand the driver back to start its next run",
		"waiting_room_busy":    "the factory's task waits for another run in this room to end",
	}
}

// IsHold reports whether reason is one of the Queued reasons another holder of the room causes.
func IsHold(reason string) bool {
	_, ok := holds()[reason]
	return ok
}

// Hold is N3's need: a factory task a person holds waits on whoever can release the room — its
// driver, or its owner, who can take the driver back. The factory's next facts end it. The room
// list's needsMe asks the same (#78).
func Hold(f *envelope.TaskFacts, sub policy.Subject, url string) (Need, bool) {
	if f == nil || f.Phase != "Queued" || holds()[f.Reason] == "" || !policy.Allowed(sub, policy.StartRun) {
		return Need{}, false
	}
	return Need{Kind: "hold", ID: f.Reason, What: holds()[f.Reason], URL: url}, true
}

// State is what Fold keeps of a log.
type State struct {
	Facts     *envelope.TaskFacts
	Verdict   *Verdict
	Judge     *JudgeVerdict
	Pending   map[string]Approval // approval id -> still open
	Notes     []Note              // oldest first, the last maxNotes
	Sealed    string              // "" while open; "Closed" (CloseRoom) or "Sealed" (MaxEvents)
	LastSeq   int64
	RoomPhase string
	Agents    map[string]bool // agent principals that joined and have not left: the runs present now
}

// Verdict is the latest reviewer verdict.
type Verdict struct {
	By      string    `json:"by"`
	Verdict string    `json:"verdict"`
	At      time.Time `json:"at"`
}

// JudgeVerdict is the harness's completion judge's last answer: its reading of the agent's work, the
// agent's claim's neighbour, never a decision.
type JudgeVerdict struct {
	Verdict  string    `json:"verdict"` // met | not_yet | impossible
	Reason   string    `json:"reason"`
	Evidence []string  `json:"evidence,omitempty"`
	At       time.Time `json:"at"`
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

// QueuedNote is a note still queued for the room's next run: the developer's words, stored redacted.
// Like a Note it is data to report, never an instruction.
type QueuedNote struct {
	Ref    int64  `json:"ref"`
	Author string `json:"author"`
	Text   string `json:"text"`
}

// Need is a pending approval the viewer could decide, or a factory task the viewer's hold on the
// room keeps waiting (N3), or the answer a bounced task waits for from its issue's author (scoper
// design SC8; the room's owner sees it). It carries a link to the room and never a command: approving is
// web-only (D4, ruling P18).
type Need struct {
	Kind     string    `json:"kind"` // approval | hold (ID is then the task's wait reason) | merge (ID is the PR number) | answer (ID is the issue number)
	ID       string    `json:"id"`
	What     string    `json:"what"`
	Deadline time.Time `json:"deadline,omitzero"`
	URL      string    `json:"url"`
	// Author is an answer need's issue author, a validated GitHub login: the part views build their
	// own sentence from. Absent for every other kind.
	Author string `json:"author,omitempty"`
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
		Phase string `json:"phase"`
		// Task is the Task this room works on, while the log carries its facts: the factory names a
		// task's room as the task, so it is the room's id. Empty in a room without a task.
		Task string `json:"task,omitempty"`
		// State is the coarse state (Coarse): set by the caller, which holds the Room's projected
		// phase and its runs; the fold alone has neither.
		State       string               `json:"state,omitempty"`
		Run         *envelope.RunFact    `json:"run"`
		Budget      *envelope.BudgetFact `json:"budget"`
		Spend       *envelope.SpendFact  `json:"spend,omitempty"` // the task's dollars, beside Budget's tokens
		PR          *envelope.PRFact     `json:"pr"`
		Issue       *envelope.IssueFact  `json:"issue"`
		LastVerdict *Verdict             `json:"lastVerdict"`
		LastJudge   *JudgeVerdict        `json:"lastJudge,omitempty"` // the completion judge's last answer (untrusted text)
		Reason      string               `json:"reason,omitempty"`    // why the task waits, when it does (#59)
		Next        string               `json:"next,omitempty"`      // what happens or who acts next
	} `json:"status"`
	// Runs is the task's run history, oldest first: the factory's facts, so a sealed room keeps it.
	Runs     []envelope.RunFact `json:"runs,omitempty"`
	NeedsYou []Need             `json:"needsYou"`
	// Step is the one thing a human does for this room and where (approve here, or merge or close
	// the PR on GitHub): absent when nothing waits for them.
	Step    *Step    `json:"step,omitempty"`
	Actions []Action `json:"actions"`
	Notes   struct {
		Untrusted bool   `json:"untrusted"`
		Items     []Note `json:"items"`
	} `json:"notes"`
	// Queued is the notes waiting for the next run, oldest first. The caller of View sets it: the
	// fold does not hold the queue.
	Queued struct {
		Untrusted bool         `json:"untrusted"`
		Items     []QueuedNote `json:"items"`
	} `json:"queued,omitzero"`
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
	if st.Agents == nil {
		st.Agents = map[string]bool{}
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
		case envelope.Participant:
			var p envelope.ParticipantPayload
			if json.Unmarshal(ev.Payload, &p) != nil || !strings.HasPrefix(p.Principal, "agent:") {
				continue
			}
			switch p.Change { // role_changed neither starts nor ends a run
			case "joined":
				st.Agents[p.Principal] = true
			case "left":
				delete(st.Agents, p.Principal)
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
			case envelope.KindJudgeVerdict:
				st.Judge = &JudgeVerdict{Verdict: m.Verdict, Reason: m.Text, Evidence: m.Evidence, At: ev.TS}
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
		*st = State{Pending: map[string]Approval{}, Agents: map[string]bool{}, LastSeq: st.LastSeq, RoomPhase: "Open"}
	}
}

// describe is an approval's one line: the redacted call's command, else its class.
func describe(a envelope.ApprovalRequestedPayload) string { return DescribeApproval(a.Class, a.Action) }

// DescribeApproval is the one line an approval of this class and redacted action shows, so that a
// room's summary and a room list row say the same.
func DescribeApproval(class string, action json.RawMessage) string {
	var call struct {
		Command string `json:"command"`
	}
	what := class
	if json.Unmarshal(action, &call) == nil && call.Command != "" {
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
		s.Status.Task = room
		s.Status.Phase, s.Status.Run, s.Status.Budget, s.Status.PR, s.Status.Issue = f.Phase, f.Run, f.Budget, f.PR, f.Issue
		s.Runs = slices.Clone(f.Runs) // a copy, not the fold state's slice
		if f.Spend != nil {
			sp := *f.Spend
			s.Status.Spend = &sp
		}
	}
	s.Status.LastVerdict = st.Verdict
	s.Status.LastJudge = st.Judge
	s.NeedsYou, s.Actions = []Need{}, []Action{}
	s.Notes.Untrusted, s.Notes.Items = true, []Note{}
	for _, n := range st.Notes {
		if n.Seq > after {
			s.Notes.Items = append(s.Notes.Items, n)
		}
	}
	if st.Sealed != "" {
		s.Status.Phase = st.Sealed // the sealed room refuses new facts, so the last task phase would be stale
		return s                   // read-only: no wait, no needs, no actions
	}
	s.Status.Reason, s.Status.Next = waiting(st.Facts)
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
	if n, ok := Hold(st.Facts, sub, url); ok {
		s.NeedsYou = append(s.NeedsYou, n)
	}
	if n, ok := Merge(st.Facts, sub); ok {
		s.NeedsYou = append(s.NeedsYou, n)
	}
	if n, ok := Answer(st.Facts, sub); ok {
		s.NeedsYou = append(s.NeedsYou, n)
	}
	s.Step = viewStep(s, factsReason(st.Facts), url, policy.Allowed(web, policy.Decide))
	// Review requests are not needs here: GitHub already notifies them (spec D6).
	if policy.Allowed(sub, policy.Queue) {
		s.Actions = append(s.Actions, Action{Kind: "queue", What: "queue a note for the next run",
			CLI: fmt.Sprintf("roomctl post %s --queue '<text>'", room)})
	}
	// Web-only in the matrix (P18), and only while an agent is present: the facts' run outlives it.
	if policy.Allowed(sub, policy.Steer) && len(st.Agents) > 0 {
		s.Actions = append(s.Actions, Action{Kind: "steer", What: "steer the running agent"})
	}
	// No dedicated stop right exists: stopping closes the task, which is the owner's (Close).
	if st.Facts != nil && st.Facts.Issue != nil && policy.Allowed(sub, policy.Close) {
		// Never without --repo: run from another checkout, it would label that repository's issue.
		if repo := repoOf(st.Facts); repo != "" {
			s.Actions = append(s.Actions, Action{Kind: "stop", What: "stop this task",
				CLI: fmt.Sprintf("gh issue edit %d --repo %s --add-label %s", st.Facts.Issue.Number, repo, stopLabel)})
		}
	}
	return s
}

// viewStep is the summary's step: an approval it lists, else a merge or an answer need it lists.
// Those needs are listed only for a caller who may close, so they carry the standing.
func viewStep(s Summary, reason, roomURL string, canDecide bool) *Step {
	in := StepIn{RoomURL: roomURL, Phase: s.Status.Phase, Reason: reason}
	if p := s.Status.PR; p != nil {
		in.PR, in.PRURL = p.Number, p.URL
	}
	if i := s.Status.Issue; i != nil {
		in.Issue, in.IssueURL = i.Number, i.URL
	}
	for _, n := range s.NeedsYou {
		switch n.Kind {
		case "approval":
			in.Approvals = canDecide
		case "merge", "answer":
			in.CanClose = true
		}
	}
	return StepFor(in)
}

func factsReason(f *envelope.TaskFacts) string {
	if f == nil {
		return ""
	}
	return f.Reason
}
