// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// Factory interfaces F2, behind a switch (D4): /mcp is off unless enabled, and on it needs the
// roomctl client, whose tokens are the only ones it takes.
func TestLoadMCP(t *testing.T) {
	c, err := Load(write(t, valid))
	if err != nil || c.Human.MCP.Enabled {
		t.Fatalf("off by default: %+v, %v", c.Human.MCP, err)
	}
	roomctl := strings.Replace(valid, "  projectIDFile:", "  roomctlClientIDFile: /etc/room-broker/roomctl/client-id\n  projectIDFile:", 1)
	c, err = Load(write(t, roomctl+"  mcp:\n    enabled: true\n"))
	if err != nil || !c.Human.MCP.Enabled {
		t.Fatalf("on: %+v, %v", c.Human.MCP, err)
	}
	if _, err := Load(write(t, valid+"  mcp:\n    enabled: true\n")); err == nil || !strings.Contains(err.Error(), "human.mcp.enabled") {
		t.Fatalf("on without the roomctl client: %v", err)
	}
}
