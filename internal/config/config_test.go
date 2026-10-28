// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/runwatch"
)

const valid = `
publicURL: https://rooms.priv.aws.ogenki.io
runIssuers:
  - issuer: https://oidc.eks.eu-west-3.amazonaws.com/id/X
    jwksURL: https://oidc.eks.eu-west-3.amazonaws.com/id/X/keys
    subPattern: '^system:serviceaccount:agents:xplane-run-([a-z2-7]{8})$'
systemIssuer:
  issuer: https://oidc.eks.eu-west-3.amazonaws.com/id/X
  jwksURL: https://oidc.eks.eu-west-3.amazonaws.com/id/X/keys
systemPrincipals:
  system:serviceaccount:agent-system:agent-factory: system:factory
human:
  issuer: https://auth.example.test
  jwksURL: https://auth.example.test/oauth/v2/keys
  clientIDFile: /etc/room-broker/oidc/client-id
  projectIDFile: /etc/room-broker/oidc/project-id
  origin: https://rooms.example.test
  groups:
    admin: agents-admin
    member: agents-member
`

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	c, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.RunIssuers) != 1 || c.SystemPrincipals["system:serviceaccount:agent-system:agent-factory"] != "system:factory" {
		t.Fatalf("%+v", c)
	}
	if c.TLS.CertFile != DefaultCertFile || c.TLS.KeyFile != DefaultKeyFile {
		t.Fatalf("the :8443 pair defaults to the GP-18 mount, got %+v", c.TLS)
	}
	if c.Human.ProjectIDFile != "/etc/room-broker/oidc/project-id" ||
		c.Human.Groups != (GroupsConfig{Admin: "agents-admin", Member: "agents-member"}) {
		t.Fatalf("human: %+v", c.Human)
	}
}

// access is the human.access block, appended to valid.
const access = "  access:\n    readerFile: /etc/room-broker/zitadel-reader/reader.json\n"

// D7: human.access turns the GitHub check on. Absent, it stays nil, and rooms are admins-only.
func TestLoadAccess(t *testing.T) {
	c, err := Load(write(t, valid))
	if err != nil || c.Human.Access != nil {
		t.Fatalf("no access block: %+v, %v", c.Human.Access, err)
	}
	c, err = Load(write(t, valid+access))
	if err != nil || c.Human.Access == nil || c.Human.Access.ReaderFile != "/etc/room-broker/zitadel-reader/reader.json" ||
		c.Human.Access.TTL.Duration != 5*time.Minute {
		t.Fatalf("the TTL defaults to 5m: %+v, %v", c.Human.Access, err)
	}
	c, err = Load(write(t, valid+access+"    ttl: 90s\n"))
	if err != nil || c.Human.Access.TTL.Duration != 90*time.Second {
		t.Fatalf("%+v, %v", c.Human.Access, err)
	}
}

// v2 suspend: off unless the file turns it on, and its idle threshold stays under the attended
// approval's 30 minutes, so a run is suspended before its approval expires.
func TestLoadSuspend(t *testing.T) {
	c, err := Load(write(t, valid))
	if err != nil || c.Suspend.Enabled || c.Suspend.After.Duration != 5*time.Minute {
		t.Fatalf("off, after 5m by default: %+v, %v", c.Suspend, err)
	}
	c, err = Load(write(t, valid+"suspend:\n  enabled: true\n  after: 10m\n"))
	if err != nil || !c.Suspend.Enabled || c.Suspend.After.Duration != 10*time.Minute {
		t.Fatalf("%+v, %v", c.Suspend, err)
	}
	for _, after := range []string{"30s", "26m"} {
		if _, err := Load(write(t, valid+"suspend:\n  after: "+after+"\n")); err == nil || !strings.Contains(err.Error(), "suspend.after") {
			t.Errorf("after %s: %v", after, err)
		}
	}
}

