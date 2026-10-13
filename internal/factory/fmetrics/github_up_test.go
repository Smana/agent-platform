// SPDX-License-Identifier: Apache-2.0

package fmetrics

import (
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// agent_factory_github_up flips with GitHub: FactoryGitHubUnreachable fires on a 0 held for 10m.
func TestGitHubUpFlips(t *testing.T) {
	s, scrape := rig(t, fake.NewClientBuilder().WithScheme(scheme()).Build(), func() bool { return false })
	for _, c := range []struct {
		up   bool
		want string
	}{{false, "agent_factory_github_up 0\n"}, {true, "agent_factory_github_up 1\n"}} {
		s.GitHubUp(t.Context(), c.up)
		if body := scrape(); !strings.Contains(body, c.want) {
			t.Fatalf("up=%v: missing %q in\n%s", c.up, c.want, body)
		}
	}
}
