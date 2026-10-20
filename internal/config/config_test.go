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

func TestSystemIssuerOptional(t *testing.T) {
	body := strings.Replace(valid, systemIssuerBlock, "", 1)
	if _, err := Load(write(t, body)); err != nil {
		t.Fatalf("no systemIssuer must load: %v", err)
	}
	half := strings.Replace(valid, systemIssuerBlock, "systemIssuer:\n  issuer: https://x\n", 1)
	if _, err := Load(write(t, half)); err == nil {
		t.Fatal("an issuer without jwksURL must be refused")
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
