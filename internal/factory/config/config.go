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
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
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
	// RefUSDPerMToken is design D11's reference token, $1.70 per million: a tier that sets no
	// runUSD or taskUSD while meter.prices is set caps at its runTokens or taskTokens of it, so
	// the standard tier's 1.5 M and 3 M become $2.55 and $5.10 (OD-6 (b), J-AC1).
	RefUSDPerMToken = 1.70
	// MaxTextCeiling leaves room for the preamble in AgentRun task.text (16384, R6).
	MaxTextCeiling = 14336
	// maxRunMinutes is SP1's XRD maximum for budget.maxMinutes.
	maxRunMinutes = 480
	// defaultMaxPendingMinutes bounds a Pending run when the config omits its own (P).
	defaultMaxPendingMinutes = 30
	// minMaxPendingMinutes keeps the Pending bound meaningful: under five minutes a busy cluster
	// would escalate healthy tasks.
	minMaxPendingMinutes = 5
	// defaultEscalatedHours: a week is long enough for a maintainer to come back from leave.
	defaultEscalatedHours = 168
	// OD-10's daily token budgets, when the config omits them: R3 wants a week of shadow
	// numbers first, so the ceilings exist even before an operator writes them down.
	defaultFactoryDaily = 25_000_000
	defaultHumanDaily   = 5_000_000
	// Disruption design §4: how many runs of one task the factory resumes on its own by default,
	// and the most a config may ask for.
	defaultResumesPerTask = 2
	maxResumesPerTask     = 5
	// The platform's values for the deployment facts an older file omits.
	defaultAdminGroup  = "agents-admin"
	defaultMemberGroup = "agents-member"
	defaultProbeAuthor = "renovate[bot]"
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
	// Repositories are the repositories the factory takes issues from, each with its own
	// maintainers, label, classes, merge gate and schedules (v2 D1).
	Repositories []Repository `json:"repositories,omitempty"`
	// Control is the global kill switch's issue (§6.1, v2 D2): one issue stops every repository.
	Control Control `json:"control,omitzero"`

	// The legacy single-repository shape (v2 D1), read as a one-entry Repositories for one minor
	// release and refused beside it. Parse moves these into Repositories and Control and empties
	// them, so code reads Repos and Control, never these.
	Repository   string           `json:"repository,omitempty"`
	Maintainers  []string         `json:"maintainers,omitempty"`
	TriggerLabel string           `json:"triggerLabel,omitempty"`
	ControlIssue int              `json:"controlIssue,omitempty"`
	Classes      map[string]Class `json:"classes,omitempty"`
	Merge        Merge            `json:"merge,omitzero"`
	Schedules    []Schedule       `json:"schedules,omitempty"`

	FactoryLogin string              `json:"factoryLogin"`
	AgentsLogin  string              `json:"agentsLogin"`
	RoomsURL     string              `json:"roomsURL"`
	Broker       Broker              `json:"broker"`
	GitHub       GitHub              `json:"github"`
	Poll         Poll                `json:"poll"`
	Defaults     Defaults            `json:"defaults"`
	Triage       Triage              `json:"triage"`
	Tiers        map[string]Tier     `json:"tiers"`
	Templates    map[string]Template `json:"templates"`
	Caps         Caps                `json:"caps"`
	Budgets      Budgets             `json:"budgets"`
	Resume       Resume              `json:"resume"`
	Meter        Meter               `json:"meter"`
	Tracing      Tracing             `json:"tracing"`
	API          API                 `json:"api"`
	RunLore      RunLore             `json:"runlore"`
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

// MaxRepositories bounds repositories (v2 D1): a repository metric label keeps at most this many
// values, and every entry is polled on the GitHub budget the whole factory shares.
const MaxRepositories = 8

