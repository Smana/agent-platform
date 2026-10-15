// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"os"
	"testing"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// One PostgreSQL for the whole suite: the roles are created once, the migrations run once into a
// template database, and each test gets its own database from that template (review 2026-10-15, B29).
var shared struct{ super string }

func TestMain(m *testing.M) {
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("postgres"), tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		fmt.Fprintln(os.Stderr, "store tests: start postgres:", err)
		os.Exit(1)
	}
	if shared.super, err = c.ConnectionString(ctx, "sslmode=disable"); err == nil {
		err = bootstrap(ctx, shared.super) // roles, the rooms_tpl database, every migration
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "store tests: bootstrap:", err)
		_ = c.Terminate(ctx)
		os.Exit(1)
	}
	code := m.Run()
	_ = c.Terminate(ctx)
	os.Exit(code)
}
