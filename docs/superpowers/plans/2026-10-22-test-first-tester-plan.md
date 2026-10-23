# The test-first tester (v3 feature 4), first slice: the contract check and the report-only job — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One script, `test-contract`, decides whether a change's tests are a contract: on the
tester's commit (red check), on an implementer's head (hold), and on every agent pull request in CI
("Test contract 🧪"). This slice ships the script, its composite action, the host's test declaration
and runner, and the job in **report-only** mode: nothing requires it, no class switches on, and the
bench gets the baseline's vacuous-test rate from the first agent pull request.

**Architecture:** The repository declares its tests in a `tests` section of feature 3's
`.github/agent-checks.yaml` (paths, selection globs, the runner's own files, a `run` line that writes
JUnit XML to `$AGENT_TEST_REPORT`). `test-contract` reads it from the base, never from the change,
checks the change out three times in detached worktrees (the base's versions of the selected files,
the overlay, the head), runs the selected files only, and compares per-test ids. The harness image
carries it as `/opt/agent/test_contract.py` → `test-contract`; a composite action in agent-platform
runs the same file in CI, pinned by commit. The host's `scripts/ci/tests/run.sh` gains a file
selection and a JUnit report, one testcase a suite.

**Tech Stack:** Python 3.13 (the harness image; PyYAML; stdlib `xml.etree`), git worktrees, pytest
9.1.1 for the pytest fixture (image test stage only), Bash suites, GitHub Actions composite actions,
`jdx/mise-action`.

