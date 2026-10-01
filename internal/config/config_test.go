// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