// I8: the agents' :8090 listener has a top-level block of its own, so it cannot be confused with
// human.mcp. Absent, it starts as every existing deployment expects, and ROOMS_MCP_KEY alone
// decides who may call it; an explicit false is the opt-out.
func TestRoomToolsDefaultAndStrictDecode(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"the existing bytes, no block: today's :8090 listener", valid, true},
		{"an empty block enables", valid + "roomTools: {}\n", true},
		{"enabled: false is the opt-out", valid + "roomTools:\n  enabled: false\n", false},
		{"enabled: true is explicit", valid + "roomTools:\n  enabled: true\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(write(t, tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if got := c.RoomToolsEnabled(); got != tc.want {
				t.Fatalf("roomTools enabled = %v, want %v", got, tc.want)
			}
		})
	}
	// Strict decoding reaches the new block too: a typo, or a "boolean" that is a string, fails the
	// rollout rather than silently starting a listener the operator meant to close.
	for _, tc := range []struct{ name, body string }{
		{"a misspelt key", valid + "roomTools:\n  enable: false\n"},
		{"a string boolean", valid + "roomTools:\n  enabled: \"false\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(write(t, tc.body)); err == nil {
				t.Fatal("want a decode error")
			}
		})
	}
}

func TestLoadRefuses(t *testing.T) {
	replace := func(old, repl string) string { return strings.Replace(valid, old, repl, 1) }
	for _, c := range []struct {
		name, body, want string
	}{
		{"a bad subPattern fails the rollout, not the first request",
			"runIssuers: [{issuer: x, jwksURL: y, subPattern: '('}]", "subPattern"},
		{"a subPattern without its one capture group",
			replace("([a-z2-7]{8})", "[a-z2-7]{8}"), "one capture group"},
		{"a subPattern with two capture groups",
			replace("([a-z2-7]{8})", "(([a-z2-7]{8}))"), "one capture group"},
		{"an unknown field is a typo", valid + "systemPrincipal: {}\n", "unknown field"},
		{"no run issuer", "systemIssuer: {issuer: x, jwksURL: https://x/keys}\n", "runIssuers"},
		{"a JWKS URL that is not https", replace("jwksURL: https://oidc.eks.eu-west-3.amazonaws.com/id/X/keys\n    subPattern",
			"jwksURL: http://oidc.eks.eu-west-3.amazonaws.com/id/X/keys\n    subPattern"), "https"},
		{"no system issuer", replace("systemIssuer:\n  issuer: https://oidc.eks.eu-west-3.amazonaws.com/id/X\n", "systemIssuer:\n"), "systemIssuer"},
		{"a system principal that is not system:*", replace(": system:factory", ": human:alice"), "system:"},
		{"not YAML", "runIssuers: [", "config"},
		// Ruling AS-a: ZITADEL mints the ids, so they are files read at use, never literals.
		{"a literal project id", replace("  projectIDFile:", "  projectID: p\n  projectIDFile:"), "unknown field"},
		{"no human issuer", replace("  issuer: https://auth.example.test\n", ""), "human: issuer"},
		{"a human JWKS URL that is not https", replace("jwksURL: https://auth.example.test", "jwksURL: http://auth.example.test"), "human: jwksURL"},
		{"no client id file", replace("  clientIDFile: /etc/room-broker/oidc/client-id\n", ""), "human.clientIDFile"},
		{"no project id file", replace("  projectIDFile: /etc/room-broker/oidc/project-id\n", ""), "human.projectIDFile"},
		{"no origin", replace("  origin: https://rooms.example.test\n", ""), "human.origin"},
		{"an origin with a path", replace("origin: https://rooms.example.test", "origin: https://rooms.example.test/r"), "human.origin"},
		{"no admin group", replace("    admin: agents-admin\n", ""), "human.groups.admin"},
		{"no member group", replace("    member: agents-member\n", ""), "human.groups.member"},
		{"one group for both", replace("member: agents-member", "member: agents-admin"), "human.groups"},
		// D7: a revoked read lags by the cache at most.
		{"an access TTL over 5 minutes", valid + access + "    ttl: 6m\n", "human.access.ttl"},
		{"a negative access TTL", valid + access + "    ttl: -1m\n", "human.access.ttl"},
		{"an access TTL without its unit", valid + access + "    ttl: 300\n", "duration"},
		{"no reader file", valid + "  access:\n    ttl: 5m\n", "human.access.readerFile"},
		{"the reader's PAT over plain http", strings.Replace(valid, "  issuer: https://auth.example.test\n", "  issuer: http://auth.example.test\n", 1) + access,
			"human.issuer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(write(t, c.body))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error naming %q", err, c.want)
			}
		})
	}
	t.Run("a missing file", func(t *testing.T) {
		if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
			t.Fatal("a missing config must fail startup")
		}
	})
}

