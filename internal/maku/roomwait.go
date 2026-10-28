// SPDX-License-Identifier: Apache-2.0

package maku

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Smana/maku/internal/mcpwire"
	"github.com/Smana/maku/internal/roomview"
	"github.com/Smana/maku/internal/summary"
)

// room_wait's bounds. A wait is bounded: timeout defaults to waitDefault seconds and never exceeds
// waitMax, so no call holds a slot forever. Waits have their own pool (maxWaits, see Serve), so
// they never starve the relay's other requests.
const (
	waitDefault = 300              // seconds
	waitMax     = 1800             // seconds
	maxWatched  = 20               // rooms one call watches
	maxWaits    = 8                // room_wait calls in flight at once; past it, a call is refused
	waitGrowth  = 1.5              // the poll interval grows by this while nothing moves, up to maxPoll
	pollFailFor = 60 * time.Second // how long polls may keep failing before the wait gives up
	urlMode     = "2025-11-25"
)

// The conditions room_wait waits for (R13), in the order they are checked.
const (
	condNeedsYou  = "needs_you"
	condPROpened  = "pr_opened"
	condEnded     = "ended"
	condAnyChange = "any_change"
	condTimeout   = "timeout"
)

var (
	waitConditions = []string{condNeedsYou, condPROpened, condEnded, condAnyChange}
	roomIDRE       = regexp.MustCompile(`^[a-z2-7]{8}$`)
	linkIDRE       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

var roomWaitEntry = map[string]any{
	"name": toolRoomWait,
	"description": "Wait until a room has something new for the developer, polling the broker from their machine: " +
		"a need (needs_you), a pull request (pr_opened), its end (ended), or any change (any_change). " +
		"A room is " + refHint + ". Without rooms it watches the developer's own open rooms, the 20 most recently updated. " +
		"Without until it waits for needs_you, pr_opened and ended. The wait is bounded: timeout is in seconds, " +
		"300 by default and at most 1800, and then the result says nothing new yet; call it again to keep waiting. " +
		"It reports progress when the client gave a progress token, and a cancellation ends it at once. " +
		"The result is the room's card and the condition that was met. Run it in a background task where the client has them.",
	"inputSchema": json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` +
		`"rooms":{"type":"array","maxItems":20,"items":{"type":"string","description":"the room's id, #<issue>, PR #<n>, a bare number or a fragment of its title"}},` +
		`"until":{"type":"array","items":{"type":"string","enum":["needs_you","pr_opened","ended","any_change"]}},` +
		`"timeout":{"type":"integer","minimum":1,"maximum":1800}}}`),
}

// waitHit is the room and the condition that ended a wait.
type waitHit struct{ Room, Condition string }

func rowEnded(r roomview.Row) bool {
	switch r.State {
	case summary.StateDone, summary.StateFailed, summary.StateStopped:
		return true
	}
	return false
}

// holds is whether condition c became true between base and now.
func holds(c string, base, now roomview.Row) bool {
	switch c {
	case condNeedsYou:
		// The caller's own fields: NeedsMe says some human has an item, not that this one does.
		return (now.NeedsYou > 0 && base.NeedsYou == 0) || (now.Need != nil && (base.Need == nil || base.Need.ID != now.Need.ID))
	case condPROpened:
		return base.PR == 0 && now.PR > 0
	case condEnded:
		return !rowEnded(base) && rowEnded(now)
	case condAnyChange:
		return now.LastSeq != base.LastSeq
	}
	return false
}

// hit is the first room, by id, in which a condition of until newly holds now, against the
// snapshot base taken when the wait began. A condition already true in base is no hit; a room
// absent from either is not compared. Without until it is the first three conditions (L13).
func hit(base, now map[string]roomview.Row, until []string) (waitHit, bool) {
	want := until
	if len(want) == 0 {
		want = waitConditions[:3]
	}
	ids := make([]string, 0, len(now))
	for id := range now {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		b, ok := base[id]
		if !ok {
			continue
		}
		for _, c := range waitConditions {
			if slices.Contains(want, c) && holds(c, b, now[id]) {
				return waitHit{Room: id, Condition: c}, true
			}
		}
	}
	return waitHit{}, false
}

type waitArgs struct {
	Rooms   []string `json:"rooms"`
	Until   []string `json:"until"`
	Timeout *int     `json:"timeout"`
}

func parseWaitArgs(raw json.RawMessage) (waitArgs, time.Duration, string) {
	const usage = "invalid_arguments: rooms are room ids, until is some of needs_you, pr_opened, ended, any_change, timeout is 1 to 1800 seconds"
	var a waitArgs
	if mcpwire.DecodeArgs(raw, &a) != nil {
		return a, 0, usage
	}
	if len(a.Rooms) > maxWatched {
		return a, 0, "invalid_arguments: at most " + strconv.Itoa(maxWatched) + " rooms"
	}
	for _, id := range a.Rooms {
		if !roomIDRE.MatchString(id) {
			return a, 0, usage
		}
	}
	for _, c := range a.Until {
		if !slices.Contains(waitConditions, c) {
			return a, 0, usage
		}
	}
	timeout := waitDefault
	if a.Timeout != nil {
		timeout = *a.Timeout
		if timeout < 1 || timeout > waitMax {
			return a, 0, usage
		}
	}
	return a, time.Duration(timeout) * time.Second, ""
}

// listRows is one rooms_list call: the rows of the broker's structuredContent.
func (r *Relay) listRows(ctx context.Context, args map[string]any) ([]roomview.Row, error) {
	text, sc, isErr, err := r.callBroker(ctx, "rooms_list", args)
	if err != nil {
		return nil, err
	}
	if isErr {
		return nil, errors.New("rooms_list: " + OneLine(text))
	}
	var list struct {
		Rooms []roomview.Row `json:"rooms"`
	}
	if isErr || json.Unmarshal(sc, &list) != nil {
		return nil, errors.New("rooms_list gave no rooms")
	}
	return list.Rooms, nil
}

// roomWait is room_wait: it polls rooms_list, one call per tick for every watched room, and
// returns when a condition newly holds against the snapshot of its first poll, or at its timeout.
// It sleeps through r.after with no lock and no slot held, and ends with ctx.
func (r *Relay) roomWait(ctx context.Context, c localCall) any {
	args, refusal := r.resolveArgs(ctx, c.Args, "rooms")
	if refusal != nil {
		return refusal
	}
	c.Args = args
	a, timeout, bad := parseWaitArgs(c.Args)
	if bad != "" {
		return mcpwire.ToolError(bad)
	}
	listArgs := map[string]any{}
	if len(a.Rooms) == 0 {
		listArgs["mine"] = true
	}
	watch := map[string]bool{}
	for _, id := range a.Rooms {
		watch[id] = true
	}
	var base, prev map[string]roomview.Row
	interval, slept, start, failedFor, failing := defaultPoll, time.Duration(0), time.Now(), time.Duration(0), false
	for poll := 1; ; poll++ {
		rows, err := r.listRows(ctx, listArgs)
		if ctx.Err() != nil {
			return mcpwire.ToolError("cancelled")
		}
		if err != nil {
			failing = true
			if failedFor >= pollFailFor || errors.Is(err, errToken) {
				return mcpwire.ToolError("not_watching: " + explainRelay(err))
			}
		} else {
			failing, failedFor = false, 0
			now := r.watched(rows, a, watch, base == nil)
			switch {
			case base == nil && len(a.Rooms) > 0 && len(now) < len(watch):
				return mcpwire.ToolError("unknown_room: " + missing(watch, now) + " is not one of the rooms you can see")
			case base == nil && len(now) == 0:
				out, _ := mcpwire.ToolResult(map[string]string{"room": "", "condition": "none"})
				out["content"] = []map[string]string{{"type": "text", "text": "You have no open rooms to watch."}}
				return out
			case base == nil:
				base = now
			default:
				if h, ok := hit(base, now, a.Until); ok {
					return r.hitResult(ctx, h, now[h.Room])
				}
				if moved(prev, now) {
					interval = defaultPoll
				} else {
					interval = min(time.Duration(float64(interval)*waitGrowth), maxPoll)
				}
			}
			prev = now
			if ctx.Err() == nil { // nothing more is written for a cancelled request
				r.progress(c.Progress, float64(poll), 0, fmt.Sprintf("watching %d rooms, nothing new yet", len(now)))
			}
		}
		remaining := timeout - max(slept, time.Since(start))
		if remaining <= 0 {
			return r.timeoutResult(prev)
		}
		wait := min(interval, remaining)
		select {
		case <-ctx.Done():
			return mcpwire.ToolError("cancelled")
		case <-r.after(wait):
			if ctx.Err() != nil { // a cancel and the tick can be ready together: select picks either
				return mcpwire.ToolError("cancelled")
			}
			slept += wait
			if failing {
				failedFor += wait
			}
		}
	}
}

// watched is the rows to compare: the named rooms, or on the first poll the developer's open
// rooms (20 most recently updated), and afterwards those same rooms.
func (r *Relay) watched(rows []roomview.Row, a waitArgs, watch map[string]bool, first bool) map[string]roomview.Row {
	if len(a.Rooms) == 0 && first {
		open := slices.DeleteFunc(slices.Clone(rows), rowEnded)
		slices.SortStableFunc(open, func(x, y roomview.Row) int { return y.LastEventAt.Compare(x.LastEventAt) })
		open = slices.DeleteFunc(open, func(row roomview.Row) bool { return !roomIDRE.MatchString(row.ID) })
		for _, row := range open[:min(len(open), maxWatched)] {
			watch[row.ID] = true
		}
	}
	out := map[string]roomview.Row{}
	for _, row := range rows {
		if roomIDRE.MatchString(row.ID) && watch[row.ID] {
			out[row.ID] = row
		}
	}
	return out
}

func missing(watch map[string]bool, now map[string]roomview.Row) string {
	var ids []string
	for id := range watch {
		if _, ok := now[id]; !ok {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return strings.Join(ids, ", ")
}

// moved is whether any room's lastSeq differs between two polls.
func moved(prev, now map[string]roomview.Row) bool {
	for id, n := range now {
		if p, ok := prev[id]; ok && p.LastSeq != n.LastSeq {
			return true
		}
	}
	return false
}

func (r *Relay) timeoutResult(rows map[string]roomview.Row) any {
	if rows == nil { // no poll ever answered
		return mcpwire.ToolError("not_watching: could not reach the broker before the timeout")
	}
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	list := make([]roomview.Row, 0, len(ids))
	for _, id := range ids {
		list = append(list, rows[id])
	}
	var view bytes.Buffer
	if roomview.Overview(&view, list, roomview.Options{Format: roomview.Markdown, Now: time.Now(), Rooms: r.URL}) != nil {
		view.Reset()
	}
	out, _ := mcpwire.ToolResult(map[string]string{"room": "", "condition": condTimeout})
	out["content"] = []map[string]string{{"type": "text", "text": strings.TrimSpace(view.String() + "\n\nnothing new yet")}}
	return out
}

// hitResult is the result of a wait that ended: the room's card and the condition. For an approval
// need, a client that declared URL mode is first sent the card's own link (R14).
func (r *Relay) hitResult(ctx context.Context, h waitHit, row roomview.Row) any {
	out, _ := mcpwire.ToolResult(map[string]string{"room": h.Room, "condition": h.Condition})
	text := fmt.Sprintf("Room %s: %s.", h.Room, h.Condition)
	text0, sc, isErr, err := r.callBroker(ctx, "room_summary", map[string]string{"room": h.Room})
	if err == nil && !isErr {
		if card, ok := cardText(viewed("room_summary", cardResult(text0, sc), r.URL, time.Now())); ok {
			text = card
		}
		if h.Condition == condNeedsYou {
			r.offerApproval(h.Room, row, sc)
		}
	}
	out["content"] = []map[string]string{{"type": "text", "text": text}}
	return out
}

// offerApproval sends the client a URL-mode elicitation for the card's approval link, and does not
// wait for the answer: it never changes the result. The link is sent only when the client declared
// URL mode at 2025-11-25, the need is an approval, and the URL is the card's own, on the rooms host
// under /r/<room> (R14).
func (r *Relay) offerApproval(room string, row roomview.Row, card json.RawMessage) {
	r.mu.Lock()
	ok := r.urls && r.version == urlMode
	r.mu.Unlock()
	if !ok || row.Need == nil || row.Need.Kind != "approval" {
		return
	}
	var sum summary.Summary
	if json.Unmarshal(card, &sum) != nil {
		return
	}
	for _, n := range sum.NeedsYou {
		if n.Kind == "approval" && n.ID == row.Need.ID && r.roomLink(room, n.URL) {
			link := strings.TrimRight(r.URL, "/") + "/r/" + room // rebuilt: nothing of the broker's URL rides along
			if linkIDRE.MatchString(n.ID) {
				link += "#" + n.ID
			}
			r.mu.Lock()
			r.nextID++
			id := "maku-" + strconv.Itoa(r.nextID)
			r.mu.Unlock()
			r.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": "elicitation/create", "params": map[string]any{
				"mode": "url", "elicitationId": id, "url": link,
				"message": "Room " + room + " needs your approval. Approvals stay on the room page: open it to decide."}})
			return
		}
	}
}

// roomLink is whether u is on the configured rooms host (same scheme, no userinfo), under /r/<room>.
func (r *Relay) roomLink(room, u string) bool {
	want, err1 := url.Parse(r.URL)
	got, err2 := url.Parse(u)
	return err1 == nil && err2 == nil && got.Scheme == want.Scheme && want.Host != "" && got.Host == want.Host &&
		got.User == nil && got.Path == "/r/"+room
}
