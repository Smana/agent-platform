// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Smana/agent-platform/internal/mcpwire"
	"github.com/Smana/agent-platform/internal/roomview"
)

// A room is named the way a developer says it (O7): its id, `#<issue>`, `PR #<n>` or a bare number,
// resolved through the room list. A reference that matches several rooms is never guessed: the
// candidates are listed and the developer chooses.

// refRE reads `#12`, `12`, `PR #12`, `pr 12`: the optional PR marker, then the number.
var refRE = regexp.MustCompile(`(?i)^(pr|pull request|pull)?\s*#?\s*([1-9][0-9]{0,8})$`)

// refHint is what the tools say a room can be.
const refHint = "the room's id, or #<issue>, PR #<n> or a bare issue number"

// roomRefTools are the broker tools whose `room` the relay resolves; room_wait and room_checkout
// resolve their own, as local tools.
var roomRefTools = map[string]bool{"room_summary": true, mcpwire.RoomChanges: true, "room_queue": true}

// matchRef is the rooms ref names among rows, by issue first and then by PR; pr says the developer
// wrote "PR", which skips the issue match. ok is false when ref is not a reference at all.
func matchRef(ref string, rows []roomview.Row) (matches []roomview.Row, ok bool) {
	m := refRE.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return nil, false
	}
	n, _ := strconv.Atoi(m[2])
	pr := m[1] != ""
	byIssue := func(r roomview.Row) int { return r.Issue }
	byPR := func(r roomview.Row) int { return r.PR }
	passes := []func(roomview.Row) int{byIssue, byPR}
	if pr {
		passes = passes[1:]
	}
	for _, by := range passes {
		seen := map[string]bool{}
		for _, row := range rows {
			if by(row) == n && row.ID != "" && !seen[row.ID] {
				seen[row.ID] = true
				matches = append(matches, row)
			}
		}
		if len(matches) > 0 {
			return matches, true
		}
	}
	return nil, true
}

// resolveRef is the room id ref names, or the tool refusal to answer with. Text that is no
// reference passes unchanged: the tool or the broker refuses it, as before.
func (r *Relay) resolveRef(ctx context.Context, ref string) (string, any) {
	ref = strings.TrimSpace(ref)
	isRef := refRE.MatchString(ref)
	if !isRef {
		return ref, nil // an id, or what no reference is: the tool or the broker checks it
	}
	var rows []roomview.Row
	for i, args := range []map[string]any{{}, {"all": true}} { // open rooms first, then the ended ones
		got, err := r.listRows(ctx, args)
		if err != nil {
			if i == 0 {
				return "", mcpwire.ToolError(OneLine(explainRelay(err)))
			}
			break
		}
		if roomIDRE.MatchString(ref) { // eight digits of 2 to 7 are an id, if one exists
			for _, row := range got {
				if row.ID == ref {
					return ref, nil
				}
			}
		}
		rows = got
		if m, _ := matchRef(ref, rows); len(m) > 0 {
			break
		}
	}
	matches, _ := matchRef(ref, rows)
	switch len(matches) {
	case 0:
		return "", mcpwire.ToolError("unknown_room: no room you can see is for " + OneLine(ref) + "; rooms_list shows the rooms")
	case 1:
		return matches[0].ID, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ambiguous_room: %s matches %d rooms. Ask the developer which one, never guess, then call again with its room id:\n", OneLine(ref), len(matches))
	for _, m := range matches {
		fmt.Fprintf(&b, "- #%d %s · %s · room %s\n", m.Issue, OneLine(m.Title), OneLine(m.Repository), m.ID)
	}
	return "", mcpwire.ToolError(strings.TrimRight(b.String(), "\n"))
}

// resolveArgs rewrites the room reference of a call's arguments to a room id: key is "room" (a
// string) or "rooms" (a list of them). Anything else passes unchanged, for the tool to refuse.
func (r *Relay) resolveArgs(ctx context.Context, args json.RawMessage, key string) (json.RawMessage, any) {
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) != nil || m == nil {
		return args, nil
	}
	var refs []string
	switch key {
	case "room":
		var s string
		if json.Unmarshal(m[key], &s) != nil {
			return args, nil
		}
		refs = []string{s}
	default:
		if json.Unmarshal(m[key], &refs) != nil {
			return args, nil
		}
	}
	for i, ref := range refs {
		id, refusal := r.resolveRef(ctx, ref)
		if refusal != nil {
			return nil, refusal
		}
		refs[i] = id
	}
	var out []byte
	if key == "room" {
		out, _ = json.Marshal(refs[0])
	} else {
		out, _ = json.Marshal(refs)
	}
	m[key] = out
	rewritten, err := json.Marshal(m)
	if err != nil {
		return args, nil
	}
	return rewritten, nil
}

// refSchema lets a room property take a reference: the id pattern goes, the description says what
// is accepted.
func refSchema(schema any) any {
	sch, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	props, _ := sch["properties"].(map[string]any)
	for _, k := range []string{"room", "rooms"} {
		p, _ := props[k].(map[string]any)
		if p == nil {
			continue
		}
		target := p
		if items, ok := p["items"].(map[string]any); ok {
			target = items
		}
		delete(target, "pattern")
		target["description"] = refHint
	}
	return sch
}
