// SPDX-License-Identifier: Apache-2.0

// Command factory-slack-app is the factory's Slack App: a mention drafts a run, a maintainer's
// check mark files it, and the broker's webhooks tell the thread. `factory-slack-app --config
// <file>` runs it; its wiring is app.RunSlackApp.
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
		_, _ = fmt.Fprintln(os.Stderr, "factory-slack-app:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := app.RunSlackApp(ctx, log, os.Args[1:], os.Getenv); err != nil {
		log.Error("factory-slack-app stopped", "err", err)
		return 1
	}
	return 0
}
