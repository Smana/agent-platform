// SPDX-License-Identifier: Apache-2.0

// Package finding holds what the two RunLore intakes (the retiring /intake/runlore and POST
// /v1/tasks for an internal principal) must write the same way while both exist: the task text,
// the public stub issue and the label charset that keeps attacker-influenced identifiers inert.
package finding

import (
	"fmt"
	"regexp"
	"strings"
)

// LabelSourceKey carries taskid.Name of a finding's dedup key on every task made from that key
// (§1): tasks of one key share the label, so a replay finds the open one and a new generation
// starts after the last task ends.
const LabelSourceKey = "agents.ogenki.io/source-key"

// labelUnsafe is every character outside the label charset ([A-Za-z0-9_.:/-]).
var labelUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.:/-]`)

// LabelSafe transposes every character outside the label charset to "_", so no markdown or HTML
// trigger reaches the public issue (TW6).
//
// Chosen over sanitize.Text deliberately: that defuses images, "]:" definitions and raw HTML but
// leaves an inline link, "[click](javascript:alert(1))", intact, and these fields are identifiers
// whose natural charset is [A-Za-z0-9_.:/-] (alert names, resource refs, severities). Transposing
// everything else to "_" costs nothing they legitimately hold while removing every markdown and
// HTML trigger character ("!", "[", "]", "(", "<", "`", space) and every invisible code point.
// Verdict is already a closed set and Confidence a float.
func LabelSafe(s string) string { return labelUnsafe.ReplaceAllString(s, "_") }

// Text is the task text of a finding: a heading, RunLore's verdict line, then its text.
func Text(title, verdict string, confidence float64, resourceRef, alert, severity, cluster, text string) string {
	return fmt.Sprintf("# %s\n\nRunLore verdict %s (confidence %.2f) on %s, alert %s, severity %s, cluster %s.\n\n%s",
		title, verdict, confidence, resourceRef, alert, severity, cluster, text)
}

// StubIssue is the public issue of a finding: alert, resource, verdict and the room, never the
// finding itself (R33).
func StubIssue(alert, resourceRef, severity, verdict string, confidence float64, task, roomsURL string) (title, body string) {
	a, ref, sev := LabelSafe(alert), LabelSafe(resourceRef), LabelSafe(severity)
	body = fmt.Sprintf("RunLore reported **%s** on `%s` (alert `%s`, verdict `%s`, confidence %.2f).\n\n"+
		"Agent factory task `%s` investigates it. The finding and the work stay in its room: %s/r/%s (tailnet only).\n\n"+
		"Apply `factory/stop` to stop it.", sev, ref, a, verdict, confidence,
		task, strings.TrimSuffix(roomsURL, "/"), task)
	return fmt.Sprintf("RunLore: %s on %s", a, ref), body
}