**Spec:** ai-native-ref
[`docs/superpowers/specs/2026-10-22-test-first-tester-design.md`](https://github.com/Smana/ai-native-ref/blob/main/docs/superpowers/specs/2026-10-22-test-first-tester-design.md)
(TF1–TF11, approved 2026-10-22 with TO1–TO5 as recommended). Built on feature 3's plan
(`/home/smana/Sources/agent-platform-sandbox-checks/docs/superpowers/plans/2026-10-22-sandbox-checks-plan.md`:
`agent-checks`, `.github/agent-checks.yaml`, `loop.checks`, its PR order), the loop policy part 1 plan
(`AGENT_LOOP`, `drive`, `loop_policy.decide`) and the merge gate classes plan (`holdoutChecks`,
`DecideArm` as `baseChecks` + conditions, `forge.FileStat`). Read the spec with this plan.

## This plan is the first slice

The whole design is ten pull requests over four repositories. Five of them build on code that is
planned but not merged (feature 3's harness and factory, the loop policy's `drive`, the merge gate's
`baseChecks`), so their diffs cannot be written against `main` today. This plan covers the part that
needs only feature 3's declaration: **the contract check, its action, the host's runner and the
job, report-only** (rows 1–3 below). Rows 4–10 are follow-up plans F1–F5, each written when what it
builds on has merged; [What the follow-up plans inherit](#what-the-follow-up-plans-inherit) fixes
their interfaces and rulings now.

### Every pull request, in order

| # | Repository | What | Depends on | Plan |
|---|---|---|---|---|
| 1 | agent-platform | `test_contract.py` (`red`, `hold`, `ci`), its fixtures (offline proof 1), the composite action, the image's `test-contract`, `agent_checks.validate` accepting `tests`; a release | feature 3 PR 1 merged | this |
| 2 | cloud-native-ref (host) | `run.sh` selection and JUnit with its suite; the `tests` section and the job's `ciOnly` entry in `.github/agent-checks.yaml`; the coverage suite accepting `tests`; `.github/workflows/test-contract.yaml`, required by nothing | 1; feature 3 PR 2 (the declaration, `--from-git`) | this |
| 3 | ai-native-ref | `HOST_REF` to 2's merge; the coverage suite byte-identical to the host's; the status page | 2; feature 3 PR 3 | this |
| 4 | agent-platform | **F1, the harness:** the tester's commit-only mode, red check, signed push and ends; the implementer's contract file, pre-commit hook and contract-red continuation; `room_verdict` for the reviewer only | 1; feature 3 PR 1; loop policy part 1 | F1 |
| 5 | crossplane-configuration | **F2:** the tester's platform rules (writes tests, commits, never pushes, no verdict); goldens; a release | none: may go first | F2 |
| 6 | agent-platform | **F3, the factory:** `testFirst`, `merge.contractCheck`, the tester stage, `status.contract`, rows 2b and 2c, `contract_disputed`, the verifier tester retired (TO1), the template and holdout warnings (TO5); a release | 4; merge gate PR A; feature 3 PR 4 | F3 |
| 7 | cloud-native-ref (host) | **F4:** the tester's trust policy `contents: write` (TO2); the composition pin of 5's release | 5 | F4 |
| 8 | ai-native-ref | **F5:** pin 6's release; `HOST_REF` to 7; the config with every switch off (`trio` without `tester`, `holdoutChecks`, `testFirst: false`, `merge.contractCheck`); the tester's profiles without `room_verdict`; the docs | 6, 7 | F5 |

Live proofs 1–3 then run in one gcp-0 session after row 8; live proof 4 (the bench) waits with the
bench. A class switches on only after its offline proofs and a live check on real issues (D4).

### This plan's rulings

Points the design left open or left to the builder, for this slice and the follow-ups. Where a
later-approved plan disagrees with the spec, the plan's interface wins and the row says so.

| # | Ruling | Why |
|---|---|---|
| R1 | **This plan is the first slice (rows 1–3); F1–F5 follow in the order above** | Rows 4–10 change code that feature 3, the loop policy and the merge gate have planned and not merged. The contract check is what every later row calls, and the report-only job measures the baseline (TF7) before anything switches on |
| R2 | **The test declaration is a `tests` section of `.github/agent-checks.yaml`:** `paths` (what a tester may write: a prefix ending `/` or a path), `select` (globs: the changed files whose tests run), `runner` (the runner's own files: never a contract, red when changed), `run` (a bash line; `"$@"` is the selected files; it writes JUnit XML to `$AGENT_TEST_REPORT`), `timeoutMinutes` (1–15, default 5). Read from the base (CI: the pull request's base commit; sandbox: `refs/remotes/origin/HEAD`) | The spec asks feature 3 for "the test paths and the runner's own files, declared with the entrypoint, in a gate path"; feature 3's plan declares none and its schema refuses unknown keys. One file keeps one reader and one gate path. **Amends feature 3:** `agent_checks.validate` and the coverage suite (Appendix A) accept the key |
| R3 | **The script is Python, `images/agent-harness/test_contract.py`, with one runner contract: JUnit XML.** pytest writes it natively (`--junitxml`); the host's `run.sh` writes one testcase a suite; promtool rule tests wait for the first class that needs them, wrapped by their repository the same way | "The script's language and its runner adapters." The harness venv is Python, PyYAML is already there, and an adapter in the repository's runner keeps the script language-agnostic |
| R4 | **An id is `classname::name`** (the name alone when the two are equal or the class is empty). **A failing id stands for every id under it** (`id::…` or `id.…`), and an id stands for the ids under it | Measured: pytest reports a test file that fails to import (the commonest failing test, a missing function) as one collection error, `tests.test_sub`, which becomes `tests.test_sub::test_sub` once the code exists. Without the rule the contract's id never passes |
| R5 | **The base's ids come from a run of the base's versions of the selected files that exist there**, not a collect-only pass: a third run in CI, none for a new file | "How the base's ids are collected." A shell suite has no collect-only mode; one rule for every runner |
| R6 | **The runner ran when its report exists and holds a testcase.** The exit status is ignored; a timeout kills the process group and leaves no report. Could-not-run is never red and never green: CI red, sandbox `contract_not_red` with "the runner could not run" and **no continuation** (F1) | The spec's "an infrastructure failure is never red". The tester cannot fix the toolchain, so a continuation would spend tokens on nothing |
| R7 | **The change is the three-dot diff (from the merge base), renames as a delete and an add; the overlay is the base's tree with the changed test-path files at the head's content, deletions applied.** A tester's commit only adds or modifies files under `paths`, never a runner file: anything else is `contract_outside_tests` | Three-dot ignores what `main` gained since the fork. A deleted test cannot be a contract, and treating renames as delete + add needs no rename heuristics |
| R8 | **The CI job:** its own workflow `.github/workflows/test-contract.yaml`, job `Test contract 🧪`, `pull_request` on `main`, run only when the author is `ogenki-agents[bot]` (a workflow cannot read the factory's `agentsLogin`), `contents: read`, a full-history checkout without persisted credentials, mise for the toolchain, PyYAML, then agent-platform's action pinned by the 40-hex commit of PR 1's merge. Inputs reach the shell through `env`. The host's declaration lists the job as a `ciOnly` entry | TF7. Feature 3's coverage rule (RC3) fails on a `pull_request` job that no step or `ciOnly` entry names |
| R9 | **The host's `run` lays the AI paths first:** `./scripts/ci/ai-overlay.sh $([ -n "${CI:-}" ] \|\| echo --from-git) >/dev/null && scripts/ci/tests/run.sh "$@"`, in each of the three trees. `ai-overlay.sh` is a runner file | Measured: `flux-schema/test-render-bundle.py` fails without the AI paths (`observability/base/runlore-factory/…`). In CI, `CI=true` takes the verified artifact; in the sandbox `CI` is scrubbed (feature 3 R7) and `--from-git` (feature 3 R18) lays the pin's tag. A failed overlay leaves no report: could-not-run, never red |
| R10 | **The host's tests are its suites:** `paths: [scripts/ci/tests/]`, `select` the three globs `run.sh` itself discovers, `runner: [scripts/ci/tests/run.sh, scripts/ci/ai-overlay.sh]`, `timeoutMinutes: 10`. A suite is one id, its path from the repository root | TO4 allows the host's `scripts` and `review` classes; their tests are these suites. `scripts/ci/tests/` is not a host gate path (measured on `.policy.yml` at `HOST_REF`) |
| R11 | **The check run's summary** is a heading with the result, the reasons as a list, then one table row per id: new, base, base + tests, head | "The check run's summary layout." The rows are the bench's per-id evidence |
| R12 | **Trailers on the contract commit:** `Agent-Contract: <tester run id>`, `Agent-Contract-File: <path>` (one per file), `Agent-Contract-Id: <id>` (one per id, at most 50), with `Agent-Run` and `Agent-Task`. The tester's harness writes them itself on a squashed commit, `git commit-tree HEAD^{tree} -p <base>`, never through the model's commit-msg hook (F1) | "The trailer names." Measured: commit-tree plus a push of the sha leaves one commit on the branch, its trailers readable with `%(trailers:key=…)`. The hook neutralises any `Agent-*` line the model wrote, so a harness-written message must bypass it |
| R13 | **The tester never pushes:** the tester's clone sets `remote.origin.pushurl` to `disabled://the-tester-never-pushes`, and the harness pushes the signed sha to the fetch URL. A courtesy: the guard is the factory's forge read (R14) | Measured: git refuses the model's push (`'remote-disabled' is not a git command`). With TO2 the token can write, so only the factory's check of what landed binds |
| R14 | **`status.contract` comes from the forge, not the room's end note** (deviates from TF4's source; owner to confirm, recommended): after the tester run the factory reads `agent/<task>`'s last push (`BranchActivity`: actor the agents App) and its commit's trailers. `Agent-Contract` equal to the tester run's id: a contract, `{runId, commit, files, ids}`. No branch: no contract (TO3). Anything else: `Escalated`, `contract_invalid`. The end note (`Harness (tester): <end>: …`) only names the category of a no-contract end, for the narration and the count | The end note is posted on the run's own room token, which the model holds too; the push record is GitHub's. It also needs no `endReason` channel (loop policy part 2). The gate compares against this commit anyway (TF8) |
| R15 | **The hook is `/etc/agent/git-hooks/pre-commit`,** beside `commit-msg`, reading `/workspace/.agent-contract.json`, which `agent-run` writes when the resumed branch holds a contract commit (`test_contract.contract_of`) (F1) | "Where the hook is installed." `core.hooksPath` already points there, so a repository's own hooks cannot shadow it |
| R16 | **The implementer's contract-red continuation runs inside loop part 1's `drive()`, before the judge:** `test-contract hold` red → `loop_policy.decide(…, Verdict("not_yet", <failing ids>, ("contract",)), …)`. With `AGENT_LOOP` unset, a contract-only loop: 2 continuations, no judge, no stall rule; spent, the run ends as today (exit 0) and row 2b judges (F1) | TF6's "within `continuations`, without the judge". The loop's exit-1 ends belong to the loop's own switch (D4): `testFirst` alone must not change how a run ends |
| R17 | **`testFirst: true` needs `checks: true` on the class and `merge.contractCheck` on the repository; it is refused on `docs-links` and `revert`; `review` may be declared with `checks` and `testFirst` only. A tester runs on public tasks only;** an internal task of the class runs today's sequence (F3) | The red check needs the toolchain (feature 3), which feature 3 refuses on internal runs. **Amends feature 3's** "review is implicit; do not declare it" |
| R18 | **A template listing `tester`:** for one release `config.Parse` drops it with a warning logged at start (so `trio` runs `[implementer, reviewer]`); the next minor refuses it. The API and `roomctl` refuse a `tester` run from the same release (`tester_is_factory_only`) (F3) | TO1's "warns for one release, then refused", made exact: a kept `tester` entry would start a verifier that can no longer post a verdict |
| R19 | **The tester's model and caps are the implementer's spec** (the task tier's), until L8's `roles.<dataClass>.tester` exists (F3) | TF1 names L8, which no plan builds yet |
| R20 | **Holdouts are the merge gate plan's `holdoutChecks`** (its R6, R8): a `[]string` condition among the class's declared conditions, posted by GitHub Actions; missing or running waits `holdout_pending`, red or another App `holdout_red`. **No `holdout_absent`** (an absent holdout waits, as a pending check does) and **no `{status, creator}` scenario form** (TF11 builds no scenario suite in v3). This feature adds only TO5: a live class without `holdoutChecks` logs a warning and sets `agent_factory_class_without_holdout{repository, class}` to 1 for one release, then is refused (F3) | The merge gate plan is the later-approved interface and wins over TF9–TF10 and live proof 3's `holdout_absent`. The repository label keeps two repositories' classes apart |
| R21 | **Row 2b:** `merge.contractCheck` joins the required checks when `status.contract` is set (row 2's `CIState`): red is `ci_red` and a fix run, pending waits. **Row 2c**, right after row 2: `Merger.ChangedFiles(contract.commit, head)` (the compare call `Diff` makes, without contents) names a contract file → `human`, `contract_modified`. A head that no longer descends from the contract commit lists the contract files as changed and hands off too (F3) | "The forge read of files at two commits": one compare call per decision instead of two reads per file. Fail closed on a rebase |
| R22 | **Release order:** PR 1 changes feature 3's `agent_checks.py`, so it follows feature 3 PR 1; if PR 1 is built before feature 3 PR 1 merges, its Task 2 change goes into that pull request instead. **No class switches `checks` on with a harness older than PR 1's release** once the host declares `tests` | "The order between the harness release and feature 3's." `agent_checks.validate` refuses unknown keys: an older harness would read the host's declaration as none (exit 2) |
| R23 | **The factory's count:** `agent_factory_contracts_total{class, end}`, `end` one of `ready`, `not_testable`, `contract_not_red`, `contract_outside_tests`, `contract_invalid`, `unreported` (no end note); the narration: "The tester wrote a contract of N tests: …" and "The tester left no contract (<category>): the implementer works without one." (F3) | "The narration's wording." The bench's contract-ends row reads the counter |

**Measured 2026-10-23** on prototypes in `$SCRATCH/tester/` (the code below is theirs; all discarded):

- **`test-contract`:** 17 tests, 16 pass and one skips (no `agent_checks.py` beside it yet) on Python
  3.14.8 with pytest 9.1.1, 5.5 s: every case of offline proof 1 on a pytest fixture and a shell-suite
  fixture, the implementer's hold, the strict `tests` schema, and the action resolving to the script.
- **pytest's collection error:** a new test importing a function that does not exist yet is reported
  as `tests.test_sub` at the overlay and `tests.test_sub::test_sub` at the head (R4).
- **The host at `HOST_REF` (`ea184f78`):** `run.sh` with the selection and the report passes
  ShellCheck; its new suite passes, and fails 7 checks on the unpatched script; a full run gives the
  unpatched script's results (63 pass, 3 skip, 3 fail without the AI paths; `test-openbao-new-lineage`
  flakes on its own: 0, 1, 0 over three runs). `test-contract red` on a new suite took 0.23 s and
  returned `contract_ready`; `ci` on that suite plus the script it tests returned green with the
  summary below; a change to `run.sh` returned red, naming it. Every worktree was removed.
- **The workflow on a copy of the host:** `actionlint`, the host's `check-workflow-secrets.sh` and
  `check-policy-gate-coverage.sh` pass with `test-contract.yaml` added. The action's `run` line,
  executed with `github.action_path` substituted, ran the script and wrote the step summary.
- **The contract commit:** `git commit-tree HEAD^{tree} -p <base>` with trailers, pushed by sha, left
  one commit on the branch; the model's push was refused by the disabled `pushurl` (R12, R13).
- **Not run here:** the overlay inside `run` (cosign and network; R9), the image's test stage (Docker
  is stopped on this machine: CI's `harness-test` runs it), feature 3's coverage suite (not merged).

## Global Constraints

- **Branches and worktrees.** agent-platform: `feat/test-first-tester` in
  `/home/smana/Sources/agent-platform-tester` (this plan is its first commit). cloud-native-ref and
  ai-native-ref: a worktree from a freshly fetched `origin/main` each; `mise trust mise.toml` once in
  an ai-native-ref worktree. Rebase onto a freshly fetched `origin/main` before every push.
- **`$SCRATCH`** is a scratch directory outside every repository.
- **Evidence:** agent-platform `task check` and CI's `harness-test` (the image's test stage);
  cloud-native-ref `scripts/ci/tests/run.sh`, `actionlint`, `shellcheck`,
  `check-policy-gate-coverage.sh`, `check-workflow-secrets.sh`; ai-native-ref `task check`. Never claim
  done without the output.
- **A host change is a cloud-native-ref pull request first**, then a `HOST_REF` bump in ai-native-ref.
  `.github/` is a host gate path: PR 2 is human-authored and reviewed.
- **Off by default (D4).** Nothing in this slice gates a merge: no required check, no factory key.
- **Commits:** conventional, English, no trailers. `git add` named paths only.
- **Verbatim from the spec:**
  - TF7 table: "the runner ran both times; no new id passes at the overlay; every id passes at the
    head, a skip being no pass; at least one id fails at the overlay and passes at the head; no runner
    file changed. No changed test file: green, reporting none".
  - TF5 table: "every changed path is a test path; the runner ran; at the overlay at least one id
    fails and no new id passes".
  - "*Overlay*: the base's tree with the changed test files at their new content. *New id*: an id the
    head or the tester's commit reports and the base does not. A changed existing id that fails at
    the overlay counts as fail-to-pass (a changed expectation); one that passes is neutral."

## File structure

| File | PR | Change | Responsibility |
|---|---|---|---|
| `images/agent-harness/test_contract.py` | 1 | create | `test-contract red | hold | ci` |
| `images/agent-harness/tests/test_test_contract.py` | 1 | create | Offline proof 1, the schema, the action |
| `images/agent-harness/tests/contract-requirements.txt` | 1 | create | pytest, hash-pinned, the test stage only |
| `images/agent-harness/Dockerfile` | 1 | modify | Copy the script, link `test-contract`, pytest in the test stage |
| `images/agent-harness/agent_checks.py` | 1 | modify | Accept the `tests` key (R2, R22) |
| `.github/actions/test-contract/action.yml` | 1 | create | The composite action CI pins |
| `images/agent-harness/README.md` | 1 | modify | The new file and command |
| host `scripts/ci/tests/run.sh`, `test-run-selection.sh` | 2 | modify, create | Selection and JUnit; their suite |
| host `.github/agent-checks.yaml`, `scripts/ci/tests/test-agent-checks.py` | 2 | modify | The `tests` section, the job's `ciOnly` entry; the coverage suite |
| host `.github/workflows/test-contract.yaml` | 2 | create | "Test contract 🧪", report-only |
| ai-native-ref `HOST_REF`, `scripts/ci/tests/test-agent-checks.py`, `website/content/docs/platform/ai-platform/status.md` | 3 | modify | The pin, the suite in step, the status |

---

## PR 1 — agent-platform: `test-contract`

### Task 1: `test-contract` and its fixtures (offline proof 1)

**Files:** Create `images/agent-harness/test_contract.py`, `images/agent-harness/tests/test_test_contract.py`.

**Interfaces:**
- Produces (F1 and the action rely on these): constants `OK, NOT_OK, UNDECLARED, OUTSIDE, COULD_NOT_RUN = 0, 1, 2, 3, 4`,
  `TRAILER_RUN, TRAILER_FILE, TRAILER_ID`, `MAX_IDS = 50`; exception `Undeclared`;
  `validate(doc) -> dict` (`paths, select, runner, run, minutes, tools`); `declaration(repo, ref) -> dict`;
  `red(repo, commit, env, say) -> (int, dict)` (`result` ∈ `contract_ready`, `contract_not_red`,
  `contract_outside_tests`, `not_testable`; `ids`, `files`, `why`, `rows`);
  `hold(repo, env, say) -> (int, dict)` (`result` ∈ `green`, `red`, `could_not_run`, `no_contract`; `failing`);
  `ci(repo, base, head, env, say) -> (int, dict)` (`result` ∈ `green`, `red`; `why`, `rows`);
  `contract_of(repo, base) -> dict | None` (`commit, run, files, ids`); `summary(res) -> str`;
  `main(argv) -> int`. CLI: `test-contract [--repo DIR] [--json FILE] red [--commit SHA] | hold | ci --base SHA --head SHA [--summary FILE]`.

- [ ] **Step 1: Write the failing test** `images/agent-harness/tests/test_test_contract.py`:

```python
"""test-contract (v3 feature 4, offline proof 1): the red check, the CI's judgement and the implementer's
hold, on two fixture repositories: one pytest, one of shell suites whose runner writes one testcase a file.

Each case builds an origin whose main declares the tests, clones it, and calls test_contract in-process.
"""
import importlib.util
import os
import subprocess
import sys
import tempfile
import unittest

import yaml

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)
import test_contract as tc  # noqa: E402

PYTEST_DECL = """\
version: 1
steps: [{name: tests, run: python3 -m pytest -q, ci: test}]
tests:
  paths: [tests/]
  select: ["tests/test_*.py"]
  runner: [tests/conftest.py]
  run: python3 -m pytest -q -p no:cacheprovider --junitxml="$AGENT_TEST_REPORT" "$@"
"""

# The shell runner: the host's run.sh contract in small. Files given: those only. One testcase a file;
# exit 77 is a skip.
RUN_SH = r"""#!/usr/bin/env bash
set -uo pipefail
report="${AGENT_TEST_REPORT:-}"
cases=""
for t in "$@"; do
  out="$(bash "$t" 2>&1)"; code=$?
  case "$code" in
    0) body="" ;;
    77) body="<skipped/>" ;;
    *) body="<failure/>" ;;
  esac
  cases="$cases<testcase classname=\"$t\" name=\"$t\">$body</testcase>"
done
[ -n "$report" ] && printf '<testsuite>%s</testsuite>\n' "$cases" > "$report"
exit 0
"""

SHELL_DECL = """\
version: 1
steps: [{name: suites, run: t/run.sh t/test-*.sh, ci: test}]
tests:
  paths: [t/]
  select: ["t/test-*.sh"]
  runner: [t/run.sh]
  run: t/run.sh "$@"
"""


def sh(cwd, *args):
    subprocess.run(args, cwd=cwd, check=True, capture_output=True)


class Fixture:
    decl = PYTEST_DECL
    files = {"src/calc.py": "def add(a, b):\n    return a + b\n",
             "tests/test_calc.py": "from src.calc import add\n\ndef test_add():\n    assert add(1, 2) == 3\n",
             "tests/conftest.py": "import sys, os\nsys.path.insert(0, os.path.dirname(os.path.dirname(__file__)))\n",
             "src/__init__.py": ""}

    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        seed = os.path.join(self.tmp, "seed")
        os.makedirs(seed)
        sh(seed, "git", "init", "-q", "-b", "main")
        for k, v in ("user.name", "t"), ("user.email", "t@t"):
            sh(seed, "git", "config", k, v)
        self.write(seed, {".github/agent-checks.yaml": self.decl, **self.files})
        sh(seed, "git", "add", "-A")
        sh(seed, "git", "commit", "-qm", "base")
        sh(self.tmp, "git", "clone", "-q", "--bare", seed, "origin")
        self.repo = os.path.join(self.tmp, "repo")
        sh(self.tmp, "git", "clone", "-q", "origin", self.repo)
        for k, v in ("user.name", "t"), ("user.email", "t@t"):
            sh(self.repo, "git", "config", k, v)
        sh(self.repo, "git", "checkout", "-qb", "agent/task")
        self.base = self.rev("origin/main")

    def write(self, root, files):
        for path, body in files.items():
            full = os.path.join(root, path)
            os.makedirs(os.path.dirname(full), exist_ok=True)
            with open(full, "w") as f:
                f.write(body)
            if path.endswith(".sh"):
                os.chmod(full, 0o755)

    def commit(self, files, msg="change", trailers=()):
        self.write(self.repo, files)
        sh(self.repo, "git", "add", "-A")
        args = ["git", "commit", "-qm", msg]
        for t in trailers:
            args += ["--trailer", t]
        sh(self.repo, *args)
        return self.rev("HEAD")

    def rev(self, ref):
        return subprocess.run(["git", "-C", self.repo, "rev-parse", ref], capture_output=True, text=True).stdout.strip()

    def red(self):
        return tc.red(self.repo, self.rev("HEAD"), dict(os.environ), lambda m: None)

    def ci(self):
        return tc.ci(self.repo, self.base, self.rev("HEAD"), dict(os.environ), lambda m: None)


SUB = "tests/test_sub.py"
SUB_TEST = "from src.calc import sub\n\ndef test_sub():\n    assert sub(3, 2) == 1\n"
SUB_IMPL = {"src/calc.py": "def add(a, b):\n    return a + b\n\ndef sub(a, b):\n    return a - b\n"}


@unittest.skipUnless(importlib.util.find_spec("pytest"), "pytest is not installed (the image's test stage has it)")
class PytestTest(Fixture, unittest.TestCase):
    def test_a_new_test_red_at_the_overlay_and_green_at_the_head_is_a_contract_then_green(self):
        self.commit({SUB: SUB_TEST})
        code, res = self.red()
        self.assertEqual((code, res["result"]), (tc.OK, "contract_ready"), res)
        self.assertEqual(res["ids"], ["tests.test_sub"])  # the import fails: pytest's collection error, the file's id
        self.commit(SUB_IMPL)
        code, res = self.ci()
        self.assertEqual((code, res["result"]), (tc.OK, "green"), res)

    def test_a_new_test_that_passes_at_the_overlay_is_vacuous(self):
        self.commit({"tests/test_more.py": "from src.calc import add\n\ndef test_more():\n    assert add(2, 2) == 4\n"})
        code, res = self.red()
        self.assertEqual((code, res["result"]), (tc.NOT_OK, "contract_not_red"), res)
        self.assertIn("pass before any change", res["why"])
        self.assertEqual(self.ci()[0], tc.NOT_OK)

    def test_a_skip_at_the_head_is_no_pass(self):
        self.commit({SUB: "import pytest\nfrom src import calc\n\ndef test_sub():\n"
                          "    if not hasattr(calc, 'sub'):\n        assert False\n    pytest.skip('later')\n"})
        self.commit(SUB_IMPL)
        code, res = self.ci()
        self.assertEqual(code, tc.NOT_OK)
        self.assertTrue(any("does not pass at the head (skip)" in w for w in res["why"]), res)

    def test_a_changed_expectation_counts_as_fail_to_pass(self):
        self.commit({"tests/test_calc.py": "from src.calc import add\n\ndef test_add():\n    assert add(1, 2) == 4\n"})
        code, res = self.red()
        self.assertEqual((code, res["result"]), (tc.OK, "contract_ready"), res)
        self.commit({"src/calc.py": "def add(a, b):\n    return a + b + 1\n"})
        self.assertEqual(self.ci()[0], tc.OK)

    def test_no_changed_test_is_green_reporting_none(self):
        self.commit(SUB_IMPL)
        code, res = self.ci()
        self.assertEqual((code, res["why"]), (tc.OK, ["no changed test file"]))

    def test_a_changed_runner_file_is_red(self):
        self.commit({SUB: SUB_TEST, "tests/conftest.py": "import pytest\n"})
        self.assertEqual(self.ci()[1]["result"], "red")
        code, res = self.red()
        self.assertEqual((code, res["result"]), (tc.OUTSIDE, "contract_outside_tests"))

    def test_a_tester_commit_outside_the_test_paths_is_refused(self):
        self.commit({SUB: SUB_TEST, **SUB_IMPL})
        code, res = self.red()
        self.assertEqual((code, res["outside"]), (tc.OUTSIDE, ["src/calc.py"]))

    def test_a_head_that_edits_the_declaration_is_judged_by_the_bases(self):
        self.commit({"tests/test_more.py": "def test_more():\n    assert True\n",
                     ".github/agent-checks.yaml": PYTEST_DECL.replace('"tests/test_*.py"', '"tests/none_*.py"')})
        code, res = self.ci()
        self.assertEqual(code, tc.NOT_OK, res)
        self.assertEqual(res["selected"], ["tests/test_more.py"])

    def test_a_runner_that_could_not_run_is_never_green(self):
        self.commit({SUB: SUB_TEST})
        code, res = tc.red(self.repo, self.rev("HEAD"), {**os.environ, "PATH": "/nonexistent"}, lambda m: None)
        self.assertEqual((code, res["why"]), (tc.COULD_NOT_RUN, "the runner could not run"))
        self.commit(SUB_IMPL)
        code, res = tc.ci(self.repo, self.base, self.rev("HEAD"), {**os.environ, "PATH": "/nonexistent"}, lambda m: None)
        self.assertEqual(code, tc.NOT_OK)
        self.assertIn("could not run", res["why"][0])

    def test_the_implementer_is_held_to_the_contract_commit(self):
        self.commit({SUB: SUB_TEST}, "test: contract", ["Agent-Contract: run-1", "Agent-Contract-File: " + SUB,
                                                         "Agent-Contract-Id: tests.test_sub"])
        code, res = tc.hold(self.repo, dict(os.environ), lambda m: None)
        self.assertEqual((code, res["failing"]), (tc.NOT_OK, ["tests.test_sub"]))
        self.commit(SUB_IMPL)
        code, res = tc.hold(self.repo, dict(os.environ), lambda m: None)
        self.assertEqual((code, res["result"], res["files"]), (tc.OK, "green", [SUB]))

    def test_no_contract_commit_holds_nothing(self):
        self.commit(SUB_IMPL)
        self.assertEqual(tc.hold(self.repo, dict(os.environ), lambda m: None)[0], tc.UNDECLARED)


class ShellTest(Fixture, unittest.TestCase):
    decl = SHELL_DECL
    files = {"t/run.sh": RUN_SH, "bin/greet": "#!/usr/bin/env bash\necho hello\n",
             "t/test-greet.sh": 'out="$(bash bin/greet)"; [ "$out" = hello ]\n'}

    def test_a_suite_is_one_id_its_file(self):
        self.commit({"t/test-name.sh": 'out="$(bash bin/greet ada)"; [ "$out" = "hello ada" ]\n'})
        code, res = self.red()
        self.assertEqual((code, res["ids"]), (tc.OK, ["t/test-name.sh"]), res)
        self.commit({"bin/greet": '#!/usr/bin/env bash\necho "hello${1:+ $1}"\n'})
        code, res = self.ci()
        self.assertEqual((code, res["result"]), (tc.OK, "green"), res)

    def test_a_suite_that_exits_77_is_a_skip_and_never_a_pass(self):
        self.commit({"t/test-later.sh": "exit 77\n"})
        self.assertEqual(self.red()[1]["result"], "contract_not_red")
        self.assertTrue(any("does not pass at the head (skip)" in w for w in self.ci()[1]["why"]))

    def test_an_undeclared_repository_is_exit_2(self):
        self.commit({".github/agent-checks.yaml": "version: 1\n"})
        sh(self.repo, "git", "push", "-q", "origin", "HEAD:main")
        sh(self.repo, "git", "fetch", "-q", "origin")
        self.assertEqual(tc.main(["--repo", self.repo, "red"]), tc.UNDECLARED)


class DeclarationTest(unittest.TestCase):
    TESTS = {"paths": ["t/"], "select": ["t/test-*.sh"], "runner": ["t/run.sh"], "run": 't/run.sh "$@"'}

    def test_the_tests_section_is_checked_strictly(self):
        doc = {"version": 1, "tests": dict(self.TESTS)}
        self.assertEqual(tc.validate(doc)["minutes"], tc.DEFAULT_MINUTES)
        for bad, why in (({"paths": []}, "paths"), ({"select": "t/*"}, "select"), ({"runner": ["/abs"]}, "runner"),
                         ({"run": " "}, "no run"), ({"timeoutMinutes": 16}, "timeoutMinutes"), ({"selcet": []}, "unknown keys")):
            with self.assertRaisesRegex(tc.Undeclared, why):
                tc.validate({"version": 1, "tests": {**self.TESTS, **bad}})

    @unittest.skipUnless(importlib.util.find_spec("agent_checks"), "agent_checks.py (v3 feature 3) is not beside this one")
    def test_agent_checks_accepts_the_section_and_keeps_its_steps(self):
        import agent_checks
        doc = {"version": 1, "steps": [{"name": "suites", "run": "true", "ci": "test"}], "tests": dict(self.TESTS)}
        self.assertEqual([s["name"] for s in agent_checks.validate(doc)["steps"]], ["suites"])


ACTION = os.path.join(HERE, "..", "..", ".github", "actions", "test-contract", "action.yml")


@unittest.skipUnless(os.path.exists(ACTION), "the composite action is not in this tree (the image's test stage)")
class ActionTest(unittest.TestCase):
    def test_the_action_runs_this_script_with_its_inputs_through_the_environment(self):
        with open(ACTION) as f:
            step = yaml.safe_load(f)["runs"]["steps"][0]
        script = step["env"]["SCRIPT"].replace("${{ github.action_path }}", os.path.dirname(ACTION))
        self.assertTrue(os.path.samefile(script, os.path.join(HERE, "test_contract.py")))
        self.assertNotIn("${{", step["run"], "inputs reach the shell through env, never inline")


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Run it to see it fail**

Run: `cd images/agent-harness && python3 -m unittest tests.test_test_contract -v`
Expected: `ModuleNotFoundError: No module named 'test_contract'`.

- [ ] **Step 3: Write `images/agent-harness/test_contract.py`**

```python
#!/agent-server/.venv/bin/python
"""test-contract: the test-first tester's contract check (v3 feature 4, TF5 to TF7).

One script for the sandbox and the CI, so they agree. The repository declares its tests in the
`tests` section of .github/agent-checks.yaml, always read from the base, never from the change:

  tests:
    paths:  [scripts/ci/tests/]                 # what a tester may write; a prefix ending in / or a path
    select: ["scripts/ci/tests/test-*.sh"]      # globs: the changed files whose tests are run
    runner: [scripts/ci/tests/run.sh]           # the runner's own files: never a contract, red when changed
    run: scripts/ci/tests/run.sh "$@"           # bash; "$@" the selected files; writes JUnit XML to $AGENT_TEST_REPORT
    timeoutMinutes: 5

  test-contract red [--commit SHA]   the tester's red check: base = the default branch (origin/HEAD)
  test-contract hold                 the implementer's contract, run at HEAD
  test-contract ci --base SHA --head SHA [--summary FILE]   the "Test contract" job

An id is a JUnit testcase's `classname::name`. A suite with no finer report writes one testcase, its
file. The runner ran when its report exists and holds a testcase: an infrastructure failure is never
red. The overlay is the base's tree with the changed test files at their new content.
"""
import argparse
import contextlib
import fnmatch
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
import xml.etree.ElementTree as ET

import yaml

DECLARATION = ".github/agent-checks.yaml"
DEFAULT_BRANCH = "refs/remotes/origin/HEAD"
DEFAULT_MINUTES, MAX_MINUTES = 5, 15
HEARTBEAT_S = 15
MAX_IDS = 50
# Exit codes, the harness's ends (TF5) and the job's conclusion (TF7).
OK, NOT_OK, UNDECLARED, OUTSIDE, COULD_NOT_RUN = 0, 1, 2, 3, 4
# The runner's shell, resolved from the caller's PATH: a step's PATH is the toolchain's.
BASH = shutil.which("bash") or "/bin/bash"
TRAILER_RUN, TRAILER_FILE, TRAILER_ID = "Agent-Contract", "Agent-Contract-File", "Agent-Contract-Id"


class Undeclared(Exception):
    """The base declares no tests: exit 2."""


def git(repo: str, *args: str, check: bool = True) -> str:
    out = subprocess.run(["git", "-C", repo, *args], capture_output=True, text=True)
    if check and out.returncode:
        raise RuntimeError("git %s: %s" % (" ".join(args), out.stderr.strip()))
    return out.stdout


def validate(doc) -> dict:
    """The declaration's tests section, checked as each repository's coverage suite checks it."""
    def need(cond, why):
        if not cond:
            raise Undeclared("%s: tests: %s" % (DECLARATION, why))
    need(isinstance(doc, dict) and isinstance(doc.get("tests"), dict), "no tests section")
    t = doc["tests"]
    need(not set(t) - {"paths", "select", "runner", "run", "timeoutMinutes"}, "unknown keys %s" % sorted(set(t) - {"paths", "select", "runner", "run", "timeoutMinutes"}))
    for key in ("paths", "select", "runner"):
        v = t.get(key)
        need(isinstance(v, list) and v and all(isinstance(p, str) and p and not p.startswith("/") for p in v),
             "%s must be a non-empty list of relative paths" % key)
    need(isinstance(t.get("run"), str) and t["run"].strip(), "no run")
    m = t.get("timeoutMinutes", DEFAULT_MINUTES)
    need(isinstance(m, int) and 1 <= m <= MAX_MINUTES, "timeoutMinutes must be 1 to %d" % MAX_MINUTES)
    return {"paths": t["paths"], "select": t["select"], "runner": t["runner"], "run": t["run"], "minutes": m,
            "tools": doc.get("tools", [])}


def declaration(repo: str, ref: str) -> dict:
    """The tests section at ref: the base's, as policy-bot reads .policy.yml from main."""
    raw = git(repo, "show", "%s:%s" % (ref, DECLARATION), check=False)
    if not raw:
        raise Undeclared("%s declares no checks at %s" % (DECLARATION, ref[:12]))
    try:
        return validate(yaml.safe_load(raw))
    except yaml.YAMLError as exc:
        raise Undeclared("%s: %s" % (DECLARATION, exc)) from exc


def under(path: str, prefixes: list[str]) -> bool:
    return any(path == p or (p.endswith("/") and path.startswith(p)) for p in prefixes)


def changes(repo: str, base: str, head: str) -> list[tuple[str, str]]:
    """(status, path) the change makes since its merge base with base; a rename is a delete and an add."""
    out = git(repo, "diff", "--name-status", "--no-renames", "-z", "%s...%s" % (base, head))
    parts = out.split("\0")
    return [(parts[i][0], parts[i + 1]) for i in range(0, len(parts) - 1, 2)]


@contextlib.contextmanager
def tree(repo: str, sha: str, head: str | None = None, files=(), gone=()):
    """A clean, detached checkout of sha; with head, the overlay: files at head's content, gone removed."""
    where = tempfile.mkdtemp(prefix="test-contract-")
    git(repo, "worktree", "add", "--detach", "-q", where, sha)
    try:
        if files:
            git(where, "checkout", head, "--", *files)
        if gone:
            git(where, "rm", "-q", "--ignore-unmatch", "--", *gone)
        yield where
    finally:
        git(repo, "worktree", "remove", "--force", where, check=False)
        shutil.rmtree(where, ignore_errors=True)


def junit(path: str) -> dict[str, str] | None:
    """{id: pass | fail | skip} from a JUnit XML report; None when there is none or it holds no testcase."""
    try:
        root = ET.parse(path).getroot()
    except (OSError, ET.ParseError):
        return None
    ids: dict[str, str] = {}
    for case in root.iter("testcase"):
        cls, name = case.get("classname") or "", case.get("name") or ""
        key = "%s::%s" % (cls, name) if cls and cls != name else name
        kinds = {child.tag for child in case}
        result = "fail" if kinds & {"failure", "error"} else "skip" if "skipped" in kinds else "pass"
        if ids.get(key) != "fail":  # a duplicate id that fails once fails
            ids[key] = result
    return ids or None


def run(where: str, decl: dict, files: list[str], env: dict, label: str, say) -> dict:
    """The declared runner on files in where: {ran, ids, seconds, exit, tail}."""
    report = os.path.join(tempfile.mkdtemp(prefix="test-contract-report-"), "report.xml")
    start = time.monotonic()
    proc = subprocess.Popen([BASH, "-c", decl["run"], "agent-tests", *files], cwd=where,
                            env={**env, "AGENT_TEST_REPORT": report}, stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, errors="replace",
                            start_new_session=True)
    done = threading.Event()

    def beat():  # the SDK's terminal pauses after 30 s without output
        while not done.wait(HEARTBEAT_S):
            say("%s running, %ds" % (label, time.monotonic() - start))
    threading.Thread(target=beat, daemon=True).start()
    try:
        out, _ = proc.communicate(timeout=decl["minutes"] * 60)
    except subprocess.TimeoutExpired:
        os.killpg(proc.pid, signal.SIGKILL)
        out, _ = proc.communicate()
    finally:
        done.set()
    ids = junit(report)
    shutil.rmtree(os.path.dirname(report), ignore_errors=True)
    return {"ran": ids is not None, "ids": ids or {}, "seconds": round(time.monotonic() - start, 1),
            "exit": proc.returncode, "tail": out.splitlines()[-30:]}


def plan(repo: str, decl: dict, base: str, head: str) -> dict:
    """What the change does to the tests: the selected files, the overlay's inputs, what lies outside."""
    ch = changes(repo, base, head)
    tests = [(s, p) for s, p in ch if under(p, decl["paths"]) and not under(p, decl["runner"])]
    return {
        "changed": [p for _, p in ch],
        "outside": [p for _, p in ch if not under(p, decl["paths"]) or under(p, decl["runner"])],
        "runner": [p for _, p in ch if under(p, decl["runner"])],
        "files": [p for s, p in tests if s != "D"],
        "gone": [p for s, p in tests if s == "D"],
        "selected": sorted(p for s, p in tests if s != "D" and any(fnmatch.fnmatchcase(p, g) for g in decl["select"])),
    }


def at_base(repo: str, decl: dict, base: str, selected: list[str], env: dict, say) -> dict:
    """The base's ids of the selected files that exist there: what a new id is new against."""
    existing = [p for p in selected
                if subprocess.run(["git", "-C", repo, "cat-file", "-e", "%s:%s" % (base, p)], capture_output=True).returncode == 0]
    if not existing:
        return {"ran": True, "ids": {}, "seconds": 0, "exit": 0, "tail": []}
    with tree(repo, base) as where:
        return run(where, decl, existing, env, "base", say)


def result_of(i: str, ids: dict[str, str]) -> str:
    """i's result in ids. A file that fails to load reports one id for the file (pytest's collection
    error, `tests.test_x`): it stands for every id under it, and an id stands for the ids under it."""
    if i in ids:
        return ids[i]
    under_i = [r for k, r in ids.items() if k.startswith(i + "::") or k.startswith(i + ".")]
    if not under_i:
        parent = [r for k, r in ids.items() if r == "fail" and (i.startswith(k + "::") or i.startswith(k + "."))]
        return "fail" if parent else "-"
    return "fail" if "fail" in under_i else "skip" if "skip" in under_i else "pass"


def judge(base_r: dict, over: dict, head_r: dict | None) -> list[dict]:
    """One row per id: its result at the base, the overlay and (in CI) the head, and whether it is new.
    In CI the rows are the head's ids; a load error at the overlay counts against each id under it."""
    names = set(head_r["ids"]) if head_r else set(over["ids"])
    return [{"id": i, "new": result_of(i, base_r["ids"]) == "-", "base": result_of(i, base_r["ids"]),
             "overlay": result_of(i, over["ids"]), "head": head_r["ids"][i] if head_r else None}
            for i in sorted(names)]


def red(repo: str, commit: str, env: dict, say) -> tuple[int, dict]:
    """TF5: every changed path a test path, the runner ran, at the overlay one id fails and no new id passes."""
    base = git(repo, "rev-parse", DEFAULT_BRANCH + "^{commit}").strip()
    decl = declaration(repo, base)
    p = plan(repo, decl, base, commit)
    res = {"base": base, "commit": commit, "files": p["files"], "selected": p["selected"]}
    if not p["changed"]:
        return NOT_OK, {**res, "result": "not_testable", "why": "the commit changes nothing"}
    if p["outside"] or p["gone"]:
        return OUTSIDE, {**res, "result": "contract_outside_tests", "outside": p["outside"] + p["gone"],
                         "why": "a contract adds or changes files under %s only, never the runner's, and deletes none"
                         % ", ".join(decl["paths"])}
    if not p["selected"]:
        return NOT_OK, {**res, "result": "contract_not_red", "why": "no changed file is a test file (%s)" % ", ".join(decl["select"])}
    b = at_base(repo, decl, base, p["selected"], env, say)
    with tree(repo, base, commit, p["files"]) as where:
        o = run(where, decl, p["selected"], env, "overlay", say)
    rows = judge(b, o, None)
    res["rows"] = rows
    if not (b["ran"] and o["ran"]):
        return COULD_NOT_RUN, {**res, "result": "contract_not_red", "why": "the runner could not run", "tail": o["tail"]}
    fails = [r["id"] for r in rows if r["overlay"] == "fail"]
    vacuous = [r["id"] for r in rows if r["new"] and r["overlay"] == "pass"]
    if vacuous:
        return NOT_OK, {**res, "result": "contract_not_red", "why": "new tests pass before any change: %s" % ", ".join(vacuous[:10])}
    if not fails:
        return NOT_OK, {**res, "result": "contract_not_red", "why": "no test fails on the base plus the tests"}
    return OK, {**res, "result": "contract_ready", "ids": fails[:MAX_IDS]}


def ci(repo: str, base: str, head: str, env: dict, say) -> tuple[int, dict]:
    """TF7: the runner ran both times, no new id passes at the overlay, every id passes at the head
    (a skip is no pass), one id fails at the overlay and passes at the head, no runner file changed."""
    decl = declaration(repo, base)
    p = plan(repo, decl, base, head)
    res = {"base": base, "head": head, "selected": p["selected"], "runner": p["runner"]}
    if p["runner"]:
        return NOT_OK, {**res, "result": "red", "why": ["the runner's own files changed: %s" % ", ".join(p["runner"])]}
    if not p["selected"]:
        return OK, {**res, "result": "green", "why": ["no changed test file"]}
    b = at_base(repo, decl, base, p["selected"], env, say)
    with tree(repo, base, head, p["files"], p["gone"]) as where:
        o = run(where, decl, p["selected"], env, "overlay", say)
    with tree(repo, head) as where:
        h = run(where, decl, p["selected"], env, "head", say)
    rows = judge(b, o, h)
    res["rows"] = rows
    why = []
    if not (b["ran"] and o["ran"] and h["ran"]):
        why.append("the runner could not run (%s)" % ", ".join(n for n, x in (("base", b), ("overlay", o), ("head", h)) if not x["ran"]))
    else:
        why += ["new test passes on the base plus the tests: %s" % r["id"] for r in rows if r["new"] and r["overlay"] == "pass"]
        why += ["does not pass at the head (%s): %s" % (r["head"], r["id"]) for r in rows if r["head"] != "pass"]
        if not any(r["overlay"] == "fail" and r["head"] == "pass" for r in rows):
            why.append("no test fails on the base plus the tests and passes at the head")
    return (NOT_OK if why else OK), {**res, "result": "red" if why else "green", "why": why or ["fail-to-pass holds"]}


def contract_of(repo: str, base: str) -> dict | None:
    """The contract commit on the branch: the first commit since base the tester's harness signed."""
    for sha in git(repo, "rev-list", "--reverse", "%s..HEAD" % base).split():
        msg = git(repo, "show", "-s", "--format=%B", sha)
        trailers = subprocess.run(["git", "-C", repo, "interpret-trailers", "--parse", "--no-divider"],
                                  input=msg, capture_output=True, text=True).stdout.splitlines()
        values = {}
        for t in trailers:
            key, _, value = t.partition(":")
            values.setdefault(key, []).append(value.strip())
        if values.get(TRAILER_RUN):
            return {"commit": sha, "run": values[TRAILER_RUN][0], "files": values.get(TRAILER_FILE, []),
                    "ids": values.get(TRAILER_ID, [])}
    return None


def hold(repo: str, env: dict, say) -> tuple[int, dict]:
    """TF6: the contract's tests at HEAD, on a clean checkout: every contract id passes."""
    base = git(repo, "rev-parse", DEFAULT_BRANCH + "^{commit}").strip()
    c = contract_of(repo, base)
    if not c:
        return UNDECLARED, {"result": "no_contract"}
    decl = declaration(repo, base)
    head = git(repo, "rev-parse", "HEAD").strip()
    selected = sorted(p for p in c["files"] if any(fnmatch.fnmatchcase(p, g) for g in decl["select"]))
    with tree(repo, head) as where:
        h = run(where, decl, selected, env, "contract", say)
    if not h["ran"]:
        return COULD_NOT_RUN, {**c, "head": head, "result": "could_not_run", "tail": h["tail"]}
    failing = sorted(i for i in c["ids"] if result_of(i, h["ids"]) != "pass")
    return (NOT_OK if failing else OK), {**c, "head": head, "result": "red" if failing else "green", "failing": failing}


def summary(res: dict) -> str:
    """The check run's summary: each id's results, the reasons."""
    lines = ["### Test contract: %s" % res["result"], ""]
    lines += ["- %s" % w for w in res.get("why", [])]
    if res.get("rows"):
        lines += ["", "| id | new | base | base + tests | head |", "|---|---|---|---|---|"]
        lines += ["| `%s` | %s | %s | %s | %s |" % (r["id"], "yes" if r["new"] else "", r["base"], r["overlay"], r["head"] or "")
                  for r in res["rows"]]
    return "\n".join(lines) + "\n"


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(prog="test-contract")
    sub = ap.add_subparsers(dest="mode", required=True)
    r = sub.add_parser("red")
    r.add_argument("--commit", default="HEAD")
    sub.add_parser("hold")
    c = sub.add_parser("ci")
    c.add_argument("--base", required=True)
    c.add_argument("--head", required=True)
    c.add_argument("--summary")
    ap.add_argument("--repo", default=os.environ.get("AGENT_CHECKS_REPO", "/workspace/repo"))
    ap.add_argument("--json", help="write the result here")
    args = ap.parse_args(argv)

    def say(m):
        print("test-contract: " + m, file=sys.stderr, flush=True)
    env = dict(os.environ)
    try:
        if args.mode == "red":
            code, res = red(args.repo, git(args.repo, "rev-parse", args.commit).strip(), env, say)
        elif args.mode == "hold":
            code, res = hold(args.repo, env, say)
        else:
            code, res = ci(args.repo, args.base, args.head, env, say)
    except Undeclared as exc:
        say(str(exc))
        return UNDECLARED
    if args.json:
        with open(args.json, "w") as f:
            json.dump(res, f, indent=1)
    if args.mode == "ci" and args.summary:
        with open(args.summary, "a") as f:
            f.write(summary(res))
    print(json.dumps({k: v for k, v in res.items() if k != "tail"}, indent=1))
    return code


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
```

`chmod 0755 images/agent-harness/test_contract.py`.

- [ ] **Step 4: Run the tests**

Run: `cd images/agent-harness && python3 -m unittest tests.test_test_contract -v` (with pytest and PyYAML
installed, e.g. `$SCRATCH/harness-venv` plus `pip install pytest==9.1.1`).
Expected: `Ran 17 tests … OK`, skipping only `test_agent_checks_accepts_the_section_and_keeps_its_steps`
until Task 2, and `ActionTest` until Task 3.

- [ ] **Step 5: Commit**

```bash
git add images/agent-harness/test_contract.py images/agent-harness/tests/test_test_contract.py
git commit -m "feat(harness): test-contract, the test-first tester's contract check"
```

### Task 2: `agent-checks` accepts the `tests` section (R2, R22)

**Files:** Modify `images/agent-harness/agent_checks.py` (feature 3 PR 1).

- [ ] **Step 1: The failing test** is Task 1's `DeclarationTest.test_agent_checks_accepts_the_section_and_keeps_its_steps`,
  now that `agent_checks.py` sits beside the script. Run: `python3 -m unittest tests.test_test_contract.DeclarationTest -v`
  Expected: FAIL, `Undeclared: .github/agent-checks.yaml: unknown keys ['tests']`.
- [ ] **Step 2: The change**, in `validate`:

```diff
-    extra = set(doc) - {"version", "timeoutMinutes", "tools", "definedBy", "steps", "ciOnly"}
+    # tests is test-contract's section (v3 feature 4), checked by test_contract.validate where it is used.
+    extra = set(doc) - {"version", "timeoutMinutes", "tools", "definedBy", "steps", "ciOnly", "tests"}
```

- [ ] **Step 3:** `python3 -m unittest tests.test_test_contract tests.test_agent_checks -v` → OK (feature 3's 18 tests unchanged).
- [ ] **Step 4:** `git add images/agent-harness/agent_checks.py && git commit -m "feat(harness): agent-checks leaves the tests section to test-contract"`

### Task 3: The image and the composite action

**Files:** Modify `images/agent-harness/Dockerfile`; create `images/agent-harness/tests/contract-requirements.txt`,
`.github/actions/test-contract/action.yml`.

- [ ] **Step 1: The failing test** is `ActionTest` (skipped while the action is absent) and, in the image,
  the pytest fixture (skipped without pytest). Create the action, `.github/actions/test-contract/action.yml`:

```yaml
# test-contract in CI (v3 feature 4, TF7): the same script the harness runs for the tester's red
# check, so the sandbox and the CI agree. Pin it by commit. Needs python3 with PyYAML, git history
# holding both commits, and the repository's toolchain on PATH.
name: test-contract
description: Every test a pull request adds fails on the base plus the tests and passes at the head.
inputs:
  base:
    description: The base branch's commit; the tests' declaration is read there.
    required: true
  head:
    description: The pull request's head commit.
    required: true
runs:
  using: composite
  steps:
    - name: test-contract ci
      shell: bash
      env:
        BASE: ${{ inputs.base }}
        HEAD_SHA: ${{ inputs.head }}
        SCRIPT: ${{ github.action_path }}/../../../images/agent-harness/test_contract.py
      run: python3 "$SCRIPT" --repo "$GITHUB_WORKSPACE" ci --base "$BASE" --head "$HEAD_SHA" --summary "$GITHUB_STEP_SUMMARY"
```

- [ ] **Step 2: The image.** In the `harness` stage, the copy and the links (after feature 3's change to the same lines):

```dockerfile
COPY --chmod=0755 agent_run.py agent_checks.py test_contract.py git_credential_agent.py pr_footer.py /opt/agent/
…
RUN ln -s /opt/agent/agent_run.py /usr/local/bin/agent-run \
 && ln -s /opt/agent/agent_checks.py /usr/local/bin/agent-checks \
 && ln -s /opt/agent/test_contract.py /usr/local/bin/test-contract \
 && ln -s /opt/agent/git_credential_agent.py /usr/local/bin/git-credential-agent
```

  The `test` stage installs pytest for the pytest fixture, hash-pinned, in the test image only:

```dockerfile
FROM harness AS test
COPY --chown=10001:10001 tests /opt/agent/tests
# pytest for test-contract's pytest fixture (v3 feature 4): the test stage only, never the shipped image.
RUN /usr/local/bin/python -m pip install --no-cache-dir --require-hashes --only-binary=:all: \
      -r /opt/agent/tests/contract-requirements.txt
RUN cd /opt/agent && /agent-server/.venv/bin/python -m unittest discover -s tests -v
```

  `images/agent-harness/tests/contract-requirements.txt` (`uv pip compile --python-version 3.13 --generate-hashes` of `pytest==9.1.1`):

```text
iniconfig==2.3.1 \
    --hash=sha256:67f4b9c50da0dedf52af349e7749a80a9057a5031199791b906c3bb3ae878960 \
    --hash=sha256:9121e2c1fdb355232495be3194c8dfe87ccc2d5dee45947b78e68f499790d7a7
    # via pytest
packaging==26.3 \
    --hash=sha256:94edc256424af38762eb31306eed28beb9f0efc50a8837492c9d6fd6004aed79 \
    --hash=sha256:d7193f7c8e4e93f444fde0262bf90af30e16fa0ad0ad44cb553c87339b23cd1c
    # via pytest
pluggy==1.7.0 \
    --hash=sha256:7dd7b0d8832ba3cb632c306926ded123429211b83641b35dc5c41ad2d34f9bec \
    --hash=sha256:d1eaa46ebb595891b860ab086b4d09c8588af65ebd4361b8e8f4bb8920b90ba8
    # via pytest
pygments==2.21.0 \
    --hash=sha256:2363c69b61c4a97c838da3b130dcd6468f4848992b21a82f2a63ec34377137d9 \
    --hash=sha256:610ca751c9bc2492b38eb9a38a7fbc93edbbb2d7182edaf34e66ae493dee5c8c
    # via pytest
pytest==9.1.1 \
    --hash=sha256:1088fbde8f2b49d95a549a195707afa7a76a3ce9bcadc26b6d71f0ffda5fe313 \
    --hash=sha256:37a86b45efb9a47a61a36449063e8e18d0cab3161329fc099eb21783169c4f0c
    # via -r req.in
```

  The fixtures' `run` line calls `python3 -m pytest`, which is `/usr/local/bin/python3` in the image.
- [ ] **Step 3:** `python3 -m unittest tests.test_test_contract -v` from `images/agent-harness` → `ActionTest` passes.
  `task check` → exit 0. Push; CI's `harness-test` runs the image's test stage: the pytest fixture runs there.
- [ ] **Step 4:** `git add .github/actions/test-contract/action.yml images/agent-harness/Dockerfile images/agent-harness/tests/contract-requirements.txt && git commit -m "feat(harness): test-contract in the image and as a composite action"`

### Task 4: The README, the pull request and the release

- [ ] **Step 1:** `images/agent-harness/README.md`, in the file table after `agent_checks.py`'s row:

```markdown
| `test_contract.py` → `test-contract` | v3 feature 4: whether a change's tests are a contract. Reads the `tests` section of `.github/agent-checks.yaml` from the base, runs the selected test files at the base, at the base plus the changed tests (the overlay) and at the head, each in a clean worktree, and compares JUnit ids. `red` is the tester's red check, `hold` the implementer's contract run, `ci` the "Test contract 🧪" job through `.github/actions/test-contract` |
```

- [ ] **Step 2:** `task check` → exit 0; commit `docs(harness): test-contract`; push; open the pull request
  (English, no trailers), citing the 17 tests and CI's `harness-test`.
- [ ] **Step 3:** After merge, cut the release (`CONTRIBUTING.md`) and note **the merge commit's 40-hex
  sha and the tag**: PR 2 pins the action to that sha.

---

## PR 2 — cloud-native-ref: the runner, the declaration and the job

Branch `feat/test-contract-report-only` from a freshly fetched `origin/main`, after feature 3's host
PR 2 merged (the declaration, the coverage suite, `ai-overlay.sh --from-git`). Every file here but
`run.sh` and its suite is a gate path: human-authored.

### Task 5: `run.sh` runs a selection and writes JUnit

**Files:** Create `scripts/ci/tests/test-run-selection.sh`; modify `scripts/ci/tests/run.sh`.

- [ ] **Step 1: The failing suite** `scripts/ci/tests/test-run-selection.sh` (`chmod +x`):

```bash
#!/usr/bin/env bash
# requires: bash
#
# run.sh's two modes for test-contract (agent-platform v3 feature 4): suites named as arguments run
# alone, and AGENT_TEST_REPORT gets one JUnit testcase a suite, its path from the repository root,
# a SKIP as <skipped/> and a FAIL as <failure/>. With no argument every suite runs, as before.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/scripts/ci/tests/sub"
cp "$HERE/run.sh" "$TMP/scripts/ci/tests/run.sh"
printf 'exit 0\n' > "$TMP/scripts/ci/tests/test-pass.sh"
printf 'echo broken; exit 1\n' > "$TMP/scripts/ci/tests/test-fail.sh"
printf 'echo later; exit 77\n' > "$TMP/scripts/ci/tests/test-skip.sh"
printf '# requires: no-such-tool-anywhere\nexit 0\n' > "$TMP/scripts/ci/tests/test-needs.sh"
printf 'raise SystemExit(0)\n' > "$TMP/scripts/ci/tests/sub/test-deep.py"
fail=0
ok() { printf 'ok   %s\n' "$1"; }
no() { printf 'FAIL %s\n' "$1"; fail=1; }

cd "$TMP"
out="$(AGENT_TEST_REPORT="$TMP/r.xml" bash scripts/ci/tests/run.sh scripts/ci/tests/test-pass.sh scripts/ci/tests/test-fail.sh \
  scripts/ci/tests/test-skip.sh scripts/ci/tests/test-needs.sh scripts/ci/tests/sub/test-deep.py)" && no "a failing suite exits 1" || ok "a failing suite exits 1"
case "$out" in *"2 passed, 2 skipped, 1 failed"*) ok "the named suites run, and only they" ;; *) no "the named suites run: $out" ;; esac
r="$(cat "$TMP/r.xml" 2>/dev/null || true)"
for want in '<testcase classname="scripts/ci/tests/test-pass.sh" name="scripts/ci/tests/test-pass.sh"></testcase>' \
            '<testcase classname="scripts/ci/tests/test-fail.sh" name="scripts/ci/tests/test-fail.sh"><failure/></testcase>' \
            '<testcase classname="scripts/ci/tests/test-skip.sh" name="scripts/ci/tests/test-skip.sh"><skipped/></testcase>' \
            '<testcase classname="scripts/ci/tests/test-needs.sh" name="scripts/ci/tests/test-needs.sh"><skipped/></testcase>' \
            '<testcase classname="scripts/ci/tests/sub/test-deep.py" name="scripts/ci/tests/sub/test-deep.py"></testcase>'; do
  case "$r" in *"$want"*) ok "report: ${want:20:48}" ;; *) no "report lacks $want" ;; esac
