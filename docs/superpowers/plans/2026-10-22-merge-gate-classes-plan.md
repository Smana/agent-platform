# Merge gate classes, first slice: the vocabulary at parity, then `dead-links` — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The merge gate decides every class from one vocabulary of named conditions on top of a
fixed base, with `docs-links` and `revert` decided exactly as today; then `dead-links`, the first
oracle-backed class, ships declared off: its detector on the host, its probe, task and oracle in
the factory, and its declaration, egress and parity suite in ai-native-ref.

**Architecture:** `DecideArm` becomes `baseChecks` (secret scan, CI, fence, policy-bot), the
`agents` author mode (`agentEvidence`), the class's declared conditions (`Conditions()`, each with
its own refusal), then the breaker, the caps and shadow. Conditions on the diff read GitHub's own
file list (`forge.Diff`), held until `awaitingCI` reads it, as R52's links-only gate already is.
`dead-links` adds a nightly lychee workflow on the host, a probe that keeps a finding only when
the newest two runs on `main` agree, the findings on the Task, and an `oracle` condition that
proves the head fixed them and moved nothing else.

**Tech Stack:** Go 1.27.3 (agent-platform; go-github v92, githubv4, controller-runtime), Python 3
and Bash (host workflow scripts and suites, ai-native-ref suites), lychee 0.24.2, GitHub Actions,
policy-bot, Flux and Cilium (ai-native-ref manifests).

