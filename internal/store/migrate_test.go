// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Smana/maku/internal/store/migrations"
)

// A database that has never been migrated reaches the current head, every file's checksum and
// applied_at are recorded, the broker's readiness probe agrees, and a second run changes nothing.
func TestMigrateEmptyToHeadAndRerun(t *testing.T) {
	ctx, log := t.Context(), slog.New(slog.DiscardHandler)
	dsn := emptyOwnedDB(t)
	files, err := migrations.Files()
	if err != nil {
		t.Fatal(err)
	}
	n, err := Migrate(ctx, dsn, log)
	if err != nil || n != len(files) {
		t.Fatalf("first run applied %d of %d files, err %v", n, len(files), err)
	}
	if got := query(t, dsn, "SELECT (to_regclass('public.events') IS NULL)::text"); got != "false" {
		t.Fatal("the first file's events table is missing")
	}
	// Every file is recorded with its own bytes, so a later tamper of either side is caught.
	for _, f := range files {
		if got := query(t, dsn, "SELECT checksum FROM room_broker_revisions WHERE version = $1", f.Version); got != checksum(f.Body) {
			t.Fatalf("%s: recorded checksum %q, want %q", f.Name, got, checksum(f.Body))
		}
	}
	// Every index the embedded txmode-none files create exists and finished building.
	owned := txModeNoneIndexNames(files)
	if got := query(t, dsn, `SELECT count(*)::text FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND i.indisvalid AND c.relname = ANY($1::text[])`, owned); got != strconv.Itoa(len(owned)) {
		t.Fatalf("%s of %d owned indexes are valid", got, len(owned))
	}
	if got := query(t, dsn, `SELECT coalesce(bool_or(i.indisvalid), false)::text FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = 'events_agent_verdicts'`); got != "true" {
		t.Fatal("the verdicts index, the first txmode-none file's, is not a valid index")
	}
	// The ledger is the owner's: no migration grants the broker's role anything on it.
	if got := query(t, dsn, "SELECT has_table_privilege('rooms_broker', 'public.room_broker_revisions', 'SELECT,INSERT,UPDATE,DELETE')::text"); got != "false" {
		t.Fatal("rooms_broker holds a privilege on room_broker_revisions")
	}
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if ready, err := st.SchemaReady(ctx); err != nil || !ready {
		t.Fatalf("SchemaReady = %v, %v, want the schema ready", ready, err)
	}

	before := query(t, dsn, "SELECT string_agg(version || '@' || applied_at::text, ',' ORDER BY version) FROM room_broker_revisions")
	if n, err := Migrate(ctx, dsn, log); err != nil || n != 0 {
		t.Fatalf("second run must be a no-op: %d %v", n, err)
	}
	after := query(t, dsn, "SELECT string_agg(version || '@' || applied_at::text, ',' ORDER BY version) FROM room_broker_revisions")
	if before != after {
		t.Fatalf("the rerun rewrote the ledger:\n%s\n%s", before, after)
	}
}

// A recorded checksum that no longer matches the file, whether the ledger or the embedded bytes
// changed, is refused by version and checksum before any pending file runs.
func TestMigrateRefusesTamperedRevision(t *testing.T) {
	ctx, log := t.Context(), slog.New(slog.DiscardHandler)
	files, err := migrations.Files()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("the ledger's checksum changed", func(t *testing.T) {
		dsn := emptyOwnedDB(t)
		if _, err := Migrate(ctx, dsn, log); err != nil {
			t.Fatal(err)
		}
		exec(t, dsn, "UPDATE room_broker_revisions SET checksum = repeat('0', 64) WHERE version = $1", files[0].Version)
		exec(t, dsn, "DELETE FROM room_broker_revisions WHERE version = $1", files[len(files)-1].Version)
		_, err := Migrate(ctx, dsn, log)
		for _, want := range []string{files[0].Version, "checksum"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("tampered checksum: err = %v, want it to name %q", err, want)
			}
		}
		// Nothing ran: the deleted revision is still missing and its file's tables are absent.
		if got := query(t, dsn, "SELECT count(*)::text FROM room_broker_revisions WHERE version = $1", files[len(files)-1].Version); got != "0" {
			t.Fatalf("a file ran after a tampered revision: %s recorded", got)
		}
	})
	t.Run("the embedded file changed", func(t *testing.T) {
		dsn := emptyOwnedDB(t)
		if _, err := migrate(ctx, dsn, files[:1], log); err != nil {
			t.Fatal(err)
		}
		tampered := slices.Clone(files)
		tampered[0].Body += "\n-- tampered\n"
		if _, err := migrate(ctx, dsn, tampered, log); err == nil || !strings.Contains(err.Error(), files[0].Version) {
			t.Fatalf("a changed embedded file must be refused by version: %v", err)
		}
		if got := query(t, dsn, "SELECT count(*)::text FROM room_broker_revisions"); got != "1" {
			t.Fatalf("a later file ran after the refusal: %s revisions", got)
		}
		if got := query(t, dsn, "SELECT (to_regclass('public.queue') IS NULL)::text"); got != "true" {
			t.Fatal("a pending file ran after the tampered one was refused")
		}
	})
}