done
out="$(bash scripts/ci/tests/run.sh scripts/ci/tests/test-pass.sh)" || no "one passing suite exits 0"
case "$out" in *"1 passed, 0 skipped, 0 failed"*) ok "one named suite runs alone" ;; *) no "one suite: $out" ;; esac
out="$(bash scripts/ci/tests/run.sh || true)"
case "$out" in *"2 passed, 2 skipped, 1 failed"*) ok "no argument runs every suite, as before" ;; *) no "every suite: $out" ;; esac
[ ! -e "$TMP/r2.xml" ] && ok "no AGENT_TEST_REPORT, no report" || no "a report without AGENT_TEST_REPORT"
exit "$fail"
```

  Run: `bash scripts/ci/tests/test-run-selection.sh` → exit 1, 7 `FAIL` lines.
- [ ] **Step 2: The change** to `scripts/ci/tests/run.sh`:

```diff
diff --git a/scripts/ci/tests/run.sh b/scripts/ci/tests/run.sh
index 8d5ae3a..c20e982 100755
--- a/scripts/ci/tests/run.sh
+++ b/scripts/ci/tests/run.sh
@@ -22,8 +22,21 @@ TESTS="${TESTS_DIR:-$HERE}"
 
 pass=0 skip=0 fail=0 rc=0
 
