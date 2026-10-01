// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ListenApplication is the listener connection's application_name, so an
// operator can find it in pg_stat_activity.
const ListenApplication = "room-broker-listen"

// notifyChannel carries "<roomId> <last_seq>": activity metadata only, never
// event content, because any role connected to the database may LISTEN.
const notifyChannel = "rooms_events"

// notify signals the room's new last seq inside the caller's transaction.
// PostgreSQL delivers it on commit only, in commit order, and never for a
// rollback: an append and its notification cannot disagree (Ruling AT).
func notify(ctx context.Context, tx pgx.Tx, roomID string, seq int64) error {
	_, err := tx.Exec(ctx, `SELECT pg_notify('`+notifyChannel+`', $1)`, roomID+" "+strconv.FormatInt(seq, 10))
	return err
}

// Listen holds one LISTEN session on a dedicated connection, outside the pool,
// so neither starves the other. It calls ready once LISTEN is in place, then
// fn for every notification, from its own goroutine: fn must not block, or
// PostgreSQL's notification queue backs up and every append fails. It returns
// nil when ctx ends and an error when the connection is lost; the caller
// reconnects and catches up, since notifications sent meanwhile are gone.
func (s *Store) Listen(ctx context.Context, ready func(), fn func(roomID string, seq int64)) error {
	cfg := s.pool.Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = ListenApplication
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return listenErr(ctx, err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = conn.Close(cctx)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return listenErr(ctx, err)
	}
	ready()
	for {
		wctx, cancel := context.WithTimeout(ctx, s.ListenPing)
		n, err := conn.WaitForNotification(wctx)
		cancel()
		switch {
		case err == nil:
			room, seq, ok := strings.Cut(n.Payload, " ")
			if v, perr := strconv.ParseInt(seq, 10, 64); ok && perr == nil {
				fn(room, v)
			}
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err):
			// A quiet spell. The timeout leaves the connection usable; a failed
			// ping means a half-open one after a restart or a failover.
			pctx, cancel := context.WithTimeout(ctx, s.ListenPing)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return listenErr(ctx, err)
			}
		default:
			return listenErr(ctx, err)
		}
	}
}

// listenErr is nil once ctx has ended: shutting down is not a lost connection.
func listenErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("store: listen: %w", err)
}