// A database ahead of the code, or a pending file older than an applied one, is refused before
// anything is applied.
func TestMigrateRefusesUnshippedAndOutOfOrder(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	dsn := emptyOwnedDB(t)
	if _, err := Migrate(t.Context(), dsn, log); err != nil {
		t.Fatal(err)
	}
	exec(t, dsn, "INSERT INTO room_broker_revisions (version, checksum) VALUES ('29990101000000', repeat('0', 64))")
	if _, err := Migrate(t.Context(), dsn, log); err == nil || !strings.Contains(err.Error(), "29990101000000") {
		t.Fatalf("a recorded version the binary does not ship must be refused by name: %v", err)
	}

	dsn = emptyOwnedDB(t)
	if _, err := Migrate(t.Context(), dsn, log); err != nil {
		t.Fatal(err)
	}
	// The first file becomes pending while later ones stay applied.
	exec(t, dsn, "DELETE FROM room_broker_revisions WHERE version = (SELECT min(version) FROM room_broker_revisions)")
	if n, err := Migrate(t.Context(), dsn, log); err == nil || n != 0 || !strings.Contains(err.Error(), "history must stay linear") {
		t.Fatalf("an out-of-order pending file must be refused with nothing applied: %d %v", n, err)
	}
}

// Only the schema's owner migrates: the broker's and the retention job's DSNs are refused before
// the ledger or any table exists, so their credentials never own a migration.
func TestMigrateRequiresOwner(t *testing.T) {
	ctx, log := t.Context(), slog.New(slog.DiscardHandler)
	owner, super := emptyOwnedDBWithSuper(t)
	for _, c := range []struct{ name, dsn string }{
		{"broker", as(owner, "rooms_broker", "broker")},
		{"retention", as(owner, "rooms_retention", "retention")},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Migrate(ctx, c.dsn, log)
			if err == nil || !strings.Contains(err.Error(), "rooms_owner") {
				t.Fatalf("migrate as rooms_%s: err = %v, want a refusal naming rooms_owner", c.name, err)
			}
			for _, table := range []string{"public.room_broker_revisions", "public.events"} {
				if got := query(t, super, "SELECT (to_regclass($1) IS NULL)::text", table); got != "true" {
					t.Fatalf("migrate as rooms_%s created %s", c.name, table)
				}
			}
		})
	}
}

// A transactional file that fails after its first statement leaves neither its objects nor its
// revision, and its error names the file.
func TestMigrateTransactionalFileRollsBack(t *testing.T) {
	ctx, log := t.Context(), slog.New(slog.DiscardHandler)
	owner, super := emptyOwnedDBWithSuper(t)
	broken := migrations.File{
		Name: "29990101000000_probe.sql", Version: "29990101000000",
		// The division by zero fails while the file runs, after the table exists: a parse-time
		// error would prove nothing about the transaction.
		Body: "CREATE TABLE migration_probe (id int);\nSELECT 1/0;\n",
	}
	if _, err := migrate(ctx, owner, []migrations.File{broken}, log); err == nil || !strings.Contains(err.Error(), broken.Name) {
		t.Fatalf("a failing file must be refused by name: %v", err)
	}
	if got := query(t, super, "SELECT (to_regclass('public.migration_probe') IS NULL)::text"); got != "true" {
		t.Fatal("the failed file's table survived the rollback")
	}
	if got := query(t, super, "SELECT count(*)::text FROM room_broker_revisions"); got != "0" {
		t.Fatalf("%s revisions survived a failed file", got)
	}
}

