// SPDX-License-Identifier: Apache-2.0

package roomview

import (
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/summary"
)

// defaultChangesLimit is how many milestones a page shows: view 4 is 30 lines, then "N more".
const defaultChangesLimit = 30

// ChangeSet is view 4, what changed since a cursor, and room_changes' structuredContent. Untrusted
// is always true: Text of a note, handoff or approval is an agent's.
type ChangeSet struct {
	Room      string      `json:"room"`
	URL       string      `json:"url"`
	Since     time.Time   `json:"since"`
	Items     []Milestone `json:"items"`
	More      int         `json:"more"`
	Cursor    string      `json:"cursor"`
	Untrusted bool        `json:"untrusted"`
}

// Milestone is one thing that happened. For a note, a handoff (Text is its summary) and an
// approval_requested (Text is what it asks, Until when it expires) Text is the agent's; for every
// other kind it is a sentence the broker wrote, with markup of its own around run ids.
type Milestone struct {
	Seq    int64     `json:"seq"`
	At     time.Time `json:"at"`
	Until  time.Time `json:"until,omitzero"`
	Kind   string    `json:"kind"`
	Run    string    `json:"run,omitempty"`
	Role   string    `json:"role,omitempty"`
	Text   string    `json:"text,omitempty"`
	URL    string    `json:"url,omitempty"`
	Turns  int       `json:"turns,omitempty"`
	Tools  int       `json:"tools,omitempty"`
	Pushes int       `json:"pushes,omitempty"`
}

// The kinds of milestone.
const (
	kindRunStarted        = "run_started"
	kindRunEnded          = "run_ended"
	kindActivity          = "activity"
	kindHandoff           = "handoff"
	kindNote              = "note"
	kindApprovalRequested = "approval_requested"
	kindApprovalDecided   = "approval_decided"
	kindVerdict           = "verdict"
	kindPROpened          = "pr_opened"
	kindMerged            = "merged"
	kindReverted          = "reverted"
	kindHeld              = "held"
	kindResumed           = "resumed"
	kindBudgetReached     = "budget_reached"
	kindEnded             = "ended"
)

// MilestoneInput is a page of a room's log and the task facts as they stood before it. A nil Base
// means the page's first facts are the first this view knows: whatever they hold predates the
// window, so it is not reported.
type MilestoneInput struct {
	Room, URL string
	Base      *envelope.TaskFacts
	Events    []envelope.Event
	Limit     int
}

// roleName is a role that is plain enough to sit in a sentence of the broker's.
var roleName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func safeRole(r string) string {
	if roleName.MatchString(r) {
		return r
	}
	return "agent"
}

// idSpan is a run id in a code span.
func idSpan(id string) string { return code(id, false) }

// prRef is "#N" for the pull request or issue URL u, "" when u is not one.
func prRef(u string) string {
	if !envelope.GitHubURL(u) {
		return ""
	}
	return "#" + u[strings.LastIndex(u, "/")+1:]
}

type act struct {
	run, role            string
	first, last          time.Time
	seq                  int64
	turns, tools, pushes int
}

