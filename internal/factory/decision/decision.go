// SPDX-License-Identifier: Apache-2.0

// Package decision is the merge gate's per-decision record (human path AM8): the values each
// declared row read, so a tally can replay a cap and count what the gate refused that a human
// merged unchanged.
package decision

import (
	"fmt"
	"strconv"
)

// Rows are the declared rows' readings. A string row is "ok", "n/a" (the class declares none),
// the row's refusal reason, or "" when a refusal before it left it unread. Files and Lines are
// GitHub's counts of the diff, read only when Counted (the decision read the diff); MaxFiles and
// MaxLines are the class's caps (0 = none). Reviewer is the reviewer run's served models joined
// with "+", "none" without a reviewer run. Paused and Capped are what the breaker and the day's
// count said, read only when Gated (the decision got past the declared rows to read them).
type Rows struct {
	Files, MaxFiles, Lines, MaxLines      int
	Counted                               bool
	Shape, Holdout, Oracle, Age, Evidence string
	Paused, Capped, Gated                 bool
	Reviewer                              string
}

// Marker is the hidden one-line record appended to the decision's comment. It is a line of its
// own beside the comment's event marker (ruling R-m11), and class= lets the tally filter by
// class (R-I9). An unread field prints "-" (R-m7): an empty string, and the counts of a diff not
// read, so a replay never reads a count of 0 that nobody took. Every field is one token.
func Marker(head, class, verdict, reason string, r Rows) string {
	return fmt.Sprintf("<!-- agent-factory-decision head=%s class=%s decision=%s:%s files=%s/%d lines=%s/%d shape=%s holdout=%s oracle=%s age=%s evidence=%s paused=%s capped=%s reviewer=%s -->",
		dash(head), dash(class), dash(verdict), dash(reason), count(r.Files, r.Counted), r.MaxFiles, count(r.Lines, r.Counted), r.MaxLines,
		dash(r.Shape), dash(r.Holdout), dash(r.Oracle), dash(r.Age), dash(r.Evidence), flag(r.Paused, r.Gated), flag(r.Capped, r.Gated), dash(r.Reviewer))
}

// dash is s, or "-" for an unread value.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// count is n, or "-" for a count not read.
func count(n int, read bool) string {
	if !read {
		return "-"
	}
	return strconv.Itoa(n)
}

// flag is b, or "-" for a flag not read.
func flag(b, read bool) string {
	if !read {
		return "-"
	}
	return strconv.FormatBool(b)
}
