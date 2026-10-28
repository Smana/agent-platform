// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Smana/agent-platform/internal/store/migrations"
)

// migrateLock is the advisory lock id room-broker migrate holds: two replicas' init
// containers apply each file once.
const migrateLock = 0x726f6f6d73 // "rooms"

// ownerRole is the role the migrations run as, and the only one they may: every object they
// create belongs to it, so the broker's own role can neither alter the schema nor drop an index
// (SC-10). A DSN for any other role is refused before a single statement runs.
const ownerRole = "rooms_owner"

// Migrate applies the embedded migrations in order, recording each file's SHA-256. A file whose
// checksum changed after it was applied, or a recorded version this binary does not ship, is
// refused: the database is ahead of or different from the code. It returns how many it applied.
func Migrate(ctx context.Context, dsn string, log *slog.Logger) (int, error) {
	files, err := migrations.Files()
	if err != nil {
		return 0, fmt.Errorf("migrate: %w", err)
	}
	return migrate(ctx, dsn, files, log)
}

// migrate is Migrate over an explicit file list. Tests inject a truncated catalog, a tampered
// file or a deliberately broken one through it instead of editing the embedded files.
func migrate(ctx context.Context, dsn string, files []migrations.File, log *slog.Logger) (int, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return 0, fmt.Errorf("migrate: connect: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	// The role first: nothing below may touch the schema as anyone but its owner.
	if err := requireOwner(ctx, conn); err != nil {
		return 0, err
	}
	if err := lock(ctx, conn, log); err != nil {
		return 0, err
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrateLock) }()
	// An invalid index of an embedded txmode-none file is the residue of a build that did not
	// finish, and CREATE INDEX CONCURRENTLY would leave it there while the log grows.
	if err := refuseInvalidIndexes(ctx, conn, files); err != nil {
		return 0, err
	}
	applied, err := recorded(ctx, conn)
	if err != nil {
		return 0, err
	}
	pending, err := validate(files, applied)
	if err != nil {
		return 0, err
	}
	if _, err := conn.Exec(ctx, createRevisions); err != nil {
		return 0, fmt.Errorf("migrate: revisions table: %w", err)
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

// requireOwner refuses a connection whose role is not the schema's owner. Every migration would
// fail on it a statement later, having left whatever the earlier files created behind; and a
// ledger written by a role that does not own the migrations hides which side is wrong.
func requireOwner(ctx context.Context, conn *pgx.Conn) error {
	var user string
	if err := conn.QueryRow(ctx, "SELECT current_user").Scan(&user); err != nil {
		return fmt.Errorf("migrate: current_user: %w", err)
	}
	if user != ownerRole {
		return fmt.Errorf("migrate: connected as %q: the migrations run as %s, which owns every table, "+
			"policy and index they create, so give the migrate DSN %s's credentials", user, ownerRole, ownerRole)
	}
	return nil
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
			return fmt.Errorf("migrate: lock: %w", ctx.Err())
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
			return nil, fmt.Errorf("migrate: %s (version %s) changed after it was applied: recorded checksum %s, now %s",
				f.Name, f.Version, c, sum)
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

const createRevisions = `CREATE TABLE IF NOT EXISTS room_broker_revisions (
	version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`

// recorded reads the ledger, which a database that was never migrated does not have yet: an
// absent table is an empty history, never an error.
func recorded(ctx context.Context, conn *pgx.Conn) (map[string]string, error) {
	applied := map[string]string{}
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('public.room_broker_revisions') IS NOT NULL").Scan(&exists); err != nil {
		return nil, fmt.Errorf("migrate: revisions: %w", err)
	}
	if !exists {
		return applied, nil
	}
	rows, err := conn.Query(ctx, "SELECT version, checksum FROM room_broker_revisions")
	if err != nil {
		return nil, fmt.Errorf("migrate: revisions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v, c string
		if err := rows.Scan(&v, &c); err != nil {
			return nil, fmt.Errorf("migrate: revisions: %w", err)
		}
		applied[v] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: revisions: %w", err)
	}
	return applied, nil
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
	for _, stmt := range splitTxModeNoneStatements(f.Body) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%w (a txmode-none file is not resumable: DROP every index this file created, an INVALID one included, then re-run)", err)
		}
	}
	_, err := conn.Exec(context.WithoutCancel(ctx), recordSQL, f.Version, sum)
	return err
}

// splitTxModeNoneStatements splits a txmode-none migration into its statements, every one of
// them: a GRANT or a COMMENT skipped here would pass the tests and still run on
// the cluster. These files hold no function body, so once the comment lines are
// gone a semicolon ends each statement.
func splitTxModeNoneStatements(sql string) []string {
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

// concurrentIndexName is a CREATE INDEX CONCURRENTLY naming its index without a schema: the
// captured group is the identifier, and a qualified name (public.events_x) captures nothing, so
// an index another schema or another tool creates is never in the list below.
var concurrentIndexName = regexp.MustCompile(
	`(?is)^CREATE\s+(?:UNIQUE\s+)?INDEX\s+CONCURRENTLY\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_$]*)\s*(?:\(|ON\b)`)

// txModeNoneIndexNames is the ownership catalog of the invalid-index preflight: the unqualified
// index names the embedded txmode-none files create, unique and sorted. It never scans the
// catalog of a live database, so an index outside it (a third party's, an operator's) is neither
// read nor refused.
func txModeNoneIndexNames(files []migrations.File) []string {
	seen := map[string]struct{}{}
	var names []string
	for _, f := range files {
		if !f.NoTx {
			continue
		}
		for _, stmt := range splitTxModeNoneStatements(f.Body) {
			m := concurrentIndexName.FindStringSubmatch(stmt)
			if m == nil {
				continue
			}
			if _, dup := seen[m[1]]; dup {
				continue
			}
			seen[m[1]] = struct{}{}
			names = append(names, m[1])
		}
	}
	sort.Strings(names)
	return names
}

// invalidIndexesSQL reads the invalid indexes of the owned list only: the list is a parameter, so
// the query cannot see an index the embedded files do not create.
const invalidIndexesSQL = `SELECT c.relname FROM pg_index i
	JOIN pg_class c ON c.oid = i.indexrelid
	JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname = 'public' AND NOT i.indisvalid AND c.relname = ANY($1::text[])
	ORDER BY c.relname`

// refuseInvalidIndexes stops a run whose owned index a half-finished concurrent build left
// invalid, and names the one statement that recovers each: dropping the invalid index is the
// operator's, so a rerun rebuilds it from its file and records the revision. It prescribes
// nothing about an index outside the embedded files.
func refuseInvalidIndexes(ctx context.Context, conn *pgx.Conn, files []migrations.File) error {
	owned := txModeNoneIndexNames(files)
	if len(owned) == 0 {
		return nil
	}
	rows, err := conn.Query(ctx, invalidIndexesSQL, owned)
	if err != nil {
		return fmt.Errorf("migrate: invalid indexes: %w", err)
	}
	defer rows.Close()
	var broken []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("migrate: invalid indexes: %w", err)
		}
		broken = append(broken, name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("migrate: invalid indexes: %w", err)
	}
	if len(broken) == 0 {
		return nil
	}
	var fixes strings.Builder
	for _, name := range broken {
		fixes.WriteString("\n  DROP INDEX CONCURRENTLY public." + name + ";")
	}
	return fmt.Errorf("migrate: %s invalid: a CREATE INDEX CONCURRENTLY of an embedded txmode-none migration did not finish. "+
		"As %s, drop it, then re-run; a file already recorded rebuilds nothing until its revision is deleted too:%s",
		strings.Join(broken, ", "), ownerRole, fixes.String())
}
