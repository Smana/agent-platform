// SPDX-License-Identifier: Apache-2.0

// Command loadconfig loads each file through a component's real config loader:
// `loadconfig broker|run-controller FILE...`. render.sh feeds it the chart's rendered config.yaml,
// so a template edit the component would refuse fails chart:check, not a rollout.
package main

import (
	"fmt"
	"os"

	"github.com/Smana/agent-platform/internal/config"
	"github.com/Smana/agent-platform/internal/runctl"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: loadconfig broker|run-controller FILE...")
		os.Exit(2)
	}
	var load func(string) error
	switch os.Args[1] {
	case "broker":
		load = func(p string) error { _, err := config.Load(p); return err }
	case "run-controller":
		load = func(p string) error { _, err := runctl.LoadConfig(p); return err }
	default:
		fmt.Fprintf(os.Stderr, "unknown component %q\n", os.Args[1])
		os.Exit(2)
	}
	failed := false
	for _, p := range os.Args[2:] {
		if err := load(p); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: %s config %s: %v\n", os.Args[1], p, err)
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}