func TestLoadRuns(t *testing.T) {
	c, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Runs != runwatch.DefaultKind {
		t.Fatalf("no runs block: %+v", c.Runs)
	}
	const native = "runs:\n  group: agents.ogenki.io\n  namePrefix: run-\n"
	// The xplane-run- issuer cannot authenticate a run-<id> ServiceAccount: refused at start.
	if _, err := Load(write(t, valid+native)); err == nil || !strings.Contains(err.Error(), "runs") || !strings.Contains(err.Error(), "runIssuers") {
		t.Fatalf("want a runs/runIssuers error, got %v", err)
	}
	c, err = Load(write(t, strings.Replace(valid, "agents:xplane-run-", "agents:run-", 1)+native))
	if err != nil {
		t.Fatal(err)
	}
	if c.Runs != (runwatch.Kind{Group: "agents.ogenki.io", Namespace: "agents", NamePrefix: "run-"}) {
		t.Fatalf("runs %+v", c.Runs)
	}
	if _, err := Load(write(t, valid+"runs:\n  namePrefix: run\n")); err == nil || !strings.Contains(err.Error(), "runs:") {
		t.Fatalf("want a runs: error, got %v", err)
	}
}

const systemIssuerBlock = `systemIssuer:
  issuer: https://oidc.eks.eu-west-3.amazonaws.com/id/X
  jwksURL: https://oidc.eks.eu-west-3.amazonaws.com/id/X/keys
`

const systemPrincipalsBlock = `systemPrincipals:
  system:serviceaccount:agent-system:agent-factory: system:factory
`

// Only the factory calls with a system token, and the factory is off in the quickstart: an absent
// systemIssuer is legal, and the broker still answers 401 on the system routes. Principals without
// it authenticate nobody, so they are refused by name.
func TestSystemIssuerOptional(t *testing.T) {
	none := strings.Replace(strings.Replace(valid, systemIssuerBlock, "", 1), systemPrincipalsBlock, "", 1)
	c, err := Load(write(t, none))
	if err != nil {
		t.Fatalf("no systemIssuer and no principals must load: %v", err)
	}
	if c.SystemIssuer != nil || len(c.SystemPrincipals) != 0 {
		t.Fatalf("absent stays absent: issuer %+v, principals %v", c.SystemIssuer, c.SystemPrincipals)
	}
	null := strings.Replace(none, "human:\n", "systemIssuer: null\nhuman:\n", 1)
	if c, err := Load(write(t, null)); err != nil || c.SystemIssuer != nil {
		t.Fatalf("systemIssuer: null must load as absent: %+v, %v", c.SystemIssuer, err)
	}
	empty := strings.Replace(none, "human:\n", "systemIssuer: {}\nhuman:\n", 1)
	if c, err := Load(write(t, empty)); err != nil || c.SystemIssuer != nil {
		t.Fatalf("an all-empty systemIssuer block is the absent one: %+v, %v", c.SystemIssuer, err)
	}
	half := strings.Replace(valid, systemIssuerBlock, "systemIssuer:\n  issuer: https://x\n", 1)
	if _, err := Load(write(t, half)); err == nil {
		t.Fatal("an issuer without jwksURL must be refused")
	}
	principals := strings.Replace(valid, systemIssuerBlock, "", 1)
	_, err = Load(write(t, principals))
	if err == nil || !strings.Contains(err.Error(), "systemPrincipals") || !strings.Contains(err.Error(), "systemIssuer") {
		t.Fatalf("systemPrincipals without systemIssuer must be refused by name, got %v", err)
	}
}