// Milestones is the milestones of in, in the order they happened, at most in.Limit (30 when it is
// 0). Task facts are diffed in order against Base, then against each other.
func Milestones(in MilestoneInput) ChangeSet {
	limit := in.Limit
	if limit <= 0 {
		limit = defaultChangesLimit
	}
	c := ChangeSet{Room: in.Room, URL: in.URL, Items: []Milestone{}, Untrusted: true}
	var ms []Milestone
	prev, known := in.Base, in.Base != nil
	acts := map[string]*act{}
	var order []string
	for _, e := range in.Events {
		if c.Since.IsZero() {
			c.Since = e.TS
		}
		switch e.Type {
		case envelope.StateChanged:
			var k struct {
				Kind   string `json:"kind"`
				Phase  string `json:"phase"`
				Events *int64 `json:"events"`
			}
			if json.Unmarshal(e.Payload, &k) != nil {
				continue
			}
			switch {
			case k.Kind == "task":
				var f envelope.TaskFacts
				if json.Unmarshal(e.Payload, &f) != nil {
					continue
				}
				if known {
					ms = append(ms, diffFacts(prev, &f, e)...)
				}
				prev, known = &f, true
			case k.Kind == "room_phase" && k.Phase == "Closed":
				ms = append(ms, Milestone{Seq: e.Seq, At: e.TS, Kind: kindEnded, Text: "the room was closed"})
			case k.Kind == "limit" && k.Events != nil:
				ms = append(ms, Milestone{Seq: e.Seq, At: e.TS, Kind: kindEnded, Text: "the room is full: its event limit was reached"})
			}
		case envelope.Message:
			var p envelope.MessagePayload
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			switch p.Kind {
			case envelope.KindProgress:
				ms = append(ms, Milestone{Seq: e.Seq, At: e.TS, Kind: kindNote, Run: e.RunID, Role: clean(e.Actor.Role), Text: clean(p.Text)})
			case envelope.KindReviewVerdict:
				m := Milestone{Seq: e.Seq, At: e.TS, Kind: kindVerdict, Run: e.RunID, Role: clean(e.Actor.Role), Text: inertWord(p.Verdict, verdictWords(p.Verdict))}
				if envelope.GitHubURL(p.PullRequest) {
					m.URL = p.PullRequest
				}
				ms = append(ms, m)
			}
		case envelope.ApprovalRequested:
			var p envelope.ApprovalRequestedPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			m := Milestone{Seq: e.Seq, At: e.TS, Until: p.ExpiresAt, Kind: kindApprovalRequested, Run: e.RunID,
				Text: summary.DescribeApproval(p.Class, p.Action)}
			if in.URL != "" && p.ApprovalID != "" {
				m.URL = in.URL + "#" + p.ApprovalID
			}
			ms = append(ms, m)
		case envelope.ApprovalDecided:
			var p envelope.ApprovalDecidedPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			ms = append(ms, Milestone{Seq: e.Seq, At: e.TS, Kind: kindApprovalDecided, Text: inertWord(p.Decision, word(map[string]string{
				"approved": "approved", "denied": "denied", "expired": "expired"}, p.Decision))})
		case envelope.Handoff:
			var p envelope.HandoffPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			ms = append(ms, Milestone{Seq: e.Seq, At: e.TS, Kind: kindHandoff, Run: e.RunID,
				Role: clean(p.FromRole) + " → " + clean(p.ToRole), Text: clean(p.Summary)})
		case envelope.Turn, envelope.ToolCall:
			a := acts[e.RunID]
			if a == nil {
				a = &act{run: e.RunID, role: clean(e.Actor.Role), first: e.TS}
				acts[e.RunID] = a
				order = append(order, e.RunID)
			}
			switch e.Type {
			case envelope.Turn:
				var p envelope.TurnPayload
				if json.Unmarshal(e.Payload, &p) == nil && p.Phase == "completed" {
					a.turns++
				}
			default:
				var p envelope.ToolCallPayload
				if json.Unmarshal(e.Payload, &p) == nil {
					a.tools++
					if p.Class == "forge.push" {
						a.pushes++
					}
				}
			}
			a.last, a.seq = e.TS, e.Seq
		}
		c.Cursor = "seq:" + strconv.FormatInt(e.Seq, 10)
	}
	for _, id := range order {
		if a := acts[id]; a.turns+a.tools > 0 {
			ms = append(ms, Milestone{Seq: a.seq, At: a.first, Until: a.last, Kind: kindActivity, Run: a.run, Role: a.role,
				Turns: a.turns, Tools: a.tools, Pushes: a.pushes})
		}
	}
	slices.SortStableFunc(ms, func(a, b Milestone) int {
		switch {
		case a.Seq < b.Seq:
			return -1
		case a.Seq > b.Seq:
			return 1
		}
		return 0
	})
	if len(ms) > limit {
		c.More = len(ms) - limit
		ms = ms[:limit]
		c.Cursor = "seq:" + strconv.FormatInt(ms[limit-1].Seq, 10)
	}
	if len(ms) > 0 {
		c.Items = ms
	}
	return c
}

