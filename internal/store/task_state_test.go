// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"

	"github.com/Smana/agent-platform/internal/envelope"
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

func stateDraft(n int64, payload []byte) envelope.Draft {
	d := draft("factory", n)
	d.Type, d.Origin, d.Payload = envelope.StateChanged, envelope.OriginBroker, payload
	d.Actor = envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"}
	d.RunID = ""
	return d
}
