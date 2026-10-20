// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Smana/agent-platform/internal/store/migrations"
)

// migrateLock is the advisory lock id room-broker migrate holds: two replicas' init
// containers apply each file once.
const migrateLock = 0x726f6f6d73 // "rooms"

// Migrate applies the embedded migrations in order, recording each file's SHA-256. A file whose
// checksum changed after it was applied, or a recorded version this binary does not ship, is
// refused: the database is ahead of or different from the code. It returns how many it applied.
func Migrate(ctx context.Context, dsn string, log *slog.Logger) (int, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return 0, fmt.Errorf("migrate: connect: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if err := lock(ctx, conn, log); err != nil {
		return 0, err
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrateLock) }()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS room_broker_revisions (
		version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return 0, fmt.Errorf("migrate: revisions table: %w", err)
	}
	applied := map[string]string{}
	rows, err := conn.Query(ctx, "SELECT version, checksum FROM room_broker_revisions")
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var v, c string
		if err := rows.Scan(&v, &c); err != nil {
			rows.Close()
			return 0, err
		}
		applied[v] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	files, err := migrations.Files()
	if err != nil {
		return 0, err
	}
	pending, err := validate(files, applied)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, f := range pending {
		if err := apply(ctx, conn, f); err != nil {
			return n, fmt.Errorf("migrate: %s: %w", f.Name, err)
		}
		log.Info("migration applied", "file", f.Name)
		n++
	}
	return n, nil
}

// lock takes the advisory lock by polling. A blocking pg_advisory_lock would leave the waiter
// inside a statement holding a snapshot, which the holder's CREATE INDEX CONCURRENTLY waits out
// while the waiter waits on the lock: a deadlock the detector resolves by aborting a build.
func lock(ctx context.Context, conn *pgx.Conn, log *slog.Logger) error {
	waited := false
	for {
		var got bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", migrateLock).Scan(&got); err != nil {
			return fmt.Errorf("migrate: lock: %w", err)
		}
		if got {
			return nil
		}
		if !waited {
			log.Info("waiting for another migrator")
			waited = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// validate checks the whole history before anything is applied and returns the pending files:
// a changed checksum, a recorded version this binary does not ship, or a pending file older than
// the newest applied one (Atlas's linear history) is refused with nothing changed.
func validate(files []migrations.File, applied map[string]string) ([]migrations.File, error) {
	shipped := map[string]bool{}
	var pending []migrations.File
	for _, f := range files {
		shipped[f.Version] = true
		c, ok := applied[f.Version]
		if !ok {
			pending = append(pending, f)
			continue
		}
		if sum := checksum(f.Body); c != sum {
			return nil, fmt.Errorf("migrate: %s changed after it was applied (recorded %s, now %s)", f.Name, c, sum)
		}
	}
	newest := ""
	for v := range applied {
		if !shipped[v] {
			return nil, fmt.Errorf("migrate: the database has version %s, which this binary does not ship: it is newer than the code", v)
		}
		newest = max(newest, v)
	}
	for _, f := range pending {
		if f.Version < newest {
			return nil, fmt.Errorf("migrate: %s is pending but version %s is already applied: history must stay linear", f.Name, newest)
		}
	}
	return pending, nil
}

func checksum(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

const recordSQL = "INSERT INTO room_broker_revisions (version, checksum) VALUES ($1, $2)"

// apply runs a file and records it. A transactional file records inside its own transaction, so a
// SIGTERM or a lost COMMIT ack cannot leave it applied but unrecorded (no file is idempotent). A
// txmode-none file (Atlas's, for CREATE INDEX CONCURRENTLY) runs a statement at a time and records
// under a context a SIGTERM cannot cancel.
func apply(ctx context.Context, conn *pgx.Conn, f migrations.File) error {
	sum := checksum(f.Body)
	if !f.NoTx {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, f.Body); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if _, err := tx.Exec(ctx, recordSQL, f.Version, sum); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		return tx.Commit(ctx)
	}
	for _, stmt := range statements(f.Body) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%w (a txmode-none file is not resumable: DROP every index this file created, an INVALID one included, then re-run)", err)
		}
	}
	_, err := conn.Exec(context.WithoutCancel(ctx), recordSQL, f.Version, sum)
	return err
}

// statements splits a txmode-none migration into its statements, every one of
// them: a GRANT or a COMMENT skipped here would pass the tests and still run on
// the cluster. These files hold no function body, so once the comment lines are
// gone a semicolon ends each statement.
func statements(sql string) []string {
	var code strings.Builder
	for line := range strings.Lines(sql) {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			code.WriteString(line)
		}
	}
	var out []string
	for stmt := range strings.SplitSeq(code.String(), ";") {
		if stmt = strings.TrimSpace(stmt); stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}
