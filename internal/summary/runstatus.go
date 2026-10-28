// SPDX-License-Identifier: Apache-2.0

package summary

import (
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/Smana/maku/internal/policy"
)

// ApprovalCounts is a room's live approvals as its status line counts them for a viewer.
type ApprovalCounts struct {
	Mine   int `json:"mine"`   // the viewer may decide them
	Others int `json:"others"` // only others may
}

// Decidable reports whether web, the viewer as the web UI, may decide an approval its prompters
// prompted: the Decide right, and under four-eyes not one of them (OD-16), as the page's
// decideStatus and the act's check say.
func Decidable(web policy.Subject, fourEyes bool, prompters []string) bool {
	return policy.Allowed(web, policy.Decide) && (!fourEyes || !slices.Contains(prompters, web.ID))
}

// CountApprovals counts the live approvals of st for web: one past its deadline that the sweep
// has not decided is none, as needsYou drops it. nil when none is live.
func (st State) CountApprovals(web policy.Subject, fourEyes bool, now time.Time) *ApprovalCounts {
	var c ApprovalCounts
	for _, a := range st.Pending {
		switch {
		case !a.ExpiresAt.IsZero() && now.After(a.ExpiresAt):
		case Decidable(web, fourEyes, a.Prompters):
			c.Mine++
		default:
			c.Others++
		}
	}
	if c == (ApprovalCounts{}) {
		return nil
	}
	return &c
}

// Run status kinds: the first thing a room says, so a reader sees at a glance where it stands.
// web/src/run-status.ts mirrors this file; internal/summary/runstatus_cases.json is the
// cases both pass.
const (
	KindRunning      = "running"
	KindQueued       = "queued"
	KindWaitingYou   = "waiting-you"
	KindWaitingOther = "waiting-other"
	KindWaitingPR    = "waiting-pr"
	KindWaiting      = "waiting"
	KindFinished     = "finished"
	KindDone         = "done"
	KindFailed       = "failed"
	KindStopped      = "stopped"
)

// RunStatus is a room's status line: a kind (the page colours by it), an icon and words. The words
// are fixed phrases, numbers and a role the broker bounds: never text an agent wrote.
type RunStatus struct {
	Kind  string `json:"kind"`
	Icon  string `json:"icon"`
	Words string `json:"words"`
}

// String is the line as text: "● Running — implementer, 4 min".
func (s RunStatus) String() string { return s.Icon + " " + s.Words }

// StatusIn is what StatusLine reads, all of it from the summary and the viewer's approvals. No
// reason code: the room page reads the summary's reason as prose, so a line keyed on the code would
// read differently there than in the list and maku status.
type StatusIn struct {
	State, Phase string
	RunRole      string    // the run's role
	RunStart     time.Time // zero when unknown
	RunPresent   bool      // an agent is present now
	PR, Issue    int
	Mine, Others int // live approvals the viewer may decide, and those only others may
	Step         *Step
	Now          time.Time
}

var roleRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)

// Elapsed is a duration at a glance: "<1 min", "4 min", "1 h 5 min".
func Elapsed(from, now time.Time) string {
	m := int(max(0, now.Sub(from)) / time.Minute)
	switch {
	case m < 1:
		return "<1 min"
	case m < 60:
		return fmt.Sprintf("%d min", m)
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
}

// StatusLine is the room's status, "" kind when the state is one this broker does not know.
func StatusLine(in StatusIn) RunStatus {
	s := func(kind, icon, words string) RunStatus { return RunStatus{kind, icon, words} }
	fail := func() RunStatus {
		switch in.Phase {
		case "Rejected":
			return s(KindFailed, "✗", "Failed — rejected")
		case "Reverted":
			return s(KindFailed, "✗", "Failed — the merge was reverted")
		}
		return s(KindFailed, "✗", "Failed")
	}
	switch {
	case in.State == StateDone && in.Phase == "NoOp":
		return s(KindDone, "✓", "Done — nothing to change")
	case in.State == StateDone && in.PR > 0:
		return s(KindDone, "✓", "Done — merged")
	case in.State == StateDone:
		return s(KindDone, "✓", "Done")
	case in.State == StateFailed:
		return fail()
	case in.State == StateStopped || in.Phase == "Closed" || in.Phase == "Sealed":
		return s(KindStopped, "■", "Stopped")
	case in.Mine == 1:
		return s(KindWaitingYou, "⏳", "Waiting for you — approve a pending action")
	case in.Mine > 1:
		return s(KindWaitingYou, "⏳", fmt.Sprintf("Waiting for you — decide %d approvals", in.Mine))
	case in.Others > 0:
		return s(KindWaitingOther, "⏳", "Waiting for another approver")
	case in.Step != nil && in.Step.Kind == StepPR && in.PR > 0:
		return s(KindWaitingPR, "⏳", fmt.Sprintf("Waiting — review and merge PR #%d", in.PR))
	case in.Step != nil && in.Step.Kind == StepAnswer && in.Issue > 0:
		return s(KindWaiting, "⏳", fmt.Sprintf("Waiting — answer the question on issue #%d", in.Issue))
	case in.Phase == "Escalated":
		return s(KindWaiting, "⏳", "Waiting — a maintainer decides")
	case in.State == StateWaiting:
		return s(KindWaiting, "⏳", "Waiting — for the next message or run")
	case in.State == StateQueued:
		return s(KindQueued, "○", "Queued")
	case in.State == StateRunning && in.RunPresent:
		parts := ""
		if roleRE.MatchString(in.RunRole) {
			parts = in.RunRole
		}
		if !in.RunStart.IsZero() {
			if parts != "" {
				parts += ", "
			}
			parts += Elapsed(in.RunStart, in.Now)
		}
		if parts == "" {
			return s(KindRunning, "●", "Running")
		}
		return s(KindRunning, "●", "Running — "+parts)
	case in.State == StateRunning && in.PR > 0 && in.Phase == "Reviewing":
		return s(KindFinished, "✓", fmt.Sprintf("Finished — PR #%d opened, handed to the reviewer", in.PR))
	case in.State == StateRunning && in.PR > 0 && in.Phase == "AwaitingCI":
		return s(KindFinished, "✓", fmt.Sprintf("Finished — PR #%d opened, waiting for CI", in.PR))
	case in.State == StateRunning:
		return s(KindRunning, "●", "Running — between runs")
	}
	return RunStatus{}
}

// StatusOf is the status of a summary, as the caller holds it: now is the clock the run's age reads.
func StatusOf(s Summary, now time.Time) RunStatus {
	in := StatusIn{State: s.Status.State, Phase: s.Status.Phase, Step: s.Step, Now: now}
	if in.State == "" {
		in.State = Coarse(s.Status.Phase, s.Status.Phase, false)
	}
	if r := s.Status.Run; r != nil {
		in.RunRole, in.RunStart, in.RunPresent = r.Role, r.StartedAt, r.EndedAt.IsZero()
	}
	if s.Status.PR != nil {
		in.PR = s.Status.PR.Number
	}
	if s.Status.Issue != nil {
		in.Issue = s.Status.Issue.Number
	}
	if a := s.Status.Approvals; a != nil {
		in.Mine, in.Others = a.Mine, a.Others
	} else { // a broker without the counts: what needs the caller is theirs
		for _, n := range s.NeedsYou {
			if n.Kind == "approval" {
				in.Mine++
			}
		}
	}
	return StatusLine(in)
}
