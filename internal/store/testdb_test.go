// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// template is the database the migrations run into once, in TestMain; every test's
// database is a copy of it, grants, row-level security and functions included.
const template = "rooms_tpl"

// bootstrap does what CNPG does on the cluster (login roles, a database owned by
// rooms_owner) and applies every migration to the template as rooms_owner, through the
// migrator room-broker migrate runs: one runner, so what the suite builds is what an
// operator's init container builds. Roles are cluster-wide, so they are created here once;
// the grants, policies and the revision ledger live inside the template and are copied with
// it. The template then refuses connections, which is what keeps it untouched and clonable.
func bootstrap(ctx context.Context, super string) error {
	tpl := pgx.Identifier{template}.Sanitize()
	if err := run(ctx, super, `CREATE ROLE rooms_owner LOGIN PASSWORD 'owner'; -- pragma: allowlist secret
		CREATE ROLE rooms_broker LOGIN PASSWORD 'broker'; -- pragma: allowlist secret
		CREATE ROLE rooms_retention LOGIN PASSWORD 'retention'; -- pragma: allowlist secret`); err != nil {
		return err
	}
	if err := run(ctx, super, "CREATE DATABASE "+tpl+" OWNER rooms_owner"); err != nil {
		return err
	}
	if _, err := Migrate(ctx, as(inDB(super, template), "rooms_owner", "owner"), slog.New(slog.DiscardHandler)); err != nil {
		return fmt.Errorf("migrate %s: %w", template, err)
	}
	return run(ctx, super, "ALTER DATABASE "+tpl+" WITH ALLOW_CONNECTIONS false")
}

// testDB gives the test a database of its own, cloned from the migrated template,
// dropped when the test ends, and returns connection URLs for the three roles and
// the superuser, all pointing at it.
func testDB(t *testing.T) (owner, broker, retention, super string) {
	t.Helper()
	name := dbName(t.Name())
	id := pgx.Identifier{name}.Sanitize()
	exec(t, shared.super, "CREATE DATABASE "+id+" TEMPLATE "+pgx.Identifier{template}.Sanitize()+" OWNER rooms_owner")
	// Registered first, so it runs last: every pool the test opened is closed by then.
	// FORCE ends any session still open rather than hanging the suite on it.
	t.Cleanup(func() { exec(t, shared.super, "DROP DATABASE IF EXISTS "+id+" WITH (FORCE)") })
	super = inDB(shared.super, name)
	return as(super, "rooms_owner", "owner"), as(super, "rooms_broker", "broker"), as(super, "rooms_retention", "retention"), super
}

// dbName is the test's name made an identifier, short enough for PostgreSQL's 63
// bytes, with a random suffix so a rerun (-count) or a same-named subtest never collides.
func dbName(test string) string {
	name := "t_" + strings.Trim(nonIdent.ReplaceAllString(strings.ToLower(test), "_"), "_")
	if len(name) > 50 {
		name = name[:50]
	}
	return name + "_" + strings.ToLower(rand.Text()[:6])
}

var nonIdent = regexp.MustCompile("[^a-z0-9]+")

// inDB returns dsn pointing at database name.
func inDB(dsn, name string) string {
	u, _ := url.Parse(dsn)
	u.Path = "/" + name
	return u.String()
}

// Every statement of a txmode-none file runs, whatever its verb (review 4.3):
// TestMigrateTxModeNoneStatements, against the production splitter.

func as(dsn, user, pass string) string {
	u, _ := url.Parse(dsn)
	u.User = url.UserPassword(user, pass)
	return u.String()
}

func exec(t *testing.T, dsn, sql string, args ...any) {
	t.Helper()
	if err := run(context.Background(), dsn, sql, args...); err != nil {
		t.Fatal(err)
	}
}

// run executes sql on a connection of its own, closed before it returns.
func run(ctx context.Context, dsn, sql string, args ...any) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("%w\n%s", err, sql)
	}
	return nil
}

// forge rewrites rooms as the superuser with triggers off (session_replication_role =
// replica): the only way to build a state the schema otherwise refuses, such as an
// expired close date.
func forge(t *testing.T, super, sql string) {
	t.Helper()
	exec(t, super, "SET session_replication_role = replica; "+sql)
}

// Each test gets a database of its own, cloned from the migrated template: a row one
// test writes is never another test's (review 2026-10-15, B29).
func TestEachTestHasItsOwnDatabase(t *testing.T) {
	var names [2]string
	for i := range names {
		t.Run(fmt.Sprint("db", i), func(t *testing.T) {
			_, broker, _, super := testDB(t)
			conn, err := pgx.Connect(t.Context(), super)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close(context.Background()) }()
			var rooms int
			if err := conn.QueryRow(t.Context(), `SELECT current_database(), (SELECT count(*) FROM rooms)`).Scan(&names[i], &rooms); err != nil {
				t.Fatal(err)
			}
			if rooms != 0 {
				t.Fatalf("a fresh database holds %d rooms, want 0", rooms)
			}
			exec(t, broker, `INSERT INTO rooms (room_id, driver, fallback_driver, retention) VALUES ('3kq7x2ma', 'system:factory', 'system:factory', '1 day')`)
		})
	}
	if names[0] == names[1] || slices.Contains(names[:], "rooms_tpl") {
		t.Fatalf("databases %q: want two distinct per-test databases, neither the template", names)
	}
}
