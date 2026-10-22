# OpenHands SDK 1.56.0 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move the agent harness from OpenHands SDK 1.49.6 to 1.56.0 with every monkey-patch re-verified and no behaviour change beyond the bump.

**Architecture:** Three pins, the upstream constraint set and the hashed lock move together, with the image's base digest. The four `sitecustomize.py` patches stay as they are (each target still exists with the same shape). Two tests are added: one that pins the "prompt cache NOT applied" line the `AgentCacheMarkerMissing` alert greps, and one that drives the SDK's own MCP client, because 1.55 changed what a refused tool call costs a run. The bridge changes only in comments and one pinned mapping case.

**Tech Stack:** Python 3.13 (unittest, in the image's test stage), uv for compiling the lock, pip `--require-hashes` for installing it, Go for `internal/bridge`.

**Spec:** v3 review R12 (ai-native-ref `docs/superpowers/specs/2026-10-21-v3-review.md`). A prerequisite of the loop policy's search child conversation (L10, `2026-10-21-loops-and-teams-design.md`) and of the cache design's C5 tool-list freeze (`2026-10-21-prompt-cache-design.md`). The `AgentCacheMarkerMissing` test is the follow-up named in ai-native-ref #95 (cache design, line 79).

## Rulings

| # | Ruling | Why |
|---|---|---|
| R1 | Bump to exactly **1.56.0**, one hop. | The review asks for it; 1.50-1.55 changed nothing the harness patches (table below), so stopping short buys nothing. |
| R2 | **Keep all four patches.** None is dropped, none is adapted. | Measured: each patched method exists with the same signature; F29 and #2283 patch `mcp` (pinned 1.30.0, unchanged), #99 and the cache patch wrap `LLM._transport_call` / `_atransport_call`, unchanged in shape. |
| R3 | Keep `kwargs["max_retries"] = 0` in `patch_model_retry` although 1.56 sets it itself in `_prepare_transport_kwargs`. | Harmless duplicate; removing it is a behaviour edit outside this plan. |
| R4 | **The `litellm<1.95.1` cap stays in this PR.** Lifting it is a separate PR. | One variable at a time. Proven safe to lift: SDK 1.50 fixed #5213 (`hasattr` guard, #5327); the cache and retry suites pass on 1.56.0 + litellm 1.102.0 (Measured). |
| R5 | Regenerate `upstream-constraints.txt` from the **v1.56.0 `uv.lock`**; keep the nine security deviations still above upstream, drop `anyio` and `urllib3` (upstream reached them). | The file's own header rule. |
| R6 | **No bridge code change.** Comments say 1.56.0; one mapping case pins `SecurityAnalysisEvent`. | Event shapes for every kind the bridge maps are unchanged (diffed); the one new kind already falls into the unknown-kind branch. |
| R7 | **Keep the bridge's shell classifier** (`classify.go`). | See "1.55 AST analysis and the classifier" below: different job, different inputs, and the harness runs no SDK analyzer. |
| R8 | The new MCP session test lands **with** the bump and asserts 1.55+ behaviour: an HTTP 403 to a `tools/call` no longer disconnects the run's MCP client. | Measured, reproduced on 1.49.6. It changes the premise of capability-profiles spike S0; this plan changes no policy, it records the fact and pins it. |
| R9 | The new SDK features (child conversations, agent profiles, security analyzer, `persona`) stay **off**: the default agent's tools and `mcp_config` shape are unchanged. | Measured: `get_default_agent(cli_mode=True)` still yields `terminal`, `file_editor`, `task_tracker`, no analyzer. L10 adds the child-conversation tool itself, in its own plan. |

## Global Constraints

- Commits are conventional, in English, with no trailers. Do not push.
- `requirements.txt` is compiled, never edited by hand: `uv pip compile --universal --generate-hashes --python-version 3.13 images/agent-harness/requirements.in -c images/agent-harness/upstream-constraints.txt -o images/agent-harness/requirements.txt`, run from the repository root.
- The image installs with `pip install --require-hashes --only-binary=:all: --no-binary=func-timeout`; every package needs a wheel for both `linux/amd64` and `linux/arm64`.
- The Dockerfile base is `ghcr.io/openhands/agent-server:<version>-python@sha256:<digest>`; Dependabot is told to leave it alone (`.github/dependabot.yml`), so it moves by hand with the pins.
- The `AgentCacheMarkerMissing` rule (ai-native-ref `observability/base/agent-platform/vmrule-logs.yaml`) matches the substring `prompt cache NOT applied`. No edit here may change that text.
- No behaviour change beyond the bump: no new feature switch, no policy change, no change to what the harness sends the gateway.

## Measured

Done in a scratch venv outside the repository, then discarded. Method: `uv venv --python 3.13`; the 1.49.6 venv from the repository's current `requirements.txt`, the 1.56.0 venv from a compile of the new pins (below). Harness suite = `python -m unittest discover -s tests` from `images/agent-harness`, with three local adjustments the image does not need (see "How to run things").

| Check | 1.49.6 | 1.56.0 |
|---|---|---|
| Harness suite, 166 tests | pass | **pass** (the one `setUpClass` error in a first run was `/workspace` missing locally; the 4 `AgentServerTest` tests pass when run alone, serially) |
| `go test ./internal/bridge/...` | pass | pass (no SDK is involved; the fake server is hand-written) |
| Compile with the **old** constraints | n/a | **fails**: `openhands-agent-server==1.56.0 depends on uvicorn>=0.54.0 and uvicorn==0.52.4`. The constraints must move with the pins. |
| Compile with constraints regenerated from the v1.56.0 `uv.lock` | n/a | resolves; 7 packages move: aiosqlite 0.21.0 to 0.22.1, binaryornot 0.4.4 to 0.6.0, posthog 6.7.7 to 7.60.0, pydantic 2.12.5 to 2.13.5, pydantic-core 2.41.5 to 2.46.5, python-frontmatter 1.1.0 to 1.3.0, uvicorn 0.52.4 to 0.54.0; chardet and python-dateutil leave the lock. `mcp` stays 1.30.0, `litellm` 1.93.2, `openai` 2.33.0. |
| `pip install --require-hashes --only-binary=:all: --no-binary=func-timeout` of that lock | n/a | installs |
| `pip download --only-binary=:all:` for `manylinux2014_aarch64` of the moved packages and the three `openhands-*` | n/a | all 10 have wheels |
| The generator reproduces the current file from the v1.49.6 lock | only the 11 known security deviations differ | n/a |
| Base image digest for `1.49.6-python` read from ghcr.io | `02ef66fd...` (matches the Dockerfile) | `1.56.0-python`: `sha256:718bf6d719f34184810ecaebe607f1aafaf526e109654815e61ddd6238728023` |
| Default agent (`get_default_agent(cli_mode=True)`) | `terminal`, `file_editor`, `task_tracker`; no analyzer | identical; one new serialised key, `persona`, which `StartConversationRequest` accepts |
| MCP: first `tools/call` refused with **HTTP 403**, then two more calls through the SDK's `MCPToolExecutor` | calls 2 and 3 fail "MCP client not connected ... Reconnection attempt failed"; the server sees 1 call | **calls 2 and 3 succeed**; the server sees 3 calls |
| MCP: first `tools/call` answered with a JSON-RPC error, HTTP 200 | calls 2 and 3 succeed | calls 2 and 3 succeed |
| Cache wire test (`test_prompt_cache.py`, 9 tests) and `test_model_retry.py` (14) on 1.56.0 + litellm 1.102.0 | n/a | pass |
| Prototype of the "NOT applied" test, mutation: reword the line | n/a | fails 3 sub-cases as it should; passes with the line restored |

### Each monkey-patch against 1.56.0

| Patch | Target | In 1.56.0 | Verdict |
|---|---|---|---|
| F29 MCP ping reply | `mcp.client.streamable_http.StreamableHTTPTransport._handle_post_request` | `mcp` is a pinned dependency (1.30.0) and does not move with the SDK. `test_mcp_ping.test_unpatched_the_session_dies_with_an_empty_error` still passes, i.e. the bug the patch fixes is still there. | Keep, unchanged. |
| #2283 traceparent | `mcp.shared.session.SessionMessage`, `_handle_post_request`, `_prepare_headers`; reads the span made current by `MCPToolExecutor.call_tool`'s `@observe` | Same `mcp`. `MCPToolExecutor.call_tool` still carries `@observe(name="MCPToolExecutor.call_tool", span_type="TOOL")` (`openhands/sdk/mcp/tool.py:117`). `test_mcp_traceparent.py` passes. | Keep, unchanged. |
| #99 model retry | `LLM._transport_call(self, *, messages, enable_streaming, on_token, **kwargs)` and `_atransport_call` | Same names and keyword signature (`llm.py:2521`, `:2547`); called from `_one_attempt` inside `_make_retry_decorator`, which `num_retries=0` still disables. 1.56 now sets `"max_retries": 0` itself in `_prepare_transport_kwargs`, so R3 is a duplicate, not a conflict. `test_model_retry.py`, including the `AgentServerTest` end to end, passes. | Keep, unchanged. |
| Prompt-cache breakpoints | the same two methods; rewrites `cache_control` into `prompt_cache_breakpoint` in `messages` | Same shape; `messages` is still the keyword the call passes. `test_prompt_cache.py`'s `WireTest` sees the same two markers (system prompt, newest tool turn) on 1.56.0. | Keep, unchanged. |

### 1.50-1.56 release notes, what touches the harness

| Release | Item | Effect here |
|---|---|---|
| 1.50 | `tolerate null cache_creation_tokens in usage telemetry` (#5327) | Fixes #5213, the reason for the `litellm<1.95.1` cap. Cap can lift (R4, follow-up). |
| 1.50 | `centralize LLM call context`; `surface LiteLLM budget denials without retry backoff` | Internal to `llm.py`; the patched methods keep their shape. |
| 1.50, 1.51 | `gate prompt_cache_key on provider support` | The harness sets `supports_prompt_cache` through `capability_overrides` and the wire test passes; no change. |
| 1.54 | `emit SecurityAnalysisEvent ... per action batch` | New event kind, emitted only when an analyzer is configured. The harness configures none. The bridge would record it as an unknown kind (pinned in Task 4). |
| 1.54 | `preserve scripts across heredoc boundaries` (terminal) | Not observed by the harness tests; the bridge classifier reads the action text, which is unchanged. |
| 1.55 | `fix(mcp): recover after HTTP session failure` (#4838) | `MCPClient.connect()` resets a finished session task. Measured: a 403 no longer ends the run's MCP tools (R8). |
| 1.55 | `Replace policy rail regexes with shell AST analysis` (#5724) | Lives in `openhands/sdk/security/defense_in_depth`; only runs as a configured analyzer. See below. |
| 1.55 | `429 when run capacity is full` on `POST /run`, `/events`, `/respond_to_confirmation`, `/goal` | `max_concurrent_runs` defaults to 10; a harness pod holds one conversation. The bridge already treats any non-2xx as a `StatusError` (`harness.go`), covered by its `runCode` tests. |
| 1.56 | `add start_child_conversation tool with a pluggable launcher`; `POST /api/conversations/{id}/children` | New tool in `openhands/tools/child_conversation`, **not** in the default agent. L10 enables it. |

### 1.55 AST analysis and the classifier

The SDK's analyzer (`openhands/sdk/security/_shell_ast.py`, tree-sitter-bash, and `defense_in_depth/policy_rails.py`) turns a terminal action into a `SecurityRisk` and a `SecurityAnalysisEvent`. It runs only when `Agent.security_analyzer` is set; the harness never sets it (measured: `None`). The bridge's `classify.go` maps an action to a platform class (`shell.high`, forge writes such as `git push` or `gh pr merge`, egress profiles, the run's own branch) and a verdict, with its own lexer in `shlex.go`. The overlap is the parsing problem, not the answer: the SDK asks "is this dangerous in general", the bridge asks "which forge write is this and is it ours". The bridge reads the SDK's `security_risk` field only as an input it already tolerates being empty. Keep the classifier. Revisit only if a run ever sets an SDK analyzer, in which case the two verdicts would be compared before one is dropped.

## File map

| File | Change |
|---|---|
| `images/agent-harness/tests/test_prompt_cache.py` | Task 1: `NotAppliedTest` |
| `images/agent-harness/requirements.in` | Task 2: three pins |
| `images/agent-harness/upstream-constraints.txt` | Task 2: regenerated body, header, deviations |
| `images/agent-harness/requirements.txt` | Task 2: recompiled |
| `images/agent-harness/Dockerfile` | Task 2: base tag and digest |
| `images/agent-harness/README.md`, `agent_run.py:663`, `tests/test_agent_run.py:1,510` | Task 2: version text in comments |
| `images/agent-harness/tests/test_mcp_session.py` | Task 3: new |
| `internal/bridge/mapping_test.go`, `mapping.go`, `harness.go`, `fakeserver_test.go` | Task 4: comment text and one case |

## How to run things

CI runs the suite in the Dockerfile's `test` stage (`task harness:test`, needs docker). Docker is stopped on the dev machine, so the offline evidence is a venv from the hashed lock. Set `SCRATCH` once to a scratch directory outside the repository.

```bash
cd /home/smana/Sources/agent-platform-sdk-156
uv venv --python 3.13 --seed "$SCRATCH/sdk156/venv"
"$SCRATCH/sdk156/venv/bin/python" -m pip install --no-cache-dir --require-hashes \
  --only-binary=:all: --no-binary=func-timeout -r images/agent-harness/requirements.txt
```

The suite hard-codes three paths of the image. Run it from a copy with them rewritten, and one server at a time (a leaked agent-server on port 8000 makes `AgentServerTest` talk to the wrong process: check `ss -ltn | grep :8000` first).

```bash
rm -rf "$SCRATCH/sdk156/h" && cp -r images/agent-harness "$SCRATCH/sdk156/h"
sed -i "s#/agent-server/.venv/bin/python#$SCRATCH/sdk156/venv/bin/python#" "$SCRATCH/sdk156/h/agent_run.py"
sed -i "s#^REPO_DIR = .*#REPO_DIR = \"$SCRATCH/sdk156/ws\"#" "$SCRATCH/sdk156/h/agent_run.py"
(cd "$SCRATCH/sdk156/h" && "$SCRATCH/sdk156/venv/bin/python" -m unittest discover -s tests)
```

Takes about 110 seconds. `task check` covers the Go side and needs no docker.

---

### Task 1: Pin the "prompt cache NOT applied" message

**Files:**
- Modify: `images/agent-harness/tests/test_prompt_cache.py` (imports at lines 11-18; new class before `if __name__ == "__main__":`)

**Interfaces:**
- Consumes: `load_site()` (already in the file), `site.patch_cache_breakpoints(llm) -> bool`, `site._install() -> None`.
- Produces: `NotAppliedTest.ALERT_GREP = "prompt cache NOT applied"`, the literal `AgentCacheMarkerMissing` matches. Two paths print it: `patch_cache_breakpoints` when `_transport_call` or `_atransport_call` is missing, and `_install` when `openhands.sdk.llm.llm` or `openai` cannot be imported (its line reads `#99 retry and prompt cache NOT applied: ...`).

- [ ] **Step 1: Write the test**

Add `import io` after `import importlib.util` and `from unittest import mock` after `import unittest`, then this class:

```python
class NotAppliedTest(unittest.TestCase):
    """The line AgentCacheMarkerMissing greps (observability/base/agent-platform/vmrule-logs.yaml in
    ai-native-ref): `prompt cache NOT applied`. A reworded line would silence the alert."""

    ALERT_GREP = "prompt cache NOT applied"

    def test_an_sdk_without_the_method_says_so_and_stays_unpatched(self):
        for missing in ("_transport_call", "_atransport_call"):
            with self.subTest(missing=missing):
                present = [n for n in ("_transport_call", "_atransport_call") if n != missing]
                llm = type("LLM", (), {n: (lambda self, **kw: None) for n in present})
                before = {n: getattr(llm, n) for n in present}
                with mock.patch("sys.stderr", new=io.StringIO()) as err:
                    self.assertFalse(load_site().patch_cache_breakpoints(llm))
                self.assertIn(self.ALERT_GREP, err.getvalue())
                self.assertEqual({n: getattr(llm, n) for n in present}, before)

    def test_an_sdk_that_cannot_be_imported_says_so_too(self):
        site = load_site()
        with mock.patch.dict(os.environ, {"PYTHONPATH": ""}), \
                mock.patch.dict(sys.modules, {"openhands.sdk.llm.llm": None}), \
                mock.patch("sys.stderr", new=io.StringIO()) as err:
            site._install()
        self.assertIn(self.ALERT_GREP, err.getvalue())
```

- [ ] **Step 2: Run it against the current code and see it pass**

Run (from the scratch copy, "How to run things"): `python -m unittest discover -s tests -p test_prompt_cache.py -k NotApplied -v`
Expected: 2 tests, OK. The code already prints the line; the test is the missing guard.

- [ ] **Step 3: Prove it guards the line**

In the scratch copy only, run `sed -i 's/prompt cache NOT applied: %s/prompt cache was not applied: %s/' site/sitecustomize.py`, rerun.
Expected: FAIL, both tests (3 failures counting sub-cases). Restore the copy.

- [ ] **Step 4: Run the whole file**

Run: `python -m unittest discover -s tests -p test_prompt_cache.py`
Expected: 11 tests, OK.

- [ ] **Step 5: Commit**

```bash
git add images/agent-harness/tests/test_prompt_cache.py
git commit -m "test(harness): assert the prompt-cache-not-applied line the alert greps"
```

### Task 2: Move the pins, the constraints, the lock and the base image to 1.56.0

**Files:**
- Modify: `images/agent-harness/requirements.in`, `upstream-constraints.txt`, `requirements.txt`, `Dockerfile:5`, `README.md:3`, `agent_run.py:663`, `tests/test_agent_run.py:1` and `:510`

**Interfaces:**
- Consumes: Task 1 (its test must pass again on the new SDK).
- Produces: a lock whose `openhands-*` entries read `1.56.0`; the image tag `1.56.0-python@sha256:718bf6d719f34184810ecaebe607f1aafaf526e109654815e61ddd6238728023`.

- [ ] **Step 1: Pins**

```bash
cd images/agent-harness
sed -i 's/^\(openhands-\(sdk\|tools\|agent-server\)\)==1\.49\.6$/\1==1.56.0/' requirements.in
```

Leave the `litellm<1.95.1` line and its comment alone (R4).

- [ ] **Step 2: Constraints, from the upstream lock**

Fetch the tag's lock and derive the single-version packages (the same rule as the file's header); compare before editing, to see only the expected movers:

```bash
curl -sfL https://raw.githubusercontent.com/OpenHands/software-agent-sdk/v1.56.0/uv.lock -o "$SCRATCH/sdk156/uv156.lock"
python3 - "$SCRATCH/sdk156/uv156.lock" > "$SCRATCH/sdk156/c156.txt" <<'EOF'
import collections, sys, tomllib
v = collections.defaultdict(set)
for p in tomllib.load(open(sys.argv[1], "rb"))["package"]:
    if not p["name"].startswith("openhands-") and "version" in p:
        v[p["name"]].add(p["version"])
for n in sorted(v):
    if len(v[n]) == 1:
        print("%s==%s" % (n, next(iter(v[n]))))
EOF
```

Apply the movers in place (keeps the file's order, so the diff is the change), and the header:

```bash
sed -i -e 's/^aiosqlite==0.21.0/aiosqlite==0.22.1/' -e 's/^binaryornot==0.4.4/binaryornot==0.6.0/' \
  -e '/^chardet==/d' -e 's/^google-cloud-aiplatform==1.157.0/google-cloud-aiplatform==2.1.3/' \
  -e 's/^nodejs-wheel-binaries==22.19.0/nodejs-wheel-binaries==24.19.0/' -e 's/^posthog==6.7.7/posthog==7.60.0/' \
  -e 's/^pydantic==2.12.5/pydantic==2.13.5/' -e 's/^pydantic-core==2.41.5/pydantic-core==2.46.5/' \
  -e 's/^python-frontmatter==1.1.0/python-frontmatter==1.3.0/' -e 's/^uvicorn==0.52.4/uvicorn==0.54.0/' \
  -e 's/v1\.49\.6 release/v1.56.0 release/; s/tag v1\.49\.6/tag v1.56.0/' \
  -e '/^#   anyio 4.14.2/d' -e '/^#   urllib3 2.8.0/d' upstream-constraints.txt
```

- [ ] **Step 3: Check the constraints are exactly the upstream set plus the nine deviations**

```bash
python3 - "$SCRATCH/sdk156/c156.txt" upstream-constraints.txt <<'EOF'
import sys
from packaging.version import Version as V
load = lambda f: dict(l.strip().split("==") for l in open(f) if "==" in l and not l.startswith("#"))
up, ours = load(sys.argv[1]), load(sys.argv[2])
assert set(up) == set(ours), set(up) ^ set(ours)
dev = {n: (up[n], ours[n]) for n in up if up[n] != ours[n]}
assert all(V(o) > V(u) for u, o in dev.values()), dev
print(sorted(dev))
EOF
```

Expected: `['fsspec', 'litellm', 'mako', 'mcp', 'multidict', 'oauthlib', 'pyjwt', 'pypdf', 'soupsieve']`, matching the header's list.

- [ ] **Step 4: Recompile the lock and look at the diff**

From the repository root, run the compile command in Global Constraints. Then `git diff --stat images/agent-harness/requirements.txt` and confirm the moved packages are the seven in Measured and `mcp==1.30.0`, `litellm==1.93.2`, `openai==2.33.0` did not move.

- [ ] **Step 5: Base image and version text**

```bash
sed -i 's#agent-server:1\.49\.6-python@sha256:02ef66fdf0b22a40b0b55c5cca8d1790cfd260c5069f7c0b99edc1b505d5ab37#agent-server:1.56.0-python@sha256:718bf6d719f34184810ecaebe607f1aafaf526e109654815e61ddd6238728023#' Dockerfile
sed -i 's/agent-server:1\.49\.6-python/agent-server:1.56.0-python/' README.md
sed -i 's/agent-server 1\.49\.6/agent-server 1.56.0/' agent_run.py tests/test_agent_run.py
```

Read the digest again from the registry before committing (`curl -sI` on the manifest with a ghcr pull token, `docker-content-digest`); it must equal the one above. `grep -rn "1\.49\.6" .` in `images/agent-harness` must print nothing.

- [ ] **Step 6: Hash-install, arm64 wheels, full suite**

Run "How to run things" with the new lock: expected 168 tests (the previous 166 plus Task 1's 2), all pass. Then for the arm64 check, for each of aiosqlite, binaryornot, posthog, pydantic, pydantic-core, python-frontmatter, uvicorn and the three `openhands-*` at their new versions: `pip download --no-deps --only-binary=:all: --platform manylinux2014_aarch64 --platform manylinux_2_28_aarch64 --python-version 3.13 --implementation cp -d "$SCRATCH/sdk156/dl" <pkg>==<ver>`. Expected: no error, 10 files.

- [ ] **Step 7: Commit**

```bash
git add images/agent-harness
git commit -m "chore(harness): bump the OpenHands SDK to 1.56.0"
```

### Task 3: Pin what a refused MCP tool call costs a run

**Files:**
- Create: `images/agent-harness/tests/test_mcp_session.py`

**Interfaces:**
- Consumes: `openhands.sdk.mcp.utils.create_mcp_tools(cfg, timeout)`, `MCPServer.model_validate`, `MCPToolAction(data={})`, `client.tools[0].executor(action) -> MCPToolObservation` (`is_error`, `text`).
- Produces: `RefusedCallTest`, run by the image's `unittest discover`.

- [ ] **Step 1: Write the test** (the file as run in the prototype)

```python
"""The SDK's own MCP client, as agent-server drives it, after a gateway refuses one tool call.

test_mcp_ping drives fastmcp directly; this drives openhands.sdk's MCPClient and MCPToolExecutor, the
code that decides whether a refused call costs the run its MCP tools. SDK 1.49.6 disconnected for the
rest of the run on an HTTP 403 to a tools/call (capability profiles spike S0); 1.55 reconnects
(software-agent-sdk#4838). The deny design may rely on either only while this test says so.
"""
import http.server
import json
import os
import subprocess
import sys
import threading
import unittest

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)

import agent_run  # noqa: E402


class Gateway(http.server.BaseHTTPRequestHandler):
    """An MCP server whose first tools/call is refused as Gateway.refusal says; later ones succeed."""
    refusal, calls = "http-403", 0

    def answer(self, code, body=b"", headers=()):
        self.send_response(code)
        for key, value in (("Content-Type", "application/json"), *headers):
            self.send_header(key, value)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        msg = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        method = msg.get("method")
        session = [("mcp-session-id", "s1")]
        if method == "tools/call":
            Gateway.calls += 1
            if Gateway.calls == 1 and Gateway.refusal == "http-403":
                return self.answer(403, b'{"error": "denied by policy"}')
            if Gateway.calls == 1:
                body = {"jsonrpc": "2.0", "id": msg["id"], "error": {"code": -32000, "message": "denied"}}
                return self.answer(200, json.dumps(body).encode(), session)
        result = {"initialize": {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}},
                                 "serverInfo": {"name": "gateway", "version": "0"}},
                  "tools/list": {"tools": [{"name": "echo", "description": "d",
                                            "inputSchema": {"type": "object", "properties": {}}}]},
                  "tools/call": {"content": [{"type": "text", "text": "pong"}], "isError": False}}.get(method, {})
        if "id" not in msg:
            return self.answer(202)
        self.answer(200, json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": result}).encode(), session)

    def do_GET(self):
        self.answer(405)

    def do_DELETE(self):
        self.answer(200)

    def log_message(self, *args):
        pass


# Three calls through the SDK's executor; prints one JSON line of each observation's outcome.
CLIENT = r"""
import json, os, sys
os.environ["OPENHANDS_SUPPRESS_BANNER"] = "1"
from openhands.sdk.mcp.config import MCPServer
from openhands.sdk.mcp.definition import MCPToolAction
from openhands.sdk.mcp.utils import create_mcp_tools

client = create_mcp_tools({"platform": MCPServer.model_validate({"url": sys.argv[1], "transport": "http"})}, timeout=15)
executor = client.tools[0].executor
outcomes = []
for _ in range(3):
    obs = executor(MCPToolAction(data={}))
    outcomes.append({"is_error": bool(obs.is_error), "text": obs.text})
print(json.dumps(outcomes), flush=True)
os._exit(0)
"""


class RefusedCallTest(unittest.TestCase):
    def calls_after(self, refusal):
        Gateway.refusal, Gateway.calls = refusal, 0
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Gateway)
        server.daemon_threads = True
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        env = {k: v for k, v in os.environ.items() if k != "PYTHONPATH"}
        done = subprocess.run([sys.executable, "-c", CLIENT, "http://127.0.0.1:%d/mcp" % server.server_port],
                              env=env, cwd="/", capture_output=True, text=True, timeout=90)
        self.assertEqual(done.returncode, 0, done.stderr[-2000:])
        return json.loads(done.stdout.strip().splitlines()[-1])

    def assert_only_the_first_is_refused(self, outcomes):
        self.assertEqual([o["is_error"] for o in outcomes], [True, False, False], outcomes)
        self.assertIn("pong", outcomes[2]["text"])
        self.assertEqual(Gateway.calls, 3, "every call reached the server")

    def test_a_jsonrpc_error_keeps_the_session(self):
        self.assert_only_the_first_is_refused(self.calls_after("jsonrpc"))

    def test_an_http_403_keeps_the_session_from_sdk_1_55(self):
        # 1.49.6: the later calls fail "MCP client not connected ... Reconnection attempt failed".
        self.assert_only_the_first_is_refused(self.calls_after("http-403"))


if __name__ == "__main__":
    unittest.main()
```

`import agent_run` is unused beyond the path insert; keep it only if the file's neighbours do (they do); delete it if the repository's linter objects.

- [ ] **Step 2: Run on 1.56.0**

Run: `python -m unittest discover -s tests -p test_mcp_session.py -v`
Expected: 2 tests, OK (about 7 s).

- [ ] **Step 3: Prove it detects the old behaviour**

Run the same file with the 1.49.6 venv. Expected: `test_an_http_403_keeps_the_session_from_sdk_1_55` fails with `[True, True, True] != [True, False, False]`; the JSON-RPC case passes. Discard the old venv.

- [ ] **Step 4: Commit**

```bash
git add images/agent-harness/tests/test_mcp_session.py
git commit -m "test(harness): pin that a refused MCP tool call keeps the run's session"
```

### Task 4: The bridge against 1.56.0

**Files:**
- Modify: `internal/bridge/harness.go:100`, `mapping.go:131,153`, `fakeserver_test.go:17`, `mapping_test.go` (one case)

**Interfaces:**
- Consumes: `mappingCases` (a slice of `{name, event, types, wantSubstring}`), `Map`.
- Produces: no new symbol.

The check behind this task: the bridge reads `GET /api/conversations/{id}` (`execution_status`), `GET .../events/search` (`items`, `next_page_id`, `limit`, `sort_order`, `page_id`), and POSTs for messages, `/run`, `/interrupt`, `/events/respond_to_confirmation` and the confirmation policy. The OpenAPI of 1.49.6 and 1.56.0 was diffed for these paths and for the schemas of the event kinds: only additions (a documented 429 on the POSTs, `tool_module_qualnames` and `observability_parent_span_context` on conversation models, `SecurityAnalysisEvent`, `/children`, a bash `stop` route). Nothing the bridge reads moved.

- [ ] **Step 1: Add the failing-by-absence case**

In `mapping_test.go`, before the `"a known kind with fields of the wrong type still maps"` case:

```go
	{"a security analysis (agent-server 1.54+, no analyzer in the harness) is recorded as an unknown kind",
		map[string]any{"id": "12b", "kind": "SecurityAnalysisEvent", "source": "agent",
			"analyzer": "defense_in_depth", "risks": map[string]any{"a1": "HIGH"}},
		[]envelope.Type{envelope.StateChanged}, `"harnessKind":"SecurityAnalysisEvent"`},
```

- [ ] **Step 2: Run**

Run: `go test ./internal/bridge/ -run 'TestMapFollowsTheSpecTable|TestMappedItemsAreValidDrafts'`
Expected: PASS (the unknown-kind branch already does this; the case pins it, and the existing check that an unknown event's content never reaches the log covers `risks`).

- [ ] **Step 3: Comment text**

Change "agent-server 1.49.6" to "agent-server 1.56.0" in `harness.go:100`, `mapping.go:131` and `:153` (`1.49.6's Observation`), and `fakeserver_test.go:17`. Comments only.

- [ ] **Step 4: Full Go tests, then the repository's check**

Run: `go test ./internal/bridge/...` then `task check`.
Expected: both pass.

- [ ] **Step 5: Commit**

```bash
git add internal/bridge
git commit -m "test(bridge): pin the SecurityAnalysisEvent mapping, name agent-server 1.56.0"
```

### Task 5: Final gate and the hand-off

- [ ] **Step 1:** `git fetch origin && git rebase origin/main`; if `requirements.txt` conflicts, recompile it, never merge it by hand.
- [ ] **Step 2:** Run the harness suite from the hashed lock ("How to run things") and `task check`. Paste both results into the PR description.
- [ ] **Step 3:** The Docker build is the authority for the base-image digest and the arm64 install; let CI's `harness-test` job run it. Before the first gcp-0 session after merge, run one internal-class AgentRun and confirm in the run log that none of `F29 patch NOT applied`, `#2283 traceparent NOT applied`, `#99 retry NOT applied` or `prompt cache NOT applied` appears, and that the run dashboard's cache-read share is not zero (D4: batched into the next platform session, not a blocker for merge).
- [ ] **Step 4 (ai-native-ref, separate PR):** the prompt-cache design (line 79) says no agent-platform test asserts the message yet; update it to name `test_prompt_cache.py::NotAppliedTest`. The capability-profiles design and its spike S0 should note the 1.55 finding (an HTTP 403 no longer disconnects); whether to use it is that design's decision.

## Not in this plan

- Lifting the `litellm<1.95.1` cap (R4); the Dockerfile's other pins.
- Using child conversations (L10), agent profiles, the SDK security analyzer, or the `persona` field.
- Any bridge behaviour change, including the shell classifier (R7).
- Measuring the prompt-cache prefix across the bump: the tool descriptions and system prompt text may differ in 1.56, so the first internal run after the bump may write the cache once more; the wire test checks markers, not prefix bytes. C5's freeze baseline should be taken on 1.56.0.
