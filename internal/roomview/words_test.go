// SPDX-License-Identifier: Apache-2.0

package roomview

import (
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Smana/agent-platform/internal/summary"
)

func TestStateWord(t *testing.T) {
	for state, want := range map[string]string{
		summary.StateWaiting: "waiting for a human", summary.StateRunning: "running", summary.StateQueued: "queued",
		summary.StateDone: "done", summary.StateFailed: "failed", summary.StateStopped: "stopped",
	} {
		if got := stateWord(state, false); got != want {
			t.Errorf("stateWord(%q) = %q, want %q", state, got, want)
		}
		if got := stateWord(state, true); got != "needs you" {
			t.Errorf("stateWord(%q, needsYou) = %q", state, got)
		}
	}
}

func TestEveryPhaseAndReasonHasWords(t *testing.T) {
	for _, p := range summary.TaskPhases() {
		if w := phaseWords(p); w == "" || w == p {
			t.Errorf("phaseWords(%q) = %q", p, w)
		}
	}
	for _, r := range summary.Reasons() {
		w := reasonWords(r)
		if w == "" || utf8.RuneCountInString(w) > 40 {
			t.Errorf("reasonWords(%q) = %q, want 1 to 40 runes", r, w)
		}
	}
}

func TestEveryTriggerOutcomeAndVerdictHasWords(t *testing.T) {
	for _, tr := range []string{"initial", "review", "human", "ci", "retry", "resume"} {
		if w := triggerWords(tr); w == "" {
			t.Errorf("triggerWords(%q) = %q", tr, w)
		}
	}
	if got := triggerWords("human"); got != "human review" {
		t.Errorf("triggerWords(human) = %q", got)
	}
	for _, o := range []string{"succeeded", "failed", "budget_exhausted", "revoked", "lost", "ended"} {
		if w := outcomeWords(o); w == "" {
			t.Errorf("outcomeWords(%q) is empty", o)
		}
	}
	for _, v := range []string{"approve", "changes", "none"} {
		if w := verdictWords(v); w == "" || w == v && v != "none" {
			t.Errorf("verdictWords(%q) = %q", v, w)
		}
	}
	if got := outcomeWords(""); got != "running" {
		t.Errorf("an unended run reads %q", got)
	}
}

func TestUnknownWordsAreCleaned(t *testing.T) {
	const hostile = "x\u202e\x1b[2J`"
	for name, got := range map[string]string{
		"phase": phaseWords(hostile), "reason": reasonWords(hostile), "trigger": triggerWords(hostile),
		"outcome": outcomeWords(hostile), "verdict": verdictWords(hostile), "state": stateWord(hostile, false),
	} {
		if got != "x[2J`" {
			t.Errorf("%s of an unknown value = %q, want it cleaned", name, got)
		}
	}
}

func TestDollars(t *testing.T) {
	for in, want := range map[int64]string{0: "$0.00", 4999: "$0.00", 5000: "$0.01", 310_000: "$0.31", 1_500_000: "$1.50", -5: "$0.00"} {
		if got := dollars(in); got != want {
			t.Errorf("dollars(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestTokens(t *testing.T) {
	for in, want := range map[int64]string{999: "999", 189093: "189 k", 999_499: "999 k", 999_500: "1 M", 1_500_000: "1.5 M"} {
		if got := tokens(in); got != want {
			t.Errorf("tokens(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestAgo(t *testing.T) {
	now := time.Date(2026, 10, 8, 19, 30, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		t    time.Time
		want string
	}{
		"minutes": {now.Add(-3 * time.Minute), "3 min ago"},
		"hours":   {now.Add(-2 * time.Hour), "2 h ago"},
		"a day":   {now.Add(-25 * time.Hour), "2026-10-07"},
		"future":  {now.Add(time.Minute), "0 min ago"},
	} {
		if got := ago(tc.t, now); got != tc.want {
			t.Errorf("%s: ago = %q, want %q", name, got, tc.want)
		}
	}
}

func TestStamp(t *testing.T) {
	now := time.Date(2026, 10, 8, 20, 0, 0, 0, time.UTC)
	if got := stamp(time.Date(2026, 10, 8, 19, 30, 0, 0, time.UTC), now); got != "19:30 UTC" {
		t.Errorf("today: %q", got)
	}
	if got := stamp(time.Date(2026, 10, 7, 19, 30, 0, 0, time.UTC), now); got != "2026-10-07 19:30 UTC" {
		t.Errorf("yesterday: %q", got)
	}
}
