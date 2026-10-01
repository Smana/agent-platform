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
		{"terminal", "gh", "LOW", Plain},

		// M4: the allowlist is keyed by server__tool.
		{"mcp-victoriametrics__query", "", "LOW", Plain},
		{"agent-router__mcp-victorialogs__hits", "", "LOW", Plain}, // a harness prefix cannot hide the server
		{"anyserver__query", "", "LOW", MCPWrite},
		{"query", "", "LOW", MCPWrite}, // a bare name never matches

		// M1: the reviewer's three rows.
		{"terminal", "npm install x && gh pr merge 1", "LOW", EgressNew},
		{"terminal", "gh api repos/x/y -X", "LOW", Plain},
		{"terminal", "git push origin agent/3kq7x2ma-x", "LOW", ForgeOther},

		// I1: wrappers and prefixes are read; what is left fails cautious.
		{"terminal", "git -C repo push origin agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "git -C /workspace/repo push origin main", "LOW", ForgeOther},
		{"terminal", "git -C repo push origin agent/otherroom", "LOW", ForgeOther},
		{"terminal", "git -c k=v push origin main", "LOW", ForgeOther},
		{"terminal", "git --no-pager push origin main", "LOW", ForgeOther},
		{"terminal", "git --git-dir=.git --work-tree=. push origin agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "env X=1 git push origin main", "LOW", ForgeOther},
		{"terminal", "env -i X=1 git push origin agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "GIT_TRACE=1 git push origin main", "LOW", ForgeOther},
		{"terminal", "sudo gh pr merge 1", "LOW", ForgeOther},
		{"terminal", "command gh pr merge 1", "LOW", ForgeOther},
		{"terminal", "nohup git push origin main", "LOW", ForgeOther},
		{"terminal", "time git push origin agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "/usr/bin/git push origin main", "LOW", ForgeOther},
		{"terminal", `bash -c "gh pr merge 1"`, "LOW", ForgeOther},
		{"terminal", `sh -c 'git push origin main'`, "LOW", ForgeOther},
		{"terminal", `bash -lc 'git push origin agent/3kq7x2ma'`, "LOW", ForgePush},
		{"terminal", `bash -o pipefail -c 'npm install x'`, "LOW", EgressNew},
		{"terminal", `bash -c "bash -c 'ls'"`, "LOW", ForgeOther}, // a shell within a shell within a shell
		{"terminal", "bash deploy.sh", "LOW", Plain},              // a script file: unseen
		{"terminal", "(git push origin main)", "LOW", ForgeOther},
		{"terminal", "echo $(gh pr merge 1)", "LOW", ForgeOther},
		{"terminal", "echo `date`", "LOW", ForgeOther},
		{"terminal", `echo "$(date)"`, "LOW", ForgeOther},
		{"terminal", `echo '$(date)'`, "LOW", Plain}, // single quotes are literal
		{"terminal", "echo $(npm install x)", "LOW", EgressNew},
		{"terminal", "sleep 1 & gh pr merge 1", "LOW", ForgeOther},
		{"terminal", "sudo -u root gh pr merge 1", "LOW", ForgeOther}, // falls to the scan
		{"terminal", "timeout 60 git push origin main", "LOW", ForgeOther},
		{"terminal", "echo main | xargs git push origin", "LOW", ForgeOther},
		{"terminal", `echo "git push"`, "LOW", ForgeOther},
		{"terminal", "ssh host gh api -XPOST repos/x/y", "LOW", ForgeOther},
		{"terminal", "ssh host gh pr create", "LOW", ForgeOther},
		{"terminal", "echo gh issue view 1", "LOW", Plain},
		{"terminal", `git commit -m "push the fix"`, "LOW", Plain}, // a read git command is never scanned
		{"terminal", `gh pr create --title "a; git push origin main" --body "see git push"`, "LOW", ForgePR},
		{"terminal", `echo "unterminated`, "LOW", ForgeOther},
		{"terminal", `echo 'unterminated`, "LOW", ForgeOther},
		{"terminal", "git push origin agent/3kq7x2ma # then gh pr merge", "LOW", ForgePush},
		{"terminal", "git push \\\n  origin agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", `git push origin agent\/3kq7x2ma`, "LOW", ForgePush},
		{"terminal", "{ git push origin agent/3kq7x2ma; }", "LOW", ForgePush},
		{"terminal", "if git push origin agent/3kq7x2ma; then echo ok; fi", "LOW", ForgePush},
		{"terminal", "diff <(git push origin main) x", "LOW", ForgeOther},
		{"terminal", "make && git push origin agent/3kq7x2ma", "LOW", ForgePush}, // each operator splits, so the push is read exactly
		{"terminal", "cd x; git push origin agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "true | git push origin agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "(git push origin agent/3kq7x2ma)", "LOW", ForgePush},
		{"terminal", "GIT_TRACE=1 git push origin agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "/usr/bin/git push origin agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "ssh host /usr/bin/git push origin main", "LOW", ForgeOther},
		{"terminal", `gh search prs "gh pr merge"`, "LOW", Plain},       // a read gh command is never scanned
		{"terminal", "gh api repos/x/y#frag -XPOST", "LOW", ForgeOther}, // # starts a comment only at a word's start
		{"terminal", "echo \"`date`\"", "LOW", ForgeOther},
		{"terminal", "echo `npm install x`", "LOW", EgressNew},
		{"terminal", `gh pr create --title "a \" b"`, "LOW", ForgePR},
		{"terminal", "grep x <<< abc", "LOW", Plain},
		{"terminal", `bash run.sh -c 'npm install x'`, "LOW", Plain}, // -c is the script's argument
		{"terminal", `bash run.sh -c 'git push origin main'`, "LOW", ForgeOther},
		{"terminal", "true && bash <<EOF\ngit push origin main\nEOF", "LOW", ForgeOther},

		// Heredocs: a body is data unless a shell reads it or it expands a substitution.
		{"terminal", "gh pr create --body-file - <<'EOF'\nRun `npm install x`, then git push.\nEOF", "LOW", ForgePR},
		{"terminal", "gh pr create --body-file - <<\"EOF\"\n$(date)\nEOF", "LOW", ForgePR},
		{"terminal", "gh pr create --body-file - <<\\EOF\n$(date)\nEOF", "LOW", ForgePR},
		{"terminal", "gh pr create --body-file - <<EOF\nplain text\nEOF", "LOW", ForgePR},
		{"terminal", "gh pr create --body-file - <<EOF\n$(date)\nEOF", "LOW", ForgeOther},
		{"terminal", "gh pr create --body-file - <<EOF\nsee `date`\nEOF", "LOW", ForgeOther},
		{"terminal", "cat > f <<-EOF\n\tgit push origin main\n\tEOF\ngit push origin agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "cat > f <<EOF\ngit push origin main\nEOF\nnpm install x", "LOW", EgressNew}, // the line after the body is read
		{"terminal", "bash <<EOF\ngit push origin main\nEOF", "LOW", ForgeOther},
		{"terminal", "bash <<'EOF'\ngit push origin agent/3kq7x2ma\nEOF", "LOW", ForgePush},
		{"terminal", "cat <<", "LOW", ForgeOther}, // no delimiter
		{"terminal", "cat <<EOF\nnever closed", "LOW", Plain},

		// I2: pushes wider than the run's branch.
		{"terminal", "git push --all origin", "LOW", ForgeOther},
		{"terminal", "git push --branches origin", "LOW", ForgeOther},
		{"terminal", "git push --mirror", "LOW", ForgeOther},
		{"terminal", "git push --tags", "LOW", ForgeOther},
		{"terminal", "git push --follow-tags origin agent/3kq7x2ma", "LOW", ForgeOther},
		{"terminal", "git push --prune origin", "LOW", ForgeOther},
		{"terminal", "git push origin --delete agent/3kq7x2ma", "LOW", ForgeOther},
		{"terminal", "git push -d origin agent/3kq7x2ma", "LOW", ForgeOther},
		{"terminal", "git push -fd origin agent/3kq7x2ma", "LOW", ForgeOther},
		{"terminal", "git push origin :agent/3kq7x2ma", "LOW", ForgeOther},
		{"terminal", "git push -f origin +agent/3kq7x2ma", "LOW", ForgeOther}, // SAP: -f is not in the safe shape
		{"terminal", "git push --force-with-lease origin agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "git push --force-with-lease=agent/3kq7x2ma:abc origin agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "git push --set-upstream -q -v --quiet --verbose origin agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "git push --push-option=ci.skip origin agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "git push --no-verify origin agent/3kq7x2ma", "LOW", ForgeOther}, // the spec does not allow skipping hooks
		{"terminal", "git push --set-up origin agent/3kq7x2ma", "LOW", ForgeOther},    // a unique prefix is still not the flag
		{"terminal", "git push -o", "LOW", ForgeOther},                                // -o without its value
		{"terminal", "git push -o $X origin agent/3kq7x2ma", "LOW", ForgeOther},
		{"terminal", "git push origin", "LOW", ForgePush},
		{"terminal", "git push agent/3kq7x2ma", "LOW", ForgeOther}, // a remote named like the branch
		{"terminal", "git push origin agent/3kq7x2ma agent/3kq7x2ma", "LOW", ForgeOther},
		{"terminal", "git push origin main:agent/3kq7x2ma", "LOW", ForgeOther}, // only HEAD:<own>
		{"terminal", "git push origin $B", "LOW", ForgeOther},
		{"terminal", "git push origin agent/{3kq7x2ma,other}", "LOW", ForgeOther},
		{"terminal", "git push origin 'agent/3kq7x2ma'", "LOW", ForgePush},

		// I3: gh api's other spellings.
		{"terminal", "gh api -XPOST repos/x/y/issues", "LOW", ForgeOther},
		{"terminal", "gh api -XGET repos/x/y", "LOW", Plain},
		{"terminal", "gh api --method=DELETE repos/x/y/git/refs/heads/agent/x", "LOW", ForgeOther},
		{"terminal", "gh api --method=get repos/x/y", "LOW", Plain},
		{"terminal", "gh api repos/x/y/issues --field=title=x", "LOW", ForgeOther},
		{"terminal", "gh api repos/x/y/issues -ftitle=x", "LOW", ForgeOther},
		{"terminal", "gh api repos/x/y/issues -F n=1", "LOW", ForgeOther},
		{"terminal", "gh api repos/x/y/issues --raw-field title=x", "LOW", ForgeOther},
		{"terminal", "gh api repos/x/y/issues --raw-field=title=x", "LOW", ForgeOther},
		{"terminal", "gh api repos/x/y/issues --field title=x", "LOW", ForgeOther},
		{"terminal", "gh api repos/x/y/issues --input body.json", "LOW", ForgeOther},
		{"terminal", "gh api repos/x/y/issues --input=body.json", "LOW", ForgeOther},
		{"terminal", "gh api -X GET search/issues -f q=x", "LOW", Plain}, // GET sends the fields as a query

		// I4: an own-branch push does not false-positive.
		{"terminal", "git push -u origin agent/3kq7x2ma 2>&1", "LOW", ForgePush},
		{"terminal", "git push origin agent/3kq7x2ma > /dev/null", "LOW", ForgePush},
		{"terminal", "git push origin agent/3kq7x2ma >/dev/null 2>&1", "LOW", ForgePush},
		{"terminal", "git push origin agent/3kq7x2ma &> log", "LOW", ForgePush},
		{"terminal", "git push origin agent/3kq7x2ma >&2", "LOW", ForgePush},
		{"terminal", "git push 2>&1 origin main", "LOW", ForgeOther}, // a redirect's & never splits the command
		{"terminal", "git push &>log origin main", "LOW", ForgeOther},
		{"terminal", `git push origin "agent/3kq7x2ma"`, "LOW", ForgePush},
		{"terminal", "git push -o ci.skip origin agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "git push -oci.skip origin agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "git push -odeploy origin agent/3kq7x2ma", "LOW", ForgePush}, // -o's value is not a -d cluster
		{"terminal", "git push --push-option ci.skip origin agent/3kq7x2ma", "LOW", ForgePush},
		{"terminal", "git push --repo origin agent/3kq7x2ma", "LOW", ForgeOther}, // SAP: off the allowlist
		{"terminal", "git push --receive-pack x origin agent/3kq7x2ma", "LOW", ForgeOther},
		{"terminal", "git push --exec x origin agent/3kq7x2ma", "LOW", ForgeOther},
		{"terminal", "git push origin refs/heads/agent/3kq7x2ma", "LOW", ForgePush},

		// M3: only real fetches are egress.new.
		{"terminal", "go mod tidy", "LOW", EgressNew},
		{"terminal", "go mod download", "LOW", EgressNew},
		{"terminal", "go mod vendor", "LOW", EgressNew},
		{"terminal", "go mod why x", "LOW", Plain},
		{"terminal", "go mod", "LOW", Plain},
		{"terminal", "cargo build", "LOW", Plain},
		{"terminal", "cargo fetch", "LOW", EgressNew},
		{"terminal", "sudo pip3 install x", "LOW", Plain}, // pypi is in the run's profiles

		// gh writes added with M6.
		{"terminal", "gh issue transfer 1 x/y", "LOW", ForgeOther},
		{"terminal", "gh repo sync", "LOW", ForgeOther},
		{"terminal", "gh run rerun 9", "LOW", ForgeOther},
	} {
		if got := c.Classify(tc.tool, shell(tc.cmd), tc.risk); got != tc.want {
			t.Errorf("%s %q %s: got %q want %q", tc.tool, tc.cmd, tc.risk, got, tc.want)
		}
	}
}

// M2: an unreadable terminal action fails cautious; an empty command is is_input polling.
func TestClassifyUnreadableAction(t *testing.T) {
	c := Classifier{Branch: "agent/3kq7x2ma"}
	for _, tc := range []struct {
		action json.RawMessage
		want   Class
	}{
		{json.RawMessage(`git push origin main`), ForgeOther},
		{json.RawMessage(`{"command": 5}`), ForgeOther},
		{json.RawMessage(`{"command": ""}`), Plain},
		{json.RawMessage(`{}`), Plain},
	} {
		if got := c.Classify("terminal", tc.action, "LOW"); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.action, got, tc.want)
		}
	}
}

// M5: with no branch to compare, every push is forge.other.
func TestClassifyWithoutABranch(t *testing.T) {
	for _, cmd := range []string{"git push", "git push origin main:", "git push origin HEAD"} {
		if got := (Classifier{}).Classify("terminal", shell(cmd), "LOW"); got != ForgeOther {
			t.Errorf("%q: got %q want forge.other", cmd, got)
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
		// I5: Decide is total over Verdict.
		{wire.ApprovalPolicy{Profile: "unattended", Overrides: map[string]string{"forge.other": "Allow"}}, ForgeOther, Human},
		{wire.ApprovalPolicy{Profile: "attended", Overrides: map[string]string{"forge.push": "allow"}}, ForgePush, Allow},
		{wire.ApprovalPolicy{Profile: "unattended", Overrides: map[string]string{"forge.push": "human"}}, ForgePush, Human},
		{wire.ApprovalPolicy{Profile: "unattended"}, Class("forge.tag"), Human},
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