// The existing JWKS bytes keep loading to the same values: the mode is the default spelled out,
// not a new requirement on a deployed broker's config.
func TestExistingJWKSConfigUnchanged(t *testing.T) {
	c, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.RunIssuers) != 1 || len(c.SystemPrincipals) != 1 || c.SystemIssuer == nil {
		t.Fatalf("%+v", c)
	}
	run := c.RunIssuers[0]
	if run.Issuer != "https://oidc.eks.eu-west-3.amazonaws.com/id/X" ||
		run.JWKSURL != "https://oidc.eks.eu-west-3.amazonaws.com/id/X/keys" ||
		run.SubPattern != `^system:serviceaccount:agents:xplane-run-([a-z2-7]{8})$` {
		t.Fatalf("runIssuers[0] %+v", run)
	}
	sys := c.SystemIssuer
	if sys.Issuer != "https://oidc.eks.eu-west-3.amazonaws.com/id/X" ||
		sys.JWKSURL != "https://oidc.eks.eu-west-3.amazonaws.com/id/X/keys" || sys.SubPattern != "" {
		t.Fatalf("systemIssuer %+v", sys)
	}
	if got := c.SystemPrincipals["system:serviceaccount:agent-system:agent-factory"]; got != "system:factory" {
		t.Fatalf("systemPrincipals %v", c.SystemPrincipals)
	}
	if c.Human.Issuer != "https://auth.example.test" || c.Human.EffectiveMode() != HumanModeOIDC {
		t.Fatalf("human %+v", c.Human)
	}
}

// A mode names how the issuer is verified: jwks is the default the existing bytes get, tokenreview
// asks the apiserver for the fixed audience and carries no issuer URL. The audience is a constant,
// never a config field. Either mode serves the system issuer too.
func TestIssuerModesAndDefaults(t *testing.T) {
	c, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.RunIssuers[0].EffectiveMode(); got != IssuerModeJWKS || c.RunIssuers[0].Mode != IssuerModeJWKS {
		t.Fatalf("a run issuer with no mode is jwks: %q / %q", c.RunIssuers[0].Mode, got)
	}
	if c.SystemIssuer == nil || c.SystemIssuer.Mode != IssuerModeJWKS || c.SystemIssuer.EffectiveMode() != IssuerModeJWKS {
		t.Fatalf("a system issuer with no mode is jwks: %+v", c.SystemIssuer)
	}
	explicit := strings.Replace(valid, "    subPattern:", "    mode: jwks\n    subPattern:", 1)
	if c, err := Load(write(t, explicit)); err != nil || c.RunIssuers[0].Mode != IssuerModeJWKS {
		t.Fatalf("mode: jwks is the default spelled out: %+v, %v", c.RunIssuers, err)
	}
	tr := "runIssuers:\n  - mode: tokenreview\n    subPattern: '^system:serviceaccount:agents:xplane-run-([a-z2-7]{8})$'\n"
	if c, err := Load(write(t, replaceRunIssuers(valid, tr))); err != nil || c.RunIssuers[0].EffectiveMode() != IssuerModeTokenReview {
		t.Fatalf("a tokenreview run issuer must load: %+v, %v", c.RunIssuers, err)
	}
	// The system issuer accepts either mode, and takes only what that mode uses.
	for name, tc := range map[string]struct {
		block string
		want  string // "", or the field the refusal must name
	}{
		"tokenreview":                {"systemIssuer:\n  mode: tokenreview\n", ""},
		"tokenreview with an issuer": {"systemIssuer:\n  mode: tokenreview\n  issuer: https://x\n", "systemIssuer"},
		"tokenreview with a pattern": {"systemIssuer:\n  mode: tokenreview\n  subPattern: '^x-(\\w+)$'\n", "systemIssuer"},
		"unknown mode":               {"systemIssuer:\n  mode: oidc\n", "systemIssuer"},
		"jwks without a jwksURL":     {"systemIssuer:\n  issuer: https://x\n", "systemIssuer"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, strings.Replace(valid, systemIssuerBlock, tc.block, 1)))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("%s must load: %v", tc.block, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s: want a %s refusal, got %v", tc.block, tc.want, err)
			}
		})
	}
	// No audience field: TokenReview's audiences are authn.AudienceRun and authn.AudienceSystem.
	audience := strings.Replace(valid, "    subPattern:", "    audience: room-broker\n    subPattern:", 1)
	if _, err := Load(write(t, audience)); err == nil || !strings.Contains(err.Error(), "audience") {
		t.Fatalf("runIssuers[].audience must be refused as unknown, got %v", err)
	}
}

