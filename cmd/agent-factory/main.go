// SPDX-License-Identifier: Apache-2.0

// Command agent-factory is SP3's orchestrator: it reconciles Tasks, polls GitHub for its
// labels, meters runs and is, from phase 5, the only creator of AgentRuns (C3). Its wiring is
// app.RunFactory.
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
		_, _ = fmt.Fprintln(os.Stderr, "agent-factory:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := app.RunFactory(ctx, log, os.Getenv); err != nil {
		log.Error("agent-factory exiting", "err", err)
		return 1
	}
	return 0
}
