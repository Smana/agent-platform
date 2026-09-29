// SPDX-License-Identifier: Apache-2.0

// Command room-broker is replaced in phase 1; this stub proves the image pipeline.
package main

import (
	"fmt"

	"github.com/Smana/agent-platform/internal/version"
)

func main() { fmt.Println("room-broker", version.Version) }
