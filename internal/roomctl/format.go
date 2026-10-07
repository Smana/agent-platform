// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/Smana/agent-platform/internal/envelope"
)

// maxText bounds the text a line quotes, in runes.
const maxText = 160

// printable keeps only printable runes: room text is untrusted, and an escape or
// bidi control would drive the developer's terminal (T10's terminal twin).
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
}

// Line is one terminal row per event: seq, who, what, then its text on one line.
func Line(ev envelope.Event) string {
	who := ev.Actor.ID
	if ev.Actor.Role != "" {
		who += " (" + ev.Actor.Role + ")"
	}
	var p map[string]any
	_ = json.Unmarshal(ev.Payload, &p)
	field := func(k string) string {
		if v, ok := p[k]; ok && v != nil {
			return fmt.Sprint(v)
		}
		return ""
	}
	what, text := []string{string(ev.Type)}, ""
	switch ev.Type {
	case envelope.Message:
		if field("kind") == string(envelope.KindReviewVerdict) {
			what = []string{"review_verdict", field("verdict")}
		} else if d := field("delivery"); d != "" && d != string(envelope.DeliveryNone) {
			what = append(what, d)
		}
		text = field("text")
	case envelope.ToolCall:
		what = append(what, field("tool"))
		if args, ok := p["args"].(map[string]any); ok {
			text, _ = args["command"].(string)
		}
	case envelope.ToolResult:
		what = append(what, field("status"))
	case envelope.StateChanged:
		what = []string{field("kind"), field("phase"), field("reason")}
	case envelope.Handoff:
		what = []string{"handoff →", field("toRole")}
		if c := field("commit"); c != "" {
			what = append(what, "@", c)
		}
		text = field("summary")
	}
	line := fmt.Sprintf("#%d %s %s", ev.Seq, who, strings.Join(strings.Fields(strings.Join(what, " ")), " "))
	if text = strings.Join(strings.Fields(text), " "); text != "" {
		if r := []rune(text); len(r) > maxText {
			text = string(r[:maxText]) + "…"
		}
		line += ": " + text
	}
	return printable(line)
}

// SafeText is printable for multi-line text the broker sent, such as a claim:
// its newlines stay.
func SafeText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
}

// reasons explains the rejections roomctl's acts can meet (docs/api.md).
var reasons = map[string]string{
	"not_permitted":       "not allowed: your role in this room or this client does not permit it, or the factory refused you a run",
	"rate_limited":        "too many actions at once, or over 3 forks then one a minute; wait and retry",
	"room_busy":           "a run is already running there, or one was just requested and has not joined yet",
	"reviewer_needs_pr":   "a reviewer needs --pr, since no agent handoff or verdict names a pull request",
	"over_budget":         "the factory refused the run: you are over your budget",
	"factory_unavailable": "the run could not be requested right now; retry",
	"bad_action":          "the broker refused the request as malformed: check the seq, role, PR and egress profiles",
	"sealed":              "the room is sealed: it takes no more messages",
	"conflict":            "the room changed at the same moment; retry",
	"log_unavailable":     "the room's log is unavailable right now; retry",
	"too_large":           "the fork point is past 5,000 events or 32 MiB of the room's log: fork at an earlier seq",
}

// Explain is a rejection in words, or the reason itself when it has none.
func Explain(reason string) string {
	if why, ok := reasons[reason]; ok {
		return reason + ": " + why
	}
	return printable(reason)
}
