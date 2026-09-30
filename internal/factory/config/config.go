// SPDX-License-Identifier: Apache-2.0

// Package config is agent-factory's one config file (§4): templates, tiers, caps and
// maintainers, parsed strictly at startup so that a bad config fails its rollout. The file is
// a gate path (§5.3): only a human-authored PR changes it. It carries the platform's facts
// (repository, logins, endpoints), so no factory package hard-codes them.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

const (
	// RunTokenCeiling is the gateway's per-run ceiling and SP1's XRD maximum (C3, C5).
	RunTokenCeiling = 5_000_000
	// MaxTextCeiling leaves room for the preamble in AgentRun task.text (16384, R6).
	MaxTextCeiling = 14336
	// maxRunMinutes is SP1's XRD maximum for budget.maxMinutes.
	maxRunMinutes = 480
)

// Duration is a time.Duration written as a string such as "30s".
type Duration struct{ time.Duration }

// UnmarshalJSON parses a Go duration string; a bare number is refused, since its unit is a guess.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("a duration is a string such as \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

// Config is the factory's configuration.
type Config struct {
	Repository   string              `json:"repository"`
	Maintainers  []string            `json:"maintainers"`
	TriggerLabel string              `json:"triggerLabel"`
	FactoryLogin string              `json:"factoryLogin"`
	AgentsLogin  string              `json:"agentsLogin"`
	RoomsURL     string              `json:"roomsURL"`
	Broker       Broker              `json:"broker"`
	GitHub       GitHub              `json:"github"`
	Poll         Poll                `json:"poll"`
	Defaults     Defaults            `json:"defaults"`
	Tiers        map[string]Tier     `json:"tiers"`
	Templates    map[string]Template `json:"templates"`
	Caps         Caps                `json:"caps"`
	Meter        Meter               `json:"meter"`
	Tracing      Tracing             `json:"tracing"`
	// Hash is the sha256 of the parsed file; tasks carry it (status.configHash).
	Hash string `json:"-"`
}

// Broker is SP2's room-broker system API. Its :8443 is TLS (GP-18): the URL is https and the
// server certificate is verified against CAFile. Files are read at use, so rotations need no restart.
type Broker struct {
	URL       string `json:"url"`
	CAFile    string `json:"caFile"`
	TokenFile string `json:"tokenFile"`
}

// GitHub names the factory App's mounted credentials.
type GitHub struct {
	AppIDFile      string `json:"appIDFile"`
	PrivateKeyFile string `json:"privateKeyFile"`
}

// Poll is how often each loop runs.
type Poll struct {
	Issues Duration `json:"issues"`
	Tasks  Duration `json:"tasks"`
	Meter  Duration `json:"meter"`
}

// Defaults apply when triage leaves a value open.
type Defaults struct {
	Template       string `json:"template"`
	Tier           string `json:"tier"`
	DataClass      string `json:"dataClass"`
	PredictedClass string `json:"predictedClass"`
}

// Tier is a model and its budgets.
type Tier struct {
	Model      string `json:"model"`
	RunTokens  int64  `json:"runTokens"`
	TaskTokens int64  `json:"taskTokens"`
	RunMinutes int64  `json:"runMinutes"`
}

// Template is a team: the roles run in order on one branch.
type Template struct {
	Roles           []string `json:"roles"`
	MaxReviewRounds int32    `json:"maxReviewRounds,omitempty"`
}

// Caps bound the factory's throughput (§6.2).
type Caps struct {
	ActiveTasks    int `json:"activeTasks"`
	ConcurrentRuns int `json:"concurrentRuns"`
	TasksPerDay    int `json:"tasksPerDay"`
	MaxTextBytes   int `json:"maxTextBytes"`
}

// Meter is where the run meter reads token usage (R12).
type Meter struct {
	URL   string `json:"url"`
	Query string `json:"query"`
}

// Tracing is where task spans go (R46): the trace collector's platform port. Empty: tracing off.
type Tracing struct {
	OTLPEndpoint string `json:"otlpEndpoint"`
}

var (
	repoRE     = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	hostPortRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?:[0-9]{1,5}$`)
	hostRE     = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`) // a DNS name, an optional port
)

// The vocabularies the config is checked against: SP1's XRD roles and C5's logical model names.
func roles() []string { return []string{"implementer", "reviewer", "tester", "triager"} }
func models() []string {
	return []string{"agent-default", "tier-light", "tier-standard", "tier-frontier"}
}
func tiers() []string { return []string{"light", "standard", "frontier"} }

// templates is the Task CRD's template enum (api/factory/v1alpha1).
func templates() []string { return []string{"solo", "pair", "trio", "investigate"} }

// Load reads and parses the file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return Parse(raw)
}

// Parse decodes raw strictly, refusing unknown keys, then validates it and stamps its hash.
func Parse(raw []byte) (*Config, error) {
	j, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	sum := sha256.Sum256(j)
	c.Hash = hex.EncodeToString(sum[:])
	return &c, nil
}

// IsMaintainer is whether login is a listed maintainer; GitHub logins are case-insensitive.
func (c *Config) IsMaintainer(login string) bool {
	return slices.ContainsFunc(c.Maintainers, func(m string) bool { return strings.EqualFold(m, login) })
}

