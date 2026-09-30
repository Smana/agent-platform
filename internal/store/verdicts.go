// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Smana/agent-platform/internal/envelope"
)

// VerdictsClient is the origin of the leader's verdict_posted and
// verdict_not_posted records. Their origin_seq is the verdict's seq, so the
// idempotency key is the record, and a new leader writes nothing twice.
const VerdictsClient = "broker:verdicts"

// UnpostedVerdicts are agents' review verdicts since `since`, oldest first, in
// rooms that can still record an outcome, with no outcome yet (SP2 design §3).
// Only room_verdict's writes count (review M5): origin client, the run's own
// tools scope, and a role that holds the tool.
func (s *Store) UnpostedVerdicts(ctx context.Context, since time.Time, limit int) ([]envelope.Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT e.room_id, e.seq FROM events e JOIN rooms r USING (room_id)
		WHERE e.type = 'message' AND e.actor_kind = 'agent' AND e.payload->>'kind' = 'review_verdict'
		  AND e.origin = 'client' AND e.origin_client = 'agent:' || e.run_id || ':tools'
		  AND e.actor_role IN ('reviewer', 'tester')
		  AND e.ts > $1 AND NOT r.sealed
		  AND NOT EXISTS (SELECT 1 FROM events p
		                  WHERE p.room_id = e.room_id AND p.origin_client = $2 AND p.origin_seq = e.seq)
		ORDER BY e.ts, e.room_id, e.seq LIMIT $3`, since, VerdictsClient, limit)
	if err != nil {
		return nil, fmt.Errorf("store: unposted verdicts: %w", err)
	}
	type key struct {
		room string
		seq  int64
	}
	keys, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (key, error) {
		var k key
		return k, r.Scan(&k.room, &k.seq)
	})
	if err != nil {
		return nil, fmt.Errorf("store: unposted verdicts: %w", err)
	}
	out := make([]envelope.Event, 0, len(keys))
	for _, k := range keys {
		evs, err := s.Range(ctx, k.room, k.seq-1, 1)
		if err != nil {
			return nil, err
		}
		out = append(out, evs...)
	}
	return out, nil
}
