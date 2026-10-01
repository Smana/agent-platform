// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"
)

// plan is the EXPLAIN of sql with args, as one string.
func plan(t *testing.T, s *Store, sql string, args ...any) string {
	t.Helper()
	rows, err := s.pool.Query(t.Context(), `EXPLAIN `+sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

// Review 4.3 I3: a stream's replay and its resume point read through their
// partial indexes, never a large room's whole log. The room holds 20 000 chat
// events, and a handful of deliveries and acks.
func TestDeliveriesAndLastAckUseTheirIndexes(t *testing.T) {
	s, _, _, super := open(t)
	forge(t, super, `INSERT INTO events (room_id, seq, id, run_id, actor_kind, actor_id, type, origin, origin_client,
		origin_seq, ts, payload)
		SELECT '`+room+`', g, 'id' || g, '7f3cq2xz', 'agent', 'agent:7f3cq2xz', 'message', 'harness', 'agent:7f3cq2xz', g, now(),
		'{"kind":"chat","text":"x","delivery":"none"}'
		FROM generate_series(1, 20000) g;
		INSERT INTO events (room_id, seq, id, actor_kind, actor_id, type, origin, origin_client, origin_seq, ts, payload) VALUES
		('`+room+`', 20001, 's1', 'human', 'human:own', 'message', 'client', 'human:own:s1', 1, now(),
		 '{"kind":"chat","text":"steer","to":["agent:7f3cq2xz"],"delivery":"steering"}'),
		('`+room+`', 20002, 's2', 'human', 'human:own', 'state_changed', 'client', 'human:own:s1', 2, now(),
		 '{"kind":"interrupt","runId":"7f3cq2xz"}');
		INSERT INTO events (room_id, seq, id, run_id, actor_kind, actor_id, type, origin, origin_client, origin_seq, ts, payload) VALUES
		('`+room+`', 20003, 'a1', '7f3cq2xz', 'agent', 'agent:7f3cq2xz', 'state_changed', 'harness', 'agent:7f3cq2xz:status', 1, now(),
		 '{"kind":"delivered","ref":20001,"runId":"7f3cq2xz"}'),
		('`+room+`', 20004, 'd1', '7f3cq2xz', 'human', 'human:own', 'approval_decided', 'client', 'human:own:s1', 3, now(),
		 '{"approvalId":"ap1","decision":"approved"}'),
		('`+room+`', 20005, 'a2', '7f3cq2xz', 'agent', 'agent:7f3cq2xz', 'state_changed', 'harness', 'agent:7f3cq2xz:status', 2, now(),
		 '{"kind":"decision_applied","ref":20004,"runId":"7f3cq2xz"}'),
		('`+room+`', 20006, 't1', '7f3cq2xz', 'agent', 'agent:7f3cq2xz', 'tool_result', 'harness', 'agent:7f3cq2xz', 20001, now(),
		 '{"callId":"c1","status":"ok"}');
		UPDATE rooms SET last_seq = 20006 WHERE room_id = '`+room+`';
		ANALYZE events;`)
	for _, c := range []struct {
		name, sql string
		indexes   []string
		args      []any
	}{
		// One OR arm per index: the deliveries and the decisions, the acks and the decision acks.
		{"deliveries", deliveriesSQL, []string{"events_deliveries", "events_decisions"}, []any{room, int64(0), int64(20006), "7f3cq2xz", 500}},
		{"last ack", lastAckSQL, []string{"events_acks", "events_decision_acks"}, []any{room, "7f3cq2xz"}},
		{"answered", `SELECT 1 FROM (SELECT '` + room + `'::text AS room_id, '7f3cq2xz'::text AS run_id, 'c1'::text AS call_id,
			0::bigint AS requested_seq) a WHERE ` + answeredSQL, []string{"events_tool_results"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			text := plan(t, s, c.sql, c.args...)
			for _, index := range c.indexes {
				if !strings.Contains(text, index) || strings.Contains(text, "Seq Scan on events") {
					t.Fatalf("%s does not read through %s:\n%s", c.name, index, text)
				}
			}
		})
	}
	if got, err := s.Deliveries(t.Context(), room, "7f3cq2xz", 0, 20006, 500); err != nil || len(got) != 3 {
		t.Fatalf("the replay reads %d events, %v; want the 2 deliveries and the decision", len(got), err)
	}
	if n, err := s.LastAck(t.Context(), room, "7f3cq2xz"); err != nil || n != 20004 {
		t.Fatalf("last ack = %d, %v", n, err)
	}
}