// Validate reports every problem at once, so one rollout shows them all.
func (c *Config) Validate() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if !repoRE.MatchString(c.Repository) {
		bad("repository %q is not owner/name", c.Repository)
	}
	if len(c.Maintainers) == 0 {
		bad("maintainers is empty: nobody could start a task")
	}
	for _, f := range []struct{ key, value string }{
		{"triggerLabel", c.TriggerLabel}, {"factoryLogin", c.FactoryLogin}, {"agentsLogin", c.AgentsLogin},
		{"roomsURL", c.RoomsURL}, {"broker.url", c.Broker.URL}, {"broker.caFile", c.Broker.CAFile},
		{"broker.tokenFile", c.Broker.TokenFile}, {"github.appIDFile", c.GitHub.AppIDFile},
		{"github.privateKeyFile", c.GitHub.PrivateKeyFile}, {"meter.url", c.Meter.URL}, {"meter.query", c.Meter.Query},
	} {
		if f.value == "" {
			bad("%s is required", f.key)
		}
	}
	// GP-18: the broker serves TLS only, and the factory never talks to it in the clear.
	if u, err := url.Parse(c.Broker.URL); c.Broker.URL != "" && (err != nil || u.Scheme != "https" || u.Host == "") {
		bad("broker.url %q must be an https:// URL", c.Broker.URL)
	}
	// Narration posts roomsURL on public issues as the watch link: the rooms UI's origin over TLS,
	// with nothing a reader could be sent elsewhere by, and nothing that could break the markdown.
	// url.Parse accepts "(", ")", "<" and ">" in a host name; hostRE does not.
	if u, err := url.Parse(c.RoomsURL); c.RoomsURL != "" && (err != nil || u.Scheme != "https" || !hostRE.MatchString(u.Host) ||
		u.User != nil || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "") {
		bad("roomsURL must be https://<host>, without credentials, path or query: %q", c.RoomsURL)
	}
	for _, p := range []struct {
		key string
		d   Duration
	}{{"poll.issues", c.Poll.Issues}, {"poll.tasks", c.Poll.Tasks}, {"poll.meter", c.Poll.Meter}} {
		if p.d.Duration <= 0 {
			bad("%s must be positive", p.key)
		}
	}
	for _, name := range tiers() {
		t, ok := c.Tiers[name]
		switch {
		case !ok:
			bad("tier %s is missing", name)
		case !slices.Contains(models(), t.Model):
			bad("tier %s: model %q is not a C5 logical name", name, t.Model)
		case t.RunTokens < 1 || t.RunTokens > RunTokenCeiling:
			bad("tier %s: runTokens must be 1..%d", name, RunTokenCeiling)
		case t.TaskTokens < t.RunTokens:
			bad("tier %s: taskTokens below runTokens", name)
		case t.RunMinutes < 1 || t.RunMinutes > maxRunMinutes:
			bad("tier %s: runMinutes must be 1..%d", name, maxRunMinutes)
		}
	}
	if len(c.Tiers) != len(tiers()) {
		bad("tiers are exactly light, standard and frontier")
	}
	for name, t := range c.Templates {
		// The reconciler creates Tasks from these names, and the Task CRD's enum refuses any other.
		if !slices.Contains(templates(), name) {
			bad("template %s is not one of solo, pair, trio, investigate", name)
		}
		if len(t.Roles) == 0 {
			bad("template %s has no roles", name)
		}
		for _, r := range t.Roles {
			if !slices.Contains(roles(), r) {
				bad("template %s: unknown role %q", name, r)
			}
		}
		// R38 (owner default, 2026-09-27): a lone triager only proposes; every other template writes.
		if !slices.Contains(t.Roles, "implementer") && !slices.Equal(t.Roles, []string{"triager"}) {
			bad("template %s: only an implementer writes, so every template but a lone triager has one", name)
		}
	}
	if _, ok := c.Templates[c.Defaults.Template]; !ok {
		bad("defaults.template %q is not a template", c.Defaults.Template)
	}
	if !slices.Contains(tiers(), c.Defaults.Tier) {
		bad("defaults.tier %q is not a tier", c.Defaults.Tier)
	}
	if c.Defaults.DataClass != "public" && c.Defaults.DataClass != "internal" {
		bad("defaults.dataClass is public or internal")
	}
	if c.Defaults.PredictedClass == "" {
		bad("defaults.predictedClass is required")
	}
	if c.Caps.ActiveTasks < 1 || c.Caps.ConcurrentRuns < 1 || c.Caps.TasksPerDay < 1 {
		bad("caps must be positive")
	}
	if c.Caps.MaxTextBytes < 1 || c.Caps.MaxTextBytes > MaxTextCeiling {
		bad("caps.maxTextBytes must be 1..%d (R6)", MaxTextCeiling)
	}
	if c.Tracing.OTLPEndpoint != "" && !hostPortRE.MatchString(c.Tracing.OTLPEndpoint) {
		bad("tracing.otlpEndpoint %q is not host:port", c.Tracing.OTLPEndpoint)
	}
	return errors.Join(errs...)
}