// held is whether f is a task another holder of the room keeps waiting.
func held(f *envelope.TaskFacts) bool { return summary.IsHold(f.Reason) }

// budgetReached is whether f's dollars, else its tokens, have hit their limit.
func budgetReached(f *envelope.TaskFacts) (string, bool) {
	if s := f.Spend; s != nil && s.LimitMicroUSD > 0 {
		return dollars(s.UsedMicroUSD) + " of " + dollars(s.LimitMicroUSD), s.UsedMicroUSD >= s.LimitMicroUSD
	}
	if b := f.Budget; b != nil && b.LimitTokens > 0 {
		return tokens(b.UsedTokens) + " of " + tokens(b.LimitTokens) + " tokens", b.UsedTokens >= b.LimitTokens
	}
	return "", false
}

// diffFacts is what changed from prev to cur, both read at e.
func diffFacts(prev, cur *envelope.TaskFacts, e envelope.Event) []Milestone {
	var ms []Milestone
	add := func(m Milestone) {
		m.Seq = e.Seq
		if m.At.IsZero() {
			m.At = e.TS
		}
		ms = append(ms, m)
	}
	var prURL string
	subject := "the task"
	if cur.PR != nil {
		prURL, subject = cur.PR.URL, "#"+strconv.Itoa(cur.PR.Number)
	}
	if prev.PR == nil && cur.PR != nil {
		add(Milestone{Kind: kindPROpened, Text: "pull request #" + strconv.Itoa(cur.PR.Number) + " opened", URL: cur.PR.URL})
	}

	before := map[string]envelope.RunFact{}
	for _, r := range prev.Runs {
		before[r.ID] = r
	}
	end := func(r envelope.RunFact) {
		text := "run " + idSpan(r.ID) + " " + inertWord(r.Outcome, outcomeWords(r.Outcome))
		if r.Outcome == envelope.OutcomeLost {
			text = reasonWords("pod_lost")
		}
		add(Milestone{At: r.EndedAt, Kind: kindRunEnded, Run: r.ID, Role: safeRole(r.Role), Text: text})
	}
	for _, r := range cur.Runs {
		if old, ok := before[r.ID]; ok && old.Outcome == "" && r.Outcome != "" {
			end(r)
		}
	}
	resumes := 0
	for _, r := range cur.Runs {
		if r.Trigger == "resume" {
			resumes++
		}
		if _, ok := before[r.ID]; ok {
			continue
		}
		text := "the factory started " + safeRole(r.Role) + " " + idSpan(r.ID)
		if r.Trigger != "" {
			how := inertWord(r.Trigger, triggerWords(r.Trigger))
			if r.Trigger == "resume" {
				how += " " + strconv.Itoa(resumes)
			}
			text += " (" + how + ")"
		}
		m := Milestone{At: r.StartedAt, Kind: kindRunStarted, Run: r.ID, Role: safeRole(r.Role)}
		if r.Trigger == "human" && cur.PR != nil {
			text, m.URL = "changes were requested on "+subject+" → "+text, prURL
		}
		m.Text = text
		add(m)
		if r.Outcome != "" {
			end(r)
		}
	}

	switch {
	case prev.Phase != cur.Phase && cur.Phase == "Merged":
		add(Milestone{Kind: kindMerged, Text: subject + " " + inertWord(cur.Phase, phaseWords(cur.Phase)), URL: prURL})
	case prev.Phase != cur.Phase && cur.Phase == "Reverted":
		add(Milestone{Kind: kindReverted, Text: subject + " " + inertWord(cur.Phase, phaseWords(cur.Phase)), URL: prURL})
	}
	switch {
	case !held(prev) && held(cur):
		add(Milestone{Kind: kindHeld, Text: word(map[string]string{
			"waiting_human_driver": "held while a person drives the room",
			"waiting_room_busy":    "held while another run uses the room",
		}, cur.Reason)})
	case held(prev) && !held(cur):
		add(Milestone{Kind: kindResumed, Text: "the hold ended: " + inertWord(cur.Phase, phaseWords(cur.Phase))})
	}
	if what, now := budgetReached(cur); now {
		if _, before := budgetReached(prev); !before {
			add(Milestone{Kind: kindBudgetReached, Text: "the budget is reached: " + what})
		}
	}
	return ms
}

