// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// dashboard is one human.dashboards entry, appended under the human block of valid.
func dashboard(label, url string) string {
	return "    - label: " + label + "\n      url: " + url + "\n"
}

// The room page's dashboard links: optional, off by default, and a typo fails startup.
func TestDashboardsConfig(t *testing.T) {
	c, err := parse(t, "")
	if err != nil || c.Human.Dashboards != nil {
		t.Fatalf("no dashboards by default: %+v, %v", c.Human.Dashboards, err)
	}
	for name, block := range map[string]string{
		"a room template": dashboard("Room metrics", "'https://grafana.example.com/d/agent-room/x?var-room={room}'"),
		"all three placeholders, a port and a fragment": dashboard("'Run · task'",
			"'https://grafana.example.com:3000/d/{run}/x?var-run={run}&var-task={task}&r={room}#panel-2'"),
		"no placeholder at all": dashboard("Overview", "https://grafana.example.com/d/agents"),
		"eight links":           strings.Repeat(dashboard("Room", "'https://grafana.example.com/?r={room}'"), 8),
	} {
		t.Run(name, func(t *testing.T) {
			c, err := parse(t, "  dashboards:\n"+block)
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Human.Dashboards) == 0 || c.Human.Dashboards[0].Label == "" || c.Human.Dashboards[0].URL == "" {
				t.Fatalf("dashboards: %+v", c.Human.Dashboards)
			}
		})
	}
	for name, tc := range map[string]struct{ block, want string }{
		"an unknown field":      {"    - label: Room\n      url: https://grafana.example.com/\n      icon: chart\n", "unknown field"},
		"an empty label":        {dashboard("''", "https://grafana.example.com/"), "human.dashboards[0]: label is required"},
		"a label over 40":       {dashboard(strings.Repeat("x", 41), "https://grafana.example.com/"), "human.dashboards[0]: label is at most 40 characters"},
		"a control character":   {dashboard(`"Room\tmetrics"`, "https://grafana.example.com/"), "human.dashboards[0]: label is printable text"},
		"no url":                {"    - label: Room\n", `human.dashboards[0] "Room": url must be an absolute https:// URL`},
		"http":                  {dashboard("Room", "'http://grafana.example.com/?r={room}'"), "url must be an absolute https:// URL"},
		"a relative url":        {dashboard("Room", "'/d/x?r={room}'"), "url must be an absolute https:// URL"},
		"a javascript: url":     {dashboard("Room", "'javascript:alert({room})'"), "url must be an absolute https:// URL"},
		"userinfo":              {dashboard("Room", "'https://u:p@grafana.example.com/?r={room}'"), "url must be an absolute https:// URL"},
		"a placeholder in host": {dashboard("Room", "'https://{room}.example.com/'"), "a placeholder only in the path, query or fragment"},
		"an unknown placeholder": {dashboard("Room", "'https://grafana.example.com/?r={rooms}'"),
			`human.dashboards[0] "Room": unknown placeholder "{rooms}": only {room}, {run} and {task}`},
		"a stray brace":                     {dashboard("Room", "'https://grafana.example.com/?r={room'"), "human.dashboards[0] \"Room\": unmatched { or }"},
		"an all-spaces label":               {dashboard("'   '", "https://grafana.example.com/"), "human.dashboards[0]: label is required"},
		"a placeholder glued to the host":   {dashboard("Room", "'https://grafana.example.com{room}'"), `human.dashboards[0] "Room": `},
		"a placeholder as the port":         {dashboard("Room", "'https://grafana.example.com:{room}/x'"), `human.dashboards[0] "Room": `},
		"a placeholder host after userinfo": {dashboard("Room", "'https://x@{room}/'"), `human.dashboards[0] "Room": `},
		"a placeholder as userinfo":         {dashboard("Room", "'https://{room}@grafana.example.com/'"), "url must be an absolute https:// URL"},
		"nine links":                        {strings.Repeat(dashboard("Room", "https://grafana.example.com/"), 9), "human.dashboards: at most 8"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parse(t, "  dashboards:\n"+tc.block)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error naming %q", err, tc.want)
			}
		})
	}
}