// Repository is one repository the factory works on, with the settings that are its own (v2 D2).
type Repository struct {
	Name         string           `json:"name"` // owner/name
	Maintainers  []string         `json:"maintainers"`
	TriggerLabel string           `json:"triggerLabel"`
	Classes      map[string]Class `json:"classes"`
	Merge        Merge            `json:"merge"`
	Schedules    []Schedule       `json:"schedules,omitempty"`
}

// Control is the pinned issue whose factory/stop label is the global stop (§6.1). Issue 0: none,
// and only the stop object stops the factory.
type Control struct {
	Repository string `json:"repository,omitempty"`
	Issue      int    `json:"issue,omitempty"`
}

// IsMaintainer is whether login is one of the repository's maintainers; GitHub logins are
// case-insensitive.
func (r *Repository) IsMaintainer(login string) bool {
	return slices.ContainsFunc(r.Maintainers, func(m string) bool { return strings.EqualFold(m, login) })
}

// NeedsMerger reports whether a class of the repository is live or in shadow.
func (r *Repository) NeedsMerger() bool {
	for _, cl := range r.Classes {
		if cl.Live || cl.Shadow {
			return true
		}
	}
	return false
}

// AnyLive reports whether a class of the repository is live.
func (r *Repository) AnyLive() bool {
	for _, cl := range r.Classes {
		if cl.Live {
			return true
		}
	}
	return false
}

// Repos is the repository list. A Config built by hand in the legacy shape, without Parse, reads
// as the one entry Parse would have made of it.
func (c *Config) Repos() []Repository {
	if len(c.Repositories) > 0 || reflect.ValueOf(c.legacyRepository()).IsZero() {
		return c.Repositories
	}
	return []Repository{c.legacyRepository()}
}

func (c *Config) legacyRepository() Repository {
	return Repository{Name: c.Repository, Maintainers: c.Maintainers, TriggerLabel: c.TriggerLabel,
		Classes: c.Classes, Merge: c.Merge, Schedules: c.Schedules}
}

// Primary is the first repository's name: RunLore's, until its config names one (v2 D6).
func (c *Config) Primary() string {
	if rs := c.Repos(); len(rs) > 0 {
		return rs[0].Name
	}
	return ""
}

// RepositoryFor is name's entry, matched case-insensitively as GitHub matches it. With one entry
// it is that entry whatever name says, as one set of settings governed every task before the
// list (v2 D1). With several, an unlisted name gets an entry with no maintainer and no class.
func (c *Config) RepositoryFor(name string) *Repository {
	rs := c.Repos()
	if i := slices.IndexFunc(rs, func(r Repository) bool { return strings.EqualFold(r.Name, name) }); i >= 0 {
		return &rs[i]
	}
	if len(rs) == 1 {
		return &rs[0]
	}
	return &Repository{Name: name}
}

// ControlFor is the kill switch's issue; Issue 0 when there is none.
func (c *Config) ControlFor() Control {
	if c.Control.Issue == 0 && c.ControlIssue > 0 {
		return Control{Repository: c.Repository, Issue: c.ControlIssue}
	}
	return c.Control
}

// NeedsMerger reports whether a class of any repository is live or in shadow: deciding it reads
// checks through the merger App, so its key is then mandatory. With every class off it is optional.
func (c *Config) NeedsMerger() bool {
	return slices.ContainsFunc(c.Repos(), func(r Repository) bool { return r.NeedsMerger() })
}

// AnyLive reports whether a class of any repository is live, so the merger App merges and labels (v2 D1).
func (c *Config) AnyLive() bool {
	return slices.ContainsFunc(c.Repos(), func(r Repository) bool { return r.AnyLive() })
}

// AnyCheckReruns reports whether any repository re-runs a cancelled check, which needs the
// merger App's actions: write.
func (c *Config) AnyCheckReruns() bool {
	return slices.ContainsFunc(c.Repos(), func(r Repository) bool { return r.Merge.CheckReruns > 0 })
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
	// CheckReruns caps the re-runs of a cancelled required check per task; 0 hands it to a human
	// instead. Above 0 the merger App must hold actions: write.
	CheckReruns  int32    `json:"checkReruns"`
	VerifyFor    Duration `json:"verifyFor"`    // 30m of main's CI after an auto-merge
	RevertWindow Duration `json:"revertWindow"` // 7 days for a maintainer's factory/revert
	Breaker      Breaker  `json:"breaker"`
}

