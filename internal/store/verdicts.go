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

// agentVerdicts selects room_verdict's writes with no outcome yet, in rooms that
// can still record one (review M5): origin client, the run's own tools scope,
// and a role that holds the tool. $1 is VerdictsClient.
const agentVerdicts = ` FROM events e JOIN rooms r USING (room_id)
		WHERE e.type = 'message' AND e.actor_kind = 'agent' AND e.payload->>'kind' = 'review_verdict'
		  AND e.origin = 'client' AND e.origin_client = 'agent:' || e.run_id || ':tools'
		  AND e.actor_role IN ('reviewer', 'tester') AND NOT r.sealed
		  AND NOT EXISTS (SELECT 1 FROM events p
		                  WHERE p.room_id = e.room_id AND p.origin_client = $1 AND p.origin_seq = e.seq)`

// UnpostedVerdicts are agents' review verdicts after since, oldest first, with
// no outcome yet (SP2 design §3), leaving out the event ids in exclude: the
// verdicts the poster holds in backoff, so they never starve the next ones.
func (s *Store) UnpostedVerdicts(ctx context.Context, since time.Time, limit int, exclude []string) ([]envelope.Event, error) {
	if exclude == nil {
		exclude = []string{} // NULL would make ANY unknown and hide every row
	}
	return s.verdicts(ctx, "unposted verdicts", `SELECT e.room_id, e.seq`+agentVerdicts+`
		  AND e.ts > $2 AND NOT (e.id = ANY($3))
		ORDER BY e.ts, e.room_id, e.seq LIMIT $4`, VerdictsClient, since, exclude, limit)
}

// ExpiredVerdicts are the verdicts UnpostedVerdicts no longer returns: stamped
// at or before before, oldest first, with no outcome yet (review 3.5 m2b).
func (s *Store) ExpiredVerdicts(ctx context.Context, before time.Time, limit int) ([]envelope.Event, error) {
	return s.verdicts(ctx, "expired verdicts", `SELECT e.room_id, e.seq`+agentVerdicts+`
		  AND e.ts <= $2
		ORDER BY e.ts, e.room_id, e.seq LIMIT $3`, VerdictsClient, before, limit)
}

// verdicts runs a query of (room_id, seq) keys and returns their events.
func (s *Store) verdicts(ctx context.Context, what, query string, args ...any) ([]envelope.Event, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: %s: %w", what, err)
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
		return nil, fmt.Errorf("store: %s: %w", what, err)
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
