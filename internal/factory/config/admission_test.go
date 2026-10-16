// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
	"time"
)

// twoListed is the good file in the list shape with Smana/ai-native-ref as a second entry.
func twoListed(t *testing.T) string {
	t.Helper()
	l := listShaped(t, good)
	i := strings.Index(l, "repositories:\n")
	entry := l[i+len("repositories:\n"):]
	second := strings.Replace(entry, "- name: Smana/cloud-native-ref", "- name: Smana/ai-native-ref", 1)
	return l + second
}

func TestAdmissionSettings(t *testing.T) {
	for name, tc := range map[string]struct {
		add, err string
	}{
		"access with its reader and a TTL":          {"  access: {readerFile: /etc/r/reader.json, ttl: 5m}\n", ""},
		"access without a reader":                   {"  access: {ttl: 5m}\n", "api.access.readerFile is required"},
		"access trusted past five minutes":          {"  access: {readerFile: /r, ttl: 6m}\n", "api.access.ttl must be above 0 and at most 5m0s (v2 D4)"},
		"a system principal scoped to a repository": {"  systemIssuer: https://k8s\n  systemJWKS: https://k8s/jwks\n  systemPrincipals: {sa: system:runlore}\n  systemRepositories: {system:runlore: [Smana/ai-native-ref]}\n", ""},
		"a scope for an unknown principal":          {"  systemRepositories: {system:nobody: [Smana/ai-native-ref]}\n", `api.systemRepositories: "system:nobody" is not a systemPrincipals value`},
		"a scope on an unlisted repository":         {"  systemIssuer: https://k8s\n  systemJWKS: https://k8s/jwks\n  systemPrincipals: {sa: system:runlore}\n  systemRepositories: {system:runlore: [Smana/other]}\n", `api.systemRepositories.system:runlore: "Smana/other" is not a listed repository`},
	} {
		t.Run(name, func(t *testing.T) {
			raw := strings.Replace(twoListed(t), "api:\n", "api:\n"+tc.add, 1)
			_, err := Parse([]byte(raw))
			switch {
			case tc.err == "" && err != nil:
				t.Fatal(err)
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
				t.Fatalf("want %q, got %v", tc.err, err)
			}
		})
	}
	c, err := Parse([]byte(strings.Replace(twoListed(t), "api:\n", "api:\n  access: {readerFile: /r}\n", 1)))
	if err != nil || c.API.Access.TTL.Duration != 5*time.Minute {
		t.Fatalf("the TTL defaults to the most D4 trusts: %v %+v", err, c)
	}
}

// v2 D6: RunLore files issues on its configured repository, by default the first.
func TestRunLoreRepository(t *testing.T) {
	c, err := Parse([]byte(twoListed(t)))
	if err != nil || c.RunLoreRepository() != "Smana/cloud-native-ref" {
		t.Fatalf("default: %v %q", err, c.RunLoreRepository())
	}
	c, err = Parse([]byte(strings.Replace(twoListed(t), "runlore:\n", "runlore:\n  repository: Smana/ai-native-ref\n", 1)))
	if err != nil || c.RunLoreRepository() != "Smana/ai-native-ref" {
		t.Fatalf("set: %v", err)
	}
	if _, err := Parse([]byte(strings.Replace(twoListed(t), "runlore:\n", "runlore:\n  repository: Smana/other\n", 1))); err == nil ||
		!strings.Contains(err.Error(), `runlore.repository "Smana/other" is not a listed repository`) {
		t.Fatalf("unlisted: %v", err)
	}
}
