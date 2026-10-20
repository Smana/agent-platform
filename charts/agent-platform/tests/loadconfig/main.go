// SPDX-License-Identifier: Apache-2.0

// Command loadconfig loads each file through the broker's real config loader. render.sh feeds it the
// chart's rendered config.yaml, so a template edit the broker would refuse fails chart:check,
// not a rollout.
package main

import (
	"fmt"
	"os"

	"github.com/Smana/agent-platform/internal/config"
)

func main() {
	failed := false
	for _, p := range os.Args[1:] {
		if _, err := config.Load(p); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: broker config %s: %v\n", p, err)
			failed = true
		}
	}
	if failed || len(os.Args) < 2 {
		os.Exit(1)
	}
}
