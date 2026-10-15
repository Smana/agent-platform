# agent-harness

The AgentRun sandbox's harness (SP1 design §5): `ghcr.io/openhands/agent-server:1.49.6-python` plus

| File | Role |
|---|---|
| `agent_run.py` → `agent-run` | Entrypoint: start agent-server, clone and resume `$BRANCH`, POST the conversation, wait, revoke, exit 0/1. On SIGTERM, within 15 s: pause, checkpoint an implementer's work (`Agent-Checkpoint: disruption`; never a GitHub token, never over 200 files or 5 MiB), the room-bridge's final read, stop, revoke. A SIGTERM after the run ended keeps its exit code |
| `git_credential_agent.py` → `git-credential-agent` | git credential helper; exchanges through identity-proxy `:4001`, caches in memory; `agent-run` calls its `revoke` on exit |
| `gh` | gh with that token in `GH_TOKEN`; `gh pr create` (or `new`) then appends the provenance footer (`pr_footer.py`, SP2 design §5) |
| `commit-msg` | adds `Agent-Run: $RUN_ID`, `Agent-Task: $TASK_ID` when set, and `Agent-Checkpoint: disruption` on agent-run's SIGTERM checkpoint |
| `site/sitecustomize.py` | agent-server's start-up hook: a 4xx to the MCP client's reply to a server `ping` is logged instead of ending the session (F29, envoyproxy/ai-gateway#2715); a transient model error is retried (#99, below) |

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
