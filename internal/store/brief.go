// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
)

// briefSourcesSQL reads the room's latest agent handoff and latest agent
// review_verdict at or before seq $2, each a backward scan of events_brief that
// stops at its first row. Each branch's predicate implies the index's.
var briefSourcesSQL = `(SELECT ` + cols + ` FROM events e WHERE e.room_id = $1 AND e.seq <= $2 AND e.type = 'handoff'
		AND e.actor_kind = 'agent' ORDER BY e.seq DESC LIMIT 1)
	UNION ALL
	(SELECT ` + cols + ` FROM events e WHERE e.room_id = $1 AND e.seq <= $2 AND e.type = 'message' AND e.actor_kind = 'agent'
		AND e.payload->>'kind' = 'review_verdict' ORDER BY e.seq DESC LIMIT 1)
	ORDER BY seq`

// BriefSources returns what the next run's brief may quote from the log: the
// room's latest agent handoff and latest agent review_verdict, in seq order
// (review 4.4 M3 and I3). Only agents wrote them; no window of the log is read.
func (s *Store) BriefSources(ctx context.Context, roomID string) ([]envelope.Event, error) {
	return s.BriefSourcesThrough(ctx, roomID, math.MaxInt64)
}

// BriefSourcesThrough is BriefSources as the log stood at seq: what a fork at
// seq bases its run on (§5).
func (s *Store) BriefSourcesThrough(ctx context.Context, roomID string, seq int64) ([]envelope.Event, error) {
	rows, err := s.pool.Query(ctx, briefSourcesSQL, roomID, seq)
	if err != nil {
		return nil, fmt.Errorf("store: brief sources of room %s: %w", roomID, err)
	}
	defer rows.Close()
	var out []envelope.Event
	for rows.Next() {
		ev, err := scan(rows, roomID)
		if err != nil {
			return nil, fmt.Errorf("store: brief sources of room %s: %w", roomID, err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// pendingRunsSQL lists the run ids of run_requested records younger than $2
// seconds that no later participant event names as agent:<runId>, both through
// events_brief.
const pendingRunsSQL = `SELECT r.payload->>'runId' FROM events r
	WHERE r.room_id = $1 AND r.type = 'state_changed' AND r.payload->>'kind' = 'run_requested'
	AND r.ts > now() - make_interval(secs => $2)
	AND NOT EXISTS (SELECT 1 FROM events p WHERE p.room_id = r.room_id AND p.seq > r.seq AND p.type = 'participant'
		AND p.payload->>'principal' = 'agent:' || (r.payload->>'runId'))
	ORDER BY r.seq`

// PendingRuns lists the runs requested in the room within the last within whose
// run has not joined yet (review 4.4 I2): a rendered claim not applied, or a
// factory run the watch has not seen.
func (s *Store) PendingRuns(ctx context.Context, roomID string, within time.Duration) ([]string, error) {
	rows, err := s.pool.Query(ctx, pendingRunsSQL, roomID, within.Seconds())
	if err != nil {
		return nil, fmt.Errorf("store: pending runs of room %s: %w", roomID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: pending runs of room %s: %w", roomID, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
