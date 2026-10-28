// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// Factory interfaces F2, behind a switch (D4): /mcp is off unless enabled, and on it needs the
// maku client, whose tokens are the only ones it takes.
func TestLoadMCP(t *testing.T) {
	c, err := Load(write(t, valid))
	if err != nil || c.Human.MCP.Enabled {
		t.Fatalf("off by default: %+v, %v", c.Human.MCP, err)
	}
	maku := strings.Replace(valid, "  projectIDFile:", "  roomctlClientIDFile: /etc/room-broker/roomctl/client-id\n  projectIDFile:", 1)
	c, err = Load(write(t, maku+"  mcp:\n    enabled: true\n"))
	if err != nil || !c.Human.MCP.Enabled {
		t.Fatalf("on: %+v, %v", c.Human.MCP, err)
	}
	if _, err := Load(write(t, valid+"  mcp:\n    enabled: true\n")); err == nil || !strings.Contains(err.Error(), "human.mcp.enabled") {
		t.Fatalf("on without the maku client: %v", err)
	}
}

// human.mcp.sessionStart lists the repositories whose canStart is true: off when empty (D4), only
// on an enabled /mcp, each one owner/name and declared once.
func TestLoadMCPSessionStart(t *testing.T) {
	maku := strings.Replace(valid, "  projectIDFile:", "  roomctlClientIDFile: /etc/room-broker/roomctl/client-id\n  projectIDFile:", 1)
	tests := []struct {
		name, doc, err string
	}{
		{"listed", maku + access + "  mcp:\n    enabled: true\n    sessionStart: [Smana/a]\n", ""},
		{"without human.access", maku + "  mcp:\n    enabled: true\n    sessionStart: [Smana/a]\n", "human.mcp.sessionStart needs human.access"},
		{"without enabled", maku + "  mcp:\n    sessionStart: [Smana/a]\n", "human.mcp.sessionStart needs human.mcp.enabled"},
		{"not a repository", maku + access + "  mcp:\n    enabled: true\n    sessionStart: [not-a-repo]\n", "human.mcp.sessionStart[0]"},
		{"twice", maku + access + "  mcp:\n    enabled: true\n    sessionStart: [Smana/a, Smana/a]\n", "declared twice"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(write(t, tc.doc))
			if tc.err == "" {
				if err != nil || len(c.Human.MCP.SessionStart) != 1 {
					t.Fatalf("%+v, %v", c.Human.MCP, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("want %q, got %v", tc.err, err)
			}
		})
	}
	if _, err := Load(write(t, maku+access+"  mcp:\n    enabled: true\n    sessionStart: [not-a-repo]\n")); err == nil || !strings.Contains(err.Error(), "owner/name") {
		t.Fatalf("the message names owner/name: %v", err)
	}
	if c, err := Load(write(t, maku+"  mcp:\n    enabled: true\n")); err != nil || len(c.Human.MCP.SessionStart) != 0 {
		t.Fatalf("empty by default: %+v, %v", c.Human.MCP, err)
	}
}