-for t in "$TESTS"/test-*.sh "$TESTS"/test-*.py "$TESTS"/*/test-*.py; do
+# Suites named as arguments run alone: test-contract's selection (agent-platform v3 feature 4).
+# With AGENT_TEST_REPORT set, each suite is one JUnit testcase, its path from the repository root.
+ROOT="$(cd "$HERE/../../.." && pwd)"
+REPORT="${AGENT_TEST_REPORT:-}"
+cases=""
+junit() { # junit <id> [skipped|failure]
+  [ -n "$REPORT" ] || return 0
+  cases="$cases<testcase classname=\"$1\" name=\"$1\">${2:+<$2/>}</testcase>"
+}
+[ "$#" -gt 0 ] || set -- "$TESTS"/test-*.sh "$TESTS"/test-*.py "$TESTS"/*/test-*.py
+
+for t in "$@"; do
   [ -e "$t" ] || continue
+  t="$(cd "$(dirname "$t")" && pwd)/$(basename "$t")"
+  id="${t#"$ROOT"/}"
   case "$t" in
     "$TESTS"/test-*.sh) name="$(basename "$t" .sh)"; interpreter=bash ;;
     "$TESTS"/test-*.py) name="$(basename "$t" .py)"; interpreter=python3 ;;
@@ -40,6 +53,7 @@ for t in "$TESTS"/test-*.sh "$TESTS"/test-*.py "$TESTS"/*/test-*.py; do
   done
   if [ -n "$missing" ]; then
     printf 'SKIP  %-42s missing:%s\n' "$name" "$missing"
+    junit "$id" skipped
     skip=$((skip + 1))
     continue
   fi
@@ -49,13 +63,16 @@ for t in "$TESTS"/test-*.sh "$TESTS"/test-*.py "$TESTS"/*/test-*.py; do
   code=$?
   if [ "$code" -eq 0 ]; then
     printf 'PASS  %-42s (%ds)\n' "$name" "$SECONDS"