// span is how long d is, at a glance.
func span(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return strconv.Itoa(int(d/(24*time.Hour))) + " d"
	case d >= time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + " h"
	}
	return strconv.Itoa(max(0, int(d/time.Minute))) + " min"
}

// Changes writes view 4: a header, then one line per milestone, then how many more there are.
func Changes(w io.Writer, c ChangeSet, o Options) error {
	v := &card{o: o}
	head := v.text(c.Room, false)
	if !c.Since.IsZero() {
		head += " · since " + stamp(c.Since, o.Now)
	}
	head = v.bold(head)
	if !c.Since.IsZero() {
		head += " (" + span(o.Now.Sub(c.Since)) + ")"
	}
	v.line(head)
	v.line("")
	if len(c.Items) == 0 {
		v.line("Nothing changed.")
	}
	for _, m := range c.Items {
		v.line("- " + clock(m.At, c.Since) + rangeEnd(m, c.Since) + " " + v.milestone(m))
	}
	if c.More > 0 {
		v.line("")
		v.line(strconv.Itoa(c.More) + " more")
	}
	_, err := io.WriteString(w, v.b.String())
	return err
}

// clock is t as the time of day, with its date when it is not on the header's day.
func clock(t, since time.Time) string {
	t = t.UTC()
	if !since.IsZero() && t.Format("2006-01-02") != since.UTC().Format("2006-01-02") {
		return t.Format("01-02 15:04")
	}
	return t.Format("15:04")
}

// rangeEnd is the "–hh:mm" an activity span ends at, when it ends in another minute.
func rangeEnd(m Milestone, since time.Time) string {
	if m.Kind != kindActivity || m.Until.IsZero() || clock(m.Until, since) == clock(m.At, since) {
		return ""
	}
	return "–" + clock(m.Until, since)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func (c *card) milestone(m Milestone) string {
	switch m.Kind {
	case kindNote:
		return "note " + c.quoted(m.Text, false)
	case kindActivity:
		who := "the room"
		if m.Run != "" {
			who = c.runRef(m.Run)
		}
		parts := []string{plural(m.Turns, "model turn", "model turns"), plural(m.Tools, "tool call", "tool calls")}
		if m.Pushes > 0 {
			parts = append(parts, plural(m.Pushes, "push", "pushes"))
		}
		return who + ": " + strings.Join(parts, ", ")
	case kindApprovalRequested:
		s := c.bold("approval requested") + ": " + c.quoted(m.Text, false)
		if !m.Until.IsZero() {
			s += ", by " + stamp(m.Until, c.o.Now)
		}
		if allowed(m.URL, c.o) {
			s += " → " + c.url("decide in the room", m.URL)
		}
		return s
	case kindApprovalDecided:
		return c.bold("approval decided") + ": " + c.sentence(m.Text, "")
	case kindVerdict:
		s := "review verdict: " + c.sentence(m.Text, "")
		if ref := prRef(m.URL); ref != "" {
			s += " on " + c.sentence(ref, m.URL)
		}
		return s
	case kindHandoff:
		s := "handoff " + c.text(m.Role, false)
		if m.Text != "" {
			s += ": " + c.quoted(m.Text, false)
		}
		return s
	}
	return c.sentence(m.Text, m.URL)
}

// runRef is a run id as the view shows it.
func (c *card) runRef(id string) string {
	if c.md() {
		return code(id, false)
	}
	return clean(id)
}

// sentence is a sentence the broker wrote, its code spans kept in Markdown and dropped on a terminal,
// with the first "#N" naming url's pull request linked.
func (c *card) sentence(s, url string) string {
	if ref := prRef(url); ref != "" && allowed(url, c.o) {
		if i := strings.Index(s, ref); i >= 0 && (i+len(ref) == len(s) || s[i+len(ref)] < '0' || s[i+len(ref)] > '9') {
			s = s[:i] + c.url(ref, url) + s[i+len(ref):]
		}
	}
	if !c.md() {
		s = strings.ReplaceAll(s, "`", "")
	}
	return s
}
