# agent-harness

The AgentRun sandbox's harness (SP1 design §5): `ghcr.io/openhands/agent-server:1.56.0-python` plus

| File | Role |
|---|---|
| `agent_run.py` → `agent-run` | Entrypoint: start agent-server, clone and resume `$BRANCH`, POST the conversation, wait (under the [loop policy](#loop-policy) when it is on), revoke, exit 0/1. On SIGTERM, within 15 s: pause, checkpoint an implementer's work (`Agent-Checkpoint: disruption`; never a GitHub token, never over 200 files or 5 MiB), the room-bridge's final read, stop, revoke. A SIGTERM after the run ended keeps its exit code |
| `loop_policy.py` | The loop policy's rules: the switch, the judge's input and verdict, the continuation, the state |
| `agent_checks.py` → `agent-checks` | v3 feature 3: the CI's fast subset the repository declares in `.github/agent-checks.yaml` (read from the default branch), with its tools installed by mise (`/opt/agent/bin/mise`, off `PATH`) at the work tree's `mise.toml` pins into `/workspace/.toolchain`. Each step runs in its own process group without `CI` or any credential path; the result is `/workspace/.agent-checks/<sha>.json` and `.txt`. Evidence, never a gate. The run is pinned to the base and declaration blob `agent-run` read at startup (`AGENT_CHECKS_BASE`, no `refs/replace`), and a result file the agent left is moved aside, never posted as the harness's own. The harness's run prints its result on a pipe only `agent-run` holds (`agent-checks --json`), its git calls run with fsmonitor, hooks, external diff, textconv and clean/smudge filters disabled, and a non-0/1 exit, a timeout, a held lock or no result reads "unreadable result". Both `agent-run` and `agent-checks --json` mark themselves undumpable (`prctl(PR_SET_DUMPABLE, 0)`), so a leftover same-uid agent process cannot open their `/proc/<pid>/fd` or ptrace them. Residual, all under one uid: the window between the tool's exec (which resets the flag) and its first prctl call, a few Python start-up milliseconds; a live agent process that was already holding an fd or running as root-equivalent; rewriting the pinned blob's objects in `.git/objects`; swapping binaries in `/workspace/.toolchain`; and declared steps that run the branch's own scripts. The result is evidence; the CI stays the judge, and the reviewer's own run backs it |
| `test_contract.py` → `test-contract` | v3 feature 4: whether a change's tests are a contract. Reads the `tests` section of `.github/agent-checks.yaml` from the base, runs the selected test files at the base, at the base plus the changed tests (the overlay) and at the head, each in a clean worktree, and compares JUnit ids. `red [--base SHA]` is the tester's red check, `hold [--base SHA --contract SHA]` the implementer's contract run (also fails when a contract or runner file changed since the contract commit), `ci --base --head` the "Test contract 🧪" job through `.github/actions/test-contract`. The harness must pass the base and contract shas it recorded at startup; without them `hold` falls back to the local `origin/HEAD` and a trailer search, and says so (`source`). Exit codes: 0 ok, 1 not ok, 2 no declaration, 3 outside the test paths, 4 could not run (or a bad ref), 5 no contract, 6 contract without ids, 7 contract without files, 8 base not under the contract commit, 9 declaration lost after a contract was found; `hold` never exits 2 once a contract exists. CI is the judge; `hold` is a sandbox signal that fails closed |
| `git_credential_agent.py` → `git-credential-agent` | git credential helper; exchanges through identity-proxy `:4001`, caches in memory; `agent-run` calls its `revoke` on exit |
| `gh` | gh with that token in `GH_TOKEN`; `gh pr create` (or `new`) then appends the provenance footer (`pr_footer.py`, SP2 design §5) |
| `commit-msg` | adds `Agent-Run: $RUN_ID`, `Agent-Task: $TASK_ID` when set, and `Agent-Checkpoint: disruption` on agent-run's SIGTERM checkpoint |
| `site/sitecustomize.py` | agent-server's start-up hook: a 4xx to the MCP client's reply to a server `ping` is logged instead of ending the session (F29, envoyproxy/ai-gateway#2715); a transient model error is retried (#99, below); an internal run's prompt-cache markers are rewritten as `prompt_cache_breakpoint`, the only one agentgateway 1.6 carries to Anthropic |

## Optional environment

| Variable | Effect when set | When unset |
|---|---|---|
| `LLM_API_KEY_FILE` | the model API key is read from this file (run-controller only) | the placeholder key; the identity-proxy injects the real one |
| `GIT_TOKEN_FILE` | the git credential helper prints this file's token; it never calls octo-sts and never revokes (run-controller only) | octo-sts exchange through identity-proxy `:4001`, revoked on exit |
| `MCP_URL` | the platform MCP entry and progress notes are wired | no platform tools, no progress notes |
| `AGENT_CHECKS` | `1`: the toolchain installs in the background after the clone, the rules gain the repository-checks rule, and a `finished` implementer run checks its head, notes the result in the room and emits the span `agent-run.checks` | today's run |
| `AGENT_RUN_SECONDS` | the pod's deadline: the final checks are skipped (`skipped: deadline`) when less than the declaration's budget plus 2 min is left | the final checks always run |
| `AGENT_LOOP` | an implementer runs under the [loop policy](#loop-policy): a JSON object holding any of `stallSteps`, `continuations` and `judgeModel`; `{}` is 80, 2 and `tier-light` | no judge and no stall rule: the run ends when its conversation does |

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
| the agent has made no commit by `stallSteps` steps, or two continuations in a row end with no new commit | 1 | `stalled` | which of the two |
| the judge answers `not_yet` and the continuations are spent | 1 | `not_met` | the last reason |

- **The judge** is called when the conversation ends `finished`: one call to the identity proxy's
  `/judge/v1`, beside `LLM_BASE_URL`'s `/v1`, on the run's token, so the gateway meters it against the
  run's cap. The proxy pins the judge's alias from its own `JUDGE_MODEL`; a refusal (403), a proxy
  with no judge path (404) or any other failure leaves the run to end as it would with no judge.
  The judge has no tools and a fresh context. It reads the issue's `## Acceptance check` section
  (else the fenced issue text), the diff against the base (16 KiB at most) and the head's check runs,
  read with the run's own `gh` after waiting up to 5 minutes for them to complete. It answers one
  JSON object; a verdict with no parse or that cites none of the evidence ids it was given is
  discarded: the first counts `not_yet`, the second `judge_impossible`.
- **`not_yet`** sends one message into the same conversation, with the judge's reason quoted as data,
  and the conversation runs again with its history.
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

## Model errors

A model call that fails ends the run (`execution_status=error`, the run's end reason `agent_error`), except
for a transient error, which agent-server retries first (#99, owner decision OD-7):

| Error | Retried | Why |
|---|---|---|
| `503`, or Envoy's `upstream connect error or disconnect/reset before headers` under any status | yes | nothing reached the model, so nothing is spent twice |
| connection refused or reset (litellm reports it as a `500` caused by `APIConnectionError`) | yes | the same |
| `429` | **never** | it can be the gateway's own budget throttle; a retry would hammer the budget |
| a timeout, any other `4xx` or `5xx` | no | a timeout would outlast the bound; the rest is not transient |

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
([#76](https://github.com/Smana/agent-platform/issues/76), [#82](https://github.com/Smana/agent-platform/issues/82)): before a CI-fixed pull request is reviewed again, and before the merge gate arms or shadows any pull request, every
push to its branch since the branch's creation must be the agents App's in GitHub's repository activity. A human
cannot push as the App without access to the agents namespace or the App key. The binding is to the App, not the
task: the implementer octo-sts trust policy (cloud-native-ref `.github/chainguard/agent-implementer.sts.yaml`) is
repository-wide, so any implementer run of any task can push as the bot, and only the trailer narrows that to this
task's runs. Commits are not signed.

The harness never holds a gateway or octo-sts token: identity-proxy injects them. Test with `task harness:test`. `release.yaml` builds, tests, signs and pushes it as
`ghcr.io/smana/agent-harness:<release tag>`; the AgentRun composition in crossplane-configuration
pins that digest. Versions up to `v0.3.0` were released from cloud-native-ref.
