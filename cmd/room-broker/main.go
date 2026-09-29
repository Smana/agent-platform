// SPDX-License-Identifier: Apache-2.0

// Command room-broker is the room broker (SP2): the log of record, its bridges'
// API, the Room controller and, from phase 2, its viewers. `room-broker serve`
// (the default) runs it; `room-broker retention` is the daily purge. Its wiring
// is app.RunBroker.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Smana/agent-platform/internal/app"
	"github.com/Smana/agent-platform/internal/logging"
)

func main() { os.Exit(run()) }

func run() int {
	log, err := logging.New(os.Stdout, os.Getenv("LOG_FORMAT"), os.Getenv("LOG_LEVEL"))
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "room-broker:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := app.RunBroker(ctx, log, os.Args[1:], os.Getenv); err != nil {
		log.Error("room-broker stopped", "err", err)
		return 1
	}
	return 0
}
