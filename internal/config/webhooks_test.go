// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

const slackHook = `webhooks:
  - name: slack-app
    network: cluster
    url: http://factory-slack-app.agent-system.svc.cluster.local:8081/webhooks
    events: [room.waiting_for_human, room.pr_opened, room.ended]
    secretFile: /etc/room-broker/webhooks/slack-app/signing-key
    repositories: [Smana/ai-native-ref]
`

// parse loads the package's valid fixture with block appended.
func parse(t *testing.T, block string) (Config, error) {
	t.Helper()
	return Load(write(t, valid+block))
}

// F6, R17, R20, live proof 2 offline: a subscriber names its network; an internet one with a
// private or cluster address, or a cluster one outside the cluster, is refused at load.
func TestWebhooksConfig(t *testing.T) {
	if _, err := parse(t, slackHook); err != nil {
		t.Fatal(err)
	}
	const clusterURL = "network: cluster\n    url: http://factory-slack-app.agent-system.svc.cluster.local:8081/webhooks"
	for name, c := range map[string]struct{ from, to, want string }{
		"no network":               {"network: cluster", "network: ''", `network is cluster or internet`},
		"cluster, a public host":   {"factory-slack-app.agent-system.svc.cluster.local:8081", "hooks.example.io", "<svc>.<ns>.svc.cluster.local"},
		"internet, http":           {clusterURL, "network: internet\n    url: http://hooks.example.io/x", "an internet subscriber is https"},
		"internet, an IP":          {clusterURL, "network: internet\n    url: https://10.0.0.7/x", "a DNS name, never an address"},
		"internet, a public IP":    {clusterURL, "network: internet\n    url: https://8.8.8.8/x", "a DNS name, never an address"},
		"internet, a cluster name": {clusterURL, "network: internet\n    url: https://x.agent-system.svc.cluster.local/x", "not a cluster or private name"},
		"internet, localhost":      {clusterURL, "network: internet\n    url: https://localhost/x", "not a cluster or private name"},
		"internet, .internal":      {clusterURL, "network: internet\n    url: https://db.internal/x", "not a cluster or private name"},
		"an unknown event":         {"room.ended]", "room.everything]", `unknown event "room.everything"`},
		"no events":                {"events: [room.waiting_for_human, room.pr_opened, room.ended]", "events: []", "events is empty"},
		"no secret":                {"    secretFile: /etc/room-broker/webhooks/slack-app/signing-key\n", "", "secretFile is required"},
		"no repository":            {"repositories: [Smana/ai-native-ref]", "repositories: []", "repositories is empty"},
		"a bad name":               {"name: slack-app", "name: Slack_App", "DNS label"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parse(t, strings.Replace(slackHook, c.from, c.to, 1)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}

func TestWebhooksConfigUniqueNameAndInternet(t *testing.T) {
	two := slackHook + strings.TrimPrefix(slackHook, "webhooks:\n")
	if _, err := parse(t, two); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("got %v, want a duplicate name", err)
	}
	ok := strings.Replace(slackHook, "network: cluster\n    url: http://factory-slack-app.agent-system.svc.cluster.local:8081/webhooks",
		"network: internet\n    url: https://hooks.example.io/x", 1)
	if _, err := parse(t, ok); err != nil {
		t.Fatal(err)
	}
}