// Breaker is the circuit breaker's outcome window (R41, review G5): a class whose last Window
// merges hold MaxReverts reverts is demoted to human review, whatever the config.
type Breaker struct {
	Window     int `json:"window"`
	MaxReverts int `json:"maxReverts"`
}

// Tier is a model and its budgets. RunUSD and TaskUSD cap a run and a task in USD, priced by
// meter.prices, in place of RunTokens and TaskTokens; 0 keeps the token cap. With meter.prices
// set each defaults to RefUSD of its token cap.
type Tier struct {
	Model      string  `json:"model"`
	RunTokens  int64   `json:"runTokens"`
	TaskTokens int64   `json:"taskTokens"`
	RunMinutes int64   `json:"runMinutes"`
	RunUSD     float64 `json:"runUSD,omitempty"`
	TaskUSD    float64 `json:"taskUSD,omitempty"`
}

// RefUSD is tokens reference tokens, in USD to the cent.
func RefUSD(tokens int64) float64 {
	return math.Round(float64(tokens)*RefUSDPerMToken/1e4) / 100
}

// MicroUSD is usd in the micro-dollars the Task CRD and the meter count in.
func MicroUSD(usd float64) int64 { return int64(math.Round(usd * 1e6)) }

// Price is a model's list price in USD per million tokens of each type the gateway reports.
// The gateway's input count includes the cache reads and writes, so each is priced once.
type Price struct {
	Input     float64 `json:"input"`
	CacheRead float64 `json:"cacheRead"`
	// CacheWrite prices cache_creation_input; omitted, the input rate (Z.ai bills no write).
	CacheWrite float64 `json:"cacheWrite,omitempty"`
	Output     float64 `json:"output"`
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
	// EscalatedHours is how long a task may wait Escalated for a maintainer before it is
	// Stopped (#61); otherwise it polls GitHub until its issue or pull request closes.
	// Omitted: 168. An explicit 0: no limit. A pointer tells the two apart.
	EscalatedHours *int `json:"escalatedHours,omitempty"`
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

// Resume bounds the automatic resume of a run lost to its infrastructure (disruption design §4).
type Resume struct {
	// MaxPerTask is how many runs of one task the factory resumes on its own; past it the task
	// escalates as before. Defaulted when omitted.
	MaxPerTask int `json:"maxPerTask"`
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
	// Prices are by the tier's logical model (C5), since the run's claim names that one. Pricing
	// a run also needs Query grouped by gen_ai_token_type; an untyped reply meters in tokens.
	Prices map[string]Price `json:"prices,omitempty"`
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
	// ProbeAuthor is the login whose open PRs the probe looks at; omitted: renovate[bot].
	ProbeAuthor string `json:"probeAuthor,omitempty"`
}

// Groups names the two agents groups the run-request API admits (R23): the broker's config
// carries the same two values.
type Groups struct {
	Admin  string `json:"admin"`
	Member string `json:"member"`
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
	// Groups defaults to agents-admin and agents-member.
	Groups Groups `json:"groups"`
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
	if err := c.normalize(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if c.Caps.MaxPendingMinutes == 0 {
		c.Caps.MaxPendingMinutes = defaultMaxPendingMinutes // the Pending bound never stays open (P)
	}
	if c.Caps.EscalatedHours == nil {
		c.Caps.EscalatedHours = new(defaultEscalatedHours)
	}
	if c.Budgets.FactoryDaily == 0 {
		c.Budgets.FactoryDaily = defaultFactoryDaily // OD-10
	}
	if c.Budgets.HumanDaily == 0 {
		c.Budgets.HumanDaily = defaultHumanDaily
	}
	if c.Resume.MaxPerTask == 0 {
		c.Resume.MaxPerTask = defaultResumesPerTask
	}
	if len(c.Meter.Prices) > 0 { // without prices, a tier keeps its token cap (one release of compatibility)
		for name, t := range c.Tiers {
			if t.RunUSD == 0 {
				t.RunUSD = RefUSD(t.RunTokens)
			}
			if t.TaskUSD == 0 {
				t.TaskUSD = RefUSD(t.TaskTokens)
			}
			c.Tiers[name] = t
		}
	}
	if c.API.Groups.Admin == "" {
		c.API.Groups.Admin = defaultAdminGroup
	}
	if c.API.Groups.Member == "" {
		c.API.Groups.Member = defaultMemberGroup
	}
	for i := range c.Repositories {
		for j := range c.Repositories[i].Schedules {
			if s := &c.Repositories[i].Schedules[j]; s.ProbeAuthor == "" {
				s.ProbeAuthor = defaultProbeAuthor
			}
		}
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	sum := sha256.Sum256(j)
	c.Hash = hex.EncodeToString(sum[:])
	return &c, nil
}

// normalize reads the legacy single-repository keys as a one-entry repositories (v2 D1), and
// refuses a file that mixes the two shapes. The legacy shape keeps its own api.repositories, the
// allowlist it always had; the list shape derives it from the names, so it is never written twice.
func (c *Config) normalize() error {
	if c.Repository == "" {
		var errs []error
		for _, k := range []struct {
			key string
			set bool
		}{
			{"maintainers", c.Maintainers != nil}, {"triggerLabel", c.TriggerLabel != ""}, {"controlIssue", c.ControlIssue != 0},
			{"classes", c.Classes != nil}, {"merge", !reflect.ValueOf(c.Merge).IsZero()}, {"schedules", c.Schedules != nil},
		} {
			if k.set {
				errs = append(errs, fmt.Errorf("%s is per repository: it belongs in a repositories entry (v2 D1)", k.key))
			}
		}
		if c.API.Repositories != nil {
			errs = append(errs, errors.New("api.repositories is derived from repositories (v2 D1): remove it"))
		}
		c.API.Repositories = make([]string, 0, len(c.Repositories))
		for _, r := range c.Repositories {
			c.API.Repositories = append(c.API.Repositories, r.Name)
		}
		return errors.Join(errs...)
	}
	switch {
	case c.Repositories != nil:
		return errors.New("repository is the legacy single-repository shape: write repositories or repository, never both (v2 D1)")
	case c.Control != Control{}:
		return errors.New("control belongs to the repositories shape: the legacy shape writes controlIssue (v2 D1)")
	}
	c.Repositories = []Repository{c.legacyRepository()}
	if c.ControlIssue != 0 {
		c.Control = Control{Repository: c.Repository, Issue: c.ControlIssue}
	}
	c.Repository, c.Maintainers, c.TriggerLabel, c.ControlIssue = "", nil, "", 0
	c.Classes, c.Merge, c.Schedules = nil, Merge{}, nil
	return nil
}

// EscalatedLimit is how long a task may wait Escalated; 0 is no limit, which is also what a
// Config built without Parse means.
func (c *Config) EscalatedLimit() time.Duration {
	if c.Caps.EscalatedHours == nil {
		return 0
	}
	return time.Duration(*c.Caps.EscalatedHours) * time.Hour
}

// Validate reports every problem at once, so one rollout shows them all.
func (c *Config) Validate() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	rs := c.Repos()
	switch {
	case len(rs) == 0:
		bad("repositories is empty: the factory has no repository to work on")
	case len(rs) > MaxRepositories:
		bad("repositories holds %d entries, at most %d (v2 D1)", len(rs), MaxRepositories)
	}
	seenRepo, seenSchedule := map[string]bool{}, map[string]bool{}
	for i := range rs {
		if k := strings.ToLower(rs[i].Name); seenRepo[k] {
			bad("repository %s is listed twice", rs[i].Name)
		} else {
			seenRepo[k] = true
		}
		c.validateRepository(&rs[i], seenSchedule, bad)
	}
	if ctl := c.ControlFor(); ctl.Issue < 0 {
		bad("control.issue must be 0 or more")
	} else if ctl.Issue > 0 && !seenRepo[strings.ToLower(ctl.Repository)] {
		bad("control.repository %q is not a listed repository", ctl.Repository)
	}
	for _, f := range []struct{ key, value string }{
		{"factoryLogin", c.FactoryLogin}, {"agentsLogin", c.AgentsLogin},
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
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
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
		case !finite(t.RunUSD) || t.RunUSD < 0:
			bad("tier %s: runUSD must be a positive amount", name)
		case !finite(t.TaskUSD) || t.TaskUSD < 0:
			bad("tier %s: taskUSD must be a positive amount", name)
		case t.TaskUSD > 0 && t.TaskUSD < t.RunUSD:
			bad("tier %s: taskUSD below runUSD", name)
		case t.RunUSD > 0 || t.TaskUSD > 0:
			// Refused rather than metered at $0: a cap nothing reaches.
			if _, ok := c.Meter.Prices[t.Model]; !ok {
				bad("tier %s: runUSD and taskUSD need meter.prices.%s", name, t.Model)
			}
		}
	}
	for model, p := range c.Meter.Prices {
		switch {
		case !slices.Contains(models(), model):
			bad("meter.prices: %q is not a C5 logical name", model)
		case !finite(p.Input) || !finite(p.CacheRead) || !finite(p.Output) || !finite(p.CacheWrite) ||
			p.Input <= 0 || p.CacheRead <= 0 || p.Output <= 0 || p.CacheWrite < 0:
			bad("meter.prices.%s: input, cacheRead and output must be positive, cacheWrite 0 or more", model)
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
	// R16: the merger App's key is both files or neither — a half pair cannot mint a token —
	// and both are required once any class is live or shadow, whose decision reads checks
	// through it.
	anyMerge := c.NeedsMerger()
	switch {
	case (c.GitHub.MergerAppIDFile == "") != (c.GitHub.MergerKeyFile == ""):
		bad("github.mergerAppIDFile and github.mergerKeyFile are both set or both empty")
	case anyMerge && c.GitHub.MergerAppIDFile == "":
		bad("github.mergerAppIDFile: required by a live or shadow class")
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
	if c.Caps.EscalatedHours != nil && *c.Caps.EscalatedHours < 0 {
		bad("caps.escalatedHours must be 0 (no limit) or more (#61)")
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
	if c.Resume.MaxPerTask < 1 || c.Resume.MaxPerTask > maxResumesPerTask {
		bad("resume.maxPerTask must be 1..%d (disruption design §4)", maxResumesPerTask)
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
	if c.API.Groups.Admin == "" {
		bad("api.groups.admin is required")
	}
	if c.API.Groups.Member == "" {
		bad("api.groups.member is required")
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

// validateRepository reports one entry's problems, each prefixed with its name.
func (c *Config) validateRepository(r *Repository, seenSchedule map[string]bool, report func(string, ...any)) {
	if !repoRE.MatchString(r.Name) {
		report("repository %q is not owner/name", r.Name)
	}
	bad := func(f string, a ...any) { report("%s: %s", r.Name, fmt.Sprintf(f, a...)) }
	if len(r.Maintainers) == 0 {
		bad("maintainers is empty: nobody could start a task")
	}
	// A maintainer starts tasks and revisions (Δ5): an App listed here could drive the factory
	// with its own output. The merger App joins this list when it is configured (phase 7).
	for _, m := range r.Maintainers {
		if strings.HasSuffix(strings.ToLower(m), "[bot]") || strings.EqualFold(m, c.FactoryLogin) || strings.EqualFold(m, c.AgentsLogin) ||
			strings.EqualFold(m+"[bot]", c.FactoryLogin) || strings.EqualFold(m+"[bot]", c.AgentsLogin) {
			bad("maintainer %q is an App: a bot never starts or revises a task", m)
		}
	}
	if r.TriggerLabel == "" {
		bad("triggerLabel is required")
	}
	for _, name := range []string{"docs-links", "revert"} {
		if _, ok := r.Classes[name]; !ok {
			bad("class %s is required (OD-8)", name)
		}
	}
	for name, cl := range r.Classes {
		if cl.Live && cl.Shadow {
			bad("class %s: live and shadow never together (R32)", name)
		}
	}
	if _, ok := r.Classes["review"]; ok {
		bad("review is the implicit class of everything else; do not declare it")
	}
	// §1 schedules. The name keys every task it starts, so names are unique across every
	// repository and C2-shaped;
	// the cron must parse to have slots at all.
	for _, e := range r.Schedules {
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
			if _, ok := r.Classes[e.Class]; !ok {
				bad("schedule %s: class %q is review or a declared class", e.Name, e.Class)
			}
		}
		if e.Text == "" || len(e.Text) > c.Caps.MaxTextBytes {
			bad("schedule %s: text must be 1..%d bytes", e.Name, c.Caps.MaxTextBytes)
		}
		if e.Probe != "" && e.Probe != "renovate-red" {
			bad("schedule %s: probe %q is not renovate-red", e.Name, e.Probe)
		}
		if e.Probe != "" && e.ProbeAuthor == "" {
			bad("schedule %s: probeAuthor is required with a probe", e.Name)
		}
		if e.DataClass != "" && e.DataClass != "public" && e.DataClass != "internal" {
			bad("schedule %s: dataClass %q is public or internal", e.Name, e.DataClass)
		}
	}
	// §5.1's arming and §6.4's rollback. The merge gate is as required as the rest of the
	// file: a config that cannot say which checks gate the arming or who arms fails its rollout.
	if len(r.Merge.RequiredChecks) == 0 {
		bad("merge.requiredChecks is empty: arming waits for every context classic protection requires")
	}
	if len(r.Merge.VerifyChecks) == 0 {
		bad("merge.verifyChecks is empty: §6.4 watches these on the merge commit")
	}
	if m := r.Merge; !slices.Contains(m.RequiredChecks, m.LeakScanCheck) || !slices.Contains(m.VerifyChecks, m.LeakScanCheck) {
		bad("merge.leakScanCheck %q must be in merge.requiredChecks and merge.verifyChecks (R42)", r.Merge.LeakScanCheck)
	}
	for _, f := range []struct{ key, value string }{
		{"merge.policyBotLogin", r.Merge.PolicyBotLogin}, {"merge.mergerLogin", r.Merge.MergerLogin},
	} {
		if !strings.HasSuffix(strings.ToLower(f.value), "[bot]") {
			bad("%s %q must be a bot login", f.key, f.value)
		}
	}
	if r.Merge.AutoMergesPerDay < 0 {
		bad("merge.autoMergesPerDay must be 0 or more")
	}
	if r.Merge.FixRuns < 0 {
		bad("merge.fixRuns must be 0 or more")
	}
	if r.Merge.CheckReruns < 0 {
		bad("merge.checkReruns must be 0 or more")
	}
	if r.Merge.VerifyFor.Duration <= 0 {
		bad("merge.verifyFor must be positive")
	}
	if r.Merge.RevertWindow.Duration <= 0 {
		bad("merge.revertWindow must be positive")
	}
	if b := r.Merge.Breaker; b.Window < 1 || b.MaxReverts < 1 || b.MaxReverts > b.Window {
		bad("merge.breaker needs 1 ≤ maxReverts ≤ window (R41)")
	}
}
