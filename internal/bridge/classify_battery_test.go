// SPDX-License-Identifier: Apache-2.0

package bridge

import "testing"

// The commands the two 5.1 reviews ran against Classify, round 1's probe and
// round 2's hunt, with the class each must keep. This is the regression net for
// rulings SAN and SAP: a change that moves a row is a change to the oversight
// the room promises, not a refactor.
func TestReviewBattery(t *testing.T) {
	c := Classifier{Branch: "agent/3kq7x2ma", Egress: map[string]bool{"pypi": true}}
	for _, tc := range []struct {
		finding, cmd string
		want         Class
	}{
		// Round 1, I1: wrappers and prefixes.
		{"I1", "git -C /workspace/repo push origin main", ForgeOther},
		{"I1", "git -C repo push origin agent/otherroom", ForgeOther},
		{"I1", "git -C repo push origin agent/3kq7x2ma", ForgePush},
		{"I1", "git -c k=v push origin main", ForgeOther},
		{"I1", "git --no-pager push origin main", ForgeOther},
		{"I1", "env X=1 git push origin main", ForgeOther},
		{"I1", "GIT_TRACE=1 git push origin main", ForgeOther},
		{"I1", "sudo gh pr merge 1", ForgeOther},
		{"I1", "command gh pr merge 1", ForgeOther},
		{"I1", `bash -c "gh pr merge 1"`, ForgeOther},
		{"I1", `sh -c 'git push origin main'`, ForgeOther},
		{"I1", "(git push origin main)", ForgeOther},
		{"I1", "echo $(gh pr merge 1)", ForgeOther},
		{"I1", "echo `gh pr merge 1`", ForgeOther},
		{"I1", "sleep 1 & gh pr merge 1", ForgeOther},

		// Round 1 I2 and round 2 N4: pushes wider than the run's branch, abbreviations included.
		{"I2", "git push --all origin", ForgeOther},
		{"I2", "git push --mirror", ForgeOther},
		{"I2", "git push --tags", ForgeOther},
		{"I2", "git push --follow-tags", ForgeOther},
		{"I2", "git push origin --delete agent/3kq7x2ma", ForgeOther},
		{"I2", "git push --prune origin", ForgeOther},
		{"N4", "git push --mirro origin", ForgeOther},
		{"N4", "git push --al origin", ForgeOther},
		{"N4", "git push --tag", ForgeOther},
		{"N4", "git push origin --del agent/3kq7x2ma", ForgeOther},
		{"N4", "git push --prun origin", ForgeOther},

		// I3: gh api spellings.
		{"I3", "gh api -XPOST repos/x/y/issues", ForgeOther},
		{"I3", "gh api --method=DELETE repos/x/y/git/refs/heads/agent/x", ForgeOther},
		{"I3", "gh api repos/x/y/issues --field=title=x", ForgeOther},
		{"I3", "gh api repos/x/y/issues -ftitle=x", ForgeOther},

		// I4: the run's own push, with what agents append.
		{"I4", "git push -u origin agent/3kq7x2ma 2>&1", ForgePush},
		{"I4", "git push origin agent/3kq7x2ma > /dev/null", ForgePush},
		{"I4", `git push origin "agent/3kq7x2ma"`, ForgePush},
		{"I4", "git push -o ci.skip origin agent/3kq7x2ma", ForgePush},

		// M1: the reviewer's three mutant-killing rows.
		{"M1", "npm install x && gh pr merge 1", EgressNew},
		{"M1", "gh api repos/x/y -X", Plain},
		{"M1", "git push origin agent/3kq7x2ma-x", ForgeOther},

		// M3: egress is only a real fetch; the rest is Cilium's.
		{"M3", "go mod tidy", EgressNew},
		{"M3", "cargo build", Plain},
		{"M3", "go mod why x", Plain},
		{"M3", "go mod graph", Plain},
		{"M3", "go build ./...", Plain},
		{"M3", "go test ./...", Plain},
		{"M3", "cargo test", Plain},
		{"M3", "uv sync", Plain},
		{"M3", "python -m pip install x", Plain}, // residual: Cilium is the limit

		// M3, judged acceptable: cautious false positives.
		{"M3", "git push upstream agent/3kq7x2ma", ForgeOther},
		{"M3", "gh api graphql -f query=x", ForgeOther},
		{"M3", `gh pr create --title "x; git push origin main"`, ForgePR},

		// M6: residuals and the gh writes added.
		{"M6", `curl -X POST -H "Authorization: token $(gh auth token)" https://api.github.com/x`, ForgeOther},
		{"M6", `curl -X POST -H "Authorization: token $GH_TOKEN" https://api.github.com/x`, Plain}, // residual (file header)
		{"M6", "gh repo sync", ForgeOther},
		{"M6", "gh issue transfer 1 x/y", ForgeOther},
		{"M6", "git push", ForgePush},             // residual: the current branch
		{"M6", "git push origin HEAD", ForgePush}, // residual: the current branch

		// N1: expansions bash resolves at run time.
		{"N1", `X="git push origin main"; eval $X`, ForgeOther},
		{"N1", `CMD='gh pr merge 1'; $CMD`, ForgeOther},
		{"N1", "P=push; git $P origin main", ForgeOther},
		{"N1", "G=gh; $G pr merge 1", ForgeOther},
		{"N1", "git $'push' origin main", ForgeOther},
		{"N1", `git $"push" origin main`, ForgeOther},
		{"N1", "bash -c $'git push origin main'", ForgeOther},
		{"N1", `bash -c "$CMD"`, ForgeOther},
		{"N1", "git {push,} origin main", ForgeOther},
		{"N1", "git pu* origin main", ForgeOther},
		{"N1", "gh $G merge 1", ForgeOther},
		{"N1", "gh pr $V 1", ForgeOther},
		{"N1", "eval git push origin main", ForgeOther},
		{"N1", "eval git push origin agent/3kq7x2ma", ForgePush},
		{"N1", "eval", Plain},
		{"N1", "env $X git push origin agent/3kq7x2ma", ForgeOther},
		{"N1", `echo "$"`, Plain}, // a lone $ is literal

		// N2: an alias hides the verb from the scan.
		{"N2", "alias p='git push origin main'\np", ForgeOther},
		{"N2", "alias m=\"gh pr merge\"\nm 1", ForgeOther},

		// N3: gh flags between the group and the verb.
		{"N3", "gh pr -R Smana/cloud-native-ref merge 12", ForgeOther},
		{"N3", "gh issue --repo x/y close 1", ForgeOther},
		{"N3", "gh pr --repo=x/y merge 1", ForgeOther},
		{"N3", "gh pr -Rx/y merge 1", ForgeOther},
		{"N3", "gh pr -R x/y list", Plain},
		{"N3", "gh pr -R x/y create", ForgePR},
		{"N3", "gh pr --json x list", ForgeOther}, // any other flag before the verb
		{"N3", "gh -R x/y pr merge 1", ForgeOther},
		{"N3", "gh pr -R", Plain},
		{"N3", "gh --version", Plain},

		// N5: a quoted redirect character is a word.
		{"N5", `git push origin agent/3kq7x2ma ">" agent/other`, ForgeOther},
		{"N5", `git push -o ">" origin agent/other`, ForgeOther},
		{"N5", `git push -o ">" origin agent/3kq7x2ma`, ForgeOther},
		{"N5", "git push origin agent/3kq7x2ma '2>x'", ForgeOther},

		// N6: interpreters fed a heredoc are scanned; script files are a residual.
		{"N6", "python3 <<'EOF'\nimport os\nos.system('git push origin main')\nEOF", ForgeOther},
		{"N6", "node <<'EOF'\nrequire('child_process').execSync('gh pr merge 1')\nEOF", ForgeOther},
		{"N6", "python3 <<'EOF'\nprint('hello')\nEOF", Plain},
		{"N6", "python3 -c \"import subprocess; subprocess.run(['git', 'push', 'origin', 'main'])\"", ForgeOther},
		{"N6", "cat <<'EOF' > x.sh\ngit push origin main\nEOF\nbash x.sh", Plain}, // residual (file header)
		{"N6", "source ./push.sh", Plain},                                         // residual (file header)
		{"N6", ". ./push.sh", Plain},                                              // residual (file header)

		// N7: alias and extension definitions.
		{"N7", "gh alias set m 'pr merge'\ngh m 1", ForgeOther},
		{"N7", "gh m 1", ForgeOther}, // an unknown group is an alias or an extension
		{"N7", "gh extension install x/gh-y", ForgeOther},
		{"N7", "git config alias.p push && git p origin main", ForgeOther},
		{"N7", "git config --global alias.p push", ForgeOther},
		{"N7", "git -c alias.p=push p origin main", ForgeOther},
		{"N7", "git --config-env=alias.p=V p origin main", ForgeOther},
		{"N7", "git config user.name bot", Plain},

		// N8: other push paths.
		{"N8", "git subtree push --prefix=x origin main", ForgeOther},
		{"N8", "git send-pack x main", ForgeOther},
		{"N8", "git http-push x main", ForgeOther},
		{"N8", "git subtree split --prefix=x", Plain},

		// Commands that run commands are scanned; reads are not.
		{"scan", "git rebase -x 'git push origin main' main", ForgeOther},
		{"scan", "git submodule foreach git push origin main", ForgeOther},
		{"scan", "git rebase main", Plain},
		{"scan", "git lfs push origin main", ForgeOther}, // an extension
		{"scan", `git commit -m "push the fix"`, Plain},
		{"scan", "git stash push -m wip", Plain},

		// The heredoc addition, judged sound.
		{"heredoc", "gh pr create --body-file - <<'EOF'\nSee `x` and $(y).\nEOF", ForgePR},
		{"heredoc", "gh pr create --body-file - <<EOF\nSee `x`.\nEOF", ForgeOther},
		{"heredoc", "bash <<'EOF'\ngit push origin main\nEOF", ForgeOther},
		{"heredoc", "cat <<EOF; gh pr merge 1\nx\nEOF", ForgeOther},
		{"heredoc", "cat <<-EOF\n\tx\n\tEOF\ngh pr merge 1", ForgeOther},

		// Concern 1 and the other judged false positives.
		{"concern 1", "gh pr create --body \"$(cat <<'EOF'\nbody\nEOF\n)\"", ForgeOther},
		{"FP", "timeout 30 git push origin agent/3kq7x2ma", ForgePush},
		{"FP", "timeout -k 5 30 git push origin agent/3kq7x2ma", ForgeOther}, // -k's value: cautious
		{"FP", "echo $((1+2))", ForgeOther},

		// Expansion stays harmless where it is not a command position.
		{"literal", "git -C $DIR push origin agent/3kq7x2ma", ForgePush},
		{"literal", "ls *.go", Plain},
		{"literal", "gh api 'repos/x/y?per_page=1'", Plain},
		{"literal", "[ -f x ] && git push origin agent/3kq7x2ma", ForgePush},
		{"literal", `X="a b" git push origin agent/3kq7x2ma`, ForgePush},
		{"literal", `"X=1" git push origin agent/3kq7x2ma`, ForgeOther},  // a quoted name is a command, not an assignment
		{"literal", `git push -o "a$" origin agent/3kq7x2ma`, ForgePush}, // a $ before the closing quote is literal
		{"literal", "git push -o a$ origin agent/3kq7x2ma", ForgePush},
		{"literal", "git push origin agent/3kq7x2ma \\>x", ForgeOther}, // an escaped > is a word
		{"literal", "git push --force-with-lease=$X origin agent/3kq7x2ma", ForgeOther},
		{"literal", "/usr/bin/env git push origin agent/3kq7x2ma", ForgePush},
		{"literal", "timeout --foreground 30 git push origin agent/3kq7x2ma", ForgePush},
		{"N1", "git pu?h origin main", ForgeOther},
		{"N1", "git pu[s]h origin main", ForgeOther},
		{"N1", "git pus{h..h} origin main", ForgeOther},
		{"N1", `X="git push origin main"`, ForgeOther}, // an assignment alone is still scanned
		{"N7", "git config alias.co checkout", ForgeOther},
		{"N7", "git -c alias.co=checkout co main", ForgeOther}, // any alias definition, not only a push alias
		{"N7", "gh alias set m 'pr merge'", ForgeOther},
		{"N1", `git "$P" origin main`, ForgeOther}, // a double-quoted expansion is still one
		{"N1", `"$CMD" x`, ForgeOther},
		{"N1", `bash -c "echo $X"`, ForgeOther}, // an expanded script is unreadable, wherever the $ sits
		{"N1", "eval echo $X", ForgeOther},
		{"N3", "gh pr --repo=x/y list", Plain},
		{"N3", "gh pr -Rx/y list", Plain},
		{"N3", "gh browse --no-browser", Plain}, // a group without verbs takes its flags
	} {
		if got := c.Classify("terminal", shell(tc.cmd), "LOW"); got != tc.want {
			t.Errorf("%s %q: got %q want %q", tc.finding, tc.cmd, got, tc.want)
		}
	}
}
