// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"
	"time"

	"github.com/Smana/maku/internal/envelope"
)

func TestLastTaskStateIsTheHighestSeq(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := t.Context()
	if p, err := s.LastTaskState(ctx, room, 0); err != nil || p != nil {
		t.Fatalf("no task event: %s, %v", p, err)
	}
	for i, payload := range []envelope.Draft{
		stateDraft(1, envelope.StatePayload("task", map[string]any{"phase": "Planning"})),
		stateDraft(2, envelope.StatePayload("task", map[string]any{"phase": "Merged"})),
		stateDraft(3, envelope.StatePayload("run_phase", map[string]any{"phase": "Failed"})),
	} {
		if _, _, err := s.Append(ctx, payload); err != nil {
			t.Fatal(i, err)
		}
	}
	p, err := s.LastTaskState(ctx, room, 0)
	if err != nil || !strings.Contains(string(p), `"Merged"`) {
		t.Fatalf("payload = %s, %v: want the latest task event, not the later run_phase", p, err)
	}
}

// A fork's log starts with its source's prefix: facts at or below the fork point are the source's.
func TestLastTaskStateIsAfterTheBound(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := t.Context()
	var seqs []int64
	for _, d := range []envelope.Draft{
		stateDraft(1, envelope.StatePayload("task", map[string]any{"phase": "Planning"})),
		stateDraft(2, envelope.StatePayload("task", map[string]any{"phase": "Merged"})),
	} {
		ev, _, err := s.Append(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, ev.Seq)
	}
	if p, err := s.LastTaskState(ctx, room, seqs[1]); err != nil || p != nil {
		t.Fatalf("payload = %s, %v: nothing is after the bound", p, err)
	}
	if p, err := s.LastTaskState(ctx, room, seqs[0]); err != nil || !strings.Contains(string(p), `"Merged"`) {
		t.Fatalf("payload = %s, %v: want the task event after the bound", p, err)
	}
}

func TestLastTaskStateUsesItsIndex(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := t.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, "EXPLAIN "+lastTaskStateSQL, room, int64(0))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if !strings.Contains(plan.String(), "events_task_facts") {
		t.Fatalf("the plan does not use the partial index:\n%s", plan.String())
	}
}

// TaskStateAt reads the task facts as they stood at a seq: the highest one in (after, upTo].
func TestTaskStateAt(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := t.Context()
	phases := map[int]string{3: "Planning", 7: "Reviewing", 12: "Merged"}
	for n := 1; n <= 12; n++ {
		kind, payload := "run_phase", map[string]any{"phase": "x"}
		if ph, ok := phases[n]; ok {
			kind, payload = "task", map[string]any{"phase": ph}
		}
		ev, _, err := s.Append(ctx, stateDraft(int64(n), envelope.StatePayload(kind, payload)))
		if err != nil || ev.Seq != int64(n) {
			t.Fatalf("event %d: seq %d, %v", n, ev.Seq, err)
		}
	}
	for _, c := range []struct {
		after, upTo int64
		want        string // "" for none
	}{
		{0, 10, "Reviewing"},
		{7, 10, ""},
		{0, 6, "Planning"},
		{0, 2, ""},
		{0, 12, "Merged"},
		{3, 12, "Merged"},
	} {
		p, err := s.TaskStateAt(ctx, room, c.after, c.upTo)
		if err != nil || (c.want == "") != (p == nil) || !strings.Contains(string(p), c.want) {
			t.Errorf("(%d, %d]: %s, %v, want %q", c.after, c.upTo, p, err, c.want)
		}
	}
}

func TestTaskStateAtUsesItsIndex(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := t.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, "EXPLAIN "+taskStateAtSQL, room, int64(0), int64(10))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if !strings.Contains(plan.String(), "events_task_facts") {
		t.Fatalf("the plan does not use the partial index:\n%s", plan.String())
	}
}

func stateDraft(n int64, payload []byte) envelope.Draft {
	d := draft("factory", n)
	d.Type, d.Origin, d.Payload = envelope.StateChanged, envelope.OriginBroker, payload
	d.Actor = envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"}
	d.RunID = ""
	return d
}

// The room list's failed rows read their reason codes in one call: each room's last task facts
// above its own bound, nothing for a room without them.
func TestTaskReasonsReadsEveryRoomInOneCall(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := t.Context()
	if _, err := s.EnsureRoom(ctx, NewRoom{ID: "aaaaaaa2", Driver: "system:factory", Retention: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	var seqs []int64
	for _, d := range []envelope.Draft{
		stateDraft(1, envelope.StatePayload("task", map[string]any{"phase": "Implementing", "reason": "older"})),
		stateDraft(2, envelope.StatePayload("task", map[string]any{"phase": "Rejected", "reason": "ci_red"})),
	} {
		ev, _, err := s.Append(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, ev.Seq)
	}
	got, err := s.TaskReasons(ctx, map[string]int64{room: 0, "aaaaaaa2": 0, "nosuchrm": 0})
	if err != nil || len(got) != 1 || got[room] != "ci_red" {
		t.Fatalf("got %v, %v: want the latest facts' code, nothing for a room without facts", got, err)
	}
	if got, err := s.TaskReasons(ctx, map[string]int64{room: seqs[1]}); err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v: nothing is after the bound", got, err)
	}
	if got, err := s.TaskReasons(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("no rooms: %v, %v", got, err)
	}
}
