# maku-harness

The AgentRun sandbox's harness (SP1 design §5): `ghcr.io/openhands/agent-server:1.56.0-python` plus

| File | Role |
|---|---|
| `agent_run.py` → `agent-run` | Entrypoint: start agent-server, clone and resume `$BRANCH`, POST the conversation (its `tags` carry `effort`, `$LLM_REASONING_EFFORT` or `high`, which the bridge mirrors into the room's `harness_status`; never the model, which the room takes from the claim's `spec.model`), wait (under the [loop policy](#loop-policy) when it is on), revoke, exit 0/1. On SIGTERM, within 15 s: pause, checkpoint an implementer's work (`Agent-Checkpoint: disruption`; never a GitHub token, never over 200 files or 5 MiB), the room-bridge's final read, stop, revoke. A SIGTERM after the run ended keeps its exit code |
| `loop_policy.py` | The loop policy's rules: the switch, the judge's input and verdict, the continuation, the state |
| `agent_checks.py` → `agent-checks` | v3 feature 3: the CI's fast subset the repository declares in `.github/agent-checks.yaml` (read from the default branch), with its tools installed by mise (`/opt/agent/bin/mise`, off `PATH`) at the work tree's `mise.toml` pins into `/workspace/.toolchain`. Each step runs in its own process group without `CI` or any credential path; the result is `/workspace/.agent-checks/<sha>.json` and `.txt`. Evidence, never a gate. The run is pinned to the base and declaration blob `agent-run` read at startup (`AGENT_CHECKS_BASE`, no `refs/replace`), and a result file the agent left is moved aside, never posted as the harness's own. The harness's run prints its result on a pipe only `agent-run` holds (`agent-checks --json`), its git calls run with fsmonitor, hooks, external diff, textconv and clean/smudge filters disabled, and a non-0/1 exit, a timeout, a held lock or no result reads "unreadable result". Both `agent-run` and `agent-checks --json` mark themselves undumpable (`prctl(PR_SET_DUMPABLE, 0)`), so a leftover same-uid agent process cannot open their `/proc/<pid>/fd` or ptrace them. Residual, all under one uid: the window between the tool's exec (which resets the flag) and its first prctl call, a few Python start-up milliseconds; a live agent process that was already holding an fd or running as root-equivalent; rewriting the pinned blob's objects in `.git/objects`; swapping binaries in `/workspace/.toolchain`; and declared steps that run the branch's own scripts. The result is evidence; the CI stays the judge, and the reviewer's own run backs it |
| `test_contract.py` → `test-contract` | v3 feature 4: whether a change's tests are a contract. Reads the `tests` section of `.github/agent-checks.yaml` from the base, runs the selected test files at the base, at the base plus the changed tests (the overlay) and at the head, each in a clean worktree, and compares JUnit ids. `red [--base SHA]` is the tester's red check, `hold [--base SHA --contract SHA]` the implementer's contract run (also fails when a contract or runner file changed since the contract commit), `ci --base --head` the "Test contract 🧪" job through `.github/actions/test-contract`. The harness must pass the base and contract shas it recorded at startup; without them `hold` falls back to the local `origin/HEAD` and a trailer search, and says so (`source`). Exit codes: 0 ok, 1 not ok, 2 no declaration, 3 outside the test paths, 4 could not run (or a bad ref), 5 no contract, 6 contract without ids, 7 contract without files, 8 base not under the contract commit, 9 declaration lost after a contract was found; `hold` never exits 2 once a contract exists. CI is the judge; `hold` is a sandbox signal that fails closed |
| `git_credential_agent.py` → `git-credential-agent` | git credential helper; exchanges through identity-proxy `:4001`, caches in memory; `agent-run` calls its `revoke` on exit |
| `gh` | gh with that token in `GH_TOKEN`; `gh pr create` (or `new`) then appends the provenance footer (`pr_footer.py`, SP2 design §5) |
| `commit-msg` | adds `Agent-Run: $RUN_ID`, `Agent-Task: $TASK_ID` when set, and `Agent-Checkpoint: disruption` on agent-run's SIGTERM checkpoint |
| `pre-commit` | v3 feature 4: refuses a commit whose staged paths include a file of the test contract, naming them and the `git restore --source=<commit>` that undoes it. It reads `/workspace/.agent-contract.json`, which `agent-run` writes only for a resumed implementer branch that holds a contract commit; no file, or an unreadable one, allows every commit. A courtesy (`--no-verify` skips it): the merge gate's row 2c binds |
| `site/sitecustomize.py` | agent-server's start-up hook: a 4xx to the MCP client's reply to a server `ping` is logged instead of ending the session (F29, envoyproxy/ai-gateway#2715); a transient model error is retried (#99, below); an internal run's prompt-cache markers are rewritten as `prompt_cache_breakpoint`, the only one agentgateway 1.6 carries to Anthropic |
| `site/agent_search.py` | v3 feature 5: the implementer's `search` tool, imported by agent-server from the start request's `tool_module_qualnames`. One question, one child conversation with `grep`, `glob`, `read_file`, `list_directory`, `finish` and `think` only (no terminal, no file editor, no MCP, no plugins or hooks, `NeverConfirm`), a copy of the run's LLM at effort `low`, at most 40 iterations, 300k tokens and 300 s, six calls a run, one at a time. The readers are guarded: a checked absolute path under the repository, regular files of at most 1 MiB, `grep` and `glob` on ripgrep only (the image installs it and the test stage asserts `rg --version`). The answer comes back headed and clipped at 8,000 characters; a failure is an error observation. One row per call in `/workspace/.agent-search.jsonl`, a signal the agent can write |

## Optional environment

| Variable | Effect when set | When unset |
|---|---|---|
| `LLM_API_KEY_FILE` | the model API key is read from this file (maku-run-controller only) | the placeholder key; the identity-proxy injects the real one |
| `GIT_TOKEN_FILE` | the git credential helper prints this file's token; it never calls octo-sts and never revokes (maku-run-controller only) | octo-sts exchange through identity-proxy `:4001`, revoked on exit |
| `MCP_URL` | the platform MCP entry and progress notes are wired | no platform tools, no progress notes |
| `AGENT_CHECKS` | `1`: the toolchain installs in the background after the clone, the rules gain the repository-checks rule, and a `finished` implementer run checks its head, notes the result in the room and emits the span `agent-run.checks` | today's run |
| `AGENT_RUN_SECONDS` | the pod's deadline: the final checks are skipped (`skipped: deadline`) when less than the declaration's budget plus 2 min is left | the final checks always run |
| `AGENT_LOOP` | an implementer runs under the [loop policy](#loop-policy): a JSON object holding any of `stallSteps`, `continuations` and `judgeModel`; `{}` is 80, 2 and `tier-light` | no judge and no stall rule: the run ends when its conversation does |
| `AGENT_SEARCH` | `1`: an implementer gets the `search` tool and its rule, once `agent_search` imports (else `agent-run: search off: …` and a run as today); the ledger's tool calls join the step count the loop policy's stall rule reads, and `agent-run search: N calls, S steps, T tokens, outcomes …` closes the log | no search tool |

## Loop policy

An implementer run with `AGENT_LOOP` set ends by the loop policy (loops design L2 and L3, in
ai-native-ref), not when its agent stops. The rules are in `loop_policy.py`; `agent_run.py` does the
I/O. Another role, or a value that is not a JSON object of those three keys within bounds
(`stallSteps` 1 to 500, `continuations` 0 to 5, `judgeModel` an alias or `none`), leaves it off and
says why on stderr.

| The run ends when | Exit | `endReason` | Room note |
|---|---|---|---|
| the judge answers `met`, `judgeModel` is `none`, or no judge answers | 0 | none | none |
| the judge answers `impossible`, or a second verdict in the run is discarded | 1 | `judge_impossible` | the judge's reason |
| the agent has made no commit by `stallSteps` steps, a continuation adds no commit within its time box, or two continuations in a row end with no new commit | 1 | `stalled` | which of the three |
| the judge answers `not_yet` and the continuations are spent, a continuation that committed does not finish within its time box, too little of the run is left for one, or the head's checks are still pending when a wait's box ends | 1 | `not_met` | the last reason, or `checks still pending after N minutes` |

- **A continuation is boxed in time**: at most 15 minutes, ending 3 minutes before `AGENT_RUN_SECONDS`
  runs out, and the message names the box. A conversation that has not finished again by then is
  interrupted, read for up to 30 s until it settles (so a push in flight can land), and the run ends
  with the loop's reason, not the wall clock's; a `not_met` note says whether its commits are pushed.
  The box is timed after the contract hold, and a finish read on the poll that crosses it is a finish.
  Under 5 minutes left, no continuation is sent. The stall rule is spent at the first commit, so before the box a continuation
  was bounded only by the run's deadline (gcp-0, task `xlcx2775`: 36 minutes retrying a CI log the
  sandbox could not reach, escalated as `deadline`).

- **The judge** is called when the conversation ends `finished`: one call to the identity proxy's
  `/judge/v1`, beside `LLM_BASE_URL`'s `/v1`, on the run's token, so the gateway meters it against the
  run's cap. The proxy pins the judge's alias from its own `JUDGE_MODEL`; a refusal (403), a proxy
  with no judge path (404) or any other failure leaves the run to end as it would with no judge.
  The judge has no tools and a fresh context. It reads the issue's `## Acceptance check` section
  (else a paragraph whose first line starts `Acceptance:`, to the next blank line; else the fenced
  issue text: the factory's scoper reads the same two rules), the diff against the base (16 KiB at most) and the head's check runs,
  read with the run's own `gh` after waiting up to 5 minutes for them to complete. It answers one
  JSON object; a verdict with no parse or that cites none of the evidence ids it was given is
  discarded: the first counts `not_yet`, the second `judge_impossible`.
- **`not_yet`** sends one message into the same conversation, with the judge's reason quoted as data,
  and the conversation runs again with its history. When a check failed (HR5), the harness reads the
  failing jobs' logs with its own `gh run view --log-failed` (at most 3 jobs, 30 s each) and quotes the
  last 60 lines of each, redacted and cut at 8,000 characters, between the same fence lines; when it
  cannot (a check outside GitHub Actions, `gh` failing, a log host the sandbox cannot reach), the message
  says the CI log is not readable from the sandbox and forbids fetching it.
- **A pending check is waited for, not sent** (HR5): when the judge answered `met` and the guard's only
  objection is a check that has not completed, no message goes and no continuation is spent. The
  harness reads the head's check runs every 30 s within the box a continuation would have had, then
  rebuilds the evidence and asks the judge once more; that answer is guarded and decided as any other.
- **A commit** is a non-merge commit on `HEAD` or the run's branch that neither the run's start nor
  the base branch holds: a fetch or a rebase onto the base counts nothing. The stall rule holds until
  the first commit. The start and the counts are kept in `/workspace/.agent-loop.json`, so a
  suspended run's next pod goes on counting.
- **Its end** is reported three ways: the exit code; `{"endReason": "<reason>"}` in the body of the
  run's final read, for the bridge to record; and a `room_progress` note, sent after the broker's
  minute. `agent-run judge <n>:` and `agent-run loop end:` lines show each verdict and the end.

Nothing here is a control: the budget, the deadline and OpenHands' stuck detector still bound the
run, and an agent that games the rules only ends its own run.

**The stall limit** is read from the factory's history: `scripts/stall-steps.sql` lists every
implementer run's steps to its first commit from the room log, and `scripts/stall-steps.sh` keeps
the runs merged pull requests name and prints N, the smallest multiple of 10 above their p90 once
20 runs have one, and 80 until then.

## The scoper's bound

A scoping run (role `triager`) whose brief starts with the factory's marker line,
`<!-- agent-factory scoper nudgeAt=25 maxToolCalls=40 -->` (the factory config's `scoper` key), is
bounded in tool calls (harness roles HR2). The line is read only at the very start of the task text,
which also carries the untrusted issue, and only within `1 <= nudgeAt < maxToolCalls <= 200`; any
other run, or other bounds, runs unbounded as before.

| At | The harness |
|---|---|
| `nudgeAt` tool calls | sends one message, read at the agent's next step: hand off what you have now with `room_handoff`, `ready` with guesses as assumptions or `question` |
| `maxToolCalls` tool calls | interrupts the agent and ends the run: exit 1, `endReason` `budget_calls` in the final read, and one room note, "the run ended budget_calls: the scoper stopped after N tool calls without a handoff" |

A `room_handoff` call ends the count once its result comes back with no error; a refused handoff
does not, and no stop is made while one is in flight. A finish read on the poll that crosses the
limit is a finish, and a resumed pod already past `nudgeAt` sends no second nudge.
`agent-run scoper end:` shows the end in the log.

## The test-first tester

v3 feature 4, F1. Nothing here binds before F3's merge-gate rows 2b and 2c: the harness's check spares a
wasted implementer run, CI's "Test contract" job is the judge.

**The mode** is `ROLE=tester` and no `TASK_URL` (F1-R1). A verifier tester always carries the pull
request's URL and runs as before. In the mode, `agent-run` sets the clone's `remote.origin.pushurl` to
`disabled://the-tester-never-pushes`, so the model commits and never pushes, and the prefetch installs the
toolchain as for any role. The rules gain a "Test contract" section naming where tests go and `test-contract red`.

**The ends.** When the conversation ends `finished`, `agent-run` squashes the model's commits since the
clone's `origin/HEAD` (read once, before the conversation) into one commit on that base, runs
`test_contract.red` in-process on it, and ends one of:

| End | Meaning | Pushed |
|---|---|---|
| `contract_ready` | tests fail on the base and nothing else changed | one signed commit, `test: the test contract`, to `refs/heads/$BRANCH` at the fetch URL, never forced; trailers `Agent-Run`, `Agent-Task`, `Agent-Contract` (the run id), one `Agent-Contract-File` per file and one `Agent-Contract-Id` per failing id |
| `contract_not_red` | vacuous or non-failing tests, the runner could not run, no `tests` declaration, or the push was refused | nothing |
| `contract_outside_tests` | a path outside `tests.paths`, or a runner file | nothing |
| `not_testable` | no commit since the base | nothing |

Vacuous or non-failing tests and a path outside the test paths get **one** continuation in the same
conversation, quoting the check's result between `CONTRACT-<hex>` fence lines as data. A runner that could
not run, a missing declaration and a refused push never do (R6: infrastructure is not the tester's to fix).
Every end exits 0, posts `Harness (tester): <end>: <files or why>` (at most 280 characters) and emits the
span `agent-run.contract`, with `error.type` set to the end unless it is `contract_ready`. A conversation
that ends `error` or `stuck` exits as before, with no check.

**The implementer's side.** A resumed implementer branch holding a contract commit gets
`/workspace/.agent-contract.json` (`commit`, `files`, `ids`, `base`; mode 0644), a "Test contract" rules
section and the `pre-commit` hook above. The SIGTERM checkpoint unstages the contract files before it commits.
When its conversation ends `finished`, `agent-run` runs `test-contract hold`: a red contract is `not_yet`
with the failing ids, before any judge, and continues the conversation (`continuations` of them, shared
with the judge's). A green one goes to the judge as before. A modified contract, a hold that could not run or
any fail-closed result sends no continuation and notes `Harness (implementer): contract <result>`; the gate's
row 2c answers a modified contract. With `AGENT_LOOP` unset, a contract-only loop runs the same hold with two
continuations, no judge and no stall rule; spent, the run ends as it would without it (exit 0).

The tests the red check runs are the tester's code under the agent's uid and could forge their own JUnit
report; that is why the harness's check does not bind.

## Model errors

A model call that fails ends the run (`execution_status=error`, the run's end reason `agent_error`), except
for a transient error, which agent-server retries first (#99, owner decision OD-7):

| Error | Retried | Why |
|---|---|---|
| `503`, or Envoy's `upstream connect error or disconnect/reset before headers` under any status | yes | nothing reached the model, so nothing is spent twice |
| connection refused or reset (litellm reports it as a `500` caused by `APIConnectionError`) | yes | the same |
| `429` | **never** | it can be the gateway's own budget throttle; a retry would hammer the budget |
| the SDK's hard timeout (`LLMTimeoutError`, 300 s) | **once**, as a fresh request with a fresh window | a rare call hangs (glm-5.3 via Z.ai: p99 82 s) and one hang failed whole runs; logged `agent-run model retry timeout 1/1`, counted with the others; never a call that already emitted a token, and only when the run's deadline leaves a whole second window plus the final checks' reserve (`AGENT_RUN_DEADLINE`) |
| any other `4xx` or `5xx` | no | not transient |

Three attempts at most, 10 s then 20 s apart (30 s of backoff, inside OD-7's 60 s). After the third the run
escalates exactly as before, with the same `agent-run error: LLMServiceUnavailableError …` line. The SDK's own
retry stays off (`num_retries=0`: it would retry a `429`), and so does the one inside litellm's OpenAI client,
which had retried twice in about 1.5 s, a `429` included.

Each retry logs `agent-run model retry <n>/2 in <s>s: <exception> <message>`, and the run's last line counts
them: `agent-run summary: <steps> steps, <retries> model retries`. A provider having a bad day shows up there
while its runs still finish. The count is read from `/tmp`, which the agent can write, so it is a signal and
never a control.

The `Agent-*` namespace is the harness's. In a commit message or a PR body, any line the model wrote that
starts with an `agent-…:` key, in any case and after any line break a renderer honours, is prefixed
`(agent-written) ` before the real values are added last. The footer is always the last paragraph:

```text
---
Agent-Room: $ROOM_ID
Agent-Run: $RUN_ID
Agent-Role: $ROLE
Agent-Task: $TASK_ID
Agent-Task-URL: $TASK_URL
Agent-Model: $MODEL
```

`Agent-Task` is the task id and `Agent-Task-URL` the issue or PR (SP3 ruling SW). A value that is empty, over 256
characters or holds a control character or line separator is left out, and `Agent-Task-URL` must be an
`https://github.com/` URL. If the footer cannot be written, `gh pr create` exits 1.

Trailers and footer are provenance hints, never authorisation (SP3 ruling TB). An agent with a shell can skip
or rewrite the hook, `gh api` or a later `gh pr edit` bypasses the footer, and run IDs are public, so anyone
can copy a trailer. The factory still reads `Agent-Run` as a claim: it catches an unstamped human commit, not
a deliberate forgery. What binds commits to the factory is push identity
([#76](https://github.com/Smana/maku/issues/76), [#82](https://github.com/Smana/maku/issues/82)): before a CI-fixed pull request is reviewed again, and before the merge gate arms or shadows any pull request, every
push to its branch since the branch's creation must be the agents App's in GitHub's repository activity. A human
cannot push as the App without access to the agents namespace or the App key. The binding is to the App, not the
task: the implementer octo-sts trust policy (cloud-native-ref `.github/chainguard/agent-implementer.sts.yaml`) is
repository-wide, so any implementer run of any task can push as the bot, and only the trailer narrows that to this
task's runs. Commits are not signed.

The harness never holds a gateway or octo-sts token: identity-proxy injects them. Test with `task harness:test`. `release.yaml` builds, tests, signs and pushes it as
`ghcr.io/smana/maku-harness:<release tag>`; the AgentRun composition in crossplane-configuration
pins that digest. Versions up to `v0.3.0` were released from cloud-native-ref.