+    junit "$id"
     pass=$((pass + 1))
   elif [ "$code" -eq 77 ]; then
     reason="${out##*$'\n'}"
     printf 'SKIP  %-42s %s\n' "$name" "${reason:-no reason given}"
+    junit "$id" skipped
     skip=$((skip + 1))
   else
     printf 'FAIL  %-42s (%ds)\n' "$name" "$SECONDS"
+    junit "$id" failure
     # The output, not just the name: a red check whose body is one word costs a
     # local re-run to learn anything from.
     printf '%s\n' "$out" | sed 's/^/      /'
@@ -65,6 +82,7 @@ for t in "$TESTS"/test-*.sh "$TESTS"/test-*.py "$TESTS"/*/test-*.py; do
 done
 
 printf '%d passed, %d skipped, %d failed\n' "$pass" "$skip" "$fail"
+[ -z "$REPORT" ] || printf '<testsuite name="scripts/ci/tests">%s</testsuite>\n' "$cases" > "$REPORT"
 
 # Zero suites is a wrong TESTS path or a broken glob, never a pass.
 if [ $((pass + skip + fail)) -eq 0 ]; then
```

- [ ] **Step 3:** `bash scripts/ci/tests/test-run-selection.sh` → 10 `ok`, exit 0. `shellcheck -x -S warning scripts/ci/tests/*.sh`
  → clean. `bash scripts/ci/tests/run.sh` → every suite runs as before.
- [ ] **Step 4:** `git add scripts/ci/tests/run.sh scripts/ci/tests/test-run-selection.sh && git commit -m "feat(ci): run.sh runs named suites and writes a JUnit report"`

### Task 6: The `tests` section and the coverage suite

**Files:** Modify `.github/agent-checks.yaml`, `scripts/ci/tests/test-agent-checks.py` (feature 3's Appendix A).

- [ ] **Step 1: The failing check.** Add to `.github/agent-checks.yaml`, after `ciOnly`'s last entry, the job's entry, and the section:

```yaml
  - {check: "Test contract 🧪", part: all, why: "judges a pull request's own tests at the base, the base plus the tests and the head; a tester's harness runs the same test-contract before it pushes"}
tests:
  paths: [scripts/ci/tests/]
  select: ["scripts/ci/tests/test-*.sh", "scripts/ci/tests/test-*.py", "scripts/ci/tests/*/test-*.py"]
  runner: [scripts/ci/tests/run.sh, scripts/ci/ai-overlay.sh]
  run: ./scripts/ci/ai-overlay.sh $([ -n "${CI:-}" ] || echo --from-git) >/dev/null && scripts/ci/tests/run.sh "$@"
  timeoutMinutes: 10
