# Repository checks in the sandbox (v3 feature 3) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A public implementer runs its repository's fast checks in its own sandbox before it pushes,
with the tools its repository pins, and the harness runs them again on the final head as the
evidence the judge and the reviewer read. The CI stays the only judge of a merge. Off by default,
per repository and class.

**Architecture:** The harness image carries mise and a new command, `agent-checks`. It reads
`.github/agent-checks.yaml` from the clone's default branch, installs the declared tools at the
work tree's `mise.toml` pins into `/workspace/.toolchain`, and runs each step in its own process
group with the agent's environment minus every credential. `agent-run` starts the install in the
background after the clone, tells the agent the rule, and on a `finished` implementer run checks the
head, writes `/workspace/.agent-checks/<sha>.json`, posts a fixed-text room note and emits a span.
The factory writes `spec.loop.checks` and the repository's egress on a switched-on class's public
implementer and reviewer claims; the composition turns it into `AGENT_CHECKS=1`. Each repository
declares its fast subset in a gate path, and a coverage suite holds the declaration to its CI.

**Tech Stack:** Python 3.13 (the harness venv: PyYAML, `tomllib`), mise 2026.10.3 (aqua and core
backends), Go 1.27.3 (factory, run-controller, controller-gen), KCL 0.11.3 and Crossplane render,
Bash suites, GitHub Actions with gVisor `runsc` (release-20261005.0).

**Spec:** ai-native-ref
[`docs/superpowers/specs/2026-10-22-sandbox-checks-design.md`](https://github.com/Smana/ai-native-ref/blob/main/docs/superpowers/specs/2026-10-22-sandbox-checks-design.md)
(decisions RC1–RC9, approved 2026-10-22 with every recommended default: Q1 mise in the harness, Q2
both hosts, Q3 `ai-overlay.sh --from-git`, Q4 the gate: acceptance not lower and the sandbox agreeing
with the CI on at least 90% of final heads). Read it with this plan.

### This plan's rulings

Points the design left open, or left to the builder.

| # | Ruling | Why |
|---|---|---|
| R1 | **mise 2026.10.3, checksum-pinned per architecture as `gh` is, at `/opt/agent/bin/mise`, not on `PATH`.** A step sees the tools' directories, never mise; a script that calls `mise which` falls back to `PATH` (the host's `validate-vmrules.sh` does) | 17 days old at planning. Off `PATH`, the agent cannot install tools outside the declaration by accident, and no repository config is ever read by mise |
| R2 | **The install names each tool: `mise install <key>@<pin>`, run in `/workspace/.toolchain` with no global or project config, `MISE_USE_VERSIONS_HOST=0`.** The pins come from the work tree's `mise.toml` `[tools]`, read with `tomllib`. Steps get `PATH`, `GOROOT` and `GOBIN` from `mise env --json` over the installed specs ("`mise exec` or `mise env`": `mise env`) | No repository code runs during the install, which may hold a token. Measured: with the versions host on, 58 refused connections and 22 s; off, 8.6 s |
| R3 | **No committed `mise.lock`; the install gets the run's GitHub token, the install alone.** `agent-checks` asks git's credential helper (`git credential fill`, which is `git-credential-agent` in a run) and passes the token to `mise install` as `GITHUB_TOKEN`; a step never sees it | Measured: an install makes 10 GitHub API calls; a lockfile with `MISE_LOCKED=1` makes 0. Ten authenticated calls against 5,000 an hour cost nothing, while a lockfile is a new file in two repositories to keep in step with every Renovate bump. The lockfile stays the fallback |
| R4 | **The image adds what both CIs install outside mise: PyYAML in the system `python3` (the venv's hash-pinned 6.0.2) and `rsync` (from the base image's Debian snapshot).** "Whether the image needs rsync": yes | Measured: the base image's system Python 3.13 has no PyYAML and the image has no `rsync`. Both overlays call `rsync`; nearly every suite imports `yaml`; the CIs `pip install pyyaml` and run on a runner that has `rsync` |
| R5 | **The declaration comes from `refs/remotes/origin/HEAD`, the clone's default branch, never `BASE_REF`** | A reviewer's `BASE_REF` is the pull request's branch (`verifierSpec`) |
| R6 | **Caches live under the toolchain: `XDG_CACHE_HOME` and `GOPATH` in `/workspace/.toolchain`** | pre-commit's environments, the Go module cache and Hugo's cache would otherwise fill `HOME`'s 2 Gi; a suspendable run's PVC keeps them |
| R7 | **A step's environment is the agent's minus `CI`, `GITHUB_ACTIONS`, `GH_TOKEN`, `GITHUB_TOKEN`, `GIT_TOKEN_FILE`, `GIT_TOKEN_CACHE`, `LLM_API_KEY_FILE`, `LLM_API_KEY`, `OH_SECRET_KEY`, `SESSION_API_KEY`, `STS_URL`, `OH_SESSION_API_KEYS_*` and `MISE_*`, with `AI_AGENT=openhands`** | SDK 1.49.6's terminal strips only `SESSION_API_KEY`, `OH_SECRET_KEY` and the session key slots, so the run-controller's `LLM_API_KEY_FILE` and `GIT_TOKEN_FILE` reach the agent's shell. `AI_AGENT` is what the terminal sets |
| R8 | **The budget counts from the first step; the install has its own 5 min.** A step not started for lack of budget reads `skip` ("the budget is spent"). When the install leaves a tool missing, every step still runs and a failed one reads "toolchain incomplete" | The declaration maps no step to its tools, and a step may not need the missing one |
| R9 | **The result:** `<head>.json` holds `version, head, dirty, base, declaration, tools, missingTools, steps[{name, ci, result, seconds, exit, reason?}], partial, ciOnly, changedByChecks, definitionTouched, seconds, log`; the summary is also `<head>.txt`, the file the loop policy's judge (L2) reads; every run appends a line to `runs.jsonl`. `--step` writes `<head>.step-<name>.*`, which never stands for the head | "The JSON's exact fields." Measured: pre-commit's fixers change tracked files, so the summary names what the checks changed |
| R10 | **The rule is a "Repository checks" section `agent-run` appends to `rules.md`, when the default branch declares checks.** Implementer: RC4's text verbatim. Reviewer: "Before your verdict, run `agent-checks` on the pull request's head and cite its first line as your evidence. It runs the CI's fast subset; the CI stays the judge." The prefetch runs for both roles | The design gave the implementer's text only |
| R11 | **The final run is the implementer's only.** A full result for the head, from a clean tree and not `dirty`, stands, as RC6 says; anything else runs again. The deadline is `AGENT_RUN_SECONDS`, the pod's `activeDeadlineSeconds`, which the composition and the run-controller set with the checks; `agent-run` counts it from its own start, the init containers' seconds falling inside RC9's 2 min. Unknown, the run goes ahead | The sandbox is the agent's to change, so the result is evidence, never a gate: the reviewer runs its own (RC6) and the CI judges |
| R12 | **The room note:** "Harness (implementer): checks on `<sha12>`: 6 pass, 1 fail. Failed: pre-commit." (or "… skipped: deadline."), at most 280 characters, the failed names cut with "…". Outside the 8-note cap; on the broker's one-a-minute limit it waits out the minute once | "The room note's wording." Step names come from the base's declaration |
| R13 | **Spans: `agent-run.checks.install` (the prefetch) and `agent-run.checks` (the final run), with `error.type` `install_incomplete`, `checks_failed`, `checks_deadline` or `checks_no_result`.** The agent's own runs are one log line from the ledger, not spans | The trace collector keeps `error.type` and drops every other attribute; a span's duration is the seconds the live check reports |
| R14 | **No factory brief change.** The reviewer gets its rule from the harness and the implementer's note through `room_read`; the tester stays off until feature 4 | RC6's "the reviewer's brief carries the note": the reviewer's task is the pull request URL, and its rules already say "Call room_read first" |
| R15 | **The coverage suites are `scripts/ci/tests/test-agent-checks.py`, one file, byte for byte, in both repositories.** A job is named by its check name (`name`, else its id; a static matrix adds `name (v, …)`) or `<workflow file stem>/<job id>`. A name that matches no `pull_request` job fails. A push-only job of a `pull_request` workflow is listed too | "The coverage suites' names." Emoji names and the host's two `build` jobs need both forms |
| R16 | **The host's shellcheck job keeps its inline command and installs ShellCheck 0.11.0 through mise; `task ci:shellcheck` is added for people and to `task check`; the declaration repeats the inline command, `-type l` included** | The design said "the CI job calling" the task, but the host's `ci.yaml` calls its gate scripts directly because a taskfile is not a gate path: a task would let an agent's pull request empty a required check |
| R17 | **The host's declaration follows the workflows as they are wired:** `idp-topology` and `openbao-policies` mirror the links job, `doc-paths` mirrors "Build the documentation site 📚", whose Hugo build stays CI-only; `test-render-bundle.py` gains `# requires: helm` | Measured on the host: the design's table placed three steps under the wrong job; and `task ci:test` failed only on that suite, which needs `helm` without saying so (`helm` is on `get.helm.sh`, outside every profile) |
| R18 | **`--from-git` takes a release tag only; `ARTIFACT_DIRS` mirrors `push-artifact.sh`, and a host suite proves the trees equal** | A pre-release pin is an OCI tag with no git tag. Measured equal on v0.1.16 |
| R19 | **`spec.loop` is an object; `loop.checks` a boolean. Both runtimes carry the same two CEL rules: an internal run with checks is refused; `loop` is immutable, presence and value.** `AGENT_CHECKS` and `AGENT_RUN_SECONDS` are set only with the checks | The native CRD keeps the XRD's contract (3a R2). Every other pod stays byte-identical |
| R20 | **`rules.md` (the composition's) is unchanged: `.github/agent-checks.yaml` does not join rule 5** | It is read from the default branch, and `.github/` is a gate path |
| R21 | **Offline proof 4 is a non-required job, `harness-checks`, in agent-platform's CI.** It skips a repository whose `main` declares nothing, so PR 1 merges before the declarations; PR 4's CI is its first full run | A required check cannot depend on two other repositories' `main` |
| R22 | **The gate, as approved (Q4):** acceptance not lower and agreement at least 90% of final heads. With the bench deferred, it is read in the gcp-0 session on real issues: agreement per head from the room note (the failed steps) and the base declaration's `ci` map against `gh pr checks` on that head, each disagreement explained | The roadmap's 2026-10-22 order of work |

**Measured 2026-10-22** on prototypes in `$SCRATCH/checks/` (the code below is theirs; all discarded):

- **The install** (mise 2026.10.3, no global config, `MISE_USE_VERSIONS_HOST=0`, through a CONNECT
  proxy allowing exactly the hosts below): ai-native-ref's six tools cold in 8.6 s on a desktop link,
  657 MB on disk; connections only to `api.github.com`, `github.com`,
  `release-assets.githubusercontent.com`, `dl.google.com`. With the versions host on: 22.1 s and 58
  refused connections to `mise-versions.jdx.dev`.
- **API calls:** 10 `GET`s to `api.github.com` per install (a release list and a tag per aqua tool).
  With a `mise.lock` and `MISE_LOCKED=1`: 0, and only `github.com`, `release-assets…` and
  `dl.google.com`.
- **Today's profiles:** `task` is refused at `release-assets.githubusercontent.com`, `go` at
  `dl.google.com`. With both hosts, everything installs.
- **ai-native-ref's seven steps** (`github` + `release-assets` + `golang` + `dl.google.com` + `pypi`,
  `CI=true` given and scrubbed): 7 pass, the two Docker suites skip; 45.5 s cold (pre-commit's
  environments 18 s), 21.4 s warm. Hugo fetches its modules from `github.com` (its
  `module.proxy` is `direct`): `proxy.golang.org` went unused. A broken relref and a trailing space
  failed `site` and `pre-commit`; the summary was 4,372 characters; pre-commit's fixer changed a
  tracked file, and `definitionTouched` named `taskfile.yaml`.
- **The host's eleven steps** at `HOST_REF` with the draft declaration and `--from-git`, through
  `github` + `release-assets` + `pypi` only: 10 pass in 90.6 s cold (suites 52.6 s, pre-commit
  25.4 s, ShellCheck 10.0 s); `suites` failed on `flux-schema/test-render-bundle`, "helm not found"
  (R17). No refused host.
- **`--from-git`** on v0.1.16 lays the verified artifact's 328 files byte for byte; the host suite
  below passed under `CI=true` (refusal, pre-release refusal, identical trees).
- **The coverage suite**: PASS on ai-native-ref `main` with the design's declaration (7 steps, 4
  CI-only entries, 3 jobs) and on the host at `HOST_REF` with the draft below (11, 13, 14 jobs).
  Dropping `prerelease`, adding a job, misnaming a `ci` or declaring an unpinned tool each fails with
  the name.
- **crossplane-configuration's `task test`**: the two hosts turned two assertions red, then 109/109
  with a new test; with `loop.checks`, 110/110 and both reject settings refused.
