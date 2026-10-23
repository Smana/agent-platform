// SPDX-License-Identifier: Apache-2.0

package slackapp

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"

	"sigs.k8s.io/yaml"
)

// minMaxCost is the factory's floor: it refuses a max_cost under one cent.
const minMaxCost = 0.01

// Config is the App's config file, loaded strictly as the broker's config is.
type Config struct {
	BotUserIDFile     string             `json:"botUserIDFile"`
	BotTokenFile      string             `json:"botTokenFile"`
	SigningSecretFile string             `json:"signingSecretFile"`
	WebhookKeyFile    string             `json:"webhookKeyFile"`
	BrokerURL         string             `json:"brokerURL"`
	RoomsURL          string             `json:"roomsURL"`
	IssuerURL         string             `json:"issuerURL"`
	ClientIDFile      string             `json:"clientIDFile"`
	ClientSecretFile  string             `json:"clientSecretFile"`
	ProjectIDFile     string             `json:"projectIDFile"`
	Namespace         string             `json:"namespace"`
	Channels          map[string]Channel `json:"channels"`
	// The three listeners (M11): Slack's events (public), the broker's webhooks (cluster only),
	// and metrics with the probes. Defaults :8080, :8081 and :9090; a test passes 127.0.0.1:0.
	EventsAddr   string `json:"eventsAddr,omitempty"`
	WebhooksAddr string `json:"webhooksAddr,omitempty"`
	OpsAddr      string `json:"opsAddr,omitempty"`
}

// Channel is a channel the App serves: the repositories a draft there may name, and the
// max_cost a ✅ there sends.
type Channel struct {
	Repositories []string `json:"repositories"`
	MaxCostUSD   float64  `json:"maxCostUSD"`
}

// LoadConfig reads, strictly decodes, defaults and validates the file at path.
func LoadConfig(path string) (Config, error) {
	var c Config
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return c, fmt.Errorf("config: %w", err)
	}
	if err := yaml.UnmarshalStrict(raw, &c); err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	for addr, def := range map[*string]string{&c.EventsAddr: ":8080", &c.WebhooksAddr: ":8081", &c.OpsAddr: ":9090"} {
		if *addr == "" {
			*addr = def
		}
	}
	if err := c.validate(); err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	return c, nil
}

func (c Config) validate() error {
	var errs []error
	for name, v := range map[string]string{"namespace": c.Namespace, "brokerURL": c.BrokerURL, "roomsURL": c.RoomsURL} {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}
	for name, v := range map[string]string{"brokerURL": c.BrokerURL, "roomsURL": c.RoomsURL} {
		if u, err := url.Parse(v); v != "" && (err != nil || u.Scheme != "https" && u.Scheme != "http" || u.Hostname() == "") {
			errs = append(errs, fmt.Errorf("%s %q must be an http(s) URL", name, v))
		}
	}
	if len(c.Channels) == 0 {
		errs = append(errs, errors.New("channels: at least one channel is required"))
	}
	ids := make([]string, 0, len(c.Channels))
	for id := range c.Channels {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ch := c.Channels[id]
		if !channelRE.MatchString(id) {
			errs = append(errs, fmt.Errorf("channels: %q is not a Slack channel id", id))
		}
		if len(ch.Repositories) == 0 {
			errs = append(errs, fmt.Errorf("channels.%s: repositories is empty", id))
		}
		for _, r := range ch.Repositories {
			if !repoRE.MatchString(r) {
				errs = append(errs, fmt.Errorf("channels.%s: repository %q is not owner/name", id, r))
			}
		}
		if ch.MaxCostUSD < minMaxCost {
			errs = append(errs, fmt.Errorf("channels.%s: maxCostUSD %v is below %v", id, ch.MaxCostUSD, minMaxCost))
		}
	}
	return errors.Join(errs...)
}
