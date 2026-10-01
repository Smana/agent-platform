// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"encoding/json"
	"testing"

	"github.com/Smana/agent-platform/internal/wire"
)

func shell(cmd string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return b
}

func TestClassify(t *testing.T) {
	c := Classifier{Branch: "agent/3kq7x2ma", Egress: map[string]bool{"pypi": true}}
	for _, tc := range []struct {
		tool   string
		action json.RawMessage
		risk   string
		want   Class
	}{
		{"terminal", shell("git push origin agent/3kq7x2ma"), "LOW", ForgePush},
		{"terminal", shell("git push"), "LOW", ForgePush},
		{"terminal", shell("git push origin main"), "LOW", ForgeOther},
		{"terminal", shell("cd repo && gh pr create --fill"), "MEDIUM", ForgePR},
		{"terminal", shell("gh pr ready 12"), "LOW", ForgePR},
		{"terminal", shell("gh pr merge 12 --squash"), "LOW", ForgeOther},
		{"terminal", shell("gh api -X POST repos/Smana/cloud-native-ref/issues/1/comments -f body=x"), "LOW", ForgeOther},
		{"terminal", shell("gh pr view 12"), "LOW", Plain},
		{"terminal", shell("pip install requests"), "LOW", Plain}, // pypi is in the run's profiles
		{"terminal", shell("npm install left-pad"), "LOW", EgressNew},
		{"terminal", shell("rm -rf /tmp/x"), "HIGH", ShellHigh},
		{"file_editor", json.RawMessage(`{"path":"a"}`), "LOW", Plain},
		{"flux-operator-mcp__get_kubernetes_resources", json.RawMessage(`{}`), "LOW", Plain},
		{"room-broker__room_verdict", json.RawMessage(`{}`), "LOW", Plain},
		{"some-mcp__delete_everything", json.RawMessage(`{}`), "LOW", MCPWrite},
	} {
		if got := c.Classify(tc.tool, tc.action, tc.risk); got != tc.want {
			t.Errorf("%s %s: got %q want %q", tc.tool, tc.action, got, tc.want)
		}
	}
}

// The branches TestClassify leaves open: each case kills a mutant of classify.go.
func TestClassifyEdges(t *testing.T) {
	c := Classifier{Branch: "agent/3kq7x2ma", Egress: map[string]bool{"pypi": true}}
	for _, tc := range []struct {
		tool, cmd, risk string
		want            Class
	}{
		{"terminal", "git push origin main && git push", "LOW", ForgeOther}, // worst wins, not last
		{"terminal", "ls; git push origin main", "LOW", ForgeOther},
		{"terminal", "echo x | gh pr merge 1", "LOW", ForgeOther},
		{"terminal", "true || npm install x", "LOW", EgressNew},
		{"terminal", "ls\ngit push origin main", "LOW", ForgeOther},
		{"terminal", "git", "LOW", Plain}, // a one-word segment is never indexed past its end
		{"terminal", "npm", "LOW", Plain},
		{"terminal", "git push -u origin agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "git push origin HEAD", "LOW", ForgePush},
		{"terminal", "git push origin HEAD:agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "git push origin HEAD:refs/heads/agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "git push origin HEAD:main", "LOW", ForgeOther},
		{"terminal", "gh pr edit 3 --title x", "LOW", ForgePR},
		{"terminal", "gh api -X GET repos/x/y", "LOW", Plain},
		{"terminal", "gh api repos/x/y", "LOW", Plain},
		{"terminal", "gh api repos/x/y/issues -f title=t", "LOW", ForgeOther},
		{"terminal", "gh api --method PATCH repos/x/y", "LOW", ForgeOther},
		{"terminal", "npm view left-pad", "LOW", Plain},
		{"terminal", "go get example.com/m", "LOW", EgressNew},
		{"execute_bash", "git push origin main", "LOW", ForgeOther},
		{"terminal", "rm -rf /tmp/x", "high", ShellHigh},
		{"terminal", "git push origin main", "HIGH", ForgeOther}, // a matched class outranks the risk label
		{"file_editor", "", "HIGH", ShellHigh},
		{"vm__query", "", "LOW", Plain},
		{"query", "", "LOW", Plain}, // an unprefixed tool name is its own name
	} {
		if got := c.Classify(tc.tool, shell(tc.cmd), tc.risk); got != tc.want {
			t.Errorf("%s %q %s: got %q want %q", tc.tool, tc.cmd, tc.risk, got, tc.want)
		}
	}
}

func TestDecideFallbacks(t *testing.T) {
	for _, tc := range []struct {
		p    wire.ApprovalPolicy
		c    Class
		want Verdict
	}{
		{wire.ApprovalPolicy{Profile: "unattended"}, ForgePR, Allow},
		{wire.ApprovalPolicy{}, ForgePR, Human},                  // no profile: attended
		{wire.ApprovalPolicy{Profile: "bogus"}, MCPWrite, Human}, // unknown: attended
		{wire.ApprovalPolicy{Profile: "attended", Overrides: map[string]string{"forge.other": "deny"}}, ForgeOther, Deny},
		{wire.ApprovalPolicy{Profile: "attended", Overrides: map[string]string{"": "deny"}}, Plain, Allow},
	} {
		if got := Decide(tc.p, tc.c); got != tc.want {
			t.Errorf("%+v/%q: got %s want %s", tc.p, tc.c, got, tc.want)
		}
	}
}

func TestDecideFollowsTheProfileTable(t *testing.T) {
	att := wire.ApprovalPolicy{Profile: "attended"}
	un := wire.ApprovalPolicy{Profile: "unattended", Overrides: map[string]string{"forge.pr": "human"}}
	for _, tc := range []struct {
		p    wire.ApprovalPolicy
		c    Class
		want Verdict
	}{
		{att, ForgePush, Allow}, {att, ForgePR, Human}, {att, ForgeOther, Human}, {att, MCPWrite, Human},
		{att, ShellHigh, Allow}, {att, EgressNew, Deny}, {att, Plain, Allow},
		{un, ForgePush, Allow}, {un, ForgePR, Human}, {un, ForgeOther, Deny}, {un, MCPWrite, Deny}, {un, ShellHigh, Allow},
		{wire.ApprovalPolicy{Profile: "unattended", Overrides: map[string]string{"egress.new": "human"}}, EgressNew, Deny},
	} {
		if got := Decide(tc.p, tc.c); got != tc.want {
			t.Errorf("%s/%s: got %s want %s", tc.p.Profile, tc.c, got, tc.want)
		}
	}
}
