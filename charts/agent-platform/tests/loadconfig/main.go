// SPDX-License-Identifier: Apache-2.0

// Command loadconfig loads each file through a component's real config loader:
// `loadconfig broker|broker-roomtools-on|broker-roomtools-off|run-controller FILE...`. render.sh feeds
// it the chart's rendered config.yaml, so a template edit the component would refuse — or a roomTools
// switch that disagrees with the values — fails chart:check, not a rollout.
package main

import (
	"fmt"
	"os"

	"github.com/Smana/agent-platform/internal/config"
	"github.com/Smana/agent-platform/internal/runctl"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: loadconfig broker|broker-roomtools-on|broker-roomtools-off|run-controller FILE...")
		os.Exit(2)
	}
	var load func(string) error
	switch os.Args[1] {
	case "broker":
		load = func(p string) error { _, err := config.Load(p); return err }
	case "broker-roomtools-on", "broker-roomtools-off":
		// The :8090 listener is the config's switch (I8): the rendered block must decode to what the
		// values say, not merely load.
		want := os.Args[1] == "broker-roomtools-on"
		load = func(p string) error {
			c, err := config.Load(p)
			if err != nil {
				return err
			}
			if got := c.RoomToolsEnabled(); got != want {
				return fmt.Errorf("roomTools enabled = %v, want %v", got, want)
			}
			return nil
		}
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
