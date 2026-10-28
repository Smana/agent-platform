// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Smana/maku/internal/mcpwire"
	"github.com/Smana/maku/internal/roomview"
)

// A room is named the way a developer says it (O7): its id, `#<issue>`, `PR #<n>` or a bare number,
// resolved through the room list. A reference that matches several rooms is never guessed: the
// candidates are listed and the developer chooses.

// refRE reads `#12`, `12`, `PR #12`, `pr 12`: the optional PR marker, then the number.
var refRE = regexp.MustCompile(`(?i)^(pr|pull request|pull)?\s*#?\s*([1-9][0-9]{0,8})$`)

// refHint is what the tools say a room can be.
const refHint = "the room's id, #<issue>, PR #<n>, a bare number (asked about when it is an issue in one room and a PR in another) or a fragment of its title"

// roomRefTools are the broker tools whose `room` the relay resolves; room_wait and room_checkout
// resolve their own, as local tools.
var roomRefTools = map[string]bool{"room_summary": true, mcpwire.RoomChanges: true, "room_queue": true, "room_tail": true, "room_fork": true}

// minFragment is the shortest title fragment that names a room.
const minFragment = 3

// match is a room a reference names, and what it named: its issue, its PR or its title.
type match struct {
	row  roomview.Row
	kind string // "issue", "PR" or "title"
}

// matchRef is the rooms ref names among rows. `#12` is the issue and `PR 12` the PR; a bare 12 is
// either, so it can match two rooms. Text that is none of those is a title fragment of at least
// minFragment characters, case-insensitive. isRef is false for a text that names nothing of the
// kind (a short fragment), so the caller passes it through.
func matchRef(ref string, rows []roomview.Row) (matches []match, isRef bool) {
	ref = strings.TrimSpace(ref)
	if m := refRE.FindStringSubmatch(ref); m != nil {
		n, _ := strconv.Atoi(m[2])
		hash := strings.Contains(ref, "#")
		pr := m[1] != ""
		seen := map[string]bool{}
		for _, row := range rows {
			if row.ID == "" {
				continue
			}
			if (!pr && row.Issue == n) && !seen["i"+row.ID] {
				seen["i"+row.ID] = true
				matches = append(matches, match{row, "issue"})
			}
			if (pr || !hash) && row.PR == n && !seen["p"+row.ID] {
				seen["p"+row.ID] = true
				matches = append(matches, match{row, "PR"})
			}
		}
		return matches, true
	}
	if utf8.RuneCountInString(ref) < minFragment {
		return nil, false
	}
	low := strings.ToLower(ref)
	for _, row := range rows {
		if row.ID != "" && strings.Contains(strings.ToLower(row.Title), low) {
			matches = append(matches, match{row, "title"})
		}
	}
	return matches, true
}

// resolveRef is the room id ref names, or the tool refusal to answer with. Text that names no room
// reference passes unchanged: the tool or the broker refuses it, as before. A room id is taken as
// it is, so a title fragment that looks like one (eight letters) is never read as a title.
func (r *Relay) resolveRef(ctx context.Context, ref string) (string, any) {
	ref = strings.TrimSpace(ref)
	if roomIDRE.MatchString(ref) && !refRE.MatchString(ref) {
		return ref, nil
	}
	if _, isRef := matchRef(ref, nil); !isRef {
		return ref, nil
	}
	var rows []roomview.Row
	var matches []match
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
		if matches, _ = matchRef(ref, rows); len(matches) > 0 {
			break
		}
	}
	switch len(matches) {
	case 0:
		if !refRE.MatchString(ref) {
			return ref, nil // no title has it: the tool or the broker refuses it
		}
		return "", mcpwire.ToolError("unknown_room: no room you can see is for " + OneLine(ref) + "; rooms_list shows the rooms")
	case 1:
		return matches[0].row.ID, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ambiguous_room: %s matches %d rooms. Ask the developer which one, never guess, then call again with its room id", OneLine(ref), len(matches))
	if m := refRE.FindStringSubmatch(ref); m != nil && !strings.Contains(ref, "#") && m[1] == "" {
		fmt.Fprintf(&b, " (or say `#%s` for the issue, `PR #%s` for the PR)", m[2], m[2])
	}
	b.WriteString(":\n")
	for _, m := range matches {
		fmt.Fprintf(&b, "- %s: #%d %s · %s · room %s\n", m.kind, m.row.Issue, OneLine(m.row.Title), OneLine(m.row.Repository), m.row.ID)
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