// Every statement of a txmode-none file runs, whatever its verb (review 4.3).
func TestMigrateTxModeNoneStatements(t *testing.T) {
	got := splitTxModeNoneStatements(`-- atlas:txmode none

-- a comment; with a semicolon
CREATE INDEX CONCURRENTLY a ON events (room_id);
GRANT SELECT ON events TO rooms_broker;
  COMMENT ON INDEX a IS 'x';
REINDEX INDEX CONCURRENTLY a;
`)
	want := []string{"CREATE INDEX CONCURRENTLY a ON events (room_id)", "GRANT SELECT ON events TO rooms_broker",
		"COMMENT ON INDEX a IS 'x'", "REINDEX INDEX CONCURRENTLY a"}
	if !slices.Equal(got, want) {
		t.Fatalf("statements = %q, want %q", got, want)
	}
}

// The ownership catalog is exactly the indexes the embedded txmode-none files build: no index of
// a transactional file, no name read from a live database.
func TestTxModeNoneIndexOwnership(t *testing.T) {
	files, err := migrations.Files()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"approvals_pending", "events_acks", "events_agent_verdicts", "events_brief",
		"events_decision_acks", "events_decisions", "events_deliveries", "events_task_facts", "events_tool_results"}
	if got := txModeNoneIndexNames(files); !slices.Equal(got, want) {
		t.Fatalf("owned indexes = %q, want %q", got, want)
	}
	inTx := migrations.File{Name: "29990101000000_tx.sql", Version: "29990101000000",
		Body: "CREATE INDEX CONCURRENTLY not_owned ON events (room_id);"}
	if got := txModeNoneIndexNames([]migrations.File{inTx}); len(got) != 0 {
		t.Fatalf("a transactional file's index is not owned: %q", got)
	}
}

// An invalid index of an embedded txmode-none file is refused with the one statement that
// recovers it; the operator drops it, the rerun rebuilds it and records its revision.
func TestMigrateRefusesInvalidIndex(t *testing.T) {
	ctx, log := t.Context(), slog.New(slog.DiscardHandler)
	dsn, super := emptyOwnedDBWithSuper(t)
	files, err := migrations.Files()
	if err != nil {
		t.Fatal(err)
	}
	// The state a concurrent build leaves when it does not finish: the index exists, its revision
	// does not. The first file only, so the rest is genuinely pending.
	if _, err := migrate(ctx, dsn, files[:1], log); err != nil {
		t.Fatal(err)
	}
	exec(t, dsn, `CREATE INDEX CONCURRENTLY events_agent_verdicts ON events (ts)
		WHERE type = 'message' AND actor_kind = 'agent' AND payload->>'kind' = 'review_verdict'`)
	exec(t, super, "UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'public.events_agent_verdicts'::regclass")

	_, err = Migrate(ctx, dsn, log)
	for _, want := range []string{"events_agent_verdicts", "DROP INDEX CONCURRENTLY public.events_agent_verdicts;"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("invalid index: err = %v, want it to name %q", err, want)
		}
	}
	// The refusal came before every later file: the driver queue's table is absent and the ledger
	// still holds one version.
	if got := query(t, dsn, "SELECT (to_regclass('public.queue') IS NULL)::text"); got != "true" {
		t.Fatal("a later file ran before the invalid index was refused")
	}
	if got := query(t, dsn, "SELECT count(*)::text FROM room_broker_revisions"); got != "1" {
		t.Fatalf("a later file was recorded: %s revisions", got)
	}

	// The recovery the error prints, then the rerun the operator is told to do.
	exec(t, dsn, "DROP INDEX CONCURRENTLY public.events_agent_verdicts")
	if n, err := Migrate(ctx, dsn, log); err != nil || n != len(files)-1 {
		t.Fatalf("the rerun applied %d of %d pending files, err %v", n, len(files)-1, err)
	}
	if got := query(t, dsn, `SELECT i.indisvalid::text FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = 'events_agent_verdicts'`); got != "true" {
		t.Fatalf("events_agent_verdicts is valid = %s after the rerun", got)
	}
	if got := query(t, dsn, "SELECT count(*)::text FROM room_broker_revisions"); got != strconv.Itoa(len(files)) {
		t.Fatalf("the rerun recorded %s revisions, want %d", got, len(files))
	}
}