// replaceRunIssuers swaps valid's runIssuers block for repl.
func replaceRunIssuers(body, repl string) string {
	const old = "runIssuers:\n  - issuer: https://oidc.eks.eu-west-3.amazonaws.com/id/X\n    jwksURL: https://oidc.eks.eu-west-3.amazonaws.com/id/X/keys\n    subPattern: '^system:serviceaccount:agents:xplane-run-([a-z2-7]{8})$'\n"
	if !strings.Contains(body, old) {
		panic("fixture's runIssuers block changed")
	}
	return strings.Replace(body, old, repl, 1)
}

func TestRunIssuerTokenReviewMode(t *testing.T) {
	tr := "runIssuers:\n  - mode: tokenreview\n    subPattern: '^system:serviceaccount:agents:xplane-run-([a-z2-7]{8})$'\n"
	if _, err := Load(write(t, replaceRunIssuers(valid, tr))); err != nil {
		t.Fatalf("tokenreview issuer must load: %v", err)
	}
	withURL := "runIssuers:\n  - mode: tokenreview\n    issuer: https://x\n    subPattern: '^system:serviceaccount:agents:xplane-run-([a-z2-7]{8})$'\n"
	if _, err := Load(write(t, replaceRunIssuers(valid, withURL))); err == nil {
		t.Fatal("tokenreview with an issuer URL must be refused")
	}
	bad := "runIssuers:\n  - mode: oidc\n    subPattern: '^x-([a-z2-7]{8})$'\n"
	if _, err := Load(write(t, replaceRunIssuers(valid, bad))); err == nil {
		t.Fatal("an unknown mode must be refused")
	}
}

// localConfig is valid with the human block replaced by mode: local.
var localConfig = func() string {
	head, _, _ := strings.Cut(valid, "human:\n")
	return strings.Replace(head, "https://rooms.priv.aws.ogenki.io", "http://localhost:8080", 1) + "human:\n  mode: local\n"
}()

func TestHumanLocalMode(t *testing.T) {
	c, err := Load(write(t, localConfig))
	if err != nil {
		t.Fatalf("local mode must load: %v", err)
	}
	if c.Human.Groups.Admin == "" || c.Human.Groups.Member == "" || c.Human.Origin != "http://localhost:8080" {
		t.Fatalf("local mode defaults the group names and origin: %+v", c.Human)
	}
	mixed := strings.Replace(localConfig, "mode: local", "mode: local\n  issuer: https://idp", 1)
	if _, err := Load(write(t, mixed)); err == nil {
		t.Fatal("local mode with OIDC fields must be refused")
	}
	if _, err := Load(write(t, strings.Replace(localConfig, "http://localhost:8080", "http://rooms.example.com", 1))); err == nil {
		t.Fatal("local mode with a non-loopback publicURL must be refused")
	}
	for _, extra := range []string{"  origin: https://elsewhere.example\n", "  groups:\n    admin: a\n    member: b\n"} {
		if _, err := Load(write(t, localConfig+extra)); err == nil {
			t.Fatalf("local mode must refuse %q", extra)
		}
	}
	for _, u := range []string{"http://localhost:8080/", "https://localhost:8080", "http://localhost", "http://user@localhost:8080", "http://localhost:8080/x"} {
		if _, err := Load(write(t, strings.Replace(localConfig, "http://localhost:8080", u, 1))); err == nil {
			t.Fatalf("publicURL %q must be refused in local mode", u)
		}
	}
	if _, err := Load(write(t, strings.Replace(localConfig, "http://localhost:8080", "http://127.0.0.1:9000", 1))); err != nil {
		t.Fatalf("127.0.0.1 with a port: %v", err)
	}
	if _, err := Load(write(t, strings.Replace(valid, "human:\n", "human:\n  mode: bogus\n", 1))); err == nil {
		t.Fatal("an unknown mode must be refused")
	}
}

