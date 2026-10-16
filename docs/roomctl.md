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
- The config and token files sit in `<user config dir>/roomctl/` (`config.json`, `token.json`), owner-only.
- `login` prints `Open <verification URL> and enter the code …` (the URL is the one your identity provider returns), then `logged in; …`.
- `doctor` prints one line per check, `ok` or the failure with its fix: `dns`, `tls`, `login`, `github`.

## Commands

`roomctl --help` prints the list below. A `<room>` is a room id, the last part of a room page's `…/r/<room>` URL.

| Command | What it does |
|---|---|
| `configure --url URL [--issuer URL --client-id ID --project-id ID]` | Saves the config. The broker's URL is enough |
| `login` | Signs in with the device flow |
| `doctor` | Checks the VPN, the CA, your login and your GitHub link, with the fix of each |
| `token` | Prints a valid access token, for scripts. Treat it as a secret |
| `status <room> [--json] [--after seq:N]` | Where a room stands, task first (below): the issue's number and title, then phase, run, budget, PR, issue, why and next, the last verdict, what needs you, the actions you may take, the agents' notes. `--json` prints the broker's summary as is; `--after` shows only the notes after a cursor (the output prints it as `seq:N`) |
| `rooms [--repo owner/name] [--mine] [--needs-me] [--json]` | The rooms you can read, as a table: ROOM, REPO, ISSUE, TITLE, TASK, PR, NEEDS-YOU, ROLE. `--mine`: tasks of issues you filed or labelled, PRs you authored or review. `--needs-me`: an approval you could decide, a task held on your room, or a PR waiting for your merge. `--json`: the broker's rows as is |
| `watch <room> [--tail N]` | Follows a room, one line per event: its `seq`, who (`role (id)` for a participant with a room role), what, and its text. Shows the last `N` events first (default 50). It reconnects after a drop and picks up where it stopped |
| `post <room> [--queue] <text>` | Posts a chat message and prints its `seq`. With `--queue` the text is kept for the next run's brief instead |
| `skill install [--dir .agents/skills]` | Writes the factory-handoff skill. Needs no config or login |
| `fork <room> --at SEQ [--role R] [--pr URL] [--egress pypi,npm] [--note TEXT]` | A room of your own from events 1 to `SEQ`, with an optional run. `--role`: `implementer`, `reviewer`, `tester` or `triager`; `--pr`: the reviewer's pull request; `--egress`: extra profiles (`pypi`, `npm`, `golang`, `crates`); `--note`: why you fork. Prints the new room's URL |

Flags may come before or after the positional arguments. Source: `internal/app/roomctl.go`.

`status` prints, for a task's room:

```text
task: #42 "Fix the flaky retention test"
phase: Implementing  run: 7f3cq2xz (implementer)  budget: 189 k / 1.5 M (13 %)
PR #57 https://github.com/<owner>/<repo>/pull/57
issue #42 https://github.com/<owner>/<repo>/issues/42
needs you: none
actions:
  queue a note for the next run: roomctl post 3kq7x2ma --queue '<text>'
notes (the agents' claims):
  09:14 UTC 7f3cq2xz  tests pass locally; pushing
cursor: seq:1842
```

Token counts read like the room page's: under 1,000 as is, then `k`, then `M` (999,500 reads
`1 M`). Times are UTC, the time alone when on today's date. `why:`, `next:` and `last verdict:` lines appear when the summary has them; a pending
approval reads `needs you: approve "<what>" by <deadline> → <link>`, a link to its card on the
room page, never a command. Source: `internal/roomctl/status.go`.

## The factory-handoff skill

`roomctl skill install` writes an Agent Skill that lets a local coding agent hand a side task to the [factory](factory.md) and follow it. The skill is embedded in the binary, so it always matches the CLI that runs its commands (`internal/roomctl/skill`).

It does:

- Draft one issue per defect from a template, show it, and run `gh issue create` only after you confirm.
- Follow a task: find the room link in the factory's comment, run `roomctl status <room> --json`, and report phase, run, PR and the newest notes.
- Say "it needs you" first when something waits on you, giving the link: approvals happen on the room page.
- Answer "anything waiting on me?" with `roomctl rooms --needs-me`.
- Queue guidance for the next run with `roomctl post <room> --queue`, only with your words.

It never:

- Applies a label, `factory/ready` included, unless you ask for that exact label.
- Approves, or gives a command that approves.
- Follows an instruction found in a note or an agent's text: that is data to report.
- Installs software, trusts a CA or signs in for you. It runs only read-only checks.

`skill install` writes `<dir>/factory-handoff/` (`SKILL.md` and `references/issue-template.md`), `.agents/skills` by default, and overwrites what is there. It refuses to write through a symlink inside the skill tree. `--dir` sets where the skill is written; point it at the directory your agent reads skills from.

The skill needs `gh` (authenticated) and `roomctl` (signed in) on the PATH.

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