// An invalid index the embedded files never create is neither read nor refused, and migrate leaves
// it exactly as it found it.
func TestMigrateIgnoresUnrelatedInvalidIndex(t *testing.T) {
	ctx, log := t.Context(), slog.New(slog.DiscardHandler)
	owner, _, _, super := testDB(t)
	exec(t, owner, "CREATE TABLE third_party (id int)")
	exec(t, owner, "CREATE INDEX CONCURRENTLY third_party_invalid ON third_party (id)")
	exec(t, super, "UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'public.third_party_invalid'::regclass")

	if n, err := Migrate(ctx, owner, log); err != nil || n != 0 {
		t.Fatalf("an unrelated invalid index must not stop the migration: %d %v", n, err)
	}
	if got := query(t, super, `SELECT i.indisvalid::text FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = 'third_party_invalid'`); got != "false" {
		t.Fatalf("the third party's index was touched: valid = %s", got)
	}
}

// Two init containers on one database: the advisory lock serialises them, each file is applied
// and recorded once, and both return nil.
func TestMigrateConcurrentRunsApplyOnce(t *testing.T) {
	ctx, log := t.Context(), slog.New(slog.DiscardHandler)
	dsn := emptyOwnedDB(t)
	var wg sync.WaitGroup
	total := make([]int, 2)
	for i := range 2 {
		wg.Go(func() {
			n, err := Migrate(ctx, dsn, log)
			if err != nil {
				t.Error(err)
			}
			total[i] = n
		})
	}
	wg.Wait()
	if want := migrationCount(t); total[0]+total[1] != want {
		t.Fatalf("each file applied once: %v, want %d together", total, want)
	}
	if got := query(t, dsn, "SELECT count(*)::text FROM room_broker_revisions"); got != strconv.Itoa(migrationCount(t)) {
		t.Fatalf("revisions = %s, want %d", got, migrationCount(t))
	}
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if ready, err := st.SchemaReady(ctx); err != nil || !ready {
		t.Fatalf("SchemaReady = %v, %v, want the schema ready", ready, err)
	}
}

// emptyOwnedDB is a database with no schema, owned by rooms_owner, and the DSN to it as that role
// (the roles exist: bootstrap created them cluster-wide).
func emptyOwnedDB(t *testing.T) string {
	t.Helper()
	owner, _ := emptyOwnedDBWithSuper(t)
	return owner
}

// emptyOwnedDBWithSuper is emptyOwnedDB plus the superuser's DSN, for the catalog states only a
// superuser may build: a half-finished concurrent build's invalid index.
func emptyOwnedDBWithSuper(t *testing.T) (owner, super string) {
	t.Helper()
	name := dbName(t.Name())
	id := pgx.Identifier{name}.Sanitize()
	exec(t, shared.super, "CREATE DATABASE "+id+" OWNER rooms_owner")
	t.Cleanup(func() { exec(t, shared.super, "DROP DATABASE IF EXISTS "+id+" WITH (FORCE)") })
	super = inDB(shared.super, name)
	return as(super, "rooms_owner", "owner"), super
}

func migrationCount(t *testing.T) int {
	t.Helper()
	files, err := migrations.Files()
	if err != nil {
		t.Fatal(err)
	}
	return len(files)
}

// query reads one text column as dsn: every fact these tests assert is a catalog one.
func query(t *testing.T, dsn, sql string, args ...any) string {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	var got string
	if err := conn.QueryRow(t.Context(), sql, args...).Scan(&got); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return got
}