- **The harness:** 18 `agent-checks` tests and 15 `agent-run` tests pass in a venv built from
  `requirements.txt` (Python 3.13.16, SDK 1.49.6); the 93 existing `test_agent_run.py` tests still
  pass. The factory and run-controller changes pass their packages' tests and `hack/lint.sh` on
  `origin/main` at `0717cd6` (Go 1.27.3, #168).
- **The SDK (1.49.6):** the terminal pauses after 30 s without new output; an observation holds at
  most 30,000 characters.
- **Not run here:** the container suite under `runsc` (Docker is stopped on this machine). It runs
  in CI.

## Global Constraints

- **Branches and worktrees.** Each repository's work in its own worktree from a freshly fetched
  `origin/main`; rebase before every push. agent-platform:
  `git -C /home/smana/Sources/agent-platform worktree add /home/smana/Sources/agent-platform-<topic> -b <branch> origin/main`.
- **`$SCRATCH`** is a scratch directory outside every repository.
- **Evidence:** agent-platform `task check` (and `task harness:test` for the image); cloud-native-ref
  `task check` plus `task ci:test`; ai-native-ref `task check`; crossplane-configuration `task check`.
  Never claim done without the output.
- **A host change is a cloud-native-ref pull request first**, then a `HOST_REF` bump in ai-native-ref.
- **Commits:** conventional, English, no trailers. `git add` named paths only.
- **Off by default (D4).** No class is switched on by any pull request here.
- **Verbatim from the spec:**
  - RC4's rule: "Before every push, run `agent-checks` and fix what it reports. It runs the CI's fast
    subset; the CI stays the judge. If a failure is not your change's, say so in the pull request."
  - RC4: "prints a line at least every 20 s, then a summary of at most 8,000 characters … the last 60
    lines of each failed step … Exit 0 when nothing failed, 1 otherwise, 2 when the repository
    declares nothing".
  - RC7: "The `github` profile gains `release-assets.githubusercontent.com`; `golang` gains
    `dl.google.com`. ai-native-ref requests `[golang, pypi]`, cloud-native-ref `[pypi]`".
  - RC9: "the install 5 min … the whole declaration at most 15 min (default 8) … The final run is
    skipped, and recorded `skipped: deadline`, when the pod has less than the budget plus 2 min left.
    `size` stays `small`".
  - RC8: "`repositories[].classes.<class>.checks: true`".

## The pull requests, in order

| # | Repository | What | Depends on | Offline proof |
|---|---|---|---|---|
| 1 | agent-platform | The harness: `agent-checks`, mise, PyYAML and `rsync` in the image, the prefetch, the rule, the milestone, the final run, its note and spans; the run-controller's two hosts and the portable `AgentRun`'s `loop.checks`; the `harness-checks` job | nothing | 3, 4 (skips), 5 |
| 2 | cloud-native-ref | `.github/agent-checks.yaml`, `ai-overlay.sh --from-git` and its suite, ShellCheck pinned in `mise.toml`, the coverage suite, the sentinel, `# requires: helm` | nothing | 1, 2 |
| 3 | ai-native-ref | `.github/agent-checks.yaml`, the coverage suite, the sentinel | nothing | 1 |
| 4 | agent-platform | The factory: `checks.egress`, `classes.<class>.checks`, the claim's `spec.loop.checks` and egress; docs. Then a release carrying PRs 1 and 4 | PR 1; PRs 2 and 3 merged for proof 4's full run | 4 (full), 7 |
| 5 | crossplane-configuration | The two hosts; XRD `spec.loop.checks` (refused on `internal`, immutable) to `AGENT_CHECKS` and `AGENT_RUN_SECONDS`; the harness digest of PR 4's release. Then a release | PR 4's release | 6 |
| 6 | cloud-native-ref | Pin PR 5's release | PR 5 | the host's CI |
| 7 | ai-native-ref | `HOST_REF`; the factory image of PR 4's release; `checks.egress` per repository with every class off; `harness-reference.md` and the status page | PRs 4 and 6 | `task check` |

Live proofs, one gcp-0 session after PR 7: [the gate](#the-gate).

---

## PR 1 — agent-platform: the harness

Branch `feat/sandbox-checks` (this worktree). This plan is its first commit.

| File | Change | Responsibility |
|---|---|---|
| `images/agent-harness/agent_checks.py` | create | The `agent-checks` command |
| `images/agent-harness/tests/test_agent_checks.py` | create | Offline proof 3 |
| `images/agent-harness/agent_run.py` | modify | Rule, milestone, prefetch, final run, note, spans |
| `images/agent-harness/tests/test_agent_run_checks.py` | create | Offline proof 5 |
| `images/agent-harness/Dockerfile`, `checks-requirements.txt` | modify, create | mise, PyYAML, `rsync`, `agent-checks` on `PATH` |
| `images/agent-harness/tests/test_image_tools.py` | create | The image carries what R1 and R4 say |
| `images/agent-harness/README.md` | modify | The new file and variables |
| `api/v1alpha1/agentrun_types.go` (+ test, deepcopy, CRD) | modify | `spec.loop.checks` |
| `internal/runctl/config.go`, `pod.go`, `checks_test.go` (+ goldens) | modify, create | The two hosts; `AGENT_CHECKS`, `AGENT_RUN_SECONDS` |
| `images/agent-harness/tests/checks/run.sh`, `proxy.py` | create | Offline proof 4 |
| `.github/workflows/ci.yaml`, `taskfile.yaml` | modify | `harness-checks`, `task harness:checks` |

### Task 1: `agent-checks` and its unit tests

**Files:** Create `images/agent-harness/agent_checks.py`, `images/agent-harness/tests/test_agent_checks.py`.

**Interfaces:**
- Produces (Task 3 relies on these): module constants `STATE`, `INSTALL_S`, `LEDGER`; exception
  `Undeclared`; `declaration() -> (base_sha, blob_sha, decl)` where `decl` is
  `{"minutes": int, "tools": [str], "definedBy": [str], "steps": [dict], "ciOnly": [dict]}`;
  `main(argv) -> int`. CLI: `agent-checks [--step NAME | --install]`, exit 0, 1 or 2. Files under
  `/workspace/.agent-checks/`: `<head>.json`, `<head>.txt`, `<head>.log`, `runs.jsonl`,
  `install.log`, `.lock`.

- [ ] **Step 1: Write the failing test** `images/agent-harness/tests/test_agent_checks.py`:

```python
"""agent-checks (v3 feature 3, offline proof 3): the declaration from the default branch, the bounds,
the summary, the step's environment, the exit codes.

Each case builds a repository with an origin whose main declares the checks, clones it, and runs
agent_checks.main() in-process against a fake mise that installs a tool by making its directory.
"""
import contextlib
import io
import json
import os
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)
import agent_checks  # noqa: E402

FAKE_MISE = r'''#!{python}
"""A stand-in for mise: `where`, `install`, `env --json`, on $MISE_DATA_DIR/installs/<tool>/<version>.
mise gets only the environment agent-checks gives it, so its knobs sit beside it in fake-mise.json."""
import json, os, sys, time
here = os.path.dirname(os.path.abspath(sys.argv[0]))
knobs = json.load(open(os.path.join(here, "fake-mise.json"))) if os.path.exists(os.path.join(here, "fake-mise.json")) else {}
data = os.path.join(os.environ["MISE_DATA_DIR"], "installs")
def where(spec):
    t, v = spec.split("@", 1)
    return os.path.join(data, t, v)
cmd, specs = sys.argv[1], [a for a in sys.argv[2:] if not a.startswith("-")]
if cmd == "where":
    sys.exit(0 if os.path.isdir(where(specs[0])) else 1)
if cmd == "install":
    with open(os.path.join(here, "mise.log"), "a") as log:
        log.write(json.dumps({"specs": specs, "token": os.environ.get("GITHUB_TOKEN", "")}) + "\n")
    time.sleep(knobs.get("sleep", 0))
    for s in specs:
        if s.split("@")[0] == knobs.get("fail"):
            continue
        b = os.path.join(where(s), "bin")
        os.makedirs(b, exist_ok=True)
        tool = os.path.join(b, s.split("@")[0])
        with open(tool, "w") as f:
            f.write("#!/bin/sh\necho %s\n" % s)
        os.chmod(tool, 0o755)
    sys.exit(0)
if cmd == "env":
    bins = [os.path.join(where(s), "bin") for s in specs]
    print(json.dumps({"PATH": ":".join(bins + [os.environ["PATH"]])}))
    sys.exit(0)
sys.exit(3)
'''

DECLARED = """version: 1
timeoutMinutes: 2
tools: [task, go]
definedBy: [taskfile.yaml, scripts/ci/, mise.toml]
steps:
  - {name: first, run: "task > first.out; go >> first.out", ci: validate, stopOnFailure: true}
  - {name: second, run: "echo second", ci: lint}
ciOnly:
  - {check: prerelease, part: all, why: "signs with the workflow's identity"}
"""


def sh(cwd, *args, **kw):
    return subprocess.run(args, cwd=cwd, check=True, capture_output=True, text=True, **kw).stdout.strip()


class Rig(unittest.TestCase):
    """A clone of an origin whose main declares DECLARED, on the branch agent/x."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        seed, self.origin, self.repo = (os.path.join(self.tmp, n) for n in ("seed", "origin.git", "repo"))
        git = ["git", "-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"]
        os.makedirs(os.path.join(seed, ".github"))
        sh(self.tmp, *git, "init", "-q", seed)
        self.write(seed, ".github/agent-checks.yaml", DECLARED)
        self.write(seed, "mise.toml", '[tools]\ntask = "3.54.0"\ngo = {version = "1.27.2"}\n')
        self.write(seed, "taskfile.yaml", "version: '3'\n")
        sh(seed, *git, "add", "-A")
        sh(seed, *git, "commit", "-qm", "seed")
        sh(self.tmp, "git", "clone", "-q", "--bare", seed, self.origin)
        sh(self.tmp, "git", "clone", "-q", self.origin, self.repo)
        sh(self.repo, "git", "checkout", "-qb", "agent/x")
        self.git = git
        mise = os.path.join(self.tmp, "mise")
        self.write(self.tmp, "mise", FAKE_MISE.replace("{python}", sys.executable, 1))
        os.chmod(mise, 0o755)
        self.mise_log = os.path.join(self.tmp, "mise.log")
        # A credential helper that answers with a token, as git-credential-agent does in a run.
        self.write(self.tmp, "gitconfig", '[credential]\n\thelper = "!f() { echo username=x-access-token; echo password=ghs_installonly; }; f"\n')
        self.env = {"PATH": os.environ["PATH"], "HOME": self.tmp,
                    "GIT_CONFIG_GLOBAL": os.path.join(self.tmp, "gitconfig"), "GIT_CONFIG_NOSYSTEM": "1",
                    "CI": "true", "GITHUB_ACTIONS": "true", "GH_TOKEN": "ghs_x", "GIT_TOKEN_FILE": "/run/agent/github/token",
                    "LLM_API_KEY_FILE": "/run/agent/model/api_key", "OH_SECRET_KEY": "k", "MISE_DATA_DIR": "/elsewhere"}
        for name, value in {"REPO": self.repo, "STATE": os.path.join(self.tmp, "state"),
                            "TOOLCHAIN": os.path.join(self.tmp, "toolchain"), "MISE": mise,
                            "MINUTE": 1, "HEARTBEAT_S": 0.2, "KILL_GRACE_S": 1, "INSTALL_S": 5}.items():
            patcher = mock.patch.object(agent_checks, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)

    @staticmethod
    def write(root, rel, text):
        with open(os.path.join(root, rel), "w") as f:
            f.write(text)

    def commit(self, rel, text):
        self.write(self.repo, rel, text)
        sh(self.repo, *self.git, "commit", "-qam", "change " + rel)

    def base_declares(self, decl):
        """main declares decl: the default branch moves, and the agent's branch with it."""
        self.write(self.repo, ".github/agent-checks.yaml", decl)
        sh(self.repo, *self.git, "commit", "-qam", "declare")
        sh(self.repo, "git", "update-ref", "refs/remotes/origin/main", sh(self.repo, "git", "rev-parse", "HEAD"))

    def run_checks(self, *argv, env=None):
        """(exit code, stdout lines with their arrival times)."""
        lines = []

        class Clock(io.TextIOBase):
            def write(self, s):
                for line in s.splitlines():
                    lines.append((time.monotonic(), line))
                return len(s)
        with mock.patch.dict(os.environ, {**self.env, **(env or {})}, clear=True), contextlib.redirect_stdout(Clock()):
            code = agent_checks.main(list(argv))
        return code, lines

    def result(self, stem=None):
        head = stem or sh(self.repo, "git", "rev-parse", "HEAD")
        with open(os.path.join(agent_checks.STATE, head + ".json")) as f:
            return json.load(f)

    def text(self, lines):
        return "\n".join(line for _, line in lines)


class DeclarationTest(Rig):
    def test_the_default_branch_declares_and_a_branch_edit_changes_nothing(self):
        self.commit(".github/agent-checks.yaml", DECLARED.replace('echo second', 'exit 1'))
        code, _ = self.run_checks()
        res = self.result()
        self.assertEqual(code, 0)
        self.assertEqual([s["result"] for s in res["steps"]], ["pass", "pass"])
        self.assertEqual(res["declaration"], sh(self.repo, "git", "rev-parse", "origin/main:.github/agent-checks.yaml"))

    def test_nothing_declared_exits_2(self):
        sh(self.repo, "git", "update-ref", "refs/remotes/origin/main", sh(self.repo, "git", "rev-parse", "HEAD"))
        os.remove(os.path.join(self.repo, ".github/agent-checks.yaml"))
        sh(self.repo, *self.git, "commit", "-qam", "drop")
        sh(self.repo, "git", "update-ref", "refs/remotes/origin/main", sh(self.repo, "git", "rev-parse", "HEAD"))
        code, lines = self.run_checks()
        self.assertEqual(code, 2)
        self.assertIn("declares no checks", self.text(lines))

    def test_a_malformed_declaration_exits_2_with_its_reason(self):
        for bad, why in ((DECLARED.replace("version: 1", "version: 2"), "version must be 1"),
                         (DECLARED.replace("name: second", "name: First"), "step names"),
                         (DECLARED.replace("timeoutMinutes: 2", "timeoutMinutes: 16"), "timeoutMinutes must be 1 to 15")):
            with self.subTest(why=why):
                with self.assertRaisesRegex(agent_checks.Undeclared, why):
                    agent_checks.validate(__import__("yaml").safe_load(bad))

    def test_an_unknown_step_exits_2(self):
        code, lines = self.run_checks("--step", "third")
        self.assertEqual(code, 2)
        self.assertIn("declared: first, second", self.text(lines))


class ToolchainTest(Rig):
    def test_the_branchs_pins_are_installed_with_the_token_and_found_first_on_path(self):
        self.commit("mise.toml", '[tools]\ntask = "3.55.0"\ngo = {version = "1.27.2"}\n')
        code, _ = self.run_checks()
        self.assertEqual(code, 0)
        with open(self.mise_log) as f:
            installs = [json.loads(line) for line in f]
        self.assertEqual(installs, [{"specs": ["task@3.55.0", "go@1.27.2"], "token": "ghs_installonly"}])
        with open(os.path.join(self.repo, "first.out")) as f:
            self.assertEqual(f.read(), "task@3.55.0\ngo@1.27.2\n")

    def test_a_second_run_installs_nothing(self):
        self.run_checks()
        self.run_checks()
        with open(self.mise_log) as f:
            self.assertEqual(len(f.readlines()), 1)

    def test_an_install_past_its_bound_is_stopped_and_the_steps_say_why(self):
        self.write(self.tmp, "fake-mise.json", '{"sleep": 30}')
        code, lines = self.run_checks()
        res = self.result()
        self.assertEqual(code, 1)
        self.assertEqual(res["missingTools"], ["task", "go"])
        self.assertEqual(res["steps"][0]["reason"], "toolchain incomplete")
        self.assertIn("toolchain incomplete: task, go", self.text(lines))

    def test_a_tool_mise_toml_does_not_pin_is_missing(self):
        self.commit("mise.toml", '[tools]\ntask = "3.54.0"\n')
        self.run_checks()
        self.assertEqual(self.result()["missingTools"], ["go"])

    def test_install_only(self):
        code, lines = self.run_checks("--install")
        self.assertEqual(code, 0)
        self.assertIn("agent-checks: toolchain ready", self.text(lines))
        self.assertFalse(os.path.exists(os.path.join(self.repo, "first.out")))


class StepTest(Rig):
    def test_the_step_environment_holds_no_ci_no_key_and_no_token_path(self):
        self.base_declares(DECLARED.replace("echo second", "env > env.out"))
        self.run_checks()
        with open(os.path.join(self.repo, "env.out")) as f:
            names = {line.split("=", 1)[0] for line in f if "=" in line}
        for name in ("CI", "GITHUB_ACTIONS", "GH_TOKEN", "GITHUB_TOKEN", "GIT_TOKEN_FILE", "LLM_API_KEY_FILE",
                     "OH_SECRET_KEY", "MISE_DATA_DIR"):
            self.assertNotIn(name, names)
        self.assertIn("XDG_CACHE_HOME", names)

    def test_a_step_past_its_timeout_is_killed_with_its_children(self):
        self.base_declares(DECLARED.replace('{name: second, run: "echo second", ci: lint}',
                                            '{name: second, run: "sleep 60 & echo $! > child.pid; wait", ci: lint, timeoutMinutes: 1}'))
        code, _ = self.run_checks()
        self.assertEqual(code, 1)
        self.assertEqual(self.result()["steps"][1]["result"], "timeout")
        with open(os.path.join(self.repo, "child.pid")) as f:
            pid = int(f.read())
        time.sleep(0.2)
        with self.assertRaises(ProcessLookupError):
            os.kill(pid, 0)

    def test_stop_on_failure_skips_the_rest(self):
        self.base_declares(DECLARED.replace("task > first.out; go >> first.out", "exit 3"))
        code, lines = self.run_checks()
        res = self.result()
        self.assertEqual(code, 1)
        self.assertEqual([(s["result"], s.get("reason")) for s in res["steps"]],
                         [("fail", None), ("skip", "an earlier step stopped the run")])
        self.assertEqual(res["steps"][0]["exit"], 3)

    def test_a_line_at_least_every_heartbeat(self):
        self.base_declares(DECLARED.replace('echo second', 'sleep 1.5'))
        _, lines = self.run_checks()
        gaps = [b[0] - a[0] for a, b in zip(lines, lines[1:])]
        self.assertLess(max(gaps), agent_checks.HEARTBEAT_S + 0.5)
        self.assertIn("agent-checks: second running", self.text(lines))

    def test_one_step(self):
        code, _ = self.run_checks("--step", "second")
        head = sh(self.repo, "git", "rev-parse", "HEAD")
        self.assertEqual(code, 0)
        self.assertTrue(self.result(head + ".step-second")["partial"])
        self.assertFalse(os.path.exists(os.path.join(agent_checks.STATE, head + ".json")))



class SummaryTest(Rig):
    def test_the_summary_is_capped_and_keeps_its_frame(self):
        noisy = "for i in $(seq 1 200); do printf 'line %d %0400d\\n' $i 0; done; exit 1"
        decl = DECLARED.replace('echo second', noisy).replace("ci: lint}", "ci: lint}\n  - {name: third, run: \"%s\", ci: lint}" % noisy)
        self.base_declares(decl)
        code, lines = self.run_checks()
        head = sh(self.repo, "git", "rev-parse", "HEAD")
        with open(os.path.join(agent_checks.STATE, head + ".txt")) as f:
            text = f.read().rstrip("\n")
        self.assertEqual(code, 1)
        self.assertLessEqual(len(text), agent_checks.SUMMARY_CAP)
        self.assertIn("2 fail", text.splitlines()[0])
        self.assertIn("--- second, last lines ---", text)
        self.assertIn("line 200 ", text)
        self.assertIn("  prerelease: all (signs with the workflow's identity)", text)
        self.assertTrue(text.endswith("a green result here is not the CI's."))

    def test_definition_touched_lists_the_changed_definition_paths(self):
        self.commit("taskfile.yaml", "version: '3'\n# changed\n")
        self.write(self.repo, "mise.toml", '[tools]\ntask = "3.54.0"\ngo = "1.27.2"\n')
        code, lines = self.run_checks()
        res = self.result()
        self.assertEqual(res["definitionTouched"], ["mise.toml", "taskfile.yaml"])
        self.assertTrue(res["dirty"])
        self.assertIn("unverified until the CI runs", self.text(lines))

    def test_files_the_checks_change_are_named(self):
        self.base_declares(DECLARED.replace('echo second', 'echo fixed >> taskfile.yaml'))
        self.run_checks()
        self.assertEqual(self.result()["changedByChecks"], ["taskfile.yaml"])

    def test_each_run_is_one_ledger_line(self):
        self.run_checks()
        self.run_checks("--step", "second")
        with open(os.path.join(agent_checks.STATE, agent_checks.LEDGER)) as f:
            rows = [json.loads(line) for line in f]
        self.assertEqual([(r["partial"], r["counts"]) for r in rows], [(False, {"pass": 2}), (True, {"pass": 1})])


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Run it to see it fail**

Run: `docker build --target test images/agent-harness` (or, with a local venv built from
`requirements.txt`: `cd images/agent-harness && python -m unittest tests.test_agent_checks`)
Expected: `ModuleNotFoundError: No module named 'agent_checks'`.

- [ ] **Step 3: Write `images/agent-harness/agent_checks.py`**

```python
#!/agent-server/.venv/bin/python
"""agent-checks: the repository's fast checks, the CI's fast subset, run in the sandbox (v3 feature 3).

The repository declares them in .github/agent-checks.yaml, read from the clone's default branch
(refs/remotes/origin/HEAD), never from the agent's branch (RC2). The tools it lists are installed by
mise at the versions the work tree's mise.toml pins, into /workspace/.toolchain (RC1). Each step is a
`bash -c` line in /workspace/repo, in its own process group, with the agent's environment minus every
credential path (RC5). The result is evidence for the agent, the judge and the reviewer, never a
gate: the sandbox is the agent's to change, and the CI stays the only judge of a merge.

  agent-checks                every step; exit 0 when none failed, 1 otherwise, 2 when nothing is declared
  agent-checks --step NAME    one step; its result never stands for the head's
  agent-checks --install      the toolchain only: agent-run's background prefetch
"""
import argparse
import collections
import fcntl
import json
import os
import re
import signal
import subprocess
import sys
import threading
import time
import tomllib

import yaml

REPO = os.environ.get("AGENT_CHECKS_REPO", "/workspace/repo")
STATE = os.environ.get("AGENT_CHECKS_DIR", "/workspace/.agent-checks")
TOOLCHAIN = os.environ.get("AGENT_CHECKS_TOOLCHAIN", "/workspace/.toolchain")
MISE = os.environ.get("AGENT_CHECKS_MISE", "/opt/agent/bin/mise")
DECLARATION = ".github/agent-checks.yaml"
DEFAULT_BRANCH = "refs/remotes/origin/HEAD"
# RC9: the install's bound; the declaration's default and ceiling, in minutes of MINUTE seconds.
MINUTE = 60
INSTALL_S = 300
DEFAULT_MINUTES, MAX_MINUTES = 8, 15
# RC4: a line at least every 20 s keeps the terminal's 30 s no-output pause away (SDK 1.49.6,
# NO_CHANGE_TIMEOUT_SECONDS); the summary's cap; each failed step's tail.
HEARTBEAT_S = 15
SUMMARY_CAP = 8000
TAIL_LINES = 60
KILL_GRACE_S = 5
STEP_NAME = re.compile(r"^[a-z0-9][a-z0-9-]{0,39}$")
# RC5: what a step never inherits. CI=true turns a missing-Docker skip into a failure.
SCRUBBED = {"CI", "GITHUB_ACTIONS", "GH_TOKEN", "GITHUB_TOKEN", "GIT_TOKEN_FILE", "GIT_TOKEN_CACHE",
            "LLM_API_KEY_FILE", "LLM_API_KEY", "OH_SECRET_KEY", "SESSION_API_KEY", "STS_URL"}
SCRUBBED_PREFIXES = ("OH_SESSION_API_KEYS_", "MISE_")
RESULTS = ("pass", "fail", "timeout", "skip")
LEDGER = "runs.jsonl"


class Undeclared(Exception):
    """The repository declares nothing usable: exit 2."""


def git(*args: str) -> subprocess.CompletedProcess:
    return subprocess.run(["git", "-C", REPO, *args], capture_output=True, text=True)


def validate(doc) -> dict:
    """The declaration, checked as each repository's coverage suite checks it; Undeclared names the
    first problem."""
    def need(cond, why):
        if not cond:
            raise Undeclared("%s: %s" % (DECLARATION, why))
    need(isinstance(doc, dict), "not a mapping")
    extra = set(doc) - {"version", "timeoutMinutes", "tools", "definedBy", "steps", "ciOnly"}
    need(not extra, "unknown keys %s" % sorted(extra))
    need(doc.get("version") == 1, "version must be 1")
    minutes = doc.get("timeoutMinutes", DEFAULT_MINUTES)
    need(isinstance(minutes, int) and 1 <= minutes <= MAX_MINUTES, "timeoutMinutes must be 1 to %d" % MAX_MINUTES)
    for key in ("tools", "definedBy"):
        value = doc.get(key, [])
        need(isinstance(value, list) and all(isinstance(t, str) and t for t in value), "%s must be a list of strings" % key)
    steps = doc.get("steps")
    need(isinstance(steps, list) and steps, "steps must be a non-empty list")
    names = set()
    for s in steps:
        need(isinstance(s, dict) and not set(s) - {"name", "run", "ci", "timeoutMinutes", "stopOnFailure"},
             "a step takes name, run, ci, timeoutMinutes and stopOnFailure")
        need(isinstance(s.get("name"), str) and STEP_NAME.match(s["name"]) and s["name"] not in names,
             "step names are unique and match %s" % STEP_NAME.pattern)
        names.add(s["name"])
        need(isinstance(s.get("run"), str) and s["run"].strip(), "step %s has no run" % s["name"])
        need(isinstance(s.get("ci"), str) and s["ci"], "step %s names no ci check" % s["name"])
        t = s.get("timeoutMinutes", minutes)
        need(isinstance(t, int) and 1 <= t <= minutes, "step %s: timeoutMinutes must be 1 to %d" % (s["name"], minutes))
        need(isinstance(s.get("stopOnFailure", False), bool), "step %s: stopOnFailure is a boolean" % s["name"])
    ci_only = doc.get("ciOnly", [])
    need(isinstance(ci_only, list) and all(isinstance(c, dict) and set(c) == {"check", "part", "why"} for c in ci_only),
         "every ciOnly entry has exactly check, part and why")
    return {"minutes": minutes, "tools": doc.get("tools", []), "definedBy": doc.get("definedBy", []),
            "steps": steps, "ciOnly": ci_only}


def declaration() -> tuple[str, str, dict]:
    """(default branch commit, declaration blob, declaration), from the clone's default branch."""
    base = git("rev-parse", "--verify", "--quiet", DEFAULT_BRANCH + "^{commit}").stdout.strip()
    if not base:
        raise Undeclared("the clone has no default branch (%s)" % DEFAULT_BRANCH)
    blob = git("rev-parse", "--verify", "--quiet", "%s:%s" % (base, DECLARATION)).stdout.strip()
    if not blob:
        raise Undeclared("the default branch declares no checks (%s)" % DECLARATION)
    try:
        doc = yaml.safe_load(git("cat-file", "blob", blob).stdout)
    except yaml.YAMLError as exc:
        raise Undeclared("%s: %s" % (DECLARATION, exc)) from exc
    return base, blob, validate(doc)


def pins(tools: list[str]) -> dict[str, str | None]:
    """Each declared tool's version in the work tree's mise.toml, the branch's pins (a Renovate bump
    included); None for a tool the file does not pin."""
    try:
        with open(os.path.join(REPO, "mise.toml"), "rb") as f:
            table = tomllib.load(f).get("tools", {})
    except (OSError, tomllib.TOMLDecodeError):
        table = {}
    out = {}
    for t in tools:
        v = table.get(t)
        v = v.get("version") if isinstance(v, dict) else v
        out[t] = v if isinstance(v, str) and v else None
    return out


def mise_env(token: str = "") -> dict:
    """mise's own environment: every directory under the toolchain, no global or project config
    read, no versions host (measured: 58 refused connections an install), and the run's GitHub
    token for its API calls when one is given: the install only, never a step."""
    env = {"HOME": os.environ.get("HOME", "/tmp"), "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
           "MISE_DATA_DIR": os.path.join(TOOLCHAIN, "mise"), "MISE_CACHE_DIR": os.path.join(TOOLCHAIN, "mise-cache"),
           "MISE_STATE_DIR": os.path.join(TOOLCHAIN, "mise-state"), "MISE_CONFIG_DIR": os.path.join(TOOLCHAIN, "mise-config"),
           "MISE_GLOBAL_CONFIG_FILE": os.path.join(TOOLCHAIN, "mise-config", "none.toml"),
           "MISE_USE_VERSIONS_HOST": "0", "MISE_YES": "1"}
    for k in ("HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy", "MISE_LOG_LEVEL"):
        if k in os.environ:
            env[k] = os.environ[k]
    if token:
        env["GITHUB_TOKEN"] = token
    return env


def github_token() -> str:
    """The run's token from git's credential helper (git-credential-agent), or "" when there is none:
    mise's GitHub API calls then go unauthenticated."""
    try:
        out = subprocess.run(["git", "credential", "fill"], input="protocol=https\nhost=github.com\n\n",
                             capture_output=True, text=True, timeout=20,
                             env={**os.environ, "GIT_TERMINAL_PROMPT": "0"}).stdout
    except (OSError, subprocess.TimeoutExpired):
        return ""
    return next((line[9:] for line in out.splitlines() if line.startswith("password=")), "")


def installed(specs: dict[str, str]) -> set[str]:
    """The tools whose spec mise already holds."""
    env = {**mise_env(), "MISE_OFFLINE": "1"}
    return {t for t, s in specs.items()
            if subprocess.run([MISE, "where", s], cwd=TOOLCHAIN, env=env, capture_output=True).returncode == 0}


def wait(proc, limit: float, beat) -> str:
    """Waits for proc at most limit seconds, calling beat(elapsed) every HEARTBEAT_S. On overrun its
    whole process group gets SIGTERM, then SIGKILL. "done" or "timeout"."""
    start = time.monotonic()
    while True:
        left = limit - (time.monotonic() - start)
        try:
            proc.wait(timeout=max(0.0, min(HEARTBEAT_S, left)))
            return "done"
        except subprocess.TimeoutExpired:
            pass
        if time.monotonic() - start >= limit:
            for sig in (signal.SIGTERM, signal.SIGKILL):
                try:
                    os.killpg(proc.pid, sig)
                except ProcessLookupError:
                    break
                try:
                    proc.wait(timeout=KILL_GRACE_S)
                    break
                except subprocess.TimeoutExpired:
                    continue
            return "timeout"
        beat(int(time.monotonic() - start))


def install(tools: dict[str, str | None], say) -> list[str]:
    """Installs the pinned tools mise lacks, within INSTALL_S; returns the declared tools still missing."""
    os.makedirs(TOOLCHAIN, exist_ok=True)
    specs = {t: "%s@%s" % (t, v) for t, v in tools.items() if v}
    have = installed(specs)
    todo = [s for t, s in specs.items() if t not in have]
    if todo:
        say("installing %s" % " ".join(todo))
        with open(os.path.join(STATE, "install.log"), "a") as log:
            proc = subprocess.Popen([MISE, "install", *todo], cwd=TOOLCHAIN, env=mise_env(github_token()),
                                    stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
            if wait(proc, INSTALL_S, lambda s: say("installing, %ds" % s)) == "timeout":
                say("the install stopped after %ds" % INSTALL_S)
        have = installed(specs)
    return [t for t in tools if t not in have]


def step_env(tools: dict[str, str | None], missing: list[str]) -> dict:
    """The agent's environment, scrubbed (RC5), with the installed tools first on PATH as `mise env`
    gives them (PATH, GOROOT, GOBIN: what the CI's mise-action exports), and the caches under the
    toolchain: the 10 Gi workspace, kept by a suspendable run's PVC, not HOME's 2 Gi."""
    env = {k: v for k, v in os.environ.items() if k not in SCRUBBED and not k.startswith(SCRUBBED_PREFIXES)}
    env.setdefault("AI_AGENT", "openhands")
    specs = ["%s@%s" % (t, v) for t, v in tools.items() if v and t not in missing]
    if specs:
        out = subprocess.run([MISE, "env", "--json", *specs], cwd=TOOLCHAIN, capture_output=True, text=True,
                             env={**mise_env(), "MISE_OFFLINE": "1", "PATH": env.get("PATH", "/usr/bin:/bin")})
        if out.returncode == 0:
            env.update(json.loads(out.stdout))
    env["XDG_CACHE_HOME"] = os.path.join(TOOLCHAIN, "cache")
    env["GOPATH"] = os.path.join(TOOLCHAIN, "go")
    return env


def run_step(step: dict, env: dict, limit: float, log, say) -> dict:
    """One step in its own process group; the full output to log, the last TAIL_LINES kept."""
    say("%s: %s" % (step["name"], step["run"]))
    log.write("\n=== %s: %s\n" % (step["name"], step["run"]))
    log.flush()
    tail = collections.deque(maxlen=TAIL_LINES)
    start = time.monotonic()
    proc = subprocess.Popen(["bash", "-c", step["run"]], cwd=REPO, env=env, stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True,
                            text=True, errors="replace")

    def drain():
        for line in proc.stdout:
            log.write(line)
            tail.append(line.rstrip("\n"))
        log.flush()
    reader = threading.Thread(target=drain, daemon=True)
    reader.start()
    ended = wait(proc, limit, lambda s: say("%s running, %ds" % (step["name"], s)))
    reader.join(KILL_GRACE_S)
    seconds = round(time.monotonic() - start, 1)
    result = "timeout" if ended == "timeout" else ("pass" if proc.returncode == 0 else "fail")
    say("%s: %s in %ss" % (step["name"], result, seconds))
    return {"name": step["name"], "ci": step["ci"], "result": result, "seconds": seconds,
            "exit": proc.returncode, "tail": list(tail) if result != "pass" else []}


def changed() -> set[str]:
    """Tracked files the work tree changes against HEAD."""
    return set(git("diff", "--name-only", "HEAD").stdout.split())


def touched(base: str, defined_by: list[str]) -> list[str]:
    """The definedBy paths the branch changes against the default branch, committed or not."""
    fork = git("merge-base", base, "HEAD").stdout.strip() or base
    names = set(git("diff", "--name-only", fork, "HEAD").stdout.split()) | changed()
    return sorted(n for n in names if any(n == p or (p.endswith("/") and n.startswith(p)) for p in defined_by))


def summary(res: dict) -> str:
    """At most SUMMARY_CAP characters: the counts, a line per step, each failed step's tail, what
    stays CI-only, the full log's path. Tails give way first."""
    counts = collections.Counter(s["result"] for s in res["steps"])
    head = ["agent-checks %s (declaration %s): %s in %ss" % (
        res["head"][:12], res["declaration"][:12],
        ", ".join("%d %s" % (counts[r], r) for r in RESULTS if counts[r]), res["seconds"])]
    if res["dirty"]:
        head.append("the work tree had uncommitted changes: this is not %s's result alone" % res["head"][:12])
    if res["changedByChecks"]:
        head.append("the checks changed these files: %s (review them, then commit or restore them)"
                    % ", ".join(res["changedByChecks"]))
    if res["missingTools"]:
        head.append("toolchain incomplete: %s" % ", ".join(res["missingTools"]))
    if res["definitionTouched"]:
        head.append("this branch changes how the checks are defined (%s): unverified until the CI runs"
                    % ", ".join(res["definitionTouched"]))
    head += ["  %-7s %-16s %6ss  ci: %s%s" % (s["result"], s["name"], s["seconds"], s["ci"],
                                               "  (%s)" % s["reason"] if s.get("reason") else "") for s in res["steps"]]
    foot = ["CI-only, the CI still runs these:"]
    foot += ["  %s: %s (%s)" % (c["check"], c["part"], c["why"]) for c in res["ciOnly"]]
    foot += ["full log: %s" % res["log"], "The CI stays the judge of a merge: a green result here is not the CI's."]
    failed = [s for s in res["steps"] if s["tail"]]
    room = SUMMARY_CAP - len("\n".join(head + foot)) - 1
    tails = []
    for i, s in enumerate(failed):
        title = "--- %s, last lines ---" % s["name"]
        keep = room // (len(failed) - i) - len(title) - 2
        if keep <= 0:
            continue
        body = "\n".join(s["tail"])
        if len(body) > keep:  # the tail's end matters most: cut from its start, at a line
            body = body[len(body) - keep:]
            body = body.split("\n", 1)[1] if "\n" in body else body
        tails.append(title + "\n" + body)
        room -= len(tails[-1]) + 1
    return "\n".join(head + tails + foot)[:SUMMARY_CAP]


def checks(decl: dict, base: str, blob: str, only: str | None, say) -> int:
    tools = pins(decl["tools"])
    missing = install(tools, say)
    steps = [s for s in decl["steps"] if only in (None, s["name"])]
    head = git("rev-parse", "HEAD").stdout.strip()
    before = changed()
    env = step_env(tools, missing)
    stem = head if only is None else "%s.step-%s" % (head, only)
    path = os.path.join(STATE, stem + ".log")
    start, budget = time.monotonic(), decl["minutes"] * MINUTE
    results, stopped = [], False
    with open(path, "a") as log:
        for s in steps:
            left = budget - (time.monotonic() - start)
            if stopped or left <= 0:
                results.append({"name": s["name"], "ci": s["ci"], "result": "skip", "seconds": 0, "exit": None, "tail": [],
                                "reason": "an earlier step stopped the run" if stopped else "the budget is spent"})
                continue
            r = run_step(s, env, min(s.get("timeoutMinutes", decl["minutes"]) * MINUTE, left), log, say)
            if r["result"] != "pass" and missing:
                r["reason"] = "toolchain incomplete"
            results.append(r)
            stopped = s.get("stopOnFailure", False) and r["result"] != "pass"
    res = {"version": 1, "head": head, "dirty": bool(before), "base": base, "declaration": blob, "tools": tools,
           "missingTools": missing, "steps": results, "partial": only is not None, "ciOnly": decl["ciOnly"],
           "changedByChecks": sorted(changed() - before), "definitionTouched": touched(base, decl["definedBy"]),
           "seconds": round(time.monotonic() - start, 1), "log": path}
    text = summary(res)
    record = {**res, "steps": [{k: v for k, v in r.items() if k != "tail"} for r in results]}
    with open(os.path.join(STATE, stem + ".json"), "w") as f:
        json.dump(record, f, indent=1)
    with open(os.path.join(STATE, stem + ".txt"), "w") as f:
        f.write(text + "\n")
    with open(os.path.join(STATE, LEDGER), "a") as f:
        f.write(json.dumps({"head": head, "partial": record["partial"], "seconds": record["seconds"],
                            "counts": dict(collections.Counter(r["result"] for r in results))}) + "\n")
    print(text, flush=True)
    return 1 if any(r["result"] in ("fail", "timeout") for r in results) else 0


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(prog="agent-checks", description="The CI's fast subset, in the sandbox.")
    ap.add_argument("--step", help="run this step only")
    ap.add_argument("--install", action="store_true", help="install the toolchain only")
    args = ap.parse_args(argv)

    def say(message):
        print("agent-checks: " + message, flush=True)
    try:
        base, blob, decl = declaration()
    except Undeclared as exc:
        say(str(exc))
        return 2
    if args.step and args.step not in [s["name"] for s in decl["steps"]]:
        say("no step %s; declared: %s" % (args.step, ", ".join(s["name"] for s in decl["steps"])))
        return 2
    os.makedirs(STATE, exist_ok=True)
    with open(os.path.join(STATE, ".lock"), "w") as lock:
        waited = time.monotonic()
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                say("waiting for another agent-checks (the toolchain install, or a run), %ds" % (time.monotonic() - waited))
                time.sleep(HEARTBEAT_S)
        if args.install:
            missing = install(pins(decl["tools"]), say)
            say("toolchain ready" if not missing else "toolchain incomplete: %s" % ", ".join(missing))
            return 1 if missing else 0
        return checks(decl, base, blob, args.step, say)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
```

- [ ] **Step 4: Run the tests**

Run: `cd images/agent-harness && python -m unittest tests.test_agent_checks -v`
Expected: `Ran 18 tests … OK`.

- [ ] **Step 5: Commit**

```bash
git add images/agent-harness/agent_checks.py images/agent-harness/tests/test_agent_checks.py
git commit -m "feat(harness): agent-checks, the repository's fast checks in the sandbox"
```

### Task 2: The image: mise, PyYAML, rsync and `agent-checks`

**Files:** Modify `images/agent-harness/Dockerfile`, `images/agent-harness/README.md`; create
`images/agent-harness/checks-requirements.txt`, `images/agent-harness/tests/test_image_tools.py`.

- [ ] **Step 1: Write the failing test** `images/agent-harness/tests/test_image_tools.py`:

```python
"""The image carries what agent-checks needs (v3 feature 3, R1 and R4). Inside the image only."""
import os
import shutil
import subprocess
import sys
import unittest

import yaml

IN_IMAGE = os.path.exists("/agent-server/.venv")


@unittest.skipUnless(IN_IMAGE, "outside the harness image")
class ImageToolsTest(unittest.TestCase):
    def test_mise_runs_and_stays_off_path(self):
        self.assertEqual(subprocess.run(["/opt/agent/bin/mise", "--version"], capture_output=True).returncode, 0)
        self.assertIsNone(shutil.which("mise"))

    def test_the_system_python_has_the_venvs_pyyaml(self):
        out = subprocess.run(["/usr/local/bin/python3", "-c", "import yaml; print(yaml.__version__)"],
                             capture_output=True, text=True, check=True).stdout.strip()
        self.assertEqual(out, yaml.__version__)

    def test_rsync_and_agent_checks_are_on_path(self):
        self.assertIsNotNone(shutil.which("rsync"))
        self.assertEqual(os.path.realpath(shutil.which("agent-checks")), "/opt/agent/agent_checks.py")


if __name__ == "__main__":
    unittest.main(argv=sys.argv)
```

- [ ] **Step 2: Run it to see it fail**

Run: `task harness:test`
Expected: FAIL in `test_mise_runs_and_stays_off_path` (`FileNotFoundError: /opt/agent/bin/mise`).

- [ ] **Step 3: Create `images/agent-harness/checks-requirements.txt`** with the `pyyaml==6.0.2`
block copied verbatim from `requirements.txt` (the line and all its `--hash` lines), under this
header:

```text
# PyYAML for the system python3 the repositories' checks run (v3 feature 3, R4): the same version
# and hashes as requirements.txt's, which pip-compile owns. Move them together.
```

- [ ] **Step 4: Change the Dockerfile.** After `ARG GH_SHA256_ARM64=…` add:

```dockerfile
# jdx/mise release (v3 feature 3): agent-checks installs a repository's declared tools with it, at
# the versions its mise.toml pins. Move MISE_VERSION and both sums together by hand.
ARG MISE_VERSION=2026.10.3
ARG MISE_SHA256_AMD64=04147c68e946902f5226dfdcd54d19907aed3cf54d95b2f27d2b9c778bb26f9e
ARG MISE_SHA256_ARM64=e79866e32624b346f6854d93ca8a24294516cd7c0b48ce0e80af508fb7c3d8b8
```

Replace the first `RUN set -eux; …` block with:

```dockerfile
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) sum="${GH_SHA256_AMD64}" msum="${MISE_SHA256_AMD64}" march=x64 ;; \
      arm64) sum="${GH_SHA256_ARM64}" msum="${MISE_SHA256_ARM64}" march=arm64 ;; \
      *) exit 1 ;; \
    esac; \
    fetch() { /usr/local/bin/python -c "import sys, urllib.request; urllib.request.urlretrieve(sys.argv[1], sys.argv[2])" "$1" "$2"; }; \
    fetch "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${TARGETARCH}.tar.gz" /tmp/gh.tgz; \
    echo "${sum}  /tmp/gh.tgz" | sha256sum -c -; \
    tar -xzf /tmp/gh.tgz -C /tmp; \
    install -m 0755 "/tmp/gh_${GH_VERSION}_linux_${TARGETARCH}/bin/gh" /usr/local/lib/gh-real; \
    rm -rf /tmp/gh.tgz "/tmp/gh_${GH_VERSION}_linux_${TARGETARCH}"; \
    fetch "https://github.com/jdx/mise/releases/download/v${MISE_VERSION}/mise-v${MISE_VERSION}-linux-${march}.tar.gz" /tmp/mise.tgz; \
    echo "${msum}  /tmp/mise.tgz" | sha256sum -c -; \
    tar -xzf /tmp/mise.tgz -C /tmp; \
    install -D -m 0755 /tmp/mise/bin/mise /opt/agent/bin/mise; \
    rm -rf /tmp/mise.tgz /tmp/mise; \
    apt-get update; \
    apt-get install -y --no-install-recommends rsync; \
    rm -rf /var/lib/apt/lists/*; \
    rm -f /etc/sudoers.d/*; \
    if [ -f /etc/sudoers ]; then sed -i '/NOPASSWD/d' /etc/sudoers; fi; \
    find / -xdev -type f -perm /6000 -exec chmod ug-s {} +
```

The base image's apt sources are `snapshot.debian.org` at `20260920T000000Z`, so `rsync` is pinned
by the base image's digest. After the venv `RUN`, add:

```dockerfile
# The system python3 the repositories' checks run gets the venv's PyYAML (v3 feature 3, R4).
COPY checks-requirements.txt /tmp/checks-requirements.txt
RUN /usr/local/bin/python -m pip install --no-cache-dir --require-hashes --only-binary=:all: \
      -r /tmp/checks-requirements.txt \
 && rm /tmp/checks-requirements.txt
```

Change the copy and the links:

```dockerfile
COPY --chmod=0755 agent_run.py agent_checks.py git_credential_agent.py pr_footer.py /opt/agent/
…
RUN ln -s /opt/agent/agent_run.py /usr/local/bin/agent-run \
 && ln -s /opt/agent/agent_checks.py /usr/local/bin/agent-checks \
 && ln -s /opt/agent/git_credential_agent.py /usr/local/bin/git-credential-agent
```

- [ ] **Step 5: README.** In the file table add the row:

```markdown
| `agent_checks.py` → `agent-checks` | v3 feature 3: the CI's fast subset the repository declares in `.github/agent-checks.yaml` (read from the default branch), with its tools installed by mise (`/opt/agent/bin/mise`, off `PATH`) at the work tree's `mise.toml` pins into `/workspace/.toolchain`. Each step runs in its own process group without `CI` or any credential path; the result is `/workspace/.agent-checks/<sha>.json` and `.txt`. Evidence, never a gate |
```

In "Optional environment" add:

```markdown
| `AGENT_CHECKS` | `1`: the toolchain installs in the background after the clone, the rules gain the repository-checks rule, and a `finished` implementer run checks its head, notes the result in the room and emits the span `agent-run.checks` | today's run |
| `AGENT_RUN_SECONDS` | the pod's deadline: the final checks are skipped (`skipped: deadline`) when less than the declaration's budget plus 2 min is left | the final checks always run |
```

- [ ] **Step 6: Run** `task harness:test`. Expected: every suite passes, `test_image_tools` included.

- [ ] **Step 7: Commit**

```bash
git add images/agent-harness/Dockerfile images/agent-harness/checks-requirements.txt \
  images/agent-harness/tests/test_image_tools.py images/agent-harness/README.md
git commit -m "feat(harness): mise, PyYAML and rsync in the image, agent-checks on PATH"
```

### Task 3: `agent-run`: the rule, the milestone, the prefetch and the final run

**Files:** Modify `images/agent-harness/agent_run.py`; create `images/agent-harness/tests/test_agent_run_checks.py`.

**Interfaces:**
- Consumes: Task 1's `agent_checks.declaration()`, `Undeclared`, `INSTALL_S`, the files it writes.
- Produces: `declared(env) -> dict | None`, `checks_rules(env, decl) -> str`,
  `prefetch(start_span) -> Popen`, `final_checks(env, decl, notes, start_span, started, clock) -> dict | None`,
  `checks_note(role, res, by_agent) -> str`, `post_once(notes, text, sleep)`, `checks_ledger() -> str`.
  The loop policy's judge reads `/workspace/.agent-checks/<head>.txt` after `final_checks`.

- [ ] **Step 1: Write the failing test** `images/agent-harness/tests/test_agent_run_checks.py`:

```python
"""agent-run's half of v3 feature 3 (offline proof 5): the rule, the milestone, the prefetch, the final
run on the head, its note and its deadline. agent-checks itself is test_agent_checks.py's."""
import json
import os
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)
import agent_run  # noqa: E402

DECL = {"minutes": 8, "tools": [], "definedBy": [], "steps": [], "ciOnly": []}
# A stand-in agent-checks: writes the result its first argument names for the head, counts its runs.
FAKE_TOOL = r'''#!{python}
import json, os, subprocess, sys
state = os.environ["FAKE_CHECKS_DIR"]
head = subprocess.run(["git", "-C", os.environ["FAKE_REPO"], "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
with open(os.path.join(state, "calls"), "a") as f:
    f.write(" ".join(sys.argv[1:]) + "\n")
steps = [{"name": "suites", "result": "pass"}, {"name": "pre-commit", "result": "fail"}]
json.dump({"version": 1, "head": head, "dirty": False, "partial": False, "steps": steps}, open(os.path.join(state, head + ".json"), "w"))
'''


class FakeNotes:
    def __init__(self, limited=0):
        self.sent, self.limited, self.last = [], limited, None

    @staticmethod
    def clock():
        return 0.0

    def send(self, text):
        if self.limited:
            self.limited -= 1
            return False
        self.sent.append(text)
        return True


class RuleTest(unittest.TestCase):
    def test_agent_checks_reaches_the_checks_milestone(self):
        self.assertEqual(agent_run.milestones("agent-checks"), {"checks"})
        self.assertEqual(agent_run.milestones("agent-checks --step suites"), {"checks"})
        self.assertEqual(agent_run.milestones("echo agent-checks"), set())

    def test_the_rule_is_the_roles_and_only_on_a_check_enabled_run_that_declares(self):
        implementer = agent_run.checks_rules({"ROLE": "implementer"}, DECL)
        self.assertIn("Before every push, run `agent-checks` and fix what it reports. It runs the CI's fast subset; "
                      "the CI stays the judge. If a failure is not your change's, say so in the pull request.", implementer)
        self.assertIn("cite its first line as your evidence", agent_run.checks_rules({"ROLE": "reviewer"}, DECL))
        self.assertEqual(agent_run.checks_rules({"ROLE": "tester"}, DECL), "")
        self.assertEqual(agent_run.checks_rules({"ROLE": "implementer"}, None), "")

    def test_nothing_is_declared_without_the_switch(self):
        with mock.patch.object(agent_run, "checks_module") as module:
            self.assertIsNone(agent_run.declared({}))
        module.assert_not_called()


class FinalChecksTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.repo, self.state = os.path.join(self.tmp, "repo"), os.path.join(self.tmp, "state")
        os.makedirs(self.state)
        subprocess.run(["git", "init", "-q", self.repo], check=True)
        subprocess.run(["git", "-C", self.repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q",
                        "--allow-empty", "-m", "head"], check=True)
        self.head = subprocess.run(["git", "-C", self.repo, "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
        tool = os.path.join(self.tmp, "agent-checks")
        with open(tool, "w") as f:
            f.write(FAKE_TOOL.replace("{python}", sys.executable, 1))
        os.chmod(tool, 0o755)
        for name, value in (("REPO_DIR", self.repo), ("CHECKS_DIR", self.state), ("CHECKS_TOOL", tool)):
            p = mock.patch.object(agent_run, name, value)
            p.start()
            self.addCleanup(p.stop)
        p = mock.patch.dict(os.environ, {"FAKE_CHECKS_DIR": self.state, "FAKE_REPO": self.repo})
        p.start()
        self.addCleanup(p.stop)
        self.env = {"ROLE": "implementer", "AGENT_CHECKS": "1"}

    def calls(self):
        try:
            with open(os.path.join(self.state, "calls")) as f:
                return f.read().splitlines()
        except FileNotFoundError:
            return []

    def test_off_or_a_reviewer_runs_nothing(self):
        self.assertIsNone(agent_run.final_checks(self.env, None))
        self.assertIsNone(agent_run.final_checks({**self.env, "ROLE": "reviewer"}, DECL))
        self.assertEqual(self.calls(), [])

    def test_the_head_is_checked_and_the_note_names_the_failed_step(self):
        notes = FakeNotes()
        res = agent_run.final_checks(self.env, DECL, notes)
        self.assertEqual(self.calls(), [""])
        self.assertEqual(res["head"], self.head)
        self.assertEqual(notes.sent, ["Harness (implementer): checks on %s: 1 pass, 1 fail. Failed: pre-commit." % self.head[:12]])

    def test_the_agents_full_clean_result_for_the_head_stands(self):
        with open(os.path.join(self.state, self.head + ".json"), "w") as f:
            json.dump({"head": self.head, "dirty": False, "partial": False, "steps": [{"name": "suites", "result": "pass"}]}, f)
        notes = FakeNotes()
        agent_run.final_checks(self.env, DECL, notes)
        self.assertEqual(self.calls(), [])
        self.assertEqual(notes.sent, ["Harness (implementer): checks on %s: 1 pass (the agent's own run)." % self.head[:12]])

    def test_a_partial_or_dirty_result_is_run_again(self):
        for stale in ({"partial": True}, {"dirty": True}):
            with self.subTest(stale=stale):
                with open(os.path.join(self.state, self.head + ".json"), "w") as f:
                    json.dump({"head": self.head, "steps": [], **stale}, f)
                before = len(self.calls())
                agent_run.final_checks(self.env, DECL, FakeNotes())
                self.assertEqual(len(self.calls()), before + 1)

    def test_too_little_time_left_records_a_skip(self):
        notes = FakeNotes()
        res = agent_run.final_checks({**self.env, "AGENT_RUN_SECONDS": "600"}, DECL, notes, started=0.0, clock=lambda: 100.0)
        self.assertEqual(res, {"version": 1, "head": self.head, "skipped": "deadline"})
        self.assertEqual(self.calls(), [])
        with open(os.path.join(self.state, self.head + ".json")) as f:
            self.assertEqual(json.load(f)["skipped"], "deadline")
        self.assertEqual(notes.sent, ["Harness (implementer): checks on %s skipped: deadline." % self.head[:12]])

    def test_enough_time_left_runs(self):
        agent_run.final_checks({**self.env, "AGENT_RUN_SECONDS": "7200"}, DECL, FakeNotes(), started=0.0, clock=lambda: 100.0)
        self.assertEqual(self.calls(), [""])

    def test_a_rate_limited_note_waits_out_the_minute_once(self):
        notes, slept = FakeNotes(limited=1), []
        agent_run.post_once(notes, "the note", sleep=slept.append)
        self.assertEqual((slept, notes.sent), ([agent_run.MIN_NOTE_GAP_S], ["the note"]))

    def test_the_final_run_is_a_span_that_says_why_it_failed(self):
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import SimpleSpanProcessor
        from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
        exporter = InMemorySpanExporter()
        provider = TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(exporter))
        agent_run.final_checks(self.env, DECL, None, lambda name: provider.get_tracer("t").start_span(name))
        (span,) = exporter.get_finished_spans()
        self.assertEqual((span.name, dict(span.attributes), span.status.status_code.name),
                         ("agent-run.checks", {"error.type": "checks_failed"}, "ERROR"))

    def test_the_note_fits_the_brokers_280_characters(self):
        steps = [{"name": "step-%02d-%s" % (i, "x" * 30), "result": "fail"} for i in range(20)]
        text = agent_run.checks_note("implementer", {"head": self.head, "steps": steps}, False)
        self.assertLessEqual(len(text), agent_run.NOTE_MAX)
        self.assertTrue(text.endswith(", ….") and "Failed: step-00" in text)


class MainChecksTest(unittest.TestCase):
    def _main(self, env, poll=0, decl=DECL):
        with tempfile.TemporaryDirectory() as d:
            task = os.path.join(d, "task.md")
            open(task, "w").write("rules")
            env = {"TASK_FILE": task, "RULES_FILE": task, "ROLE": "implementer", **env}
            with mock.patch.dict(os.environ, env, clear=True), \
                    mock.patch("agent_run.subprocess.Popen"), \
                    mock.patch("agent_run.wait_ready"), \
                    mock.patch("agent_run.persisted", return_value=False), \
                    mock.patch("agent_run.clone", return_value=False), \
                    mock.patch("agent_run.checks_module") as module, \
                    mock.patch("agent_run.prefetch") as pre, \
                    mock.patch("agent_run.final_checks") as final, \
                    mock.patch("agent_run.build_request", return_value={"x": 1}) as build, \
                    mock.patch("agent_run.http", return_value={"id": "c1"}), \
                    mock.patch("agent_run.poll", return_value=poll), \
                    mock.patch("agent_run.StepLog"), \
                    mock.patch("agent_run.stop", return_value="stopped"), \
                    mock.patch("agent_run.timed"):
                module.return_value.declaration.return_value = ("base", "blob", decl)
                agent_run.main()
        return pre, final, build.call_args.args[2]

    def test_a_check_enabled_run_prefetches_states_the_rule_and_checks_the_finished_head(self):
        pre, final, rules = self._main({"AGENT_CHECKS": "1"})
        pre.assert_called_once()
        self.assertTrue(rules.startswith("rules\nRepository checks:\n- Before every push"))
        self.assertEqual(final.call_args.args[1], DECL)

    def test_without_the_switch_nothing_changes(self):
        pre, final, rules = self._main({})
        pre.assert_not_called()
        self.assertEqual(rules, "rules")
        self.assertIsNone(final.call_args.args[1])

    def test_a_failed_conversation_is_not_checked(self):
        _, final, _ = self._main({"AGENT_CHECKS": "1"}, poll=1)
        final.assert_not_called()


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Run it to see it fail**

Run: `cd images/agent-harness && python -m unittest tests.test_agent_run_checks`
Expected: `AttributeError: module 'agent_run' has no attribute 'checks_rules'`.

- [ ] **Step 3: Apply this change to `images/agent-harness/agent_run.py`**

```diff
--- /home/smana/Sources/agent-platform-sandbox-checks/images/agent-harness/agent_run.py	2026-10-22 19:19:19.033497814 +0200
+++ harness/agent_run.py	2026-10-22 19:58:13.246330621 +0200
@@ -394,7 +394,8 @@
         elif (prog in ("pytest", "py.test") or (prog.startswith("python") and rest[:2] == ["-m", "pytest"])
               or (prog in ("go", "cargo", "make") and rest[:1] == ["test"])
               or (prog == "npm" and (rest[:1] == ["test"] or rest[:2] == ["run", "test"]))
-              or (prog == "task" and rest[:1] and CHECK_TARGET.fullmatch(rest[0]))):
+              or (prog == "task" and rest[:1] and CHECK_TARGET.fullmatch(rest[0]))
+              or prog == "agent-checks"):
             found.add("checks")
     return found
 
@@ -532,6 +533,161 @@
         return True
 
 
+# v3 feature 3: the repository's checks in the sandbox, on a check-enabled run only (AGENT_CHECKS=1,
+# set by the composition from the claim's spec.loop.checks). agent-checks sits beside this file.
+CHECKS_TOOL = "/usr/local/bin/agent-checks"
+CHECKS_DIR = "/workspace/.agent-checks"
+CHECKS_RULES = {
+    "implementer": "Before every push, run `agent-checks` and fix what it reports. It runs the CI's fast subset; the "
+                   "CI stays the judge. If a failure is not your change's, say so in the pull request.",
+    "reviewer": "Before your verdict, run `agent-checks` on the pull request's head and cite its first line as your "
+                "evidence. It runs the CI's fast subset; the CI stays the judge.",
+}
+# RC9: the final run is skipped when the pod has less than the declaration's budget plus this left.
+CHECKS_MARGIN_S = 120
+NOTE_MAX = 280
+CHECKS_SPAN, INSTALL_SPAN = "agent-run.checks", "agent-run.checks.install"
+
+
+def checks_module():
+    sys.path.insert(0, os.path.dirname(os.path.realpath(__file__)))
+    import agent_checks
+
+    return agent_checks
+
+
+def declared(env: dict):
+    """The checks the clone's default branch declares, on a check-enabled run; None otherwise."""
+    if env.get("AGENT_CHECKS") != "1":
+        return None
+    ac = checks_module()
+    try:
+        return ac.declaration()[2]
+    except ac.Undeclared as exc:
+        print("agent-run: no repository checks: %s" % exc, file=sys.stderr, flush=True)
+        return None
+
+
+def checks_rules(env: dict, decl) -> str:
+    """The rule appended to the platform rules (RC4), for the roles that run the checks."""
+    rule = CHECKS_RULES.get(env.get("ROLE", "")) if decl else None
+    return "\nRepository checks:\n- %s\n" % rule if rule else ""
+
+
+def _end(span, error: str | None) -> None:
+    if not span:
+        return
+    if error:
+        from opentelemetry.trace import Status, StatusCode
+
+        span.set_attribute("error.type", error)  # the collector keeps error.type and drops the rest
+        span.set_status(Status(StatusCode.ERROR))
+    span.end()
+
+
+def prefetch(start_span=None):
+    """agent-checks --install in the background, right after the clone, so the toolchain installs
+    while the model reads the task (RC1). A waiter thread ends its span when it exits."""
+    os.makedirs(CHECKS_DIR, exist_ok=True)
+    span = start_span(INSTALL_SPAN) if start_span else None
+    with open(os.path.join(CHECKS_DIR, "prefetch.log"), "a") as log:
+        proc = subprocess.Popen([CHECKS_TOOL, "--install"], stdin=subprocess.DEVNULL, stdout=log,
+                                stderr=subprocess.STDOUT, start_new_session=True)
+    threading.Thread(target=lambda: _end(span, "install_incomplete" if proc.wait() else None), daemon=True).start()
+    return proc
+
+
+def _git_out(*args: str) -> str:
+    return subprocess.run(["git", "-C", REPO_DIR, *args], capture_output=True, text=True).stdout.strip()
+
+
+def _load(path: str) -> dict | None:
+    try:
+        with open(path) as f:
+            return json.load(f)
+    except (OSError, ValueError):
+        return None
+
+
+def checks_note(role: str, res: dict, by_agent: bool) -> str:
+    """The room note's fixed text (RC6): the head, the counts and the failed steps' names, which come
+    from the base's declaration. Nothing the agent wrote."""
+    lead = "Harness (%s): checks on %s" % (role, res["head"][:12])
+    if res.get("skipped"):
+        return "%s skipped: %s." % (lead, res["skipped"])
+    counts = {}
+    for s in res.get("steps", []):
+        counts[s["result"]] = counts.get(s["result"], 0) + 1
+    text = "%s: %s%s." % (lead, ", ".join("%d %s" % (counts[r], r) for r in ("pass", "fail", "timeout", "skip") if r in counts),
+                          " (the agent's own run)" if by_agent else "")
+    failed = [s["name"] for s in res.get("steps", []) if s["result"] in ("fail", "timeout")]
+    for i in range(len(failed), 0, -1):
+        more = " Failed: %s%s." % (", ".join(failed[:i]), ", …" if i < len(failed) else "")
+        if len(text + more) <= NOTE_MAX:
+            return text + more
+    return text[:NOTE_MAX]
+
+
+def final_checks(env: dict, decl, notes=None, start_span=None, started: float = 0.0, clock=time.monotonic):
+    """RC6: when an implementer's conversation ends finished, its head's checks are the evidence the
+    judge and the reviewer read. A full result the agent left for this head on a clean tree stands;
+    otherwise agent-checks runs, unless the pod lacks the budget plus CHECKS_MARGIN_S. Writes
+    CHECKS_DIR/<head>.json, posts the note, emits the span; returns the result, or None when off."""
+    if not decl or env.get("ROLE") != "implementer":
+        return None
+    head = _git_out("rev-parse", "HEAD")
+    path = os.path.join(CHECKS_DIR, head + ".json")
+    clean = not _git_out("diff", "--name-only", "HEAD")
+    res, by_agent = _load(path), True
+    if not (res and clean and not res.get("dirty") and not res.get("partial") and not res.get("skipped")):
+        res, by_agent = None, False
+    budget = decl["minutes"] * 60
+    limit = env.get("AGENT_RUN_SECONDS", "")
+    if res is None and limit.isdigit() and int(limit) - (clock() - started) < budget + CHECKS_MARGIN_S:
+        res = {"version": 1, "head": head, "skipped": "deadline"}
+        os.makedirs(CHECKS_DIR, exist_ok=True)
+        with open(path, "w") as f:
+            json.dump(res, f)
+    span = start_span(CHECKS_SPAN) if start_span and not by_agent else None
+    if res is None:
+        try:
+            subprocess.run([CHECKS_TOOL], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
+                           timeout=budget + checks_module().INSTALL_S + 60)
+        except subprocess.TimeoutExpired:
+            pass
+        res = _load(path) or {"version": 1, "head": head, "skipped": "no result"}
+    failed = any(s["result"] in ("fail", "timeout") for s in res.get("steps", []))
+    _end(span, "checks_" + res["skipped"].replace(" ", "_") if res.get("skipped") else "checks_failed" if failed else None)
+    text = checks_note(env.get("ROLE"), res, by_agent)
+    print("agent-run " + text.split(": ", 1)[1], flush=True)
+    if notes:
+        post_once(notes, text)
+    return res
+
+
+def post_once(notes, text: str, sleep=time.sleep) -> None:
+    """The final note, outside the milestone cap: on the broker's one-a-minute limit, it waits out
+    the rest of the minute once."""
+    try:
+        if not notes.send(text):
+            left = MIN_NOTE_GAP_S - (notes.clock() - notes.last) if notes.last is not None else MIN_NOTE_GAP_S
+            sleep(max(0.0, min(MIN_NOTE_GAP_S, left)))
+            notes.send(text)
+    except Exception as exc:  # noqa: BLE001 -- a note must never fail the run
+        print("agent-run: checks note not posted: %s" % exc, file=sys.stderr, flush=True)
+
+
+def checks_ledger() -> str:
+    """How often the agent ran agent-checks itself, from its ledger: a signal, the agent can write it."""
+    try:
+        with open(os.path.join(CHECKS_DIR, "runs.jsonl")) as f:
+            rows = [json.loads(line) for line in f if line.strip()]
+    except (OSError, ValueError):
+        return ""
+    return "agent-run checks ledger: %d runs, %d of one step, %.0fs" % (
+        len(rows), sum(1 for r in rows if r.get("partial")), sum(r.get("seconds", 0) for r in rows))
+
+
 def model_retries() -> int:
     try:
         with open(RETRY_LOG) as f:
@@ -824,6 +980,7 @@
 
 def main() -> int:
     signal.signal(signal.SIGTERM, _on_sigterm)
+    started = time.monotonic()
     env = dict(os.environ)
     # agent-server's conversations_path and bash_events_dir are relative to
     # its cwd; pin it to "/" so its state lands in the intended paths
@@ -840,8 +997,11 @@
         # either: it waits on a call's approval, a new message would reject that call, and a bare
         # run would execute it unconfirmed. The bridge answers it from the room's decision.
         resumed = persisted(env) or clone(env)
+        decl = declared(env)
+        if decl:
+            prefetch(child_spans(provider, span))
         with open(env["TASK_FILE"]) as t, open(env["RULES_FILE"]) as r:
-            request = build_request(env, t.read(), r.read(), span)
+            request = build_request(env, t.read(), r.read() + checks_rules(env, decl), span)
         cid = http("POST", "/api/conversations", request)["id"]
         notes = ProgressNotes(env["MCP_URL"], env.get("ROLE") or "agent", resumed,
                               start_span=child_spans(provider, span)) if env.get("MCP_URL") else None
@@ -849,6 +1009,10 @@
         code = poll(cid, on_tick=steps)
         steps()
         steps.summary()
+        if code == 0:
+            final_checks(env, decl, notes, child_spans(provider, span), started)
+        if decl and (ledger := checks_ledger()):
+            print(ledger, flush=True)
         if notes:
             notes.close()
         if env.get("BRIDGE_URL"):
```

- [ ] **Step 4: Run the harness suites**

Run: `cd images/agent-harness && python -m unittest tests.test_agent_run_checks tests.test_agent_checks tests.test_agent_run`
Expected: all pass (15 + 18 + 93 locally; `task harness:test` runs every suite in the image).

- [ ] **Step 5: Commit**

```bash
git add images/agent-harness/agent_run.py images/agent-harness/tests/test_agent_run_checks.py
git commit -m "feat(harness): the checks' rule, prefetch and final run on the head, with its note and span"
```

### Task 4: The run-controller's hosts and the portable `AgentRun`'s `loop.checks`

**Files:** Modify `api/v1alpha1/agentrun_types.go`, `api/v1alpha1/agentrun_crd_test.go`,
`internal/runctl/config.go`, `internal/runctl/config_test.go`, `internal/runctl/pod.go`; create
`internal/runctl/checks_test.go`; regenerate `api/v1alpha1/zz_generated.deepcopy.go`,
`config/crd/agents.ogenki.io_agentruns.yaml`, `internal/runctl/testdata/cnp.golden.yaml`.

- [ ] **Step 1: Write the failing tests:** the test hunks of the diff below
  (`agentrun_crd_test.go`: 9 rules; `config_test.go`: a github profile of 6; the new
  `internal/runctl/checks_test.go`).
- [ ] **Step 2:** `go test ./api/... ./internal/runctl/` → FAIL (`r.Spec.Loop undefined`).
- [ ] **Step 3: Apply the rest of the diff:**

```diff
diff --git a/api/v1alpha1/agentrun_crd_test.go b/api/v1alpha1/agentrun_crd_test.go
index 964b1d2..ee9e818 100644
--- a/api/v1alpha1/agentrun_crd_test.go
+++ b/api/v1alpha1/agentrun_crd_test.go
@@ -48,13 +48,15 @@ func TestAgentRunCRDKeepsTheXRDContract(t *testing.T) {
 		"has(self.roomRef) == has(oldSelf.roomRef) && (!has(self.roomRef) || self.roomRef == oldSelf.roomRef)":                         "roomRef is immutable and cannot be added or removed after creation",
 		"has(self.queueName) == has(oldSelf.queueName) && (!has(self.queueName) || self.queueName == oldSelf.queueName)":               "queueName is immutable and cannot be added or removed after creation",
 		"has(self.suspendable) == has(oldSelf.suspendable) && (!has(self.suspendable) || self.suspendable == oldSelf.suspendable)":     "suspendable is immutable and cannot be added or removed after creation",
+		"self.dataClass != 'internal' || !has(self.loop) || !has(self.loop.checks) || !self.loop.checks":                               "an internal run runs no repository checks",
+		"has(self.loop) == has(oldSelf.loop) && (!has(self.loop) || self.loop == oldSelf.loop)":                                        "loop is immutable and cannot be added or removed after creation",
 	} {
 		if m, ok := got[rule]; !ok || m != msg {
 			t.Errorf("spec rule %q: message %q, want %q", rule, m, msg)
 		}
 	}
-	if len(spec.XValidations) != 7 {
-		t.Errorf("spec has %d rules, want 7", len(spec.XValidations))
+	if len(spec.XValidations) != 9 {
+		t.Errorf("spec has %d rules, want 9", len(spec.XValidations))
 	}
 	immutable := func(name string, p apiextensionsv1.JSONSchemaProps) {
 		for _, r := range p.XValidations {
diff --git a/api/v1alpha1/agentrun_types.go b/api/v1alpha1/agentrun_types.go
index a37d05a..1774f84 100644
--- a/api/v1alpha1/agentrun_types.go
+++ b/api/v1alpha1/agentrun_types.go
@@ -38,6 +38,8 @@ type AgentRunList struct {
 // +kubebuilder:validation:XValidation:rule="has(self.roomRef) == has(oldSelf.roomRef) && (!has(self.roomRef) || self.roomRef == oldSelf.roomRef)",message="roomRef is immutable and cannot be added or removed after creation"
 // +kubebuilder:validation:XValidation:rule="has(self.queueName) == has(oldSelf.queueName) && (!has(self.queueName) || self.queueName == oldSelf.queueName)",message="queueName is immutable and cannot be added or removed after creation"
 // +kubebuilder:validation:XValidation:rule="has(self.suspendable) == has(oldSelf.suspendable) && (!has(self.suspendable) || self.suspendable == oldSelf.suspendable)",message="suspendable is immutable and cannot be added or removed after creation"
+// +kubebuilder:validation:XValidation:rule="self.dataClass != 'internal' || !has(self.loop) || !has(self.loop.checks) || !self.loop.checks",message="an internal run runs no repository checks"
+// +kubebuilder:validation:XValidation:rule="has(self.loop) == has(oldSelf.loop) && (!has(self.loop) || self.loop == oldSelf.loop)",message="loop is immutable and cannot be added or removed after creation"
 type AgentRunSpec struct {
 	// +kubebuilder:validation:Enum=implementer;reviewer;tester;triager
 	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="role is immutable"
@@ -95,6 +97,16 @@ type AgentRunSpec struct {
 	QueueName string `json:"queueName,omitempty"`
 	// +optional
 	Suspendable bool `json:"suspendable,omitempty"`
+	// +optional
+	Loop AgentRunLoop `json:"loop,omitzero"`
+}
+
+// AgentRunLoop is the run's loop policy, fixed at creation (v3: the loop policy's claim object).
+type AgentRunLoop struct {
+	// Checks runs the repository's checks in the sandbox (v3 feature 3): the harness gets
+	// AGENT_CHECKS=1 and the pod's deadline as AGENT_RUN_SECONDS. A public run only.
+	// +optional
+	Checks bool `json:"checks,omitempty"`
 }
 
 // AgentRunTask is exactly one of text or url.
diff --git a/api/v1alpha1/zz_generated.deepcopy.go b/api/v1alpha1/zz_generated.deepcopy.go
index 7a7c3ae..8cbbed8 100644
--- a/api/v1alpha1/zz_generated.deepcopy.go
+++ b/api/v1alpha1/zz_generated.deepcopy.go
@@ -105,12 +105,28 @@ func (in *AgentRunList) DeepCopyObject() runtime.Object {
 	return nil
 }
 
+// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
+func (in *AgentRunLoop) DeepCopyInto(out *AgentRunLoop) {
+	*out = *in
+}
+
+// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new AgentRunLoop.
+func (in *AgentRunLoop) DeepCopy() *AgentRunLoop {
+	if in == nil {
+		return nil
+	}
+	out := new(AgentRunLoop)
+	in.DeepCopyInto(out)
+	return out
+}
+
 // DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
 func (in *AgentRunSpec) DeepCopyInto(out *AgentRunSpec) {
 	*out = *in
 	out.Task = in.Task
 	out.Budget = in.Budget
 	in.Egress.DeepCopyInto(&out.Egress)
+	out.Loop = in.Loop
 }
 
 // DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new AgentRunSpec.
diff --git a/config/crd/agents.ogenki.io_agentruns.yaml b/config/crd/agents.ogenki.io_agentruns.yaml
index df29f8b..bde1835 100644
--- a/config/crd/agents.ogenki.io_agentruns.yaml
+++ b/config/crd/agents.ogenki.io_agentruns.yaml
@@ -127,6 +127,16 @@ spec:
                 x-kubernetes-validations:
                 - message: harness is immutable
                   rule: self == oldSelf
+              loop:
+                description: 'AgentRunLoop is the run''s loop policy, fixed at creation
+                  (v3: the loop policy''s claim object).'
+                properties:
+                  checks:
+                    description: |-
+                      Checks runs the repository's checks in the sandbox (v3 feature 3): the harness gets
+                      AGENT_CHECKS=1 and the pod's deadline as AGENT_RUN_SECONDS. A public run only.
+                    type: boolean
+                type: object
               model:
                 default: agent-default
                 description: A gateway tier on cloud-native-ref; the run-controller
@@ -229,6 +239,12 @@ spec:
                 creation
               rule: has(self.suspendable) == has(oldSelf.suspendable) && (!has(self.suspendable)
                 || self.suspendable == oldSelf.suspendable)
+            - message: an internal run runs no repository checks
+              rule: self.dataClass != 'internal' || !has(self.loop) || !has(self.loop.checks)
+                || !self.loop.checks
+            - message: loop is immutable and cannot be added or removed after creation
+              rule: has(self.loop) == has(oldSelf.loop) && (!has(self.loop) || self.loop
+                == oldSelf.loop)
           status:
             description: AgentRunStatus is what runwatch and the factory read (C3),
               as the composition writes it.
diff --git a/internal/runctl/checks_test.go b/internal/runctl/checks_test.go
new file mode 100644
index 0000000..e9f761c
--- /dev/null
+++ b/internal/runctl/checks_test.go
@@ -0,0 +1,46 @@
+// SPDX-License-Identifier: Apache-2.0
+
+package runctl
+
+import (
+	"slices"
+	"testing"
+
+	corev1 "k8s.io/api/core/v1"
+
+	v1alpha1 "github.com/Smana/agent-platform/api/v1alpha1"
+)
+
+func harnessEnv(p *corev1.Pod) map[string]string {
+	out := map[string]string{}
+	for _, e := range p.Spec.Containers[0].Env {
+		out[e.Name] = e.Value
+	}
+	return out
+}
+
+// v3 feature 3: spec.loop.checks hands the harness AGENT_CHECKS=1 and the pod's deadline; without it
+// the pod is today's.
+func TestLoopChecksReachTheHarness(t *testing.T) {
+	on := run("run-abcdefgh", func(r *v1alpha1.AgentRun) { r.Spec.Loop.Checks = true; r.Spec.Budget.MaxMinutes = 45 })
+	env := harnessEnv(Pod(on, testConfig(t), testModel, "abcdefgh"))
+	if env["AGENT_CHECKS"] != "1" || env["AGENT_RUN_SECONDS"] != "2700" {
+		t.Fatalf("AGENT_CHECKS %q AGENT_RUN_SECONDS %q, want 1 and 2700", env["AGENT_CHECKS"], env["AGENT_RUN_SECONDS"])
+	}
+	off := harnessEnv(Pod(run("run-abcdefgh"), testConfig(t), testModel, "abcdefgh"))
+	for _, k := range []string{"AGENT_CHECKS", "AGENT_RUN_SECONDS"} {
+		if _, ok := off[k]; ok {
+			t.Fatalf("%s set on a run without checks", k)
+		}
+	}
+}
+
+// RC7: a GitHub release download lands on release-assets; Go's archive is on dl.google.com.
+func TestTheDefaultProfilesReachTheChecksHosts(t *testing.T) {
+	fqdns := testConfig(t).FQDNs(goRun())
+	for _, h := range []string{"release-assets.githubusercontent.com", "dl.google.com"} {
+		if !slices.Contains(fqdns, h) {
+			t.Fatalf("%s missing from %v", h, fqdns)
+		}
+	}
+}
diff --git a/internal/runctl/config.go b/internal/runctl/config.go
index 8526b97..23bf4bf 100644
--- a/internal/runctl/config.go
+++ b/internal/runctl/config.go
@@ -52,12 +52,13 @@ type Images struct {
 	Bridge  string `json:"bridge"`
 }
 
-// defaultProfiles are the composition's _EGRESS_PROFILES (crossplane-configuration v0.9.15).
+// defaultProfiles are the composition's _EGRESS_PROFILES. v3 feature 3 (RC7) adds GitHub's release
+// host, where a release download lands (302), and Go's archive host.
 var defaultProfiles = map[string][]string{
-	"github": {"github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "raw.githubusercontent.com"},
+	"github": {"github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "raw.githubusercontent.com", "release-assets.githubusercontent.com"},
 	"pypi":   {"pypi.org", "files.pythonhosted.org"},
 	"npm":    {"registry.npmjs.org"},
-	"golang": {"proxy.golang.org", "sum.golang.org"},
+	"golang": {"proxy.golang.org", "sum.golang.org", "dl.google.com"},
 	"crates": {"index.crates.io", "static.crates.io"},
 }
 
diff --git a/internal/runctl/config_test.go b/internal/runctl/config_test.go
index 423577a..ffae735 100644
--- a/internal/runctl/config_test.go
+++ b/internal/runctl/config_test.go
@@ -60,7 +60,7 @@ func run(name string, mut ...func(*v1alpha1.AgentRun)) *v1alpha1.AgentRun {
 
 func TestLoadConfigDefaultsTheEgressProfiles(t *testing.T) {
 	c := testConfig(t)
-	if got := c.EgressProfiles["github"]; len(got) != 5 || got[0] != "github.com" {
+	if got := c.EgressProfiles["github"]; len(got) != 6 || got[0] != "github.com" {
 		t.Fatalf("github profile %v", got)
 	}
 	for _, p := range []string{"pypi", "npm", "golang", "crates"} {
diff --git a/internal/runctl/pod.go b/internal/runctl/pod.go
index b773289..a4aceaf 100644
--- a/internal/runctl/pod.go
+++ b/internal/runctl/pod.go
@@ -5,6 +5,7 @@ package runctl
 import (
 	"fmt"
 	"net/url"
+	"strconv"
 
 	corev1 "k8s.io/api/core/v1"
 	"k8s.io/apimachinery/pkg/api/resource"
@@ -105,6 +106,11 @@ func Pod(r *v1alpha1.AgentRun, c Config, m Model, runID string) *corev1.Pod {
 	if s.Task.URL != "" {
 		env = append(env, corev1.EnvVar{Name: "TASK_URL", Value: s.Task.URL})
 	}
+	// v3 feature 3: the repository's checks, and the pod's deadline for the final run's margin (RC9).
+	if s.Loop.Checks {
+		env = append(env, corev1.EnvVar{Name: "AGENT_CHECKS", Value: "1"},
+			corev1.EnvVar{Name: "AGENT_RUN_SECONDS", Value: strconv.FormatInt(deadline, 10)})
+	}
 	harness := corev1.Container{
 		Name: "harness", Image: c.Images.Harness, Env: env,
 		StartupProbe:    &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: localGet("/ready")}}, PeriodSeconds: 5, FailureThreshold: 60, TimeoutSeconds: 5},
diff --git a/internal/runctl/testdata/cnp.golden.yaml b/internal/runctl/testdata/cnp.golden.yaml
index 4ec3f7c..0ad4079 100644
--- a/internal/runctl/testdata/cnp.golden.yaml
+++ b/internal/runctl/testdata/cnp.golden.yaml
@@ -26,8 +26,10 @@ spec:
         - matchName: codeload.github.com
         - matchName: objects.githubusercontent.com
         - matchName: raw.githubusercontent.com
+        - matchName: release-assets.githubusercontent.com
         - matchName: proxy.golang.org
         - matchName: sum.golang.org
+        - matchName: dl.google.com
         - matchName: api.anthropic.com
         - matchName: room-broker.agent-platform.svc.cluster.local
   - toFQDNs:
@@ -36,8 +38,10 @@ spec:
     - matchName: codeload.github.com
     - matchName: objects.githubusercontent.com
     - matchName: raw.githubusercontent.com
+    - matchName: release-assets.githubusercontent.com
     - matchName: proxy.golang.org
     - matchName: sum.golang.org
+    - matchName: dl.google.com
     - matchName: api.anthropic.com
     toPorts:
     - ports:
```

  The CRD and deepcopy hunks are generated: run `task crd:gen`, then
  `go test ./internal/runctl/ -update` and review that `cnp.golden.yaml` gains exactly the two
  hosts. Run `gofmt -w api/v1alpha1/agentrun_crd_test.go` (the map's alignment).
- [ ] **Step 4:** `task check` → exit 0 (`crd:check` included).
- [ ] **Step 5: Commit**

```bash
git add api/v1alpha1 config/crd internal/runctl
git commit -m "feat(runctl): spec.loop.checks and the two checks hosts in the default profiles"
```

### Task 5: The container suite (offline proof 4) and its CI job

**Files:** Create `images/agent-harness/tests/checks/run.sh` (mode 0755) and
`images/agent-harness/tests/checks/proxy.py`; modify `.github/workflows/ci.yaml`, `taskfile.yaml`.

- [ ] **Step 1:** `images/agent-harness/tests/checks/proxy.py`:

```python
#!/usr/bin/env python3
"""A CONNECT proxy that allows exactly the hosts in ALLOW (comma-separated) on 443 and logs each
CONNECT as `allow <host>` or `refuse <host>` to the file named by LOG. Stands in for the run's CNP."""
import asyncio
import os
import sys

ALLOW = set(filter(None, os.environ["ALLOW"].split(",")))
LOG = open(os.environ["LOG"], "a", buffering=1)


async def pipe(r, w):
    try:
        while data := await r.read(65536):
            w.write(data)
            await w.drain()
    except Exception:
        pass
    finally:
        w.close()


async def handle(r, w):
    line = (await r.readline()).decode(errors="replace")
    while (await r.readline()) not in (b"\r\n", b""):
        pass
    parts = line.split()
    if len(parts) < 2 or parts[0] != "CONNECT":
        LOG.write("refuse non-connect %s\n" % line.strip())
        w.write(b"HTTP/1.1 405 Method Not Allowed\r\n\r\n")
        w.close()
        return
    host, _, port = parts[1].rpartition(":")
    if host not in ALLOW or port != "443":
        LOG.write("refuse %s:%s\n" % (host, port))
        w.write(b"HTTP/1.1 403 Forbidden\r\n\r\n")
        w.close()
        return
    LOG.write("allow %s\n" % host)
    try:
        ur, uw = await asyncio.open_connection(host, 443)
    except OSError as exc:
        LOG.write("error %s %s\n" % (host, exc))
        w.write(b"HTTP/1.1 502 Bad Gateway\r\n\r\n")
        w.close()
        return
    w.write(b"HTTP/1.1 200 Connection established\r\n\r\n")
    await w.drain()
    await asyncio.gather(pipe(r, uw), pipe(ur, w))


async def main():
    server = await asyncio.start_server(handle, "127.0.0.1", int(sys.argv[1]))
    async with server:
        await server.serve_forever()


asyncio.run(main())
```

- [ ] **Step 2:** `images/agent-harness/tests/checks/run.sh`:

```bash
#!/usr/bin/env bash
# v3 feature 3, offline proof 4: the harness image under gVisor (runsc), on a network whose only way
# out is a proxy allowing exactly a check-enabled run's hosts, clones REPO's main and runs its
# declared checks. Fails on a refused host (named), on a step that ends fail or timeout, and on an
# install that leaves a tool missing. Records the install's and each step's seconds and mise's
# GitHub API calls in $GITHUB_STEP_SUMMARY when set.
#
# Usage: run.sh IMAGE OWNER/REPO PROFILES TOKEN_FILE   e.g. run.sh agent-harness:checks Smana/ai-native-ref golang,pypi /tmp/token
# PROFILES are the repository's checks.egress (the factory config); github is always on.
set -euo pipefail
image=$1 repo=$2 profiles=$3 token=$4
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# The composition's _EGRESS_PROFILES, the two v3 hosts included; runctl's defaultProfiles are the same.
declare -A HOSTS=(
  [github]="github.com api.github.com codeload.github.com objects.githubusercontent.com raw.githubusercontent.com release-assets.githubusercontent.com"
  [pypi]="pypi.org files.pythonhosted.org"
  [npm]="registry.npmjs.org"
  [golang]="proxy.golang.org sum.golang.org dl.google.com"
  [crates]="index.crates.io static.crates.io"
)

if ! curl -fsS -o /dev/null "https://raw.githubusercontent.com/${repo}/HEAD/.github/agent-checks.yaml"; then
  echo "SKIP ${repo}: its main declares no checks yet"
  exit 0
fi

allow=${HOSTS[github]}
for p in ${profiles//,/ }; do
  [ -n "${HOSTS[$p]:-}" ] || { echo "unknown profile $p" >&2; exit 2; }
  allow="$allow ${HOSTS[$p]}"
done

name="checks-$(echo "$repo" | tr '/A-Z' '-a-z')"
out="$(mktemp -d)"
cleanup() {
  docker rm -f "$name-proxy" >/dev/null 2>&1 || true
  docker network rm "$name" >/dev/null 2>&1 || true
  docker volume rm "$name" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup
docker network create --internal "$name" >/dev/null
docker volume create "$name" >/dev/null
chmod 0777 "$out"

# The proxy: the image's own Python, on the default bridge (the way out) and on the internal network.
docker run -d --name "$name-proxy" --network bridge -e ALLOW="${allow// /,}" -e LOG=/out/proxy.log \
  -v "$here/proxy.py:/proxy.py:ro" -v "$out:/out" --entrypoint /usr/local/bin/python "$image" /proxy.py 3128 >/dev/null
docker network connect "$name" "$name-proxy"

# /workspace as the composition gives it: writable by uid 10001, the root filesystem read-only.
docker run --rm --user 0 -v "$name:/workspace" --entrypoint chown "$image" 10001:10001 /workspace

docker run --rm --runtime=runsc --network "$name" --read-only --cpus 2 --memory 4g \
  --tmpfs /tmp:size=2g,mode=1777 --tmpfs /home/openhands:size=2g,uid=10001,gid=10001 \
  -v "$name:/workspace" -v "$out:/out" -v "$token:/run/agent/github/token:ro" \
  -e HOME=/home/openhands -e HTTPS_PROXY="http://$name-proxy:3128" -e GIT_TOKEN_FILE=/run/agent/github/token \
  -e MISE_LOG_LEVEL=debug -e REPO="$repo" \
  --entrypoint bash "$image" -c '
    set -u
    dmesg 2>/dev/null | head -1 > /out/kernel
    git clone --quiet "https://github.com/$REPO.git" /workspace/repo || exit 3
    s=$SECONDS; agent-checks --install > /out/install.out 2>&1; echo $? > /out/install.exit; echo $((SECONDS - s)) > /out/install.seconds
    agent-checks > /out/checks.out 2>&1; echo $? > /out/checks.exit
    cp /workspace/.agent-checks/*.json /workspace/.agent-checks/install.log /out/ 2>/dev/null || true'

fail=0
grep -qi gvisor "$out/kernel" || { echo "FAIL ${repo}: the container did not run under gVisor ($(cat "$out/kernel"))"; fail=1; }
if grep '^refuse' "$out/proxy.log" | sort -u | grep .; then
  echo "FAIL ${repo}: the checks reached hosts beyond the run's egress (above)"; fail=1
fi
[ "$(cat "$out/install.exit")" = 0 ] || { echo "FAIL ${repo}: the install left a tool missing"; cat "$out/install.out"; fail=1; }
cat "$out/checks.out"
python3 - "$out" "$repo" <<'EOF' || fail=1
import glob, json, os, re, sys
out, repo = sys.argv[1:3]
res = [json.load(open(p)) for p in glob.glob(os.path.join(out, "*.json")) if ".step-" not in p]
assert len(res) == 1, "want one result, got %d" % len(res)
steps = res[0]["steps"]
bad = [s["name"] for s in steps if s["result"] not in ("pass", "skip")]
api = len(re.findall(r"GET https://api\.github\.com\S*$", open(os.path.join(out, "install.log")).read(), re.M))
rows = ["### %s: install %ss, %d GitHub API calls" % (repo, open(os.path.join(out, "install.seconds")).read().strip(), api),
        "", "| step | result | seconds |", "|---|---|---|"] + ["| %s | %s | %s |" % (s["name"], s["result"], s["seconds"]) for s in steps]
if os.environ.get("GITHUB_STEP_SUMMARY"):
    open(os.environ["GITHUB_STEP_SUMMARY"], "a").write("\n".join(rows) + "\n\n")
print("\n".join(rows))
if bad:
    sys.exit("FAIL %s: %s did not pass" % (repo, ", ".join(bad)))
EOF
exit "$fail"
```

- [ ] **Step 3:** `taskfile.yaml`, after `harness:test`:

```yaml
  harness:checks:
    desc: "v3 feature 3, offline proof 4: the harness under runsc runs both repositories' checks behind an allowlist proxy (docker with runsc; TOKEN_FILE=<a GitHub token file>)"
    cmds:
      - docker build -t agent-harness:checks images/agent-harness
      - images/agent-harness/tests/checks/run.sh agent-harness:checks Smana/ai-native-ref golang,pypi {{.TOKEN_FILE}}
      - images/agent-harness/tests/checks/run.sh agent-harness:checks Smana/cloud-native-ref pypi {{.TOKEN_FILE}}
```

- [ ] **Step 4:** `.github/workflows/ci.yaml`, a job after `harness-test` (not required, R21):

```yaml
  # v3 feature 3, offline proof 4: the harness image under gVisor, behind a proxy allowing exactly a
  # check-enabled run's hosts, runs both repositories' declared checks on their main. Not required:
  # it reads two other repositories and the network. A repository that declares nothing yet skips.
  harness-checks:
    runs-on: ubuntu-latest
    timeout-minutes: 45
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5.1.0
        with:
          persist-credentials: false
      - name: Install gVisor (release-20261005.0)
        run: |
          set -eux
          curl -fsSLo /tmp/gvisor.tar.zstd https://storage.googleapis.com/gvisor/releases/release/20261005.0/x86_64/gvisor.tar.zstd
          echo "27fed731b432d6a39d908956623fd273979fda66b3ccf4b9c12413ea417c516e18a9dc3cced403fb28efe495d64dce4251f51cf8c4d9f4a72f9833bd96b6bb35  /tmp/gvisor.tar.zstd" | sha512sum -c -
          mkdir /tmp/gvisor && tar --zstd -xf /tmp/gvisor.tar.zstd -C /tmp/gvisor
          sudo install -m 0755 "$(find /tmp/gvisor -name runsc -type f | head -1)" /usr/local/bin/runsc
          sudo runsc install
          sudo systemctl restart docker
      - uses: docker/setup-buildx-action@8d2750c68a42422c14e847fe6c8ac0403b4cbd6f # v3.12.0
      - uses: docker/build-push-action@10e90e3645eae34f1e60eeb005ba3a3d33f178e8 # v6.19.2
        with:
          context: images/agent-harness
          load: true
          push: false
          tags: agent-harness:checks
      - name: The run's token, a file as the run-controller mounts it
        env:
          GH_TOKEN: ${{ github.token }}
        run: install -m 0644 /dev/null "$RUNNER_TEMP/token" && printf '%s' "$GH_TOKEN" > "$RUNNER_TEMP/token"
      - run: images/agent-harness/tests/checks/run.sh agent-harness:checks Smana/ai-native-ref golang,pypi "$RUNNER_TEMP/token"
      - run: images/agent-harness/tests/checks/run.sh agent-harness:checks Smana/cloud-native-ref pypi "$RUNNER_TEMP/token"
```

- [ ] **Step 5:** `shellcheck -x -S warning images/agent-harness/tests/checks/run.sh` → clean; `task check` → exit 0.
- [ ] **Step 6: Commit**

```bash
git add images/agent-harness/tests/checks .github/workflows/ci.yaml taskfile.yaml
git commit -m "test(harness): the checks under runsc behind an allowlist proxy (offline proof 4)"
```

- [ ] **Step 7: Pull request.** Push, open it; CI's `harness-checks` prints `SKIP` for both
  repositories until PRs 2 and 3 merge. Record the image's compressed size against v0.15.2's
  1,305 MB in the description.

---

## PR 2 — cloud-native-ref (the host)

A worktree from `origin/main`. Every file below is a gate path or under one: a human authors and
merges it.

### Task 6: ShellCheck from mise, `task ci:shellcheck`

**Files:** `mise.toml`, `.github/workflows/ci.yaml` (job `shellcheck`), `scripts/tasks.yaml`, `taskfile.yaml`.

- [ ] **Step 1:** `mise.toml`, after `kustomize`:

```toml
# ShellCheck for "Check the shell scripts" and an agent's sandbox checks (agent-platform v3 feature 3):
# one pin for both, which the apt package was not.
shellcheck = "0.11.0"
```

- [ ] **Step 2:** In the `shellcheck` job, replace the `Install ShellCheck` step with:

```yaml
      - name: Install ShellCheck
        uses: jdx/mise-action@08ae2a091c038e1e05ec6f5115dc5c69d5a2a6b8 # v5
        with:
          install_args: shellcheck
```

  Keep the `Run ShellCheck` step and its command exactly (R16).
- [ ] **Step 3:** `scripts/tasks.yaml`:

```yaml
  shellcheck:
    desc: ShellCheck every script, symlinks included (the CI job's command; keep -type l)
    cmds:
      - find {{.ROOT_DIR}}/scripts \( -type f -o -type l \) -name "*.sh" -print0 | xargs -0 shellcheck -x -S warning
```

  and `- {task: "ci:shellcheck"}` in `taskfile.yaml`'s `check`, after `ci:test`.
- [ ] **Step 4:** `mise install shellcheck && task ci:shellcheck` → exit 0.
- [ ] **Step 5:** `git commit -m "ci: ShellCheck pinned in mise.toml, task ci:shellcheck"`

### Task 7: `ai-overlay.sh --from-git` (offline proof 2)

**Files:** Modify `scripts/ci/ai-overlay.sh`; create `scripts/ci/tests/test-ai-overlay-from-git.sh`.

- [ ] **Step 1: The failing suite** `scripts/ci/tests/test-ai-overlay-from-git.sh` (mode 0755):

```bash
#!/usr/bin/env bash
# requires: cosign flux git rsync python3
# ai-overlay.sh --from-git (agent-platform v3 feature 3, the checks in an agent's sandbox): it lays the
# AI paths from the pin's tag on GitHub, unverified. It must lay exactly the verified artifact's tree,
# refuse CI=true, and refuse a pre-release pin, which has no git tag.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# A throwaway repository holding what the overlay reads: the .gitignore, the pin and the script.
copy() {
  mkdir -p "$1"
  (cd "$ROOT" && cp --parents .gitignore flux/sources/ocirepo-ai-native-ref.yaml scripts/ci/ai-overlay.sh "$1/")
  git -C "$1" init -q && git -C "$1" add -A
}

copy "$tmp/ci"
if (cd "$tmp/ci" && CI=true ./scripts/ci/ai-overlay.sh --from-git) >"$tmp/out" 2>&1; then
  echo "FAIL: --from-git ran under CI=true"; exit 1
fi
grep -q "CI verifies the signed artifact" "$tmp/out" || { echo "FAIL: CI=true refused without its reason:"; cat "$tmp/out"; exit 1; }

copy "$tmp/pre"
sed -i 's|^    tag: .*|    tag: "v0.1.17-pr12.gdeadbeef"|' "$tmp/pre/flux/sources/ocirepo-ai-native-ref.yaml"
if (cd "$tmp/pre" && env -u CI ./scripts/ci/ai-overlay.sh --from-git) >"$tmp/out" 2>&1; then
  echo "FAIL: --from-git took a pre-release pin"; exit 1
fi
grep -q "needs a release tag" "$tmp/out" || { echo "FAIL: a pre-release pin refused without its reason:"; cat "$tmp/out"; exit 1; }

copy "$tmp/verified"
copy "$tmp/git"
(cd "$tmp/verified" && ./scripts/ci/ai-overlay.sh >/dev/null)
(cd "$tmp/git" && env -u CI ./scripts/ci/ai-overlay.sh --from-git >/dev/null)
if ! diff -r --exclude=.git "$tmp/verified" "$tmp/git" >"$tmp/diff"; then
  echo "FAIL: --from-git lays another tree than the verified artifact:"; head -20 "$tmp/diff"; exit 1
fi
echo "PASS: --from-git lays the verified artifact's $(cd "$tmp/git/.ai-artifact" && find . -type f | wc -l) files, refuses CI=true and a pre-release pin"
```

- [ ] **Step 2:** `bash scripts/ci/tests/test-ai-overlay-from-git.sh` → FAIL (`--from-git ran under CI=true`: today the flag is ignored).
- [ ] **Step 3: The change:**

```diff
diff --git a/scripts/ci/ai-overlay.sh b/scripts/ci/ai-overlay.sh
index 0198798..f1adfea 100755
--- a/scripts/ci/ai-overlay.sh
+++ b/scripts/ci/ai-overlay.sh
@@ -10,15 +10,35 @@
 #
 # Idempotent: the previous overlay's files are removed first, so a file dropped upstream does
 # not linger. Needs cosign, flux and python3 with PyYAML.
+#
+# --from-git lays the same paths from the pin's tag, cloned from GitHub and NOT verified: an agent's
+# sandbox reaches neither ghcr.io nor Sigstore (agent-platform v3 feature 3, the checks in the
+# sandbox). It is a feedback loop for the sandbox only, and refuses to run under CI=true: CI always
+# verifies the signed artifact. Needs git, python3 with PyYAML and rsync.
 set -euo pipefail
 
 cd "$(git rev-parse --show-toplevel)"
 
 PIN=flux/sources/ocirepo-ai-native-ref.yaml
 OUT=.ai-artifact
+# What ai-native-ref's scripts/ci/push-artifact.sh puts in the artifact, from the repository the
+# signer identity names. tests/test-ai-overlay-from-git.sh compares the two on the pinned tag.
+SOURCE=https://github.com/Smana/ai-native-ref
+ARTIFACT_DIRS="clusters infrastructure security tooling observability apps flux"
 
 die() { echo "ai-overlay: $*" >&2; exit 1; }
-for bin in cosign flux python3 rsync; do
+from_git=false
+case "${1:-}" in
+  "") ;;
+  --from-git) from_git=true ;;
+  *) die "usage: $0 [--from-git]" ;;
+esac
+if $from_git && [ "${CI:-}" = true ]; then
+  die "--from-git lays an unverified tree; CI verifies the signed artifact (run it without --from-git)"
+fi
+tools="cosign flux python3 rsync"
+if $from_git; then tools="git python3 rsync"; fi
+for bin in $tools; do
   command -v "$bin" >/dev/null || die "$bin not found on PATH (mise install)"
 done
 
@@ -37,18 +57,24 @@ print(spec["url"])
 print(spec["ref"]["digest"])
 print(issuers.pop())
 print("|".join(f"(?:{i['subject']})" for i in ids))
+print(spec["ref"].get("tag", ""))
 EOF
 )
-[ "${#pin[@]}" -eq 4 ] || die "cannot read the pin from ${PIN}"
-url=${pin[0]} digest=${pin[1]} issuer=${pin[2]} subjects=${pin[3]}
+[ "${#pin[@]}" -ge 4 ] || die "cannot read the pin from ${PIN}"
+url=${pin[0]} digest=${pin[1]} issuer=${pin[2]} subjects=${pin[3]} tag=${pin[4]:-}
 [[ $url == oci://* ]] || die "url ${url} is not oci://"
 [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || die "digest ${digest} is not a sha256 digest"
 ref="${url#oci://}@${digest}"
 
-echo "==> Verifying ${ref}"
-cosign verify "$ref" \
-  --certificate-identity-regexp "$subjects" \
-  --certificate-oidc-issuer-regexp "$issuer" >/dev/null
+if $from_git; then
+  # A release tag only: a pre-release pin (v<x>-pr<N>.g<sha8>) is an OCI tag with no git tag.
+  [[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "--from-git needs a release tag in ${PIN}, got '${tag}'"
+else
+  echo "==> Verifying ${ref}"
+  cosign verify "$ref" \
+    --certificate-identity-regexp "$subjects" \
+    --certificate-oidc-issuer-regexp "$issuer" >/dev/null
+fi
 
 if [ -d "$OUT" ]; then
   echo "==> Removing the previous overlay"
@@ -56,9 +82,21 @@ if [ -d "$OUT" ]; then
   rm -rf "$OUT"
 fi
 
-echo "==> Pulling ${url}@${digest}"
 mkdir -p "$OUT"
-flux pull artifact "${url}@${digest}" -o "$OUT" >/dev/null
+if $from_git; then
+  echo "==> Cloning ${SOURCE} at ${tag} (unverified)"
+  src="$(mktemp -d)"
+  trap 'rm -rf "$src"' EXIT
+  git -c advice.detachedHead=false clone --quiet --depth 1 --branch "$tag" "$SOURCE" "$src"
+  for d in $ARTIFACT_DIRS; do
+    if [ -d "$src/$d" ]; then cp -a "$src/$d" "$OUT/"; fi
+  done
+  from="${tag} (git, unverified)"
+else
+  echo "==> Pulling ${url}@${digest}"
+  flux pull artifact "${url}@${digest}" -o "$OUT" >/dev/null
+  from="$digest"
+fi
 
 # Every file must land on an ignored path: anything else would overwrite a host file.
 files=$(cd "$OUT" && find . -type f | sed 's|^\./||' | sort)
@@ -68,4 +106,4 @@ outside=$(comm -23 <(echo "$files") <(echo "$files" | git check-ignore --no-inde
 ${outside}"
 
 rsync -a "$OUT/" ./
-echo "==> Overlaid $(echo "$files" | wc -l) files from ${digest}"
+echo "==> Overlaid $(echo "$files" | wc -l) files from ${from}"
```

- [ ] **Step 4:** `CI=true bash scripts/ci/tests/test-ai-overlay-from-git.sh` → `PASS: --from-git lays the verified artifact's 328 files, refuses CI=true and a pre-release pin`; `task ci:shellcheck` → clean.
- [ ] **Step 5:** `git commit -m "feat(ci): ai-overlay.sh --from-git, the AI paths from the pin's tag for an agent's sandbox"`

### Task 8: The declaration, the coverage suite, the sentinel (offline proof 1)

**Files:** Create `.github/agent-checks.yaml`, `scripts/ci/tests/test-agent-checks.py`; modify
`scripts/ci/check-policy-gate-coverage.sh` (SENTINELS), `scripts/ci/tests/flux-schema/test-render-bundle.py`.

- [ ] **Step 1:** Create `scripts/ci/tests/test-agent-checks.py` with [Appendix A](#appendix-a-the-coverage-suite)'s content. Run `python3 scripts/ci/tests/test-agent-checks.py` → `FAIL .github/agent-checks.yaml: missing`.
- [ ] **Step 2:** `.github/agent-checks.yaml`:

```yaml
# .github/agent-checks.yaml: what an agent runs before it pushes, the CI's fast subset
# (agent-platform v3 feature 3). A gate path. The harness reads it from the default branch, never
# from the agent's branch. Every job of a pull_request workflow is a step's ci or a ciOnly entry
# (scripts/ci/tests/test-agent-checks.py).
version: 1
timeoutMinutes: 8
tools: [task, promtool, kustomize, pre-commit, shellcheck]
definedBy: [taskfile.yaml, scripts/tasks.yaml, scripts/ci/, .pre-commit-config.yaml, mise.toml, flux/sources/ocirepo-ai-native-ref.yaml]
steps:
  - {name: overlay, run: ./scripts/ci/ai-overlay.sh --from-git, ci: "Kubernetes validation ☸", stopOnFailure: true, timeoutMinutes: 3}
  - {name: suites, run: task ci:test, ci: "Kubernetes validation ☸"}
  - {name: gate-invariants, run: ./scripts/ci/check-policy-gate-coverage.sh && ./scripts/ci/check-workflow-secrets.sh, ci: "Kubernetes validation ☸"}
  - {name: vmrules, run: ./scripts/ci/validate-vmrules.sh, ci: "Kubernetes validation ☸"}
  - {name: shellcheck, run: "find ./scripts \\( -type f -o -type l \\) -name '*.sh' -print0 | xargs -0 shellcheck -x -S warning", ci: "Check the shell scripts 💻"}
  - {name: links, run: ./scripts/ci/validate-links.sh, ci: "Check the documentation links 🔗"}
  - {name: doc-claims, run: ./scripts/ci/validate-doc-claims.sh, ci: "Check the documentation links 🔗"}
  - {name: idp-topology, run: ./scripts/ci/validate-idp-topology.sh, ci: "Check the documentation links 🔗"}
  - {name: openbao-policies, run: ./scripts/ci/validate-openbao-policies.sh, ci: "Check the documentation links 🔗"}
  - {name: doc-paths, run: ./scripts/ci/verify-doc-paths.sh, ci: "Build the documentation site 📚"}
  - {name: pre-commit, run: "SKIP=terraform_fmt,terraform_validate,terraform_tflint pre-commit run --all-files --show-diff-on-failure", ci: "Pre-commit checks 🛃"}
ciOnly:
  - {check: "Pre-commit checks 🛃", part: "the terraform hooks, the OpenBao module test", why: "OpenTofu provider and tflint plugin downloads beyond the egress"}
  - {check: "Kubernetes validation ☸", part: "task ci:validate (the render, flux schema, polaris)", why: "about 20 chart hosts beyond the egress"}
  - {check: "Kubernetes validation ☸", part: "the Docker suites", why: "no Docker daemon under gVisor"}
  - {check: "Build the documentation site 📚", part: "the Hugo build", why: "Go's archive is on dl.google.com, beyond this repository's egress"}
  - {check: "Security scanning 🔒", part: all, why: "Trivy's database, Checkov, TruffleHog's verification"}
  - {check: "Upload SARIF results", part: all, why: "security-events: write, no checkout"}
  - {check: "Rendered manifest diff 📝", part: all, why: "a render, and a diff against main"}
  - {check: "Post the rendered diff comment", part: all, why: "pull-requests: write, no checkout"}
  - {check: "Report a broken main 🚨", part: all, why: "runs on push to main only"}
  - {check: "Validate Vector Log Parsing Configuration", part: "both variants", why: "Docker"}
  - {check: "check-container-images/detect-changes", part: all, why: "builds container images: Docker"}
  - {check: "check-container-images/build", part: all, why: "builds container images: Docker"}
  - {check: "check-container-images/build-summary", part: all, why: "builds container images: Docker"}
```

- [ ] **Step 3:** In `check-policy-gate-coverage.sh`'s `SENTINELS`, add `.github/agent-checks.yaml`
  after `.github/agent-factory.yaml`. In `test-render-bundle.py`, add `# requires: python3 helm` as
  line 2 (R17).
- [ ] **Step 4:** `python3 scripts/ci/tests/test-agent-checks.py` → `PASS 11 steps, 13 CI-only entries, 14 pull_request jobs covered`; `task check` and `task ci:test` → exit 0.
- [ ] **Step 5:** `git commit -m "feat(ci): the sandbox checks' declaration and its coverage suite"`; push, open the PR.

---

## PR 3 — ai-native-ref

### Task 9: The declaration, the coverage suite, the sentinel (offline proof 1)

**Files:** Create `.github/agent-checks.yaml`, `scripts/ci/tests/test-agent-checks.py`; modify
`scripts/ci/tests/test-agent-repo-files.py`.

- [ ] **Step 1:** `scripts/ci/tests/test-agent-checks.py` with [Appendix A](#appendix-a-the-coverage-suite)'s content; `bash $SCRATCH/run-suite.sh python3 scripts/ci/tests/test-agent-checks.py` → FAIL (missing).
- [ ] **Step 2:** `.github/agent-checks.yaml`, the design's declaration:

```yaml
# .github/agent-checks.yaml: what an agent runs before it pushes, the CI's fast subset.
# A gate path. The harness reads it from the base branch, never from the agent's branch.
version: 1
timeoutMinutes: 8
tools: [task, promtool, kustomize, hugo-extended, go, pre-commit]
definedBy: [taskfile.yaml, scripts/ci/, .pre-commit-config.yaml, mise.toml, HOST_REF]
steps:
  - {name: composite, run: task host-profile, ci: validate, stopOnFailure: true, timeoutMinutes: 3}
  - {name: vmrules, run: task vmrules, ci: validate}
  - {name: suites, run: task test, ci: validate}
  - {name: links, run: task links, ci: validate}
  - {name: site, run: task docs, ci: validate}
  - {name: sources, run: task sources, ci: validate}
  - {name: pre-commit, run: pre-commit run --all-files --show-diff-on-failure, ci: lint}
ciOnly:
  - {check: validate, part: "task validate, task gates", why: "renders every HelmRelease: about 20 chart hosts beyond the egress"}
  - {check: validate, part: "the two Docker suites", why: "no Docker daemon under gVisor"}
  - {check: lint, part: TruffleHog, why: "an Action over the pushed range"}
  - {check: prerelease, part: all, why: "signs and pushes with the workflow's identity"}
```

- [ ] **Step 3:** In `test-agent-repo-files.py`'s `SENTINELS`, add `".github/agent-checks.yaml"`.
- [ ] **Step 4:** the suite → `PASS 7 steps, 4 CI-only entries, 3 pull_request jobs covered`; `bash $SCRATCH/check.sh <worktree>` → `exit=0`.
- [ ] **Step 5:** `git commit -m "feat(ci): the sandbox checks' declaration and its coverage suite"`; push, open the PR.

---

## PR 4 — agent-platform: the factory, then a release

Branch `feat/sandbox-checks-factory` from `origin/main` after PR 1 merges.

### Task 10: The config keys, the claim and the reconciler (offline proof 7)

**Files:** `internal/factory/config/config.go`, `internal/factory/config/checks_test.go`,
`internal/factory/api/server.go`, `internal/factory/runs/runs.go`, `runs_test.go`,
`internal/factory/reconciler/implement.go`, `team.go`, `checks_test.go`, `docs/factory.md`.

**Interfaces:** `config.EgressProfiles`, `config.Checks{Egress}`, `Repository.Checks`,
`Class.Checks`, `(*Repository).ChecksFor(class) (bool, []string)`, `runs.Spec.Checks`,
`(*Reconciler).withChecks(*runs.Spec, *v1alpha1.Task)`.

- [ ] **Step 1: The failing tests:** the `_test.go` hunks below. `go test ./internal/factory/...` → FAIL (`unknown field Checks`).
- [ ] **Step 2: The change** (`server.go`: the `profiles` var goes, `prURL` loses its alignment, the loop reads `config.EgressProfiles`):

```diff
diff --git a/internal/factory/api/server.go b/internal/factory/api/server.go
index 7eb8f37..72c902a 100644
--- a/internal/factory/api/server.go
+++ b/internal/factory/api/server.go
@@ -108,8 +108,7 @@ var (
 	roles  = []string{"implementer", "reviewer", "tester", "triager"}
 	models = []string{"agent-default", "tier-light", "tier-standard", "tier-frontier"}
 
-	profiles = []string{"pypi", "npm", "golang", "crates"}
-	prURL    = regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+/pull/[0-9]+$`)
+	prURL = regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+/pull/[0-9]+$`)
 )
 
 // Ledger layout (R50). One ConfigMap per UTC day, agent-factory-ledger-usd-<YYYYMMDD>, in the
@@ -428,7 +427,7 @@ func validate(in RunRequest) string {
 		return "internal_runs_take_no_egress_profiles"
 	}
 	for _, e := range in.EgressProfiles {
-		if !slices.Contains(profiles, e) {
+		if !slices.Contains(config.EgressProfiles, e) {
 			return "bad_egress_profile"
 		}
 	}
diff --git a/internal/factory/config/checks_test.go b/internal/factory/config/checks_test.go
new file mode 100644
index 0000000..f1d16be
--- /dev/null
+++ b/internal/factory/config/checks_test.go
@@ -0,0 +1,49 @@
+// SPDX-License-Identifier: Apache-2.0
+
+package config
+
+import (
+	"reflect"
+	"strings"
+	"testing"
+)
+
+// withChecks is the list-shaped good with v3 feature 3's keys: the repository's checks.egress and
+// the docs class switched on.
+func withChecks(t *testing.T, egress string) string {
+	t.Helper()
+	raw := listShaped(t, good)
+	raw = strings.Replace(raw, "  - name: Smana/cloud-native-ref\n", "  - name: Smana/cloud-native-ref\n    checks: {egress: "+egress+"}\n", 1)
+	return strings.Replace(raw, "docs: {}", "docs: {checks: true}", 1)
+}
+
+func TestChecksAreSwitchedOnPerClassWithTheRepositorysEgress(t *testing.T) {
+	c, err := Parse([]byte(withChecks(t, "[golang, pypi]")))
+	if err != nil {
+		t.Fatal(err)
+	}
+	r := c.RepositoryFor("Smana/cloud-native-ref")
+	if on, egress := r.ChecksFor("docs"); !on || !reflect.DeepEqual(egress, []string{"golang", "pypi"}) {
+		t.Fatalf("docs: %v %v, want on with [golang pypi]", on, egress)
+	}
+	for _, class := range []string{"tests", "review", "docs-links"} {
+		if on, egress := r.ChecksFor(class); on || egress != nil {
+			t.Fatalf("%s: %v %v, want off", class, on, egress)
+		}
+	}
+}
+
+func TestChecksEgressIsTheProfileEnumAtMostFourOnce(t *testing.T) {
+	for _, tc := range []struct{ egress, want string }{
+		{"[pypi, docker]", `"docker" is not one of`},
+		{"[pypi, pypi]", `"pypi" is not one of`},
+		{"[pypi, npm, golang, crates, pypi]", "at most 4 profiles"},
+	} {
+		if _, err := Parse([]byte(withChecks(t, tc.egress))); err == nil || !strings.Contains(err.Error(), tc.want) {
+			t.Fatalf("egress %s: %v, want %q", tc.egress, err, tc.want)
+		}
+	}
+	if _, err := Parse([]byte(strings.Replace(withChecks(t, "[pypi]"), "checks: {egress:", "checks: {hosts: [x], egress:", 1))); err == nil {
+		t.Fatal("an unknown checks key parsed: the decoder must refuse it")
+	}
+}
diff --git a/internal/factory/config/config.go b/internal/factory/config/config.go
index 459e5eb..d25fa2a 100644
--- a/internal/factory/config/config.go
+++ b/internal/factory/config/config.go
@@ -172,6 +172,26 @@ type Repository struct {
 	Classes      map[string]Class `json:"classes"`
 	Merge        Merge            `json:"merge"`
 	Schedules    []Schedule       `json:"schedules,omitempty"`
+	Checks       Checks           `json:"checks,omitzero"`
+}
+
+// EgressProfiles are the registry profiles a run may request on top of github: the AgentRun
+// XRD's enum, which the composition turns into FQDNs.
+var EgressProfiles = []string{"pypi", "npm", "golang", "crates"}
+
+// Checks is what a check-enabled run of the repository requests beyond github (v3 feature 3,
+// RC7): the platform names the egress, never the repository's own file.
+type Checks struct {
+	Egress []string `json:"egress,omitempty"`
+}
+
+// ChecksFor reports whether a run of class runs the repository's checks in its sandbox (RC8), and
+// the egress profiles it then requests. Absent or false, the run is today's.
+func (r *Repository) ChecksFor(class string) (bool, []string) {
+	if !r.Classes[class].Checks {
+		return false, nil
+	}
+	return true, slices.Clone(r.Checks.Egress)
 }
 
 // Control is the pinned issue whose factory/stop label is the global stop (§6.1). Issue 0: none,
@@ -306,6 +326,9 @@ type Triage struct {
 type Class struct {
 	Live   bool `json:"live,omitempty"`
 	Shadow bool `json:"shadow,omitempty"`
+	// Checks switches the repository's checks on for the class's public implementer and reviewer
+	// runs (v3 feature 3, RC8): the claim's spec.loop.checks and the repository's checks.egress.
+	Checks bool `json:"checks,omitempty"`
 }
 
 // Merge is §5.1's merge actor and §6.4's rollback.
@@ -1031,6 +1054,14 @@ func (c *Config) validateRepository(r *Repository, seenSchedule map[string]bool,
 	if _, ok := r.Classes["review"]; ok {
 		bad("review is the implicit class of everything else; do not declare it")
 	}
+	if len(r.Checks.Egress) > 4 {
+		bad("checks.egress: at most 4 profiles, as the claim takes")
+	}
+	for i, p := range r.Checks.Egress {
+		if !slices.Contains(EgressProfiles, p) || slices.Index(r.Checks.Egress, p) != i {
+			bad("checks.egress: %q is not one of %v, or is listed twice", p, EgressProfiles)
+		}
+	}
 	// §1 schedules. The name keys every task it starts, so names are unique across every
 	// repository and C2-shaped;
 	// the cron must parse to have slots at all.
diff --git a/internal/factory/reconciler/checks_test.go b/internal/factory/reconciler/checks_test.go
new file mode 100644
index 0000000..92d16cf
--- /dev/null
+++ b/internal/factory/reconciler/checks_test.go
@@ -0,0 +1,46 @@
+// SPDX-License-Identifier: Apache-2.0
+
+package reconciler
+
+import (
+	"reflect"
+	"testing"
+
+	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
+
+	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
+	"github.com/Smana/agent-platform/internal/factory/config"
+	"github.com/Smana/agent-platform/internal/factory/runs"
+)
+
+// v3 feature 3 (RC8, offline proof 7): a switched-on class's public implementer and reviewer claims
+// carry spec.loop.checks and the repository's egress; every other claim is today's.
+func TestASwitchedOnClassChecksItsPublicImplementerAndReviewer(t *testing.T) {
+	g := newRig(t)
+	repo := g.r.Cfg.Repos()[0]
+	repo.Classes = map[string]config.Class{"docs-links": {Shadow: true}, "revert": {}, "docs": {Checks: true}, "tests": {}}
+	repo.Checks = config.Checks{Egress: []string{"golang", "pypi"}}
+	g.r.Cfg.Repositories = []config.Repository{repo}
+	task := func(class, dataClass string) *v1alpha1.Task {
+		tk := issueTask("3buqdlot", 7, "fix")
+		tk.Spec.PredictedClass, tk.Spec.DataClass = class, dataClass
+		tk.Status.PullRequest = &v1alpha1.PullRequestRef{Number: 12, URL: "https://github.com/Smana/cloud-native-ref/pull/12"}
+		return tk
+	}
+	on := []runs.Spec{g.r.implementerSpec(task("docs", "public"), "brief"), g.r.verifierSpec(task("docs", "public"), "reviewer")}
+	for _, s := range on {
+		if !s.Checks || !reflect.DeepEqual(s.EgressProfiles, []string{"golang", "pypi"}) {
+			t.Fatalf("%s: checks %v egress %v, want on with [golang pypi]", s.Role, s.Checks, s.EgressProfiles)
+		}
+	}
+	off := []runs.Spec{g.r.verifierSpec(task("docs", "public"), "tester"), g.r.implementerSpec(task("docs", "internal"), "brief"),
+		g.r.implementerSpec(task("tests", "public"), "brief"), g.r.implementerSpec(task("review", "public"), "brief")}
+	for _, s := range off {
+		u := runs.Build(s)
+		for _, key := range []string{"loop", "egress"} {
+			if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", key); found {
+				t.Fatalf("%s %s: spec.%s written on a run without checks", s.Role, s.DataClass, key)
+			}
+		}
+	}
+}
diff --git a/internal/factory/reconciler/implement.go b/internal/factory/reconciler/implement.go
index 604111f..f7f7e56 100644
--- a/internal/factory/reconciler/implement.go
+++ b/internal/factory/reconciler/implement.go
@@ -132,10 +132,24 @@ func sourceURL(t *v1alpha1.Task) string {
 // implementerSpec: every run of a task writes agent/<taskId> (C3); baseRef main, because the
 // harness resumes origin/<branch> when an earlier run pushed it (SP1 R7).
 func (r *Reconciler) implementerSpec(t *v1alpha1.Task, text string) runs.Spec {
-	return runs.Spec{TaskID: t.Name, Role: "implementer", Repository: t.Spec.Repository, BaseRef: "main",
+	s := runs.Spec{TaskID: t.Name, Role: "implementer", Repository: t.Spec.Repository, BaseRef: "main",
 		Branch: "agent/" + t.Name, TaskText: text, Principal: runs.PrincipalFactory, DataClass: t.Spec.DataClass,
 		Model: t.Spec.Budget.Model, RoomRef: t.Status.RoomRef, SourceURL: sourceURL(t), Queue: runs.QueueFactory,
 		MaxTokens: t.Spec.Budget.RunTokens, MaxMinutes: t.Spec.Budget.RunMinutes, Traceparent: traceparent(t), Tier: t.Spec.Budget.Tier}
+	r.withChecks(&s, t)
+	return s
+}
+
+// withChecks switches the repository's checks on for a public implementer or reviewer run of a class
+// that has them (v3 feature 3, RC8): spec.loop.checks and the repository's checks.egress. Any other
+// run, an internal one included, is today's.
+func (r *Reconciler) withChecks(s *runs.Spec, t *v1alpha1.Task) {
+	on, egress := r.repo(t).ChecksFor(t.Spec.PredictedClass)
+	if !on || t.Spec.DataClass != "public" || (s.Role != "implementer" && s.Role != "reviewer") {
+		s.Checks, s.EgressProfiles = false, nil
+		return
+	}
+	s.Checks, s.EgressProfiles = true, egress
 }
 
 // reserveFactoryDay is §6.2's "SP3 at admission" for the factory's own principal (R34), admitted
diff --git a/internal/factory/reconciler/team.go b/internal/factory/reconciler/team.go
index 8945084..c19b4a9 100644
--- a/internal/factory/reconciler/team.go
+++ b/internal/factory/reconciler/team.go
@@ -54,6 +54,7 @@ func otherTier(tier string) string {
 func (r *Reconciler) verifierSpec(t *v1alpha1.Task, role string) runs.Spec {
 	s := r.implementerSpec(t, "")
 	s.Role, s.TaskText, s.TaskURL, s.BaseRef = role, "", t.Status.PullRequest.URL, "agent/"+t.Name
+	r.withChecks(&s, t)
 	if role == "reviewer" { // "on a different tier from the implementer where possible" (§3); R47
 		s.Tier = otherTier(t.Spec.Budget.Tier)
 		s.Model = r.Cfg.Tiers[s.Tier].Model
diff --git a/internal/factory/runs/runs.go b/internal/factory/runs/runs.go
index 2a3b627..d71a9c6 100644
--- a/internal/factory/runs/runs.go
+++ b/internal/factory/runs/runs.go
@@ -87,6 +87,9 @@ type Spec struct {
 	EgressProfiles                                                                                                                      []string
 	// Suspendable is spec.suspendable (v2 suspend), written only when true: it is immutable.
 	Suspendable bool
+	// Checks is spec.loop.checks (v3 feature 3), written only when true, so every other claim is
+	// today's byte for byte. The composition turns it into the harness's AGENT_CHECKS=1.
+	Checks bool
 }
 
 var (
@@ -149,6 +152,9 @@ func Build(s Spec) *unstructured.Unstructured {
 	if s.Suspendable {
 		spec["suspendable"] = true
 	}
+	if s.Checks {
+		spec["loop"] = map[string]any{"checks": true}
+	}
 	if len(s.EgressProfiles) > 0 {
 		profiles := make([]any, 0, len(s.EgressProfiles))
 		for _, p := range s.EgressProfiles {
diff --git a/internal/factory/runs/runs_test.go b/internal/factory/runs/runs_test.go
index e5f11d7..1167e7f 100644
--- a/internal/factory/runs/runs_test.go
+++ b/internal/factory/runs/runs_test.go
@@ -3,6 +3,7 @@
 package runs
 
 import (
+	"slices"
 	"strings"
 	"testing"
 	"time"
@@ -382,3 +383,19 @@ func TestChargedToRoundTrips(t *testing.T) {
 		t.Fatal("no person, no annotation")
 	}
 }
+
+// v3 feature 3: spec.loop.checks is written only when on, so every other claim is today's.
+func TestBuildWritesLoopChecksOnlyWhenOn(t *testing.T) {
+	if _, found, _ := unstructured.NestedMap(Build(spec()).Object, "spec", "loop"); found {
+		t.Fatal("spec.loop written for a run without checks")
+	}
+	s := spec()
+	s.Checks, s.EgressProfiles = true, []string{"golang", "pypi"}
+	u := Build(s)
+	if on, _, _ := unstructured.NestedBool(u.Object, "spec", "loop", "checks"); !on {
+		t.Fatalf("spec.loop.checks not set: %v", u.Object["spec"])
+	}
+	if p, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "egress", "profiles"); !slices.Equal(p, []string{"golang", "pypi"}) {
+		t.Fatalf("egress %v", p)
+	}
+}
```

- [ ] **Step 3:** `docs/factory.md`, the `repositories` table: change the `classes` row to
  `` `<name>: {live, shadow, checks}` ``, `live` and `shadow` never both; `checks: true` runs the
  repository's checks on the class's public implementer and reviewer runs (`spec.loop.checks`), and
  add the row:

```markdown
| `checks` | `{egress: [profiles]}`: what a check-enabled run requests beyond `github`, at most 4 of `pypi`, `npm`, `golang`, `crates`. The repository's `.github/agent-checks.yaml` never names egress | None: github only |
```

- [ ] **Step 4:** `task check` → exit 0. Then CI's `harness-checks` runs both repositories in full (PRs 2 and 3 merged): every step `pass` or `skip`, no refused host; copy its job summary (install seconds, API calls, each step's seconds) into the PR.
- [ ] **Step 5:** `git commit -m "feat(factory): repository checks per class, spec.loop.checks and the repository's egress"`; PR; after merge, the release (`CONTRIBUTING.md`), and note the harness digest and the factory tag.

---

## PR 5 — crossplane-configuration (now at v0.9.17)

### Task 11: The hosts, `spec.loop.checks`, the harness digest (offline proof 6)

**Files:** `apis/agentrun/kcl/main.k`, `main_test.k`, `settings-reject-internal-checks.yaml` (new),
`apis/agentrun/definition.yaml`, `apis/agentrun/composition.yaml` (generated),
`examples/agentrun-complete.yaml`, `tests/golden/agentrun-{basic,complete,suspendable}.yaml`.

- [ ] **Step 1: The failing tests** (the `main_test.k` and settings hunks below); `task test` → 3 failures.
- [ ] **Step 2: The KCL change** (measured: 110/110, both reject settings refused):

```diff
diff --git a/apis/agentrun/kcl/main.k b/apis/agentrun/kcl/main.k
index e9b833a..2cd8fd5 100644
--- a/apis/agentrun/kcl/main.k
+++ b/apis/agentrun/kcl/main.k
@@ -54,10 +54,10 @@ _SIZES = {
 }
 
 _EGRESS_PROFILES = {
-    github = ["github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "raw.githubusercontent.com"]
+    github = ["github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "raw.githubusercontent.com", "release-assets.githubusercontent.com"]
     pypi = ["pypi.org", "files.pythonhosted.org"]
     npm = ["registry.npmjs.org"]
-    golang = ["proxy.golang.org", "sum.golang.org"]
+    golang = ["proxy.golang.org", "sum.golang.org", "dl.google.com"]
     crates = ["index.crates.io", "static.crates.io"]
 }
 
@@ -346,6 +346,16 @@ _renderRun = lambda _oxr: any, _ocds: any, _dxr: any, _pods: any -> [any] {
     _profiles = ([] if _internal else ["github"]) + (_spec.egress?.profiles or [])
     _fqdns = [f for p in _profiles for f in _EGRESS_PROFILES[p]]
 
+    # v3 feature 3 (agent-platform's sandbox checks, RC8): the harness runs the repository's checks and
+    # needs the pod's deadline for its final run (RC9). The checks need egress, which an internal run
+    # has none of: the XRD refuses that claim first; this holds if that rule is ever lost.
+    _checks = _spec.loop?.checks or False
+    assert not (_internal and _checks), "an internal run runs no repository checks"
+    _checksEnv = [
+        {name = "AGENT_CHECKS", value = "1"}
+        {name = "AGENT_RUN_SECONDS", value = str(_runSeconds)}
+    ] if _checks else []
+
     # P6 (agentgateway plan): the harness clones https://github.com/<repo>.git; git's
     # environment config sends an internal run's fetch to identity-proxy, then the
     # internal listener's read-only route (ai-native-ref agent-gateway/github.yaml).
@@ -690,7 +700,7 @@ _renderRun = lambda _oxr: any, _ocds: any, _dxr: any, _pods: any -> [any] {
                             # honours LMNR_TRACE_CONTENT and injects traceparent; lmnr's LiteLLM one does neither.
                             {name = "LMNR_INSTRUMENTS", value = "openai"}
                             {name = "LMNR_TRACE_CONTENT", value = "false"}
-                        ] + ([{name = "TRACEPARENT", value = _tp}] if _tp else []) + _footerEnv + _bridgeEnv + _gitEnv + ([{name = "AGENT_SUSPENDABLE", value = "true"}] if _suspendable else [])
+                        ] + ([{name = "TRACEPARENT", value = _tp}] if _tp else []) + _footerEnv + _bridgeEnv + _gitEnv + ([{name = "AGENT_SUSPENDABLE", value = "true"}] if _suspendable else []) + _checksEnv
                         # No containerPort: agent-server listens on loopback only.
                         # timeoutSeconds covers python3's start-up under gVisor
                         # plus urlopen's own 2 s; kubelet's 1 s default would
diff --git a/apis/agentrun/kcl/main_test.k b/apis/agentrun/kcl/main_test.k
index bfa9457..2832331 100644
--- a/apis/agentrun/kcl/main_test.k
+++ b/apis/agentrun/kcl/main_test.k
@@ -274,7 +274,7 @@ _routerRulesFor = lambda spec: any -> any {
 
 test_public_run_keeps_github_egress = lambda {
     _fqdn = [e for e in _kind(_run({}), "CiliumNetworkPolicy")[0].spec.egress if e.toFQDNs][0]
-    assert [f.matchName for f in _fqdn.toFQDNs] == ["github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "raw.githubusercontent.com"]
+    assert [f.matchName for f in _fqdn.toFQDNs] == ["github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "raw.githubusercontent.com", "release-assets.githubusercontent.com"]
 }
 
 test_public_run_reaches_only_the_public_listener = lambda {
@@ -330,7 +330,7 @@ test_cnp_is_default_deny_with_named_egress = lambda {
     # Only the proxy's health listener: not agent-server (M6), not Envoy admin (I2).
     assert _spec.ingress == [{fromEntities = ["host"], toPorts = [{ports = [{port = "9902", protocol = "TCP"}]}]}]
     _fqdn = [e for e in _spec.egress if e.toFQDNs][0]
-    assert [f.matchName for f in _fqdn.toFQDNs] == ["github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "raw.githubusercontent.com", "pypi.org", "files.pythonhosted.org"]
+    assert [f.matchName for f in _fqdn.toFQDNs] == ["github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "raw.githubusercontent.com", "release-assets.githubusercontent.com", "pypi.org", "files.pythonhosted.org"]
     _dns = [n.matchName for n in _spec.egress[0].toPorts[0].rules.dns]
     assert "agent-router.agent-gateway.svc.cluster.local" in _dns and "pypi.org" in _dns
     _dnsRules = _spec.egress[0].toPorts[0].rules.dns
@@ -1291,3 +1291,22 @@ test_no_pod_or_a_malformed_image_id_projects_nothing = lambda {
     assert "harnessImage" not in _endWith({phase = "Running"}, _appliedSandbox(False, [{type = "Ready", status = "True"}]), [_runningPod("")])
     assert "harnessImage" not in _status(_run({})), "kcl test: no requiredResources at all"
 }
+
+test_check_hosts_reach_the_cnp = lambda {
+    # v3 feature 3 (RC7): a GitHub release download lands on release-assets, Go's archive on dl.google.com.
+    _spec = _kind(_run({
+        egress = {profiles = ["golang"]}
+    }), "CiliumNetworkPolicy")[0].spec
+    _fqdns = [f.matchName for e in _spec.egress if e.toFQDNs for f in e.toFQDNs]
+    _dns = [n.matchName for n in _spec.egress[0].toPorts[0].rules.dns]
+    assert "release-assets.githubusercontent.com" in _fqdns and "release-assets.githubusercontent.com" in _dns
+    assert "dl.google.com" in _fqdns and "dl.google.com" in _dns
+}
+
+test_loop_checks_reach_the_harness = lambda {
+    # v3 feature 3 (RC8, RC9): AGENT_CHECKS and the pod's deadline, on a check-enabled run only.
+    _env = {e.name: e.value for e in _pod(_run({loop = {checks = True}, budget = {maxMinutes = 45}})).containers[0].env}
+    assert _env["AGENT_CHECKS"] == "1" and _env["AGENT_RUN_SECONDS"] == "2700"
+    _off = [e.name for e in _pod(_run({})).containers[0].env]
+    assert "AGENT_CHECKS" not in _off and "AGENT_RUN_SECONDS" not in _off, "a run without checks is today's"
+}
diff --git a/apis/agentrun/kcl/settings-reject-internal-checks.yaml b/apis/agentrun/kcl/settings-reject-internal-checks.yaml
new file mode 100644
index 0000000..bf91210
--- /dev/null
+++ b/apis/agentrun/kcl/settings-reject-internal-checks.yaml
@@ -0,0 +1,29 @@
+# expect: an internal run runs no repository checks
+# v3 feature 3 (RC8): the checks need egress, and an internal run has none. The XRD refuses
+# this claim too; this proves the composition does not depend on it.
+kcl_options:
+  - key: params
+    value:
+      oxr:
+        apiVersion: cloud.ogenki.io/v1alpha1
+        kind: AgentRun
+        metadata:
+          name: xplane-run-7f3cq2xz
+          namespace: agents
+          uid: 0b3c6f0e-5d1a-4b8e-9f41-2a7c3e9d8b10
+        spec:
+          role: triager
+          repository: Smana/cloud-native-ref
+          principal: "system:runlore"
+          dataClass: internal
+          task:
+            text: "Investigate the alert."
+          loop:
+            checks: true
+      ocds: {}
+      dxr:
+        apiVersion: cloud.ogenki.io/v1alpha1
+        kind: AgentRun
+        metadata:
+          name: xplane-run-7f3cq2xz
+          namespace: agents
```

- [ ] **Step 3: The XRD.** Under `spec.x-kubernetes-validations`, after the `suspendable` rule:

```yaml
                # agent-platform v3 feature 3 (RC8): the checks need egress, which an internal run
                # has none of; and the loop policy is fixed at creation, presence and value.
                - rule: "self.dataClass != 'internal' || !has(self.loop) || !has(self.loop.checks) || !self.loop.checks"
                  message: "an internal run runs no repository checks"
                - rule: "has(self.loop) == has(oldSelf.loop) && (!has(self.loop) || self.loop == oldSelf.loop)"
                  message: "loop is immutable and cannot be added or removed after creation"
```

  and under `spec.properties`, after `suspendable`:

```yaml
                loop:
                  description: >-
                    The run's loop policy, fixed at creation (v3). checks runs the repository's checks in the
                    sandbox (agent-platform v3 feature 3): the harness gets AGENT_CHECKS=1 and the pod's
                    deadline as AGENT_RUN_SECONDS. A public run only.
                  type: object
                  properties:
                    checks:
                      type: boolean
```

- [ ] **Step 4:** `_HARNESS_PROFILES.openhands.image` and `_BRIDGE_IMAGE` to PR 4's release digests, with a comment line naming it; `examples/agentrun-complete.yaml` gains `loop: {checks: true}`.
- [ ] **Step 5:** `task generate`; with Docker running, re-render the three goldens:
  `for e in agentrun-basic agentrun-complete agentrun-suspendable; do crossplane render examples/$e.yaml apis/agentrun/composition.yaml functions.yaml --extra-resources examples/environmentconfig.yaml > tests/golden/$e.yaml; done`.
  Review the diff: the two hosts in each CNP's DNS and FQDN rules, the digests, and `AGENT_CHECKS`
  and `AGENT_RUN_SECONDS` in `agentrun-complete` only. `agentrun-internal` must not change.
- [ ] **Step 6:** `task check` → exit 0. Commit `feat(agentrun): spec.loop.checks, the checks hosts, agent-platform <release>`; PR; release `v0.9.18`.

## PR 6 — cloud-native-ref: the pin

### Task 12

- [ ] Bump every crossplane-configuration pin to `v0.9.18`
  (`infrastructure/base/crossplane/configuration-core/configuration-core.yaml`,
  `configuration-aws/configuration-packages.yaml`, `configuration-gcp/configuration-packages.yaml`;
  `test-crossplane-configuration-pins.sh` holds them equal); `task check` → exit 0; commit
  `chore(crossplane): crossplane-configuration v0.9.18, repository checks in the sandbox`; PR. Its
  CI is the proof.

## PR 7 — ai-native-ref: `HOST_REF`, the pins, the keys off, the docs

### Task 13

- [ ] **Step 1:** `HOST_REF` to PR 6's merge commit; the factory image in
  `tooling/base/agent-factory/helm-values-configmap.yaml` to PR 4's release tag and digest, its comment
  naming the release.
- [ ] **Step 2:** In the same file, each repository entry gains, every class left as it is:

```yaml
          # v3 feature 3: what a check-enabled run requests beyond github. No class has checks: true
          # until the gcp-0 session's live proofs (D4).
          checks: {egress: [pypi]}            # Smana/cloud-native-ref
          checks: {egress: [golang, pypi]}    # Smana/ai-native-ref
```

- [ ] **Step 3:** `harness-reference.md`: the claim table gains
  `` | `loop.checks` | — | Runs the repository's checks in the sandbox: `AGENT_CHECKS=1`, refused on `internal` | ``;
  the composition's Egress row becomes "`github.com` and its API, codeload, content and release-asset
  hosts, plus each profile's registries (`golang` includes `dl.google.com`)"; the Environment row
  gains `AGENT_CHECKS`, `AGENT_RUN_SECONDS` "with `loop.checks`"; the harness table gains
  `` | `agent-checks` | the CI's fast subset from `.github/agent-checks.yaml` | Before every push; the harness re-runs it on a finished implementer's head and notes the result | {{< badge content="deployed" color="blue" >}} | ``.
- [ ] **Step 4:** `status.md`, the roadmap row "Repository checks in the sandbox": its gate column
  becomes "Acceptance not lower, and the sandbox agreeing with the CI on at least 90% of final heads"
  (Q4 as approved).
- [ ] **Step 5:** `bash $SCRATCH/check.sh <worktree>` → `exit=0` (the pins suite included); commit
  `feat(agent-factory): repository checks wired, every class off`; PR.

---

## The gate

**Offline:** each PR merges on the proofs in its row above.

**Live, one gcp-0 session after PR 7** (switch `classes.docs.checks: true`, then others, by a
human commit to the config):

1. A check-enabled run on each repository: the CNP drops no flow to a declared host (Hubble), and the
   result on `main` matches `harness-checks`.
2. A drill on each repository: a small change with a defect in its first commit (a ShellCheck warning
   on the host, a broken relref on ai-native-ref); the sandbox reports it, the first pushed head is
   green, `status.fixRuns` stays 0.
3. The gate (R22): on every final head of the session's real issues, agreement per CI check from the
   room note and the declaration's `ci` map against `gh pr checks <pr>`; at least 90%, each
   disagreement explained; acceptance not lower than the switch-off runs. Report CI round-trips,
   and the install's and the checks' seconds from the spans.

---

## Appendix A: the coverage suite

`scripts/ci/tests/test-agent-checks.py`, the same bytes in cloud-native-ref and ai-native-ref:

```python
#!/usr/bin/env python3
# requires: python3
"""The sandbox checks' declaration mirrors the CI (v3 feature 3, RC2, RC3, offline proof 1).

.github/agent-checks.yaml is what an agent runs in its sandbox before it pushes, read by the harness
from the default branch. This suite holds it to the same schema the harness enforces, and to the
CI: every job of a workflow that runs on pull_request is named by a step's `ci` or by a `ciOnly`
entry, every name the declaration uses is such a job, every tool is a key mise.toml pins, every
definedBy path exists. A job is named by its check name (its `name`, else its id; a static matrix
adds `name (value, ...)`) or as `<workflow file stem>/<job id>`. The same file runs in
cloud-native-ref and ai-native-ref, on the repository the working directory is in.

Run: python3 scripts/ci/tests/test-agent-checks.py
"""
import itertools
import pathlib
import re
import subprocess
import sys
import tomllib

try:
    import yaml
except ImportError:
    print("SKIP: pyyaml not installed")
    sys.exit(77)

# The working directory's repository: ai-native-ref runs its suites from .composite/ with the
# working directory in this repository (scripts/ci/run-tests.sh); the host from its root.
ROOT = pathlib.Path(subprocess.run(["git", "rev-parse", "--show-toplevel"], capture_output=True, text=True).stdout.strip() or ".")
DECL = ROOT / ".github/agent-checks.yaml"
STEP_NAME = re.compile(r"^[a-z0-9][a-z0-9-]{0,39}$")
MAX_MINUTES = 15
fails = []


def fail(msg):
    fails.append(msg)


def pr_jobs():
    """{identifier: job key} for every job of a workflow triggered by pull_request, and the job keys."""
    ids, keys = {}, set()
    for wf in sorted((ROOT / ".github/workflows").glob("*.y*ml")):
        doc = yaml.safe_load(wf.read_text()) or {}
        on = doc.get("on", doc.get(True)) or {}
        events = [on] if isinstance(on, str) else list(on)
        if not {"pull_request", "pull_request_target"} & set(events):
            continue
        for jid, job in (doc.get("jobs") or {}).items():
            key = "%s/%s" % (wf.stem, jid)
            keys.add(key)
            name = str(job.get("name") or jid)
            names = {key, name}
            matrix = (job.get("strategy") or {}).get("matrix")
            if isinstance(matrix, dict) and "${{" not in name:
                axes = [v for k, v in matrix.items() if k not in ("include", "exclude") and isinstance(v, list)]
                if axes:
                    names |= {"%s (%s)" % (name, ", ".join(map(str, combo))) for combo in itertools.product(*axes)}
            for n in names:
                ids.setdefault(n, set()).add(key)
    return ids, keys


if not DECL.is_file():
    print("FAIL .github/agent-checks.yaml: missing")
    sys.exit(1)
doc = yaml.safe_load(DECL.read_text())
if not isinstance(doc, dict):
    print("FAIL .github/agent-checks.yaml: not a mapping")
    sys.exit(1)

# The schema the harness enforces (agent-platform images/agent-harness/agent_checks.py, validate).
if extra := set(doc) - {"version", "timeoutMinutes", "tools", "definedBy", "steps", "ciOnly"}:
    fail("unknown keys %s" % sorted(extra))
if doc.get("version") != 1:
    fail("version must be 1")
minutes = doc.get("timeoutMinutes", 8)
if not (isinstance(minutes, int) and 1 <= minutes <= MAX_MINUTES):
    fail("timeoutMinutes must be 1 to %d" % MAX_MINUTES)
    minutes = MAX_MINUTES
try:
    pinned = tomllib.loads((ROOT / "mise.toml").read_text()).get("tools", {})
except (OSError, tomllib.TOMLDecodeError) as exc:
    pinned = {}
    fail("mise.toml: %s" % exc)
for t in doc.get("tools", []):
    if t not in pinned:
        fail("tool %s is not a key of mise.toml's [tools]" % t)
for p in doc.get("definedBy", []):
    if not (ROOT / p).exists() or (p.endswith("/") and not (ROOT / p).is_dir()):
        fail("definedBy %s does not exist" % p)
steps = doc.get("steps") or []
if not steps:
    fail("steps must be a non-empty list")
seen = set()
for s in steps:
    name = s.get("name") if isinstance(s, dict) else None
    if not isinstance(name, str) or not STEP_NAME.match(name) or name in seen:
        fail("step %r: names are unique and match %s" % (name, STEP_NAME.pattern))
        continue
    seen.add(name)
    if extra := set(s) - {"name", "run", "ci", "timeoutMinutes", "stopOnFailure"}:
        fail("step %s: unknown keys %s" % (name, sorted(extra)))
    if not (isinstance(s.get("run"), str) and s["run"].strip()):
        fail("step %s: no run" % name)
    t = s.get("timeoutMinutes", minutes)
    if not (isinstance(t, int) and 1 <= t <= minutes):
        fail("step %s: timeoutMinutes must be 1 to %d" % (name, minutes))
    if not isinstance(s.get("stopOnFailure", False), bool):
        fail("step %s: stopOnFailure is a boolean" % name)
ci_only = doc.get("ciOnly") or []
for c in ci_only:
    if not (isinstance(c, dict) and set(c) == {"check", "part", "why"} and all(isinstance(v, str) and v for v in c.values())):
        fail("ciOnly entry %r: exactly check, part and why, each a string" % (c,))

# The coverage rule (RC3): every pull_request job is mirrored or listed, every name is a job.
ids, keys = pr_jobs()
named = [("step %s" % s.get("name"), s.get("ci")) for s in steps if isinstance(s, dict)]
named += [("ciOnly entry %s" % c.get("check"), c.get("check")) for c in ci_only if isinstance(c, dict)]
covered = set()
for where, n in named:
    if n not in ids:
        fail("%s names %r, which is no job of a pull_request workflow" % (where, n))
    else:
        covered |= ids[n]
for key in sorted(keys - covered):
    fail("job %s runs on pull_request and is neither a step's ci nor a ciOnly entry" % key)

for msg in fails:
    print("FAIL", msg)
if fails:
    sys.exit(1)
print("PASS %d steps, %d CI-only entries, %d pull_request jobs covered" % (len(steps), len(ci_only), len(keys)))
```