```

  Run: `python3 scripts/ci/tests/test-agent-checks.py` → `FAIL unknown keys ['tests']` and, until Task 7,
  `FAIL ciOnly entry Test contract 🧪 names 'Test contract 🧪', which is no job of a pull_request workflow`.
- [ ] **Step 2: The suite** (and ai-native-ref's copy in PR 3, byte for byte):

```diff
-if extra := set(doc) - {"version", "timeoutMinutes", "tools", "definedBy", "steps", "ciOnly"}:
+if extra := set(doc) - {"version", "timeoutMinutes", "tools", "definedBy", "steps", "ciOnly", "tests"}:
     fail("unknown keys %s" % sorted(extra))
```

  and, before `# The coverage rule (RC3)`:

```python
# test-contract's section (agent-platform v3 feature 4; images/agent-harness/test_contract.py, validate).
tests = doc.get("tests")
if tests is not None:
    if not isinstance(tests, dict):
        fail("tests must be a mapping")
        tests = {}
    if extra := set(tests) - {"paths", "select", "runner", "run", "timeoutMinutes"}:
        fail("tests: unknown keys %s" % sorted(extra))
    for key in ("paths", "select", "runner"):
        v = tests.get(key)
        if not (isinstance(v, list) and v and all(isinstance(p, str) and p and not p.startswith("/") for p in v)):
            fail("tests: %s must be a non-empty list of relative paths" % key)
    for p in [p for key in ("paths", "runner") for p in (tests.get(key) or []) if isinstance(p, str)]:
        if not (ROOT / p).exists():
            fail("tests: %s does not exist" % p)
    if not (isinstance(tests.get("run"), str) and tests["run"].strip()):
        fail("tests: no run")
    t = tests.get("timeoutMinutes", 5)
    if not (isinstance(t, int) and 1 <= t <= MAX_MINUTES):
        fail("tests: timeoutMinutes must be 1 to %d" % MAX_MINUTES)
```