// human.dashboards is optional in either mode, and local mode checks it as OIDC mode does.
func TestHumanLocalModeDashboards(t *testing.T) {
	c, err := Load(write(t, localConfig+"  dashboards:\n"+dashboard("Room", "'https://grafana.example.com/?r={room}'")))
	if err != nil || len(c.Human.Dashboards) != 1 {
		t.Fatalf("local mode takes dashboards: %+v, %v", c.Human.Dashboards, err)
	}
	if _, err := Load(write(t, localConfig+"  dashboards:\n"+dashboard("Room", "'http://grafana.example.com/'"))); err == nil ||
		!strings.Contains(err.Error(), "human.dashboards[0]") {
		t.Fatalf("local mode must check dashboards: %v", err)
	}
}

// Local mode has one fixed loopback user and no IdP to name a sponsored principal or a repository's
// GitHub identity, so every human.mcp feature is refused by name: the rollout fails rather than
// carrying a switch the broker can never honour.
func TestHumanLocalModeRefusesMCP(t *testing.T) {
	tests := []struct {
		name, mcp, want string
	}{
		{"enabled", "  mcp:\n    enabled: true\n", "human.mcp.enabled: mode local"},
		{"sponsored", "  mcp:\n    sponsored:\n      - name: slack-app\n        clientIDFile: /etc/room-broker/sponsored/slack-app\n",
			"human.mcp.sponsored: mode local"},
		{"sessionStart", "  mcp:\n    sessionStart: [Smana/a]\n", "human.mcp.sessionStart: mode local"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, localConfig+tc.mcp))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
	// The switch is off by default, and an empty block changes nothing.
	if _, err := Load(write(t, localConfig+"  mcp: {}\n")); err != nil {
		t.Fatalf("an empty mcp block: %v", err)
	}
	// OIDC mode keeps validating /mcp as before: the refusal is local mode's alone.
	roomctl := strings.Replace(valid, "  projectIDFile:", "  roomctlClientIDFile: /etc/room-broker/roomctl/client-id\n  projectIDFile:", 1)
	if _, err := Load(write(t, roomctl+"  mcp:\n    enabled: true\n")); err != nil {
		t.Fatalf("oidc mode with /mcp on: %v", err)
	}
}

// localBare is local mode and nothing else: no publicURL (the port-forward's origin is the default)
// and no OIDC field.
const localBare = `
runIssuers:
  - issuer: https://oidc.eks.eu-west-3.amazonaws.com/id/X
    jwksURL: https://oidc.eks.eu-west-3.amazonaws.com/id/X/keys
    subPattern: '^system:serviceaccount:agents:xplane-run-([a-z2-7]{8})$'
human:
  mode: local
`

// localAt is localBare with publicURL set.
func localAt(u string) string { return "publicURL: " + u + "\n" + localBare }

