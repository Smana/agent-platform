// SPDX-License-Identifier: Apache-2.0

// Command room-bridge is the native sidecar of an AgentRun sandbox with a
// roomRef (SP2 §3): it mirrors the harness into the room and carries the room's
// steering, interrupts and decisions back. Its wiring is app.RunBridge.
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
		_, _ = fmt.Fprintln(os.Stderr, "room-bridge:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := app.RunBridge(ctx, log, os.Getenv); err != nil {
		log.Error("room-bridge stopped", "err", err)
		return 1
	}
	return 0
}