- [ ] **Step 3:** the suite → only the `ciOnly` failure is left (Task 7 adds the job).

### Task 7: "Test contract 🧪", report-only

**Files:** Create `.github/workflows/test-contract.yaml`.

- [ ] **Step 1:** The workflow; replace `<PR 1 merge sha>` and `<tag>` with PR 1's (Task 4, Step 3):

```yaml
# Test contract 🧪 (agent-platform v3 feature 4, TF7): on an agent's pull request, every test it adds
# or changes fails on the base plus the tests and passes at the head. test-contract reads the tests'
# declaration from the base (.github/agent-checks.yaml), never from the pull request.
# Its own workflow, so a red here stays out of ci.yaml's conclusion, which policy-bot reads. The
# factory requires it only on a task whose tester wrote a contract (merge.contractCheck); on any
# other agent pull request it reports. A gate path (^\.github/).
name: Test contract

on:
  pull_request:
    branches: ["main"]

permissions:
  contents: read

concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: true

jobs:
  test-contract:
    name: Test contract 🧪
    if: github.event.pull_request.user.login == 'ogenki-agents[bot]'
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7
        with:
          fetch-depth: 0 # the base, the head and their merge base
          persist-credentials: false

      - name: Install tools
        uses: jdx/mise-action@08ae2a091c038e1e05ec6f5115dc5c69d5a2a6b8 # v5

      - name: Install Python dependencies
        run: python3 -m pip install --quiet --disable-pip-version-check pyyaml

      - name: Judge the contract
        uses: Smana/agent-platform/.github/actions/test-contract@<PR 1 merge sha> # <tag>
        with:
          base: ${{ github.event.pull_request.base.sha }}
          head: ${{ github.event.pull_request.head.sha }}
```

