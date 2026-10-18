// SPDX-License-Identifier: Apache-2.0

// Command run-controller reconciles agents.ogenki.io AgentRuns into gVisor pods (adoption 3a),
// on clusters without cloud-native-ref's Crossplane runtime. Its wiring is app.RunController.
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
		_, _ = fmt.Fprintln(os.Stderr, "run-controller:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := app.RunController(ctx, log, os.Getenv); err != nil {
		log.Error("run-controller stopped", "err", err)
		return 1
	}
	return 0
}
