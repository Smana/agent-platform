// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"

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
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLock); err != nil {
		return 0, fmt.Errorf("migrate: lock: %w", err)
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
	shipped := map[string]bool{}
	n := 0
	for _, f := range files {
		shipped[f.Version] = true
		sum := sha256.Sum256([]byte(f.Body))
		checksum := hex.EncodeToString(sum[:])
		if c, ok := applied[f.Version]; ok {
			if c != checksum {
				return n, fmt.Errorf("migrate: %s changed after it was applied (recorded %s, now %s)", f.Name, c, checksum)
			}
			continue
		}
		if err := apply(ctx, conn, f); err != nil {
			return n, fmt.Errorf("migrate: %s: %w", f.Name, err)
		}
		if _, err := conn.Exec(ctx, "INSERT INTO room_broker_revisions (version, checksum) VALUES ($1, $2)", f.Version, checksum); err != nil {
			return n, err
		}
		log.Info("migration applied", "file", f.Name)
		n++
	}
	for v := range applied {
		if !shipped[v] {
			return n, fmt.Errorf("migrate: the database has version %s, which this binary does not ship: it is newer than the code", v)
		}
	}
	return n, nil
}

// apply runs a file in one transaction, or statement by statement when it opts out (Atlas's
// txmode none, for CREATE INDEX CONCURRENTLY).
func apply(ctx context.Context, conn *pgx.Conn, f migrations.File) error {
	if !f.NoTx {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, f.Body); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		return tx.Commit(ctx)
	}
	for _, stmt := range statements(f.Body) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%w (a txmode-none file can leave an INVALID index: DROP it, then re-run)", err)
		}
	}
	return nil
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