- [ ] **Step 2:** `actionlint .github/workflows/test-contract.yaml`, `bash scripts/ci/check-workflow-secrets.sh`,
  `bash scripts/ci/check-policy-gate-coverage.sh`, `python3 scripts/ci/tests/test-agent-checks.py`
  (`PASS 11 steps, 14 CI-only entries, 15 pull_request jobs covered`), `task check`, `task ci:test` → all exit 0.
- [ ] **Step 3: Prove the job's script on this branch.** This pull request is a human's, so the job
  skips on it; and `main` declares no tests yet, so `test-contract ci --base origin/main` exits 2 here
  (the declaration is always the base's). Prove it against this branch as the base instead, on a
  throwaway local branch: add `scripts/ci/greet.sh` (`echo "hello ${1:-world}"`) and a suite
  `scripts/ci/tests/test-zz-greet.sh` expecting `hello ada` from `greet.sh ada`, commit, then
  `python3 <agent-platform>/images/agent-harness/test_contract.py --repo . ci --base feat/test-contract-report-only --head HEAD`
  → exit 0, `green`, the suite `fail` at the base plus the tests and `pass` at the head. Commit only the
  suite on a second throwaway branch → exit 1, "no test fails on the base plus the tests and passes at
  the head". Paste both summaries in the pull request, then delete both branches.
- [ ] **Step 4:** `git add .github/agent-checks.yaml scripts/ci/tests/test-agent-checks.py .github/workflows/test-contract.yaml && git commit -m "feat(ci): the test contract, declared and reported on every agent pull request"`;
  push, open the pull request for a human review (gate paths).

---

## PR 3 — ai-native-ref: the pin and the status

### Task 8: `HOST_REF`, the suite in step, the status page

**Files:** Modify `HOST_REF`, `scripts/ci/tests/test-agent-checks.py`, `website/content/docs/platform/ai-platform/status.md`.

- [ ] **Step 1:** `HOST_REF` ← PR 2's merge commit on cloud-native-ref `main`.
- [ ] **Step 2:** Copy Task 6's two hunks into `scripts/ci/tests/test-agent-checks.py`; then
  `cmp scripts/ci/tests/test-agent-checks.py .host/scripts/ci/tests/test-agent-checks.py` → no output (after `task check` clones the host).
  ai-native-ref declares no `tests` (TO4: its tests live under the gate path `scripts/ci/`).
- [ ] **Step 3:** In `status.md`'s roadmap table, the test-first tester row's middle cell gains a last sentence:
  "Since <merge date>, every agent pull request in cloud-native-ref reports whether its new tests fail
  on the base and pass on the head; nothing requires it yet."
- [ ] **Step 4:** `bash $SCRATCH/check.sh <worktree>` → `exit=0`. Commit `feat(ci): HOST_REF with the test contract, report-only`; push; open the pull request.

---

## What the follow-up plans inherit

Each follow-up plan starts from this plan's rulings and these interfaces; it re-measures what it
builds on before it plans.

| Plan | Builds on | Fixed now | Offline proof |
|---|---|---|---|
| **F1** agent-platform, the harness | this PR 1; feature 3 PR 1 (`declaration`, `pins`, `install`, `step_env`, `checks_rules`, the prefetch); loop policy part 1 (`continue_conversation`, `drive`, `loop_policy.decide`, `ProgressNotes.final`) | The tester's clone sets the `pushurl` (R13); its rules gain a "Test contract" section naming `paths`, `select` and `test-contract red`; the prefetch runs for the tester too (feature 3's `checks_rules` returns the tester's section instead of `""`). On `finished`: no commit since `origin/HEAD` → `not_testable`; else `red()` on `git commit-tree HEAD^{tree} -p <base>` with `env = agent_checks.step_env(...)` and `TMPDIR=/workspace/.agent-checks/trees`; `contract_not_red` or `contract_outside_tests` once → one continuation quoting `why` (fenced as loop R14), then the end; could-not-run → no continuation (R6); red → sign (R12), push by sha, `contract_ready`. Every end exits 0 and posts `Harness (tester): <end>: <files or why>` (≤ 280 characters, `ProgressNotes.final`) and the span `agent-run.contract` with `error.type` = the end unless ready. The implementer: `contract_of` at start → `/workspace/.agent-contract.json` (files, ids, commit) and a rules section; the pre-commit hook (R15); `hold()` at the end inside `drive()` or the contract-only loop (R16). `internal/mcp/tools.go`: `room_verdict` roles `[reviewer]` | 2, 3 |
| **F2** crossplane-configuration | nothing | `_rulesFor`'s tester branch: "2. Write tests only under the paths your rules' Test contract section names. Commit them on {branch}; never push: the platform checks that they fail on the base and pushes them. 3. Never change the code under test." plus rules 3–4 of `_read` renumbered; `_verdict` for the reviewer only; `_handoff` without the tester. Goldens and `test_room_rules_only_in_a_room` updated; a release | 5 |
| **F3** agent-platform, the factory | F1's release; merge gate PR A (`baseChecks`, `forge.FileStat`, `holdoutChecks`); feature 3 PR 4 (`Class.Checks`, `withChecks`) | `Class.TestFirst`, `Merge.ContractCheck`, `Config.Warnings` (R17, R18, R20); `TaskStatus.Contract{RunID, Commit string; Files, IDs []string}` (`maxItems` 50); in `queued`, a public task of a `testFirst` class with no tester run starts `testerSpec` (`implementerSpec` with `Role: "tester"`, `TesterBrief`, `withChecks` covering the tester); `phaseFor("tester")` is Implementing; `implementing` reads the forge after a tester run (R14) before the `infraLost` block and requeues the implementer, whose every brief gains the contract's files and ids; an implementer's handoff to `tester` ends `Escalated`, `contract_disputed`; `nextVerifier`, `arm.go`, `merge.go`, `rooms.verifier`, `store/verdicts.go`, `summary`, `narrate`, `api/server.go`, `humanapi/startrun.go`, `roomctl` stop treating `tester` as a verifier (R18); rows 2b and 2c in `baseChecks` (R21) with `Merger.ChangedFiles`; the reasons `contract_invalid`, `contract_disputed`, `contract_modified` with prose; the counter (R23) and the gauge (R20); `docs/factory.md` | 4 |
| **F4** cloud-native-ref | F2's release | `.github/chainguard/agent-tester.sts.yaml`: `contents: write`, the comment "writes the test contract to agent/<task>; its harness pushes only after the red check (TO2)"; the composition pin of F2's release. Human-authored (gate paths) | the host's CI |
| **F5** ai-native-ref | F3's release, F4 | The factory pin; `HOST_REF`; in the config: `trio: {roles: [implementer, reviewer], maxReviewRounds: 2}`; `holdoutChecks: ["Check the documentation links 🔗"]` on the host's `docs-links` and `revert`, `[validate]` on ai-native-ref's (unless the merge gate's PR D declared them first); the host's `scripts: {checks: false, testFirst: false}`, `review: {checks: false, testFirst: false}`, `merge.contractCheck: "Test contract 🧪"`; nothing for ai-native-ref (TO4). `infrastructure/base/agent-gateway/profiles/tester.{public,internal}.yaml` without `room_verdict`, `mcp.yaml` regenerated by `scripts/ci/gen-capabilities.py --write`; the harness and factory pages, the status page | 6 |

## Self-review

- **Spec coverage.** TF1 (switch, sequence, caps): F3, R17, R19. TF2 (verifier retired, `trio`): F3,
  F5, R18. TF3 (three filters): the class switch and `not_testable` in F1/F3; the scoper's filter
  joins with feature 1's plan. TF4 (contract, trailers): R12, R14 (source deviates, flagged). TF5
  (red before push): Task 1's `red`, F1, R6, R13. TF6 (held to the contract): Task 1's `hold`, F1
  (hook, continuation), F3 (`contract_disputed`, every implementer brief), F3 row 2c. TF7 (CI judges):
  Tasks 1, 3, 5–7; required on a contract in F3/F5. TF8 (rows 2b, 2c): R21, F3. TF9–TF11 (holdouts):
  R20, the merge gate plan, F3's warning, F5's declarations. TO1–TO5: R18, F4, TO3 in R14, R17 and F5,
  R20. Offline proofs: 1 here; 2, 3 in F1; 4 in F3; 5 in F2; 6 in F5.
- **Placeholders.** The only values left to fill are PR 1's merge sha and tag (Task 7) and PR 2's
  merge commit (Task 8), which exist only after those merges.
- **Names.** `red`, `hold`, `ci`, `contract_of`, `validate`, `declaration`, the exit codes and the
  trailer names are the same in Task 1, the action, F1 and F3.
