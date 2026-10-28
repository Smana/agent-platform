// SPDX-License-Identifier: Apache-2.0

// Command maku follows and feeds rooms from a terminal (SP2 §8): it lists
// rooms, watches one, posts, queues and forks. It never steers, interrupts,
// moves the driver token or decides (ruling P18). Its wiring is app.RunMaku.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Smana/maku/internal/app"
)

func main() { os.Exit(run()) }

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := app.RunMaku(ctx, os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "maku:", err)
		return 1
	}
	return 0
}
