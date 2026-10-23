// SPDX-License-Identifier: Apache-2.0

package slackapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodConfig = `
teamID: T01ABC
botUserIDFile: /s/uid
botTokenFile: /s/tok
signingSecretFile: /s/sig
webhookKeyFile: /s/key
brokerURL: https://room-broker.agents.svc.cluster.local
roomsURL: https://rooms.example
namespace: agents
channels:
  C03:
    repositories: [Smana/ai-native-ref]
    maxCostUSD: 2
`

func loadConfigText(t *testing.T, text string) (Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(p)
}

// The App's config is strict like the broker's: no channel, a sub-cent max_cost (the factory
// refuses it), a repository that is not owner/name, or an unknown key is refused; the three
// listener addresses default.
func TestConfig(t *testing.T) {
	c, err := loadConfigText(t, goodConfig)
	if err != nil {
		t.Fatal(err)
	}
	if c.EventsAddr != ":8080" || c.WebhooksAddr != ":8081" || c.OpsAddr != ":9090" {
		t.Fatalf("defaults: %+v", c)
	}
	for name, text := range map[string]string{
		"no channel":  strings.Replace(goodConfig, "channels:\n  C03:\n    repositories: [Smana/ai-native-ref]\n    maxCostUSD: 2\n", "channels: {}\n", 1),
		"sub-cent":    strings.Replace(goodConfig, "maxCostUSD: 2", "maxCostUSD: 0.005", 1),
		"bad repo":    strings.Replace(goodConfig, "Smana/ai-native-ref", "not-a-repo", 1),
		"no repos":    strings.Replace(goodConfig, "[Smana/ai-native-ref]", "[]", 1),
		"unknown key": goodConfig + "surprise: 1\n",
		"no team":     strings.Replace(goodConfig, "teamID: T01ABC\n", "", 1),
		"bad team":    strings.Replace(goodConfig, "T01ABC", "nope", 1),
		"no secret":   strings.Replace(goodConfig, "signingSecretFile: /s/sig\n", "", 1),
		"no token":    strings.Replace(goodConfig, "botTokenFile: /s/tok\n", "", 1),
		"no key":      strings.Replace(goodConfig, "webhookKeyFile: /s/key\n", "", 1),
		"no bot id":   strings.Replace(goodConfig, "botUserIDFile: /s/uid\n", "", 1),
	} {
		if _, err := loadConfigText(t, text); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