**Spec:** [merge gate classes](https://github.com/Smana/ai-native-ref/blob/main/docs/superpowers/specs/2026-10-22-merge-gate-classes-design.md) on ai-native-ref
`main` (decisions MC1–MC14, M1–M6 as recommended): its PR 1, its PR 3 without the monitor, the
detector part of its PR 4, and its PR 5 without the monitor; offline proofs 1, 2, 3, 8, 9, 10, 11
and 12. This plan lives in agent-platform because most of it is there; the host and ai-native-ref
tasks name their own repositories.

## This plan is the first slice

The whole design is eight pull requests over three repositories plus three live sessions. This
plan covers the first shippable slice: **the vocabulary at parity, then `dead-links` shipped off**.
The rest follows as separate plans, in this order:

| # | Follow-up plan | Content (design PR) | Depends on |
|---|---|---|---|
| F1 | Outcomes and the monitor | Follow-up detection and `factory/follow-up-of:#<n>`, `breaker.maxFollowUps`, the post-merge monitor (main CI, the detector's next runs through this plan's `RunArtifact`, Flux with its read-only ClusterRole off by default), the class `monitor` key, `agent_factory_monitor_verdicts_total`, `outcome="follow_up"`, `shadow-merge-tally.sh --outcomes` with the Clopper-Pearson bound (design PR 2); in ai-native-ref the Flux RBAC value, `AgentFactoryMergeRegression` and its promtool test, runbook 15's monitor step (design PR 5's rest). Offline proofs 4–7, 13 | This slice |
| F2 | `deps-patch` | Adopted Renovate PRs, `author: {adopt}`, `semver-patch` and M2's `semver-minor` shadow shape, `releaseAgeCheck`, the host's `renovate` rule with the gate list; ai-native-ref's pin, declaration and runbook 16; the host's live-switch PR (design PRs 4 part, 6, 7, 8). Offline proofs 14–16 | F1 |
| F3 | ai-native-ref's own `dead-links` lane | Its `dead-links.yaml` (running the host's scripts from `.host/` at `HOST_REF`, never vendored), its `low-risk: dead-links` rule ([R22](#this-plans-rulings)), its schedule entry | This slice |

The live proofs L1–L5 stay in the gcp-0 sessions. **`dead-links`' shadow switch (L1) waits for F1**,
so every shadow merge gets its monitor verdict, as the design's PR order intended.

### This plan's rulings

| # | Ruling | Why |
|---|---|---|
| R1 | **The slice moves the monitor out** (design PR 2 and PR 5's monitor parts become F1), and L1 waits for F1 | The caller asked for the base and conditions, then `dead-links`. The design ordered PR 2 before PR 3; holding L1 keeps its evidence order. F1's oracle signal reuses this slice's `RunArtifact` instead of building a second reader |
| R2 | **Parity is literal.** PR A leaves `arm_test.go`, `merge_test.go` and `ledger_test.go` byte-identical (sha256 below) and green. PR B moves `TestFencesOf` and `TestLinksOnly` verbatim to `mdlinks` | Offline proof 1. A move in PR A would make "unchanged" unverifiable |
| R3 | **The diff's numbers are GitHub's.** `Merger.Files` becomes `Merger.Diff`: the same compare call, keeping each file's `status`, `additions`, `deletions` | policy-bot's `modified_lines` sums the same numbers; one call, no second reading of the diff |
| R4 | **Conditions on the diff hold until it is read** (`ArmInputs.DiffRead`). `awaitingCI` reads it only for a first-pass `arm` or `shadow` of a class that `ReadsDiff`, then decides again | The spec left it open. It is R52's existing two-pass pattern; the class-name test in `merge.go` becomes `ReadsDiff` |
| R5 | **The built-in shape.** `docs-links` and `revert` get `diffShape: links-only` at decision time when their config names none; naming another shape for them is refused. They declare no other condition, so their fence stays policy-bot's alone | MC1's "built-in defaults at parity". Declaring their fence too could change a refusal reason, which parity forbids |
| R6 | **Order:** base, `not_agent_authored`, `maintainer_approved`, class switch, agent evidence, then `allowedFiles`, `protectedFiles`, `modifyOnly`, `maxPatchFiles`, `maxModifiedLines`, `diffShape`, `holdoutChecks`, `oracle`, then breaker, caps (repository, then class), shadow | The design's table order, inserted where R52's links-only row already sat, so every existing case keeps its reason |
| R7 | **`modifyOnly`** holds only when every file's status is `modified`: added, removed, renamed, copied and changed refuse (`file_added_or_deleted`) | Fail closed; policy-bot's `file_not_added`/`file_not_deleted` cannot be looser |
| R8 | **`holdoutChecks`**: missing or running waits (`wait`, `holdout_pending`); failed, cancelled, or posted by an App other than GitHub Actions (`checkSuite.app.databaseId` 15368, measured) refuses (`holdout_red`). An App the merger token cannot read reads 0 and refuses | Pending waits (design); "posted by GitHub Actions" (design) made checkable. Fail closed |
| R9 | **`maxPerDay`** counts the class's merges today (Tasks' `armedAt` and its labelled merges), next to `autoMergesPerDay`; both refuse `auto_merge_cap` | The design's base row; one reason for both caps |
| R10 | **Not in the slice:** the `author` key (`agents` is built in; `adopt` comes with F2), `diffShape: semver-patch`, `releaseAgeCheck`, `monitor` | YAGNI; each arrives with its first user. An unknown key fails the strict decoder until then |
| R11 | **`oracle`** needs `holdoutChecks` and `diffShape: links-only` (Validate); `maxFindings` is 1..16 (the CRD's `maxItems`). A retarget is allowed to the same https host or to exactly the finding's archive; a task with no findings fails `oracle_finding_open` | Offline proof 3; "same host or the snapshot given" (design's task rule) made exact |
| R12 | **The Task's findings** are `spec.findings: [{file, url, archive?}]`; line and status stay in the text | Only what the gate re-checks is stored (design: "Task fields for findings", left to the builder) |
| R13 | **The probe** takes the newest two completed runs on `main` that a schedule or a maintainer's dispatch started; both must be `success`. It keeps (file, url) pairs both report, with status `404`, `410` or `nxdomain`, an https target, a clean path the class allows and does not protect, still a link target at that file on `main`; sorted, at most `maxFindings`. Nothing while the class is neither live nor shadow (MC14), nothing while a non-terminal Task of the schedule exists (L9) | MC5, MC6 and the design's probe row, made exact. An http target is dropped: R52 retargets https only |
| R14 | **NXDOMAIN is the findings script's call** (`getaddrinfo` → `EAI_NONAME`) | Measured: lychee 0.24.2 reports a name that does not resolve as `Connection failed`, like a refused port |
| R15 | **The archive is best effort**: the Wayback availability API, its `http://web.archive.org` answer rewritten to https, at most 20 lookups 5 s apart, the first refusal ending them for the run | Measured: the API answers http, and 429s after a few requests from one address |
| R16 | **lychee is installed from its release tarball pinned by sha256**, not by `lycheeverse/lychee-action` | The spec pinned the action. The tarball is a digest pin, serves both jobs with one script, and avoids the action's `eval` of its args |
| R17 | **"External links 🔗"** runs on pull requests touching `**.md`; the targets are `lychee --dump` of each changed Markdown file at the merge base and at head, the new ones checked with `--accept 200..=299 --max-retries 3 --timeout 20`. No target is a pass | Measured on a fixture repository. Every `dead-links` PR touches `.md`, so its holdout always runs |
| R18 | **One workflow, `dead-links.yaml`**, jobs `scan` (04:23 UTC and dispatch) and `external-links`; scripts in `.github/scripts/dead-links/` (a gate path); artifact `findings`, 14 days. lychee exit 0 or 2 passes, anything else fails the run | The design names one file. 14 days covers F1's two following runs |
| R19 | **The factory gets egress to `productionresultssa*.blob.core.windows.net:443`** and downloads the zip with the plain egress client, no token, at most 4 MiB | Measured: the artifact API answers 302 to `productionresultssa11.blob.core.windows.net`, outside today's `api.github.com`-only egress. The design's "no new permission" holds for GitHub, not for the network |
| R20 | **An oracle class's task is `solo` at `light`**, with no classifier call and outside the control group | MC6. OD-14's control group measures the classifier, which is not asked |
| R21 | **`dead-links` is declared off on both repositories.** The host's schedule entry ships with it, inert while the class is off; ai-native-ref gets no schedule (F3) | MC14: a session's first commit is then `shadow: true` alone |
| R22 | **ai-native-ref's shadow needs a policy rule.** Without a `low-risk` rule there, policy-bot reads pending and no decision reaches `would-arm` (true of `docs-links` there today, v2 D9). F3 adds `low-risk: dead-links` with its workflow; a policy SUCCESS merges nothing, the merger does, and only for a live class | A gap in the design: "shadow on both" and "the rule only after 20 shadow decisions" cannot both hold. **Owner to confirm in F3** |
| R23 | **The parity suite** holds each class that declares `allowedFiles`, `protectedFiles`, `modifyOnly` or `maxModifiedLines` to `low-risk: <class>`: `allowedFiles` = `only_changed_files`; `no_changed_files` = the gate list ∪ `protectedFiles`; `modifyOnly` ⇔ both `file_not_*` are `['.*']`; `maxModifiedLines n` ⇔ `< n+1`. A class without its rule must not be live. Its self-test runs the comparison on the host's real `low-risk: docs-links` rule | MC2 and offline proof 11. On the host, `dead-links` PRs are approved by `low-risk: docs-links` until `low-risk: dead-links` lands with the live switch, so the two must already agree |
| R24 | **Release v0.17.0**, the minor after v0.16.0 (#168, daily caps in dollars). ai-native-ref's PR D comes after truthful costs PR 3 (the v0.16.0 pin and its USD keys) | A release carries everything before it: v0.16.0's config must be in place first |
| R25 | **One dashboard panel**, "Merge decisions by class and reason" (7 days) | The design: "a reason per condition keeps every refusal explainable on the dashboard"; no panel shows reasons today |
| R26 | **New reasons**, each with prose in `narrate`: `file_not_allowed`, `file_protected`, `file_added_or_deleted`, `patch_too_large`, `holdout_red`, `holdout_pending` (also in the summary, a wait), `oracle_finding_open`, `oracle_unrequested` | `narrate`'s summary test holds wait reasons to both tables |

**Measured 2026-10-22, re-run 2026-10-23** on a prototype (a detached agent-platform worktree under
`$SCRATCH/mergegate/ap`, rebased onto `origin/main` `0717cd6`; the code below is its code), lychee
v0.24.2 and the host at ai-native-ref's `HOST_REF` `ea184f78`:

- **Parity:** after the refactor, `arm_test.go` (`876e7fd8…`), `merge_test.go` (`dce85b82…`) and
  `ledger_test.go` (`88c45d54…`) are byte-identical and their 101 cases and subtests pass. Every
  package under `internal/factory/`, `internal/app/` and `api/` passes with `-race`;
  `hack/lint.sh` reports 0 issues; `crd:check` and `chart:check` pass.
- **The real config:** ai-native-ref's factory config, rendered with gcp-0's variables, parses
  with hash `0456407…` on `origin/main` and on the prototype alike. With `dead-links` declared as
  in Task 13 it parses on the prototype and fails on `origin/main` with `unknown field
  "allowedFiles"`: the pin and the declaration ship in one commit.
- **Conditions:** each condition failed alone yields its reason (12 cases), an unread diff holds
  every diff condition, the oracle's six fixtures yield their reasons, and the probe keeps 1 of 8
  candidate findings (the one finding both runs on `main` report, at a link target, allowed).
- **GitHub:** `statusCheckRollup` gives `checkSuite.app.databaseId` 15368 for every Actions check
  of host PR #2531 (and 57789 for code scanning). An artifact's `/zip` answers `302` to
  `productionresultssa11.blob.core.windows.net`.
- **lychee 0.24.2:** its JSON report carries `span.line`; 404, 410 and a redirect to a 404 carry
  `code`; 403, 429 and 503 carry their codes too; a timeout lands in `timeout_map`; an unused
  reference definition and a fenced URL are never reported; a used reference is reported at its
  use; a `.invalid` host is excluded by lychee itself; a name that does not resolve and a refused
  port both read `Connection failed`/`Connection refused` with no code (`getaddrinfo` tells
  them apart: `EAI_NONAME`). Exit 2 when anything is broken, 0 otherwise, 0 on an empty input.
- **Wayback:** the availability API's closest snapshot is `http://web.archive.org/web/…`; after
  two requests in ten minutes it answered 429 to curl and to Python, with no `Retry-After`.
- **The host side:** the findings suite passes; `actionlint` and `shellcheck` pass on the workflow
  and its scripts; the host's `check-workflow-secrets.sh` passes on it; the installer verifies the
  tarball and installs lychee 0.24.2. `changed-targets.sh` on a fixture repository lists only the
  pull request's new target; lychee then exits 0 on a 2xx target and 2 on a 403.
- **The parity suite** passes on the declared config and fails with `class dead-links is live with
  no 'low-risk: dead-links' rule` when the host's `dead-links` is set live.

## Global Constraints

- **Repositories and branches.** agent-platform: PR A `feat/merge-gate-vocabulary`, PR B
  `feat/dead-links`, each in a sibling worktree from a freshly fetched `origin/main`
  (`git -C /home/smana/Sources/agent-platform worktree add /home/smana/Sources/agent-platform-<topic> -b <branch> origin/main`),
  this plan's own worktree being `/home/smana/Sources/agent-platform-merge-gate`. cloud-native-ref:
  PR C `feat/dead-links-detector`. ai-native-ref: PR D `feat/dead-links-declared-off`, in a
  worktree from `origin/main`, `mise trust mise.toml` once. Rebase onto a freshly fetched
  `origin/main` before every push.
- **`$SCRATCH`** is a scratch directory outside the repositories. In ai-native-ref it holds
  `check.sh <worktree>` (runs `task check` with a `python3` that has PyYAML, logs to
  `$SCRATCH/check-<worktree directory name>.log`, ending `exit=N`) and `run-suite.sh <command…>`.
- **The evidence.** agent-platform: `task check` exits 0 (it needs Docker for the store tests;
  start the daemon for the final run). ai-native-ref: `task check`'s log through `check.sh`.
  cloud-native-ref: its suites (`scripts/ci/tests/run.sh`), `actionlint`, `shellcheck`,
  `check-policy-gate-coverage.sh`, `check-workflow-secrets.sh`.
- **agent-platform conventions** (its AGENTS.md): every `.go` file starts with
  `// SPDX-License-Identifier: Apache-2.0`; exported symbols carry doc comments; lint through
  `task lint`, never a bare `golangci-lint`; `task crd:gen` after an API change, and commit what it
  writes.
- **Commits** are conventional, in English, with **no trailers**. Pull request descriptions in
  English.
- **Never vendor host scripts in ai-native-ref.** F3's workflow will run them from `.host/`.
- **Constitution** (ai-native-ref): default-deny CiliumNetworkPolicy; R19 adds one FQDN allow and
  nothing else.
- **Verbatim from the spec:**
  - MC1: "`DecideArm` becomes a fixed base, which no class can remove, plus conditions a class
    names in its config, each a predicate with its own refusal reason … An unknown name fails the
    strict decoder".
  - MC5: "A finding is a 404, a 410 or a name that does not resolve, reported by two consecutive
    scheduled runs on `main`. 403, 429, 5xx and timeouts are never findings. On the pull request,
    every target it changed must answer 2xx after redirects, or the holdout check fails".
  - MC6: "The probe renders the task from the findings; the gate refuses a target changed outside
    them (`oracle_unrequested`). `dead-links` runs `solo` on `light`, with no reviewer".
  - MC14: "The release ships the vocabulary at parity; the classes are declared with no switch and
    no intake, and a session's first commit sets `shadow: true`".
  - Offline proofs: 1 (Task 3), 2 (Task 3), 3 (Tasks 2 and 6), 8 (Task 8), 9 (Task 6), 10 (Tasks 10
    and 11), 11 (Task 13), 12 (Task 14).

## Pull requests, in order

| PR | Repository | Content | Depends on |
|---|---|---|---|
| A | agent-platform | The vocabulary at parity: `forge.Diff` and `Check.App`, the class conditions in config, `DecideArm` as base + author + conditions, `maxPerDay`, the reasons, `docs/factory.md` (Tasks 1–4) | — |
| B | agent-platform | `dead-links`: `mdlinks`, `spec.findings`, `oracle`, the Actions reads, the probe and its schedule wiring, the oracle class's triage; release v0.17.0 (Tasks 5–9) | A |
| C | cloud-native-ref | `dead-links.yaml`, its scripts and the findings suite (Tasks 10–11) | — (any time before D) |
| D | ai-native-ref | Pin v0.17.0, `HOST_REF` to C's merge, `dead-links` declared off, the schedule, R19's egress, the parity suite, the panel, runbook 15, the docs (Tasks 12–14) | B released, C merged, truthful costs PR 3 merged |

## File structure

| File | PR | Change | Responsibility |
|---|---|---|---|
| `internal/factory/forge/forge.go` | A, B | modify | `Check.App`, `ActionsApp`, `FileStat`, `Diff`; B: `WorkflowRun` and three `Merger` reads |
| `internal/factory/forge/github.go` | A, B | modify | `Diff` in place of `Files`, the rollup's App; B: the `plain` client |
| `internal/factory/forge/actions.go` | B | create | `WorkflowRuns`, `RunArtifact`, `FileAt` |
| `internal/factory/forge/absent.go`, `forgetest/fake.go` | A, B | modify | The new `Merger` methods, failing closed / faked |
| `internal/factory/config/classes.go` | A, B | create, modify | `Class` and its conditions, `Oracle`, `validateClass` |
| `internal/factory/config/config.go` | A, B | modify | `Class` moves out; B: the `dead-links` probe's validation |
| `internal/factory/reconciler/arm.go` | A, B | modify | `DecideArm` as `baseChecks` + `agentEvidence` + `declared`; B: the link code moves out |
| `internal/factory/reconciler/gate.go` | A, B | create, modify | `Conditions()` and each predicate; B: `oracle` |
| `internal/factory/reconciler/merge.go` | A | modify | The second pass on `ReadsDiff`, `armedTodayOf` |
| `internal/factory/mdlinks/` | B | create | R52's link reading, shared by the gate, the probe and the oracle |
| `api/factory/v1alpha1/task_types.go` (+ generated CRDs) | B | modify | `spec.findings` |
| `internal/factory/intake/deadlinks.go`, `schedule.go` | B | create, modify | The probe, the task text, the schedule wiring |
| `internal/factory/triage/classify.go` | B | modify | Oracle class: `solo` on `light` |
| `internal/factory/narrate/narrate.go`, `internal/summary/waiting.go` | A, B | modify | Reason prose |
| `docs/factory.md` | A, B | modify | The gate's rows, reasons, config keys, templates, probe |
| host `.github/workflows/dead-links.yaml`, `.github/scripts/dead-links/*` | C | create | The detector and the holdout |
| host `scripts/ci/tests/test-dead-links-findings.sh` | C | create | Offline proof 10 |
| ai-native-ref `tooling/base/agent-factory/*`, `flux/sources/ocirepo-agent-factory.yaml`, `HOST_REF` | D | modify | Pin, declaration, schedule, egress |
| ai-native-ref `scripts/ci/tests/test-agent-merge-gate-parity.py` | D | create | Offline proof 11 |
| ai-native-ref dashboard, runbook 15, `factory.md`, `status.md`, the spec | D | modify, create | Panel, drill, docs |

---

## PR A — agent-platform: the vocabulary at parity

### Task 1: The diff's file list and the check's App in the forge

**Files:**
- Modify: `internal/factory/forge/forge.go`, `internal/factory/forge/github.go`, `internal/factory/forge/absent.go`, `internal/factory/forge/forgetest/fake.go`
- Test: `internal/factory/forge/github_test.go`, `internal/factory/forge/forgetest/fake_test.go`

**Interfaces:**
- Produces: `forge.Check.App int64`, `forge.ActionsApp = 15368`, `forge.FileStat{Path, Previous, Status string; Additions, Deletions int}`, `forge.Diff{Base, Head map[string]string; Files []FileStat}`, `Merger.Diff(ctx, base, head string) (Diff, error)` (replaces `Files`), `(*forgetest.Fake).SetFileStats(...forge.FileStat)`.

- [ ] **Step 1: Write the failing tests.** In `github_test.go`, the rollup fixture gains an App
  and the two `Files` tests read `Diff`, with GitHub's counts:

```diff
diff --git a/internal/factory/forge/github_test.go b/internal/factory/forge/github_test.go
index 60cd1d4..1c95d16 100644
--- a/internal/factory/forge/github_test.go
+++ b/internal/factory/forge/github_test.go
@@ -427,7 +427,7 @@ func newFakeGitHub(t *testing.T, pub *rsa.PublicKey, now func() time.Time) *fake
 	mux.HandleFunc("GET /repos/Smana/demo/compare/{basehead}", g.authed(func(w http.ResponseWriter, r *http.Request) {
 		switch r.PathValue("basehead") {
 		case "old...new": // one modified file: both sides have content.
-			_, _ = io.WriteString(w, `{"merge_base_commit":{"sha":"old"},"files":[{"filename":"docs/a.md","status":"modified"}]}`)
+			_, _ = io.WriteString(w, `{"merge_base_commit":{"sha":"old"},"files":[{"filename":"docs/a.md","status":"modified","additions":2,"deletions":1}]}`)
 		case "old...mid": // a file over the contents API's megabyte: GitHub lists it without content.
 			_, _ = io.WriteString(w, `{"merge_base_commit":{"sha":"old"},"files":[{"filename":"docs/big.png","status":"modified"}]}`)
 		case "main...behind": // a branch cut at "fork", before main moved on: three-dot lists its own change.
@@ -892,17 +892,20 @@ func TestRevertLabelListsClosedPullRequests(t *testing.T) {
 func TestFilesReadsBothSidesOfADiff(t *testing.T) {
 	r := newRig(t)
 	ctx := t.Context()
-	b, h, err := r.g.Files(ctx, "old", "new")
+	d, err := r.g.Diff(ctx, "old", "new")
 	if err != nil {
 		t.Fatal(err)
 	}
-	if b["docs/a.md"] != aBase || h["docs/a.md"] != aHead {
-		t.Fatalf("base %q head %q", b["docs/a.md"], h["docs/a.md"])
+	if d.Base["docs/a.md"] != aBase || d.Head["docs/a.md"] != aHead {
+		t.Fatalf("base %q head %q", d.Base["docs/a.md"], d.Head["docs/a.md"])
 	}
-	if _, _, err := r.g.Files(ctx, "old", "mid"); err == nil || !strings.Contains(err.Error(), "big.png") {
+	if want := (FileStat{Path: "docs/a.md", Status: "modified", Additions: 2, Deletions: 1}); len(d.Files) != 1 || d.Files[0] != want {
+		t.Fatalf("files %+v, want [%+v]: GitHub's own counts, which policy-bot's modified_lines sums", d.Files, want)
+	}
+	if _, err := r.g.Diff(ctx, "old", "mid"); err == nil || !strings.Contains(err.Error(), "big.png") {
 		t.Errorf("an unfetchable file must be an error, not a skip: %v", err)
 	}
-	if _, _, err := r.g.Files(ctx, "mid", "new"); err == nil || !strings.Contains(err.Error(), "page") {
+	if _, err := r.g.Diff(ctx, "mid", "new"); err == nil || !strings.Contains(err.Error(), "page") {
 		t.Errorf("a truncated compare must be an error: %v", err)
 	}
 }
@@ -913,14 +916,14 @@ func TestFilesReadsBothSidesOfADiff(t *testing.T) {
 func TestFilesReadsTheBaseAtTheMergeBase(t *testing.T) {
 	r := newRig(t)
 	ctx := t.Context()
-	b, h, err := r.g.Files(ctx, "main", "behind")
+	d, err := r.g.Diff(ctx, "main", "behind")
 	if err != nil {
 		t.Fatal(err)
 	}
-	if b["docs/b.md"] != bFork || h["docs/b.md"] != bHead {
-		t.Fatalf("base %q head %q: the base must be the merge base, not main's tip", b["docs/b.md"], h["docs/b.md"])
+	if d.Base["docs/b.md"] != bFork || d.Head["docs/b.md"] != bHead {
+		t.Fatalf("base %q head %q: the base must be the merge base, not main's tip", d.Base["docs/b.md"], d.Head["docs/b.md"])
 	}
-	if _, _, err := r.g.Files(ctx, "main", "nobase"); err == nil || !strings.Contains(err.Error(), "merge base") {
+	if _, err := r.g.Diff(ctx, "main", "nobase"); err == nil || !strings.Contains(err.Error(), "merge base") {
 		t.Errorf("a compare without a merge base must be an error: %v", err)
 	}
 }
```

and the rollup's App (one check run carries `checkSuite.app`):

```diff
diff --git a/internal/factory/forge/github_test.go b/internal/factory/forge/github_test.go
index 1c95d16..bf36f95 100644
--- a/internal/factory/forge/github_test.go
+++ b/internal/factory/forge/github_test.go
@@ -92,7 +92,7 @@ const appID = "123456"
 // rollupJSON is the merger's one-query read of a head commit (task 7.1, R16): three check runs
 // in three states and one policy-bot commit status whose creator is a Bot.
 const rollupJSON = `{"data":{"repository":{"pullRequest":{"commits":{"nodes":[{"commit":{"statusCheckRollup":{"contexts":{"nodes":[
-   {"__typename":"CheckRun","name":"Pre-commit checks","status":"COMPLETED","conclusion":"SUCCESS"},
+   {"__typename":"CheckRun","name":"Pre-commit checks","status":"COMPLETED","conclusion":"SUCCESS","checkSuite":{"app":{"databaseId":15368}}},
    {"__typename":"CheckRun","name":"Kubernetes validation","status":"IN_PROGRESS","conclusion":null},
    {"__typename":"CheckRun","name":"Security scanning","status":"COMPLETED","conclusion":"FAILURE"},
    {"__typename":"StatusContext","context":"policy-bot: main","state":"PENDING","creator":{"__typename":"Bot","login":"ogenki-merge-gate"}}
@@ -818,7 +818,7 @@ func TestChecksAndMutations(t *testing.T) {
 	if err != nil {
 		t.Fatal(err)
 	}
-	want := []Check{{Name: "Pre-commit checks", State: "SUCCESS"}, {Name: "Kubernetes validation", State: "PENDING"}, {Name: "Security scanning", State: "FAILURE"}}
+	want := []Check{{Name: "Pre-commit checks", State: "SUCCESS", App: ActionsApp}, {Name: "Kubernetes validation", State: "PENDING"}, {Name: "Security scanning", State: "FAILURE"}}
 	if len(c.Runs) != 3 || c.Runs[0] != want[0] || c.Runs[1] != want[1] || c.Runs[2] != want[2] {
 		t.Fatalf("%+v", c.Runs)
 	}
```

In `forgetest/fake_test.go`:

```diff
diff --git a/internal/factory/forge/forgetest/fake_test.go b/internal/factory/forge/forgetest/fake_test.go
index 6d13621..7374879 100644
--- a/internal/factory/forge/forgetest/fake_test.go
+++ b/internal/factory/forge/forgetest/fake_test.go
@@ -83,8 +83,9 @@ func TestFakeRecordsWhatTheFactoryDid(t *testing.T) {
 		t.Errorf("%+v %v", p, err)
 	}
 	f.SetFiles(map[string]string{"docs/a.md": "old"}, map[string]string{"docs/a.md": "new"})
-	if b, h, err := f.Files(ctx, "base", "head"); err != nil || b["docs/a.md"] != "old" || h["docs/a.md"] != "new" {
-		t.Errorf("files %v %v %v", b, h, err)
+	if d, err := f.Diff(ctx, "base", "head"); err != nil || d.Base["docs/a.md"] != "old" || d.Head["docs/a.md"] != "new" ||
+		len(d.Files) != 1 || d.Files[0] != (forge.FileStat{Path: "docs/a.md", Status: "modified", Additions: 1, Deletions: 1}) {
+		t.Errorf("diff %+v %v", d, err)
 	}
 }
 
```

- [ ] **Step 2: Run them to see them fail.**
  Run: `go test ./internal/factory/forge/...`
  Expected: build failure, `r.g.Diff undefined` and `unknown field App in struct literal of type Check`.

- [ ] **Step 3: Implement.** `forge.go`:

```diff
diff --git a/internal/factory/forge/forge.go b/internal/factory/forge/forge.go
index 0908035..eef54cb 100644
--- a/internal/factory/forge/forge.go
+++ b/internal/factory/forge/forge.go
@@ -178,6 +178,31 @@ type Check struct {
 	ID    int64 // the check run's database id, which is its Actions job id
 	Name  string
 	State string // SUCCESS | FAILURE | CANCELLED | PENDING
+	// App is the database id of the GitHub App whose check suite holds the run: GitHub Actions
+	// is ActionsApp. 0 when GitHub did not say.
+	App int64
+}
+
+// ActionsApp is GitHub Actions' App id on github.com, the one classic protection pins required
+// checks to (15368).
+const ActionsApp int64 = 15368
+
+// FileStat is one changed file as GitHub's compare lists it: its status (added, removed,
+// modified, renamed, copied, changed, unchanged), its previous name on a rename, and its line
+// counts, which policy-bot's modified_lines sums.
+type FileStat struct {
+	Path      string
+	Previous  string
+	Status    string
+	Additions int
+	Deletions int
+}
+
+// Diff is what a squash merge of head applies (R52): each changed file's content at the merge
+// base and at head, "" where it is absent, and GitHub's own list of the changed files.
+type Diff struct {
+	Base, Head map[string]string
+	Files      []FileStat
 }
 
 // Status is one classic commit status, policy-bot's among them.
@@ -242,7 +267,7 @@ type Merger interface {
 	// RerunCheck re-runs the Actions job a check run is. It needs actions: write, beyond what
 	// the merger App held until merge.checkReruns was turned on.
 	RerunCheck(ctx context.Context, id int64) error
-	Files(ctx context.Context, base, head string) (baseFiles, headFiles map[string]string, err error)
+	Diff(ctx context.Context, base, head string) (Diff, error)
 	Merge(ctx context.Context, nodeID, head string) error
 	EnableAutoMerge(ctx context.Context, nodeID, expectedHeadSHA string) error
 	DisableAutoMerge(ctx context.Context, nodeID string) error
```

`github.go` (`Files` becomes `Diff`, the rollup reads the App):

```diff
diff --git a/internal/factory/forge/github.go b/internal/factory/forge/github.go
index 2aa133c..1948b95 100644
--- a/internal/factory/forge/github.go
+++ b/internal/factory/forge/github.go
@@ -616,6 +616,9 @@ func (g *GitHub) PullRequestChecks(ctx context.Context, number int) (Checks, err
 											Name       string
 											Status     string
 											Conclusion string
+											CheckSuite struct {
+												App *struct{ DatabaseID int64 }
+											}
 										} `graphql:"... on CheckRun"`
 										StatusContext struct {
 											Context string
@@ -651,7 +654,11 @@ func (g *GitHub) PullRequestChecks(ctx context.Context, number int) (Checks, err
 	for _, n := range rollup.Contexts.Nodes {
 		switch n.Typename {
 		case "CheckRun":
-			out.Runs = append(out.Runs, Check{ID: n.CheckRun.DatabaseID, Name: n.CheckRun.Name, State: checkState(n.CheckRun.Status, n.CheckRun.Conclusion)})
+			c := Check{ID: n.CheckRun.DatabaseID, Name: n.CheckRun.Name, State: checkState(n.CheckRun.Status, n.CheckRun.Conclusion)}
+			if a := n.CheckRun.CheckSuite.App; a != nil {
+				c.App = a.DatabaseID
+			}
+			out.Runs = append(out.Runs, c)
 		case "StatusContext":
 			out.Statuses = append(out.Statuses, Status{Context: n.StatusContext.Context, State: n.StatusContext.State, Creator: n.StatusContext.Creator.login()})
 		}
@@ -838,28 +845,30 @@ func (g *GitHub) OpenPullRequests(ctx context.Context) ([]PRSummary, error) {
 // file's base side and a deleted file's head side read "", so the maps' union is the changed
 // set. A rename reads as its delete beside its add — which is right: a rename is not a link
 // retarget. §5.1's links-only test for the docs classes (R52) decides on these.
-func (g *GitHub) Files(ctx context.Context, base, head string) (map[string]string, map[string]string, error) {
+func (g *GitHub) Diff(ctx context.Context, base, head string) (Diff, error) {
 	cmp, _, err := g.rest.Repositories.CompareCommits(ctx, g.owner, g.name, base, head, &github.ListOptions{PerPage: 100})
 	g.mark(err)
 	if err != nil {
-		return nil, nil, wrap("compare commits", err)
+		return Diff{}, wrap("compare commits", err)
 	}
 	if len(cmp.Files) == 100 { // one page exactly: unseen files must not let a diff pass as links-only
-		return nil, nil, fmt.Errorf("forge: 100 files differ between %s and %s, a full page: the links-only test cannot see them all", base, head)
+		return Diff{}, fmt.Errorf("forge: 100 files differ between %s and %s, a full page: the links-only test cannot see them all", base, head)
 	}
 	// The file list is three-dot: changed since the merge base. Reading the base side at base's
 	// tip instead would fold in whatever landed there since the branch was cut
 	// (cloud-native-ref#2515), so both sides come from the commits the list compares.
 	mergeBase := cmp.GetMergeBaseCommit().GetSHA()
 	if mergeBase == "" {
-		return nil, nil, fmt.Errorf("forge: comparing %s and %s returned no merge base: the links-only test cannot tell the PR's change from %s's", base, head, base)
+		return Diff{}, fmt.Errorf("forge: comparing %s and %s returned no merge base: the links-only test cannot tell the PR's change from %s's", base, head, base)
 	}
-	baseFiles, headFiles := make(map[string]string, len(cmp.Files)), make(map[string]string, len(cmp.Files))
+	d := Diff{Base: make(map[string]string, len(cmp.Files)), Head: make(map[string]string, len(cmp.Files))}
 	for _, f := range cmp.Files {
 		p := f.GetFilename()
+		d.Files = append(d.Files, FileStat{Path: p, Previous: f.GetPreviousFilename(), Status: f.GetStatus(),
+			Additions: f.GetAdditions(), Deletions: f.GetDeletions()})
 		hc, err := g.fileAt(ctx, p, head)
 		if err != nil {
-			return nil, nil, err
+			return Diff{}, err
 		}
 		bp := f.GetPreviousFilename()
 		if bp == "" {
@@ -867,11 +876,11 @@ func (g *GitHub) Files(ctx context.Context, base, head string) (map[string]strin
 		}
 		bc, err := g.fileAt(ctx, bp, mergeBase)
 		if err != nil {
-			return nil, nil, err
+			return Diff{}, err
 		}
-		headFiles[p], baseFiles[bp] = hc, bc
+		d.Head[p], d.Base[bp] = hc, bc
 	}
-	return baseFiles, headFiles, nil
+	return d, nil
 }
 
 // fileAt is one file's content at one commit. Absent there — an added file's base, a deleted
```

`absent.go`:

```diff
diff --git a/internal/factory/forge/absent.go b/internal/factory/forge/absent.go
index d85578f..bb8a937 100644
--- a/internal/factory/forge/absent.go
+++ b/internal/factory/forge/absent.go
@@ -26,10 +26,8 @@ func (Absent) CommitChecks(context.Context, string) ([]Check, error) { return ni
 // RerunCheck implements Merger.
 func (Absent) RerunCheck(context.Context, int64) error { return ErrNoMerger }
 
-// Files implements the Merger's Files and fails closed.
-func (Absent) Files(context.Context, string, string) (map[string]string, map[string]string, error) {
-	return nil, nil, ErrNoMerger
-}
+// Diff implements the Merger's Diff and fails closed.
+func (Absent) Diff(context.Context, string, string) (Diff, error) { return Diff{}, ErrNoMerger }
 
 // Merge implements the Merger's Merge and fails closed.
 func (Absent) Merge(context.Context, string, string) error { return ErrNoMerger }
```

`forgetest/fake.go`:

```diff
diff --git a/internal/factory/forge/forgetest/fake.go b/internal/factory/forge/forgetest/fake.go
index 18dace1..98a491f 100644
--- a/internal/factory/forge/forgetest/fake.go
+++ b/internal/factory/forge/forgetest/fake.go
@@ -44,6 +44,7 @@ type Fake struct {
 	activityErr  map[string]error
 	filesBase    map[string]string
 	filesHead    map[string]string
+	fileStats    []forge.FileStat
 	open         []forge.PRSummary
 	created      []string
 	armed        []string
@@ -394,11 +395,53 @@ func (f *Fake) SetFiles(base, head map[string]string) {
 	f.filesBase, f.filesHead = maps.Clone(base), maps.Clone(head)
 }
 
-// Files implements the Merger's Files.
-func (f *Fake) Files(context.Context, string, string) (map[string]string, map[string]string, error) {
+// SetFileStats sets the changed-file list Diff returns in place of the one SetFiles implies.
+func (f *Fake) SetFileStats(fs ...forge.FileStat) {
 	f.mu.Lock()
 	defer f.mu.Unlock()
-	return maps.Clone(f.filesBase), maps.Clone(f.filesHead), nil
+	f.fileStats = slices.Clone(fs)
+}
+
+// Diff implements the Merger's Diff: SetFiles' two sides, and SetFileStats' list, or else the
+// one they imply: a side "" is an added or removed file, and each line that differs at its
+// position counts as one addition and one deletion.
+func (f *Fake) Diff(context.Context, string, string) (forge.Diff, error) {
+	f.mu.Lock()
+	defer f.mu.Unlock()
+	d := forge.Diff{Base: maps.Clone(f.filesBase), Head: maps.Clone(f.filesHead), Files: slices.Clone(f.fileStats)}
+	if d.Files != nil {
+		return d, nil
+	}
+	paths := slices.Sorted(maps.Keys(d.Base))
+	for p := range d.Head {
+		if _, ok := d.Base[p]; !ok {
+			paths = append(paths, p)
+		}
+	}
+	for _, p := range paths {
+		b, h := d.Base[p], d.Head[p]
+		st := forge.FileStat{Path: p, Status: "modified"}
+		switch {
+		case b == "":
+			st.Status = "added"
+		case h == "":
+			st.Status = "removed"
+		}
+		bl, hl := strings.Split(b, "\n"), strings.Split(h, "\n")
+		for i := range max(len(bl), len(hl)) {
+			if i < len(bl) && i < len(hl) && bl[i] == hl[i] {
+				continue
+			}
+			if i < len(hl) && h != "" {
+				st.Additions++
+			}
+			if i < len(bl) && b != "" {
+				st.Deletions++
+			}
+		}
+		d.Files = append(d.Files, st)
+	}
+	return d, nil
 }
 
 // SetOpenPRs sets the open pull requests OpenPullRequests returns.
```

The reconciler still calls `Files`; Task 3 moves it. Until then, build only the forge packages.

- [ ] **Step 4: Run the tests.**
  Run: `go test ./internal/factory/forge/...`
  Expected: `ok` for `forge` and `forgetest`.

- [ ] **Step 5: Commit** (with Task 3's reconciler change if the tree must build at every
  commit; otherwise now):

```bash
git add internal/factory/forge
git commit -m "feat(forge): read the diff's file list and a check's App"
```

### Task 2: The class conditions in config

**Files:**
- Create: `internal/factory/config/classes.go`
- Modify: `internal/factory/config/config.go` (the `Class` type moves out; the class loop calls `validateClass`)
- Test: `internal/factory/config/classes_test.go`

**Interfaces:**
- Produces: `config.Class{Live, Shadow bool; AllowedFiles, ProtectedFiles []string; ModifyOnly bool; MaxPatchFiles, MaxModifiedLines int; DiffShape string; HoldoutChecks []string; MaxPerDay int}`, `config.DiffShapes() []string`, `config.BuiltInShape(name string) string`, `(config.Class).ReadsDiff(name string) bool`.

- [ ] **Step 1: Write the failing test** (offline proof 3, its decoder half):

```go
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

const classesLine = "classes: {docs-links: {shadow: true}, revert: {shadow: true}, docs: {}, tests: {}, dashboards: {}}"

const deadLinks = `dead-links: {allowedFiles: ['^(docs|website/content)/.+\.md$'], protectedFiles: ['^docs/(superpowers|specs)/'],
    modifyOnly: true, maxPatchFiles: 3, maxModifiedLines: 20, diffShape: links-only, holdoutChecks: ["External links 🔗"], maxPerDay: 1}`

func withClasses(t *testing.T, classes string) string {
	t.Helper()
	if !strings.Contains(good, classesLine) {
		t.Fatal("the fixture's classes line moved")
	}
	return strings.Replace(good, classesLine, "classes:\n  "+classes, 1)
}

// MC1: a class declares its conditions by name; absent, it is decided as before.
func TestDeclaredConditionsParse(t *testing.T) {
	c, err := Parse([]byte(withClasses(t, "docs-links: {shadow: true}\n  revert: {shadow: true}\n  "+deadLinks)))
	if err != nil {
		t.Fatal(err)
	}
	cl := c.RepositoryFor("Smana/cloud-native-ref").Classes["dead-links"]
	if !cl.ModifyOnly || cl.MaxModifiedLines != 20 || cl.DiffShape != "links-only" || cl.HoldoutChecks[0] != "External links 🔗" || cl.Live || cl.Shadow {
		t.Fatalf("%+v", cl)
	}
	if !cl.ReadsDiff("dead-links") || !(Class{}).ReadsDiff("docs-links") || (Class{Shadow: true}).ReadsDiff("docs") {
		t.Fatal("ReadsDiff: a declared file condition or a built-in shape reads the diff, nothing else does")
	}
}

// Offline proof 3: an unknown condition fails the strict decoder; a bad value fails Validate.
func TestBadConditionsFail(t *testing.T) {
	for name, c := range map[string]struct{ classes, err string }{
		"gh-aw's spelling":   {"docs-links: {live: true, max-patch-size: 3}\n  revert: {}", `unknown field "max-patch-size"`},
		"an unknown key":     {"docs-links: {live: true, maxPatchSize: 3}\n  revert: {}", `unknown field "maxPatchSize"`},
		"a bad pattern":      {"docs-links: {}\n  revert: {}\n  x: {allowedFiles: ['(']}", `class x: allowedFiles "(" is not a regular expression`},
		"an empty pattern":   {"docs-links: {}\n  revert: {}\n  x: {protectedFiles: ['']}", `class x: protectedFiles "" is not a regular expression`},
		"a negative cap":     {"docs-links: {}\n  revert: {}\n  x: {maxModifiedLines: -1}", "class x: maxModifiedLines must be 0 (no cap) or more"},
		"an unknown shape":   {"docs-links: {}\n  revert: {}\n  x: {diffShape: semver-patch}", `class x: diffShape "semver-patch" is not one of [links-only]`},
		"a holdout twice":    {"docs-links: {}\n  revert: {}\n  x: {holdoutChecks: [a, a]}", `class x: holdoutChecks names "a" empty or twice`},
		"live and shadow":    {"docs-links: {live: true, shadow: true}\n  revert: {}", "class docs-links: live and shadow never together (R32)"},
		"a negative per day": {"docs-links: {}\n  revert: {}\n  x: {maxPerDay: -2}", "class x: maxPerDay must be 0 (no cap) or more"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(withClasses(t, c.classes)))
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("got %v, want %q", err, c.err)
			}
		})
	}
}
```

- [ ] **Step 2: Run it.**
  Run: `go test ./internal/factory/config/ -run 'TestDeclaredConditionsParse|TestBadConditionsFail'`
  Expected: FAIL, `unknown field "allowedFiles"`.

- [ ] **Step 3: Implement.** `classes.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"regexp"
	"slices"
)

// Class is one merge class (§5.2): live classes auto-merge once policy-bot agrees; a shadow
// class is decided exactly like a live one and never armed until the wave (R32, owner,
// 2026-09-27); an unmarked class is a prediction and a PR label only (OD-8). Never both.
//
// The other fields are the class's declared conditions (merge gate classes MC1), named as gh-aw
// names them where it has a name. Each is checked on top of the gate's fixed base, which no class
// can remove; an absent one is not checked, so a class that declares none is decided as before.
type Class struct {
	Live   bool `json:"live,omitempty"`
	Shadow bool `json:"shadow,omitempty"`
	// AllowedFiles: every changed path matches one of these regular expressions.
	AllowedFiles []string `json:"allowedFiles,omitempty"`
	// ProtectedFiles: no changed path matches one of these.
	ProtectedFiles []string `json:"protectedFiles,omitempty"`
	// ModifyOnly: no file is added, deleted, renamed or copied.
	ModifyOnly bool `json:"modifyOnly,omitempty"`
	// MaxPatchFiles caps the changed files and MaxModifiedLines their additions plus deletions,
	// as GitHub counts them (policy-bot's modified_lines); 0 is no cap.
	MaxPatchFiles    int `json:"maxPatchFiles,omitempty"`
	MaxModifiedLines int `json:"maxModifiedLines,omitempty"`
	// DiffShape is a shape every changed line must have: links-only is R52's LinksOnly.
	DiffShape string `json:"diffShape,omitempty"`
	// HoldoutChecks are checks GitHub Actions must report SUCCESS on the head; their workflow is
	// a gate path, so the pull request cannot change them (loops L7).
	HoldoutChecks []string `json:"holdoutChecks,omitempty"`
	// MaxPerDay caps this class's merges a day, under merge.autoMergesPerDay; 0 is no cap of its own.
	MaxPerDay int `json:"maxPerDay,omitempty"`
}

// DiffShapes are the shapes diffShape names.
func DiffShapes() []string { return []string{"links-only"} }

// BuiltInShape is the diff shape the gate holds a class to when its config names none: R52's
// promise of the two entry classes (MC1: their conditions become built-in defaults at parity).
func BuiltInShape(name string) string {
	if name == "docs-links" || name == "revert" {
		return "links-only"
	}
	return ""
}

// ReadsDiff reports whether deciding the class reads the pull request's diff: a built-in shape
// or any declared condition on its files.
func (c Class) ReadsDiff(name string) bool {
	return BuiltInShape(name) != "" || len(c.AllowedFiles) > 0 || len(c.ProtectedFiles) > 0 || c.ModifyOnly ||
		c.MaxPatchFiles > 0 || c.MaxModifiedLines > 0 || c.DiffShape != ""
}

// validateClass reports one class's problems.
func validateClass(name string, cl Class, bad func(string, ...any)) {
	if cl.Live && cl.Shadow {
		bad("class %s: live and shadow never together (R32)", name)
	}
	for _, f := range []struct {
		key  string
		pats []string
	}{{"allowedFiles", cl.AllowedFiles}, {"protectedFiles", cl.ProtectedFiles}} {
		for _, p := range f.pats {
			if _, err := regexp.Compile(p); err != nil || p == "" {
				bad("class %s: %s %q is not a regular expression", name, f.key, p)
			}
		}
	}
	for _, f := range []struct {
		key string
		n   int
	}{{"maxPatchFiles", cl.MaxPatchFiles}, {"maxModifiedLines", cl.MaxModifiedLines}, {"maxPerDay", cl.MaxPerDay}} {
		if f.n < 0 {
			bad("class %s: %s must be 0 (no cap) or more", name, f.key)
		}
	}
	if cl.DiffShape != "" && !slices.Contains(DiffShapes(), cl.DiffShape) {
		bad("class %s: diffShape %q is not one of %v", name, cl.DiffShape, DiffShapes())
	}
	if b := BuiltInShape(name); b != "" && cl.DiffShape != "" && cl.DiffShape != b {
		bad("class %s: diffShape is %s, built in (R52)", name, b)
	}
	seen := map[string]bool{}
	for _, h := range cl.HoldoutChecks {
		if h == "" || seen[h] {
			bad("class %s: holdoutChecks names %q empty or twice", name, h)
		}
		seen[h] = true
	}
}
```

`config.go`:

```diff
diff --git a/internal/factory/config/config.go b/internal/factory/config/config.go
index 459e5eb..e06eab5 100644
--- a/internal/factory/config/config.go
+++ b/internal/factory/config/config.go
@@ -300,14 +300,6 @@ type Triage struct {
 	ControlPercent int `json:"controlPercent"`
 }
 
-// Class is one merge class (§5.2): live classes auto-merge once policy-bot agrees; a shadow
-// class is decided exactly like a live one and never armed until the wave (R32, owner,
-// 2026-09-27); an unmarked class is a prediction and a PR label only (OD-8). Never both.
-type Class struct {
-	Live   bool `json:"live,omitempty"`
-	Shadow bool `json:"shadow,omitempty"`
-}
-
 // Merge is §5.1's merge actor and §6.4's rollback.
 type Merge struct {
 	// The 8 contexts classic protection requires, by name (GitHub Actions, app 15368).
@@ -1023,10 +1015,8 @@ func (c *Config) validateRepository(r *Repository, seenSchedule map[string]bool,
 			bad("class %s is required (OD-8)", name)
 		}
 	}
-	for name, cl := range r.Classes {
-		if cl.Live && cl.Shadow {
-			bad("class %s: live and shadow never together (R32)", name)
-		}
+	for _, name := range slices.Sorted(maps.Keys(r.Classes)) {
+		validateClass(name, r.Classes[name], bad)
 	}
 	if _, ok := r.Classes["review"]; ok {
 		bad("review is the implicit class of everything else; do not declare it")
```

- [ ] **Step 4: Run the package.**
  Run: `go test ./internal/factory/config/`
  Expected: `ok`.

- [ ] **Step 5: Commit.**

```bash
git add internal/factory/config
git commit -m "feat(config): declare a merge class's conditions"
```

### Task 3: `DecideArm` as base, author and declared conditions

**Files:**
- Create: `internal/factory/reconciler/gate.go`
- Modify: `internal/factory/reconciler/arm.go`, `internal/factory/reconciler/merge.go`
- Test: `internal/factory/reconciler/gate_test.go`, `internal/factory/reconciler/merge_gate_test.go`; `arm_test.go`, `merge_test.go`, `ledger_test.go` unchanged

**Interfaces:**
- Consumes: Task 1's `forge.FileStat`, `forge.ActionsApp`, `Merger.Diff`; Task 2's `config.Class`, `BuiltInShape`, `ReadsDiff`.
- Produces: `ArmInputs.Stats []forge.FileStat`, `ArmInputs.DiffRead bool`, `ArmInputs.ClassArmedToday int`; `reconciler.Condition{Key string; Declared func(config.Class) bool; Check func(config.Class, ArmInputs) (reason string, wait bool)}`, `reconciler.Conditions() []Condition`; `(*Reconciler).armedTodayOf(ctx, rc, class string) (int, error)`.

- [ ] **Step 1: Record the parity files' hashes.**

```bash
sha256sum internal/factory/reconciler/arm_test.go internal/factory/reconciler/merge_test.go internal/factory/reconciler/ledger_test.go | tee /tmp/parity.sha
```
  Expected: `876e7fd82c7bc01699f0f98bbdea394135cbfefd30b511f94262c12c0dc18af9`,
  `dce85b826bf6ff27b74bfefdc4e373a8933a745d969a5c47ed36534a27f54c7b`,
  `88c45d54c3bea98356148778af6b4c0a0acb2235be1b41e0a3ba82485dfb059d`.

- [ ] **Step 2: Write the failing tests** (offline proof 2). `gate_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"

	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

// declaredClass is a class declaring every PR 1 condition, as dead-links will (MC1).
func declaredClass() config.Class {
	return config.Class{Live: true, AllowedFiles: []string{`^(docs|website/content)/.+\.md$`},
		ProtectedFiles: []string{`^docs/(superpowers|specs)/`}, ModifyOnly: true, MaxPatchFiles: 3, MaxModifiedLines: 20,
		DiffShape: "links-only", HoldoutChecks: []string{"External links 🔗"}, MaxPerDay: 1}
}

// gateInputs passes every condition of declaredClass: a green holdout from GitHub Actions and a
// one-line retarget, its diff read.
func gateInputs() ArmInputs {
	checks := green("SUCCESS")
	checks.Runs = append(checks.Runs, forge.Check{Name: "External links 🔗", State: "SUCCESS", App: forge.ActionsApp})
	in := ArmInputs{PR: agentPR(), Checks: checks, Task: armTask("dead-links", "solo", ""), ReportedHead: head,
		Files: changed("[x](https://example.com/old)", "[x](https://example.com/new)"), DiffRead: true,
		Stats: []forge.FileStat{{Path: testFile, Status: "modified", Additions: 1, Deletions: 1}}}
	in.Cfg = armCfg()
	in.Cfg.Classes["dead-links"] = declaredClass()
	return in
}

// Offline proof 2: each condition, failed alone, yields exactly its reason.
func TestEachDeclaredConditionRefusesAlone(t *testing.T) {
	if d := DecideArm(gateInputs()); d.Verdict != "arm" || d.Matched != "dead-links" {
		t.Fatalf("every condition holds: %+v", d)
	}
	for name, c := range map[string]struct {
		mutate          func(*ArmInputs)
		verdict, reason string
	}{
		"a file outside allowedFiles": {func(in *ArmInputs) {
			in.Stats = append(in.Stats, forge.FileStat{Path: "README.md", Status: "modified", Additions: 1})
		}, "human", "file_not_allowed"},
		"a protected file": {func(in *ArmInputs) { in.Stats[0].Path = "docs/specs/x.md" }, "human", "file_protected"},
		"an added file":    {func(in *ArmInputs) { in.Stats[0].Status = "added" }, "human", "file_added_or_deleted"},
		"a renamed file": {func(in *ArmInputs) { in.Stats[0].Status, in.Stats[0].Previous = "renamed", "docs/b.md" },
			"human", "file_added_or_deleted"},
		"four files": {func(in *ArmInputs) {
			for _, p := range []string{"docs/b.md", "docs/c.md", "docs/d.md"} {
				in.Stats = append(in.Stats, forge.FileStat{Path: p, Status: "modified"})
			}
		}, "human", "patch_too_large"},
		"21 lines":             {func(in *ArmInputs) { in.Stats[0].Additions = 20 }, "human", "patch_too_large"},
		"beyond link targets":  {func(in *ArmInputs) { in.Files = changed("text.", "text, reworded.") }, "human", "class_mismatch"},
		"the holdout red":      {func(in *ArmInputs) { in.Checks.Runs[2].State = "FAILURE" }, "human", "holdout_red"},
		"the holdout foreign":  {func(in *ArmInputs) { in.Checks.Runs[2].App = 1 }, "human", "holdout_red"},
		"the holdout running":  {func(in *ArmInputs) { in.Checks.Runs[2].State = "PENDING" }, "wait", "holdout_pending"},
		"the holdout not seen": {func(in *ArmInputs) { in.Checks.Runs = in.Checks.Runs[:2] }, "wait", "holdout_pending"},
		"the class's day":      {func(in *ArmInputs) { in.ClassArmedToday = 1 }, "human", "auto_merge_cap"},
	} {
		t.Run(name, func(t *testing.T) {
			in := gateInputs()
			c.mutate(&in)
			if d := DecideArm(in); d.Verdict != c.verdict || d.Reason != c.reason || d.Matched != "dead-links" {
				t.Fatalf("got %+v, want %s/%s/dead-links", d, c.verdict, c.reason)
			}
		})
	}
}

// The conditions on the diff hold until it is read: the first pass of awaitingCI decides
// without it, and only a decision that would arm or shadow reads it.
func TestDiffConditionsWaitForTheDiff(t *testing.T) {
	in := gateInputs()
	in.Stats, in.DiffRead = []forge.FileStat{{Path: "README.md", Status: "added", Additions: 99}}, false
	if d := DecideArm(in); d.Verdict != "arm" {
		t.Fatalf("an unread diff holds every condition on it: %+v", d)
	}
	in.DiffRead = true
	if d := DecideArm(in); d.Reason != "file_not_allowed" {
		t.Fatalf("read, it decides: %+v", d)
	}
}

// The base holds for a class that declares nothing: docs is label-only, so class_mismatch/live
// as today; a declared class with nothing but a switch is decided by the base alone.
func TestTheBaseHoldsForAClassThatDeclaresNothing(t *testing.T) {
	in := gateInputs()
	in.Cfg.Classes["dead-links"] = config.Class{Shadow: true}
	in.Stats, in.Files = []forge.FileStat{{Path: "README.md", Status: "added", Additions: 99}}, changed("a", "b")
	if d := DecideArm(in); d.Verdict != "shadow" || d.Reason != "shadow_would_arm" {
		t.Fatalf("no declared condition: the base alone decides, %+v", d)
	}
	in.Checks.Runs[1].State = "FAILURE"
	if d := DecideArm(in); d.Verdict != "ci_red" {
		t.Fatalf("the base still refuses: %+v", d)
	}
}
```

`merge_gate_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"
	"time"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

// declaredRig is mergeRig's task in a class that declares conditions (MC1): its gate reads the
// diff and the holdout, as dead-links' will.
func declaredRig(t *testing.T) *rig {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	var tk v1alpha1.Task
	if err := g.c.Get(t.Context(), keyFor("3buqdlot"), &tk); err != nil {
		t.Fatal(err)
	}
	tk.Spec.PredictedClass = "dead-links"
	if err := g.c.Update(t.Context(), &tk); err != nil {
		t.Fatal(err)
	}
	g.r.Cfg.Classes["dead-links"] = declaredClass()
	checks := green("SUCCESS")
	checks.Runs = append(checks.Runs, forge.Check{Name: "External links 🔗", State: "SUCCESS", App: forge.ActionsApp})
	g.f.SetChecks(12, checks)
	g.f.SetFiles(map[string]string{testFile: "[x](https://example.com/old)"}, map[string]string{testFile: "[x](https://example.com/new)"})
	return g
}

// The second pass reads the diff for any class with a condition on it, and decides on GitHub's
// own list of the changed files.
func TestADeclaredClassIsDecidedOnItsDiff(t *testing.T) {
	g := declaredRig(t)
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAutoMerging || len(g.f.Armed()) != 1 {
		t.Fatalf("a clean retarget arms: %s %s", tk.Status.Phase, tk.Status.Reason)
	}
	g = declaredRig(t)
	g.f.SetFileStats(forge.FileStat{Path: testFile, Status: "modified", Additions: 15, Deletions: 6})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "patch_too_large" ||
		len(g.f.Armed()) != 0 {
		t.Fatalf("21 lines refuse: %s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// maxPerDay counts only the class's own merges: another class's merge today leaves room.
func TestMaxPerDayCountsTheClassOnly(t *testing.T) {
	g := declaredRig(t)
	g.f.SetPR(forge.PR{Number: 9, State: "MERGED", MergedBy: "ogenki-agent-merger[bot]", MergedAt: now.Add(-time.Hour),
		Labels: []string{AutoMergedLabel("docs-links")}})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAutoMerging {
		t.Fatalf("docs-links' merge is not dead-links': %s %s", tk.Status.Phase, tk.Status.Reason)
	}
	g = declaredRig(t)
	g.f.SetPR(forge.PR{Number: 9, State: "MERGED", MergedBy: "ogenki-agent-merger[bot]", MergedAt: now.Add(-time.Hour),
		Labels: []string{AutoMergedLabel("dead-links")}})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Reason != "auto_merge_cap" {
		t.Fatalf("one dead-links merge today fills its day: %s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// A pending holdout waits in AwaitingCI, like CI itself.
func TestAPendingHoldoutWaits(t *testing.T) {
	g := declaredRig(t)
	checks := green("SUCCESS")
	checks.Runs = append(checks.Runs, forge.Check{Name: "External links 🔗", State: "PENDING", App: forge.ActionsApp})
	g.f.SetChecks(12, checks)
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAwaitingCI || tk.Status.Reason != "holdout_pending" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
}
```

- [ ] **Step 3: Run them.**
  Run: `go test ./internal/factory/reconciler/ -run 'TestEachDeclaredConditionRefusesAlone|TestDiffConditionsWaitForTheDiff|TestTheBaseHoldsForAClassThatDeclaresNothing|TestADeclaredClassIsDecidedOnItsDiff|TestMaxPerDayCountsTheClassOnly|TestAPendingHoldoutWaits'`
  Expected: build failure, `unknown field DiffRead in struct literal of type ArmInputs`.

- [ ] **Step 4: Implement.** `gate.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"regexp"
	"slices"

	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

// Condition is one condition a class declares in its config (merge gate classes MC1): Declared
// reads whether the class names it; Check is "" when it holds on the decision's inputs, else the
// reason it refuses with. A condition on the diff holds until the diff is read (in.DiffRead):
// awaitingCI reads it only for a decision that would arm or shadow, then decides again.
type Condition struct {
	Key      string
	Declared func(cl config.Class) bool
	Check    func(cl config.Class, in ArmInputs) (reason string, wait bool)
}

// Conditions are the declared conditions, in the order the gate checks them.
func Conditions() []Condition {
	return []Condition{
		{"allowedFiles", func(cl config.Class) bool { return len(cl.AllowedFiles) > 0 }, allowedFiles},
		{"protectedFiles", func(cl config.Class) bool { return len(cl.ProtectedFiles) > 0 }, protectedFiles},
		{"modifyOnly", func(cl config.Class) bool { return cl.ModifyOnly }, modifyOnly},
		{"maxPatchFiles", func(cl config.Class) bool { return cl.MaxPatchFiles > 0 }, maxPatchFiles},
		{"maxModifiedLines", func(cl config.Class) bool { return cl.MaxModifiedLines > 0 }, maxModifiedLines},
		{"diffShape", func(cl config.Class) bool { return cl.DiffShape != "" }, diffShape},
		{"holdoutChecks", func(cl config.Class) bool { return len(cl.HoldoutChecks) > 0 }, holdoutChecks},
	}
}

// changedPaths are every path the diff touches, a rename's two names included.
func changedPaths(in ArmInputs) []string {
	var out []string
	for _, f := range in.Stats {
		out = append(out, f.Path)
		if f.Previous != "" {
			out = append(out, f.Previous)
		}
	}
	return out
}

// matchesAny: path matches one of pats. A pattern that does not compile matches nothing, so an
// allow list fails closed and a protect list is caught by Validate before it ever runs.
func matchesAny(pats []string, path string) bool {
	return slices.ContainsFunc(pats, func(p string) bool {
		re, err := regexp.Compile(p)
		return err == nil && re.MatchString(path)
	})
}

func allowedFiles(cl config.Class, in ArmInputs) (string, bool) {
	for _, p := range changedPaths(in) {
		if !matchesAny(cl.AllowedFiles, p) {
			return "file_not_allowed", false
		}
	}
	return "", false
}

func protectedFiles(cl config.Class, in ArmInputs) (string, bool) {
	if slices.ContainsFunc(changedPaths(in), func(p string) bool { return matchesAny(cl.ProtectedFiles, p) }) {
		return "file_protected", false
	}
	return "", false
}

func modifyOnly(_ config.Class, in ArmInputs) (string, bool) {
	if slices.ContainsFunc(in.Stats, func(f forge.FileStat) bool { return f.Status != "modified" }) {
		return "file_added_or_deleted", false
	}
	return "", false
}

func maxPatchFiles(cl config.Class, in ArmInputs) (string, bool) {
	if len(in.Stats) > cl.MaxPatchFiles {
		return "patch_too_large", false
	}
	return "", false
}

func maxModifiedLines(cl config.Class, in ArmInputs) (string, bool) {
	n := 0
	for _, f := range in.Stats {
		n += f.Additions + f.Deletions
	}
	if n > cl.MaxModifiedLines {
		return "patch_too_large", false
	}
	return "", false
}

// diffShape: links-only is R52's promise, read on in.Files as it always was, so a diff not read
// yet passes it vacuously (LinksOnly of two empty trees).
func diffShape(cl config.Class, in ArmInputs) (string, bool) {
	if cl.DiffShape == "links-only" {
		if ok, _ := LinksOnly(in.Files.Base, in.Files.Head); !ok {
			return "class_mismatch", false
		}
	}
	return "", false
}

// holdoutChecks (loops L7): each named check passed on the head, posted by GitHub Actions. Not
// reported yet waits; failed, cancelled or posted by another App refuses.
func holdoutChecks(cl config.Class, in ArmInputs) (string, bool) {
	wait := false
	for _, name := range cl.HoldoutChecks {
		i := slices.IndexFunc(in.Checks.Runs, func(x forge.Check) bool { return x.Name == name })
		switch {
		case i < 0 || in.Checks.Runs[i].State == "PENDING":
			wait = true
		case in.Checks.Runs[i].App != forge.ActionsApp || in.Checks.Runs[i].State != "SUCCESS":
			return "holdout_red", false
		}
	}
	if wait {
		return "holdout_pending", true
	}
	return "", false
}

// onDiff are the conditions that read the diff, which hold until it is read.
var onDiff = []string{"allowedFiles", "protectedFiles", "modifyOnly", "maxPatchFiles", "maxModifiedLines"}

// declared checks the class's declared conditions in order: the first refusal, or "".
func declared(cl config.Class, in ArmInputs) (string, bool) {
	for _, c := range Conditions() {
		if !c.Declared(cl) || (!in.DiffRead && slices.Contains(onDiff, c.Key)) {
			continue
		}
		if reason, wait := c.Check(cl, in); reason != "" {
			return reason, wait
		}
	}
	return "", false
}
```

`arm.go` (`DecideArm` composed; `baseChecks`, `gateOf`, `agentEvidence` factored out of it):

```diff
diff --git a/internal/factory/reconciler/arm.go b/internal/factory/reconciler/arm.go
index 6deeefd..f8d3451 100644
--- a/internal/factory/reconciler/arm.go
+++ b/internal/factory/reconciler/arm.go
@@ -31,6 +31,12 @@ type ArmInputs struct {
 	// NotOwned: the repository's fence file does not name this factory (v2 D7). It never merges
 	// there, whatever else holds.
 	NotOwned bool
+	// Stats is GitHub's list of the changed files, and DiffRead is true once Files and Stats hold
+	// the pull request's diff: until then the declared conditions on the diff hold (MC1).
+	Stats    []forge.FileStat
+	DiffRead bool
+	// ClassArmedToday is today's merges of the task's class, against the class's maxPerDay.
+	ClassArmedToday int
 }
 
 // ArmDecision is the verdict and why. Verdict ∈ arm | shadow | wait | ci_red | ci_cancelled | escalate | human.
@@ -69,61 +75,105 @@ func CIState(c forge.Checks, required []string) string {
 	return state
 }
 
-// DecideArm is §5.1's merge-actor rule. Every "no" fails towards a human (property 3).
+// DecideArm is §5.1's merge-actor rule: a fixed base no class can remove, one author mode, and
+// the conditions the class declares (merge gate classes MC1). Every "no" fails towards a human
+// (property 3), each with its own reason.
 func DecideArm(in ArmInputs) ArmDecision {
 	rc := in.Cfg.RepositoryFor(in.Task.Spec.Repository)
+	if d, done := baseChecks(in, rc); done {
+		return d
+	}
+	human := func(reason, matched string) ArmDecision {
+		return ArmDecision{Verdict: "human", Reason: reason, Matched: matched}
+	}
+	if !forge.SameLogin(in.PR.Author, in.Cfg.AgentsLogin) {
+		return human("not_agent_authored", "")
+	}
+	for _, rv := range in.PR.Reviews {
+		if rv.State == "APPROVED" && rc.IsMaintainer(rv.Author) {
+			return human("maintainer_approved", "review") // success came from a human: a human merges
+		}
+	}
+	class := in.Task.Spec.PredictedClass
+	cl := gateOf(rc, class)
+	if !cl.Live && !cl.Shadow {
+		return human("class_mismatch", "live") // the diff matched a live class the triage did not predict
+	}
+	if reason := agentEvidence(in); reason != "" {
+		return human(reason, class)
+	}
+	if reason, wait := declared(cl, in); wait {
+		return ArmDecision{Verdict: "wait", Reason: reason, Matched: class}
+	} else if reason != "" {
+		return human(reason, class)
+	}
+	if in.Paused {
+		return human("class_paused", class)
+	}
+	if in.ArmedToday >= rc.Merge.AutoMergesPerDay || (cl.MaxPerDay > 0 && in.ClassArmedToday >= cl.MaxPerDay) {
+		return human("auto_merge_cap", class)
+	}
+	if !cl.Live { // R32: every condition holds, and the gate is in shadow until the wave
+		return ArmDecision{Verdict: "shadow", Reason: "shadow_would_arm", Matched: class}
+	}
+	return ArmDecision{Verdict: "arm", Matched: class}
+}
+
+// gateOf is the class as the gate reads it: its config, with the diff shape built into
+// docs-links and revert (R52) when the config names none.
+func gateOf(rc *config.Repository, class string) config.Class {
+	cl := rc.Classes[class]
+	if cl.DiffShape == "" {
+		cl.DiffShape = config.BuiltInShape(class)
+	}
+	return cl
+}
+
+// baseChecks are the base's rows before any class is read: the secret scan, CI, the fence and
+// policy-bot. done is false when all of them hold.
+func baseChecks(in ArmInputs, rc *config.Repository) (ArmDecision, bool) {
 	// R42 (review G6): TruffleHog runs --only-verified, so a red scan is a live credential in the
 	// diff. A fix run would remove it from the head and leave it in a public history.
 	if i := slices.IndexFunc(in.Checks.Runs, func(x forge.Check) bool { return x.Name == rc.Merge.LeakScanCheck }); i >= 0 &&
 		in.Checks.Runs[i].State == "FAILURE" {
-		return ArmDecision{Verdict: "escalate", Reason: "secret_scan_red"}
+		return ArmDecision{Verdict: "escalate", Reason: "secret_scan_red"}, true
 	}
 	switch CIState(in.Checks, rc.Merge.RequiredChecks) {
 	case "FAILURE":
-		return ArmDecision{Verdict: "ci_red", Reason: "ci_red"}
+		return ArmDecision{Verdict: "ci_red", Reason: "ci_red"}, true
 	case "CANCELLED":
-		return ArmDecision{Verdict: "ci_cancelled", Reason: "ci_cancelled"}
+		return ArmDecision{Verdict: "ci_cancelled", Reason: "ci_cancelled"}, true
 	case "PENDING":
-		return ArmDecision{Verdict: "wait", Reason: "ci_pending"}
-	}
-	human := func(reason, matched string) ArmDecision {
-		return ArmDecision{Verdict: "human", Reason: reason, Matched: matched}
+		return ArmDecision{Verdict: "wait", Reason: "ci_pending"}, true
 	}
 	if in.NotOwned {
-		return human("not_owned", "")
+		return ArmDecision{Verdict: "human", Reason: "not_owned"}, true
 	}
-	class := in.Task.Spec.PredictedClass
 	state, creator := in.Checks.StatusOf("policy-bot: main")
 	if !forge.SameLogin(creator, rc.Merge.PolicyBotLogin) {
-		return human("policy_absent", "") // policy-bot down, or a status someone else posted
+		return ArmDecision{Verdict: "human", Reason: "policy_absent"}, true // policy-bot down, or a status someone else posted
 	}
 	switch state {
 	case "ERROR":
-		return human("gate_path", "gate") // unmergeable, even with an approval (S9)
+		return ArmDecision{Verdict: "human", Reason: "gate_path", Matched: "gate"}, true // unmergeable, even with an approval (S9)
 	case "SUCCESS":
+		return ArmDecision{}, false
 	default:
-		return human("policy_pending", "review")
-	}
-	if !forge.SameLogin(in.PR.Author, in.Cfg.AgentsLogin) {
-		return human("not_agent_authored", "")
-	}
-	for _, rv := range in.PR.Reviews {
-		if rv.State == "APPROVED" && rc.IsMaintainer(rv.Author) {
-			return human("maintainer_approved", "review") // success came from a human: a human merges
-		}
-	}
-	cl := rc.Classes[class]
-	if !cl.Live && !cl.Shadow {
-		return human("class_mismatch", "live") // the diff matched a live class the triage did not predict
+		return ArmDecision{Verdict: "human", Reason: "policy_pending", Matched: "review"}, true
 	}
+}
+
+// agentEvidence is the agents author mode's evidence (R02, R03, SC-14): "" when the room
+// reported this head, the verifiers approved it and its trailer names one of the task's runs.
+func agentEvidence(in ArmInputs) string {
 	if slices.Contains(in.Cfg.Templates[in.Task.Spec.Template].Roles, "reviewer") && in.Task.Status.Verdict != "approve" {
-		return human("no_approving_verdict", class)
+		return "no_approving_verdict"
 	}
 	// R02: the rule can only speak about the commit the room named. An unreported head — a run
 	// pushed without a handoff, or a push after the last report, the trailer inside the head
 	// notwithstanding — is a commit nobody watched.
 	if in.ReportedHead == "" || in.ReportedHead != in.PR.HeadSHA {
-		return human("head_unreported", class)
+		return "head_unreported"
 	}
 	// R03: an approve is of the head its run was given, and of it only. If the verifier's last
 	// approve predates the head the room just reported, the approval was pulled out from under.
@@ -133,31 +183,15 @@ func DecideArm(in ArmInputs) ArmDecision {
 			continue
 		}
 		if r.HeadSHA != in.PR.HeadSHA {
-			return human("verdict_stale", class)
+			return "verdict_stale"
 		}
 		break
 	}
 	trailer := in.PR.Trailer("Agent-Run")
 	if !slices.ContainsFunc(in.Task.Status.Runs, func(r v1alpha1.RunRecord) bool { return r.ID == trailer }) {
-		return human("foreign_trailer", class) // SC-14, SP1 R9: another task's run pushed here
+		return "foreign_trailer" // SC-14, SP1 R9: another task's run pushed here
 	}
-	// R52: the two entry classes carry a promise about the diff, not just the head: a docs-links
-	// or revert change that moved anything but link targets is not the class it claims.
-	if class == "docs-links" || class == "revert" {
-		if ok, _ := LinksOnly(in.Files.Base, in.Files.Head); !ok {
-			return human("class_mismatch", class)
-		}
-	}
-	if in.Paused {
-		return human("class_paused", class)
-	}
-	if in.ArmedToday >= rc.Merge.AutoMergesPerDay {
-		return human("auto_merge_cap", class)
-	}
-	if !cl.Live { // R32: every condition holds, and the gate is in shadow until the wave
-		return ArmDecision{Verdict: "shadow", Reason: "shadow_would_arm", Matched: class}
-	}
-	return ArmDecision{Verdict: "arm", Matched: class}
+	return ""
 }
 
 // reLinkTarget matches one link's target: a reference definition's ([label]: target at a line
```

`merge.go` (the second pass on `ReadsDiff`, and the class's day):

```diff
diff --git a/internal/factory/reconciler/merge.go b/internal/factory/reconciler/merge.go
index cba106e..1e1174b 100644
--- a/internal/factory/reconciler/merge.go
+++ b/internal/factory/reconciler/merge.go
@@ -73,7 +73,11 @@ func (r *Reconciler) awaitingCI(ctx context.Context, t *v1alpha1.Task) error {
 	if err != nil {
 		return err
 	}
-	in := ArmInputs{PR: pr, Checks: checks, Task: t, Cfg: r.Cfg, ArmedToday: today, Paused: paused,
+	classToday, err := r.armedTodayOf(ctx, r.repo(t), t.Spec.PredictedClass)
+	if err != nil {
+		return err
+	}
+	in := ArmInputs{PR: pr, Checks: checks, Task: t, Cfg: r.Cfg, ArmedToday: today, ClassArmedToday: classToday, Paused: paused,
 		NotOwned: r.Owned == nil || !r.Owned(t.Spec.Repository)}
 	if CIState(checks, r.repo(t).Merge.RequiredChecks) == "SUCCESS" {
 		// R02/R03: the rule can only speak about a commit the room reported and the verifiers
@@ -85,15 +89,15 @@ func (r *Reconciler) awaitingCI(ctx context.Context, t *v1alpha1.Task) error {
 		in.Verifiers = verifiers(t)
 	}
 	d := DecideArm(in)
-	if (d.Verdict == "arm" || d.Verdict == "shadow") && (t.Spec.PredictedClass == "docs-links" || t.Spec.PredictedClass == "revert") {
-		// forge.Files is read late: only a decision that would reach the LinksOnly gate of an
-		// entry class pays its 2N+1 API calls. The first pass passed that gate vacuously; this
-		// second pass decides on the real diff.
-		base, head, ferr := r.merger(ctx).Files(ctx, "main", pr.HeadSHA)
+	if (d.Verdict == "arm" || d.Verdict == "shadow") && r.repo(t).Classes[t.Spec.PredictedClass].ReadsDiff(t.Spec.PredictedClass) {
+		// The diff is read late: only a decision that would arm or shadow a class with a condition
+		// on its diff pays its 2N+1 API calls. The first pass held those conditions vacuously;
+		// this second pass decides on the real diff.
+		diff, ferr := r.merger(ctx).Diff(ctx, "main", pr.HeadSHA)
 		if ferr != nil {
 			return ferr
 		}
-		in.Files.Base, in.Files.Head = base, head
+		in.Files.Base, in.Files.Head, in.Stats, in.DiffRead = diff.Base, diff.Head, diff.Files, true
 		d = DecideArm(in)
 	}
 	if d.Verdict == "arm" || d.Verdict == "shadow" {
@@ -752,6 +756,11 @@ func Demoted(merges []Merge, b config.Breaker) (bool, int) {
 // merge itself went, and §5.1's cap counts the decisions, not their outcomes. GitHub's labelled
 // merges of today, of every class, count too (v2 D1), once per pull request.
 func (r *Reconciler) armedToday(ctx context.Context, rc *config.Repository) (int, error) {
+	return r.armedTodayOf(ctx, rc, "")
+}
+
+// armedTodayOf is armedToday for one class's merges (its maxPerDay), every class's when class is "".
+func (r *Reconciler) armedTodayOf(ctx context.Context, rc *config.Repository, class string) (int, error) {
 	gh, err := r.githubLedger(ctx, rc)
 	if err != nil {
 		return 0, err
@@ -763,12 +772,15 @@ func (r *Reconciler) armedToday(ctx context.Context, rc *config.Repository) (int
 	n, seen := 0, map[int]bool{}
 	for i := range l.Items {
 		if ref := l.Items[i].Status.PullRequest; ref != nil && ref.ArmedAt != nil && sameUTCDay(ref.ArmedAt.Time, r.Now()) &&
-			r.onRepo(&l.Items[i], rc) {
+			r.onRepo(&l.Items[i], rc) && (class == "" || l.Items[i].Spec.PredictedClass == class) {
 			n++
 			seen[ref.Number] = true
 		}
 	}
-	for _, ms := range gh {
+	for c, ms := range gh {
+		if class != "" && c != class {
+			continue
+		}
 		for _, m := range ms {
 			if sameUTCDay(m.At, r.Now()) && !seen[m.Number] {
 				n++
```

- [ ] **Step 5: Run the new tests, then the parity proof** (offline proof 1).

```bash
go test -count=1 ./internal/factory/reconciler/
sha256sum -c /tmp/parity.sha
go test -count=1 -v -run "^($(grep -ho '^func Test[A-Za-z]*' internal/factory/reconciler/arm_test.go internal/factory/reconciler/merge_test.go internal/factory/reconciler/ledger_test.go | sed 's/func //' | paste -sd'|'))\$" ./internal/factory/reconciler/ | grep -c -- '--- PASS'
```
  Expected: `ok`; three `OK` lines; `101`.

- [ ] **Step 6: Commit.**

```bash
git add internal/factory/reconciler
git commit -m "feat(factory): compose the merge gate from a base and declared conditions"
```

### Task 4: The reasons' prose, the docs, the gate

**Files:**
- Modify: `internal/factory/narrate/narrate.go`, `internal/factory/narrate/merge_how_test.go`, `internal/summary/waiting.go`, `docs/factory.md`

- [ ] **Step 1: Write the failing test.** In `merge_how_test.go`, `waitReasons` gains the wait the
  gate can now hold a task in:

```go
	"waiting_daily_budget", "waiting_room_log", "waiting_broker_permission", "holdout_pending",
```
  (replacing the list's last line `"waiting_daily_budget", "waiting_room_log", "waiting_broker_permission",`).

- [ ] **Step 2: Run it.** `go test ./internal/factory/narrate/` — Expected: FAIL, `narrate does not know the wait reason "holdout_pending"`.

- [ ] **Step 3: Add the prose.** In `narrate.go`'s `reasons()`, after `"class_mismatch"`:

```go
		"file_not_allowed":          "it changes a file outside its class's allowed files",
		"file_protected":            "it changes a file its class protects",
		"file_added_or_deleted":     "it adds, deletes, renames or copies a file, which its class does not allow",
		"patch_too_large":           "it changes more files or lines than its class allows",
		"holdout_red":               "a holdout check its class requires failed, or was not posted by GitHub Actions",
		"holdout_pending":           "a holdout check its class requires has not passed yet",
```
  In `internal/summary/waiting.go`'s `reasons()`, after `"ci_pending"`:

```go
		"holdout_pending":           "a holdout check has not passed yet",
```

- [ ] **Step 4: Update `docs/factory.md`.**
  - In "The merge gate", replace row 8 with:
    `| 8 | The class's declared conditions, in this order: \`allowedFiles\`, \`protectedFiles\`, \`modifyOnly\`, \`maxPatchFiles\`, \`maxModifiedLines\` (GitHub's counts), \`diffShape\` (\`links-only\`, built into \`docs-links\` and \`revert\`), \`holdoutChecks\` (SUCCESS from GitHub Actions) | \`file_not_allowed\`, \`file_protected\`, \`file_added_or_deleted\`, \`patch_too_large\`, \`class_mismatch\`, \`holdout_red\`; a pending holdout waits |`
    and row 10 with `| 10 | Under \`merge.autoMergesPerDay\` today, and under the class's \`maxPerDay\` | \`auto_merge_cap\` |`.
    The sentence before the table: "Row 12 is read last" stays; add after the table: "A condition
    on the diff (rows 8's file conditions and shape) is decided on GitHub's own file list, read
    only when every other row would merge or post a shadow decision."
  - In the reasons table, after `class_mismatch`, add one row per new reason with the prose of
    Step 3, and change `class_mismatch`'s text to "The predicted class is neither live nor shadow
    (such as \`review\`), or its \`diffShape\` does not hold".
  - In the config table, `classes` becomes: "`<name>: {live, shadow, allowedFiles, protectedFiles,
    modifyOnly, maxPatchFiles, maxModifiedLines, diffShape, holdoutChecks, maxPerDay}`; `live` and
    `shadow` never both; an absent condition is not checked; `docs-links` and `revert` required and
    hold `diffShape: links-only` built in; `review` never declared".

- [ ] **Step 5: Run the whole gate.**
  Run: `task check` (Docker running).
  Expected: exit 0.

- [ ] **Step 6: Commit and open PR A.**

```bash
git add internal/factory/narrate internal/summary docs/factory.md
git commit -m "docs(factory): the merge gate's declared conditions and their reasons"
git fetch origin && git rebase origin/main && git push -u origin feat/merge-gate-vocabulary
gh pr create --title "feat(factory): the merge gate's vocabulary, at parity" --body "Merge gate classes PR 1 (ai-native-ref docs/superpowers/specs/2026-10-22-merge-gate-classes-design.md, MC1). DecideArm becomes a fixed base, the agents author mode and the conditions a class declares, each with its own reason. docs-links and revert keep links-only built in and declare nothing else. Offline proof 1: arm_test.go, merge_test.go and ledger_test.go unchanged and green (101 cases). Offline proofs 2 and 3 in gate_test.go and classes_test.go."
```

---

## PR B — agent-platform: `dead-links`

### Task 5: `mdlinks`, R52's link reading as a package

**Files:**
- Create: `internal/factory/mdlinks/mdlinks.go`, `internal/factory/mdlinks/mdlinks_test.go`
- Modify: `internal/factory/reconciler/arm.go` (the link code leaves), `internal/factory/reconciler/arm_test.go` (`TestFencesOf`, `TestLinksOnly` leave), `internal/factory/reconciler/gate.go` (`mdlinks.LinksOnly`)

**Interfaces:**
- Produces: `mdlinks.Targets(s string) (text string, targets []string)` (was `linksOf`), `mdlinks.LinksOnly(base, head map[string]string) (bool, string)`.

- [ ] **Step 1: Move the tests verbatim** into `mdlinks_test.go`:

````go
// SPDX-License-Identifier: Apache-2.0

package mdlinks

import (
	"slices"
	"testing"
)

const testFile = "website/content/docs/a.md"

// A closing fence ends its block: each block is its own range, and only an unclosed one runs
// to the end of the text (cloud-native-ref#2517).
func TestFencesOf(t *testing.T) {
	for name, c := range map[string]struct {
		s    string
		want [][2]int
	}{
		"two blocks":            {"```\nx\n```\ntext\n```\ny\n```\n", [][2]int{{0, 9}, {15, 24}}},
		"indented blocks":       {"- a\n  ```\n  x\n  ```\n- b\n\t```\n\t```", [][2]int{{4, 19}, {24, 33}}},
		"unclosed at EOF":       {"a\n```\nb", [][2]int{{2, 7}}},
		"closed, then unclosed": {"```\n```\nt\n```\nu", [][2]int{{0, 7}, {10, 15}}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := fencesOf(c.s); !slices.Equal(got, c.want) {
				t.Fatalf("fencesOf = %v, want %v", got, c.want)
			}
		})
	}
}

// R52's LinksOnly: a docs-links or revert diff may move link targets and nothing else.
func TestLinksOnly(t *testing.T) {
	one := func(s string) map[string]string { return map[string]string{testFile: s} }
	for name, c := range map[string]struct {
		base, headText string
		ok             bool
	}{
		"a relative retarget":             {"see [the guide](docs/old.md).", "see [the guide](docs/new.md).", true},
		"a retarget within the host":      {"[a](https://example.com/old)", "[a](https://example.com/new)", true},
		"a retarget to the archive":       {"[a](https://example.com/gone)", "[a](https://web.archive.org/web/2020/https://example.com/gone)", true},
		"a definition retarget":           {"[a]: https://example.com/old\n[a] and text", "[a]: https://example.com/new\n[a] and text", true},
		"a title unchanged, target moved": {`[a](docs/old.md "The Guide")`, `[a](docs/new.md "The Guide")`, true},
		"relative to absolute":            {"[a](docs/x.md)", "[a](https://example.com/x)", false},
		"a protocol-relative jump":        {"[a](docs/x.md)", "[a](//evil.com/x)", false},
		"a fenced retarget":               {"```\n[a](docs/old.md)\n```", "```\n[a](docs/new.md)\n```", false},
		"an injected script":              {"plain text.", "plain text <script>alert(1)</script>.", false},
		"a changed fenced command":        {"```\ncurl https://example.com/old\n```", "```\ncurl https://example.com/new\n```", false},
		"a reworded sentence":             {"The link is broken.", "The link has rotted.", false},
		"a new external host":             {"[a](https://example.com/x)", "[a](https://evil.example/x)", false},
		"a javascript target":             {"[a](https://example.com/x)", "[a](javascript:alert(1))", false},
		"a link added":                    {"text.", "text. [a](new.md)", false},
		// cloud-native-ref#2517: a code block above the links must not fence everything after it.
		"a retarget after a block":           {"```\nx\n```\n[a](docs/old.md)", "```\nx\n```\n[a](docs/new.md)", true},
		"a retarget between two blocks":      {"```\nx\n```\n[a](old.md)\n```\ny\n```", "```\nx\n```\n[a](new.md)\n```\ny\n```", true},
		"a retarget after two blocks":        {"```\nx\n```\n```\ny\n```\n[a](old.md)", "```\nx\n```\n```\ny\n```\n[a](new.md)", true},
		"a retarget after a list's block":    {"- step:\n  ```sh\n  ls\n  ```\n[a](old.md)", "- step:\n  ```sh\n  ls\n  ```\n[a](new.md)", true},
		"a retarget in a list's block":       {"- step:\n  ```\n  [a](old.md)\n  ```\n", "- step:\n  ```\n  [a](new.md)\n  ```\n", false},
		"a retarget after an unclosed fence": {"```\nx\n[a](old.md)", "```\nx\n[a](new.md)", false},
	} {
		t.Run(name, func(t *testing.T) {
			ok, why := LinksOnly(one(c.base), one(c.headText))
			if ok != c.ok {
				t.Fatalf("ok = %v (%s), want %v", ok, why, c.ok)
			}
			if ok && why != "" {
				t.Fatalf("why = %q on a links-only diff", why)
			}
		})
	}
	t.Run("files", func(t *testing.T) {
		if ok, _ := LinksOnly(map[string]string{"a.md": "keep", "b.md": "[x](old.md)"},
			map[string]string{"a.md": "keep", "b.md": "[x](new.md)"}); !ok {
			t.Fatal("an unchanged file beside a retarget is links-only")
		}
		if ok, _ := LinksOnly(map[string]string{"a.md": "x"}, map[string]string{"a.md": "x", "b.md": "[y](z.md)"}); ok {
			t.Fatal("an added file is not a link retarget")
		}
		if ok, _ := LinksOnly(nil, nil); !ok {
			t.Fatal("no changed file contradicts nothing")
		}
	})
}
````

  and delete them from `arm_test.go`:

````diff
diff --git a/internal/factory/reconciler/arm_test.go b/internal/factory/reconciler/arm_test.go
index ca04579..6b806a4 100644
--- a/internal/factory/reconciler/arm_test.go
+++ b/internal/factory/reconciler/arm_test.go
@@ -90,79 +90,6 @@ func TestHeadAndVerdictMustBeFresh(t *testing.T) {
 	}
 }
 
-// A closing fence ends its block: each block is its own range, and only an unclosed one runs
-// to the end of the text (cloud-native-ref#2517).
-func TestFencesOf(t *testing.T) {
-	for name, c := range map[string]struct {
-		s    string
-		want [][2]int
-	}{
-		"two blocks":            {"```\nx\n```\ntext\n```\ny\n```\n", [][2]int{{0, 9}, {15, 24}}},
-		"indented blocks":       {"- a\n  ```\n  x\n  ```\n- b\n\t```\n\t```", [][2]int{{4, 19}, {24, 33}}},
-		"unclosed at EOF":       {"a\n```\nb", [][2]int{{2, 7}}},
-		"closed, then unclosed": {"```\n```\nt\n```\nu", [][2]int{{0, 7}, {10, 15}}},
-	} {
-		t.Run(name, func(t *testing.T) {
-			if got := fencesOf(c.s); !slices.Equal(got, c.want) {
-				t.Fatalf("fencesOf = %v, want %v", got, c.want)
-			}
-		})
-	}
-}
-
-// R52's LinksOnly: a docs-links or revert diff may move link targets and nothing else.
-func TestLinksOnly(t *testing.T) {
-	one := func(s string) map[string]string { return map[string]string{testFile: s} }
-	for name, c := range map[string]struct {
-		base, headText string
-		ok             bool
-	}{
-		"a relative retarget":             {"see [the guide](docs/old.md).", "see [the guide](docs/new.md).", true},
-		"a retarget within the host":      {"[a](https://example.com/old)", "[a](https://example.com/new)", true},
-		"a retarget to the archive":       {"[a](https://example.com/gone)", "[a](https://web.archive.org/web/2020/https://example.com/gone)", true},
-		"a definition retarget":           {"[a]: https://example.com/old\n[a] and text", "[a]: https://example.com/new\n[a] and text", true},
-		"a title unchanged, target moved": {`[a](docs/old.md "The Guide")`, `[a](docs/new.md "The Guide")`, true},
-		"relative to absolute":            {"[a](docs/x.md)", "[a](https://example.com/x)", false},
-		"a protocol-relative jump":        {"[a](docs/x.md)", "[a](//evil.com/x)", false},
-		"a fenced retarget":               {"```\n[a](docs/old.md)\n```", "```\n[a](docs/new.md)\n```", false},
-		"an injected script":              {"plain text.", "plain text <script>alert(1)</script>.", false},
-		"a changed fenced command":        {"```\ncurl https://example.com/old\n```", "```\ncurl https://example.com/new\n```", false},
-		"a reworded sentence":             {"The link is broken.", "The link has rotted.", false},
-		"a new external host":             {"[a](https://example.com/x)", "[a](https://evil.example/x)", false},
-		"a javascript target":             {"[a](https://example.com/x)", "[a](javascript:alert(1))", false},
-		"a link added":                    {"text.", "text. [a](new.md)", false},
-		// cloud-native-ref#2517: a code block above the links must not fence everything after it.
-		"a retarget after a block":           {"```\nx\n```\n[a](docs/old.md)", "```\nx\n```\n[a](docs/new.md)", true},
-		"a retarget between two blocks":      {"```\nx\n```\n[a](old.md)\n```\ny\n```", "```\nx\n```\n[a](new.md)\n```\ny\n```", true},
-		"a retarget after two blocks":        {"```\nx\n```\n```\ny\n```\n[a](old.md)", "```\nx\n```\n```\ny\n```\n[a](new.md)", true},
-		"a retarget after a list's block":    {"- step:\n  ```sh\n  ls\n  ```\n[a](old.md)", "- step:\n  ```sh\n  ls\n  ```\n[a](new.md)", true},
-		"a retarget in a list's block":       {"- step:\n  ```\n  [a](old.md)\n  ```\n", "- step:\n  ```\n  [a](new.md)\n  ```\n", false},
-		"a retarget after an unclosed fence": {"```\nx\n[a](old.md)", "```\nx\n[a](new.md)", false},
-	} {
-		t.Run(name, func(t *testing.T) {
-			ok, why := LinksOnly(one(c.base), one(c.headText))
-			if ok != c.ok {
-				t.Fatalf("ok = %v (%s), want %v", ok, why, c.ok)
-			}
-			if ok && why != "" {
-				t.Fatalf("why = %q on a links-only diff", why)
-			}
-		})
-	}
-	t.Run("files", func(t *testing.T) {
-		if ok, _ := LinksOnly(map[string]string{"a.md": "keep", "b.md": "[x](old.md)"},
-			map[string]string{"a.md": "keep", "b.md": "[x](new.md)"}); !ok {
-			t.Fatal("an unchanged file beside a retarget is links-only")
-		}
-		if ok, _ := LinksOnly(map[string]string{"a.md": "x"}, map[string]string{"a.md": "x", "b.md": "[y](z.md)"}); ok {
-			t.Fatal("an added file is not a link retarget")
-		}
-		if ok, _ := LinksOnly(nil, nil); !ok {
-			t.Fatal("no changed file contradicts nothing")
-		}
-	})
-}
-
 func TestDecideArm(t *testing.T) {
 	approved := agentPR()
 	approved.Reviews = []forge.Review{{Author: "Smana", State: "APPROVED"}}
````

- [ ] **Step 2: Run.** `go test ./internal/factory/mdlinks/` — Expected: FAIL, no non-test Go files.

- [ ] **Step 3: Move the code.** `mdlinks.go` is `arm.go`'s link code, `linksOf` renamed `Targets`:

````go
// SPDX-License-Identifier: Apache-2.0

// Package mdlinks reads Markdown link targets the way the merge gate's links-only shape does
// (R52): a link target is an inline one's or a reference definition's, outside a code fence. The
// gate, the dead-links probe and the oracle read targets through it alone, so the three agree.
package mdlinks

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// reLinkTarget matches one link's target: a reference definition's ([label]: target at a line
// start) or an inline one's (](target), angle-bracketed or bare). Titles, labels and every other
// byte stay in the compared text, so a change beside a link is visible. Deliberately not a full
// markdown parser: a diff that hides a change inside what this reads as a link target is refused
// by the target rule below, and the classes it gates stay shadow until the wave re-reads this
// against a month of forecasts (task 7.9, R52).
var reLinkTarget = regexp.MustCompile(`(?m)^ {0,3}\[[^()\]\r\n]+\]:[ \t]*(<[^<>\r\n]*>|\S+)|\]\((<[^<>\r\n]*>|[^\s()<]*)`)

// linkPlaceholder blanks a target so the rest of the text compares byte for byte. NUL: not in
// any file a class like this edits, and counted below if one ever appears.
const linkPlaceholder = "\x00"

// Targets is the text with every link target outside a code fence replaced by the placeholder,
// and those targets in order of appearance. Fenced lines are left unsubstituted on purpose:
// fenced content is code, and a docs-links diff that moved a link inside a fence moved code,
// which the byte compare below then sees.
func Targets(s string) (string, []string) {
	var out []string
	m := reLinkTarget.FindAllStringSubmatchIndex(s, -1)
	if m == nil {
		return s, nil
	}
	fences := fencesOf(s)
	var b strings.Builder
	last := 0
	for _, ix := range m {
		if inFence(fences, ix[0]) {
			continue // the fenced text still gets written: last never jumps over it
		}
		start, end := ix[4], ix[5] // the inline alternative
		if ix[2] >= 0 {
			start, end = ix[2], ix[3] // the reference-definition alternative
		}
		b.WriteString(s[last:start])
		b.WriteString(linkPlaceholder)
		t := s[start:end]
		if len(t) >= 2 && strings.HasPrefix(t, "<") {
			t = t[1 : len(t)-1]
		}
		out = append(out, t)
		last = end
	}
	b.WriteString(s[last:])
	return b.String(), out
}

// fencesOf are the byte ranges of ```-fenced blocks, fence lines included. An unbalanced
// fence runs to the end of the text: its links go uncounted, which fails the byte compare
// for any change there — the safe direction.
func fencesOf(s string) [][2]int {
	var out [][2]int
	in, start := false, 0
	for off := 0; off < len(s); {
		end := strings.IndexByte(s[off:], '\n')
		if end < 0 {
			end = len(s)
		} else {
			end += off
		}
		if strings.HasPrefix(strings.TrimLeft(s[off:end], " \t"), "```") {
			if in {
				out, in = append(out, [2]int{start, end}), false
			} else {
				start, in = off, true
			}
		}
		if end == len(s) {
			break
		}
		off = end + 1
	}
	if in {
		out = append(out, [2]int{start, len(s)})
	}
	return out
}

func inFence(ranges [][2]int, off int) bool {
	return slices.ContainsFunc(ranges, func(r [2]int) bool { return r[0] <= off && off < r[1] })
}

// targetKind is a link target's scheme and host, with ("", "") only for a target relative to
// the current document. A schemeless one with a host is protocol-relative — host-absolute, and
// its non-empty host keeps it out of the relative clause. An unparseable one gets a scheme of
// its own so no pair can match it.
func targetKind(t string) (scheme, host string) {
	u, err := url.Parse(t)
	if err != nil {
		return "!", ""
	}
	if u.Scheme == "" && u.Host == "" {
		return "", "" // a path, a query, a fragment: relative
	}
	return strings.ToLower(u.Scheme), strings.ToLower(u.Host)
}

// retargetOK is R52's rule for a changed pair: relative to relative — hosts empty on both sides,
// so a protocol-relative //host is never "relative" — or https staying on its host, or landing
// on the archive. Never another scheme, never a host jump.
func retargetOK(from, to string) bool {
	fs, fh := targetKind(from)
	ts, th := targetKind(to)
	switch {
	case fs == "" && fh == "" && ts == "" && th == "":
		return true
	case fs == "https" && ts == "https" && (fh == th || th == "web.archive.org"):
		return true
	default:
		return false
	}
}

// LinksOnly is R52's promise of the docs-links and revert classes: between two commits of the
// tree, every changed file changed only its link targets, the counts are equal, and every
// retarget moves relatively, keeps its host, or lands on web.archive.org. The bool decides;
// the string is the first violation, named for the shadow forecast's record (task 7.9).
func LinksOnly(base, head map[string]string) (bool, string) {
	paths := make([]string, 0, len(base)+len(head))
	for p := range base {
		paths = append(paths, p)
	}
	for p := range head {
		if _, ok := base[p]; !ok {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	for _, p := range paths {
		b, h := base[p], head[p]
		if b == h {
			continue
		}
		nb, tb := Targets(b)
		nh, th := Targets(h)
		if nb != nh {
			return false, p + ": changed beyond link targets"
		}
		if len(tb) != len(th) { // redundant with nb == nh unless the text holds a real NUL
			return false, p + ": added or removed a link"
		}
		for i := range tb {
			if tb[i] != th[i] && !retargetOK(tb[i], th[i]) {
				return false, p + ": retargeted " + tb[i] + " to " + th[i]
			}
		}
	}
	return true, ""
}
````

  `arm.go` loses it (and its `net/url`, `regexp`, `strings` imports):

````diff
diff --git a/internal/factory/reconciler/arm.go b/internal/factory/reconciler/arm.go
index f8d3451..768f1b0 100644
--- a/internal/factory/reconciler/arm.go
+++ b/internal/factory/reconciler/arm.go
@@ -3,10 +3,7 @@
 package reconciler
 
 import (
-	"net/url"
-	"regexp"
 	"slices"
-	"strings"
 
 	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
 	"github.com/Smana/agent-platform/internal/factory/config"
@@ -193,152 +190,3 @@ func agentEvidence(in ArmInputs) string {
 	}
 	return ""
 }
-
-// reLinkTarget matches one link's target: a reference definition's ([label]: target at a line
-// start) or an inline one's (](target), angle-bracketed or bare). Titles, labels and every other
-// byte stay in the compared text, so a change beside a link is visible. Deliberately not a full
-// markdown parser: a diff that hides a change inside what this reads as a link target is refused
-// by the target rule below, and the classes it gates stay shadow until the wave re-reads this
-// against a month of forecasts (task 7.9, R52).
-var reLinkTarget = regexp.MustCompile(`(?m)^ {0,3}\[[^()\]\r\n]+\]:[ \t]*(<[^<>\r\n]*>|\S+)|\]\((<[^<>\r\n]*>|[^\s()<]*)`)
-
-// linkPlaceholder blanks a target so the rest of the text compares byte for byte. NUL: not in
-// any file a class like this edits, and counted below if one ever appears.
-const linkPlaceholder = "\x00"
-
-// linksOf is the text with every link target outside a code fence replaced by the placeholder,
-// and those targets in order of appearance. Fenced lines are left unsubstituted on purpose:
-// fenced content is code, and a docs-links diff that moved a link inside a fence moved code,
-// which the byte compare below then sees.
-func linksOf(s string) (string, []string) {
-	var out []string
-	m := reLinkTarget.FindAllStringSubmatchIndex(s, -1)
-	if m == nil {
-		return s, nil
-	}
-	fences := fencesOf(s)
-	var b strings.Builder
-	last := 0
-	for _, ix := range m {
-		if inFence(fences, ix[0]) {
-			continue // the fenced text still gets written: last never jumps over it
-		}
-		start, end := ix[4], ix[5] // the inline alternative
-		if ix[2] >= 0 {
-			start, end = ix[2], ix[3] // the reference-definition alternative
-		}
-		b.WriteString(s[last:start])
-		b.WriteString(linkPlaceholder)
-		t := s[start:end]
-		if len(t) >= 2 && strings.HasPrefix(t, "<") {
-			t = t[1 : len(t)-1]
-		}
-		out = append(out, t)
-		last = end
-	}
-	b.WriteString(s[last:])
-	return b.String(), out
-}
-
-// fencesOf are the byte ranges of ```-fenced blocks, fence lines included. An unbalanced
-// fence runs to the end of the text: its links go uncounted, which fails the byte compare
-// for any change there — the safe direction.
-func fencesOf(s string) [][2]int {
-	var out [][2]int
-	in, start := false, 0
-	for off := 0; off < len(s); {
-		end := strings.IndexByte(s[off:], '\n')
-		if end < 0 {
-			end = len(s)
-		} else {
-			end += off
-		}
-		if strings.HasPrefix(strings.TrimLeft(s[off:end], " \t"), "```") {
-			if in {
-				out, in = append(out, [2]int{start, end}), false
-			} else {
-				start, in = off, true
-			}
-		}
-		if end == len(s) {
-			break
-		}
-		off = end + 1
-	}
-	if in {
-		out = append(out, [2]int{start, len(s)})
-	}
-	return out
-}
-
-func inFence(ranges [][2]int, off int) bool {
-	return slices.ContainsFunc(ranges, func(r [2]int) bool { return r[0] <= off && off < r[1] })
-}
-
-// targetKind is a link target's scheme and host, with ("", "") only for a target relative to
-// the current document. A schemeless one with a host is protocol-relative — host-absolute, and
-// its non-empty host keeps it out of the relative clause. An unparseable one gets a scheme of
-// its own so no pair can match it.
-func targetKind(t string) (scheme, host string) {
-	u, err := url.Parse(t)
-	if err != nil {
-		return "!", ""
-	}
-	if u.Scheme == "" && u.Host == "" {
-		return "", "" // a path, a query, a fragment: relative
-	}
-	return strings.ToLower(u.Scheme), strings.ToLower(u.Host)
-}
-
-// retargetOK is R52's rule for a changed pair: relative to relative — hosts empty on both sides,
-// so a protocol-relative //host is never "relative" — or https staying on its host, or landing
-// on the archive. Never another scheme, never a host jump.
-func retargetOK(from, to string) bool {
-	fs, fh := targetKind(from)
-	ts, th := targetKind(to)
-	switch {
-	case fs == "" && fh == "" && ts == "" && th == "":
-		return true
-	case fs == "https" && ts == "https" && (fh == th || th == "web.archive.org"):
-		return true
-	default:
-		return false
-	}
-}
-
-// LinksOnly is R52's promise of the docs-links and revert classes: between two commits of the
-// tree, every changed file changed only its link targets, the counts are equal, and every
-// retarget moves relatively, keeps its host, or lands on web.archive.org. The bool decides;
-// the string is the first violation, named for the shadow forecast's record (task 7.9).
-func LinksOnly(base, head map[string]string) (bool, string) {
-	paths := make([]string, 0, len(base)+len(head))
-	for p := range base {
-		paths = append(paths, p)
-	}
-	for p := range head {
-		if _, ok := base[p]; !ok {
-			paths = append(paths, p)
-		}
-	}
-	slices.Sort(paths)
-	for _, p := range paths {
-		b, h := base[p], head[p]
-		if b == h {
-			continue
-		}
-		nb, tb := linksOf(b)
-		nh, th := linksOf(h)
-		if nb != nh {
-			return false, p + ": changed beyond link targets"
-		}
-		if len(tb) != len(th) { // redundant with nb == nh unless the text holds a real NUL
-			return false, p + ": added or removed a link"
-		}
-		for i := range tb {
-			if tb[i] != th[i] && !retargetOK(tb[i], th[i]) {
-				return false, p + ": retargeted " + tb[i] + " to " + th[i]
-			}
-		}
-	}
-	return true, ""
-}
````

  `gate.go`'s `diffShape` calls `mdlinks.LinksOnly` (part of Task 6's diff below; apply that line now).

- [ ] **Step 4: Run.** `go test ./internal/factory/mdlinks/ ./internal/factory/reconciler/` — Expected: `ok` twice.

- [ ] **Step 5: Commit.** `git commit -m "refactor(factory): move R52's link reading into mdlinks"` (the two packages' files).

### Task 6: The findings on the Task, the `oracle` key and condition

**Files:**
- Modify: `api/factory/v1alpha1/task_types.go` (then `task crd:gen`: `zz_generated.deepcopy.go`, `config/crd/agents.ogenki.io_tasks.yaml`, `charts/agent-factory/crds/agents.ogenki.io_tasks.yaml`), `internal/factory/config/classes.go`, `internal/factory/reconciler/gate.go`
- Test: `internal/factory/reconciler/oracle_test.go`, `internal/factory/config/classes_test.go`

**Interfaces:**
- Produces: `v1alpha1.TaskSpec.Findings []v1alpha1.Finding`, `v1alpha1.Finding{File, URL, Archive string}`; `config.Class.Oracle *config.Oracle`, `config.Oracle{Probe, Workflow string; MaxFindings int}`, `config.MaxFindings = 16`.

- [ ] **Step 1: Write the failing tests** (offline proof 9). `oracle_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

const (
	gone    = "https://example.com/gone"
	archive = "https://web.archive.org/web/20261011195309/https://example.com/gone"
	page    = "[a](https://example.com/gone) and [b](https://other.example/kept)."
)

// oracleInputs: a dead-links task of one finding, fixed on the head by a retarget to its archive.
func oracleInputs(headText string) ArmInputs {
	in := gateInputs()
	cl := declaredClass()
	cl.Oracle = &config.Oracle{Probe: "dead-links", Workflow: "dead-links.yaml", MaxFindings: 10}
	in.Cfg.Classes["dead-links"] = cl
	in.Task.Spec.Findings = []v1alpha1.Finding{{File: testFile, URL: gone, Archive: archive}}
	in.Files = changed(page, headText)
	in.Stats = []forge.FileStat{{Path: testFile, Status: "modified", Additions: 1, Deletions: 1}}
	return in
}

// Offline proof 9: the oracle condition on fixtures, each with its reason.
func TestTheOracle(t *testing.T) {
	for name, c := range map[string]struct {
		head, reason string
	}{
		"to the snapshot given":     {"[a](" + archive + ") and [b](https://other.example/kept).", ""},
		"within the host":           {"[a](https://example.com/moved) and [b](https://other.example/kept).", ""},
		"the finding left":          {page, "oracle_finding_open"},
		"another target moved":      {"[a](" + archive + ") and [b](https://other.example/new).", "oracle_unrequested"},
		"another snapshot":          {"[a](https://web.archive.org/web/2020/https://example.com/gone) and [b](https://other.example/kept).", "oracle_unrequested"},
		"the finding's file unread": {"", "oracle_finding_open"},
	} {
		t.Run(name, func(t *testing.T) {
			in := oracleInputs(c.head)
			if c.head == "" {
				in.Files = changed("x", "x")
				in.Files.Base, in.Files.Head = map[string]string{"docs/other.md": "x"}, map[string]string{"docs/other.md": "x"}
			}
			d := DecideArm(in)
			if (c.reason == "" && d.Verdict != "arm") || (c.reason != "" && d.Reason != c.reason) {
				t.Fatalf("got %+v, want %q", d, c.reason)
			}
		})
	}
	in := oracleInputs(page)
	in.Task.Spec.Findings = nil
	if d := DecideArm(in); d.Reason != "oracle_finding_open" {
		t.Fatalf("an oracle task without findings proves nothing: %+v", d)
	}
}
```

  and in `classes_test.go`'s `TestBadConditionsFail` table (offline proof 3, its oracle half):

```go
		"an oracle without a holdout": {"docs-links: {}\n  revert: {}\n  x: {diffShape: links-only, oracle: {probe: dead-links, workflow: dead-links.yaml, maxFindings: 10}}",
			"class x: an oracle needs holdoutChecks, or the head goes unproven (MC5)"},
		"an oracle off links-only": {"docs-links: {}\n  revert: {}\n  x: {holdoutChecks: [h], oracle: {probe: dead-links, workflow: dead-links.yaml, maxFindings: 10}}",
			"class x: an oracle reads link targets: diffShape must be links-only"},
		"too many findings": {"docs-links: {}\n  revert: {}\n  x: {diffShape: links-only, holdoutChecks: [h], oracle: {probe: dead-links, workflow: dead-links.yaml, maxFindings: 17}}",
			"class x: oracle.maxFindings must be 1..16"},
		"an unknown probe": {"docs-links: {}\n  revert: {}\n  x: {diffShape: links-only, holdoutChecks: [h], oracle: {probe: trivy, workflow: t.yaml, maxFindings: 1}}",
			`class x: oracle.probe "trivy" is not dead-links`},
```

- [ ] **Step 2: Run.** `go test ./internal/factory/config/ ./internal/factory/reconciler/ -run 'TestBadConditionsFail|TestTheOracle'` — Expected: build failure, `unknown field Findings` / `undefined: config.Oracle`.

- [ ] **Step 3: Implement.** `task_types.go`:

```diff
diff --git a/api/factory/v1alpha1/task_types.go b/api/factory/v1alpha1/task_types.go
index c33af6d..3a57a78 100644
--- a/api/factory/v1alpha1/task_types.go
+++ b/api/factory/v1alpha1/task_types.go
@@ -78,6 +78,28 @@ type TaskSpec struct {
 	Template string `json:"template,omitempty"`
 	// +optional
 	Budget Budget `json:"budget,omitempty"`
+	// The findings an oracle class's probe made this task of (merge gate classes MC6): what the
+	// run is asked to fix, and what the gate's oracle checks the head no longer holds.
+	// +kubebuilder:validation:MaxItems=16
+	// +optional
+	Findings []Finding `json:"findings,omitempty"`
+}
+
+// Finding is one dead external link two consecutive detector runs on main reported (MC5).
+type Finding struct {
+	// The Markdown file on main that holds the link.
+	// +kubebuilder:validation:MinLength=1
+	// +kubebuilder:validation:MaxLength=512
+	File string `json:"file"`
+	// The link target, an https URL.
+	// +kubebuilder:validation:MinLength=1
+	// +kubebuilder:validation:MaxLength=2048
+	URL string `json:"url"`
+	// The Wayback Machine's closest snapshot of URL, an https://web.archive.org/web/ URL; empty
+	// when it has none.
+	// +kubebuilder:validation:MaxLength=2048
+	// +optional
+	Archive string `json:"archive,omitempty"`
 }
 
 // Source is where the task came from and how far its text is trusted.
```

  Run `task crd:gen` and keep what it writes; run `gofmt -w internal/factory` after editing the tests. `classes.go`:

```diff
diff --git a/internal/factory/config/classes.go b/internal/factory/config/classes.go
index bb7e316..2727669 100644
--- a/internal/factory/config/classes.go
+++ b/internal/factory/config/classes.go
@@ -34,8 +34,24 @@ type Class struct {
 	HoldoutChecks []string `json:"holdoutChecks,omitempty"`
 	// MaxPerDay caps this class's merges a day, under merge.autoMergesPerDay; 0 is no cap of its own.
 	MaxPerDay int `json:"maxPerDay,omitempty"`
+	// Oracle makes the class oracle-backed (MC4–MC6): its tasks are a detector's findings, and its
+	// gate holds only when the head no longer holds them and changes nothing else.
+	Oracle *Oracle `json:"oracle,omitempty"`
 }
 
+// Oracle is an oracle-backed class's detector: the probe that turns its findings into a task,
+// the workflow on main whose runs report them, and the most findings one task holds.
+type Oracle struct {
+	Probe       string `json:"probe"`
+	Workflow    string `json:"workflow"`
+	MaxFindings int    `json:"maxFindings"`
+}
+
+// MaxFindings bounds oracle.maxFindings: the Task CRD's spec.findings holds at most 16.
+const MaxFindings = 16
+
+var workflowRE = regexp.MustCompile(`^[A-Za-z0-9._-]+\.ya?ml$`)
+
 // DiffShapes are the shapes diffShape names.
 func DiffShapes() []string { return []string{"links-only"} }
 
@@ -52,7 +68,7 @@ func BuiltInShape(name string) string {
 // or any declared condition on its files.
 func (c Class) ReadsDiff(name string) bool {
 	return BuiltInShape(name) != "" || len(c.AllowedFiles) > 0 || len(c.ProtectedFiles) > 0 || c.ModifyOnly ||
-		c.MaxPatchFiles > 0 || c.MaxModifiedLines > 0 || c.DiffShape != ""
+		c.MaxPatchFiles > 0 || c.MaxModifiedLines > 0 || c.DiffShape != "" || c.Oracle != nil
 }
 
 // validateClass reports one class's problems.
@@ -84,6 +100,20 @@ func validateClass(name string, cl Class, bad func(string, ...any)) {
 	if b := BuiltInShape(name); b != "" && cl.DiffShape != "" && cl.DiffShape != b {
 		bad("class %s: diffShape is %s, built in (R52)", name, b)
 	}
+	if o := cl.Oracle; o != nil {
+		switch {
+		case o.Probe != "dead-links":
+			bad("class %s: oracle.probe %q is not dead-links", name, o.Probe)
+		case !workflowRE.MatchString(o.Workflow):
+			bad("class %s: oracle.workflow %q is not a workflow file name", name, o.Workflow)
+		case o.MaxFindings < 1 || o.MaxFindings > MaxFindings:
+			bad("class %s: oracle.maxFindings must be 1..%d", name, MaxFindings)
+		case len(cl.HoldoutChecks) == 0:
+			bad("class %s: an oracle needs holdoutChecks, or the head goes unproven (MC5)", name)
+		case cl.DiffShape != "links-only":
+			bad("class %s: an oracle reads link targets: diffShape must be links-only", name)
+		}
+	}
 	seen := map[string]bool{}
 	for _, h := range cl.HoldoutChecks {
 		if h == "" || seen[h] {
```

  `gate.go` (`mdlinks` for `diffShape`; the `oracle` condition):

```diff
diff --git a/internal/factory/reconciler/gate.go b/internal/factory/reconciler/gate.go
index 59bb45d..8b8804d 100644
--- a/internal/factory/reconciler/gate.go
+++ b/internal/factory/reconciler/gate.go
@@ -3,11 +3,17 @@
 package reconciler
 
 import (
+	"maps"
+	"net/url"
 	"regexp"
 	"slices"
+	"strings"
+
+	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
 
 	"github.com/Smana/agent-platform/internal/factory/config"
 	"github.com/Smana/agent-platform/internal/factory/forge"
+	"github.com/Smana/agent-platform/internal/factory/mdlinks"
 )
 
 // Condition is one condition a class declares in its config (merge gate classes MC1): Declared
@@ -30,6 +36,7 @@ func Conditions() []Condition {
 		{"maxModifiedLines", func(cl config.Class) bool { return cl.MaxModifiedLines > 0 }, maxModifiedLines},
 		{"diffShape", func(cl config.Class) bool { return cl.DiffShape != "" }, diffShape},
 		{"holdoutChecks", func(cl config.Class) bool { return len(cl.HoldoutChecks) > 0 }, holdoutChecks},
+		{"oracle", func(cl config.Class) bool { return cl.Oracle != nil }, oracle},
 	}
 }
 
@@ -99,7 +106,7 @@ func maxModifiedLines(cl config.Class, in ArmInputs) (string, bool) {
 // yet passes it vacuously (LinksOnly of two empty trees).
 func diffShape(cl config.Class, in ArmInputs) (string, bool) {
 	if cl.DiffShape == "links-only" {
-		if ok, _ := LinksOnly(in.Files.Base, in.Files.Head); !ok {
+		if ok, _ := mdlinks.LinksOnly(in.Files.Base, in.Files.Head); !ok {
 			return "class_mismatch", false
 		}
 	}
@@ -125,8 +132,50 @@ func holdoutChecks(cl config.Class, in ArmInputs) (string, bool) {
 	return "", false
 }
 
+// oracle (MC5, MC6): the head no longer holds any of the task's findings — each finding's file
+// changed, and its URL is no longer a link target there — and every target it moved is a
+// finding's, moved to the same host or to the snapshot the finding gave. diffShape links-only
+// (Validate) has already held the rest of the text and the targets' count.
+func oracle(_ config.Class, in ArmInputs) (string, bool) {
+	fs := in.Task.Spec.Findings
+	if len(fs) == 0 {
+		return "oracle_finding_open", false // a task of an oracle class is its findings: none is no proof
+	}
+	for _, f := range fs {
+		h, ok := in.Files.Head[f.File]
+		if _, ts := mdlinks.Targets(h); !ok || slices.Contains(ts, f.URL) {
+			return "oracle_finding_open", false
+		}
+	}
+	for _, p := range slices.Sorted(maps.Keys(in.Files.Head)) {
+		_, tb := mdlinks.Targets(in.Files.Base[p])
+		_, th := mdlinks.Targets(in.Files.Head[p])
+		if len(tb) != len(th) {
+			return "oracle_unrequested", false
+		}
+		for i := range tb {
+			if tb[i] == th[i] {
+				continue
+			}
+			j := slices.IndexFunc(fs, func(f v1alpha1.Finding) bool { return f.File == p && f.URL == tb[i] })
+			if j < 0 || (th[i] != fs[j].Archive && !sameHost(tb[i], th[i])) {
+				return "oracle_unrequested", false
+			}
+		}
+	}
+	return "", false
+}
+
+// sameHost: both are https URLs on one host.
+func sameHost(a, b string) bool {
+	ua, ea := url.Parse(a)
+	ub, eb := url.Parse(b)
+	return ea == nil && eb == nil && ua.Scheme == "https" && ub.Scheme == "https" && ua.Host != "" &&
+		strings.EqualFold(ua.Host, ub.Host)
+}
+
 // onDiff are the conditions that read the diff, which hold until it is read.
-var onDiff = []string{"allowedFiles", "protectedFiles", "modifyOnly", "maxPatchFiles", "maxModifiedLines"}
+var onDiff = []string{"allowedFiles", "protectedFiles", "modifyOnly", "maxPatchFiles", "maxModifiedLines", "oracle"}
 
 // declared checks the class's declared conditions in order: the first refusal, or "".
 func declared(cl config.Class, in ArmInputs) (string, bool) {
```

- [ ] **Step 4: Run.** `go test ./internal/factory/... ./api/... && task crd:check` — Expected: `ok` everywhere; `crd:check` passes once the generated files are committed.

- [ ] **Step 5: Commit.** `git commit -m "feat(factory): an oracle class's findings and its gate condition"` (the files above and the generated ones).

### Task 7: The detector's runs and artifacts, through the merger

**Files:**
- Create: `internal/factory/forge/actions.go`, `internal/factory/forge/actions_test.go`
- Modify: `internal/factory/forge/forge.go`, `internal/factory/forge/github.go`, `internal/factory/forge/absent.go`, `internal/factory/forge/forgetest/fake.go`

**Interfaces:**
- Produces: `forge.WorkflowRun{ID int64; Event, Branch, Actor, Conclusion string; Created time.Time}`; on `Merger`: `WorkflowRuns(ctx, workflow string) ([]WorkflowRun, error)` (newest first), `RunArtifact(ctx, runID int64, name, file string) ([]byte, error)`, `FileAt(ctx, path, ref string) (string, error)`; `(*forgetest.Fake).SetRuns([]forge.WorkflowRun, map[int64][]byte)`, `SetMainFiles(map[string]string)`.

- [ ] **Step 1: Write the failing test:**

```go
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"testing"
)

// actionsRig serves two runs of dead-links.yaml, run 7's findings artifact, and the storage
// GitHub redirects its download to: a path off the API, which must never see the token.
func actionsRig(t *testing.T) *rig {
	r := newRig(t)
	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	w, _ := zw.Create("findings.json")
	_, _ = io.WriteString(w, `[{"file":"docs/a.md","line":3,"url":"https://example.com/gone","status":"404"}]`)
	_ = zw.Close()
	mux := r.gh.srv.Config.Handler.(*http.ServeMux)
	mux.HandleFunc("GET /repos/Smana/demo/actions/workflows/dead-links.yaml/runs", r.gh.authed(func(w http.ResponseWriter, req *http.Request) {
		if q := req.URL.Query(); q.Get("branch") != "main" || q.Get("status") != "completed" {
			t.Errorf("runs query %v", q)
		}
		_, _ = io.WriteString(w, `{"total_count":2,"workflow_runs":[
		  {"id":6,"event":"schedule","head_branch":"main","conclusion":"success","created_at":"2026-10-21T04:23:00Z","triggering_actor":{"login":"Smana"}},
		  {"id":7,"event":"workflow_dispatch","head_branch":"main","conclusion":"success","created_at":"2026-10-22T04:23:00Z","triggering_actor":{"login":"Smana"}}]}`)
	}))
	mux.HandleFunc("GET /repos/Smana/demo/actions/runs/7/artifacts", r.gh.authed(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"total_count":2,"artifacts":[{"id":70,"name":"findings","expired":true},{"id":71,"name":"findings","expired":false}]}`)
	}))
	mux.HandleFunc("GET /repos/Smana/demo/actions/artifacts/71/zip", r.gh.authed(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", r.gh.srv.URL+"/storage/71.zip?sig=x")
		w.WriteHeader(http.StatusFound)
	}))
	mux.HandleFunc("GET /storage/71.zip", func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "" {
			t.Error("the installation token reached the artifact storage")
		}
		_, _ = w.Write(zipped.Bytes())
	})
	return r
}

func TestWorkflowRunsNewestFirst(t *testing.T) {
	r := actionsRig(t)
	runs, err := r.g.WorkflowRuns(t.Context(), "dead-links.yaml")
	if err != nil || len(runs) != 2 || runs[0].ID != 7 || runs[0].Event != "workflow_dispatch" || runs[0].Actor != "Smana" ||
		runs[1].Branch != "main" || runs[1].Conclusion != "success" {
		t.Fatalf("%+v %v", runs, err)
	}
}

func TestRunArtifactReadsTheFileFromStorage(t *testing.T) {
	r := actionsRig(t)
	b, err := r.g.RunArtifact(t.Context(), 7, "findings", "findings.json")
	if err != nil || !bytes.Contains(b, []byte(`"status":"404"`)) {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := r.g.RunArtifact(t.Context(), 7, "findings", "other.json"); err == nil {
		t.Error("a file the zip does not hold is an error")
	}
	if _, err := r.g.RunArtifact(t.Context(), 7, "sarif", "x"); err == nil {
		t.Error("an artifact the run does not hold is an error")
	}
}
```

- [ ] **Step 2: Run.** `go test ./internal/factory/forge/ -run 'TestWorkflowRunsNewestFirst|TestRunArtifactReadsTheFileFromStorage'` — Expected: build failure, `r.g.WorkflowRuns undefined`.

- [ ] **Step 3: Implement.** `actions.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/go-github/v92/github"

	"github.com/Smana/agent-platform/internal/httpx"
)

// maxArtifactBytes bounds an artifact's zip and the file read out of it.
const maxArtifactBytes = 4 << 20

// WorkflowRuns are the newest completed runs of workflow, a file under .github/workflows, on
// main: one page of 20, newest first. The dead-links probe reads the newest two a schedule or a
// maintainer started. It needs actions: read, which the merger App holds (runbook 12).
func (g *GitHub) WorkflowRuns(ctx context.Context, workflow string) ([]WorkflowRun, error) {
	res, _, err := g.rest.Actions.ListWorkflowRunsByFileName(ctx, g.owner, g.name, workflow, &github.ListWorkflowRunsOptions{
		Branch: "main", Status: "completed", ExcludePullRequests: true, ListOptions: github.ListOptions{PerPage: 20}})
	g.mark(err)
	if err != nil {
		return nil, wrap("list workflow runs", err)
	}
	out := make([]WorkflowRun, 0, len(res.WorkflowRuns))
	for _, r := range res.WorkflowRuns {
		out = append(out, WorkflowRun{ID: r.GetID(), Event: r.GetEvent(), Branch: r.GetHeadBranch(),
			Actor: r.GetTriggeringActor().GetLogin(), Conclusion: r.GetConclusion(), Created: r.GetCreatedAt().Time})
	}
	slices.SortStableFunc(out, func(a, b WorkflowRun) int { return cmp.Compare(b.Created.UnixNano(), a.Created.UnixNano()) })
	return out, nil
}

// RunArtifact is file inside run runID's unexpired artifact name. GitHub's API answers the
// download with a redirect to its storage (productionresultssa*.blob.core.windows.net); the zip is
// fetched there through the plain egress client, without the installation token, and read whole
// within maxArtifactBytes.
func (g *GitHub) RunArtifact(ctx context.Context, runID int64, name, file string) ([]byte, error) {
	list, _, err := g.rest.Actions.ListWorkflowRunArtifacts(ctx, g.owner, g.name, runID, &github.ListOptions{PerPage: 100})
	g.mark(err)
	if err != nil {
		return nil, wrap("list run artifacts", err)
	}
	i := slices.IndexFunc(list.Artifacts, func(a *github.Artifact) bool { return a.GetName() == name && !a.GetExpired() })
	if i < 0 {
		return nil, fmt.Errorf("forge: run %d has no artifact %q", runID, name)
	}
	u, _, err := g.rest.Actions.DownloadArtifact(ctx, g.owner, g.name, list.Artifacts[i].GetID(), 0)
	g.mark(err)
	if err != nil {
		return nil, wrap("download an artifact", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("forge: artifact %q: %w", name, err)
	}
	resp, err := g.plain.Do(req)
	if err != nil {
		return nil, fmt.Errorf("forge: fetch artifact %q: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("forge: fetch artifact %q: %s", name, resp.Status)
	}
	zipped, err := httpx.ReadBody(resp.Body, maxArtifactBytes)
	if err != nil {
		return nil, fmt.Errorf("forge: fetch artifact %q: %w", name, err)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		return nil, fmt.Errorf("forge: artifact %q is not a zip: %w", name, err)
	}
	i = slices.IndexFunc(zr.File, func(f *zip.File) bool { return f.Name == file })
	if i < 0 {
		return nil, fmt.Errorf("forge: artifact %q holds no %s", name, file)
	}
	rc, err := zr.File[i].Open()
	if err != nil {
		return nil, fmt.Errorf("forge: artifact %q: %w", name, err)
	}
	defer func() { _ = rc.Close() }()
	return httpx.ReadBody(rc, maxArtifactBytes)
}

// FileAt is path's content at ref, "" when absent there: the dead-links probe reads a finding's
// file on main.
func (g *GitHub) FileAt(ctx context.Context, path, ref string) (string, error) {
	return g.fileAt(ctx, path, ref)
}
```

  `forge.go`:

```diff
diff --git a/internal/factory/forge/forge.go b/internal/factory/forge/forge.go
index eef54cb..bb51a77 100644
--- a/internal/factory/forge/forge.go
+++ b/internal/factory/forge/forge.go
@@ -228,6 +228,17 @@ func (c Checks) StatusOf(context string) (state, creator string) {
 	return "", ""
 }
 
+// WorkflowRun is one completed run of a workflow, as the dead-links probe reads it: what
+// triggered it, on which branch, by whom, and how it ended.
+type WorkflowRun struct {
+	ID         int64
+	Event      string // schedule | workflow_dispatch | pull_request | push | …
+	Branch     string // the run's head branch
+	Actor      string // the login that triggered it
+	Conclusion string // success | failure | cancelled | …
+	Created    time.Time
+}
+
 // Revert is the pull request RevertPR opened.
 type Revert struct {
 	Number int
@@ -278,6 +289,11 @@ type Merger interface {
 	// permission for a pull request's labels.
 	AddLabels(ctx context.Context, number int, labels ...string) error
 	PullsWithLabel(ctx context.Context, label string) ([]LabelledPull, error)
+	// WorkflowRuns, RunArtifact and FileAt are the dead-links probe's reads (merge gate MC5):
+	// the detector's runs on main, their findings, and a finding's file there.
+	WorkflowRuns(ctx context.Context, workflow string) ([]WorkflowRun, error)
+	RunArtifact(ctx context.Context, runID int64, name, file string) ([]byte, error)
+	FileAt(ctx context.Context, path, ref string) (string, error)
 }
 
 var _ Merger = (*GitHub)(nil)
```

  `github.go` (the token-free client):

```diff
diff --git a/internal/factory/forge/github.go b/internal/factory/forge/github.go
index 1948b95..9f2bb3a 100644
--- a/internal/factory/forge/github.go
+++ b/internal/factory/forge/github.go
@@ -46,6 +46,7 @@ type GitHub struct {
 	v4          *githubv4.Client
 	tokens      *tokens
 	api         *url.URL
+	plain       *http.Client // o.HTTP, never carrying the token: artifact downloads off the API host
 	agent       string
 	owner, name string
 	now         func() time.Time
@@ -73,7 +74,7 @@ func Connect(ctx context.Context, o Options) (*GitHub, error) {
 	if !strings.HasSuffix(api.Path, "/") {
 		api.Path += "/"
 	}
-	g := &GitHub{api: api, agent: o.UserAgent, owner: owner, name: name, now: o.Now,
+	g := &GitHub{api: api, plain: o.HTTP, agent: o.UserAgent, owner: owner, name: name, now: o.Now,
 		tokens: &tokens{api: api, hc: o.HTTP, owner: owner, name: name, agent: o.UserAgent,
 			idFile: o.AppIDFile, keyFile: o.PrivateKeyFile, now: o.Now, perms: o.Permissions}}
 	hc := g.authed(o.HTTP)
@@ -840,7 +841,7 @@ func (g *GitHub) OpenPullRequests(ctx context.Context) ([]PRSummary, error) {
 	return out, nil
 }
 
-// Files are the contents of every file head changes since it forked from base, read at their
+// Diff is the contents of every file head changes since it forked from base, read at their
 // merge base and at head — what a squash merge applies, not head against base's tip: an added
 // file's base side and a deleted file's head side read "", so the maps' union is the changed
 // set. A rename reads as its delete beside its add — which is right: a rename is not a link
```

  `absent.go`:

```diff
diff --git a/internal/factory/forge/absent.go b/internal/factory/forge/absent.go
index bb8a937..66c4ee9 100644
--- a/internal/factory/forge/absent.go
+++ b/internal/factory/forge/absent.go
@@ -26,6 +26,17 @@ func (Absent) CommitChecks(context.Context, string) ([]Check, error) { return ni
 // RerunCheck implements Merger.
 func (Absent) RerunCheck(context.Context, int64) error { return ErrNoMerger }
 
+// WorkflowRuns implements the Merger's WorkflowRuns and fails closed.
+func (Absent) WorkflowRuns(context.Context, string) ([]WorkflowRun, error) { return nil, ErrNoMerger }
+
+// RunArtifact implements the Merger's RunArtifact and fails closed.
+func (Absent) RunArtifact(context.Context, int64, string, string) ([]byte, error) {
+	return nil, ErrNoMerger
+}
+
+// FileAt implements the Merger's FileAt and fails closed.
+func (Absent) FileAt(context.Context, string, string) (string, error) { return "", ErrNoMerger }
+
 // Diff implements the Merger's Diff and fails closed.
 func (Absent) Diff(context.Context, string, string) (Diff, error) { return Diff{}, ErrNoMerger }
 
```

  `forgetest/fake.go`:

```diff
diff --git a/internal/factory/forge/forgetest/fake.go b/internal/factory/forge/forgetest/fake.go
index 98a491f..0212ea2 100644
--- a/internal/factory/forge/forgetest/fake.go
+++ b/internal/factory/forge/forgetest/fake.go
@@ -45,6 +45,9 @@ type Fake struct {
 	filesBase    map[string]string
 	filesHead    map[string]string
 	fileStats    []forge.FileStat
+	runs         []forge.WorkflowRun
+	artifacts    map[int64][]byte
+	mainFiles    map[string]string
 	open         []forge.PRSummary
 	created      []string
 	armed        []string
@@ -395,6 +398,45 @@ func (f *Fake) SetFiles(base, head map[string]string) {
 	f.filesBase, f.filesHead = maps.Clone(base), maps.Clone(head)
 }
 
+// SetRuns sets the workflow runs WorkflowRuns returns, and each run's findings artifact.
+func (f *Fake) SetRuns(runs []forge.WorkflowRun, artifacts map[int64][]byte) {
+	f.mu.Lock()
+	defer f.mu.Unlock()
+	f.runs, f.artifacts = slices.Clone(runs), maps.Clone(artifacts)
+}
+
+// SetMainFiles sets the files FileAt reads, at any ref.
+func (f *Fake) SetMainFiles(files map[string]string) {
+	f.mu.Lock()
+	defer f.mu.Unlock()
+	f.mainFiles = maps.Clone(files)
+}
+
+// WorkflowRuns implements the Merger's WorkflowRuns.
+func (f *Fake) WorkflowRuns(context.Context, string) ([]forge.WorkflowRun, error) {
+	f.mu.Lock()
+	defer f.mu.Unlock()
+	return slices.Clone(f.runs), nil
+}
+
+// RunArtifact implements the Merger's RunArtifact: the run's artifact as SetRuns gave it.
+func (f *Fake) RunArtifact(_ context.Context, id int64, name, _ string) ([]byte, error) {
+	f.mu.Lock()
+	defer f.mu.Unlock()
+	b, ok := f.artifacts[id]
+	if !ok {
+		return nil, fmt.Errorf("forgetest: run %d has no artifact %q", id, name)
+	}
+	return b, nil
+}
+
+// FileAt implements the Merger's FileAt.
+func (f *Fake) FileAt(_ context.Context, path, _ string) (string, error) {
+	f.mu.Lock()
+	defer f.mu.Unlock()
+	return f.mainFiles[path], nil
+}
+
 // SetFileStats sets the changed-file list Diff returns in place of the one SetFiles implies.
 func (f *Fake) SetFileStats(fs ...forge.FileStat) {
 	f.mu.Lock()
```

- [ ] **Step 4: Run.** `go test ./internal/factory/forge/...` — Expected: `ok`.

- [ ] **Step 5: Commit.** `git commit -m "feat(forge): read a workflow's runs and artifacts through the merger"`.

### Task 8: The probe, its schedule and its triage

**Files:**
- Create: `internal/factory/intake/deadlinks.go`, `internal/factory/intake/deadlinks_test.go`, `internal/factory/intake/deadlinks_schedule_test.go`, `internal/factory/intake/testdata/dead-links-task.golden`
- Modify: `internal/factory/intake/schedule.go`, `internal/factory/config/config.go`, `internal/factory/triage/classify.go`, `internal/factory/triage/classify_test.go`

**Interfaces:**
- Consumes: Task 5's `mdlinks.Targets`; Task 6's `v1alpha1.Finding`, `config.Oracle`; Task 7's `Merger` reads.
- Produces: `intake.LinkFinding{File string; Line int; URL, Status, Archive string}`, `intake.ParseFindings([]byte) ([]LinkFinding, error)`, `intake.Agreed(newest, previous []LinkFinding) []LinkFinding`, `intake.DeadLinksMerger`; `ScheduleMerger` embeds `DeadLinksMerger`; schedules accept `probe: dead-links`.

- [ ] **Step 1: Write the failing tests** (offline proof 8). `deadlinks_test.go`:

````go
// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

type linksMerger struct {
	runs      []forge.WorkflowRun
	artifacts map[int64][]LinkFinding
	files     map[string]string
}

func (m *linksMerger) WorkflowRuns(context.Context, string) ([]forge.WorkflowRun, error) {
	return m.runs, nil
}

func (m *linksMerger) RunArtifact(_ context.Context, id int64, _, _ string) ([]byte, error) {
	fs, ok := m.artifacts[id]
	if !ok {
		return nil, fmt.Errorf("run %d has no artifact", id)
	}
	return json.Marshal(fs)
}

func (m *linksMerger) FileAt(_ context.Context, p, _ string) (string, error) { return m.files[p], nil }

func linksClass() config.Class {
	return config.Class{AllowedFiles: []string{`^(docs|website/content)/.+\.md$`},
		ProtectedFiles: []string{`^docs/(superpowers|specs)/`, `^website/content/docs/decisions/`},
		Oracle:         &config.Oracle{Probe: "dead-links", Workflow: "dead-links.yaml", MaxFindings: 10}}
}

const goneURL = "https://example.com/gone"

// Offline proof 8: a finding is kept only when both runs report it, the class may change its
// file, and it is a link target there on main.
func TestTheDeadLinksProbe(t *testing.T) {
	both := []LinkFinding{
		{File: "docs/a.md", Line: 3, URL: goneURL, Status: "404", Archive: "https://web.archive.org/web/20261011195309/" + goneURL},
		{File: "docs/specs/x.md", Line: 1, URL: goneURL, Status: "404"},                 // protected
		{File: "docs/a.md", Line: 9, URL: "https://example.com/prose", Status: "410"},   // prose, not a link target
		{File: "docs/a.md", Line: 4, URL: "https://example.com/fenced", Status: "404"},  // inside a fence
		{File: "docs/a.md", Line: 5, URL: "https://example.com/limited", Status: "429"}, // never a finding
		{File: "docs/a.md", Line: 6, URL: "http://example.com/plain", Status: "404"},    // R52 cannot retarget http
		{File: "../etc/passwd", Line: 1, URL: goneURL, Status: "404"},                   // not a clean path
	}
	once := LinkFinding{File: "docs/a.md", Line: 7, URL: "https://example.com/once", Status: "404"}
	m := &linksMerger{
		runs: []forge.WorkflowRun{
			{ID: 30, Event: "pull_request", Branch: "agent/abcdefgh", Conclusion: "success"}, // a pull request's run
			{ID: 20, Event: "schedule", Branch: "main", Conclusion: "success"},
			{ID: 15, Event: "workflow_dispatch", Branch: "main", Actor: "stranger", Conclusion: "success"},
			{ID: 10, Event: "workflow_dispatch", Branch: "main", Actor: "Smana", Conclusion: "success"},
		},
		artifacts: map[int64][]LinkFinding{20: append(both, once), 10: both, 30: {once}, 15: {once}},
		files: map[string]string{"docs/a.md": "[a](" + goneURL + ") and [b](https://example.com/once).\n" +
			"See https://example.com/prose.\n```\n[c](https://example.com/fenced)\n```\n[d](https://example.com/limited) [e](http://example.com/plain)\n"},
	}
	rc := &config.Repository{Name: "Smana/cloud-native-ref", Maintainers: []string{"Smana"}}
	got, err := deadLinks(t.Context(), m, rc, linksClass())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].File != "docs/a.md" || got[0].URL != goneURL || got[0].Archive == "" {
		t.Fatalf("got %+v", got)
	}
	text, spec := deadLinksTask(got)
	golden := filepath.Join("testdata", "dead-links-task.golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		_ = os.MkdirAll("testdata", 0o755)
		_ = os.WriteFile(golden, []byte(text), 0o644)
	}
	if want, err := os.ReadFile(golden); err != nil || string(want) != text {
		t.Fatalf("task text differs from %s:\n%s", golden, text)
	}
	if len(spec) != 1 || spec[0].URL != goneURL {
		t.Fatalf("%+v", spec)
	}
	// A failed run among the newest two: nothing, not a half-agreed set.
	m.runs[1].Conclusion = "failure"
	if got, err := deadLinks(t.Context(), m, rc, linksClass()); err != nil || len(got) != 0 {
		t.Fatalf("a failed run finds nothing: %+v %v", got, err)
	}
}

func TestParseFindingsIsStrict(t *testing.T) {
	if _, err := ParseFindings([]byte(`[{"file":"a.md","url":"https://x","status":"404","line":1,"note":"x"}]`)); err == nil {
		t.Fatal("an unknown key refuses the file")
	}
	if _, err := ParseFindings([]byte(`[] []`)); err == nil {
		t.Fatal("trailing data refuses the file")
	}
}
````

  `deadlinks_schedule_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/forge/forgetest"
)

func deadLinksScheduler(t *testing.T, cl config.Class, objs ...v1alpha1.Task) *Scheduler {
	t.Helper()
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s)
	for i := range objs {
		c = c.WithObjects(&objs[i])
	}
	now := time.Date(2026, 10, 22, 5, 0, 0, 0, time.UTC)
	f := forgetest.NewFake()
	art := []byte(`[{"file":"docs/a.md","line":1,"url":"https://example.com/gone","status":"404"}]`)
	f.SetRuns([]forge.WorkflowRun{{ID: 2, Event: "schedule", Branch: "main", Conclusion: "success"},
		{ID: 1, Event: "schedule", Branch: "main", Conclusion: "success"}}, map[int64][]byte{1: art, 2: art})
	f.SetMainFiles(map[string]string{"docs/a.md": "[a](https://example.com/gone)"})
	cfg := &config.Config{Repository: "Smana/cloud-native-ref", Maintainers: []string{"Smana"}, Defaults: config.Defaults{DataClass: "public"},
		Classes:   map[string]config.Class{"dead-links": cl},
		Schedules: []config.Schedule{{Name: "dead-links", Cron: "0 5 * * *", Class: "dead-links", Text: "Fix the dead links below.", Probe: "dead-links"}}}
	return scheduler(f, c, func(context.Context) bool { return false }, now, cfg)
}

func scheduledTasks(t *testing.T, s *Scheduler) []v1alpha1.Task {
	var l v1alpha1.TaskList
	if err := s.Client.List(t.Context(), &l); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

// MC14 and L9: a class off has no intake; a class in shadow gets one task of its findings, and
// none more while that task is open.
func TestTheDeadLinksSchedule(t *testing.T) {
	cl := linksClass()
	sch := deadLinksScheduler(t, cl)
	if err := sch.Tick(t.Context()); err != nil || len(scheduledTasks(t, sch)) != 0 {
		t.Fatalf("a class off starts nothing: %v", err)
	}
	cl.Shadow = true
	sch = deadLinksScheduler(t, cl)
	if err := sch.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	ts := scheduledTasks(t, sch)
	if len(ts) != 1 || len(ts[0].Spec.Findings) != 1 || ts[0].Spec.Findings[0].URL != "https://example.com/gone" ||
		ts[0].Spec.PredictedClass != "dead-links" {
		t.Fatalf("%+v", ts)
	}
	open := v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "abcdefgh", Namespace: "agent-system"},
		Spec:   v1alpha1.TaskSpec{Source: v1alpha1.Source{Kind: "schedule", Ref: "dead-links"}},
		Status: v1alpha1.TaskStatus{Phase: v1alpha1.PhaseAwaitingHuman}}
	sch = deadLinksScheduler(t, cl, open)
	if err := sch.Tick(t.Context()); err != nil || len(scheduledTasks(t, sch)) != 1 {
		t.Fatalf("an open task of the schedule holds the next one (L9): %v", err)
	}
}
```

  The golden, `testdata/dead-links-task.golden` (no trailing newline):

```text
Dead external links, each reported by the last two nightly runs of the dead-links workflow on main:

1. `docs/a.md`, line 3: <https://example.com/gone> (404 Not Found). Snapshot: <https://web.archive.org/web/20261011195309/https://example.com/gone>.

Change only these link targets, in these files: each to a page on the same host, or to the snapshot given. Leave link text, prose and code blocks alone, and add or delete no file. A target with no replacement on its host and no snapshot stays as it is.
```

  In `triage/classify_test.go`:

```diff
diff --git a/internal/factory/triage/classify_test.go b/internal/factory/triage/classify_test.go
index a4533da..9c82075 100644
--- a/internal/factory/triage/classify_test.go
+++ b/internal/factory/triage/classify_test.go
@@ -177,3 +177,17 @@ func TestClassifyReadsTheIssueOnItsRepository(t *testing.T) {
 		t.Fatalf("an unknown repository reads no label: %q %v", d.PredictedClass, err)
 	}
 }
+
+// MC6: an oracle class's task runs solo on light, without a classifier call or the control group.
+func TestAnOracleClassRunsSoloOnLight(t *testing.T) {
+	cfg := testCfg()
+	cfg.Triage.ControlPercent = 100
+	cfg.Classes["dead-links"] = config.Class{Shadow: true, Oracle: &config.Oracle{Probe: "dead-links", Workflow: "dead-links.yaml", MaxFindings: 10}}
+	c7 := &fakeC7{resp: C7Response{Tier: "frontier", Classifier: "semantic-router"}}
+	d, err := Classify{Cfg: cfg, C: c7, Forge: forgetest.NewFake()}.Triage(context.Background(),
+		&v1alpha1.Task{Spec: v1alpha1.TaskSpec{Source: v1alpha1.Source{Kind: "schedule"}, PredictedClass: "dead-links", Text: "x", DataClass: "public"}})
+	if err != nil || d.Template != "solo" || d.Budget.Tier != "light" || d.PredictedClass != "dead-links" || d.Classification.Control ||
+		c7.got.Text != "" {
+		t.Fatalf("%+v %v (classifier asked: %+v)", d, err, c7.got)
+	}
+}
```

- [ ] **Step 2: Run.** `go test ./internal/factory/intake/ ./internal/factory/triage/` — Expected: build failure, `undefined: deadLinks`.

- [ ] **Step 3: Implement.** `deadlinks.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/mdlinks"
)

// LinkFinding is one entry of a dead-links run's findings.json: a link target lychee could not reach
// with a status that means gone (404, 410) or a name that does not resolve (nxdomain).
type LinkFinding struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	URL     string `json:"url"`
	Status  string `json:"status"`
	Archive string `json:"archive,omitempty"`
}

// maxFindingsBytes bounds findings.json: 1,000 findings of 1 KiB each.
const maxFindingsBytes = 1 << 20

// ParseFindings reads findings.json strictly: an unknown key or trailing data refuses the whole
// file, so a changed detector never feeds the probe half-read findings.
func ParseFindings(raw []byte) ([]LinkFinding, error) {
	if len(raw) > maxFindingsBytes {
		return nil, fmt.Errorf("intake: findings.json is %d bytes, over %d", len(raw), maxFindingsBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var fs []LinkFinding
	if err := dec.Decode(&fs); err != nil {
		return nil, fmt.Errorf("intake: findings.json: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("intake: findings.json holds more than one document")
	}
	return fs, nil
}

// Agreed are newest's findings that previous reported too, same file and URL (MC5: two
// consecutive runs on main agree). newest's line and archive are kept.
func Agreed(newest, previous []LinkFinding) []LinkFinding {
	var out []LinkFinding
	for _, f := range newest {
		if slices.ContainsFunc(previous, func(p LinkFinding) bool { return p.File == f.File && p.URL == f.URL }) &&
			!slices.ContainsFunc(out, func(o LinkFinding) bool { return o.File == f.File && o.URL == f.URL }) {
			out = append(out, f)
		}
	}
	return out
}

var archiveRE = regexp.MustCompile(`^https://web\.archive\.org/web/[0-9]{1,14}/https://[^\s<>()]+$`)

// keepable is f as a task may hold it, or false: a gone status, a clean relative path the class
// allows and does not protect, an https target R52 can retarget, an archive only when it is a
// Wayback snapshot of that target.
func keepable(f LinkFinding, cl config.Class) (LinkFinding, bool) {
	if f.Status != "404" && f.Status != "410" && f.Status != "nxdomain" {
		return LinkFinding{}, false
	}
	if f.File == "" || path.Clean(f.File) != f.File || strings.HasPrefix(f.File, "/") || strings.HasPrefix(f.File, "..") ||
		len(f.File) > 512 || !matches(cl.AllowedFiles, f.File) || matches(cl.ProtectedFiles, f.File) {
		return LinkFinding{}, false
	}
	u, err := url.Parse(f.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(f.URL) > 2048 ||
		strings.ContainsAny(f.URL, " \t\r\n<>()") {
		return LinkFinding{}, false
	}
	if f.Archive != "" && (!archiveRE.MatchString(f.Archive) || !strings.HasSuffix(f.Archive, "/"+f.URL) || len(f.Archive) > 2048) {
		f.Archive = ""
	}
	return f, true
}

func matches(pats []string, p string) bool {
	return slices.ContainsFunc(pats, func(s string) bool {
		re, err := regexp.Compile(s)
		return err == nil && re.MatchString(p)
	})
}

// DeadLinksMerger is what the dead-links probe reads through the merger App: the detector's runs
// and their artifacts, and a file on main.
type DeadLinksMerger interface {
	WorkflowRuns(ctx context.Context, workflow string) ([]forge.WorkflowRun, error)
	RunArtifact(ctx context.Context, runID int64, name, file string) ([]byte, error)
	FileAt(ctx context.Context, path, ref string) (string, error)
}

// deadLinks is the dead-links probe (MC5, MC6): the findings of the newest two completed runs of
// the class's workflow on main, kept when both runs report them and the class may fix them, and
// each still a link target at its file on main. Nothing while either run is missing or failed.
func deadLinks(ctx context.Context, m DeadLinksMerger, rc *config.Repository, cl config.Class) ([]LinkFinding, error) {
	runs, err := m.WorkflowRuns(ctx, cl.Oracle.Workflow)
	if err != nil {
		return nil, err
	}
	var two []forge.WorkflowRun
	for _, r := range runs { // newest first
		if !detectorRun(r, rc) {
			continue
		}
		if two = append(two, r); len(two) == 2 {
			break
		}
	}
	if len(two) < 2 || two[0].Conclusion != "success" || two[1].Conclusion != "success" {
		return nil, nil
	}
	var sets [2][]LinkFinding
	for i, r := range two {
		raw, err := m.RunArtifact(ctx, r.ID, "findings", "findings.json")
		if err != nil {
			return nil, err
		}
		if sets[i], err = ParseFindings(raw); err != nil {
			return nil, err
		}
	}
	var out []LinkFinding
	files := map[string][]string{}
	for _, f := range Agreed(sets[0], sets[1]) {
		k, ok := keepable(f, cl)
		if !ok {
			continue
		}
		ts, read := files[k.File]
		if !read {
			text, err := m.FileAt(ctx, k.File, "main")
			if err != nil {
				return nil, err
			}
			_, ts = mdlinks.Targets(text)
			files[k.File] = ts
		}
		if slices.Contains(ts, k.URL) {
			out = append(out, k)
		}
	}
	slices.SortFunc(out, func(a, b LinkFinding) int { return strings.Compare(a.File+"\x00"+a.URL, b.File+"\x00"+b.URL) })
	return out[:min(len(out), cl.Oracle.MaxFindings)], nil
}

// detectorRun: a run on main that a schedule started, or a maintainer by hand. A pull request's
// run reads the pull request's tree, and anyone else's dispatch is not the detector's verdict.
func detectorRun(r forge.WorkflowRun, rc *config.Repository) bool {
	switch {
	case r.Branch != "main":
		return false
	case r.Event == "schedule":
		return true
	default:
		return r.Event == "workflow_dispatch" && rc.IsMaintainer(r.Actor)
	}
}

// deadLinksTask renders the findings as the task's text (MC6): validated paths and URLs and the
// statuses' fixed words, nothing else from the artifact.
func deadLinksTask(fs []LinkFinding) (string, []v1alpha1.Finding) {
	var b strings.Builder
	b.WriteString("Dead external links, each reported by the last two nightly runs of the dead-links workflow on main:\n\n")
	spec := make([]v1alpha1.Finding, 0, len(fs))
	for i, f := range fs {
		why := map[string]string{"404": "404 Not Found", "410": "410 Gone", "nxdomain": "the host name does not resolve"}[f.Status]
		fmt.Fprintf(&b, "%d. `%s`, line %d: <%s> (%s).", i+1, f.File, f.Line, f.URL, why)
		if f.Archive != "" {
			fmt.Fprintf(&b, " Snapshot: <%s>.", f.Archive)
		}
		b.WriteString("\n")
		spec = append(spec, v1alpha1.Finding{File: f.File, URL: f.URL, Archive: f.Archive})
	}
	b.WriteString("\nChange only these link targets, in these files: each to a page on the same host, or to the snapshot " +
		"given. Leave link text, prose and code blocks alone, and add or delete no file. A target with no " +
		"replacement on its host and no snapshot stays as it is.")
	return b.String(), spec
}
```

  `schedule.go` (the probe dispatches; `dead-links` gated by the switch and L9; findings onto the Task):

```diff
diff --git a/internal/factory/intake/schedule.go b/internal/factory/intake/schedule.go
index d058396..f94e3b0 100644
--- a/internal/factory/intake/schedule.go
+++ b/internal/factory/intake/schedule.go
@@ -9,6 +9,7 @@ import (
 	"errors"
 	"fmt"
 	"log/slog"
+	"slices"
 	"strings"
 	"time"
 
@@ -54,6 +55,7 @@ type ScheduleForge interface {
 // probe is a merger-side read, so checks:read never joins the factory App's scope.
 type ScheduleMerger interface {
 	PullRequestChecks(ctx context.Context, number int) (forge.Checks, error)
+	DeadLinksMerger
 }
 
 // Scheduler is a leader-only manager.Runnable that turns config's schedules into Tasks (§1):
@@ -135,8 +137,9 @@ func (s *Scheduler) tick(ctx context.Context, rc *config.Repository) []error {
 		}
 		key := taskid.ScheduleKey(e.Name, slot)
 		text := e.Text
+		var findings []v1alpha1.Finding
 		if e.Probe != "" {
-			found, detail, err := s.probe(ctx, rc, e.Probe, e.ProbeAuthor)
+			found, detail, fs, err := s.probe(ctx, rc, e)
 			if err != nil {
 				errs = append(errs, fmt.Errorf("%s: %w", e.Name, err))
 				continue
@@ -144,7 +147,7 @@ func (s *Scheduler) tick(ctx context.Context, rc *config.Repository) []error {
 			if !found {
 				continue // "if it finds nothing, no task is created" (§1)
 			}
-			text += "\n\n" + detail
+			text, findings = text+"\n\n"+detail, fs
 		}
 		dataClass := e.DataClass
 		if dataClass == "" {
@@ -156,7 +159,7 @@ func (s *Scheduler) tick(ctx context.Context, rc *config.Repository) []error {
 			Spec: v1alpha1.TaskSpec{
 				Source: v1alpha1.Source{Kind: "schedule", Ref: e.Name, Key: key, RequestedBy: "system:scheduler",
 					Trust: "trusted", ContentSHA256: hex.EncodeToString(sum[:])},
-				Repository: rc.Name, Text: text, DataClass: dataClass, PredictedClass: e.Class,
+				Repository: rc.Name, Text: text, DataClass: dataClass, PredictedClass: e.Class, Findings: findings,
 			},
 		}
 		if err := s.Client.Create(ctx, t); err != nil && !apierrors.IsAlreadyExists(err) {
@@ -166,21 +169,68 @@ func (s *Scheduler) tick(ctx context.Context, rc *config.Repository) []error {
 	return errs
 }
 
-// probe: renovate-red lists open Renovate PRs whose checks have been red for more than 24 h —
-// bumps the factory can land itself when Renovate's own branch went stale.
-func (s *Scheduler) probe(ctx context.Context, rc *config.Repository, name, author string) (bool, string, error) {
-	if name != "renovate-red" {
-		return false, "", fmt.Errorf("intake: unknown probe %q", name)
+// probe runs the schedule's probe: whether it found work, the text it adds, and the findings an
+// oracle class's task carries.
+func (s *Scheduler) probe(ctx context.Context, rc *config.Repository, e config.Schedule) (bool, string, []v1alpha1.Finding, error) {
+	switch e.Probe {
+	case "renovate-red":
+		found, detail, err := s.renovateRed(ctx, rc, e.ProbeAuthor)
+		return found, detail, nil, err
+	case "dead-links":
+		return s.deadLinksProbe(ctx, rc, e)
 	}
+	return false, "", nil, fmt.Errorf("intake: unknown probe %q", e.Probe)
+}
+
+// conns is the repository's two connections: Clients' (v2 D8), or Forge and Merger.
+func (s *Scheduler) conns(rc *config.Repository) (ScheduleForge, ScheduleMerger, error) {
+	if s.Clients == nil {
+		return s.Forge, s.Merger, nil
+	}
+	gh, merger, ok := s.Clients(rc.Name)
+	if !ok {
+		return nil, nil, fmt.Errorf("intake: no GitHub connection for %s (v2 D8)", rc.Name)
+	}
+	return gh, merger, nil
+}
+
+// deadLinksProbe (MC5, MC6): nothing while the class is off (MC14) or while a task of this
+// schedule is still open (loops L9); else the findings both newest detector runs agree on.
+func (s *Scheduler) deadLinksProbe(ctx context.Context, rc *config.Repository, e config.Schedule) (bool, string, []v1alpha1.Finding, error) {
+	cl := rc.Classes[e.Class]
+	if (!cl.Live && !cl.Shadow) || cl.Oracle == nil {
+		return false, "", nil, nil
+	}
+	var l v1alpha1.TaskList
+	if err := s.Client.List(ctx, &l, client.InNamespace(s.Namespace)); err != nil {
+		return false, "", nil, err
+	}
+	if slices.ContainsFunc(l.Items, func(t v1alpha1.Task) bool {
+		return t.Spec.Source.Kind == "schedule" && t.Spec.Source.Ref == e.Name && !v1alpha1.TerminalPhase(t.Status.Phase)
+	}) {
+		return false, "", nil, nil
+	}
+	_, merger, err := s.conns(rc)
+	if err != nil {
+		return false, "", nil, err
+	}
+	fs, err := deadLinks(ctx, merger, rc, cl)
+	if err != nil || len(fs) == 0 {
+		return false, "", nil, err
+	}
+	text, spec := deadLinksTask(fs)
+	return true, text, spec, nil
+}
+
+// renovateRed lists open Renovate PRs whose checks have been red for more than 24 h — bumps the
+// factory can land itself when Renovate's own branch went stale.
+func (s *Scheduler) renovateRed(ctx context.Context, rc *config.Repository, author string) (bool, string, error) {
 	if !rc.NeedsMerger() {
 		return false, "", nil // no merger key is required, and reading checks needs it: nothing to find
 	}
-	gh, merger := s.Forge, s.Merger
-	if s.Clients != nil {
-		var ok bool
-		if gh, merger, ok = s.Clients(rc.Name); !ok {
-			return false, "", fmt.Errorf("intake: no GitHub connection for %s (v2 D8)", rc.Name)
-		}
+	gh, merger, err := s.conns(rc)
+	if err != nil {
+		return false, "", err
 	}
 	prs, err := gh.OpenPullRequests(ctx)
 	if err != nil {
```

  `config.go` (the probe's validation):

```diff
diff --git a/internal/factory/config/config.go b/internal/factory/config/config.go
index e06eab5..8bebbcd 100644
--- a/internal/factory/config/config.go
+++ b/internal/factory/config/config.go
@@ -1043,8 +1043,14 @@ func (c *Config) validateRepository(r *Repository, seenSchedule map[string]bool,
 		if e.Text == "" || len(e.Text) > c.Caps.MaxTextBytes {
 			bad("schedule %s: text must be 1..%d bytes", e.Name, c.Caps.MaxTextBytes)
 		}
-		if e.Probe != "" && e.Probe != "renovate-red" {
-			bad("schedule %s: probe %q is not renovate-red", e.Name, e.Probe)
+		switch e.Probe {
+		case "", "renovate-red":
+		case "dead-links":
+			if o := r.Classes[e.Class].Oracle; o == nil || o.Probe != "dead-links" {
+				bad("schedule %s: probe dead-links needs class %q to declare oracle.probe dead-links", e.Name, e.Class)
+			}
+		default:
+			bad("schedule %s: probe %q is not renovate-red or dead-links", e.Name, e.Probe)
 		}
 		if e.Probe != "" && e.ProbeAuthor == "" {
 			bad("schedule %s: probeAuthor is required with a probe", e.Name)
```

  `triage/classify.go`:

```diff
diff --git a/internal/factory/triage/classify.go b/internal/factory/triage/classify.go
index 5421598..80a5540 100644
--- a/internal/factory/triage/classify.go
+++ b/internal/factory/triage/classify.go
@@ -78,6 +78,12 @@ func (c Classify) Triage(ctx context.Context, t *v1alpha1.Task) (Decision, error
 	if t.Spec.Source.Kind == "runlore" {
 		dataClass = "internal" // the investigate template forces it (§2)
 	}
+	if c.Cfg.RepositoryFor(t.Spec.Repository).Classes[class].Oracle != nil {
+		// MC6: the findings are the task and the oracle its verifier: one implementer at tier light,
+		// no classifier call, so no control group either (OD-14 measures the classifier).
+		return Decision{Template: "solo", PredictedClass: class, DataClass: dataClass, Budget: BudgetFor(c.Cfg, "light"),
+			Classification: v1alpha1.Classification{Tier: "light", Classifier: "static"}}, nil
+	}
 	res, err := c.C.Classify(ctx, C7Request{Text: t.Spec.Text, Ref: t.Name, DataClass: dataClass})
 	if err != nil || (res.Tier != "light" && res.Tier != "standard" && res.Tier != "frontier") {
 		res = C7Response{Tier: "standard", Classifier: "static", Fallback: "static"} // never blocks (C7)
```

- [ ] **Step 4: Run.** `go test -race ./internal/factory/... && task lint` — Expected: `ok` everywhere, `0 issues.`

- [ ] **Step 5: Commit.** `git commit -m "feat(factory): the dead-links probe, its schedule and its triage"`.

### Task 9: Reasons, docs, gate and release

**Files:**
- Modify: `internal/factory/narrate/narrate.go`, `docs/factory.md`

- [ ] **Step 1: Prose.** In `narrate.go`'s `reasons()`, after `"holdout_pending"`:

```go
		"oracle_finding_open":       "a finding its task was made of is still in the pull request's head",
		"oracle_unrequested":        "it moves a link target its task's findings did not name, or to a place they do not allow",
```

- [ ] **Step 2: Docs.** In `docs/factory.md`:
  - Row 8 of the gate table gains `oracle` after `holdoutChecks` ("the head holds none of the
    task's findings and moves no other target") with reasons `oracle_finding_open`,
    `oracle_unrequested`; the reasons table gains both rows.
  - The templates table gains a first row: `| \`solo\` | A class with an \`oracle\` (such as \`dead-links\`), always at \`light\`, without the classifier |`.
  - The config table's `classes` cell adds `oracle: {probe: dead-links, workflow, maxFindings ≤ 16}`
    (needs `holdoutChecks` and `diffShape: links-only`); `schedules` adds "`probe` is
    `renovate-red` or `dead-links`; `dead-links` needs its class to declare the oracle, finds
    nothing while the class is off or a task of the schedule is open, and reads the newest two
    runs of the oracle's workflow on `main`".
  - A short section after the merge gate, "Oracle classes": the detector's two runs, the probe's
    filters (R13), the task text, the oracle at the gate, and that the merger's token must read
    `actions` and download artifacts from `productionresultssa*.blob.core.windows.net` (egress).

- [ ] **Step 3: Gate.** Run: `task check` (Docker running). Expected: exit 0.

- [ ] **Step 4: Commit, push, PR B.**

```bash
git add internal/factory/narrate docs/factory.md
git commit -m "docs(factory): oracle classes and the dead-links probe"
git fetch origin && git rebase origin/main && git push -u origin feat/dead-links
gh pr create --title "feat(factory): dead-links, the first oracle-backed class" --body "Merge gate classes PR 3 without the monitor (ai-native-ref docs/superpowers/specs/2026-10-22-merge-gate-classes-design.md, MC4–MC6). The probe keeps a finding only when the newest two runs of the detector on main agree; the findings go on the Task; the oracle condition proves the head fixed them and moved nothing else. Offline proofs 3, 8 and 9. The class ships declared off (MC14)."
```

- [ ] **Step 5: Release, once PR B is merged.**

```bash
git fetch origin && git checkout --detach origin/main
git tag -a v0.17.0 -m "v0.17.0" && git push origin v0.17.0
gh run watch "$(gh run list --workflow release.yaml --limit 1 --json databaseId --jq '.[0].databaseId')"
```
  Expected: the release workflow green; `regctl image digest ghcr.io/smana/agent-factory:v0.17.0`
  and `regctl image digest ghcr.io/smana/charts/agent-factory:0.17.0` each print a digest.

---

## PR C — cloud-native-ref: the detector

### Task 10: The findings script and its suite

**Files:**
- Create: `.github/scripts/dead-links/findings.py`, `.github/scripts/dead-links/changed-targets.sh`, `.github/scripts/dead-links/install-lychee.sh`, `scripts/ci/tests/test-dead-links-findings.sh`

- [ ] **Step 1: Write the failing suite** (offline proof 10), `scripts/ci/tests/test-dead-links-findings.sh`:

```bash
#!/usr/bin/env bash
# requires: python3
#
# Merge gate classes MC5: findings.py keeps 404, 410 and a name that does not resolve, and drops
# 403, 429, 5xx, timeouts and a refused connection; the archive is upgraded to https. A fixture
# lychee report and a stub availability API, no other network but the resolver.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBJECT="$HERE/../../../.github/scripts/dead-links/findings.py"
fails=0
fail() { printf 'FAIL  %s\n' "$*" >&2; fails=$((fails + 1)); }
d="$(mktemp -d)"
trap 'kill "$stub" 2>/dev/null; rm -rf "$d"' EXIT

cat >"$d/lychee.json" <<'JSON'
{"error_map": {
  "docs/a.md": [
    {"url": "https://example.com/gone", "status": {"text": "Rejected status code: 404 Not Found", "code": 404}, "span": {"line": 3, "column": 4}},
    {"url": "https://example.com/removed", "status": {"text": "Rejected status code: 410 Gone", "code": 410}, "span": {"line": 4, "column": 4}},
    {"url": "https://example.com/forbidden", "status": {"text": "Rejected status code: 403 Forbidden", "code": 403}, "span": {"line": 5, "column": 4}},
    {"url": "https://example.com/limited", "status": {"text": "Rejected status code: 429 Too Many Requests", "code": 429}, "span": {"line": 6, "column": 4}},
    {"url": "https://example.com/down", "status": {"text": "Rejected status code: 503 Service Unavailable", "code": 503}, "span": {"line": 7, "column": 4}},
    {"url": "https://dead-links-fixture.invalid/x", "status": {"text": "Network error: Connection failed.", "details": "Connection failed."}, "span": {"line": 8, "column": 4}},
    {"url": "http://127.0.0.1:1/x", "status": {"text": "Network error: Connection refused", "details": "Connection refused"}, "span": {"line": 9, "column": 4}}
  ]},
 "timeout_map": {"docs/a.md": [{"url": "https://example.com/slow", "status": {"text": "Timeout"}}]}}
JSON

WAYBACK_API=off python3 "$SUBJECT" "$d/lychee.json" >"$d/out.json" || fail "findings.py exited $?"
got=$(python3 -c 'import json,sys; print(" ".join(f["status"]+"="+f["url"] for f in json.load(open(sys.argv[1]))))' "$d/out.json")
want="nxdomain=https://dead-links-fixture.invalid/x 404=https://example.com/gone 410=https://example.com/removed"
[ "$got" = "$want" ] || fail "findings: got '$got', want '$want'"

port=$((20000 + RANDOM % 20000))
cat >"$d/stub.py" <<'PY'
import http.server, json, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers()
        self.wfile.write(json.dumps({"archived_snapshots": {"closest": {"status": "200", "available": True,
            "url": "http://web.archive.org/web/20261011195309/https://example.com/gone"}}}).encode())
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
python3 "$d/stub.py" "$port" & stub=$!
sleep 1
WAYBACK_API="http://127.0.0.1:$port/" WAYBACK_PACE=0 python3 "$SUBJECT" "$d/lychee.json" >"$d/arch.json" || fail "findings.py exited $?"
arch=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[1].get("archive", ""))' "$d/arch.json")
[ "$arch" = "https://web.archive.org/web/20261011195309/https://example.com/gone" ] || fail "archive: got '$arch', want the https snapshot"

[ "$fails" -eq 0 ] && echo "ok  dead-links findings" || exit 1
```

- [ ] **Step 2: Run.** `bash scripts/ci/tests/test-dead-links-findings.sh` — Expected: FAIL, `findings.py exited 2` (no such file).

- [ ] **Step 3: Implement.** `.github/scripts/dead-links/findings.py`:

```python
#!/usr/bin/env python3
"""findings.json from a lychee JSON report: the dead-links detector's verdict (merge gate MC5).

A finding is a link target that answered 404 or 410 after redirects, or whose host name does not
resolve (the resolver's EAI_NONAME, checked here: lychee reports it as a generic connection
failure). 403, 429, 5xx, timeouts and every other error are never findings. Each finding carries
the Wayback Machine's closest snapshot when one exists, as https.

    findings.py lychee.json > findings.json

The archive is best effort: archive.org answers 429 after a few requests from one address, so
lookups are paced (WAYBACK_PACE seconds apart, 5 by default), at most 20 a run, and the first
refusal ends them for the run. A finding without an archive is still a finding. WAYBACK_API
overrides the availability endpoint (tests); WAYBACK_API=off skips the lookup.
"""
import json
import os
import socket
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

WAYBACK = os.environ.get("WAYBACK_API", "https://archive.org/wayback/available")
PACE = float(os.environ.get("WAYBACK_PACE", "5"))
GONE = {404: "404", 410: "410"}
lookups = {"left": 20}


def nxdomain(url):
    host = urllib.parse.urlsplit(url).hostname
    if not host:
        return False
    try:
        socket.getaddrinfo(host, 443)
    except socket.gaierror as e:
        return e.errno == socket.EAI_NONAME
    return False


def snapshot(url):
    if WAYBACK == "off" or lookups["left"] <= 0:
        return ""
    if lookups["left"] < 20:
        time.sleep(PACE)
    lookups["left"] -= 1
    q = WAYBACK + "?" + urllib.parse.urlencode({"url": url})
    req = urllib.request.Request(q, headers={"User-Agent": "dead-links (+https://github.com/Smana/cloud-native-ref)"})
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            closest = json.load(r).get("archived_snapshots", {}).get("closest") or {}
    except urllib.error.HTTPError:
        lookups["left"] = 0  # a 429 or worse: no more lookups this run
        return ""
    except (OSError, ValueError):
        return ""
    snap = closest.get("url", "")
    if closest.get("available") is not True or closest.get("status") != "200" or not snap:
        return ""
    if snap.startswith("http://web.archive.org/"):  # the API answers http; R52 retargets https to https only
        snap = "https://" + snap[len("http://"):]
    return snap if snap.startswith("https://web.archive.org/web/") else ""


def main(path):
    report = json.load(open(path))
    out = []
    for file, entries in sorted((report.get("error_map") or {}).items()):
        for e in entries:
            url, code = e["url"], e.get("status", {}).get("code")
            status = GONE.get(code) or ("nxdomain" if code is None and nxdomain(url) else None)
            if status is None:
                continue
            f = {"file": file.removeprefix("./"), "line": (e.get("span") or {}).get("line", 0), "url": url, "status": status}
            snap = snapshot(url)
            if snap:
                f["archive"] = snap
            out.append(f)
    out.sort(key=lambda f: (f["file"], f["url"]))
    json.dump(out, sys.stdout, indent=1)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main(sys.argv[1])
```

  `.github/scripts/dead-links/changed-targets.sh`:

```bash
#!/usr/bin/env bash
# The external link targets a pull request adds or changes in Markdown, one per line: what
# "External links 🔗" fetches (merge gate MC5). lychee --dump extracts them as the nightly scan
# does, code blocks skipped; a target already on the merge base is not the pull request's.
#
#   changed-targets.sh <base-sha>   (from the head's checkout, with the base fetched)
set -euo pipefail
mb=$(git merge-base "$1" HEAD)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
git diff --name-only --diff-filter=AMR "$mb" HEAD -- '*.md' | while IFS= read -r f; do
  lychee --dump --scheme https --scheme http -- "$f" | sort -u >"$tmp/head"
  if git cat-file -e "$mb:$f" 2>/dev/null; then
    git show "$mb:$f" >"$tmp/base.md"
    lychee --dump --scheme https --scheme http -- "$tmp/base.md" | sort -u >"$tmp/base"
  else
    : >"$tmp/base"
  fi
  comm -13 "$tmp/base" "$tmp/head"
done | sort -u
```

  `.github/scripts/dead-links/install-lychee.sh`:

```bash
#!/usr/bin/env bash
# Installs lychee $LYCHEE_VERSION from its release, checked against $LYCHEE_SHA256, and puts it on
# PATH for the job's later steps. The workflow pins both: a digest, not a tag (merge gate MC5).
set -euo pipefail
d="$RUNNER_TEMP/lychee"
mkdir -p "$d"
curl -fsSL -o "$d/lychee.tgz" \
  "https://github.com/lycheeverse/lychee/releases/download/lychee-v${LYCHEE_VERSION}/lychee-x86_64-unknown-linux-gnu.tar.gz"
echo "${LYCHEE_SHA256}  $d/lychee.tgz" | sha256sum -c -
tar -xzf "$d/lychee.tgz" -C "$d" --strip-components=1 lychee-x86_64-unknown-linux-gnu/lychee
echo "$d" >>"$GITHUB_PATH"
```

  `chmod +x .github/scripts/dead-links/*.sh .github/scripts/dead-links/findings.py scripts/ci/tests/test-dead-links-findings.sh`.

- [ ] **Step 4: Run.** `bash scripts/ci/tests/test-dead-links-findings.sh && shellcheck -x .github/scripts/dead-links/*.sh scripts/ci/tests/test-dead-links-findings.sh` — Expected: `ok  dead-links findings`, no shellcheck output.

- [ ] **Step 5: Commit.** `git commit -m "feat(ci): dead-links' findings script and its suite"`.

### Task 11: The workflow

**Files:**
- Create: `.github/workflows/dead-links.yaml`

- [ ] **Step 1: Write it:**

```yaml
# Dead external links (agent factory, merge gate classes MC4–MC6; a gate path, `^\.github/`).
#
# scan: nightly on main, and by hand. lychee checks the external targets of the Markdown that
# docs-links covers; findings.py keeps what is surely gone (404, 410, a name that does not
# resolve) and uploads findings.json, which the factory's dead-links probe reads through the
# merger App. Two consecutive runs must agree before a finding becomes a task.
#
# external-links ("External links 🔗"): on a pull request, every external target it adds or
# changes must answer 2xx after redirects. dead-links' holdout check (loops L7), not a required
# check: a human's docs pull request does not wait on someone else's site.
name: Dead links

on:
  schedule:
    - cron: "23 4 * * *"
  workflow_dispatch:
  pull_request:
    branches: ["main"]
    paths: ["**.md"]

permissions:
  contents: read

concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}

env:
  LYCHEE_VERSION: "0.24.2"
  LYCHEE_SHA256: "1f4e0ef7f6554a6ed33dd7ac144fb2e1bbed98598e7af973042fc5cd43951c9a"

jobs:
  scan:
    name: Scan external links 🕸
    if: github.event_name != 'pull_request'
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7

      - name: Install lychee
        run: .github/scripts/dead-links/install-lychee.sh

      # 0: nothing broken; 2: something broken. Anything else is lychee failing, and the run fails
      # with it, so the probe finds nothing on it (MC5 fails closed).
      - name: Scan
        env:
          GITHUB_TOKEN: ${{ github.token }}  # lychee reads github.com targets through the API
        run: |
          set +e
          lychee --no-progress --format json --output lychee.json --max-retries 3 --timeout 20 \
            --scheme https --scheme http \
            --exclude-path docs/superpowers --exclude-path docs/specs --exclude-path website/content/docs/decisions \
            'docs/**/*.md' 'website/content/**/*.md'
          rc=$?
          [ "$rc" -eq 0 ] || [ "$rc" -eq 2 ]

      - name: Findings
        run: python3 .github/scripts/dead-links/findings.py lychee.json >findings.json

      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with:
          name: findings
          path: findings.json
          retention-days: 14
          if-no-files-found: error

  external-links:
    name: External links 🔗
    if: github.event_name == 'pull_request'
    runs-on: ubuntu-latest
    timeout-minutes: 15
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7
        with:
          ref: ${{ github.event.pull_request.head.sha }}
          fetch-depth: 0

      - name: Install lychee
        run: .github/scripts/dead-links/install-lychee.sh

      - name: The pull request's external targets
        env:
          BASE: ${{ github.event.pull_request.base.sha }}
        run: .github/scripts/dead-links/changed-targets.sh "$BASE" >targets.txt

      - name: Each one answers 2xx
        run: |
          if [ ! -s targets.txt ]; then echo "no external link target added or changed"; exit 0; fi
          cat targets.txt
          lychee --no-progress --max-retries 3 --timeout 20 --accept '200..=299' targets.txt
```

- [ ] **Step 2: Check it.**

```bash
actionlint .github/workflows/dead-links.yaml
./scripts/ci/check-workflow-secrets.sh
./scripts/ci/check-policy-gate-coverage.sh
./scripts/ci/tests/run.sh
```
  Expected: no actionlint output; both checks exit 0 (the `.policy.yml` is unchanged, the
  workflow has no secret and no write scope); the suites pass with `test-dead-links-findings` among them.

- [ ] **Step 3: Commit, push, PR C.** After merge, run the workflow by hand once
  (`gh workflow run dead-links.yaml`) and check the run uploads `findings`: that run is a live
  observation, not a gate.

```bash
git add .github/workflows/dead-links.yaml
git commit -m "feat(ci): dead-links, a nightly lychee scan and the External links check"
git fetch origin && git rebase origin/main && git push -u origin feat/dead-links-detector
gh pr create --title "feat(ci): dead-links detector and the External links check" --body "The detector of the agent factory's dead-links class (ai-native-ref merge gate classes design, MC4–MC6): a nightly lychee scan of the Markdown docs-links covers, whose findings.json the factory's probe reads, and External links 🔗, which fetches every external target a pull request adds or changes. Not a required check. lychee 0.24.2 pinned by sha256."
```

---

## PR D — ai-native-ref: `dead-links` declared off

### Task 12: Pin v0.17.0 and the host

**Files:**
- Modify: `flux/sources/ocirepo-agent-factory.yaml`, `tooling/base/agent-factory/helm-values-configmap.yaml` (`image.tag`), `HOST_REF`

- [ ] **Step 1: Read the digests.**

```bash
regctl image digest ghcr.io/smana/charts/agent-factory:0.17.0
regctl image digest ghcr.io/smana/agent-factory:v0.17.0
git -C .host fetch origin main && git -C .host rev-parse origin/main   # PR C's merge or later
```

- [ ] **Step 2: Write them.** `ocirepo-agent-factory.yaml`: `tag: "0.17.0"  # agent-platform v0.17.0`
  and `digest:` the chart's. The values: `# agent-platform v0.17.0, the chart's appVersion.` and
  `tag: "v0.17.0@<image digest>"`. `HOST_REF`: the host commit.

- [ ] **Step 3: Run the pins suite.** `bash $SCRATCH/run-suite.sh python3 scripts/ci/tests/test-agent-factory-pins.py` — Expected: `0 problem(s)`.

- [ ] **Step 4: Do not commit yet:** Task 13's declaration rides in the same commit (measured: the
  declaration refuses to load on v0.16.0 and earlier).

### Task 13: The declaration, the schedule, the egress and the parity suite

**Files:**
- Modify: `tooling/base/agent-factory/helm-values-configmap.yaml`, `tooling/base/agent-factory/network-policy.yaml`
- Create: `scripts/ci/tests/test-agent-merge-gate-parity.py`

- [ ] **Step 1: Write the failing suite** (offline proof 11), `scripts/ci/tests/test-agent-merge-gate-parity.py`:

```python
#!/usr/bin/env python3
# requires: python3
"""Each class's fence, declared twice, is one fence (merge gate classes MC2).

A class that declares allowedFiles, protectedFiles, modifyOnly or maxModifiedLines in the
factory's config is held to its `low-risk: <class>` rule in the repository's .policy.yml, the
independent second check policy-bot evaluates:

- allowedFiles is the rule's only_changed_files, as a set;
- the rule's no_changed_files is the gate list plus protectedFiles, as a set;
- modifyOnly is file_not_added and file_not_deleted, both ['.*'];
- maxModifiedLines n is modified_lines total "< n+1".

A class with no rule may not be live. docs-links and revert declare none of the four: their fence
is the policy's alone, as before the vocabulary. The host's .policy.yml is the composite's; this
repository's is the working directory's (run-tests.sh runs this from .composite/).
"""
import copy
import pathlib
import sys

import yaml

HOST = pathlib.Path(__file__).resolve().parents[3]
OWN = pathlib.Path.cwd()
POLICY = {"Smana/cloud-native-ref": HOST / ".policy.yml", "Smana/ai-native-ref": OWN / ".policy.yml"}
CANON = "agent change approved by a maintainer"
FENCE = ("allowedFiles", "protectedFiles", "modifyOnly", "maxModifiedLines")


def rules_of(policy):
    return {r["name"]: r for r in policy.get("approval_rules", [])}


def mismatches(name, cl, rule, gates):
    """Every way the class's declared fence and its policy rule disagree."""
    cond = rule.get("if", {})
    out = []
    allowed = set((cond.get("only_changed_files") or {}).get("paths", []))
    if set(cl.get("allowedFiles", [])) != allowed:
        out.append(f"{name}: allowedFiles {sorted(cl.get('allowedFiles', []))} != only_changed_files {sorted(allowed)}")
    protected = set((cond.get("no_changed_files") or {}).get("paths", []))
    if protected != set(gates) | set(cl.get("protectedFiles", [])):
        out.append(f"{name}: no_changed_files is not the gate list plus protectedFiles: "
                   f"{sorted(protected ^ (set(gates) | set(cl.get('protectedFiles', []))))}")
    added = (cond.get("file_not_added") or {}).get("paths") == [".*"]
    deleted = (cond.get("file_not_deleted") or {}).get("paths") == [".*"]
    if bool(cl.get("modifyOnly")) != (added and deleted):
        out.append(f"{name}: modifyOnly {bool(cl.get('modifyOnly'))} but file_not_added/file_not_deleted {added}/{deleted}")
    total = (cond.get("modified_lines") or {}).get("total")
    want = f"< {cl['maxModifiedLines'] + 1}" if cl.get("maxModifiedLines") else None
    if total != want:
        out.append(f"{name}: maxModifiedLines {cl.get('maxModifiedLines')} but modified_lines total {total!r}")
    return out


def check(repo, classes, policy):
    rules = rules_of(policy)
    gates = rules[CANON]["if"]["no_changed_files"]["paths"]
    out = []
    for name, cl in sorted(classes.items()):
        if not any(k in cl for k in FENCE):
            continue
        rule = rules.get(f"low-risk: {name}")
        if rule is None:
            if cl.get("live"):
                out.append(f"{repo}: class {name} is live with no 'low-risk: {name}' rule in .policy.yml (MC2)")
            continue
        out += [f"{repo}: {m}" for m in mismatches(name, cl, rule, gates)]
    return out


cm = yaml.safe_load((HOST / "tooling/base/agent-factory/helm-values-configmap.yaml").read_text())
config = yaml.safe_load(cm["data"]["values.yaml"])["config"]
fails = []
for r in config["repositories"]:
    fails += check(r["name"], r.get("classes") or {}, yaml.safe_load(POLICY[r["name"]].read_text()))

# Offline proof 11, on the host's real rule: dead-links' fence as the factory declares it on the
# host agrees with docs-links' rule renamed, and a deliberate mismatch is caught.
host = yaml.safe_load(POLICY["Smana/cloud-native-ref"].read_text())
dead = next(r for r in config["repositories"] if r["name"] == "Smana/cloud-native-ref")["classes"].get("dead-links")
if dead is None:
    fails.append("self-test: the host declares no dead-links class")
else:
    probe = copy.deepcopy(host)
    twin = copy.deepcopy(rules_of(probe)["low-risk: docs-links"])
    twin["name"] = "low-risk: dead-links"
    probe["approval_rules"].append(twin)
    live = dict(dead, live=True)
    if check("self-test", {"dead-links": live}, probe):
        fails.append(f"self-test: dead-links disagrees with docs-links' rule: {check('self-test', {'dead-links': live}, probe)}")
    for key, value in [("maxModifiedLines", 30), ("modifyOnly", False), ("allowedFiles", [r"^docs/.+\.md$"]),
                       ("protectedFiles", [r"^website/"])]:
        if not check("self-test", {"dead-links": dict(live, **{key: value})}, probe):
            fails.append(f"self-test: a {key} mismatch went unnoticed")
    if not check("self-test", {"dead-links": live}, host):
        fails.append("self-test: a live class without its rule went unnoticed")

for f in fails:
    print(f"FAIL: {f}")
print(f"==> merge gate parity: {len(config['repositories'])} repositories, {len(fails)} problem(s)")
sys.exit(1 if fails else 0)
```

- [ ] **Step 2: Run it.** `git add scripts/ci/tests/test-agent-merge-gate-parity.py && bash $SCRATCH/check.sh "$PWD"` — Expected: the suite FAILs with `self-test: the host declares no dead-links class`.

- [ ] **Step 3: Declare.** In `helm-values-configmap.yaml`, the host's `classes`, after `scripts: {}`:

```yaml
            # Declared off (merge gate classes MC14): no switch, so no intake and no decision. A
            # session's first commit sets shadow: true (runbook 15). Its fence repeats the host's
            # `low-risk: docs-links` rule, which approves its pull requests until `low-risk:
            # dead-links` lands with the live switch; test-agent-merge-gate-parity.py holds the two.
            dead-links:
              allowedFiles: ['^(docs|website/content)/.+\.md$']
              protectedFiles: ['^docs/(superpowers|specs)/', '^website/content/docs/decisions/']
              modifyOnly: true
              maxModifiedLines: 20
              diffShape: links-only
              holdoutChecks: ["External links 🔗"]
              oracle: {probe: dead-links, workflow: dead-links.yaml, maxFindings: 10}
              maxPerDay: 1
```
  the host's `schedules`, after `link-rot`:

```yaml
            # dead-links' intake (MC6): the findings two consecutive nightly runs of the host's
            # dead-links.yaml agree on become one task. Inert while the class is off.
            - name: dead-links
              cron: "0 7 * * *"  # daily, after the 04:23 UTC scan
              class: dead-links
              probe: dead-links
              text: Fix the dead external links listed below.
```
  and ai-native-ref's `classes`, after `scripts: {}`:

```yaml
            # Declared off (MC14). No schedule here yet: this repository has no dead-links.yaml, and
            # its .policy.yml no low-risk rule, so a shadow decision could not reach would-arm.
            dead-links:
              allowedFiles: ['^(docs|website/content)/.+\.md$']
              protectedFiles: ['^docs/(superpowers|specs)/', '^website/content/docs/decisions/']
              modifyOnly: true
              maxModifiedLines: 20
              diffShape: links-only
              holdoutChecks: ["External links 🔗"]
              oracle: {probe: dead-links, workflow: dead-links.yaml, maxFindings: 10}
              maxPerDay: 1
```

- [ ] **Step 4: The egress (R19).** In `network-policy.yaml`, after the `api.github.com` rule:

```yaml
    - toFQDNs:  # dead-links' probe: GitHub redirects an artifact's download to its storage (R19)
        - matchPattern: "productionresultssa*.blob.core.windows.net"
      toPorts:
        - ports: [{port: "443", protocol: TCP}]
```

- [ ] **Step 5: Run.** `bash $SCRATCH/check.sh "$PWD"` — Expected: the parity suite prints `==> merge gate parity: 2 repositories, 0 problem(s)`.

- [ ] **Step 6: Commit Tasks 12 and 13 together.**

```bash
git add HOST_REF flux/sources/ocirepo-agent-factory.yaml tooling/base/agent-factory/helm-values-configmap.yaml \
  tooling/base/agent-factory/network-policy.yaml scripts/ci/tests/test-agent-merge-gate-parity.py
git commit -m "feat(agent-factory): pin v0.17.0 and declare dead-links off"
```

### Task 14: The panel, runbook 15, the docs, the gate

**Files:**
- Modify: `observability/base/agent-platform/grafana-dashboard-agent-factory.yaml`, `website/content/docs/platform/ai-platform/agents/factory.md`, `website/content/docs/platform/ai-platform/status.md`, `docs/runbooks/agent-factory/README.md`, `docs/superpowers/specs/2026-10-22-merge-gate-classes-design.md`
- Create: `docs/runbooks/agent-factory/15-dead-links.md`

- [ ] **Step 1: The panel (R25).** After panel 11, the last element of `panels`:

```json
        {"id": 12, "type": "table", "title": "Merge decisions by class and reason (7 days)", "gridPos": {"x": 12, "y": 32, "w": 12, "h": 8},
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "targets": [{"refId": "A", "expr": "sum by (class, verdict, reason) (increase(agent_factory_merge_decisions_total[7d])) > 0", "format": "table", "instant": true}]}
```
  (panel 11's closing `}` gains a `,`).

- [ ] **Step 2: Runbook 15**, `docs/runbooks/agent-factory/15-dead-links.md`, with these sections:
  1. *Prerequisites:* agent-platform v0.17.0 pinned (this PR); F1's release pinned (L1 waits for
     it, R1); the host's `dead-links.yaml` has run on `main` twice
     (`gh run list -R Smana/cloud-native-ref --workflow dead-links.yaml --branch main`), each run
     holding a `findings` artifact; the merger App's `actions` permission (runbook 12's mint test).
  2. *Shadow, L1:* one commit, `dead-links: {…, shadow: true}` on the host's entry, and the
     `link-rot` schedule removed (M4). Proof: the next 07:00 UTC slot with findings creates one
     Task (`kubectl -n agent-system get tasks -l` … `spec.source.ref=dead-links`), `solo`, tier
     `light`, `spec.findings` set; its PR gets "External links 🔗" and a `would-arm head=<sha>`
     comment; `vm 'sum by (class, verdict, reason) (agent_factory_merge_decisions_total{class="dead-links"})'`
     shows `shadow_would_arm`. No egress drop: `hubble observe --namespace agent-system --verdict DROPPED --to-fqdn '*.blob.core.windows.net'` is empty.
  3. *The drill, L2 (owner's go):* plant `[x](https://<host>/does-not-exist)` in a host doc,
     dispatch the workflow twice, then the class `live: true` with `low-risk: dead-links` in the
     host's `.policy.yml` (a host PR first: the docs-links rule renamed, R23); the merger merges at
     the head; after F1, the monitor's verdict; then `factory/revert`, the revert merged and
     `class_paused` on the next decision. Back to `shadow: true`.
  4. *What fails closed:* a failed scan run, a pull request run, a stranger's dispatch, a finding in
     one run only, an http target, a protected file, a URL in prose or a code block: no task. An
     artifact unreadable (egress, permission): the tick errors (`agent_factory_intake_errors_total{source="schedule"}`), nothing is created.
  Add its row to `docs/runbooks/agent-factory/README.md`'s table.

- [ ] **Step 3: Docs.** `factory.md`'s **Merge gate** row (line 25) gains: "Each class declares its
  conditions (allowed and protected files, size, diff shape, holdout checks) on top of a base it
  cannot remove; `dead-links`, which fixes dead external links a nightly scan found twice, is
  declared and switched off." `status.md`'s "More auto-merge classes" row gains, in "What it does
  for you": "The vocabulary and `dead-links` ship switched off (v0.17.0)." The spec's status line
  gains "First slice planned in agent-platform `docs/superpowers/plans/2026-10-22-merge-gate-classes-plan.md`; its rulings R1–R26".

- [ ] **Step 4: The gate** (offline proof 12). `bash $SCRATCH/check.sh "$PWD"` — Expected: the log ends `exit=0`, the parity, pins and repo-files suites pass, links and doc paths pass (runbook 15 names host files at the new `HOST_REF`).

- [ ] **Step 5: Commit, push, PR D.**

```bash
git add observability/base/agent-platform/grafana-dashboard-agent-factory.yaml docs/runbooks/agent-factory \
  website/content/docs/platform/ai-platform docs/superpowers/specs/2026-10-22-merge-gate-classes-design.md
git commit -m "docs(agent-factory): runbook 15 and the merge gate's declared classes"
git fetch origin && git rebase origin/main && git push -u origin feat/dead-links-declared-off
gh pr create --title "feat(agent-factory): pin v0.17.0, dead-links declared off" --body "Merge gate classes PR 5 without the monitor (docs/superpowers/specs/2026-10-22-merge-gate-classes-design.md). agent-platform v0.17.0 (the vocabulary at parity and dead-links), dead-links declared off on both repositories (MC14), the host's schedule inert while off, the factory's egress to GitHub's artifact storage, the parity suite (MC2, offline proof 11), a merge decisions panel, runbook 15. task check: exit 0."
```

## Self-review

- **Spec coverage.** MC1 (Tasks 2–3), MC2 (Task 13), MC3 (unchanged: the merger stays the only
  merge actor), MC4–MC6 (Tasks 5–11), MC14 (Tasks 8, 13). MC7–MC13 are F1 and F2 (R1). Offline
  proofs 4–7 and 13–16 are F1 and F2's. The `revert` class keeps its built-in shape (R5).
- **Placeholders.** None: the two digests and the host commit are read in Task 12, Step 1.
- **Names.** `ArmInputs.DiffRead`, `Stats`, `ClassArmedToday`; `Conditions()`; `config.Oracle`;
  `v1alpha1.Finding`; `intake.LinkFinding` (RunLore owns `intake.Finding`); `Merger.Diff`,
  `WorkflowRuns`, `RunArtifact`, `FileAt` are used under the same names in every task.
