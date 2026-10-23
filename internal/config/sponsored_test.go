// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// Factory interfaces F4, R13: sponsored principals on /mcp.
func TestLoadMCPSponsored(t *testing.T) {
	roomctl := strings.Replace(valid, "  projectIDFile:", "  roomctlClientIDFile: /etc/room-broker/roomctl/client-id\n  projectIDFile:", 1)
	with := func(factory bool, mcp string) string {
		doc := roomctl + "  mcp:\n" + mcp
		if factory {
			doc = "factoryURL: http://factory.example:8080\n" + doc
		}
		return doc
	}
	const on = "    enabled: true\n"
	one := "    sponsored:\n      - {name: slack-app, clientIDFile: /x}\n"

	c, err := Load(write(t, with(true, on+one)))
	if err != nil || len(c.Human.MCP.Sponsored) != 1 || c.Human.MCP.Sponsored[0].Name != "slack-app" {
		t.Fatalf("sponsored: %+v, %v", c.Human.MCP, err)
	}
	for name, tc := range map[string]struct{ doc, want string }{
		"not a DNS label": {with(true, on+"    sponsored:\n      - {name: Slack_App, clientIDFile: /x}\n"), "not a DNS label"},
		"declared twice":  {with(true, on+"    sponsored:\n      - {name: a, clientIDFile: /x}\n      - {name: a, clientIDFile: /y}\n"), "declared twice"},
		"empty file":      {with(true, on+"    sponsored:\n      - {name: a}\n"), "clientIDFile is required"},
		"mcp off":         {with(true, "    enabled: false\n"+one), "human.mcp.sponsored needs human.mcp.enabled"},
		"without factory": {with(false, on+one), "human.mcp.sponsored needs factoryURL"},
	} {
		if _, err := Load(write(t, tc.doc)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}
}

// C22, as the factory's api.sponsored: a name another component answers to is refused.
func TestLoadMCPSponsoredReservedNames(t *testing.T) {
	roomctl := strings.Replace(valid, "  projectIDFile:", "  roomctlClientIDFile: /etc/room-broker/roomctl/client-id\n  projectIDFile:", 1)
	roomctl = strings.Replace(roomctl, "systemPrincipals:\n", "systemPrincipals:\n  system:serviceaccount:x:y: system:slack-app\n", 1)
	for _, name := range []string{"policy", "factory", "room-broker", "slack-app"} {
		doc := "factoryURL: http://factory.example:8080\n" + roomctl +
			"  mcp:\n    enabled: true\n    sponsored:\n      - {name: " + name + ", clientIDFile: /x}\n"
		if _, err := Load(write(t, doc)); err == nil || !strings.Contains(err.Error(), `"`+name+`" is reserved`) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
