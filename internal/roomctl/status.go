// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Smana/agent-platform/internal/summary"
)

// oneLine is SafeText for a value that must stay on its line: every agent-written
// string passes through it, the broker's own links included.
func oneLine(s string) string {
	return SafeText(strings.Join(strings.Fields(s), " "))
}

// stamp is t in UTC, and says so: the time alone when t falls on now's UTC day, else the whole
// RFC 3339 instant, so a deadline read the next day is not taken for today's.
func stamp(t, now time.Time) string {
	t, now = t.UTC(), now.UTC()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04 UTC")
	}
	return t.Format(time.RFC3339)
}

// RenderSummary prints a summary/v1 body for a terminal. Approvals are links and
// never a command (ruling P18); the commands printed are the ones the broker
// already filtered by the caller's standing. Times are UTC, dated unless on now's day.
func RenderSummary(w io.Writer, raw []byte, now time.Time) error {
	var s summary.Summary
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("status: the broker's answer: %w", err)
	}
	var b strings.Builder
	head := []string{"phase: " + oneLine(s.Status.Phase)}
	if r := s.Status.Run; r != nil {
		head = append(head, fmt.Sprintf("run: %s (%s)", oneLine(r.ID), oneLine(r.Role)))
	}
	if u := s.Status.Budget; u != nil {
		head = append(head, fmt.Sprintf("budget: %d/%d", u.UsedTokens, u.LimitTokens))
	}
	b.WriteString(strings.Join(head, "  ") + "\n")
	if p := s.Status.PR; p != nil {
		fmt.Fprintf(&b, "PR #%d %s\n", p.Number, oneLine(p.URL))
	}
	if i := s.Status.Issue; i != nil {
		fmt.Fprintf(&b, "issue #%d %s\n", i.Number, oneLine(i.URL))
	}
	if v := s.Status.LastVerdict; v != nil {
		fmt.Fprintf(&b, "last verdict: %s %s\n", oneLine(v.By), oneLine(v.Verdict))
	}
	if len(s.NeedsYou) == 0 {
		b.WriteString("needs you: none\n")
	}
	for _, n := range s.NeedsYou {
		by := ""
		if !n.Deadline.IsZero() {
			by = " by " + stamp(n.Deadline, now)
		}
		fmt.Fprintf(&b, "needs you: approve \"%s\"%s → %s\n", oneLine(n.What), by, oneLine(n.URL))
	}
	if len(s.Actions) > 0 {
		b.WriteString("actions:\n")
		for _, a := range s.Actions {
			if a.CLI == "" {
				fmt.Fprintf(&b, "  %s\n", oneLine(a.What))
			} else {
				fmt.Fprintf(&b, "  %s: %s\n", oneLine(a.What), oneLine(a.CLI))
			}
		}
	}
	if len(s.Notes.Items) > 0 {
		b.WriteString("notes (the agents' claims):\n")
		for _, n := range s.Notes.Items {
			fmt.Fprintf(&b, "  %s %s  %s\n", stamp(n.At, now), oneLine(n.Run), oneLine(n.Text))
		}
	}
	fmt.Fprintf(&b, "cursor: %s\n", oneLine(s.Cursor))
	_, err := io.WriteString(w, b.String())
	return err
}