// The bytes every deployed broker already carries need no edit: an empty mode is OIDC, and the
// defaults those values had before local mode existed are the ones they still get.
func TestExistingConfigDefaultsUnchanged(t *testing.T) {
	c, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Human.Mode != HumanModeOIDC {
		t.Fatalf("mode %q: an unset mode is oidc", c.Human.Mode)
	}
	if c.Human.EffectiveGroups() != c.Human.Groups ||
		c.Human.EffectiveGroups() != (GroupsConfig{Admin: "agents-admin", Member: "agents-member"}) {
		t.Fatalf("oidc keeps the configured groups: %+v", c.Human.EffectiveGroups())
	}
	if c.PublicURL != "https://rooms.priv.aws.ogenki.io" || c.Human.Origin != "https://rooms.example.test" ||
		c.Human.Issuer != "https://auth.example.test" || c.Human.ClientIDFile != "/etc/room-broker/oidc/client-id" {
		t.Fatalf("config: %+v", c)
	}
	if c.Human.Access != nil || c.Human.MCP.Enabled || len(c.Human.MCP.Sponsored) != 0 || len(c.Human.MCP.SessionStart) != 0 {
		t.Fatalf("human switches: %+v", c.Human)
	}
	if c.TLS != (TLSConfig{CertFile: DefaultCertFile, KeyFile: DefaultKeyFile}) || c.Suspend != (SuspendConfig{After: Duration{DefaultSuspendAfter}}) {
		t.Fatalf("defaults: tls %+v suspend %+v", c.TLS, c.Suspend)
	}
}

// A local install names only the mode: the listener is the loopback port-forward, so publicURL and
// origin default to it, the groups are fixed, and every IdP or /mcp field is refused by name.
func TestLoadLocalHuman(t *testing.T) {
	t.Run("publicURL and origin default to the port-forward", func(t *testing.T) {
		c, err := Load(write(t, localBare))
		if err != nil {
			t.Fatal(err)
		}
		if c.PublicURL != "http://localhost:8080" || c.Human.Origin != c.PublicURL {
			t.Fatalf("publicURL %q origin %q", c.PublicURL, c.Human.Origin)
		}
		if c.Human.EffectiveGroups() != (GroupsConfig{Admin: "admin", Member: "member"}) || c.Human.Groups != c.Human.EffectiveGroups() {
			t.Fatalf("local groups: %+v", c.Human.EffectiveGroups())
		}
	})
	t.Run("publicURL", func(t *testing.T) {
		for _, u := range []string{"http://localhost:18080", "http://127.0.0.1:8080"} {
			if _, err := Load(write(t, localAt(u))); err != nil {
				t.Fatalf("%s must be accepted: %v", u, err)
			}
		}
		for _, u := range []string{"https://rooms.example", "http://0.0.0.0:8080", "http://localhost:8080/r/3kq7x2ma"} {
			if _, err := Load(write(t, localAt(u))); err == nil || !strings.Contains(err.Error(), "publicURL") {
				t.Fatalf("%s must be refused by name, got %v", u, err)
			}
		}
	})
	t.Run("every oidc field is refused by name", func(t *testing.T) {
		for _, tc := range []struct{ name, extra, want string }{
			{"issuer", "  issuer: https://idp.example\n", "human.issuer"},
			{"jwksURL", "  jwksURL: https://idp.example/keys\n", "human.jwksURL"},
			{"clientIDFile", "  clientIDFile: /etc/room-broker/oidc/client-id\n", "human.clientIDFile"},
			{"roomctlClientIDFile", "  roomctlClientIDFile: /etc/room-broker/roomctl/client-id\n", "human.roomctlClientIDFile"},
			{"projectIDFile", "  projectIDFile: /etc/room-broker/oidc/project-id\n", "human.projectIDFile"},
			{"origin", "  origin: http://localhost:18080\n", "human.origin"},
			{"groups", "  groups:\n    admin: a\n    member: b\n", "human.groups"},
			{"access", "  access:\n    readerFile: /etc/room-broker/access/reader\n", "human.access"},
			{"mcp", "  mcp:\n    enabled: true\n", "human.mcp.enabled"},
			{"sponsored", "  mcp:\n    sponsored:\n      - name: slack-app\n        clientIDFile: /etc/room-broker/sponsored/slack-app\n", "human.mcp.sponsored"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := Load(write(t, localBare+tc.extra)); err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("want %q, got %v", tc.want, err)
				}
			})
		}
		if _, err := Load(write(t, strings.Replace(localBare, "mode: local", "mode: bogus", 1))); err == nil ||
			!strings.Contains(err.Error(), `mode "bogus"`) {
			t.Fatalf("an unknown mode: %v", err)
		}
	})
}
