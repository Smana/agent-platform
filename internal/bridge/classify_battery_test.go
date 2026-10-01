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
		{"FP", "timeout -k 5 30 git push origin agent/3kq7x2ma", ForgePush}, // SAR's runner table knows -k's value
		{"FP", "echo $((1+2))", ForgeOther},

		// Expansion stays harmless where it is not a command position.
		{"literal", "git -C $DIR push origin agent/3kq7x2ma", ForgePush},
		{"literal", "ls *.go", Plain},
		{"literal", "gh api 'repos/x/y?per_page=1'", Plain},
		{"literal", "[ -f x ] && git push origin agent/3kq7x2ma", ForgePush},
		{"literal", `X="a b" git push origin agent/3kq7x2ma`, ForgeOther}, // SAR: off git's env allowlist
		{"literal", `X="a b" make build`, Plain},                          // any other command takes any harmless variable
		{"literal", `"X=1" git push origin agent/3kq7x2ma`, ForgeOther},   // a quoted name is a command, not an assignment
		{"literal", `git push -o "a$" origin agent/3kq7x2ma`, ForgePush},  // a $ before the closing quote is literal
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

		// R1: -c config on a safe subcommand; only safeConfig keys pass.
		{"R1", "git -c core.pager='git push origin main' log", ForgeOther},
		{"R1", "git -c core.fsmonitor='git push origin main' status", ForgeOther},
		{"R1", "git -c core.editor='gh pr merge 1' commit", ForgeOther},
		{"R1", "git -c credential.helper='!git push origin main' fetch", ForgeOther},
		{"R1", "git -c diff.external='touch F' diff", ForgeOther},
		{"R1", "git -c protocol.ext.allow=always fetch 'ext::sh -c touch% F'", ForgeOther},
		{"R1", "git -c core.hooksPath=/tmp/h commit -m x", ForgeOther},
		{"R1", "git -c core.sshCommand='touch F' fetch", ForgeOther},
		{"R1", "git --config-env=core.pager=P log", ForgeOther},
		{"R1", "git --config-env core.pager=P log", ForgeOther},
		{"R1", "git -c", ForgeOther},
		{"R1", "git -c user.name=bot -c user.email=b@x commit -m x", Plain},
		{"R1", "git -c color.ui=always -c advice.detachedHead=false -c core.quotepath=off log", Plain},
		{"R1", "git -c User.Name=bot commit -m x", Plain}, // keys are case-insensitive
		{"R1", "git -c init.defaultBranch=main init", Plain},
		{"R1", "git --exec-path=/tmp/x status", ForgeOther},
		{"R1", "git --no-pager --git-dir=.git --work-tree=. log", Plain},
		{"R1", "git -C repo --no-optional-locks status", Plain},
		{"R1", "git --no-pager=x log", ForgeOther}, // a flag never takes a value
		// R1: command-valued flags on safe subcommands.
		{"R1", "git ls-remote --upload-pack='git push origin main #' .", ForgeOther},
		{"R1", "git ls-remote --upload-pack 'touch F' .", ForgeOther},
		{"R1", "git fetch --upload-pack=x origin", ForgeOther},
		{"R1", "git pull --receive-pack=x", ForgeOther},
		{"R1", "git clone -u 'touch F' . /tmp/x", ForgeOther},
		{"R1", "git clone -c core.fsmonitor=x . /tmp/x", ForgeOther},
		{"R1", "git clone --config core.pager=x . /tmp/x", ForgeOther},
		{"R1", "git clone --template=/tmp/t . /tmp/x", ForgeOther},
		{"R1", "git init --template /tmp/t", ForgeOther},
		{"R1", "git archive --remote=. --exec='touch F' HEAD", ForgeOther},
		{"R1", "git grep -O'touch F #' foo", ForgeOther},
		{"R1", "git grep --open-files-in-pager=x foo", ForgeOther},
		{"R1", "git diff --ext-diff", ForgeOther},
		{"R1", "git log --ext-diff -p", ForgeOther},
		{"R1", "git clone https://github.com/x/y /tmp/y", Plain},
		{"R1", "git commit --template=msg.txt", Plain}, // commit's template is a message file
		{"R1", "git grep -n foo", Plain},
		// R1: the environment.
		{"R1", "GIT_PAGER='git push origin main' git log", ForgeOther},
		{"R1", "GIT_EXTERNAL_DIFF='touch F' git diff", ForgeOther},
		{"R1", "GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.pager GIT_CONFIG_VALUE_0=x git log", ForgeOther},
		{"R1", "EDITOR='gh pr merge 1' git commit", ForgeOther},
		{"R1", "env GIT_SSH_COMMAND='touch F' git fetch", ForgeOther},
		{"R1", "GH_PAGER='git push origin main' gh pr view 1", ForgeOther},
		{"R1", "BROWSER='gh pr merge 1 #' gh browse", ForgeOther},
		{"R1", "GH_TOKEN=x gh pr view 1", ForgeOther},
		{"R1", "HOME=/tmp/h git log", ForgeOther}, // off git's allowlist: another ~/.gitconfig
		{"R1", "PAGER='git push origin main' man ls", ForgeOther},
		{"R1", "LD_PRELOAD=/tmp/x.so ls", ForgeOther},
		{"R1", "BASH_ENV=/tmp/x bash -c ls", ForgeOther},
		{"R1", "SSH_ASKPASS=/tmp/x ls", ForgeOther},
		{"R1", "PAGER=x", ForgeOther}, // an exported PAGER changes for every later command
		{"R1", "export GIT_PAGER='git push origin main'", ForgeOther},
		{"R1", "export GIT_PAGER", ForgeOther},
		{"R1", "declare -x EDITOR=x", ForgeOther},
		{"R1", "export NODE_ENV=production", Plain},
		{"R1", "CGO_ENABLED=0 GOOS=linux go build ./...", Plain},
		{"R1", "LANG=C TZ=UTC git log", Plain},
		// R1: gh's own hooks.
		{"R1", "gh config set pager 'git push origin main'", ForgeOther},
		{"R1", "gh config set browser 'gh pr merge 1'", ForgeOther},
		{"R1", "gh config get pager", Plain},
		{"R1", "gh repo clone x/y -- -c core.fsmonitor=x", ForgeOther},
		{"R1", "gh repo clone x/y", Plain},

		// R2: config that widens the push SAP calls forge.push.
		{"R2", "git -c remote.origin.mirror=true push", ForgeOther},
		{"R2", "git -c push.default=matching push", ForgeOther},
		{"R2", "git -c remote.origin.push='+refs/heads/*:refs/heads/*' push origin", ForgeOther},
		{"R2", "git config remote.origin.mirror true && git push", ForgeOther},
		{"R2", "git config push.default matching\ngit push origin", ForgeOther},
		{"R2", "git config --add remote.origin.push 'refs/tags/*:refs/tags/*'; git push", ForgeOther},
		{"R2", "git config set branch.x.remote other", ForgeOther},
		{"R2", "git config core.fsmonitor \"$X\"", ForgeOther},
		{"R2", "git config --global -e", ForgeOther},
		{"R2", "git config edit", ForgeOther},
		{"R2", "git config --rename-section remote.origin remote.x", ForgeOther},
		{"R2", "git config --get remote.origin.url", Plain},
		{"R2", "git config remote.origin.url", Plain}, // one key and no value is a read
		{"R2", "git config --list --show-origin", Plain},
		{"R2", "git config get user.name", Plain},
		{"R2", "git config --global user.email bot@x", Plain},
		{"R2", "git config --file .gitmodules --get-regexp path", Plain},
		{"R2", "git config pull.rebase false", Plain},
		{"R2", "git remote add --mirror=push backup https://x/y", ForgeOther},
		{"R2", "git remote add --mirror backup https://x/y", ForgeOther},
		{"R2", "git remote rename upstream origin", ForgeOther},
		{"R2", "git remote -v", Plain},
		{"R2", "git remote add upstream https://github.com/x/y", Plain},
		{"R2", "git remote rename origin old", Plain},
		{"M7", "git remote set-url origin https://github.com/x/other && git push origin agent/3kq7x2ma", ForgePush}, // residual: octo-sts bounds the repo

		// R3: gh's read verbs per group; aliases and unknown verbs are not reads.
		{"R3", "gh pr new", ForgePR},
		{"R3", "gh pr new --fill", ForgePR},
		{"R3", "gh issue new", ForgeOther},
		{"R3", "gh repo new x", ForgeOther},
		{"R3", "gh gist new f.txt", ForgeOther},
		{"R3", "gh release new v9.9.9", ForgeOther},
		{"R3", "gh secret remove X", ForgeOther},
		{"R3", "gh variable remove X", ForgeOther},
		{"R3", "gh copilot suggest 'git push'", ForgeOther},
		{"R3", "gh copilot explain x", ForgeOther},
		{"R3", "gh auth login --with-token", ForgeOther},
		{"R3", "gh auth setup-git", ForgeOther},
		{"R3", "gh auth status", Plain},
		{"R3", "gh pr checks 12", Plain},
		{"R3", "gh pr co 12", Plain},
		{"R3", "gh co 12", Plain},
		{"R3", "gh run watch 9", Plain},
		{"R3", "gh release download v1", Plain},
		{"R3", "gh issue list --state open", Plain},
		{"R3", "gh variable get X", Plain},
		{"R3", "gh workflow list", Plain},
		{"R3", "gh repo set-default x/y", Plain},
		{"R3", "ssh host gh pr new", ForgeOther},
		{"R3", "echo gh secret remove X", ForgeOther}, // the scan uses the same allowlist
		{"M7", "gh gist create f.txt", ForgeOther},

		// R4: runners; an expanded word in a command position is unreadable.
		{"R4", `git rebase -x "$CMD" HEAD~1`, ForgeOther},
		{"R4", `git rebase --exec="$CMD" HEAD~1`, ForgeOther},
		{"R4", `git rebase -x"$CMD" HEAD~1`, ForgeOther},
		{"R4", "git rebase -x 'make test' HEAD~3", Plain},
		{"R4", "git rebase --exec 'gh pr merge 1' HEAD~1", ForgeOther},
		{"R4", `git submodule foreach "$CMD"`, ForgeOther},
		{"R4", "git submodule foreach --recursive git status", Plain},
		{"R4", "git submodule foreach 'git push origin main'", ForgeOther},
		{"R4", "git submodule update --init", Plain},
		{"R4", "git bisect run $CMD", ForgeOther},
		{"R4", "git bisect run make test", Plain},
		{"R4", "git bisect run gh pr merge 1", ForgeOther},
		{"R4", "git bisect start", Plain},
		{"R4", `git filter-branch --tree-filter "$CMD" HEAD`, ForgeOther},
		{"R4", "git filter-branch --env-filter 'gh pr merge 1' HEAD", ForgeOther},
		{"R4", "git filter-branch --msg-filter 'sed s/a/b/' HEAD", Plain},
		{"R4", `git difftool -x "$CMD"`, ForgeOther},
		{"R4", "git difftool --extcmd='gh pr merge 1'", ForgeOther},
		{"R4", "echo x | xargs $CMD", ForgeOther},
		{"R4", "echo x | xargs -n 1 $CMD", ForgeOther},
		{"R4", "echo x | xargs -I{} $CMD {}", ForgeOther},
		{"R4", "echo x | xargs -0 rm -f", Plain},
		{"R4", "echo main | xargs git push origin", ForgeOther}, // the refspec comes from stdin
		{"R4", "echo 1 | xargs gh pr view", ForgeOther},
		{"R4", "echo x | xargs git", ForgeOther},
		{"R4", "echo f | xargs git add", Plain},
		{"R4", `find . -exec $CMD \;`, ForgeOther},
		{"R4", `find . -name '*.go' -execdir $CMD {} +`, ForgeOther},
		{"R4", `find . -ok gh pr merge 1 \;`, ForgeOther},
		{"R4", `find . -okdir git push origin main \;`, ForgeOther},
		{"R4", `find . -name '*.go' -exec gofmt -l {} +`, Plain},
		{"R4", `find "$DIR" -name x -exec rm {} \;`, Plain}, // an expanded path is not a command position
		{"R4", `find . -exec sh -c 'gh pr merge 1' \;`, ForgeOther},
		{"R4", "watch $CMD", ForgeOther},
		{"R4", "watch -n 5 gh pr merge 1", ForgeOther},
		{"R4", "watch -n 5 git status", Plain},
		{"R4", "parallel $CMD ::: a b", ForgeOther},
		{"R4", "parallel gzip ::: $FILES", Plain},
		{"R4", "parallel gh pr merge ::: 1 2", ForgeOther},
		{"R4", "ssh host $CMD", ForgeOther},
		{"R4", "ssh -i key -p 22 host \"$CMD\"", ForgeOther},
		{"R4", "ssh host git push origin agent/3kq7x2ma", ForgeOther}, // another host's credentials
		{"R4", "ssh host uptime", Plain},
		{"R4", "ssh host", Plain},
		{"R4", `flock /tmp/l -c "$CMD"`, ForgeOther},
		{"R4", "flock /tmp/l -c 'gh pr merge 1'", ForgeOther},
		{"R4", "flock -w 5 /tmp/l git push origin agent/3kq7x2ma", ForgePush},
		{"R4", "flock /tmp/l $CMD", ForgeOther},
		{"R4", "setsid $CMD", ForgeOther},
		{"R4", "stdbuf -o L $CMD", ForgeOther},
		{"R4", "stdbuf -oL git push origin agent/3kq7x2ma", ForgePush},
		{"R4", `script -q -c "$CMD" /dev/null`, ForgeOther},
		{"R4", "script -qc 'gh pr merge 1' /dev/null", ForgeOther},
		{"R4", "script -q /dev/null", Plain},
		{"R4", `script -qc "$CMD" /dev/null`, ForgeOther}, // a cluster ends at its first valued letter
		{"R4", "sudo -iu root $CMD", ForgeOther},
		{"R4", "sudo -iu root git push origin agent/3kq7x2ma", ForgePush},
		{"R4", "stdbuf -oL -eL git push origin agent/3kq7x2ma", ForgePush},
		{"R4", "sudo -u root $CMD", ForgeOther},
		{"R4", "sudo --user=root gh pr merge 1", ForgeOther},
		{"R4", "nice -n 10 $CMD", ForgeOther},
		{"R4", "env -S 'gh pr merge 1'", ForgeOther},
		{"R4", `env --split-string="$CMD"`, ForgeOther},
		{"R4", "env -C /tmp git push origin agent/3kq7x2ma", ForgePush},
		{"R4", "time -p git push origin agent/3kq7x2ma", ForgePush},
		{"R4", "timeout -s KILL 30 $CMD", ForgeOther},

		// M7: the reviewer's missing rows.
		{"M7", "git push --del origin agent/3kq7x2ma", ForgeOther},
		{"M7", `eval "git push origin main"`, ForgeOther},
		{"M7", "eval 'gh pr merge 1'", ForgeOther},
		{"M7", "exec git push origin main", ForgeOther},
		{"M7", "exec git push origin agent/3kq7x2ma", ForgePush},
		{"M8", "timeout 30 $CMD", ForgeOther},
		{"M8", "timeout $T git push origin agent/3kq7x2ma", ForgePush}, // the command is literal
		{"M8", "timeout git push origin main", ForgeOther},

		// Rows that pin each SAR branch (mutation testing).
		{"R1", "git --config-env user.name=N log", Plain},
		{"R1", "git --exec-path status", ForgeOther},
		{"R1", "GIT_SSH_COMMAND=x ls", ForgeOther},
		{"R1", "GH_TOKEN=x make deploy", ForgeOther},
		{"R1", "HOME=/tmp gh pr view 1", ForgeOther},
		{"R2", "git config --get remote.origin.url '^https'", Plain},
		{"R2", "git config --file f user.name bot", Plain},
		{"R2", "git config set user.name bot", Plain},
		{"R2", "git remote rename --no-progress upstream origin", ForgeOther},
		{"R3", "gh copilot", ForgeOther},
		{"R3", "gh m", ForgeOther},
		{"R4", `git rebase -x"echo $X" HEAD~1`, ForgeOther},
		{"R4", `git rebase -x "echo $X" HEAD~1`, ForgeOther},
		{"R4", "git rebase -x", Plain},
		{"R4", `git submodule add "$URL" lib`, Plain}, // only foreach runs a command
		{"R4", "git submodule foreach --recursive git push origin agent/3kq7x2ma", ForgePush},
		{"R4", `find . -ok $CMD \;`, ForgeOther},
		{"R4", `find . -okdir $CMD \;`, ForgeOther},
		{"R4", `find . -exec rm {} \; -exec $CMD \;`, ForgeOther},
		{"R4", "find . -exec gofmt -l {} + -exec $CMD {} +", ForgeOther},
		{"R4", `watch "echo $X"`, ForgeOther},
		{"R4", "watch echo $X", ForgeOther},
		{"R4", "watch git push origin agent/3kq7x2ma", ForgePush},
		{"R4", `env --split-string="echo $X"`, ForgeOther},
		{"R4", "flock /tmp/l -c", Plain},
		{"R4", "sudo -u", Plain},
		{"R4", "ssh host gh pr create", ForgeOther}, // another host's credentials
		{"scan", "echo gh api -XPOST repos/x/y", ForgeOther},
		{"scan", "echo /usr/bin/git push origin main", ForgeOther},
		{"N3", "gh pr --web list", ForgeOther}, // a flag before the verb, even before a read
		{"R4", "env -S 'git push origin agent/3kq7x2ma'", ForgePush},
		{"R4", "flock /tmp/l -c 'git push origin agent/3kq7x2ma'", ForgePush},
		{"R4", `git filter-branch --setup "$CMD" HEAD`, ForgeOther},
	} {
		if got := c.Classify("terminal", shell(tc.cmd), "LOW"); got != tc.want {
			t.Errorf("%s %q: got %q want %q", tc.finding, tc.cmd, got, tc.want)
		}
	}
}
