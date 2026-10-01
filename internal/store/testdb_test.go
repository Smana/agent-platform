// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// testDB starts PostgreSQL 18, does what CNPG does on the cluster (login roles,
// a database owned by rooms_owner), applies every migration as rooms_owner the way
// Atlas does, and returns connection URLs for the three roles.
func testDB(t *testing.T) (owner, broker, retention, super string) {
	t.Helper()
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("rooms"), tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	super, err = c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	exec(t, super, `CREATE ROLE rooms_owner LOGIN PASSWORD 'owner'; -- pragma: allowlist secret
		CREATE ROLE rooms_broker LOGIN PASSWORD 'broker'; -- pragma: allowlist secret
		CREATE ROLE rooms_retention LOGIN PASSWORD 'retention'; -- pragma: allowlist secret
		ALTER DATABASE rooms OWNER TO rooms_owner;`)
	owner, broker, retention = as(super, "rooms_owner", "owner"), as(super, "rooms_broker", "broker"), as(super, "rooms_retention", "retention")
	files, _ := filepath.Glob("migrations/*.sql")
	sort.Strings(files)
	for _, f := range files {
		sql, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(sql), "-- atlas:txmode none") {
			exec(t, owner, string(sql))
			continue
		}
		// Atlas runs such a file a statement at a time, outside a transaction: a
		// multi-statement Exec would wrap CREATE INDEX CONCURRENTLY in one.
		for _, stmt := range statements(string(sql)) {
			exec(t, owner, stmt)
		}
	}
	return owner, broker, retention, super
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

// Every statement of a txmode-none file runs, whatever its verb (review 4.3).
func TestTxModeNoneStatements(t *testing.T) {
	got := statements(`-- atlas:txmode none

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

func as(dsn, user, pass string) string {
	u, _ := url.Parse(dsn)
	u.User = url.UserPassword(user, pass)
	return u.String()
}

func exec(t *testing.T, dsn, sql string) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
}

// forge rewrites rooms as the superuser with triggers off (session_replication_role =
// replica): the only way to build a state the schema otherwise refuses, such as an
// expired close date.
func forge(t *testing.T, super, sql string) {
	t.Helper()
	exec(t, super, "SET session_replication_role = replica; "+sql)
}
