// SPDX-License-Identifier: Apache-2.0

// Package decision is the merge gate's per-decision record (human path AM8): the values each
// declared row read, so a tally can replay a cap and count what the gate refused that a human
// merged unchanged.
package decision

import "fmt"

// Rows are the declared rows' readings. A string row is "ok", "n/a" (the class declares none),
// the row's refusal reason, or "" when a refusal before it left it unread; Files and Lines are
// GitHub's counts against the class's caps (0 = none). Reviewer is the reviewer run's served
// models joined with "+", "none" without a reviewer run.
type Rows struct {
	Files, MaxFiles, Lines, MaxLines      int
	Shape, Holdout, Oracle, Age, Evidence string
	Paused, Capped                        bool
	Reviewer                              string
}

// Marker is the hidden one-line record appended to the decision's comment. It is a line of its
// own beside the comment's event marker (ruling R-m11), and class= lets the tally filter by
// class (R-I9). An empty field prints "-" (R-m7), so every field is one token.
func Marker(head, class, verdict, reason string, r Rows) string {
	return fmt.Sprintf("<!-- agent-factory-decision head=%s class=%s decision=%s:%s files=%d/%d lines=%d/%d shape=%s holdout=%s oracle=%s age=%s evidence=%s paused=%t capped=%t reviewer=%s -->",
		dash(head), dash(class), dash(verdict), dash(reason), r.Files, r.MaxFiles, r.Lines, r.MaxLines,
		dash(r.Shape), dash(r.Holdout), dash(r.Oracle), dash(r.Age), dash(r.Evidence), r.Paused, r.Capped, dash(r.Reviewer))
}

// dash is s, or "-" for an unread value.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
