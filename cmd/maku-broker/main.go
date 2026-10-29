// SPDX-License-Identifier: Apache-2.0

// Command maku-broker is the room broker (SP2): the log of record, its bridges'
// API, the Room controller and, from phase 2, its viewers. `maku-broker serve`
// (the default) runs it; `maku-broker retention` is the daily purge and `maku-broker migrate`
// applies the embedded schema as rooms_owner, from ROOMS_MIGRATION_DATABASE_URL (an init
// container, or a one-off outside the Atlas operator). `maku-broker migrate-group --from <group>
// --to <group> [--apply]` moves Rooms and Tasks between API groups (dry run without `--apply`).
// Its wiring is app.RunBroker.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Smana/maku/internal/app"
	"github.com/Smana/maku/internal/logging"
)

func main() { os.Exit(run()) }

func run() int {
	log, err := logging.New(os.Stdout, os.Getenv("LOG_FORMAT"), os.Getenv("LOG_LEVEL"))
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "maku-broker:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := app.RunBroker(ctx, log, os.Args[1:], os.Getenv); err != nil {
		log.Error("maku-broker stopped", "err", err)
		return 1
	}
	return 0
}
