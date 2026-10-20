// SPDX-License-Identifier: Apache-2.0

package store

import (
	"log/slog"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Smana/agent-platform/internal/store/migrations"
)

func TestMigrate(t *testing.T) {
	dsn := emptyOwnedDB(t)
	n, err := Migrate(t.Context(), dsn, slog.New(slog.DiscardHandler))
	if err != nil || n != migrationCount(t) {
		t.Fatalf("first run applied %d err %v", n, err)
	}
	if n, err := Migrate(t.Context(), dsn, slog.New(slog.DiscardHandler)); err != nil || n != 0 {
		t.Fatalf("second run must be a no-op: %d %v", n, err)
	}
	exec(t, dsn, "UPDATE room_broker_revisions SET checksum = 'x' WHERE version = (SELECT min(version) FROM room_broker_revisions)")
	if _, err := Migrate(t.Context(), dsn, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("a changed checksum must be refused")
	}
}

func TestMigrateConcurrent(t *testing.T) {
	dsn := emptyOwnedDB(t)
	var wg sync.WaitGroup
	total := make([]int, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := Migrate(t.Context(), dsn, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Error(err)
			}
			total[i] = n
		}()
	}
	wg.Wait()
	if total[0]+total[1] != migrationCount(t) {
		t.Fatalf("each file applied once: %v", total)
	}
}

// emptyOwnedDB is a database with no schema, owned by rooms_owner, and the DSN to it as that role
// (the roles exist: bootstrap created them cluster-wide).
func emptyOwnedDB(t *testing.T) string {
	t.Helper()
	name := dbName(t.Name())
	id := pgx.Identifier{name}.Sanitize()
	exec(t, shared.super, "CREATE DATABASE "+id+" OWNER rooms_owner")
	t.Cleanup(func() { exec(t, shared.super, "DROP DATABASE IF EXISTS "+id+" WITH (FORCE)") })
	return as(inDB(shared.super, name), "rooms_owner", "owner")
}

func migrationCount(t *testing.T) int {
	t.Helper()
	files, err := migrations.Files()
	if err != nil {
		t.Fatal(err)
	}
	return len(files)
}
