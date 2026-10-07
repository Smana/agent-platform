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

	"github.com/robfig/cron/v3"
	"sigs.k8s.io/yaml"
)

const (
	// RunTokenCeiling is the gateway's per-run ceiling and SP1's XRD maximum (C3, C5).
	RunTokenCeiling = 5_000_000
	// MaxTextCeiling leaves room for the preamble in AgentRun task.text (16384, R6).
	MaxTextCeiling = 14336
	// maxRunMinutes is SP1's XRD maximum for budget.maxMinutes.
	maxRunMinutes = 480
	// defaultMaxPendingMinutes bounds a Pending run when the config omits its own (P).
	defaultMaxPendingMinutes = 30
	// minMaxPendingMinutes keeps the Pending bound meaningful: under five minutes a busy cluster
	// would escalate healthy tasks.
	minMaxPendingMinutes = 5
	// OD-10's daily token budgets, when the config omits them: R3 wants a week of shadow
	// numbers first, so the ceilings exist even before an operator writes them down.
	defaultFactoryDaily = 25_000_000
	defaultHumanDaily   = 5_000_000
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
	Repository   string   `json:"repository"`
	Maintainers  []string `json:"maintainers"`
	TriggerLabel string   `json:"triggerLabel"`
	// ControlIssue is the pinned issue whose factory/stop label is the global stop (§6.1);
	// 0: none, and only the stop object stops the factory.
	ControlIssue int                 `json:"controlIssue,omitempty"`
	FactoryLogin string              `json:"factoryLogin"`
	AgentsLogin  string              `json:"agentsLogin"`
	RoomsURL     string              `json:"roomsURL"`
	Broker       Broker              `json:"broker"`
	GitHub       GitHub              `json:"github"`
	Poll         Poll                `json:"poll"`
	Defaults     Defaults            `json:"defaults"`
	Triage       Triage              `json:"triage"`
	Classes      map[string]Class    `json:"classes"`
	Merge        Merge               `json:"merge"`
	Tiers        map[string]Tier     `json:"tiers"`
	Templates    map[string]Template `json:"templates"`
	Caps         Caps                `json:"caps"`
	Budgets      Budgets             `json:"budgets"`
	Meter        Meter               `json:"meter"`
	Tracing      Tracing             `json:"tracing"`
	API          API                 `json:"api"`
	RunLore      RunLore             `json:"runlore"`
	Schedules    []Schedule          `json:"schedules,omitempty"`
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

// GitHub names the factory App's mounted credentials, and the merger App's (R16): merge-side
// calls — checks, merges, reverts — go through the merger's key, which no other workload holds.
type GitHub struct {
	AppIDFile      string `json:"appIDFile"`
	PrivateKeyFile string `json:"privateKeyFile"`
	// The merger App's pair: both files or neither, and both required once any class is live
	// or shadow, because deciding that class reads checks and arms through the merger.
	MergerAppIDFile string `json:"mergerAppIDFile,omitempty"`
	MergerKeyFile   string `json:"mergerKeyFile,omitempty"`
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

// Triage is phase 4's classification config (§2, §5.2).
type Triage struct {
	// C7's endpoint (R24). An error or timeout falls back to standard: never blocks.
	ClassifierURL string `json:"classifierURL"`
	// OD-14: this share of tasks runs at tier-frontier whatever C7 says.
	ControlPercent int `json:"controlPercent"`
}

// Class is one merge class (§5.2): live classes auto-merge once policy-bot agrees; a shadow
// class is decided exactly like a live one and never armed until the wave (R32, owner,
// 2026-09-27); an unmarked class is a prediction and a PR label only (OD-8). Never both.
type Class struct {
	Live   bool `json:"live,omitempty"`
	Shadow bool `json:"shadow,omitempty"`
}

// Merge is §5.1's merge actor and §6.4's rollback.
type Merge struct {
	// The 8 contexts classic protection requires, by name (GitHub Actions, app 15368).
	RequiredChecks []string `json:"requiredChecks"`
	// The checks main's CI runs on push, watched on the merge commit. Not RequiredChecks: a
	// path-filtered push workflow never reports there, and absent must not mean pending (§6.4).
	VerifyChecks     []string `json:"verifyChecks"`
	LeakScanCheck    string   `json:"leakScanCheck"`  // R42: TruffleHog's check run, "Security scanning 🔒"
	PolicyBotLogin   string   `json:"policyBotLogin"` // the status's expected creator
	MergerLogin      string   `json:"mergerLogin"`    // the arming actor, so the merge actor (R16)
	AutoMergesPerDay int      `json:"autoMergesPerDay"`
	FixRuns          int32    `json:"fixRuns"`
	VerifyFor        Duration `json:"verifyFor"`    // 30m of main's CI after an auto-merge
	RevertWindow     Duration `json:"revertWindow"` // 7 days for a maintainer's factory/revert
	Breaker          Breaker  `json:"breaker"`
}

// Breaker is the circuit breaker's outcome window (R41, review G5): a class whose last Window
// merges hold MaxReverts reverts is demoted to human review, whatever the config.
type Breaker struct {
	Window     int `json:"window"`
	MaxReverts int `json:"maxReverts"`
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
	// How long a run that never started may sit Pending before the task escalates as
	// run_unschedulable (P): no other layer bounds it — activeDeadlineSeconds counts from the
	// pod's start, and Kueue queues unadmitted work forever. Defaulted when omitted, never left
	// open.
	MaxPendingMinutes int `json:"maxPendingMinutes"`
	// AwaitingHumanWIP bounds the tasks waiting on a human review before the factory queues
	// more of the review-class work that produces them (§6.2's back-pressure on reviewers).
	// Required, never defaulted: a config that omits it fails its rollout (§4).
	AwaitingHumanWIP int `json:"awaitingHumanWIP"`
}

// Budgets are the admission-time caps SP3 owns (C5). The task cap is enforced unless the config
// writes enforceTask: false (owner, 2026-10-07: a shadow cap let a task run to 3.35 M of its 3 M);
// the principal's starts false, a week of shadow numbers first (OD-10, R3). The run cap is always
// enforced by the meter.
type Budgets struct {
	EnforceTask      bool  `json:"enforceTask"`
	EnforcePrincipal bool  `json:"enforcePrincipal"`
	FactoryDaily     int64 `json:"factoryDaily"`
	HumanDaily       int64 `json:"humanDaily"`
}

// Meter is where the run meter reads token usage (R12) and the gateway's 429s (R13).
type Meter struct {
	URL   string `json:"url"`
	Query string `json:"query"`
	// LogsURL is VictoriaLogs', where VL looks up which runs agent-router throttled.
	LogsURL string `json:"logsURL"`
	// ThrottleQuery is the LogsQL that names them: the last minutes of 429s with Envoy's RL
	// flag on agent-router, grouped by the verified identity header.
	ThrottleQuery string `json:"throttleQuery"`
}

// Tracing is where task spans go (R46): the trace collector's platform port. Empty: tracing off.
type Tracing struct {
	OTLPEndpoint string `json:"otlpEndpoint"`
}

// Schedule starts a task from config (§1). The config is a gate path, so its text is trusted.
type Schedule struct {
	Name      string `json:"name"`
	Cron      string `json:"cron"`  // 5 fields, UTC
	Class     string `json:"class"` // the predicted class (§2)
	Text      string `json:"text"`
	DataClass string `json:"dataClass,omitempty"`
	Probe     string `json:"probe,omitempty"` // "" or renovate-red
}

// API is the run-request API's configuration (§4): what it binds, which repositories it
// accepts (OD-6), and how it authenticates callers (Task 5.1's Authenticator).
type API struct {
	Listen       string   `json:"listen"`
	Repositories []string `json:"repositories"` // OD-6: cloud-native-ref only
	HumanIssuer  string   `json:"humanIssuer"`
	HumanJWKS    string   `json:"humanJWKS"`
	// Files holding the rooms-proxy and roomctl client ids (from agents-secrets).
	ClientIDFiles []string `json:"clientIDFiles"`
	// Empty today (R23): no system caller of POST /v1/runs exists.
	SystemIssuer     string            `json:"systemIssuer,omitempty"`
	SystemJWKS       string            `json:"systemJWKS,omitempty"`
	SystemPrincipals map[string]string `json:"systemPrincipals,omitempty"`
}

// RunLore is the intake of §1: findings become tasks when actionable, 5 a day (OD-9).
type RunLore struct {
	Listen        string  `json:"listen"`
	TokenFile     string  `json:"tokenFile"`
	MinConfidence float64 `json:"minConfidence"`
	DailyCap      int     `json:"dailyCap"`
}

var (
	repoRE      = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	hostPortRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?:[0-9]{1,5}$`)
	hostRE      = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`) // a DNS name, an optional port
	schedNameRE = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
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
	c := Config{Budgets: Budgets{EnforceTask: true}} // decoding keeps a default the file omits
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if c.Caps.MaxPendingMinutes == 0 {
		c.Caps.MaxPendingMinutes = defaultMaxPendingMinutes // the Pending bound never stays open (P)
	}
	if c.Budgets.FactoryDaily == 0 {
		c.Budgets.FactoryDaily = defaultFactoryDaily // OD-10
	}
	if c.Budgets.HumanDaily == 0 {
		c.Budgets.HumanDaily = defaultHumanDaily
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
	if c.ControlIssue < 0 {
		bad("controlIssue must be 0 or more")
	}
	if len(c.Maintainers) == 0 {
		bad("maintainers is empty: nobody could start a task")
	}
	// A maintainer starts tasks and revisions (Δ5): an App listed here could drive the factory
	// with its own output. The merger App joins this list when it is configured (phase 7).
	for _, m := range c.Maintainers {
		if strings.HasSuffix(strings.ToLower(m), "[bot]") || strings.EqualFold(m, c.FactoryLogin) || strings.EqualFold(m, c.AgentsLogin) ||
			strings.EqualFold(m+"[bot]", c.FactoryLogin) || strings.EqualFold(m+"[bot]", c.AgentsLogin) {
			bad("maintainer %q is an App: a bot never starts or revises a task", m)
		}
	}
	for _, f := range []struct{ key, value string }{
		{"triggerLabel", c.TriggerLabel}, {"factoryLogin", c.FactoryLogin}, {"agentsLogin", c.AgentsLogin},
		{"roomsURL", c.RoomsURL}, {"broker.url", c.Broker.URL}, {"broker.caFile", c.Broker.CAFile},
		{"broker.tokenFile", c.Broker.TokenFile}, {"github.appIDFile", c.GitHub.AppIDFile},
		{"github.privateKeyFile", c.GitHub.PrivateKeyFile}, {"meter.url", c.Meter.URL}, {"meter.query", c.Meter.Query},
		{"meter.logsURL", c.Meter.LogsURL}, {"meter.throttleQuery", c.Meter.ThrottleQuery},
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
	if c.Triage.ClassifierURL == "" {
		bad("triage.classifierURL is required")
	}
	if c.Triage.ControlPercent < 0 || c.Triage.ControlPercent > 100 {
		bad("triage.controlPercent must be 0..100")
	}
	for _, name := range []string{"docs-links", "revert"} {
		if _, ok := c.Classes[name]; !ok {
			bad("class %s is required (OD-8)", name)
		}
	}
	for name, cl := range c.Classes {
		if cl.Live && cl.Shadow {
			bad("class %s: live and shadow never together (R32)", name)
		}
	}
	// R16: the merger App's key is both files or neither — a half pair cannot mint a token —
	// and both are required once any class is live or shadow, whose decision reads checks
	// through it.
	var anyMerge bool
	for _, cl := range c.Classes {
		anyMerge = anyMerge || cl.Live || cl.Shadow
	}
	switch {
	case (c.GitHub.MergerAppIDFile == "") != (c.GitHub.MergerKeyFile == ""):
		bad("github.mergerAppIDFile and github.mergerKeyFile are both set or both empty")
	case anyMerge && c.GitHub.MergerAppIDFile == "":
		bad("github.mergerAppIDFile: required by a live or shadow class")
	}
	if _, ok := c.Classes["review"]; ok {
		bad("review is the implicit class of everything else; do not declare it")
	}
	// §1 schedules. The name keys every task it starts, so names are unique and C2-shaped;
	// the cron must parse to have slots at all.
	seenSchedule := map[string]bool{}
	for _, e := range c.Schedules {
		switch {
		case !schedNameRE.MatchString(e.Name):
			bad("schedule name %q must match ^[a-z0-9-]{1,40}$", e.Name)
		case seenSchedule[e.Name]:
			bad("schedule name %q is used twice", e.Name)
		}
		seenSchedule[e.Name] = true
		if _, err := cron.ParseStandard(e.Cron); err != nil {
			bad("schedule %s: cron %q does not parse", e.Name, e.Cron)
		}
		if e.Class != "review" {
			if _, ok := c.Classes[e.Class]; !ok {
				bad("schedule %s: class %q is review or a declared class", e.Name, e.Class)
			}
		}
		if e.Text == "" || len(e.Text) > c.Caps.MaxTextBytes {
			bad("schedule %s: text must be 1..%d bytes", e.Name, c.Caps.MaxTextBytes)
		}
		if e.Probe != "" && e.Probe != "renovate-red" {
			bad("schedule %s: probe %q is not renovate-red", e.Name, e.Probe)
		}
		if e.DataClass != "" && e.DataClass != "public" && e.DataClass != "internal" {
			bad("schedule %s: dataClass %q is public or internal", e.Name, e.DataClass)
		}
	}
	if c.Caps.ActiveTasks < 1 || c.Caps.ConcurrentRuns < 1 || c.Caps.TasksPerDay < 1 {
		bad("caps must be positive")
	}
	if c.Caps.MaxTextBytes < 1 || c.Caps.MaxTextBytes > MaxTextCeiling {
		bad("caps.maxTextBytes must be 1..%d (R6)", MaxTextCeiling)
	}
	if c.Caps.MaxPendingMinutes < minMaxPendingMinutes || c.Caps.MaxPendingMinutes > maxRunMinutes {
		bad("caps.maxPendingMinutes must be %d..%d (P)", minMaxPendingMinutes, maxRunMinutes)
	}
	if c.Caps.AwaitingHumanWIP < 1 {
		bad("caps.awaitingHumanWIP must be positive")
	}
	if c.Budgets.FactoryDaily < 1 {
		bad("budgets.factoryDaily must be positive")
	}
	if c.Budgets.HumanDaily < 1 {
		bad("budgets.humanDaily must be positive")
	}
	// §5.1's arming and §6.4's rollback. The merge gate is as required as the rest of the
	// file: a config that cannot say which checks gate the arming or who arms fails its rollout.
	if len(c.Merge.RequiredChecks) == 0 {
		bad("merge.requiredChecks is empty: arming waits for every context classic protection requires")
	}
	if len(c.Merge.VerifyChecks) == 0 {
		bad("merge.verifyChecks is empty: §6.4 watches these on the merge commit")
	}
	if m := c.Merge; !slices.Contains(m.RequiredChecks, m.LeakScanCheck) || !slices.Contains(m.VerifyChecks, m.LeakScanCheck) {
		bad("merge.leakScanCheck %q must be in merge.requiredChecks and merge.verifyChecks (R42)", c.Merge.LeakScanCheck)
	}
	for _, f := range []struct{ key, value string }{
		{"merge.policyBotLogin", c.Merge.PolicyBotLogin}, {"merge.mergerLogin", c.Merge.MergerLogin},
	} {
		if !strings.HasSuffix(strings.ToLower(f.value), "[bot]") {
			bad("%s %q must be a bot login", f.key, f.value)
		}
	}
	if c.Merge.AutoMergesPerDay < 0 {
		bad("merge.autoMergesPerDay must be 0 or more")
	}
	if c.Merge.FixRuns < 0 {
		bad("merge.fixRuns must be 0 or more")
	}
	if c.Merge.VerifyFor.Duration <= 0 {
		bad("merge.verifyFor must be positive")
	}
	if c.Merge.RevertWindow.Duration <= 0 {
		bad("merge.revertWindow must be positive")
	}
	if b := c.Merge.Breaker; b.Window < 1 || b.MaxReverts < 1 || b.MaxReverts > b.Window {
		bad("merge.breaker needs 1 ≤ maxReverts ≤ window (R41)")
	}
	// The run-request API (§4): the factory binary serves it from Task 5.4's wiring, so its
	// block is as required as the rest of this file. A human caller is verified against the
	// ZITADEL issuer and one of the rooms client ids; a system caller pairs issuer with JWKS.
	for _, f := range []struct{ key, value string }{
		{"api.listen", c.API.Listen}, {"api.humanIssuer", c.API.HumanIssuer}, {"api.humanJWKS", c.API.HumanJWKS},
	} {
		if f.value == "" {
			bad("%s is required", f.key)
		}
	}
	if len(c.API.Repositories) == 0 {
		bad("api.repositories is empty: with none listed the API admits no repository")
	}
	for _, r := range c.API.Repositories {
		if !repoRE.MatchString(r) {
			bad("api.repository %q is not owner/name", r)
		}
	}
	if len(c.API.ClientIDFiles) == 0 {
		bad("api.clientIDFiles is empty: the rooms-proxy and roomctl client ids name the human callers")
	}
	if (c.API.SystemIssuer == "") != (c.API.SystemJWKS == "") {
		bad("api.systemIssuer and api.systemJWKS are both set or both empty (R23)")
	}
	// §1's RunLore intake (FA-8): every replica serves it, like the API, so its block is as
	// required as api's. OD-9: the bar is real and the cap is stated; 0 admits nothing.
	if c.RunLore.Listen == "" {
		bad("runlore.listen is required")
	}
	if c.RunLore.TokenFile == "" {
		bad("runlore.tokenFile is required")
	}
	if c.RunLore.MinConfidence <= 0 || c.RunLore.MinConfidence > 1 {
		bad("runlore.minConfidence must be in (0, 1] (OD-9)")
	}
	if c.RunLore.DailyCap < 0 {
		bad("runlore.dailyCap must be 0 or more")
	}
	if c.Tracing.OTLPEndpoint != "" && !hostPortRE.MatchString(c.Tracing.OTLPEndpoint) {
		bad("tracing.otlpEndpoint %q is not host:port", c.Tracing.OTLPEndpoint)
	}
	return errors.Join(errs...)
}
