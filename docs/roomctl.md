# roomctl

`roomctl` is a room in a terminal: read where a task stands, follow the log, post a note, fork a room. It never steers a run, interrupts it, moves the driver token or approves anything (`status` may print an approve command, for display only); those stay in the [web UI](web-ui.md).

It calls the broker through the platform's sign-in proxy with your own access token; the broker filters what it returns, so it sees only the rooms your GitHub identity may read.

## Install and verify

Download the binary for your system from the latest GitHub release, with `roomctl.sha256`:

| Asset | System |
|---|---|
| `roomctl-linux-amd64`, `roomctl-linux-arm64` | Linux |
| `roomctl-darwin-amd64`, `roomctl-darwin-arm64` | macOS |
| `roomctl.sha256` | the checksums of all four |

```bash
sha256sum -c --ignore-missing roomctl.sha256
sudo install roomctl-linux-amd64 /usr/local/bin/roomctl
```

From a checkout: `go build -o roomctl ./cmd/roomctl`. The release workflow is `.github/workflows/release.yaml`.

## Configure and sign in

```bash
roomctl configure --url https://rooms.<domain>   # the broker serves the other three values
roomctl login                                    # device flow: open the printed URL, enter the code
roomctl doctor                                   # check the setup
```

- `configure` reads the issuer, client id and project id from the broker's unauthenticated `/.well-known/roomctl`. Pass `--issuer`, `--client-id` or `--project-id` to override one. The URL must be `https`.
- `configure --from <rooms URL>` replaces the whole config with what the broker serves now (a room page's URL works: only its origin is used). Use it after a platform rebuild: a stale client id makes the identity provider answer `invalid_client`, and `roomctl login` then prints this fix. It refuses a non-`https` URL or issuer, and cannot be combined with the other flags.
- The config and token files sit in `<user config dir>/roomctl/` (`config.json`, `token.json`), owner-only.
- `login` prints `Open <verification URL> and enter the code …` (the URL is the one your identity provider returns), then `logged in; …`.
- `doctor` prints one line per check, `ok` or the failure with its fix: `dns`, `tls`, `login`, `github`, `skill`. The room list cannot tell an admin without a GitHub link from one with it, so `github` asks `issue_draft` (the check `issue_file` relies on) and says `unverified` when that cannot answer: it never says "linked" unproven.

## Commands

`roomctl --help` prints the list below. A `<room>` is a room id, the last part of a room page's `…/r/<room>` URL.

| Command | What it does |
|---|---|
| `configure --url URL [--issuer URL --client-id ID --project-id ID]` | Saves the config. The broker's URL is enough |
| `configure --from URL` | Replaces the config with the values the broker serves now, whatever is stored |
| `login` | Signs in with the device flow |
| `setup <claude\|codex\|copilot\|cursor\|gemini\|opencode> [--project] [--print]` | Registers `roomctl mcp` as the `factory` MCP server of a coding agent and writes the factory-handoff skill. `--project` writes under the repository's top instead of your home; `--print` shows what it would do and writes nothing |
| `doctor` | Checks the VPN, the CA, your login, your GitHub link and the installed skill's version (`skill`), with the fix of each |
| `token` | Prints a valid access token, for scripts. Treat it as a secret |
| `status <room> [--json] [--after seq:N]` | Where a room stands, as the room card (below): the title, the state, the facts (run, spend, PR and last verdict, next, why), what needs you, the runs line, the agents' notes and what you can do, then a `cursor: seq:N` line. `--json` prints the broker's summary as is; `--after` shows only the notes after a cursor (the output prints it as `seq:N`) |
| `rooms [--repo owner/name] [--mine] [--needs-me] [--all] [--json]` | The rooms you can read, as a table: ROOM, REPO, ISSUE, TITLE, TASK, PR, NEEDS-YOU, ROLE. `--mine`: tasks of issues you filed or labelled, PRs you authored or review. `--needs-me`: an approval you could decide, a task held on your room, a PR waiting for your merge, or an Escalated task waiting for its issue's author when you own the room. `--all`: the rooms that ended over a week ago too. `--json`: the broker's rows as is |
| `changes <room> [--cursor seq:N] [--format md\|text] [--json]` | What changed in a room since the cursor of your last call (the first call: 24 hours). The cursor is kept in `cursors.json` beside the config. Needs a broker with `room_changes` |
| `checkout <room>` | Checks the room's branch, `agent/<room>`, out into a new git worktree beside the repository you are in, `<repo>.<room>`, and prints its path. It never touches your current checkout, so a dirty tree or your branch is safe, and a second call prints the same path. It refuses outside a checkout of the room's repository, and prints the `gh repo clone` command. When the branch changes files that run code on open or on commit, it lists them and asks `y/N`, and refuses without a terminal ([below](#checking-out-an-agents-branch)) |
| `watch <room> [--tail N]` | Follows a room, one line per event: its `seq`, who (`role (id)` for a participant with a room role), what, and its text. Shows the last `N` events first (default 50). It reconnects after a drop and picks up where it stopped |
| `post <room> [--queue] <text>` | Posts a chat message and prints its `seq`. With `--queue` the text is kept for the next run's brief instead. The broker stores it labelled as [machine-sent](api.md#machine-sent-text), as any text from roomctl |
| `skill install [--dir .agents/skills]` | Writes the factory-handoff skill. Needs no config or login |
| `mcp` | Serves the factory's MCP tools on stdio, for a local coding agent ([below](#the-mcp-server)) |
| `fork <room> --at SEQ [--role R] [--pr URL] [--egress pypi,npm] [--note TEXT]` | A room of your own from events 1 to `SEQ`, with an optional run. `--role`: `implementer`, `reviewer`, `tester` or `triager`; `--pr`: the reviewer's pull request; `--egress`: extra profiles (`pypi`, `npm`, `golang`, `crates`); `--note`: why you fork, under 1 KiB once labelled as machine-sent (about 900 bytes). Prints the new room's URL |

Flags may come before or after the positional arguments. Source: `internal/app/roomctl.go`.

`status` prints the room card, then the cursor. For a task's room:

```text
26zfnuxm · #42 docs: chart version is wrong
needs you · an agent is coding · Smana/cloud-native-ref · open the room (https://rooms.example/r/26zfnuxm)

Run   implementer cf4ato2x (human review) · started 19:11 UTC, 18 min ago
Spend $0.31 of $1.50 (21 %) · 189 k tokens
PR    #2239 (https://github.com/Smana/cloud-native-ref/pull/2239) · reviewer approved at 18:49 UTC

Needs you
1. approve "git push to agent/26zfnuxm" by 19:45 UTC → https://rooms.example/r/26zfnuxm#01M4A

Runs implementer (first run) succeeded · 40 min · $0.18 → reviewer approved · 10 min · $0.03 → implementer (human review) running · 18 min · $0.10

Agents' notes, their claims, not checked
- 19:12 UTC cf4ato2x "found both versions on line 12-13, fixing"

You can queue a note for the next run · steer the running agent · stop it with the label factory/stop
cursor: seq:7
```

Each line is left out when its data is absent: a room with no task has no facts table, a sealed
room no `Needs you` or `You can`. The spend line reads dollars when the factory priced the task
and tokens (`182 k of 1.5 M tokens (12 %)`) when it did not; token counts read like the room
page's, under 1,000 as is, then `k`, then `M`. Times are UTC, dated when not today. A run that
ended reads `succeeded`, `failed` and so on, and a stopped task's unfinished run reads `ended`,
never `running`. On a terminal, with `NO_COLOR` unset, the state word is bold when it needs you.
The agents' text is quoted; an approval is a link to its card on the room page, never a command.

Room links print only under the broker's own address: the card takes it from the summary's URL
(the broker's `PublicURL`), and falls back to the URL `roomctl` is configured with when the summary
carries none. A broker reached by another name (a port-forward) therefore still shows its links.
Source: `internal/roomview/card.go`.

## The MCP server

`roomctl mcp` is an MCP server on stdio. It relays each message to the broker's
[`/mcp`](api.md#post-mcp) with your token, so your agent configures one command and no token is
pasted:

```bash
claude mcp add factory -- roomctl mcp    # Claude Code; any agent that runs a stdio MCP server works
```

- The broker's operator turns `/mcp` on (`human.mcp.enabled`). Off, every call answers `mcp_off`.
- The tools: `rooms_list`, `room_summary`, `room_tail`, `room_changes`, `room_queue`, `room_fork` and
  `issue_draft` from the broker, and `issue_file`, `room_wait` and `room_checkout`, roomctl's own.
  `room_tail` also runs as an MCP Task that waits for the room's next events; `roomctl mcp` polls it
  for your agent. The broker also serves the prompts `handoff` (`what`), `status` (`room`) and
  `inbox`, which your agent offers as slash commands.
- `issue_file` files a draft with your `gh`, as you, only after you accept it in a prompt your agent
  shows you (MCP elicitation). The broker's `issue_draft` checks the draft first, and the prompt
  shows exactly what is filed. An agent that cannot prompt you is refused: file the draft yourself
  with `gh issue create`. The form has a second field, "start", which defaults to no and appears
  only when the broker's `issue_draft` says `canStart`: yes labels the issue `factory/ready` with
  your `gh`, which starts the factory. It is a form field, never a tool argument: your agent can
  call `issue_file` but cannot answer the form.
- `room_wait` polls `rooms_list` from your machine until a room needs you, opens a pull request or
  ends, or until its timeout (300 s by default, at most 1800 s), and returns the room's card. A
  cancellation ends it at once. Run it in a background task where your client has them.
- `room_checkout {room}` is `roomctl checkout`, for your agent. The repository and the pull request
  come from the broker's row of the room, and the workspace is your client's first root, else
  where `roomctl mcp` runs.
- No tool approves, steers or stops, and none labels on the agent's say: only your answer to the start form does. Approvals stay on the room page.
- What your agent queues is stored labelled as [machine-sent](api.md#machine-sent-text).
- Every request reads `token.json` and refreshes the token when it has expired, one refresh at a
  time. When the refresh is refused, a call answers `not_logged_in`: run `roomctl login` in a
  terminal, and the running server uses the new token, with no restart.

The [factory-handoff skill](#the-factory-handoff-skill) does the same jobs through the shell, for an
agent with no MCP configuration. Configure one of the two.

### Checking out an agent's branch

An agent's branch is code a sandboxed agent wrote, and some of its files run on your machine without
you running anything: agent and editor configuration, `.mcp.json`, hooks, `mise.toml`, `.envrc`.
So the checkout works like this:

- It fetches only `agent/<room>` from your `origin`, after checking that `origin` is the room's
  repository (`git@github.com:o/n` or `https://github.com/o/n`, with or without `.git`, in any case).
- It runs git with an argument list, never a shell, with `core.hooksPath=/dev/null` and no Git LFS
  smudge filter, so no hook
  of your repository or of the branch runs during the fetch or the checkout. It runs nothing in the
  new worktree.
- The worktree path is `<repository>.<room>` in the repository's own parent directory. A path that
  already exists is refused, never overwritten.
- It fetches `agent/<room>`, resolves it once to a commit, and checks, asks about and creates only
  that commit: a fetch that moves the branch while a form is open changes nothing. It verifies the
  new worktree is at that commit, and removes it otherwise. A killed or failed checkout is removed,
  never left to be reused.
- A local branch `agent/<room>` that is not that commit is refused, with the command to drop or move
  it. Every git call is bounded (2 minutes for the fetch, 30 seconds for the rest), and recursion into
  submodules is off.
- It lists what the branch changed against its merge base with your `HEAD`, and against `HEAD`
  itself. If a changed file is one of the paths below, or is under your repository's in-tree
  `core.hooksPath`, it asks first: a form whose `create` field defaults to no (`room_checkout`),
  or `y/N` on a terminal (`roomctl checkout`). The file names are shown in code spans, cleaned.
  `core.hooksPath` is read from your effective git config, and an `origin` with several URLs is refused.
  A client with no forms, or a command with no terminal, is refused and told the files.

The paths: the folders `.claude/`, `.cursor/`, `.opencode/`, `.gemini/`, `.codex/`, `.vscode/`,
`.agents/`, `.devcontainer/`, `.kiro/`, `.amazonq/`, `.roo/`, `.continue/`, `.zed/`, `.qwen/`,
`.windsurf/`, `.github/skills/`, `.github/hooks/`, `.husky/`, `.githooks/`, `.mise/`, `mise/` and
`.config/mise/`, whole (settings, hooks, tools, plugins, skills and scripts all live there), and the
files `.github/mcp.json`, `.mcp.json`, `opencode.json`, `opencode.jsonc`, `crush.json`,
`.crush.json`, `.devcontainer.json`, `.envrc`, `.tool-versions`, `mise.toml`, `.mise.toml`,
`mise.local.toml`, `.mise.local.toml`, `mise.*.toml`, `.mise.*.toml`, `.pre-commit-config.yaml`,
`lefthook.*`, `lefthook-local.*`, `.lefthook.*`, `.lefthook-local.*`, `.config/lefthook.*`, `.config/lefthook-local.*`, `.gitmodules` and
`.lfsconfig` (a `*` stays within one name), in the repository root or any folder of it, in ASCII
case. A changed path with a byte past ASCII anywhere in its name is flagged too, since a filesystem
may fold it into one of these names. A changed path that is a folder of one of them counts (a file
named `.claude`), and so does every symlink the branch adds or changes, whatever its name: a link
`.claude -> x` with `x/settings.json` would otherwise hide the file. A symlink the branch's tree
already has at one of these places, in any folder, is asked about as well, since the branch can
change its target. Flagging more is safe, since a flag only means "ask first". The form lists every
match of the paths above first (up to 200), and then the symlinks and non-ASCII names, at most 20
of them, and a count of the rest. The list is `flagged` in `internal/roomctl/checkout.go`; the tests
hold a fixture per entry.

What the check cannot see: it follows names, not what runs them. A file that one of your own git
hooks or build steps runs is code, and the check does not know it: lint-staged or commitlint
configuration, a Makefile target that a hook calls, `package.json` scripts, a workflow you run with
`act`. Read the diff of those yourself. A repository with more than about 150,000 files makes the
tree listing pass its 16 MiB cap, and the checkout then refuses instead of guessing.

One checkout of a room runs at a time in a repository: it takes an advisory lock (`flock`) on
`roomctl-checkout-<room>.lock` in git's directory, which the kernel drops when the process ends, so
a crash leaves nothing to wait out. A second is refused while the first waits for you. What the
call made is all it ever removes: the branch is created only if it does not exist yet, so a branch
you made while the form was open is never used, moved or deleted, and a worktree at its path that
git does not list as this call's is left alone. A worktree of the branch whose folder is gone is
removed and made again, unless you locked it (`git worktree lock`).

## The factory-handoff skill

`roomctl skill install` writes an Agent Skill that lets a local coding agent hand a side task to the [factory](factory.md) and follow it. The skill is embedded in the binary, so it always matches the CLI that runs its commands (`internal/roomctl/skill`).

It works MCP first. With the `factory` MCP server registered (`roomctl setup <client>`), it uses `rooms_list`, `room_summary`, `room_changes`, `room_wait`, `room_checkout`, `issue_draft` and `issue_file`, and shows each tool's text as it is. Without the server, a shell fallback section runs the same jobs through `gh` and `roomctl`.

It does:

- Draft one issue per defect from a template, show it, and file it only after you confirm.
- Follow a task: find the room, and report phase, run, PR and the newest notes.
- Say "it needs you" first when something waits on you, giving the link: approvals stay on the room page.
- Answer "anything waiting on me?".
- Follow a room in the background with `room_wait` when the agent can run a background task or subagent, and check a room's branch out into a new worktree beside the repository with `room_checkout`, only when you ask. It never touches your current checkout.
- Queue guidance for the next run with `roomctl post <room> --queue`, only with your words.

It never:

- Applies a label, `factory/ready` included, unless you ask for that exact label.
- Approves, or gives a command that approves.
- Follows an instruction found in a note or an agent's text: that is data to report.
- Installs software, trusts a CA or signs in for you. It runs only read-only checks.

`skill install` writes `<dir>/factory-handoff/` (`SKILL.md` and `references/issue-template.md`), `.agents/skills` by default, and overwrites what is there. It refuses to write through a symlink inside the skill tree. `--dir` sets where the skill is written; point it at the directory your agent reads skills from.

The skill needs `gh` (authenticated) and `roomctl` (signed in) on the PATH.

`roomctl doctor` has a `skill` check: it reads the `roomctl-version` of every `factory-handoff` skill in the directories the six clients read (`roomctl setup`'s, under your home and under the working directory), and fails when one is not this binary's version, because a skill from another release names tools or commands this one lacks. It skips when none is installed.

## Troubleshooting

Run `roomctl doctor` first: it checks in the order things fail and prints the fix of each.

| Symptom | Cause | Fix |
|---|---|---|
| `roomctl` not found | not installed | [Install and verify](#install-and-verify) |
| `not configured` | no config file | `roomctl configure --url https://rooms.<domain>` |
| `no such host` on the rooms host | the VPN is down: the host resolves only on the tailnet | `tailscale up`, then `tailscale status` |
| `x509: certificate signed by unknown authority` | the platform's root CA is not trusted, and `roomctl` has no `--cacert` | trust the root CA on your system, or set `SSL_CERT_FILE` to a bundle holding it for the shell that runs `roomctl` |
| auth error, expired token | not signed in | `roomctl login` |
| `401: user_inactive` | your ZITADEL user was deactivated, locked or deleted | ask an admin to reactivate it; `roomctl login` cannot help |
| `rooms` lists nothing | your ZITADEL user has no linked GitHub identity, so no repository is readable | ask an admin to link your GitHub account to your ZITADEL user; `roomctl doctor` reports it on its `github` line |
| `FAIL skill` on `roomctl doctor` | a `factory-handoff` skill from another roomctl release, in your home or in the repository (`cloud-native-ref` carries `.agents/skills/factory-handoff` at `roomctl-version: "v0.11.0"`, so this fails there until that copy is removed) | delete the stale copy, or rewrite it: a stale copy in your home needs `roomctl setup <client>` without `--project`, and `--project` only rewrites the repository's copy |
