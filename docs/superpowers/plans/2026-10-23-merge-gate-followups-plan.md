# Merge gate classes, follow-ups F1–F3: the monitor, `deps-patch` and ai-native-ref's `dead-links` lane — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every merge of a class that declares a `monitor` is watched after it lands (main's CI, the
detector's next runs, Flux on the factory's cluster, follow-up fixes for 7 days), its outcome is
written on GitHub, and follow-ups and regressions demote the class like reverts (F1, closing
agent-platform #182 on the way). Renovate's patch bumps become the `deps-patch` class: adopted,
decided on a diff the factory computes, monitored, shipped declared off and started in shadow
(F2). ai-native-ref gets its own `dead-links` detector, policy rule and schedule (F3).

**Architecture:** F1 adds no runnable. A `Done` Task whose class declares `monitor` stays on the
reconciler's books, as a revertable one already does (`revertWait`): every `monitorEvery` it reads
the signals, records them in `status.monitor`, and writes the outcome on GitHub (labels and a
`event=monitor-<n> verdict=<v>` comment). The breaker's `Merge` gains `FollowUp` and `Regression`;
`Demoted` counts them against `breaker.maxFollowUps`. F2 adds an `author: {adopt}` mode beside the
built-in `agents` mode, an adopt poller (a leader-only runnable next to the `Scheduler`) that turns
an open Renovate PR into a run-less Task in `AwaitingCI`, two diff shapes (`semver-patch`,
`semver-minor`) in a new `semverdiff` package, and `releaseAgeCheck`. F3 is YAML in ai-native-ref:
a workflow that runs the host's detector scripts from `.host/`.

**Tech Stack:** Go 1.27.3 (agent-platform; go-github v92, githubv4, controller-runtime,
`unstructured` for Flux objects), Bash, jq and Python 3 (the tally, ai-native-ref suites), promtool,
GitHub Actions, policy-bot, Renovate, Flux.

**Spec:** [merge gate classes](https://github.com/Smana/ai-native-ref/blob/main/docs/superpowers/specs/2026-10-22-merge-gate-classes-design.md)
on ai-native-ref `main` (MC1–MC14, M1–M6 as recommended): its PR 2, the rest of its PR 5, the
Renovate part of its PR 4, its PRs 6, 7 and 8; offline proofs 4–7 and 13–16; live proofs L1–L5.
**First slice:** `docs/superpowers/plans/2026-10-22-merge-gate-classes-plan.md` in
`/home/smana/Sources/agent-platform-merge-gate` (rulings R1–R26) and its ledger
`.superpowers/sdd/2026-10-22-merge-gate-classes-plan/progress.md`. Merged: agent-platform #183, #184;
cloud-native-ref #2542.

## Read before starting

| Fact | Where, read 2026-10-23 on agent-platform `origin/main` `ac62de4` (v0.20.0 plus #199, #200; #196–#198 merged), ai-native-ref `origin/main` `b3fe753` (#110–#112), cloud-native-ref `origin/main` `8fe4df99` (#2554, ai-native-ref's `HOST_REF`) |
|---|---|
| The gate | `internal/factory/reconciler/arm.go` (`DecideArm` l.80-123: `baseChecks` l.136, `not_agent_authored` before the class lookup, `maintainer_approved`, class switch, `agentEvidence` l.170, steering, `declared`, breaker, caps, shadow); `gate.go` (`Conditions()`, `onDiff`, a static key list, `oracle`; `holdout_pending` l.156) |
| After the merge | `merge.go`: `autoMerging` labels `factory/auto-merged:<class>` and moves to `Verifying`; `verifying` reverts on `MainState == FAILURE` within `verifyFor`; human merges end `Done "merged"` in `awaitingCI` (l.41) and in `prEnded` (`implement.go:707`, l.713) with `MergedAt = r.Now()`, the factory's clock, not GitHub's `mergedAt` |
| Breaker | `merge.go` `paused` (l.733) unites Tasks' merges with `githubLedger` (`ledger.go:45`: one read per process of `factory/auto-merged:<class>` and `factory/revert-of:#<n>`); `Demoted` (l.757) counts reverts only; `armedTodayOf` (l.785-803) counts every ledger merge of today. `r.label` (`ledger.go:111`) only logs a failure. The merger asks `issues: write` only when `cfg.AnyLive()` (`forge/app.go:50`, `internal/app/factory.go:362`) |
| Terminal tasks | `reconciler.go:197-219`: `revertWait` l.201, an ended task left alone (l.210) unless `revertWait`, an outbox, a span, a settle, facts or a room close is due; the revert watch's requeue l.271; `step`'s `PhaseDone` arm l.326-327. `narrateOn` (l.514-520) skips a key already queued or posted. Tasks are never deleted |
| Probe reads | `forge.Merger.WorkflowRuns`, `RunArtifact`, `FileAt` (`actions.go`); the fake's `RunArtifact` returns the stored bytes raw (`forgetest/fake.go:504-512`). `intake.ParseFindings` (`deadlinks.go:43`), and the unexported `errFindingsRefused` (l.36), `gone` (l.64), `detectorRun` (l.229) |
| Open PRs | `forge.PRSummary{Number, Author, Created}` (`forge.go:278`) from `OpenPullRequests` (one page of 100, the factory App); `intake.ScheduleMerger` (`schedule.go:59-62`) has no `Diff` |
| Task API | `Source.Kind` enum `issue;runlore;schedule;api` (`task_types.go:248`, #196), its CEL rules l.242-246 (a sponsor only on `api`, the kind never changes); `Template` enum `solo;pair;trio;investigate`; `PullRequestRef` (l.616-645) has no monitor field; `(*Task).Payer()` (`person.go:23`) charges a `github:<login>` requester as that person |
| Metrics | `fmetrics.outcomes()` = `auto_merged, human_merged, closed, reverted`; `intakeSources()` = `issue, runlore, schedule, api` (`fmetrics.go:56`); `agent_factory_merge_decisions_total{class,verdict,reason}` |
| Chart | `charts/agent-factory/templates/rbac.yaml`: two namespaced Roles, no ClusterRole; the chart has no values schema |
| Scripts | `scripts/shadow-merge-tally.sh` and its suite `scripts/shadow-merge-tally_test.sh` (a fake `gh` in a temp dir), run by `task scripts` (`taskfile.yaml:41-45`). No `scripts/tests/`, no `# requires:` harness |
| #182 | AwaitingCI's `case "wait"` (`merge.go:135`) returns `nil` forever; nothing times it out |
| Host Renovate | `.github/renovate.json`: patch+minor automerge, `minimumReleaseAge: 1 day`; never-automerge by package (agent MCP servers, the factory group, the AI pin, OpenBao, agent-sandbox, ZITADEL, Gateway API) and by file (`vm-common-helm-values-configmap.yaml`); action digests. Renovate branches are `renovate/<dep>-<major>.x`, one branch per line carrying patch **and** minor |
| Host policy | `.policy.yml`: the `renovate` rule (l.30-33) is a bare `has_author_in`; `low-risk: docs-links` l.35-87; the canonical rule l.141. `scripts/ci/check-policy-gate-coverage.sh` skips `renovate[bot]` (`u != "renovate[bot]"`, l.28) and prints `FAIL: rule '…' lacks gate paths: […]`; its suite is `scripts/ci/tests/test-policy-gate-coverage.sh`. `.github/agent-checks.yaml` l.33-34 declares the two dead-links jobs `ciOnly` |
| Host Renovate merges, 2026-08-18 → 2026-10-23 | file touches by top directory: `.github` 42, `mise.toml` 42, `flux` 37, `observability` 30, `opentofu` 23, `infrastructure` 22, `security` 18, `container-images` 10, `tooling` 7, `apps` 6, `website` 6, `scripts` 3, `crds` 2 |
| ai-native-ref | pinned at agent-platform v0.20.0 (#112: chart, factory image, broker, bridge, harness, CRD and atlas). `dead-links` is declared off on both repositories (#110; values l.49-57 and l.125-133, the latter under the comment "No schedule here yet", l.123); the host's `docs-links` and `revert` are live for the drill ("Back to {shadow: true} once the drill is over", l.37-40). Runbooks 15, 17 and 18 exist; 16 is absent, reserved by the spec for the deps-patch drill. Its `.policy.yml` has no `low-risk` rule (header l.5-6; `human-authored` l.26, the canonical rule l.31); its workflows are `ci.yaml`, `docs.yml`, `price-drift.yaml`, `release.yaml`, `ci.yaml` checking out the host at `HOST_REF` into `.host`. `test-agent-checks.py` requires every job of a `pull_request` workflow to be named by a step's `ci` or by `ciOnly` in `.github/agent-checks.yaml`. No offline gate parses the factory config with the factory's strict decoder: `test-agent-factory-pins.py` reads tags, digests and `renovate.json` only. The parity suite's `HOST` is the composite, `OWN` the working directory (this repository), whose `.host/` is the host's own checkout |

## This plan's rulings

Numbered on from the first slice's R1–R26. The first slice's rulings that changed the design hold
here unchanged: an oracle retarget equals the finding's archive snapshot exactly; an oracle class
needs `modifyOnly` and `allowedFiles`; `maxModifiedLines ≥ 2 × maxFindings`; `allowedFiles` anchored
`^…$` and matched as `^(?:p)$`; a holdout needs an exact `SUCCESS` from GitHub Actions on every run
of its name, and a truncated rollup refuses; a 100-file diff hands off as `patch_too_large`.

| # | Ruling | Why |
|---|---|---|
| R27 | **#182 is not the monitor's.** The monitor starts after a merge; #182 is a wait before one. It is fixed in F1's first PR as its own task: `merge.awaitingCIFor` (optional, 0 = no bound, today's behaviour); past it, a task still on a wait verdict goes to `AwaitingHuman` with reason `ci_wait_expired`, its narration naming the checks and statuses not reported. ai-native-ref sets `24h` for both repositories in F1's config PR | Off by default (D4), so parity holds in the release. F2 needs it: `releaseAgeCheck` and holdouts can wait on a status that never comes |
| R28 | **The monitor lives in the reconciler, on `Done` tasks** (`monitorDue`, beside `revertable`), polled every `monitorEvery` (10 min). No runnable | The reconciler is the only writer of a Task's status today; a second writer would race it. Tasks are never deleted, so 7 days on the books costs nothing but the polls |
| R29 | **Only a class that declares `monitor` is monitored.** `docs-links` and `revert` declare none and keep today's behaviour exactly | MC14 and the first slice's parity rule (R2) |
| R30 | **Every merge the monitor watches is labelled `factory/merged:<class>`**, whoever merged it (the merger, a maintainer, Renovate). The ledger reads it beside `factory/auto-merged:<class>`, and each merge's outcome labels: `factory/revert-of:#<n>` (exists), `factory/follow-up-of:#<n>` on the later PR, `factory/monitor:regression` on the merge itself | MC13: "every outcome lives on GitHub". Without it a rebuild forgets human and Renovate merges, and with them their follow-ups and regressions: offline proof 5 requires the demotion to survive every Task deleted |
| R31 | **A regression is written at once; `healthy` and `inconclusive` once every signal is final**, at the end of `followUpWindow`. A follow-up is labelled when found. The verdict comment is posted once (narration key `monitor-<n>`) | Nothing can turn a regression healthy; the breaker must see it at its first bad outcome (MC9). A healthy verdict before the follow-up window closes would be retracted later |
| R32 | **Flux regression = a Kustomization or HelmRelease whose `Ready` is `False` with `lastTransitionTime` after the source's artifact reached the merge commit**, seen at any poll within `within` | Equivalent to "Ready before, not Ready after" without storing a snapshot of every object in the Task's status. An object already failing before keeps its older transition and is not blamed |
| R33 | **"Reached the merge commit"**: the GitRepository's `status.artifact.revision` (`main@sha1:<sha>`) is the merge commit or a descendant of it (`Merger.IsAncestor`, GitHub's compare `status` `ahead` or `identical`); from `artifact.lastUpdateTime`. A suspended source, or none reached within `within` of the merge, is `inconclusive` | The design's row made exact. Flux may skip straight to a later commit |
| R34 | **The Flux reader is uncached** (`mgr.GetAPIReader()`, `unstructured`), so the ClusterRole is `get, list` on `kustomizations`, `helmreleases`, `gitrepositories`, no `watch`; rendered only when the chart's `monitor.flux.enabled` is true (default false). With no reader the Flux signal is `inconclusive` | Narrower than the design's get/list/watch: no informer is started for three cluster-wide kinds the factory reads every ten minutes at most |
| R35 | **A follow-up of merge *m*** is a PR merged after *m* within `followUpWindow` that is not *m*'s revert, not authored by one of *m*'s class `adopt` logins (Renovate's next bump of any line is the lane working), and either carries `factory/follow-up-of:#<m>` (any author: the factory writes it only through the merger; a maintainer's counts) or changes a base line whose blame, at the later PR's merge commit's first parent, is *m*'s merge commit. The changed base lines are the `-` lines of its per-file patch (`Merger.PullFiles`); blame is one GraphQL call per (later PR, file *m* also touched) | The design's rule, with "the same adopted lane" widened from one branch to the login: Renovate's branch names carry the major (`renovate/foo-1.x`), so a major bump of the same line would read as a follow-up |
| R36 | **`breaker.maxFollowUps` is optional**: 0 = follow-ups and regressions do not demote (today). ai-native-ref sets 1 (M5). Validate: `0 ≤ maxFollowUps ≤ window` | Off by default; at parity in the release |
| R37 | **The oracle signal**: the targets the PR wrote are its findings' archives. A later scheduled run (as `detectorRun` defines it, created after the merge) that reports any finding's file with its archive, or its original URL, as gone is `regression`; `oracleRuns` such runs all clean is `healthy`; a failed run or fewer than `oracleRuns` within 72 h is `inconclusive` | The design's row, on the probe's own readers (R1: no second artifact reader) |
| R38 | **The tally's bound is computed by `python3`** (bisection on the binomial CDF, one-sided 95% Clopper-Pearson), called from `shadow-merge-tally.sh --outcomes`; `--since <YYYY-MM-DD>` is the width's start | jq has no beta quantile; the script already needs gh and jq, python3 is on every runner |
| R39 | **An adopted Task**: `Source.Kind: adopt` (CRD enum), `Source.Ref: <repo>#<n>`, no template, no room, no runs. `received` skips triage and the daily task cap (no run, no spend) and moves it to `AwaitingCI` with `status.pullRequest` read from the PR. The adopt poller bounds them: at most 10 non-terminal adopted Tasks per repository | MC10. Design: "a Task with no runs (template `none`)": no enum value is needed when the field is simply empty |
| R40 | **`author: {adopt}` evidence** replaces `not_agent_authored` and `agentEvidence`: the PR's author is one of `logins` (`not_adopted_author`), its head branch matches one of `branches` anchored `^(?:p)$` (`branch_not_allowed`), every commit's author login is one of `logins`, and every push since the branch's latest creation is one of `logins`' (`BranchActivity` on the head branch, `human_commit_in_pr`) | The design names the commit authors, which are a claim (`forge.Commit.Author`: "never proof"); the pusher is GitHub's record, as #76 ruled for agent branches |
| R41 | **`semver-patch`, exactly** (package `internal/factory/semverdiff`): every changed file is `modified` and has as many lines on both sides; each differing line pair is identical once its version tokens (`v?MAJOR.MINOR.PATCH`, optionally followed by `-pre` or `+build`) and digests (`sha256:` and 64 hex) are masked; the pair holds the same number of each; at least one version moves, each moved version keeps MAJOR.MINOR and raises PATCH, neither side carries a pre-release or build suffix; a digest may change only on a line where a version moved. `semver-minor`: the same, each moved version keeping MAJOR and raising MINOR, or MINOR equal and PATCH raised, MAJOR ≥ 1 or not | MC11 made checkable. A lockfile fails it by construction (integrity and resolved lines carry no version token); so does a digest-only update |
| R42 | **`deps-patch` at w0 declares `allowedFiles: ['^(apps|flux|infrastructure|observability|security|tooling)/.+\.ya?ml$']` and `modifyOnly: true`**, beside the design's protected files, sizes, shape, release age and cap | Fail closed: an allow list of what Flux deploys, because Flux is what the monitor watches. `mise.toml` (42 touches, CI tools nothing deploys), `container-images/`, `opentofu/`, `.github/`, lockfiles and `crds/` stay with Renovate and humans, as the design's w0 already says of `mise.toml`. 120 of the 251 file touches since 2026-08-18 are in the allow list |
| R43 | **`semver-minor` can never be live** (Validate). `deps-minor: {shadow: true}` observes; going live is M2's (b) or (c), a code change and an owner's decision | M2's recommended (a) |
| R44 | **`releaseAgeCheck`** holds on a commit status of that name posted by one of the class's `adopt` logins in state `SUCCESS`; absent or `PENDING` waits (`release_age_pending`, bounded by R27); anything else refuses (`release_age_red`). It needs `author.adopt` | The design's row; a status anyone else posted is no age |
| R45 | **An adopted Task never runs an agent**: `ci_red` hands off (`ci_red`; `renovate-red` takes the PR after 24 h), a maintainer's Request changes hands off, `AwaitingHuman` never reminds nor stale-closes Renovate's PR, and a new head there returns it to `AwaitingCI` (Renovate's rebase), uncounted against `maxGateReentries` | A fix run on Renovate's branch is `renovate-red`'s job (MC10); closing Renovate's PR as stale would fight Renovate |
| R46 | **An adopted shadow decision narrates `WouldArmAdopted`**: the same `event=would-arm-<n> head=<sha>` marker and "would auto-merge: `<class>`" text the tally reads, without "verdict approve". The tally gains `--author` (default `app/ogenki-agents`; `app/renovate` for `deps-patch`) | The tally's jq reads the marker and the class text; the body must not claim a verdict |
| R47 | **The host's `renovate` rule gains the gate list** in `no_changed_files` (design PR 4's part), in F2's host PR, before any shadow decision | Without it, a Renovate PR on a gate path reads policy SUCCESS and a shadow decision would count it |
| R48 | **At the live switch the `renovate` rule stops approving the lane's PRs alone** (owner decision OD-F2-1): recommended, it gains `requires: {count: 1, users: [Smana]}`, so a lane PR's SUCCESS comes only from `low-risk: deps-patch`. Renovate's own merges bypass the ruleset and are unaffected | The `or` of rules means the `renovate` rule, approving every Renovate PR off the gate list, makes policy-bot no independent check of the lane's fence (MC2). A gap in the design |
| R49 | **ai-native-ref's `dead-links.yaml` runs the host's three scripts from `.host/`** (a checkout of cloud-native-ref at `HOST_REF`), and reads `LYCHEE_VERSION` and `LYCHEE_SHA256` from the host's workflow with `yq` at run time | AGENTS.md: host scripts are used from the host profile, never vendored; the pin is the host's too |
| R50 | **ai-native-ref gets `low-risk: dead-links`** in its `.policy.yml` (the first slice's R22), the class's fence and `has_workflow_result` on `ci.yaml`. **Owner to confirm** (OD-F3-1): it is the repository's first `low-risk` rule. A policy SUCCESS merges nothing: the merger merges, and only a live class | Without it no shadow decision there reaches `would-arm` (v2 D9); the design's "shadow on both" needs it |

## Amendments (pre-flight 2026-10-23)

The pre-flight scan (`.superpowers/sdd/2026-10-23-merge-gate-followups-plan/preflight.md`) found 1 B,
16 M and 19 L; every proposed ruling is adopted and written into the task it affects. Where one
changes a ruling above, the scan's ruling wins, as marked.

- **B1** (labels need `issues: write`, which the merger asks only while a class is live): Task 2
  adds `config.AnyMonitored()` and passes `AnyLive() || AnyMonitored()` as `MergerPermissions`'
  `label`; the owner check lands in runbook 15 (Tasks 11, 23), runbook 16 (Task 20), the live
  proofs' prerequisites and the owner decisions.
- **M1** (`factory/merged` would fill the daily cap): Task 3, `Merge.Auto` and `armedTodayOf`
  counting only `Auto`, with a case in `merge_gate_test.go`.
- **M2** (the control-issue copies dedup away): Task 5, their own narration keys and a test.
- **M3** (`config.Author` before Task 12): Task 5 skips only the revert; Task 16 adds the
  adopt-login skip to `monitor.go`. R35 is unchanged, delivered in two steps.
- **M4** (`monitorDue` is a method): Task 5's test.
- **M5** (the fake client's RESTMapper): Task 7.
- **M6** (typed-nil `*fakeFlux`): Task 8's `fluxRig`.
- **M7** (unexported intake helpers, no `zipOf`): Task 8 exports `intake.DetectorRun`, `intake.Gone`,
  `intake.ErrFindingsRefused`, drops the conditional package move, and passes JSON bytes.
- **M8** (a second tally suite): Tasks 6 and 17 extend `scripts/shadow-merge-tally_test.sh`
  (fixtures `scripts/testdata/tally/`); the file structure table follows.
- **M9** (promtool's exact labels): Task 10.
- **M10** (the enum drops `api`): Task 12, `issue;runlore;schedule;api;adopt` and an envtest.
- **M11** (the oracle case's message): Task 12, `holdoutChecks: [h]` in the case.
- **M12** (an adopted Task reads a room named ""): Task 15.
- **M13** (no array in the coverage check): Task 18 Step 1.
- **M14** (`reached()` mismeasures): Task 19, dependency references only, with a fixture test.
- **M15** (`test-agent-checks.py`): Task 22 modifies `.github/agent-checks.yaml`.
- **M16** (no strict parse offline): Tasks 10, 19 and 23 each run a one-off strict `Parse` from the
  pinned tag's worktree; Task 23's claim that the pins suite parses the config is removed.
- **L1**: "Read before starting", refreshed. **L2**: Task 20 keeps `16-deps-patch.md`.
  **L3**: Task 11's anchors. **L4**: Task 22's probe exports `RUNNER_TEMP` and `GITHUB_PATH`.
  **L5**: Task 19 (`gitrepo-gateway-api` only; `reached()` reads `.host/`).
  **L6**: Task 5 reads GitHub's `mergedAt` for every window; where R31 and R35 say `MergedAt`,
  this wins. **L7**: Tasks 7 and 8, only the watched source's objects; this narrows R32, and wins.
  **L8**: Task 15 (`declared`). **L9**: Task 16 (`ScheduleMerger` gains `Diff`).
  **L10**, **L11**: Task 6. **L12**: Task 21 Step 3. **L13**: Task 18. **L14**: Task 5.
  **L15**: Task 15 (`ci_cancelled`) and Task 16 (`Payer()`). **L16**: Tasks 10 and 23.
  **L17**: Task 23's PR body. **L18**: Task 17's docs. **L19**: Self-review, "Not planned".
  L16 also orders PR L after PR G: the strict decoder at an older pin refuses `monitor`.
- **Ledger rulings.** PRs I and L merge on green (GitHub-side only; OD-F2-1 and OD-F3-1
  approved): Tasks 18 and 23. PR K stays held: Task 21. B1's App permission is an owner action for
  the gcp-0 session: the live proofs and the owner decisions.

## Global Constraints

- **Repositories and branches.** agent-platform: this plan's worktree is
  `/home/smana/Sources/agent-platform-merge-gate-f` (branch `feat/merge-gate-followups`, the plan
  only). Each PR in its own sibling worktree from a freshly fetched `origin/main`:
  `git -C /home/smana/Sources/agent-platform worktree add /home/smana/Sources/agent-platform-<topic> -b <branch> origin/main`.
  Branches: PR E `feat/merge-outcomes`, PR F `feat/merge-monitor-signals`, PR H `feat/deps-patch`.
  cloud-native-ref: PR I `feat/renovate-rule-gate-list`, PR K `feat/deps-patch-live` (held).
  ai-native-ref: PR G `feat/merge-monitor-declared`, PR J `feat/deps-patch-declared-off`, PR L
  `feat/dead-links-own-lane`, each a worktree from `origin/main`, `mise trust mise.toml` once.
  Rebase onto a freshly fetched `origin/main` before every push. Never push this plan's branch.
- **Offline first (D4).** Each PR merges on its offline proof. Live proofs wait for the next gcp-0
  session ([below](#next-gcp-0-session-live-proofs)). Every new key, signal and class ships **off by
  default**; a class's first session commit sets `shadow: true`, never `live: true`.
- **The evidence.** agent-platform: `task check` exits 0 (store tests need Docker: start the daemon
  for the final run of each PR only; it is off by default here), `task lint` 0 issues, envtest with
  `KUBEBUILDER_ASSETS=/home/smana/.local/share/kubebuilder-envtest/k8s/1.36.2-linux-amd64` and
  `-p 1`, `task crd:check` and `task chart:check` after an API or chart change. ai-native-ref:
  `task check` through `$SCRATCH/check.sh <worktree>` (logs to `$SCRATCH/check-<dir>.log`, ends
  `exit=N`; it needs a `python3` with PyYAML), and PRs G, J and L quote a one-off strict parse of
  the factory config at their pin (M16, Task 10 Step 5). cloud-native-ref: `scripts/ci/tests/run.sh`,
  `actionlint`, `shellcheck`, `check-policy-gate-coverage.sh`, `npx --yes --package renovate -- renovate-config-validator`.
- **agent-platform conventions** (its AGENTS.md): `// SPDX-License-Identifier: Apache-2.0` on every
  `.go` file; doc comments on exported symbols; lint through `task lint`; `task crd:gen` after an
  API change, committing what it writes.
- **Commits** are conventional, in English, with **no trailers**. PR descriptions in English.
- **Never vendor host scripts in ai-native-ref** (R49).
- **Constitution** (ai-native-ref): F1 adds one ClusterRole (read-only, three kinds) and nothing
  else to the factory's RBAC; F2 and F3 add no egress (Renovate PRs are read through
  `api.github.com`; the artifact storage FQDN is the first slice's R19).
- **Verbatim from the spec:**
  - MC7: "A monitor per merge watches main's CI (as today), the detector's next runs and Flux on the
    factory's cluster, then records `healthy`, `regression` or `inconclusive` … It never reverts on
    its own: main red within `verifyFor` still opens the revert (§6.4), nothing else does".
  - MC8: "`breaker.maxFollowUps` beside `maxReverts`, both 1 in 10".
  - MC10: "The factory authors no bump: an adopt poller turns an eligible open Renovate PR into a
    Task with no run, decided and merged like any class".
  - MC11: "`semver-patch` is computed from the diff, not from Renovate's label … The
    never-automerge list is protected, and a parity suite reads it from the host's `renovate.json`".
  - MC12: "Renovate keeps auto-merging while the class is in shadow; the factory records its
    decision with `head=` and monitors the merge".

## Pull requests, in order

| PR | Repository | Content | Depends on | Tasks |
|---|---|---|---|---|
| E | agent-platform | #182's bound; `maxFollowUps`; the outcome labels and the ledger's reads; `monitor` key; the monitor with its main-CI and follow-up signals; metrics; `shadow-merge-tally.sh --outcomes` | — | 1–6 |
| F | agent-platform | The oracle and Flux signals, `IsAncestor`, the Flux reader, the chart's ClusterRole; docs; **release** (next minor) | E | 7–9 |
| G | ai-native-ref | Pin F's release; `awaitingCIFor: 24h`, `maxFollowUps: 1`; `dead-links`' `monitor` on the host (ai-native-ref's lands in PR L, L16); `monitor.flux.enabled`; `AgentFactoryMergeRegression` and its promtool suite; runbook 15's monitor step; docs | F released (the combined pin PR is merged: #110) | 10–11 |
| H | agent-platform | `Source.Kind: adopt`; `author.adopt`; `semverdiff`; `diffShape: semver-patch, semver-minor`; `releaseAgeCheck`; the adopt poller; the adopted lifecycle; the tally's `--author`; the Renovate replay; docs; **release** | E (F for the release order) | 12–17 |
| I | cloud-native-ref | The `renovate` rule gains the gate list (R47); the coverage check reads the `renovate` rule. **Merges on green** (GitHub-side only, ledger) | — (before J) | 18 |
| J | ai-native-ref | Pin H's release; `deps-patch` and `deps-minor` declared off on the host; the parity suite reads `renovate.json` (offline proof 16); runbook 16; docs | H released, I merged, G merged | 19–20 |
| K | cloud-native-ref | **Held until the shadow evidence and the owner's go:** `renovate.json` patch `automerge: false` and `minimumReleaseAge: 3 days` in the lane's files; `low-risk: deps-patch`; the `renovate` rule's `requires` (R48); the revert rule for the lane's files | J; L3's evidence; OD-F2-1 | 21 |
| L | ai-native-ref | `dead-links.yaml` from the host's scripts and its `ciOnly` rows; `low-risk: dead-links` (R50); the schedule entry; `dead-links`' `monitor` on ai-native-ref (L16). **Merges on green** (GitHub-side only, OD-F3-1 approved, ledger) | the combined pin PR merged (#110: met); G merged (the monitor key) | 22–23 |

F3 (PR L) is independent of F1 and F2 except for one key: ai-native-ref's `dead-links` monitor
(L16) needs F's release pinned, so PR L lands after PR G. Both edit `helm-values-configmap.yaml`.
PR K stays held (Task 21).

## File structure

| File | PR | Change | Responsibility |
|---|---|---|---|
| `internal/factory/config/config.go` | E | modify | `Merge.AwaitingCIFor`, `Breaker.MaxFollowUps`, their validation; `AnyMonitored()` (B1) |
| `internal/factory/forge/app.go` | E | modify | `MergerPermissions`' doc: labels written once a class is live or monitored (B1) |
| `internal/factory/config/classes.go` | E, H | modify | `Class.Monitor` (`Monitor{OracleRuns, Flux *FluxWatch, FollowUpWindow}`); H: `Class.Author`, `ReleaseAgeCheck`, shapes `semver-patch`, `semver-minor`, their validation |
| `api/factory/v1alpha1/task_types.go` (+ CRDs) | E, H | modify | E: `status.monitor` (`MonitorStatus`); H: `Source.Kind` enum gains `adopt` after `api` |
| `api/factory/v1alpha1/person.go` | H | modify | `Payer()` is `""` for an adopted task (L15) |
| `internal/factory/reconciler/adopt_envtest_test.go` | H | create | An `adopt` Task meets the Source CEL rules (M10) |
| `internal/factory/forge/forge.go`, `github.go`, `absent.go`, `forgetest/fake.go` | E, F, H | modify | E: `MergedPull`, `FilePatch`, `BlameRange`, `Merger.MergedSince`, `PullFiles`, `BlameAtParent`; F: `IsAncestor`; H: `PRSummary.HeadRef`, `HeadSHA` |
| `internal/factory/reconciler/ledger.go` | E | modify | `Merge.FollowUp`, `Regression`, `Auto`; labels; `githubLedger` reads them |
| `internal/factory/reconciler/merge.go` | E, H | modify | E: `Demoted` with follow-ups, `armedTodayOf` on `Auto` merges only (M1), #182's bound; H: adopt evidence inputs, adopted `ci_red` and `ci_cancelled`, no room read for an adopted task (M12) |
| `internal/factory/reconciler/monitor.go` | E, F, H | create, modify | `monitorDue`, `monitor`, `followUps`, `mainSignal`; F: `oracleSignal`, `fluxSignal`, `MonitorVerdict`; H: the adopt-login skip in `followUps` (M3) |
| `internal/factory/intake/deadlinks.go` | F | modify | Exports `DetectorRun`, `Gone`, `ErrFindingsRefused` for the oracle signal (M7) |
| `internal/factory/reconciler/reconciler.go` | E, H | modify | E: `monitorWait` in `Reconcile`, `PhaseDone` in `step`; H: `received` for `adopt`, `Flux` field |
| `internal/factory/reconciler/arm.go`, `gate.go` | H | modify | `adoptEvidence`, `releaseAgeCheck`, the semver shapes |
| `internal/factory/reconciler/implement.go` | H | modify | `awaitingHuman` for adopted tasks |
| `internal/factory/fluxwatch/fluxwatch.go` | F | create | `Reader` on `client.Reader`: the source's revision, the objects turned not Ready |
| `internal/factory/semverdiff/semverdiff.go` | H | create | `Patch`, `Minor` |
| `internal/factory/intake/adopt.go` | H | create | `Adopter`, the adopt poller |
| `internal/factory/fmetrics/fmetrics.go` | E | modify | `outcome="follow_up"`, `agent_factory_monitor_verdicts_total` |
| `internal/factory/narrate/narrate.go`, `internal/summary/waiting.go` | E, H | modify | `MonitorVerdict`, `ci_wait_expired`; H: `WouldArmAdopted`, the adopt reasons |
| `internal/app/factory.go` | E, F, H | modify | E: the merger's `label` permission (B1); F: the Flux reader; H: the `Adopter` |
| `internal/factory/intake/schedule.go` | H | modify | `ScheduleMerger` gains `Diff` (L9) |
| `charts/agent-factory/templates/rbac.yaml`, `values.yaml`, `tests/render.sh` | F | modify | The ClusterRole behind `monitor.flux.enabled` |
| `scripts/shadow-merge-tally.sh`, `scripts/clopper-pearson.py`, `scripts/shadow-merge-tally_test.sh`, `scripts/testdata/tally/` | E, H | modify, create | `--outcomes`, `--since`; H: `--author`. The existing suite is extended, run by `task scripts` (M8) |
| `hack/renovate-replay.sh`, `internal/factory/semverdiff/testdata/` | H | create | Offline proof 14 |
| `docs/factory.md` | E, F, H | modify | Monitor, outcomes, adopt, shapes, reasons |
| host `.policy.yml` | I, K | modify | R47; K: `low-risk: deps-patch`, R48, the lane's revert rule |
| host `scripts/ci/check-policy-gate-coverage.sh`, `scripts/ci/tests/test-policy-gate-coverage.sh` | I | modify | The `renovate` rule is held to the gate list, with a fixture case (M13, L13) |
| host `.github/renovate.json` | K | modify | The lane's patch rule |
| ai-native-ref `tooling/base/agent-factory/helm-values-configmap.yaml`, pins, `HOST_REF` | G, J, L | modify | Pins, declarations, schedule |
| ai-native-ref `observability/base/agent-platform/vmrule-agent-factory.yaml`, `scripts/ci/tests/test-agent-merge-regression-alert.sh` | G | modify, create | Offline proof 13 |
| ai-native-ref `scripts/ci/tests/test-agent-merge-gate-parity.py` | J, L | modify | Offline proofs 11 and 16 |
| ai-native-ref `.github/workflows/dead-links.yaml`, `.github/agent-checks.yaml`, `.policy.yml` | L | create, modify | F3; the two jobs' `ciOnly` rows (M15) |
| ai-native-ref `docs/runbooks/agent-factory/15-dead-links.md`, `16-deps-patch.md`, README, `factory.md`, `status.md`, the spec | G, J, L | modify, create | Drills and docs |

---

## F1, PR E — agent-platform: outcomes, follow-ups and the monitor's first two signals

### Task 1: AwaitingCI's bound (#182)

**Files:**
- Modify: `internal/factory/config/config.go` (`Merge.AwaitingCIFor`), `internal/factory/reconciler/merge.go` (`case "wait"`), `internal/factory/narrate/narrate.go`, `internal/summary/waiting.go`
- Test: `internal/factory/reconciler/cibound_test.go`, `internal/factory/config/config_test.go`

**Interfaces:**
- Produces: `config.Merge.AwaitingCIFor Duration` (`json:"awaitingCIFor,omitempty"`, 0 = no bound), reason `ci_wait_expired`, `narrate.ChecksNeverReported(t, missing []string) Event`, `reconciler.unreported(in ArmInputs, cl config.Class) []string`.

- [ ] **Step 1: Write the failing tests.** `cibound_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"
	"time"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
)

// #182: a wait verdict past merge.awaitingCIFor hands the task to a human, naming what never
// reported; inside the bound, or with no bound (0, the default), it keeps waiting.
func TestAwaitingCIIsBounded(t *testing.T) {
	for name, c := range map[string]struct {
		bound, since  time.Duration
		phase, reason string
	}{
		"no bound, a week waiting": {0, 7 * 24 * time.Hour, v1alpha1.PhaseAwaitingCI, "holdout_pending"},
		"inside the bound":         {24 * time.Hour, 23 * time.Hour, v1alpha1.PhaseAwaitingCI, "holdout_pending"},
		"past the bound":           {24 * time.Hour, 25 * time.Hour, v1alpha1.PhaseAwaitingHuman, "ci_wait_expired"},
	} {
		t.Run(name, func(t *testing.T) {
			g := declaredRig(t)
			g.r.Cfg.Merge.AwaitingCIFor.Duration = c.bound
			g.f.SetChecks(12, green("SUCCESS")) // the holdout never reports
			g.setPhaseSince(t, "3buqdlot", now.Add(-c.since))
			tk := g.reconcile(t, "3buqdlot", 1)
			if tk.Status.Phase != c.phase || tk.Status.Reason != c.reason {
				t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
			}
			if c.reason == "ci_wait_expired" && !contains(g.f.Comments(12), "External links 🔗") {
				t.Fatalf("the hand-off names the check that never reported: %q", g.f.Comments(12))
			}
		})
	}
}

// ci_pending is a wait like any other: a required check that never reports is bounded too.
func TestAPendingRequiredCheckIsBounded(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.r.Cfg.Merge.AwaitingCIFor.Duration = time.Hour
	checks := green("SUCCESS")
	checks.Runs = checks.Runs[1:] // one required check missing
	g.f.SetChecks(12, checks)
	g.setPhaseSince(t, "3buqdlot", now.Add(-2*time.Hour))
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Reason != "ci_wait_expired" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
}
```

`setPhaseSince` is a new rig helper in `reconciler_test.go` (get the Task, set
`Status.PhaseSince = &metav1.Time{Time: at}`, `Status().Update`); `contains` is a substring test
over the comments. In `config_test.go`, add a case to the existing bad-config table:
`merge: {..., awaitingCIFor: -1m}` fails with `merge.awaitingCIFor must be 0 (no bound) or positive`.

- [ ] **Step 2: Run them.**
  `ulimit -v 4000000; timeout 300 go test ./internal/factory/reconciler/ -run 'TestAwaitingCIIsBounded|TestAPendingRequiredCheckIsBounded' -p 1`
  Expected: FAIL, `g.r.Cfg.Merge.AwaitingCIFor undefined`.

- [ ] **Step 3: Implement.** `config.go`, in `Merge` after `RevertWindow`:

```go
	// AwaitingCIFor bounds a pull request's stay in AwaitingCI on a wait verdict (#182): past it,
	// a human is told which checks never reported. 0 is no bound, the behaviour before it.
	AwaitingCIFor Duration `json:"awaitingCIFor,omitempty"`
```

and in `validateRepository`: `if r.Merge.AwaitingCIFor.Duration < 0 { bad("merge.awaitingCIFor must be 0 (no bound) or positive") }`.

`merge.go`, the `case "wait":` arm becomes:

```go
	case "wait":
		if b := r.repo(t).Merge.AwaitingCIFor.Duration; b > 0 && t.Status.PhaseSince != nil && r.Now().Sub(t.Status.PhaseSince.Time) >= b {
			// #182: a check that never reports (a typo in a holdout, a workflow not set up) must not
			// hold the task silently. Nothing merged: the hand-off says what is missing.
			t.Status.PullRequest.HeadSHA = pr.HeadSHA
			r.to(t, v1alpha1.PhaseAwaitingHuman, "ci_wait_expired")
			narrateOn(t, pr.Number, narrate.ChecksNeverReported(t, unreported(in, gateOf(r.repo(t), t.Spec.PredictedClass))))
			return nil
		}
		t.Status.Reason = d.Reason
		return nil
```

`unreported` (in `gate.go`): the required checks whose state is not `SUCCESS`, then the class's
holdouts not `SUCCESS` with conclusion `SUCCESS`, in config order, each once; Task 15 adds the
release-age status. `ci_wait_expired` is not in `reenter`'s transient list: a human decides.
`narrate.go`: `ChecksNeverReported` (key `"ci-wait-expired-" + head`, body "Agent factory task `%s`
waited %s for checks that never reported: %s. Nothing was merged; a maintainer decides."), and
`"ci_wait_expired"` in the reasons table with "a check it waits on never reported within
merge.awaitingCIFor". `waiting.go`: the same reason, prose only (it is no wait).

- [ ] **Step 4: Run the packages.**
  `ulimit -v 4000000; timeout 900 go test ./internal/factory/config/ ./internal/factory/reconciler/ ./internal/factory/narrate/ ./internal/summary/ -p 1`
  Expected: `ok` four times; `arm_test.go`, `merge_test.go`, `ledger_test.go` unchanged.

- [ ] **Step 5: Commit.**

```bash
git add internal/factory/config internal/factory/reconciler internal/factory/narrate internal/summary
git commit -m "feat(factory): bound the wait in AwaitingCI, off by default (#182)"
```

### Task 2: `maxFollowUps` and the class's `monitor` key

**Files:**
- Modify: `internal/factory/config/config.go` (`Breaker.MaxFollowUps`, `AnyMonitored`), `internal/factory/config/classes.go`, `internal/app/factory.go` (the merger's `MergerPermissions` call, l.362), `internal/factory/forge/app.go` (`MergerPermissions`' doc comment)
- Test: `internal/factory/config/classes_test.go`

**Interfaces:**
- Produces: `config.Breaker.MaxFollowUps int` (`json:"maxFollowUps,omitempty"`); `config.Class.Monitor *config.Monitor`; `config.Monitor{OracleRuns int; Flux *config.FluxWatch; FollowUpWindow Duration}`; `config.FluxWatch{Source string; Within Duration}`; `(config.FluxWatch).Namespace() string`, `.Name() string`; `(*config.Repository).AnyMonitored() bool`, `(*config.Config).AnyMonitored() bool` (B1).

- [ ] **Step 1: Write the failing tests** (add to `classes_test.go`):

```go
// MC7, MC8: a class declares its monitor; the breaker counts follow-ups when maxFollowUps > 0.
func TestMonitorParses(t *testing.T) {
	c, err := Parse([]byte(withClasses(t, "docs-links: {}\n  revert: {}\n  "+deadLinks[:len(deadLinks)-1]+
		", oracle: {probe: dead-links, workflow: dead-links.yaml, maxFindings: 10}, modifyOnly: true,"+
		" monitor: {oracleRuns: 2, followUpWindow: 168h, flux: {source: flux-system/flux-system, within: 1h}}}")))
	if err != nil {
		t.Fatal(err)
	}
	m := c.RepositoryFor("Smana/cloud-native-ref").Classes["dead-links"].Monitor
	if m == nil || m.OracleRuns != 2 || m.FollowUpWindow.Duration != 168*time.Hour || m.Flux.Namespace() != "flux-system" ||
		m.Flux.Name() != "flux-system" || m.Flux.Within.Duration != time.Hour {
		t.Fatalf("%+v", m)
	}
}

func TestBadMonitorFails(t *testing.T) {
	for name, c := range map[string]struct{ classes, err string }{
		"no window":              {"docs-links: {}\n  revert: {}\n  x: {monitor: {}}", "class x: monitor.followUpWindow must be 1h..720h"},
		"oracle runs, no oracle": {"docs-links: {}\n  revert: {}\n  x: {monitor: {oracleRuns: 2, followUpWindow: 168h}}", "class x: monitor.oracleRuns needs an oracle"},
		"too many runs":          {"docs-links: {}\n  revert: {}\n  x: {monitor: {oracleRuns: 9, followUpWindow: 168h}}", "class x: monitor.oracleRuns must be 0..5"},
		"a bad source":           {"docs-links: {}\n  revert: {}\n  x: {monitor: {followUpWindow: 168h, flux: {source: flux-system, within: 1h}}}", `class x: monitor.flux.source "flux-system" is not namespace/name`},
		"no within":              {"docs-links: {}\n  revert: {}\n  x: {monitor: {followUpWindow: 168h, flux: {source: a/b}}}", "class x: monitor.flux.within must be 5m..6h"},
		"an unknown key":         {"docs-links: {}\n  revert: {}\n  x: {monitor: {followUps: 1}}", `unknown field "followUps"`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(withClasses(t, c.classes))); err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("got %v, want %q", err, c.err)
			}
		})
	}
}
```

and in `config_test.go`'s bad table: `breaker: {window: 10, maxReverts: 1, maxFollowUps: 11}` fails
with `merge.breaker needs 0 ≤ maxFollowUps ≤ window (MC8)`; a good case with `maxFollowUps: 1` parses.

B1: the monitor's labels (`factory/merged`, `follow-up-of`, `monitor:regression`) are written in
shadow, where the tally's evidence is gathered, so the merger must ask `issues: write` whenever a
class declares a monitor, not only once one is live. Add to `classes_test.go`:

```go
// B1: a monitored class needs the merger's label permission even while no class is live.
func TestAnyMonitored(t *testing.T) {
	c, err := Parse([]byte(withClasses(t, "docs-links: {}\n  revert: {}\n  x: {monitor: {followUpWindow: 168h}}")))
	if err != nil {
		t.Fatal(err)
	}
	if !c.AnyMonitored() || c.AnyLive() {
		t.Fatalf("monitored %v, live %v", c.AnyMonitored(), c.AnyLive())
	}
	if c, err = Parse([]byte(withClasses(t, "docs-links: {}\n  revert: {}"))); err != nil || c.AnyMonitored() {
		t.Fatalf("no monitor declared: %v %v", c.AnyMonitored(), err)
	}
}
```

- [ ] **Step 2: Run them.** `ulimit -v 4000000; timeout 300 go test ./internal/factory/config/ -run 'Monitor|Breaker' -p 1` — Expected: FAIL, `unknown field "monitor"`.

- [ ] **Step 3: Implement.** `classes.go`:

```go
	// Monitor watches each merge of the class after it lands (merge gate classes MC7): main's CI
	// for every merge, the detector's next runs for an oracle class, Flux on the factory's cluster,
	// and follow-up fixes for FollowUpWindow. Absent: no monitor, as before.
	Monitor *Monitor `json:"monitor,omitempty"`
```

```go
// Monitor is a class's post-merge watch (MC7).
type Monitor struct {
	// OracleRuns is how many scheduled detector runs after the merge must report none of the
	// targets the pull request wrote; 0 reads none. Needs an oracle.
	OracleRuns int `json:"oracleRuns,omitempty"`
	// Flux watches the factory's cluster once its source holds the merge commit.
	Flux *FluxWatch `json:"flux,omitempty"`
	// FollowUpWindow is how long later merges are read for a fix of this one (MC8), and when the
	// monitor's verdict is final.
	FollowUpWindow Duration `json:"followUpWindow"`
}

// FluxWatch names the GitRepository that delivers the repository to the factory's cluster, and
// how long after it reaches the merge commit a Kustomization or HelmRelease turning not Ready is
// blamed on the merge.
type FluxWatch struct {
	Source string   `json:"source"` // namespace/name
	Within Duration `json:"within"`
}

// Namespace and Name split Source.
func (f FluxWatch) Namespace() string { ns, _, _ := strings.Cut(f.Source, "/"); return ns }
func (f FluxWatch) Name() string      { _, n, _ := strings.Cut(f.Source, "/"); return n }

var sourceRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?/[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
```

`validateClass` gains:

```go
	if m := cl.Monitor; m != nil {
		if w := m.FollowUpWindow.Duration; w < time.Hour || w > 720*time.Hour {
			bad("class %s: monitor.followUpWindow must be 1h..720h", name)
		}
		switch {
		case m.OracleRuns < 0 || m.OracleRuns > 5:
			bad("class %s: monitor.oracleRuns must be 0..5", name)
		case m.OracleRuns > 0 && cl.Oracle == nil:
			bad("class %s: monitor.oracleRuns needs an oracle", name)
		}
		if f := m.Flux; f != nil {
			if !sourceRE.MatchString(f.Source) {
				bad("class %s: monitor.flux.source %q is not namespace/name", name, f.Source)
			}
			if f.Within.Duration < 5*time.Minute || f.Within.Duration > 6*time.Hour {
				bad("class %s: monitor.flux.within must be 5m..6h", name)
			}
		}
	}
```

`config.go`: `Breaker` gains `MaxFollowUps int \`json:"maxFollowUps,omitempty"\`` with the comment
"follow-up fixes and monitor regressions in the last Window merges that demote a class (MC8); 0
counts none", and the check `if b := r.Merge.Breaker; b.MaxFollowUps < 0 || b.MaxFollowUps > b.Window { bad("merge.breaker needs 0 ≤ maxFollowUps ≤ window (MC8)") }`.

`config.go`, beside `AnyLive` (l.227 and l.296), the same two shapes:

```go
// AnyMonitored reports whether a class of the repository declares a monitor, whose outcome
// labels the merger writes in shadow too (merge gate R30).
func (r *Repository) AnyMonitored() bool {
	for _, cl := range r.Classes {
		if cl.Monitor != nil {
			return true
		}
	}
	return false
}

// AnyMonitored reports whether a class of any repository declares a monitor, so the merger App
// labels its merges' outcomes (merge gate R30, MC13) before any class is live.
func (c *Config) AnyMonitored() bool {
	return slices.ContainsFunc(c.Repos(), func(r Repository) bool { return r.AnyMonitored() })
}
```

`internal/app/factory.go:362`: `forge.MergerPermissions(cfg.AnyCheckReruns(), cfg.AnyLive())`
becomes `forge.MergerPermissions(cfg.AnyCheckReruns(), cfg.AnyLive() || cfg.AnyMonitored())`.
`forge/app.go`, `MergerPermissions`' comment: "label adds issues: write for the breaker's labels
(v2 D1) and the monitor's outcome labels (merge gate R30), written once a class is live or
declares a monitor: an App with neither keeps minting without it, and still reads them". The
installation must then grant `issues: write` on both repositories: runbooks 15 and 16 check it
(Tasks 11, 20, 23), an owner action before the gcp-0 session.

- [ ] **Step 4: Run.** `ulimit -v 4000000; timeout 300 go test ./internal/factory/config/ ./internal/factory/forge/ ./internal/app/ -p 1` — Expected: `ok` three times.

- [ ] **Step 5: Commit.**

```bash
git add internal/factory/config internal/factory/forge/app.go internal/app/factory.go
git commit -m "feat(config): a class's monitor and the breaker's follow-ups, off by default"
```

### Task 3: The outcome labels, the ledger and `Demoted`

**Files:**
- Modify: `internal/factory/reconciler/ledger.go`, `internal/factory/reconciler/merge.go` (`Demoted`, `paused`, `armedTodayOf`)
- Test: `internal/factory/reconciler/outcomes_test.go`, `internal/factory/reconciler/merge_gate_test.go` (M1)

**Interfaces:**
- Produces: `MergedLabel(class string) string` = `factory/merged:<class>`; `FollowUpOfLabel(n int) string` = `factory/follow-up-of:#<n>`; `const RegressionLabel = "factory/monitor:regression"`; `Merge{Number int; At time.Time; Reverted, FollowUp, Regression, Auto bool}`; `Demoted(ms []Merge, b config.Breaker) (bool, int)` unchanged in signature, the int still the reverts.

- [ ] **Step 1: Write the failing tests** (offline proof 5):

```go
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"
	"time"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

func tenMerges() []Merge {
	ms := make([]Merge, 10)
	for i := range ms {
		ms[i] = Merge{Number: 100 + i, At: now.Add(-time.Duration(i) * time.Hour)}
	}
	return ms
}

// MC8, M5: one follow-up or one regression in 10 demotes with maxFollowUps 1; with 0, as before,
// neither does; an eleventh clean merge pushes it out of the window.
func TestFollowUpsAndRegressionsDemote(t *testing.T) {
	on, off := config.Breaker{Window: 10, MaxReverts: 1, MaxFollowUps: 1}, config.Breaker{Window: 10, MaxReverts: 1}
	for _, bad := range []func(*Merge){func(m *Merge) { m.FollowUp = true }, func(m *Merge) { m.Regression = true }} {
		ms := tenMerges()
		bad(&ms[9])
		if d, _ := Demoted(ms, on); !d {
			t.Fatal("one bad outcome in 10 demotes")
		}
		if d, _ := Demoted(ms, off); d {
			t.Fatal("maxFollowUps 0 counts none")
		}
		if d, _ := Demoted(append(ms, Merge{Number: 99, At: now.Add(time.Hour)}), on); d {
			t.Fatal("an eleventh clean merge pushes it out")
		}
	}
}

// Offline proof 5, v2 G1's shape: with every Task deleted, the labels alone keep the class
// demoted — a human merge of the class (factory/merged) and its follow-up's label.
func TestADemotionOnFollowUpSurvivesEveryTaskDeleted(t *testing.T) {
	g := declaredRig(t)
	g.r.Cfg.Merge.Breaker = config.Breaker{Window: 10, MaxReverts: 1, MaxFollowUps: 1}
	g.f.SetPR(forge.PR{Number: 40, State: "MERGED", MergedBy: "Smana", MergedAt: now.Add(-48 * time.Hour),
		Labels: []string{MergedLabel("dead-links")}})
	g.f.SetPR(forge.PR{Number: 41, State: "MERGED", MergedBy: "Smana", MergedAt: now.Add(-24 * time.Hour),
		Labels: []string{FollowUpOfLabel(40)}})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Reason != "class_paused" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
	g = declaredRig(t)
	g.r.Cfg.Merge.Breaker = config.Breaker{Window: 10, MaxReverts: 1, MaxFollowUps: 1}
	g.f.SetPR(forge.PR{Number: 40, State: "MERGED", MergedBy: "renovate[bot]", MergedAt: now.Add(-48 * time.Hour),
		Labels: []string{MergedLabel("dead-links"), RegressionLabel}})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Reason != "class_paused" {
		t.Fatalf("a regression label demotes: %s %s", tk.Status.Phase, tk.Status.Reason)
	}
	_ = v1alpha1.PhaseDone
}
```

M1: once `githubLedger` unites `factory/merged:<class>`, a maintainer's or Renovate's merge of
today would count against `autoMergesPerDay` and `maxPerDay`, which count "the merges the factory
issued". In `merge_gate_test.go`, beside `TestMaxPerDayCountsTheClassOnly`:

```go
// M1: the daily caps count the factory's own merges only. A maintainer's merge of the class
// today, labelled factory/merged by the monitor, fills no day; the merger's does.
func TestTheDailyCapCountsAutoMergesOnly(t *testing.T) {
	g := declaredRig(t)
	g.f.SetPR(forge.PR{Number: 9, State: "MERGED", MergedBy: "Smana", MergedAt: now.Add(-time.Hour),
		Labels: []string{MergedLabel("dead-links")}})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAutoMerging {
		t.Fatalf("a maintainer's merge is not the factory's: %s %s", tk.Status.Phase, tk.Status.Reason)
	}
	g = declaredRig(t)
	g.f.SetPR(forge.PR{Number: 9, State: "MERGED", MergedBy: "ogenki-agent-merger[bot]", MergedAt: now.Add(-time.Hour),
		Labels: []string{MergedLabel("dead-links"), AutoMergedLabel("dead-links")}})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Reason != "auto_merge_cap" {
		t.Fatalf("the merger's merge today fills its day: %s %s", tk.Status.Phase, tk.Status.Reason)
	}
}
```

- [ ] **Step 2: Run.** `ulimit -v 4000000; timeout 300 go test ./internal/factory/reconciler/ -run 'FollowUps|SurvivesEveryTask|DailyCapCountsAutoMergesOnly' -p 1` — Expected: FAIL, `unknown field FollowUp in struct literal`.

- [ ] **Step 3: Implement.** `ledger.go`: the three label functions and the fields on `Merge`
(doc: "FollowUp: a later merge fixed it (MC8); Regression: the monitor found one (MC7)").
`githubLedger`, per class: list `AutoMergedLabel(class)` **and** `MergedLabel(class)`, united by
number (a merger merge carries both); `Auto` is true for a merge listed under
`AutoMergedLabel(class)` (doc: "Auto: the merger merged it, so it counts against the daily caps").
`paused`'s and `armedTodayOf`'s Task side set `Auto: ref.ArmedAt != nil`. `uniteMerges` ORs `Auto`.
`armedTodayOf` (merge.go:785-803): the ledger loop counts `m` only when `m.Auto`, keeping
`seen`; its comment stays "the merges the factory issued" and is now true of both sides (M1).
Before the loop, read once
`PullsWithLabel(RegressionLabel)` into a set. For each of the newest `Window`: `Reverted` as today,
`FollowUp = len(PullsWithLabel(FollowUpOfLabel(n))) > 0` (merged or open: a fix in review is not
yet a fix — count only `State == "MERGED"`), `Regression = set[n]`. `uniteMerges` ORs the two new
fields as it does `Reverted`. `paused`: a Task's `Merge` gains `FollowUp: o.Status.Monitor != nil && len(o.Status.Monitor.FollowUps) > 0`
and `Regression: o.Status.Monitor != nil && o.Status.Monitor.Verdict == "regression"` (Task 5 adds
`status.monitor`; until then leave the two false and add them in Task 5's step 3). `Demoted`:

```go
func Demoted(merges []Merge, b config.Breaker) (bool, int) {
	newestFirst(merges)
	reverts, followUps := 0, 0
	for _, m := range merges[:min(len(merges), b.Window)] {
		if m.Reverted {
			reverts++
		}
		if m.FollowUp || m.Regression {
			followUps++
		}
	}
	return reverts >= b.MaxReverts || (b.MaxFollowUps > 0 && followUps >= b.MaxFollowUps), reverts
}
```

The ledger still reads once per process (as today): the Tasks carry what the monitor learns later,
so a running factory sees it through `paused`'s Task side.

- [ ] **Step 4: Run.** `ulimit -v 4000000; timeout 900 go test ./internal/factory/reconciler/ -p 1` — Expected: `ok`; `ledger_test.go` unchanged and green (a breaker with `MaxFollowUps` 0 decides as before); `TestMaxPerDayCountsTheClassOnly` unchanged and green (its merges carry `AutoMergedLabel`).

- [ ] **Step 5: Commit.**

```bash
git add internal/factory/reconciler
git commit -m "feat(factory): follow-ups and regressions in the merge ledger and the breaker"
```

### Task 4: The merger's reads for follow-ups

**Files:**
- Modify: `internal/factory/forge/forge.go`, `github.go`, `absent.go`, `forgetest/fake.go`
- Test: `internal/factory/forge/github_test.go` (its httptest pattern), `internal/factory/forge/patch_test.go`

**Interfaces:**
- Produces: `forge.MergedPull{Number int; Author, HeadRef, MergeCommitSHA, Title string; MergedAt time.Time; Labels []string}`; `forge.FilePatch{Path, Previous, Status, Patch string}`; `forge.BlameRange{Start, End int; Commit string}`; `forge.ErrMergedTruncated`; `forge.BaseLines(patch string) []int`; `Merger.MergedSince(ctx, since time.Time) ([]MergedPull, error)`, `Merger.PullFiles(ctx, n int) ([]FilePatch, error)`, `Merger.BlameAtParent(ctx, mergeSHA, path string) ([]BlameRange, error)`. Fake: `SetMerged(ps ...forge.MergedPull)`, `SetPullFiles(n int, fs ...forge.FilePatch)`, `SetBlame(mergeSHA, path string, rs ...forge.BlameRange)`, `BlameCalls() int`.

- [ ] **Step 1: Write the failing tests.** `patch_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"slices"
	"testing"
)

// BaseLines are the base-side line numbers a unified patch removes or rewrites: what a later pull
// request changed of the file as it found it (merge gate R35). Pure additions change none.
func TestBaseLines(t *testing.T) {
	patch := "@@ -3,4 +3,4 @@ name: x\n context\n-  version: 1.2.3\n+  version: 1.2.4\n context\n context\n" +
		"@@ -20,2 +20,3 @@\n context\n+added\n context\n@@ -40,3 +41,1 @@\n-gone a\n-gone b\n keep\n"
	if got := BaseLines(patch); !slices.Equal(got, []int{4, 40, 41}) {
		t.Fatalf("%v", got)
	}
	if BaseLines("") != nil || BaseLines("Binary files differ") != nil {
		t.Fatal("no hunk, no line")
	}
}
```

In `github_test.go`, three cases on the existing httptest server: `MergedSince` sends the search
`repo:o/n is:pr is:merged merged:>=2026-10-16T10:00:00Z` and maps 2 nodes; 100 nodes return
`ErrMergedTruncated`; `PullFiles` maps `patch` and `previous_filename`, and 3,000 files (GitHub's
cap) return an error; `BlameAtParent` posts a query naming `parents(first: 1)` and `blame(path:`
and maps `startingLine`, `endingLine`, `commit.oid`.

- [ ] **Step 2: Run.** `ulimit -v 4000000; timeout 300 go test ./internal/factory/forge/ -p 1` — Expected: FAIL, `undefined: BaseLines`.

- [ ] **Step 3: Implement.** `BaseLines` (in `forge.go`, beside `trailer`): walk lines; a hunk header
`@@ -a[,b] +c[,d] @@` sets the base cursor to `a`; a ` ` line advances it; a `-` line records the
cursor and advances it; a `+` line does not move it; a `\` line is skipped; return sorted,
deduplicated, `nil` when none. `github.go`:
- `MergedSince`: GraphQL `search(type: ISSUE, first: 100, query: $q)` with `... on PullRequest { number author { login } headRefName title mergedAt mergeCommit { oid } labels(first: 20) { nodes { name } } }`, the query `repo:<owner>/<name> is:pr is:merged merged:>=<since RFC 3339>`; logins REST-shaped (a bot gains `[bot]`, as `PullRequest` does); 100 results → `ErrMergedTruncated` (the caller narrows `since`).
- `PullFiles`: REST `PullRequests.ListFiles`, 100 a page, up to 30 pages; past 3,000 an error.
- `BlameAtParent`: GraphQL `repository { object(oid: $sha) { ... on Commit { parents(first: 1) { nodes { blame(path: $path) { ranges { startingLine endingLine commit { oid } } } } } } } }`; a missing file at the parent returns no range, not an error.
All three are merger reads (contents and pull requests read, which it holds). `absent.go` fails
closed with its usual error; the fake returns what was set and counts `BlameAtParent` calls.

- [ ] **Step 4: Run.** `ulimit -v 4000000; timeout 300 go test ./internal/factory/forge/... -p 1` — Expected: `ok`.

- [ ] **Step 5: Commit.**

```bash
git add internal/factory/forge
git commit -m "feat(forge): merged pull requests, their patches and blame, for follow-up fixes"
```

### Task 5: The monitor: main's CI and follow-ups

**Files:**
- Modify: `api/factory/v1alpha1/task_types.go` (+ `task crd:gen` output), `internal/factory/reconciler/reconciler.go`, `internal/factory/reconciler/merge.go` (`paused`), `internal/factory/reconciler/implement.go` (`prEnded`), `internal/factory/fmetrics/fmetrics.go`, `internal/factory/narrate/narrate.go`
- Create: `internal/factory/reconciler/monitor.go`
- Test: `internal/factory/reconciler/monitor_test.go`, `internal/factory/fmetrics/fmetrics_test.go`

**Interfaces:**
- Consumes: Task 2's `config.Monitor`, Task 3's labels, Task 4's reads.
- Produces: `v1alpha1.MonitorStatus{Since metav1.Time; Main, Oracle, Flux, Verdict string; FollowUps []int; OracleRuns []int64; Applied *metav1.Time; Posted bool}` at `status.monitor`, each signal `""` (open), `healthy`, `regression` or `inconclusive`; `reconciler.monitorDue(t) bool`; `const monitorEvery = 10 * time.Minute`; `reconciler.Worst(vs ...string) string`; metric `agent_factory_monitor_verdicts_total{class,verdict,repository}` (`fmetrics.(*Set).MonitorVerdict`); `outcome="follow_up"`; `narrate.MonitorVerdict(t, n int, verdict string, why []string, control bool) Event` (key `monitor-<n> verdict=<v>` on the PR, `monitor-<n> control verdict=<v>` for the control-issue copy, M2), `narrate.FollowUpFound(t, m, later int, control bool) Event` (key `follow-up-<later>`, or `follow-up-<later> control`).

- [ ] **Step 1: Write the failing tests** (offline proof 4 and the main signal):

```go
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"slices"
	"strconv"
	"testing"
	"time"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

const mSHA = "aaaa000000000000000000000000000000000001"

// monitorRig: task 3buqdlot of a monitored class, merged by a maintainer 2 h ago at PR 12 as
// mSHA, which changed website/content/docs/a.md line 7.
func monitorRig(t *testing.T) *rig {
	g := declaredRig(t)
	cl := g.r.Cfg.Classes["dead-links"]
	cl.Monitor = &config.Monitor{FollowUpWindow: config.Duration{Duration: 168 * time.Hour}}
	g.r.Cfg.Classes["dead-links"] = cl
	g.r.Cfg.Merge.Breaker = config.Breaker{Window: 10, MaxReverts: 1, MaxFollowUps: 1}
	g.setDone(t, "3buqdlot", 12, "Smana", mSHA, now.Add(-2*time.Hour))
	g.f.SetPullFiles(12, forge.FilePatch{Path: testFile, Status: "modified", Patch: "@@ -7 +7 @@\n-[x](https://a)\n+[x](https://b)\n"})
	g.f.SetCommitChecks(mSHA, green("SUCCESS").Runs...)
	return g
}

func later(n int, author, branch string, labels ...string) forge.MergedPull {
	return forge.MergedPull{Number: n, Author: author, HeadRef: branch, MergeCommitSHA: "bbbb" + string(rune('0'+n%10)),
		MergedAt: now.Add(-time.Hour), Labels: labels}
}

// Offline proof 4: an overlapping later PR is a follow-up and gets the label; one elsewhere is
// not; a maintainer's label counts. Renovate's next bump (an adopt login of the class) is not:
// that case joins this table in Task 16, once config.Author exists.
func TestFollowUps(t *testing.T) {
	for name, c := range map[string]struct {
		pr     forge.MergedPull
		files  []forge.FilePatch
		blame  []forge.BlameRange
		follow bool
	}{
		"changes a line m wrote": {later(20, "Smana", "fix"), []forge.FilePatch{{Path: testFile, Status: "modified",
			Patch: "@@ -7 +7 @@\n-[x](https://b)\n+[x](https://c)\n"}}, []forge.BlameRange{{Start: 1, End: 6, Commit: "old"}, {Start: 7, End: 7, Commit: mSHA}}, true},
		"changes another line": {later(21, "Smana", "fix"), []forge.FilePatch{{Path: testFile, Status: "modified",
			Patch: "@@ -2 +2 @@\n-a\n+b\n"}}, []forge.BlameRange{{Start: 1, End: 6, Commit: "old"}, {Start: 7, End: 7, Commit: mSHA}}, false},
		"another file":         {later(22, "Smana", "fix"), []forge.FilePatch{{Path: "docs/b.md", Status: "modified", Patch: "@@ -7 +7 @@\n-a\n+b\n"}}, nil, false},
		"a maintainer's label": {later(24, "Smana", "x", FollowUpOfLabel(12)), nil, nil, true},
		"m's revert": {later(25, "ogenki-agent-merger[bot]", "revert-12", RevertOfLabel(12)), []forge.FilePatch{{Path: testFile,
			Status: "modified", Patch: "@@ -7 +7 @@\n-b\n+a\n"}}, []forge.BlameRange{{Start: 7, End: 7, Commit: mSHA}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			g := monitorRig(t)
			g.f.SetMerged(c.pr)
			g.f.SetPullFiles(c.pr.Number, c.files...)
			g.f.SetBlame(c.pr.MergeCommitSHA, testFile, c.blame...)
			tk := g.reconcile(t, "3buqdlot", 1)
			got := tk.Status.Monitor != nil && slices.Contains(tk.Status.Monitor.FollowUps, c.pr.Number)
			if got != c.follow {
				t.Fatalf("follow-up %v, want %v", got, c.follow)
			}
			labelled := slices.Contains(g.f.Added(c.pr.Number), FollowUpOfLabel(12))
			if c.follow && !slices.Contains(c.pr.Labels, FollowUpOfLabel(12)) && !labelled {
				t.Fatal("the factory labels a follow-up it found (MC13)")
			}
			// M2: the control issue's copy has its own narration key, so it is posted too.
			if c.follow && !contains(g.f.Comments(g.r.Cfg.ControlFor().Issue), "follow-up-"+strconv.Itoa(c.pr.Number)+" control") {
				t.Fatalf("the control issue hears of the follow-up: %q", g.f.Comments(g.r.Cfg.ControlFor().Issue))
			}
		})
	}
}

// The main signal for a merge nobody verified (a maintainer's or Renovate's): red within verifyFor
// is a regression, labelled at once, never a revert (MC7); green past it is healthy.
func TestMainSignal(t *testing.T) {
	g := monitorRig(t)
	g.f.SetCommitChecks(mSHA, forge.Check{Name: "Pre-commit checks 🛃", State: "FAILURE"})
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Monitor.Main != "regression" || tk.Status.Monitor.Verdict != "regression" || len(g.f.Reverts()) != 0 ||
		!slices.Contains(g.f.Added(12), RegressionLabel) {
		t.Fatalf("%+v reverts %v", tk.Status.Monitor, g.f.Reverts())
	}
	// M2: the same verdict reaches the kill-switch issue under its own key.
	if ctl := g.r.Cfg.ControlFor().Issue; !contains(g.f.Comments(ctl), "monitor-12 control verdict=regression") {
		t.Fatalf("the control issue %d hears of the regression: %q", ctl, g.f.Comments(ctl))
	}
	g = monitorRig(t)
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Monitor.Main != "healthy" || tk.Status.Monitor.Verdict != "" {
		t.Fatalf("main healthy, verdict open until the follow-up window ends: %+v", tk.Status.Monitor)
	}
}

// The verdict is final and posted once at the end of followUpWindow; the task then leaves the
// reconciler's books. A class without monitor is never watched (R29).
func TestTheVerdictClosesTheWatch(t *testing.T) {
	g := monitorRig(t)
	g.setDone(t, "3buqdlot", 12, "Smana", mSHA, now.Add(-169*time.Hour))
	tk := g.reconcile(t, "3buqdlot", 2)
	if tk.Status.Monitor.Verdict != "healthy" || !tk.Status.Monitor.Posted || !contains(g.f.Comments(12), "verdict=healthy") ||
		!slices.Contains(g.f.Added(12), MergedLabel("dead-links")) {
		t.Fatalf("%+v %q", tk.Status.Monitor, g.f.Comments(12))
	}
	if g.r.monitorDue(tk) {
		t.Fatal("a posted verdict is not due again")
	}
	// R29, not vacuous (L14): a merged docs-links task, with its merge commit and time, is still
	// never watched, because docs-links declares no monitor.
	g = mergeRig(t, v1alpha1.PhaseDone)
	g.setDone(t, "3buqdlot", 12, "Smana", mSHA, now.Add(-2*time.Hour))
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Monitor != nil || g.r.monitorDue(tk) {
		t.Fatal("docs-links declares no monitor (R29)")
	}
}

func TestWorst(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{{[]string{"healthy", "healthy"}, "healthy"}, {[]string{"healthy", "inconclusive"}, "inconclusive"},
		{[]string{"inconclusive", "regression"}, "regression"}, {[]string{"healthy", ""}, ""}} {
		if got := Worst(c.in...); got != c.want {
			t.Fatalf("%v: %q", c.in, got)
		}
	}
}
```

`setDone` is a rig helper: phase `Done`, reason `merged`, `status.pullRequest` with the number,
`MergedBy`, `MergeCommitSHA`, `MergedAt`, and the fake's PR `n` set `MERGED` with the same
`MergedAt` (the monitor reads GitHub's, L6). `monitorRig` gives the config a control issue
(`g.r.Cfg.Control = config.Control{Repository: "Smana/cloud-native-ref", Issue: 3}`) when
`g.r.Cfg.ControlFor().Issue` is 0. In `fmetrics_test.go`, `MonitorVerdict` records
`class`, `verdict` and `repository`, an unknown verdict reads `other`, and `PROutcome(..., "follow_up")`
is kept, not `other`.

- [ ] **Step 2: Run.** `ulimit -v 4000000; timeout 300 go test ./internal/factory/reconciler/ -run 'FollowUps|MainSignal|ClosesTheWatch|Worst' -p 1` — Expected: FAIL, `tk.Status.Monitor undefined`.

- [ ] **Step 3: Implement.**

`task_types.go`, in `TaskStatus` after `PullRequest`:

```go
	// The post-merge monitor (merge gate classes MC7), for a class that declares one: each signal's
	// reading and the verdict, the worst of them. Unset until the task ends merged.
	// +optional
	Monitor *MonitorStatus `json:"monitor,omitempty"`
```

```go
// MonitorStatus is one merge's watch. A signal is "" while open, then healthy, regression or
// inconclusive; Verdict is set at the first regression, or when every signal is final.
type MonitorStatus struct {
	Since metav1.Time `json:"since"`
	// +kubebuilder:validation:Enum="";healthy;regression;inconclusive
	// +optional
	Main string `json:"main,omitempty"`
	// +kubebuilder:validation:Enum="";healthy;regression;inconclusive
	// +optional
	Oracle string `json:"oracle,omitempty"`
	// +kubebuilder:validation:Enum="";healthy;regression;inconclusive
	// +optional
	Flux string `json:"flux,omitempty"`
	// +kubebuilder:validation:Enum="";healthy;regression;inconclusive
	// +optional
	Verdict string `json:"verdict,omitempty"`
	// Later pull requests that fixed this merge (MC8).
	// +kubebuilder:validation:MaxItems=16
	// +optional
	FollowUps []int `json:"followUps,omitempty"`
	// The detector runs read after the merge (R37).
	// +kubebuilder:validation:MaxItems=8
	// +optional
	OracleRuns []int64 `json:"oracleRuns,omitempty"`
	// When the Flux source first held the merge commit (R33).
	// +optional
	Applied *metav1.Time `json:"applied,omitempty"`
	// The verdict comment is on the pull request.
	// +optional
	Posted bool `json:"posted,omitempty"`
}
```

Run `task crd:gen` and keep its output.

`monitor.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package reconciler

// monitorEvery is how often a merge under watch is read: its signals move on CI's, Flux's and the
// detector's clocks, and each read costs the merger's GitHub calls.
const monitorEvery = 10 * time.Minute

// monitorDue: a Done task merged through a class that declares a monitor (R29), whose verdict is
// not posted yet. Reverted tasks are not watched: the revert is their outcome.
func (r *Reconciler) monitorDue(t *v1alpha1.Task) bool {
	ref := t.Status.PullRequest
	if t.Status.Phase != v1alpha1.PhaseDone || ref == nil || ref.MergedAt == nil || ref.MergeCommitSHA == "" {
		return false
	}
	cl, ok := r.repo(t).Classes[t.Spec.PredictedClass]
	return ok && cl.Monitor != nil && (t.Status.Monitor == nil || !t.Status.Monitor.Posted)
}

// Worst is the monitor's verdict over its signals: regression over inconclusive over healthy; ""
// while any is open and none regressed.
func Worst(vs ...string) string {
	rank := map[string]int{"healthy": 0, "inconclusive": 1, "regression": 2}
	out, open := "healthy", false
	for _, v := range vs {
		if v == "" {
			open = true
			continue
		}
		if rank[v] > rank[out] {
			out = v
		}
	}
	if out != "regression" && open {
		return ""
	}
	return out
}

// monitor reads every open signal once, labels what it learns on GitHub (R30, R31), and posts the
// verdict when it is final.
func (r *Reconciler) monitor(ctx context.Context, t *v1alpha1.Task) error {
	ref, cl := t.Status.PullRequest, r.repo(t).Classes[t.Spec.PredictedClass]
	pr, err := r.gh(ctx).PullRequest(ctx, ref.Number)
	if err != nil {
		return err
	}
	m := t.Status.Monitor
	if m == nil {
		// L6: every window counts from GitHub's merge time. The Task's MergedAt is the factory's
		// clock when it saw the merge (implement.go, merge.go), which may be later.
		at := pr.MergedAt
		if at.IsZero() {
			at = ref.MergedAt.Time
		}
		m = &v1alpha1.MonitorStatus{Since: metav1.Time{Time: at}}
		t.Status.Monitor = m
	}
	r.label(ctx, pr, MergedLabel(t.Spec.PredictedClass))
	if m.Main == "" {
		if m.Main, err = r.mainSignal(ctx, t); err != nil {
			return err
		}
	}
	// Task 8 adds: m.Oracle (cl.Monitor.OracleRuns > 0) and m.Flux (cl.Monitor.Flux != nil).
	if err := r.followUps(ctx, t, cl, pr); err != nil {
		return err
	}
	signals := []string{m.Main}
	if cl.Monitor.OracleRuns > 0 {
		signals = append(signals, m.Oracle)
	}
	if cl.Monitor.Flux != nil {
		signals = append(signals, m.Flux)
	}
	ended := r.Now().Sub(m.Since.Time) >= cl.Monitor.FollowUpWindow.Duration
	switch v := Worst(signals...); {
	case v == "regression": // a follow-up is its own bad outcome (Task 3), never a monitor verdict
		m.Verdict = "regression"
	case ended && v != "":
		m.Verdict = v
	}
	if m.Verdict == "regression" {
		r.label(ctx, pr, RegressionLabel)
	}
	if m.Verdict != "" && !m.Posted {
		class, verdict := t.Spec.PredictedClass, m.Verdict
		record(ctx, func(ctx context.Context) { r.Metrics.MonitorVerdict(ctx, class, verdict) })
		narrateOn(t, ref.Number, narrate.MonitorVerdict(t, ref.Number, m.Verdict, monitorWhy(m), false))
		if m.Verdict == "regression" {
			// M2: its own key, or narrateOn drops it as the PR's comment already queued.
			if n := r.Cfg.ControlFor().Issue; n > 0 {
				narrateOn(t, n, narrate.MonitorVerdict(t, ref.Number, m.Verdict, monitorWhy(m), true))
			}
		}
		m.Posted = m.Verdict != "regression" || ended // a regression is posted at once and watched on for follow-ups
	}
	return nil
}
```

A follow-up is a bad outcome the breaker counts (Task 3), not a monitor verdict: MC13 keeps the
kinds apart for the tally. A regression posts once (narration key `monitor-<n> verdict=regression`) and the task stays
due for follow-ups until the window ends, then `Posted` closes the watch; the verdict comment with
`verdict=healthy` or `inconclusive` is posted once, at the end.

`mainSignal`: for an auto-merge (`ref.AutoMerged`) `verifying` already watched main and reached
`Done "merged_verified"`: `healthy`. Otherwise `MainState(CommitChecks(merge SHA), verifyChecks)`:
`FAILURE` → `regression` at once; `SUCCESS` once `verifyFor` has passed since `m.Since` →
`healthy`; still open otherwise; open past `2 × verifyFor` → `inconclusive`.

`followUps(ctx, t, cl, pr)`: one `MergedSince(m.Since)` per repository and 10 minutes, cached
on the Reconciler (`mergedCache`, keyed by repository, like `ledger`); `ErrMergedTruncated` reads
the window in two halves (since, since+window/2) once, then logs and keeps what it read. *m*'s
files: `PullFiles(m)` once, cached in-process by PR number. For each later PR `p` (not *m*, merged
after `m.Since`, before `m.Since + FollowUpWindow`, not in `m.FollowUps`): skip it when it carries
`RevertOfLabel(m)`. (R35's other skip, a later PR authored by one of the class's `adopt` logins,
needs `config.Author`, which arrives in Task 12: Task 16 adds it here, M3. Do not stub it.) It is
a follow-up when it carries `FollowUpOfLabel(m)`, or when for some file of `PullFiles(p)` that *m*
also touched, `BlameAtParent(p.MergeCommitSHA, path)` has a range holding one of
`forge.BaseLines(patch)` whose commit is *m*'s merge commit. Blame results are cached by (merge
SHA, path) in-process. A follow-up: append to `m.FollowUps` (sorted, at most 16), label the later
PR with `FollowUpOfLabel(m)` (`r.label` takes a `forge.PR`: pass
`forge.PR{Number: p.Number, Labels: p.Labels}`), `PROutcome(class, "follow_up")`, narrate
`FollowUpFound(t, m, p.Number, false)` on *m*'s PR and `FollowUpFound(t, m, p.Number, true)` on
the control issue (its own key, M2).

`reconciler.go`, `Reconcile` (l.197-219): `revertWait := r.revertable(&t) || r.revertPending(&t)` (l.201) becomes
`revertWait := r.revertable(&t) || r.revertPending(&t) || r.monitorDue(&t)` (rename it
`watchWait` and update its comment: "a requested revert, an open revert PR and a merge under watch
are the three exceptions"), and the early return (l.210) reads it; the terminal requeue, beside
the revert watch's (l.271), returns `RequeueAfter: monitorEvery` when `monitorDue` and nothing
shorter is due. `step`'s `PhaseDone` arm (l.326-327):

```go
	case v1alpha1.PhaseDone:
		if r.revertable(t) {
			return r.revert(ctx, t, "revert_requested")
		}
		return r.monitor(ctx, t) // reached only when monitorDue
```

`roomclose.go` needs no change: the monitor reads no room. `merge.go` `paused`: fill the Task side's
`FollowUp` and `Regression` from `o.Status.Monitor` (Task 3, Step 3). `fmetrics.go`: `outcomes()`
gains `follow_up`; `monitorVerdicts` counter `agent_factory_monitor_verdicts_total` (description
"Post-merge monitor verdicts (merge gate classes MC7), by class and verdict") with
`MonitorVerdict(ctx, class, verdict)` (`oneOf(verdict, []string{"healthy","regression","inconclusive"})`,
`s.repoOf(ctx)`); the reconciler's `metrics` interface gains `MonitorVerdict`. `narrate.go`:
`MonitorVerdict` body "Agent factory monitor of #%d (`%s`): **%s**. %s" with the signals read
(`main CI: healthy · Flux: inconclusive (the source never reached the merge)`), and for a
regression "The class is demoted at its next decision (breaker.maxFollowUps); nothing is reverted
(MC7)."; `FollowUpFound` "#%d changed lines #%d introduced within %s: counted as a follow-up fix
(MC8)." With `control` true each keeps its body and takes the control-issue key (M2), as
`ClassDemoted` (narrate.go:511) is a distinct event for the same reason.

- [ ] **Step 4: Run the factory's packages with envtest.**
  `ulimit -v 4000000; KUBEBUILDER_ASSETS=/home/smana/.local/share/kubebuilder-envtest/k8s/1.36.2-linux-amd64 timeout 1200 go test ./internal/factory/... ./api/... -p 1`
  Expected: `ok` for every package; `task crd:check` passes.

- [ ] **Step 5: Commit.**

```bash
git add api internal/factory charts/agent-factory/crds config
git commit -m "feat(factory): the post-merge monitor, main's CI and follow-up fixes"
```

### Task 6: The tally's outcomes, the docs and PR E

**Files:**
- Modify: `scripts/shadow-merge-tally.sh`, `scripts/shadow-merge-tally_test.sh` (the existing suite, run by `task scripts`, M8), `docs/factory.md`
- Create: `scripts/clopper-pearson.py`, `scripts/testdata/tally/gh-stub.sh`, `scripts/testdata/tally/*.json`

- [ ] **Step 1: Write the failing suite part** (offline proof 7). Extend
  `scripts/shadow-merge-tally_test.sh`, before its closing `echo ok` (it runs from the repository
  root under `set -euo pipefail`; there is no `scripts/tests/` and no `# requires:` harness, M8):

```bash
# Offline proof 7: the bound on fixtures. 0 bad in 28 fails 10%, 0 in 29 passes; 1 in 46 passes;
# immature (merged under followUpWindow ago) and inconclusive merges are left out of the bound. A
# stub gh serves scripts/testdata/tally/, so nothing reaches GitHub.
bound() { python3 scripts/clopper-pearson.py "$1" "$2" | awk '{print ($1 < 0.10) ? "pass" : "fail"}'; }
expect() { [ "$1" = "$2" ] || { echo "FAIL: $3: got $1, want $2" >&2; exit 1; }; }
expect "$(bound 0 28)" fail "0 in 28 is not under 10%"
expect "$(bound 0 29)" pass "0 in 29 is under 10%"
expect "$(bound 1 46)" pass "1 in 46 is under 10%"
expect "$(bound 1 45)" fail "1 in 45 is not"
expect "$(bound 2 61)" pass "2 in 61 is under 10%"
export GH_LOG="$tmp/gh.log"
tally() { GH=scripts/testdata/tally/gh-stub.sh NOW=2026-10-23T00:00:00Z scripts/shadow-merge-tally.sh \
  --class dead-links --outcomes --since 2026-08-01 --window 168h; }
out="$(tally)"
expect "$(grep -c '^#' <<<"$out")" 31 "31 merges listed"
expect "$(grep -o 'mature=[0-9]*' <<<"$out")" mature=30 "an immature merge left out"
expect "$(grep -o ' conclusive=[0-9]*' <<<"$out")" " conclusive=29" "the bound's merges: mature and conclusive"
expect "$(grep -o ' inconclusive=[0-9]*' <<<"$out")" " inconclusive=1" "an inconclusive merge counted apart"
expect "$(grep -o 'bad=[0-9]*' <<<"$out")" bad=0 "no bad outcome"
expect "$(grep -o 'widen=[a-z]*' <<<"$out")" widen=yes "0 bad in 29 conclusive mature merges: widen"
out="$(TALLY_FIXTURE=regression tally)"
expect "$(grep -o 'bad=[0-9]*' <<<"$out") $(grep -o 'regression=[0-9]*' <<<"$out")" "bad=1 regression=1" "a regression label is a bad outcome"
expect "$(grep -o 'widen=[a-z]*' <<<"$out")" widen=no "1 bad in 29: no widening"
grep -qF 'label:"factory/merged:dead-links"' "$GH_LOG" || { echo "FAIL: the search quotes the label (L11)" >&2; exit 1; }
```

Build the fixtures so the counts hold: 31 merged PRs of the class labelled `factory/merged:dead-links`
since 2026-08-01, 1 merged 2026-10-20 (immature at 168 h), so 30 mature: 1 with a factory comment
`event=monitor-<n> verdict=inconclusive` and 29 conclusive with `verdict=healthy` (L10: the bound
is over the 29). The bound over 29 passes, so `widen=yes`. `gh-stub.sh` appends its arguments to
`$GH_LOG` and serves `prs.json`, or
`prs-regression.json` when `TALLY_FIXTURE=regression`, where one of the 29 carries
`factory/monitor:regression`: `bad=1 regression=1` and `widen=no` (1 in 29).

- [ ] **Step 2: Run.** `ulimit -v 4000000; timeout 120 bash scripts/shadow-merge-tally_test.sh` — Expected: FAIL, `clopper-pearson.py: No such file`.

- [ ] **Step 3: Implement.** `scripts/clopper-pearson.py <bad> <n> [confidence=0.95]` prints the
one-sided upper bound: `n == 0` prints `1`; else bisect `p` in [0, 1] for 60 rounds on
`sum(comb(n, i) p^i (1-p)^(n-i), i ≤ bad) = 1 - confidence`, standard library only, with `-I`-safe
imports. `shadow-merge-tally.sh` gains `--outcomes`, `--since <date>`, `--window <h>` (default
`168h`) and `--budget` (default `0.10`); with `--outcomes` it lists merged PRs labelled
`factory/merged:<class>` or `factory/auto-merged:<class>` merged since `--since`
(`gh pr list --search "label:\"factory/merged:<class>\" merged:>=<since>"` and the same for
auto-merged, united by number: the label is quoted inside the search, L11, or the `:` and `/`
split it and the list comes back empty), reads each PR's revert (the existing `$reverted`), a merged PR
labelled `factory/follow-up-of:#<n>`, its own `factory/monitor:regression` label and its newest
factory comment `event=monitor-<n> verdict=<v>`, prints `#<n> <mature|immature> <verdict> <bad kinds>`
per PR, then `merges=… mature=… conclusive=… inconclusive=… bad=… reverted=… follow_up=… regression=…
bound=… budget=… widen=yes|no`, where `bound` is over the mature, conclusive merges (`clopper-pearson.py
bad conclusive`), and `widen=yes` only when `bound < budget`. `NOW` overrides the clock for the suite.
`task scripts` already runs the suite (`taskfile.yaml:41-45`): the taskfile is unchanged.

`docs/factory.md`: a **Monitor** section (the signals, the verdicts, the labels, what demotes, what
never reverts), `merge.awaitingCIFor`, `breaker.maxFollowUps`, `classes.<c>.monitor`, the reasons
`ci_wait_expired`, and "Calibration" with the tally's `--outcomes` and the rule "widen one step only
when bound < budget, by the owner's config PR citing the tally (MC9)".

- [ ] **Step 4: The gate.** `timeout 120 task scripts` (Expected: `ok` from each suite), `timeout 600 task lint` (Expected: `0 issues`), then with the Docker daemon started `timeout 2400 task check` — Expected: exit 0.

- [ ] **Step 5: Commit, push, PR E.**

```bash
git add scripts docs/factory.md
git commit -m "feat(scripts): the merge tally's outcomes and the widening bound"
git fetch origin && git rebase origin/main && git push -u origin feat/merge-outcomes
gh pr create --title "feat(factory): post-merge outcomes and the monitor, off by default (merge gate F1 PR E)" --body "Merge gate classes F1, first PR (ai-native-ref docs/superpowers/specs/2026-10-22-merge-gate-classes-design.md, MC7–MC9, MC13): merge.awaitingCIFor bounds AwaitingCI (closes #182), breaker.maxFollowUps, the outcome labels factory/merged, factory/follow-up-of and factory/monitor:regression read by the breaker's ledger, a class's monitor key, the monitor on Done tasks with main's CI and follow-up fixes, agent_factory_monitor_verdicts_total, outcome=follow_up, and shadow-merge-tally.sh --outcomes with the Clopper-Pearson bound. Everything off unless configured. Offline proofs 4, 5, 7. task check: exit 0."
```

---

## F1, PR F — agent-platform: the oracle and Flux signals, the release

### Task 7: `IsAncestor` and the Flux reader

**Files:**
- Modify: `internal/factory/forge/forge.go`, `github.go`, `absent.go`, `forgetest/fake.go`
- Create: `internal/factory/fluxwatch/fluxwatch.go`
- Test: `internal/factory/fluxwatch/fluxwatch_test.go`, `internal/factory/forge/github_test.go`

**Interfaces:**
- Produces: `Merger.IsAncestor(ctx, ancestor, sha string) (bool, error)`; `fluxwatch.Reader{C client.Reader}`; `(Reader).Source(ctx, ns, name string) (fluxwatch.Source, error)` with `Source{Suspended bool; Revision string; At time.Time}`; `(Reader).NotReadySince(ctx, ns, name string, since time.Time) ([]string, error)` returning `kind/namespace/name` sorted, only of the objects the GitRepository `ns/name` delivers (L7).

- [ ] **Step 1: Write the failing tests** (offline proof 6's reader half). `fluxwatch_test.go` with
controller-runtime's fake client holding `unstructured` objects, built with a RESTMapper that knows
the three Flux kinds (M5):

```go
// SPDX-License-Identifier: Apache-2.0

package fluxwatch

import (
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	ksV1  = "kustomize.toolkit.fluxcd.io/v1"
	hrV2  = "helm.toolkit.fluxcd.io/v2"
	srcV1 = "source.toolkit.fluxcd.io/v1"
)

var at = time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)

// fluxClient is a fake client that knows Flux's kinds: it resolves each kind's scope through its
// RESTMapper, and an empty scheme gives it none for them (M5).
func fluxClient(objs ...client.Object) client.Client {
	m := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"},
		{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"},
		{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository"},
	} {
		m.Add(gvk, meta.RESTScopeNamespace)
		m.Add(gvk.GroupVersion().WithKind(gvk.Kind+"List"), meta.RESTScopeNamespace)
	}
	return fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithRESTMapper(m).WithObjects(objs...).Build()
}

func obj(apiVersion, kind, ns, name, ready string, since time.Time) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetName(name)
	_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"type": "Ready", "status": ready,
		"lastTransitionTime": since.Format(time.RFC3339)}}, "status", "conditions")
	return u
}

// sourced sets a Kustomization's spec.sourceRef in flux-system.
func sourced(u *unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	_ = unstructured.SetNestedMap(u.Object, map[string]any{"kind": kind, "name": name, "namespace": "flux-system"}, "spec", "sourceRef")
	return u
}

// ownedBy labels an object as kustomize-controller labels what a Kustomization applies.
func ownedBy(u *unstructured.Unstructured, ks string) *unstructured.Unstructured {
	u.SetLabels(map[string]string{"kustomize.toolkit.fluxcd.io/name": ks, "kustomize.toolkit.fluxcd.io/namespace": "flux-system"})
	return u
}

// R32, L7: an object the watched source delivers, turned not Ready after the source applied the
// merge, is blamed; one already failing before, Ready, Unknown (still reconciling), or delivered
// by another source (an AI child on OCIRepository/ai-native-ref) is not.
func TestNotReadySince(t *testing.T) {
	c := fluxClient(
		sourced(obj(ksV1, "Kustomization", "flux-system", "tooling", "True", at.Add(time.Minute)), "GitRepository", "flux-system"),
		sourced(obj(ksV1, "Kustomization", "flux-system", "apps", "False", at.Add(-time.Hour)), "GitRepository", "flux-system"),
		sourced(obj(ksV1, "Kustomization", "flux-system", "agent-factory", "False", at.Add(5*time.Minute)), "OCIRepository", "ai-native-ref"),
		ownedBy(obj(hrV2, "HelmRelease", "tooling", "grafana", "False", at.Add(5*time.Minute)), "tooling"),
		ownedBy(obj(hrV2, "HelmRelease", "apps", "x", "Unknown", at.Add(time.Minute)), "apps"),
		ownedBy(obj(hrV2, "HelmRelease", "agent-system", "agent-factory", "False", at.Add(5*time.Minute)), "agent-factory"),
	)
	got, err := Reader{C: c}.NotReadySince(t.Context(), "flux-system", "flux-system", at)
	if err != nil || !slices.Equal(got, []string{"HelmRelease/tooling/grafana"}) {
		t.Fatalf("%v %v", got, err)
	}
}

func TestSource(t *testing.T) {
	u := obj(srcV1, "GitRepository", "flux-system", "flux-system", "True", at)
	_ = unstructured.SetNestedField(u.Object, "main@sha1:abc", "status", "artifact", "revision")
	_ = unstructured.SetNestedField(u.Object, at.Format(time.RFC3339), "status", "artifact", "lastUpdateTime")
	_ = unstructured.SetNestedField(u.Object, true, "spec", "suspend")
	s, err := Reader{C: fluxClient(u)}.Source(t.Context(), "flux-system", "flux-system")
	if err != nil || s.Revision != "abc" || !s.Suspended || !s.At.Equal(at) {
		t.Fatalf("%+v %v", s, err)
	}
}
```

If the fake client still refuses an unregistered kind with the mapper alone, register each kind
and its `List` in the scheme as unstructured (`s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})`,
`&unstructured.UnstructuredList{}` for the list) inside `fluxClient`.

In `github_test.go`: `IsAncestor` maps compare `status` `ahead` and `identical` to true, `behind`
and `diverged` to false.

- [ ] **Step 2: Run.** `ulimit -v 4000000; timeout 300 go test ./internal/factory/fluxwatch/ ./internal/factory/forge/ -p 1` — Expected: FAIL, `no Go files` / `undefined: IsAncestor`.

- [ ] **Step 3: Implement.** `fluxwatch.go` (package doc: "Package fluxwatch reads Flux's objects on
the factory's cluster for the post-merge monitor (merge gate classes MC7): uncached, read-only,
`unstructured`, so the factory carries no Flux API dependency."). `NotReadySince` lists
`kustomize.toolkit.fluxcd.io/v1` `KustomizationList` and `helm.toolkit.fluxcd.io/v2`
`HelmReleaseList` across namespaces, keeps the Kustomizations whose `spec.sourceRef` is
`GitRepository` `ns/name` (a missing `sourceRef.namespace` is the Kustomization's own) and the
HelmReleases whose `kustomize.toolkit.fluxcd.io/name` and `/namespace` labels name one of those
(L7: an AI child on `OCIRepository/ai-native-ref`, and what it applies, is never blamed on a host
merge), and of those returns each whose `Ready` condition is `False` with `lastTransitionTime`
after `since`. `Source` gets `source.toolkit.fluxcd.io/v1` `GitRepository`,
reads `spec.suspend`, the revision after `@sha1:` (or the whole value when there is none) and
`artifact.lastUpdateTime`. `IsAncestor`: REST `CompareCommits(ancestor, sha)` with one file per
page, `status` `ahead` or `identical`.

- [ ] **Step 4: Run.** Same command — Expected: `ok` twice.

- [ ] **Step 5: Commit.**

```bash
git add internal/factory/fluxwatch internal/factory/forge
git commit -m "feat(factory): read Flux's readiness and the merge commit's ancestry for the monitor"
```

### Task 8: The oracle and Flux signals, the wiring, the ClusterRole

**Files:**
- Modify: `internal/factory/reconciler/monitor.go`, `reconciler.go` (field `Flux`), `internal/factory/intake/deadlinks.go` (exports, M7; its callers and tests in `intake` follow the rename), `internal/factory/forge/forgetest/fake.go` (`SetAncestor`), `internal/app/factory.go`, `charts/agent-factory/templates/rbac.yaml`, `charts/agent-factory/values.yaml`, `charts/agent-factory/tests/render.sh`
- Test: `internal/factory/reconciler/monitor_signals_test.go`

**Interfaces:**
- Consumes: `forge.Merger.WorkflowRuns`, `RunArtifact`; `intake.ParseFindings`; `fluxwatch.Reader`.
- Produces: `intake.DetectorRun(r forge.WorkflowRun, rc *config.Repository) bool`, `intake.Gone(status string) bool`, `intake.ErrFindingsRefused` (M7: today `detectorRun` l.229, `gone` l.64, `errFindingsRefused` l.36, renamed, bodies unchanged); `Reconciler.Flux interface{ Source(ctx, ns, name string) (fluxwatch.Source, error); NotReadySince(ctx, ns, name string, since time.Time) ([]string, error) }` (nil: the Flux signal reads `inconclusive`); chart value `monitor.flux.enabled` (default `false`).

The reconciler imports `intake` directly: no cycle exists (`intake` imports only `api`, `config`,
`fmetrics`, `forge`, `killswitch`, `narrate`, `sanitize` and `taskid`), so nothing moves. The three
helpers it needs are exported first, in `deadlinks.go`, their `intake` callers and tests renamed.

- [ ] **Step 1: Write the failing tests** (offline proof 6 and the oracle half of 9's monitor):

```go
// SPDX-License-Identifier: Apache-2.0

package reconciler

// fakeFlux serves a source and the objects turned not Ready.
type fakeFlux struct {
	src      fluxwatch.Source
	notReady []string
}

func (f *fakeFlux) Source(context.Context, string, string) (fluxwatch.Source, error) {
	return f.src, nil
}
func (f *fakeFlux) NotReadySince(context.Context, string, string, time.Time) ([]string, error) {
	return f.notReady, nil
}

// fluxRig sets the reader only when there is one: a nil *fakeFlux in the interface is not a nil
// interface, and the monitor would call through it (M6).
func fluxRig(t *testing.T, f *fakeFlux) *rig {
	g := monitorRig(t)
	cl := g.r.Cfg.Classes["dead-links"]
	cl.Monitor.Flux = &config.FluxWatch{Source: "flux-system/flux-system", Within: config.Duration{Duration: time.Hour}}
	g.r.Cfg.Classes["dead-links"] = cl
	if f != nil {
		g.r.Flux = f
	}
	return g
}

// Offline proof 6: a HelmRelease turning not Ready is regression; a suspended source is
// inconclusive; all Ready for `within` after the source applied the merge is healthy.
func TestFluxSignal(t *testing.T) {
	applied := fluxwatch.Source{Revision: mSHA, At: now.Add(-90 * time.Minute)}
	for name, c := range map[string]struct {
		f    *fakeFlux
		want string
	}{
		"a HelmRelease turned not Ready": {&fakeFlux{src: applied, notReady: []string{"HelmRelease/tooling/grafana"}}, "regression"},
		"suspended":                      {&fakeFlux{src: fluxwatch.Source{Suspended: true}}, "inconclusive"},
		"never reached the merge":        {&fakeFlux{src: fluxwatch.Source{Revision: "older"}}, "inconclusive"},
		"all Ready":                      {&fakeFlux{src: applied}, "healthy"},
	} {
		t.Run(name, func(t *testing.T) {
			g := fluxRig(t, c.f)
			if name == "never reached the merge" {
				g.f.SetAncestor(mSHA, "older", false)
			}
			if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Monitor.Flux != c.want {
				t.Fatalf("%+v", tk.Status.Monitor)
			}
		})
	}
	if tk := fluxRig(t, nil).reconcile(t, "3buqdlot", 1); tk.Status.Monitor.Flux != "inconclusive" {
		t.Fatal("no reader (the chart's value off): inconclusive")
	}
}

// R37: a scheduled run after the merge that reports a written target gone is a regression; two
// clean runs are healthy; a failed run is inconclusive.
func TestOracleSignal(t *testing.T) {
	run := func(id int64, at time.Time, conclusion string) forge.WorkflowRun {
		return forge.WorkflowRun{ID: id, RunAttempt: 1, Event: "schedule", Branch: "main", Conclusion: conclusion, Created: at}
	}
	written := `[{"file":"website/content/docs/a.md","line":7,"url":"https://web.archive.org/web/2020/https://a","status":"404"}]`
	for name, c := range map[string]struct {
		runs []forge.WorkflowRun
		arts map[int64][]byte
		want string
	}{
		"a written target gone": {[]forge.WorkflowRun{run(2, now.Add(-time.Hour), "success")}, map[int64][]byte{2: []byte(written)}, "regression"},
		"two clean runs": {[]forge.WorkflowRun{run(3, now.Add(-time.Hour), "success"), run(2, now.Add(-90*time.Minute), "success"), run(1, now.Add(-3*time.Hour), "success")},
			map[int64][]byte{3: []byte("[]"), 2: []byte("[]"), 1: []byte(written)}, "healthy"},
		"a failed run": {[]forge.WorkflowRun{run(2, now.Add(-time.Hour), "failure")}, nil, "inconclusive"},
	} {
		t.Run(name, func(t *testing.T) {
			g := monitorRig(t)
			cl := g.r.Cfg.Classes["dead-links"]
			cl.Monitor.OracleRuns = 2
			g.r.Cfg.Classes["dead-links"] = cl
			g.setFindings(t, "3buqdlot", v1alpha1.Finding{File: testFile, URL: "https://a", Archive: "https://web.archive.org/web/2020/https://a"})
			g.f.SetRuns(c.runs, c.arts)
			if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Monitor.Oracle != c.want {
				t.Fatalf("%+v", tk.Status.Monitor)
			}
		})
	}
}
```

The artifacts are the findings JSON bytes themselves: the fake's `RunArtifact` returns what was
stored, raw (`fake.go:504-512`), and no `zipOf` exists (M7; `probe_test.go` in the reconciler is the
stop probe). Run 1 predates the merge and is ignored. The fake forge gains
`SetAncestor(ancestor, sha string, ok bool)` (default true).

- [ ] **Step 2: Run.** `ulimit -v 4000000; timeout 300 go test ./internal/factory/reconciler/ -run 'FluxSignal|OracleSignal' -p 1` — Expected: FAIL, `g.r.Flux undefined`.

- [ ] **Step 3: Implement.** `monitor.go`:
- `oracleSignal`: `WorkflowRuns(cl.Oracle.Workflow)`, keep `intake.DetectorRun` runs created after
  `m.Since`, oldest first, not yet in `m.OracleRuns`. A run with conclusion other than `success` →
  `inconclusive`. Else read its findings (`RunArtifact`, `intake.ParseFindings`); any `intake.Gone` finding whose
  file is a task finding's and whose URL is that finding's archive or its URL → `regression`;
  append the run ID; `len(m.OracleRuns) >= OracleRuns` → `healthy`; none within 72 h of the merge
  → `inconclusive`. A refused artifact (`errors.Is(err, intake.ErrFindingsRefused)`) is
  `inconclusive`, never an error that blocks the other signals.
- `fluxSignal`: `r.Flux == nil` → `inconclusive`. `Source`: suspended → `inconclusive`; revision
  not the merge commit and `IsAncestor(merge, revision)` false → still open, `inconclusive` once
  `within` has passed since the merge; else `m.Applied` = the source's `At` (first time only);
  `NotReadySince(f.Namespace(), f.Name(), m.Applied)` (`f` = `cl.Monitor.Flux`) non-empty → `regression` (its names in the
  narration; only the source's own objects, L7); `within` past
  `m.Applied` → `healthy`.
`monitor` calls both when declared, before the follow-ups.

`internal/app/factory.go`: `Flux: fluxReader(cfg, mgr)` on the Reconciler, where `fluxReader`
returns `fluxwatch.Reader{C: mgr.GetAPIReader()}` when any repository's class declares
`monitor.flux`, else nil. The Reconciler field's type is the interface above (nil-safe).

`rbac.yaml`, at the end:

```yaml
{{- if .Values.monitor.flux.enabled }}
---
# The post-merge monitor's Flux signal (merge gate classes MC7, R34): read-only, uncached, three
# kinds. Nothing else of the cluster.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: agent-factory-flux-reader
  labels: {{- include "agent-factory.labels" . | nindent 4 }}
rules:
  - apiGroups: [kustomize.toolkit.fluxcd.io]
    resources: [kustomizations]
    verbs: [get, list]
  - apiGroups: [helm.toolkit.fluxcd.io]
    resources: [helmreleases]
    verbs: [get, list]
  - apiGroups: [source.toolkit.fluxcd.io]
    resources: [gitrepositories]
    verbs: [get, list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: agent-factory-flux-reader
  labels: {{- include "agent-factory.labels" . | nindent 4 }}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: agent-factory-flux-reader}
subjects:
  - {kind: ServiceAccount, name: agent-factory, namespace: {{ .Release.Namespace }}}
{{- end }}
```

`values.yaml`: `monitor: {flux: {enabled: false}}` with a comment naming R34. `render.sh`: by
default no `ClusterRole` renders (`ynone '[select(.kind == "ClusterRole")] | .[]'`); a second render
with `--set monitor.flux.enabled=true` has exactly the three rules, each `[get, list]`.

- [ ] **Step 4: Run.**
  `ulimit -v 4000000; KUBEBUILDER_ASSETS=/home/smana/.local/share/kubebuilder-envtest/k8s/1.36.2-linux-amd64 timeout 1200 go test ./internal/... -p 1`
  then `timeout 300 task chart:check` — Expected: `ok` for every package; the chart check passes.

- [ ] **Step 5: Commit.**

```bash
git add internal charts
git commit -m "feat(factory): the monitor's detector and Flux signals, the read-only Flux ClusterRole off by default"
```

### Task 9: Docs, the gate, PR F and the release

- [ ] **Step 1: Docs.** `docs/factory.md`'s Monitor section gains the oracle and Flux rows, R32's
  rule, R33's "reached", R34's RBAC and the chart value; the chart README's values table gains
  `monitor.flux.enabled`.
- [ ] **Step 2: The gate.** `timeout 600 task lint` (Expected: 0 issues), `timeout 300 task crd:check`,
  `timeout 300 task chart:check`, then with Docker started `timeout 2400 task check` — Expected: exit 0.
- [ ] **Step 3: Commit, push, PR F.**

```bash
git add docs charts/agent-factory/README.md
git commit -m "docs(factory): the monitor's signals and its Flux RBAC"
git fetch origin && git rebase origin/main && git push -u origin feat/merge-monitor-signals
gh pr create --title "feat(factory): the monitor's detector and Flux signals (merge gate F1 PR F)" --body "Merge gate classes F1, second PR (MC7): the oracle signal reads the detector's scheduled runs after a merge through the probe's readers; the Flux signal reads the factory's cluster through an uncached, read-only reader, rendered only with monitor.flux.enabled (default false). Offline proof 6. task check: exit 0."
```

- [ ] **Step 4: Release**, once PR F is merged: the repository's release procedure (tag the next
  minor after the newest release, `vX.Y.0`; the release workflow builds the image and the chart).
  Record the version: PR G pins it.

---

## F1, PR G — ai-native-ref: the monitor declared, its alert and runbook 15's step

Prerequisite: the combined pin PR (first-slice PR D: `dead-links` declared off, runbook 15) is on
ai-native-ref's `main`: met, #110 (and #112 pins v0.20.0).

### Task 10: Pin, config and the regression alert

**Files:**
- Modify: `flux/sources/ocirepo-agent-factory.yaml`, `tooling/base/agent-factory/helm-values-configmap.yaml`, `observability/base/agent-platform/vmrule-agent-factory.yaml`
- Create: `scripts/ci/tests/test-agent-merge-regression-alert.sh`
- Scratch, not committed: a worktree of agent-platform at F's tag and a one-off `zz_parse_test.go` in it (M16)

- [ ] **Step 1: Write the failing suite** (offline proof 13), modelled on
  `test-agent-cache-alerts.sh` (annotations stripped, group `agent-factory`):

```bash
#!/usr/bin/env bash
# requires: promtool python3
#
# Merge gate classes MC7, offline proof 13: one regression verdict fires
# AgentFactoryMergeRegression; healthy and inconclusive verdicts never do.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
python3 -c 'import yaml' 2>/dev/null || { echo "SKIP: pyyaml not installed"; exit 77; }
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
ROOT="$ROOT" OUT="$tmp/rules.yaml" python3 - <<'PY' || exit 1
import os, sys, yaml
path = os.path.join(os.environ["ROOT"], "observability/base/agent-platform/vmrule-agent-factory.yaml")
rules = [r for d in yaml.safe_load_all(open(path)) if d and d.get("kind") == "VMRule"
         for g in d["spec"]["groups"] for r in g["rules"] if r.get("alert") == "AgentFactoryMergeRegression"]
if len(rules) != 1:
    sys.exit("AgentFactoryMergeRegression: want exactly one rule")
rules[0].pop("annotations", None)
yaml.safe_dump({"groups": [{"name": "t", "rules": rules}]}, open(os.environ["OUT"], "w"), sort_keys=False)
PY
S='agent_factory_monitor_verdicts_total{class="dead-links", repository="Smana/cloud-native-ref"'
cat >"$tmp/tests.yaml" <<EOF
rule_files: [rules.yaml]
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - {series: '${S}, verdict="regression"}', values: '0 0 1 1 1'}
      - {series: '${S}, verdict="healthy"}', values: '0 1 2 3 4'}
    alert_rule_test:
      - {eval_time: 3m, alertname: AgentFactoryMergeRegression, exp_alerts: [{exp_labels: {severity: warning, class: dead-links, repository: Smana/cloud-native-ref}}]}
  - interval: 1m
    input_series:
      - {series: '${S}, verdict="inconclusive"}', values: '0 1 2 3 4'}
    alert_rule_test:
      - {eval_time: 3m, alertname: AgentFactoryMergeRegression, exp_alerts: []}
EOF
cd "$tmp" && promtool test rules tests.yaml
```

- [ ] **Step 2: Run.** `git add scripts/ci/tests/test-agent-merge-regression-alert.sh && bash $SCRATCH/check.sh "$PWD"` — Expected: the suite FAILs, `want exactly one rule`.

- [ ] **Step 3: The alert**, after `AgentFactoryAutoMergeReverted`:

```yaml
        - alert: AgentFactoryMergeRegression
          expr: sum by (class, repository) (increase(agent_factory_monitor_verdicts_total{verdict="regression"}[15m])) > 0
          labels:
            severity: warning
          annotations:
            summary: "The post-merge monitor found a regression after a {{ $labels.class }} merge"
            description: "The class is demoted at its next decision; nothing was reverted (MC7). The PR carries factory/monitor:regression and the monitor's comment says which signal."
            runbook_url: "https://github.com/Smana/ai-native-ref/blob/main/docs/runbooks/agent-factory/15-dead-links.md"
            dashboard: "https://grafana.${private_domain_name}/d/agent-factory"
```

(`test-agent-alert-annotations.sh` owns the annotations: keep all four.)

- [ ] **Step 4: Pin and configure.** Read the digests of F's release
  (`regctl image digest ghcr.io/smana/charts/agent-factory:X.Y.0`, `regctl image digest ghcr.io/smana/agent-factory:vX.Y.0`)
  and write them as the first slice's Task 12 did. In the values: `monitor: {flux: {enabled: true}}`
  at the top level (the gcp-0 factory reads the host's Flux; aws-0 renders the same read-only
  ClusterRole); for **both** repositories `merge.awaitingCIFor: 24h` and
  `breaker: {window: 10, maxReverts: 1, maxFollowUps: 1}`; on the **host's** `dead-links`
  `monitor: {oracleRuns: 2, followUpWindow: 168h}` (no `flux`: Markdown deploys nothing).
  ai-native-ref's `dead-links` gets no `monitor` here: its `dead-links.yaml` does not exist until
  PR L, which declares it (Task 23, L16). No `monitor` on `docs-links` or `revert` (R29).

- [ ] **Step 5: Run.** `bash $SCRATCH/check.sh "$PWD"` — Expected: the alert suite passes, the pins and parity suites print `0 problem(s)`, the log ends `exit=0`.

  Then the strict parse (M16): no offline gate here parses the factory config with the factory's
  decoder (`test-agent-factory-pins.py` reads tags, digests and `renovate.json` only), and an
  unknown key (`awaitingCIFor`, `maxFollowUps`, `monitor` on an older binary, or a typo) fails only
  at rollout (`Parse`, `config.go:800-810`, `DisallowUnknownFields`). Run it once from the pinned
  tag's worktree and quote its output in the PR body:

```bash
cfg="$SCRATCH/factory-config.yaml"
yq '.data["values.yaml"]' tooling/base/agent-factory/helm-values-configmap.yaml | yq '.config' |
  sed -e 's/\${cluster_name}/gcp-0/g' -e 's/\${private_domain_name}/example.org/g' \
    -e 's|\${identity_provider_url}|https://auth.example.org|g' >"$cfg"
git -C /home/smana/Sources/agent-platform fetch --tags origin
git -C /home/smana/Sources/agent-platform worktree add "$SCRATCH/ap-parse" vX.Y.0
cat >"$SCRATCH/ap-parse/internal/factory/config/zz_parse_test.go" <<'EOF'
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"testing"
)

// TestParse parses $FACTORY_CONFIG with the factory's strict decoder (one-off, never committed).
func TestParse(t *testing.T) {
	raw, err := os.ReadFile(os.Getenv("FACTORY_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(raw); err != nil {
		t.Fatal(err)
	}
}
EOF
(cd "$SCRATCH/ap-parse" && ulimit -v 4000000 && FACTORY_CONFIG="$cfg" timeout 300 go test ./internal/factory/config -run '^TestParse$' -count=1 -v)
git -C /home/smana/Sources/agent-platform worktree remove --force "$SCRATCH/ap-parse"
```

  Expected: `--- PASS: TestParse`, `ok`. A failure names the key: fix the values, not the test.
  PRs J and L repeat this step at their own pins (Tasks 19, 23).

- [ ] **Step 6: Commit.**

```bash
git add flux/sources/ocirepo-agent-factory.yaml tooling/base/agent-factory/helm-values-configmap.yaml \
  observability/base/agent-platform/vmrule-agent-factory.yaml scripts/ci/tests/test-agent-merge-regression-alert.sh
git commit -m "feat(agent-factory): pin the monitor's release, declare it on dead-links, alert on a regression"
```

### Task 11: Runbook 15's monitor step, the docs, PR G

- [ ] **Step 1: Runbook 15** (`docs/runbooks/agent-factory/15-dead-links.md`): the prerequisite
  at l.11, "agent-platform v0.19.0 or later pinned", names the version pinned in Task 10 (L3); a
  new prerequisite beside it: the merger App's installation grants `issues: write` on both
  repositories (GitHub → the App's installation → Permissions; accept the update if GitHub asks),
  since the monitor's labels are written in shadow too (B1). L2's step 3 at l.43, "After the
  post-merge monitor's release, its verdict.", becomes: after two scheduled detector runs, `kubectl -n agent-system get task <id> -o jsonpath='{.status.monitor}'`
  shows `oracle: healthy` and `main: healthy`, and the PR carries `factory/merged:dead-links`; the
  verdict comment arrives at 168 h (a drill does not wait for it); `factory/revert` then reverts
  and the next decision reads `class_paused`. A section *What the monitor never does*: revert on
  its own, widen a class.
- [ ] **Step 2: Docs.** `website/content/docs/platform/ai-platform/agents/factory.md`: a paragraph
  "After a merge" (the signals, the labels, the alert); `status.md`'s "More auto-merge classes" row:
  "The post-merge monitor watches `dead-links`' merges (vX.Y.0)". The spec's status line gains
  "F1–F3 planned in agent-platform `docs/superpowers/plans/2026-10-23-merge-gate-followups-plan.md`; rulings R27–R50".
- [ ] **Step 3: The gate** (offline proof 12). `bash $SCRATCH/check.sh "$PWD"` — Expected: `exit=0`.
- [ ] **Step 4: Commit, push, PR G.**

```bash
git add docs website/content/docs/platform/ai-platform docs/superpowers/specs/2026-10-22-merge-gate-classes-design.md
git commit -m "docs(agent-factory): the post-merge monitor in runbook 15 and the factory docs"
git fetch origin && git rebase origin/main && git push -u origin feat/merge-monitor-declared
gh pr create --title "feat(agent-factory): the post-merge monitor declared on dead-links (merge gate F1)" --body "Pins agent-platform vX.Y.0 (the monitor). merge.awaitingCIFor 24h and breaker.maxFollowUps 1 on both repositories; the host's dead-links monitor (two detector runs, 7 days of follow-ups; ai-native-ref's arrives with its dead-links.yaml); the read-only Flux ClusterRole value; AgentFactoryMergeRegression and its promtool suite (offline proof 13); runbook 15's monitor step and its issues: write prerequisite. dead-links stays declared off. task check: exit 0. Strict parse at vX.Y.0: <the TestParse output>."
```

---

## F2 — `deps-patch`: what qualifies, what proves it, what stops it

This is the riskiest new class: a dependency bump changes what runs, not what is read. Everything
below is fail-closed; the class ships declared off and its first state is shadow, where Renovate
keeps merging and the factory only records (MC12).

| Question | Answer at w0 |
|---|---|
| Source | Renovate's open PRs on the host only (ai-native-ref has no Renovate; its pins are gate paths). The factory authors no bump (MC10) |
| Author | `renovate[bot]`, every commit's author and every push to the branch since its creation by that login, head branch `^renovate/` (R40) |
| Ecosystems | Whatever Renovate's managers write into the allowed files: Flux `HelmRelease` chart versions, `OCIRepository` and `GitRepository` tags (`flux` manager), image tags with or without a digest in manifests and Helm values (`helm-values`, `kubernetes`), Crossplane package references outside the gate list (`crossplane`). Not at w0: `mise.toml` (CI tools), `container-images/` (Dockerfile ARGs, npm `package.json`), `opentofu/` (Terraform, Terramate, OpenBao), `.github/` (actions, a gate path), `crds/`, any lockfile |
| Files | `allowedFiles: ['^(apps|flux|infrastructure|observability|security|tooling)/.+\.ya?ml$']` (R42), minus the gate list (policy) and `protectedFiles`: the never-automerge files the parity suite derives from `renovate.json` (offline proof 16). `modifyOnly: true`, `maxPatchFiles: 3`, `maxModifiedLines: 10` |
| Diff shape | `semver-patch` computed from the diff (R41): line-for-line, only version tokens and a digest beside its tag change, each version `x.y.z → x.y.z'` with `z' > z`, no pre-release or build suffix. Renovate's labels and title are never read. A lockfile, a digest-only update, a minor or a major refuses `class_mismatch` |
| Release age | `releaseAgeCheck: renovate/stability-days`, `SUCCESS` posted by `renovate[bot]` (R44). 1 day in shadow (Renovate's setting today), 3 days at the live switch (PR K) |
| Proof before the merge | The base, unchanged: all 8 required checks green on the head, among them "Kubernetes validation ☸" (the whole tree rendered and schema-checked) and "Rendered manifest diff 📝"; TruffleHog; policy-bot `SUCCESS` from its rule (the `renovate` rule with the gate list, R47; at live `low-risk: deps-patch`, R48); no maintainer approval; the release age; the diff shape and fence above. There is no holdout check: no PR-time job starts what a bump deploys. That is why the post-merge proof matters |
| Proof after the merge | The monitor (F1): main's `verifyChecks` for 30 min (red opens the revert for an auto-merge, §6.4), Flux on gcp-0 for 1 h after the source applies the merge (`monitor.flux`), follow-up fixes for 7 days |
| Breaker and budget | The repository's breaker, per class: `window: 10`, `maxReverts: 1`, `maxFollowUps: 1` (one revert, or one follow-up or regression, in the last 10 merges demotes; GitHub's labels keep it across a rebuild). Calibration budget 10% (M6): shadow → live needs ≥ 20 shadow decisions merged unchanged at their `head=`, 0 bad among them once mature (7 days), and a passed drill (L4) |
| Caps | `maxPerDay: 2`; the repository's `autoMergesPerDay` (1 today) is raised by the owner in PR K, never by this plan |
| Revert | The merger's revert merges unattended only once the host's policy carries a revert rule for the lane's files (PR K); before that a maintainer merges it |
| Start | Shadow, at the next gcp-0 rebuild (M3), with Renovate's native auto-merge untouched: the factory posts `would-arm head=<sha>`, Renovate merges, the monitor watches. A Renovate merge before the factory decided is not counted |
| Minors | `deps-minor: {shadow: true}` beside it, shape `semver-minor`, which Validate never lets go live (R43, M2) |

## F2, PR H — agent-platform: adopted pull requests and `semver-patch`

### Task 12: The `adopt` source, the class keys, their validation

**Files:**
- Modify: `api/factory/v1alpha1/task_types.go` (+ `task crd:gen`), `internal/factory/config/classes.go`
- Test: `internal/factory/config/classes_test.go`, `internal/factory/reconciler/adopt_envtest_test.go` (create, M10)

**Interfaces:**
- Produces: `Source.Kind` enum `issue;runlore;schedule;api;adopt` (M10: `api` is #196's and stays); `config.Author{Adopt *config.Adopt}` (`json:"author,omitempty"` on `Class`), `config.Adopt{Logins, Branches []string}`; `Class.ReleaseAgeCheck string`; `config.DiffShapes()` = `[links-only semver-patch semver-minor]`; `(config.Class).Adopted() bool`.

- [ ] **Step 1: Write the failing tests:**

```go
const depsPatch = `deps-patch: {author: {adopt: {logins: ["renovate[bot]"], branches: ['^renovate/']}}, diffShape: semver-patch,
    allowedFiles: ['^(apps|flux|infrastructure|observability|security|tooling)/.+\.ya?ml$'], protectedFiles: ['^security/base/zitadel/'],
    modifyOnly: true, maxPatchFiles: 3, maxModifiedLines: 10, releaseAgeCheck: renovate/stability-days, maxPerDay: 2}`

// MC10, MC11: an adopted class parses; author absent is the agents mode.
func TestAdoptedClassParses(t *testing.T) {
	c, err := Parse([]byte(withClasses(t, "docs-links: {}\n  revert: {}\n  "+depsPatch)))
	if err != nil {
		t.Fatal(err)
	}
	cl := c.RepositoryFor("Smana/cloud-native-ref").Classes["deps-patch"]
	if !cl.Adopted() || cl.Author.Adopt.Logins[0] != "renovate[bot]" || cl.ReleaseAgeCheck != "renovate/stability-days" || cl.DiffShape != "semver-patch" {
		t.Fatalf("%+v", cl)
	}
	if (Class{}).Adopted() {
		t.Fatal("no author is the agents mode")
	}
}

func TestBadAdoptionFails(t *testing.T) {
	base := "docs-links: {}\n  revert: {}\n  "
	for name, c := range map[string]struct{ classes, err string }{
		"semver without adopt":      {base + "x: {diffShape: semver-patch, modifyOnly: true}", "class x: diffShape semver-patch needs author.adopt (MC10)"},
		"adopt without semver":      {base + "x: {author: {adopt: {logins: [a], branches: ['^r/']}}}", "class x: author.adopt needs diffShape semver-patch or semver-minor"},
		"no logins":                 {base + "x: {author: {adopt: {logins: [], branches: ['^r/']}}, diffShape: semver-patch, modifyOnly: true}", "class x: author.adopt needs logins and branches"},
		"an unanchored branch":      {base + "x: {author: {adopt: {logins: [a], branches: ['renovate/']}}, diffShape: semver-patch, modifyOnly: true}", `class x: author.adopt.branches "renovate/" must start with ^`},
		"semver without modifyOnly": {base + "x: {author: {adopt: {logins: [a], branches: ['^r/']}}, diffShape: semver-patch}", "class x: a semver shape needs modifyOnly: true"},
		"a minor class live":        {base + "x: {live: true, author: {adopt: {logins: [a], branches: ['^r/']}}, diffShape: semver-minor, modifyOnly: true}", "class x: semver-minor is shadow only (M2)"},
		"release age, agents":       {base + "x: {releaseAgeCheck: renovate/stability-days}", "class x: releaseAgeCheck needs author.adopt"},
		"adopt with an oracle":      {base + "x: {author: {adopt: {logins: [a], branches: ['^r/']}}, diffShape: semver-patch, modifyOnly: true, holdoutChecks: [h], oracle: {probe: dead-links, workflow: d.yaml, maxFindings: 1}}", "class x: an oracle reads link targets: diffShape must be links-only"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(withClasses(t, c.classes))); err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("got %v, want %q", err, c.err)
			}
		})
	}
}
```

The oracle case carries `holdoutChecks: [h]` (M11): the oracle `switch` (`classes.go:115-133`)
returns its first match, and without holdouts that is "an oracle needs holdoutChecks", not the
shape message this case is about.

M10: the enum keeps `api` (#196), and an adopted Task must pass the Source CEL rules #196 added
(`task_types.go:242-246`: a sponsor only on `api`, the kind never changes). A new
`internal/factory/reconciler/adopt_envtest_test.go`, on `sponsored_envtest_test.go`'s pattern:

```go
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
)

// M10, R39: an adopt Task, with no sponsor, no ceiling and no template, passes the Source rules,
// and its kind never changes once written.
func TestEnvtestAnAdoptedTask(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset: run `task test`, which sets it")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true}
	restCfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = env.Stop() }()
	c, err := client.New(restCfg, client.Options{Scheme: scheme()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agent-system"}}); err != nil {
		t.Fatal(err)
	}
	tk := issueTask("aaaaaaae", 30, "Adopted pull request #30 from renovate[bot] (renovate/kyverno-3.x).")
	tk.CreationTimestamp = metav1.Time{}
	tk.Spec.Template = ""
	tk.Spec.Source = v1alpha1.Source{Kind: "adopt", Ref: "Smana/cloud-native-ref#30", Key: "adopt:Smana/cloud-native-ref#30",
		RequestedBy: "github:renovate[bot]", Trust: "untrusted", ContentSHA256: strings.Repeat("a", 64)}
	if err := c.Create(ctx, tk); err != nil {
		t.Fatalf("an adopted task is refused: %v", err)
	}
	tk.Spec.Source.Kind = "issue"
	if err := c.Update(ctx, tk); err == nil || !strings.Contains(err.Error(), "a task's source kind never changes") {
		t.Fatalf("got %v, want the kind rule", err)
	}
}
```

- [ ] **Step 2: Run.** `ulimit -v 4000000; timeout 300 go test ./internal/factory/config/ -run 'Adopt' -p 1` — Expected: FAIL, `unknown field "author"`.

- [ ] **Step 3: Implement.** On `Class`:

```go
	// Author is how the class's pull requests are authored (MC1's author row): absent, the agents
	// App through a task's runs; Adopt, another author's pull requests the factory adopts (MC10).
	Author *Author `json:"author,omitempty"`
	// ReleaseAgeCheck names the commit status that holds the release age (MC11), posted by an
	// adopted login: Renovate's renovate/stability-days.
	ReleaseAgeCheck string `json:"releaseAgeCheck,omitempty"`
```

```go
// Author is a class's author mode.
type Author struct {
	Adopt *Adopt `json:"adopt,omitempty"`
}

// Adopt names the logins whose pull requests are adopted and the head branches they come from
// (gh-aw's allowed-branches); every commit and every push of the branch must be theirs.
type Adopt struct {
	Logins   []string `json:"logins"`
	Branches []string `json:"branches"`
}

// Adopted: the class's pull requests are another author's, adopted (MC10).
func (c Class) Adopted() bool { return c.Author != nil && c.Author.Adopt != nil }
```

`DiffShapes()` returns the three; `validateClass` gains, after the shape check:

```go
	semver := cl.DiffShape == "semver-patch" || cl.DiffShape == "semver-minor"
	switch {
	case semver && !cl.Adopted():
		bad("class %s: diffShape %s needs author.adopt (MC10)", name, cl.DiffShape)
	case cl.Adopted() && !semver:
		bad("class %s: author.adopt needs diffShape semver-patch or semver-minor", name)
	case semver && !cl.ModifyOnly:
		bad("class %s: a semver shape needs modifyOnly: true", name)
	case cl.DiffShape == "semver-minor" && cl.Live:
		bad("class %s: semver-minor is shadow only (M2)", name)
	case cl.ReleaseAgeCheck != "" && !cl.Adopted():
		bad("class %s: releaseAgeCheck needs author.adopt", name)
	}
	if cl.Adopted() {
		a := cl.Author.Adopt
		if len(a.Logins) == 0 || len(a.Branches) == 0 {
			bad("class %s: author.adopt needs logins and branches", name)
		}
		for _, b := range a.Branches {
			if _, err := regexp.Compile(b); err != nil || !strings.HasPrefix(b, "^") {
				bad("class %s: author.adopt.branches %q must start with ^", name, b)
			}
		}
	}
```

`task_types.go`: the `Kind` marker becomes `// +kubebuilder:validation:Enum=issue;runlore;schedule;api;adopt`
(l.248; `adopt` after `api`, which stays), its comment "adopt: another author's pull request
the factory decides (merge gate MC10)", `Ref`'s comment gains `owner/name#<n>`, `RequestedBy`'s
`github:<login>`. `task crd:gen`.

- [ ] **Step 4: Run.** `ulimit -v 4000000; timeout 300 go test ./internal/factory/config/ ./api/... -p 1 && KUBEBUILDER_ASSETS=/home/smana/.local/share/kubebuilder-envtest/k8s/1.36.2-linux-amd64 timeout 600 go test ./internal/factory/reconciler/ -run 'TestEnvtestAnAdoptedTask|TestEnvtestSponsoredTask' -p 1 && timeout 300 task crd:check` — Expected: `ok` three times (the sponsored envtest still green: `api` kept), the CRD check passes.

- [ ] **Step 5: Commit.**

```bash
git add api internal/factory/config internal/factory/reconciler/adopt_envtest_test.go charts/agent-factory/crds config
git commit -m "feat(config): adopted classes, the semver shapes and the release age check"
```

### Task 13: `semverdiff`

**Files:**
- Create: `internal/factory/semverdiff/semverdiff.go`, `semverdiff_test.go`

**Interfaces:**
- Produces: `semverdiff.Patch(base, head map[string]string) (ok bool, why string)`, `semverdiff.Minor(base, head map[string]string) (bool, string)`.

- [ ] **Step 1: Write the failing table test** (the shapes of offline proof 14, each from a real
  host merge):

```go
// SPDX-License-Identifier: Apache-2.0

package semverdiff

import "testing"

const digA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const digB = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestShapes(t *testing.T) {
	f := "infrastructure/base/x/helmrelease.yaml"
	for name, c := range map[string]struct {
		base, head   string
		patch, minor bool
	}{
		"a chart patch (#2537)":        {"      version: 3.9.1\n", "      version: 3.9.2\n", true, true},
		"a 0.x patch (#2532)":          {"    tag: v0.75.0\n", "    tag: v0.75.1\n", true, true},
		"a tag and its digest":         {"  tag: \"v1.2.3@" + digA + "\"\n", "  tag: \"v1.2.4@" + digB + "\"\n", true, true},
		"a digest alone":               {"  image: x@" + digA + "\n", "  image: x@" + digB + "\n", false, false},
		"a minor (#2471)":              {"      version: 2.12.4\n", "      version: 2.13.0\n", false, true},
		"a major":                      {"      version: 2.9.9\n", "      version: 3.0.0\n", false, false},
		"a downgrade":                  {"      version: 1.2.4\n", "      version: 1.2.3\n", false, false},
		"a pre-release":                {"      version: 1.2.3\n", "      version: 1.2.4-rc.1\n", false, false},
		"text beside the version":      {"      version: 1.2.3 # pinned\n", "      version: 1.2.4 # unpinned\n", false, false},
		"a line added":                 {"a: 1.2.3\n", "a: 1.2.4\nb: 1\n", false, false},
		"a line with no version moved": {"a: 1.2.3\nkey: x\n", "a: 1.2.4\nkey: y\n", false, false},
		"a lockfile integrity line":    {"\"integrity\": \"sha512-AAA\"\n", "\"integrity\": \"sha512-BBB\"\n", false, false},
		"two versions on one line":     {"img: a:1.2.3 b:4.5.6\n", "img: a:1.2.4 b:4.5.7\n", true, true},
		"one of two moves backwards":   {"img: a:1.2.3 b:4.5.6\n", "img: a:1.2.4 b:4.5.5\n", false, false},
		"an added file":                {"", "a: 1.2.3\n", false, false},
	} {
		t.Run(name, func(t *testing.T) {
			base, head := map[string]string{f: c.base}, map[string]string{f: c.head}
			if ok, why := Patch(base, head); ok != c.patch {
				t.Fatalf("Patch %v (%s), want %v", ok, why, c.patch)
			}
			if ok, why := Minor(base, head); ok != c.minor {
				t.Fatalf("Minor %v (%s), want %v", ok, why, c.minor)
			}
		})
	}
	if ok, _ := Patch(map[string]string{}, map[string]string{}); ok {
		t.Fatal("an empty diff is no bump")
	}
}
```

- [ ] **Step 2: Run.** `ulimit -v 4000000; timeout 120 go test ./internal/factory/semverdiff/ -p 1` — Expected: FAIL, `no Go files` / `undefined: Patch`.

- [ ] **Step 3: Implement.**

```go
// SPDX-License-Identifier: Apache-2.0

// Package semverdiff decides whether a diff is a dependency bump of one kind, from the diff alone
// (merge gate classes MC11, R41): a label is a claim, the diff is the change.
package semverdiff

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
)

var (
	versionRE = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)([-+][0-9A-Za-z.+-]*)?`)
	digestRE  = regexp.MustCompile(`sha256:[0-9a-f]{64}`)
)

type version struct {
	major, minor, patch int
	suffix              string
}

// Patch: every changed line moves versions within their major.minor to a higher patch.
func Patch(base, head map[string]string) (bool, string) {
	return check(base, head, func(a, b version) bool { return a.major == b.major && a.minor == b.minor && b.patch > a.patch })
}

// Minor: every changed line moves versions within their major, to a higher minor or patch.
func Minor(base, head map[string]string) (bool, string) {
	return check(base, head, func(a, b version) bool {
		return a.major == b.major && (b.minor > a.minor || b.minor == a.minor && b.patch > a.patch)
	})
}

func check(base, head map[string]string, moves func(a, b version) bool) (bool, string) {
	files := slices.Sorted(maps.Keys(head))
	if len(files) == 0 || !slices.Equal(files, slices.Sorted(maps.Keys(base))) {
		return false, "no file, or a file added or deleted"
	}
	changed := 0
	for _, f := range files {
		if base[f] == "" || head[f] == "" {
			return false, f + ": added or deleted"
		}
		bl, hl := lines(base[f]), lines(head[f])
		if len(bl) != len(hl) {
			return false, f + ": lines added or removed"
		}
		for i := range bl {
			if bl[i] == hl[i] {
				continue
			}
			if ok, why := line(bl[i], hl[i], moves); !ok {
				return false, fmt.Sprintf("%s:%d: %s", f, i+1, why)
			}
			changed++
		}
	}
	if changed == 0 {
		return false, "nothing changed"
	}
	return true, ""
}

// line: the two lines are equal once versions and digests are masked, hold as many of each, at
// least one version moved, every moved version moves as allowed and carries no suffix, and a
// digest changed only where a version moved.
func line(a, b string, moves func(a, b version) bool) (bool, string) {
	if mask(a) != mask(b) {
		return false, "text other than versions and digests changed"
	}
	va, vb := versions(a), versions(b)
	da, db := digestRE.FindAllString(a, -1), digestRE.FindAllString(b, -1)
	if len(va) != len(vb) || len(da) != len(db) {
		return false, "a version or digest added or removed"
	}
	moved := false
	for i := range va {
		if va[i] == vb[i] {
			continue
		}
		if va[i].suffix != "" || vb[i].suffix != "" || !moves(va[i], vb[i]) {
			return false, "a version moved outside the shape"
		}
		moved = true
	}
	if !moved {
		return false, "no version moved (a digest alone is a digest update)"
	}
	return true, ""
}
```

`mask` replaces every `digestRE` then every `versionRE` match with `\x00D` and `\x00V`; `versions`
parses `versionRE`'s submatches with `strconv.Atoi` (an overflow is no version: return a version
that never equals, so the line refuses); `lines` splits on `\n` keeping a final empty line out.

- [ ] **Step 4: Run.** Same command — Expected: `ok`.

- [ ] **Step 5: Commit.**

```bash
git add internal/factory/semverdiff
git commit -m "feat(factory): semver-patch and semver-minor computed from the diff"
```

### Task 14: The Renovate replay (offline proof 14)

**Files:**
- Create: `hack/renovate-replay.sh`, `internal/factory/semverdiff/replay_test.go`, `internal/factory/semverdiff/testdata/replay/*.json` (8 committed cases)

- [ ] **Step 1: The fetcher.** `hack/renovate-replay.sh <owner/repo> <since> <out-dir>`: for every
  Renovate PR merged since `<since>` (`gh pr list --author app/renovate --state merged --search "merged:>=<since>" --limit 400 --json number,body,files,mergeCommit`),
  write `<out-dir>/<n>.json` = `{"number", "update", "base": {path: text}, "head": {path: text}}`,
  where `update` is the first cell of the "Update" column of Renovate's table in the body
  (`patch`, `minor`, `major`, `digest`, `pin`, …; `unknown` when absent), `head` the files at the
  merge commit and `base` at its first parent (`gh api repos/…/contents/<path>?ref=<sha>`, base64).
  It needs `gh` and `jq` and reaches GitHub: run it by hand, never in CI.
- [ ] **Step 2: The test.** `replay_test.go` reads `testdata/replay/*.json` always, and
  `$SEMVER_REPLAY_DIR/*.json` when that variable is set; for each case: `Patch` must refuse every
  `minor`, `major`, `digest` and `pin`; it may accept only `patch` ones; it logs per update type how
  many it accepted. The committed cases: #2537 (patch, accepted), #2532 (patch, 0.x, accepted),
  #2530 (patch, accepted), #2471 (minor, two files, refused), #2380 (digest, refused), #2386
  (digest, refused), #2392 (python 3.14 → 3.15 image tag, refused: not three-part), #2454 (patch
  across `opentofu/` and decisions docs: `Patch` decides the shape only, the fence refuses it),
  fetched with the script then trimmed to the changed files.
- [ ] **Step 3: Run the committed cases.** `ulimit -v 4000000; timeout 120 go test ./internal/factory/semverdiff/ -run Replay -v -p 1` — Expected: PASS, "patch accepted 4 of 5, minor 0 of 1, digest 0 of 2, other 0 of 1" (adjust the expected line to the eight fixtures when written).
- [ ] **Step 4: Run the whole replay** (the proof, by hand):
  `hack/renovate-replay.sh Smana/cloud-native-ref 2026-08-18 $SCRATCH/replay && ulimit -v 4000000 && SEMVER_REPLAY_DIR=$SCRATCH/replay timeout 600 go test ./internal/factory/semverdiff/ -run Replay -v -p 1`
  Expected: PASS; 0 minor, major or digest accepted; the log's accepted patch count and, through
  `TestReplayWithinLimits` (the same cases through the w0 fence: allowed files, ≤ 3 files, ≤ 10
  lines, `modifyOnly`), how many the lane would have taken. Paste the summary into PR H's
  description; the design measured 75 patch merges, 62 outside the gate paths.
- [ ] **Step 5: Commit.**

```bash
git add hack/renovate-replay.sh internal/factory/semverdiff
git commit -m "test(factory): replay the host's Renovate merges through semver-patch"
```

### Task 15: The adopted author mode and the release age at the gate

**Files:**
- Modify: `internal/factory/reconciler/arm.go`, `gate.go` (`declared`, `diffShape`, `releaseAge`), `merge.go` (`awaitingCI`: no `reportedHead` nor `verifiers` for an adopted task, M12; `case "ci_red"` and `case "ci_cancelled"`, L15), `internal/factory/narrate/narrate.go`, `internal/summary/waiting.go`
- Test: `internal/factory/reconciler/adopt_gate_test.go`

**Interfaces:**
- Consumes: Task 12's `Class.Adopted`, Task 13's shapes.
- Produces: `ArmInputs.Adoption struct{ Read bool; CommitAuthors []string; Pushed pushCheck }`; reasons `not_adopted_author`, `branch_not_allowed`, `release_age_pending` (wait), `release_age_red`; `diffShape` handles `semver-patch` and `semver-minor` on `in.Files`; `unreported` adds the release-age status.

- [ ] **Step 1: Write the failing tests** (offline proof 15):

```go
// SPDX-License-Identifier: Apache-2.0

package reconciler

// adoptRig: an adopted Task for Renovate's PR 30 on renovate/kyverno-3.x, a patch bump of one
// allowed file, CI and policy green, the release age passed, every commit and push Renovate's.
func adoptRig(t *testing.T) *rig { … }

// Offline proof 15: a second committer, a branch outside renovate/*, a stranger's push or a
// pending renovate/stability-days refuses or waits; a minor goes nowhere near deps-patch.
func TestAdoptedGate(t *testing.T) {
	for name, c := range map[string]struct {
		edit          func(g *rig)
		phase, reason string
	}{
		"clean, shadow": {func(*rig) {}, v1alpha1.PhaseAwaitingHuman, "shadow_would_arm"},
		"a second committer": {func(g *rig) {
			g.f.SetCommits(30, forge.Commit{SHA: "c1", Author: "renovate[bot]"}, forge.Commit{SHA: headSHA, Author: "Smana"})
		}, v1alpha1.PhaseAwaitingHuman, "human_commit_in_pr"},
		"a stranger's push": {func(g *rig) {
			g.f.SetActivity("renovate/kyverno-3.x", forge.Push{Type: "push", Actor: "Smana", Before: "c1", After: headSHA},
				forge.Push{Type: "branch_creation", Actor: "renovate[bot]", After: "c1"})
		}, v1alpha1.PhaseAwaitingHuman, "human_commit_in_pr"},
		"another branch": {func(g *rig) { g.setHeadRef(30, "feature/x") }, v1alpha1.PhaseAwaitingHuman, "branch_not_allowed"},
		"another author": {func(g *rig) { g.setAuthor(30, "dependabot[bot]") }, v1alpha1.PhaseAwaitingHuman, "not_adopted_author"},
		"release age pending": {func(g *rig) { g.setStatus(30, "renovate/stability-days", "PENDING", "renovate[bot]") },
			v1alpha1.PhaseAwaitingCI, "release_age_pending"},
		"release age by a stranger": {func(g *rig) { g.setStatus(30, "renovate/stability-days", "SUCCESS", "Smana") },
			v1alpha1.PhaseAwaitingHuman, "release_age_red"},
		"a minor": {func(g *rig) {
			g.f.SetFiles(map[string]string{depFile: "version: 3.8.4\n"}, map[string]string{depFile: "version: 3.9.0\n"})
		}, v1alpha1.PhaseAwaitingHuman, "class_mismatch"},
		"a maintainer approved": {func(g *rig) { g.approve(30, "Smana") }, v1alpha1.PhaseAwaitingHuman, "maintainer_approved"},
	} {
		t.Run(name, func(t *testing.T) {
			g := adoptRig(t)
			c.edit(g)
			tk := g.reconcile(t, adoptTask, 1)
			if tk.Status.Phase != c.phase || tk.Status.Reason != c.reason {
				t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
			}
			if len(g.f.Armed()) != 0 {
				t.Fatal("shadow never merges")
			}
		})
	}
}

// M12: an adopted Task has no room (status.roomRef is ""). On green CI the gate must read none:
// the broker refuses a room named "", and every adopted Task would retry forever.
func TestAnAdoptedTaskReadsNoRoom(t *testing.T) {
	g := adoptRig(t)
	g.r.Rooms = noEmptyRoom{g.r.Rooms}
	if tk := g.reconcile(t, adoptTask, 1); tk.Status.Reason != "shadow_would_arm" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// L15: a cancelled check on Renovate's PR is handed off, never re-run by the factory.
func TestAnAdoptedCancelledCheckHandsOff(t *testing.T) {
	g := adoptRig(t)
	g.cancelOne(30)
	if tk := g.reconcile(t, adoptTask, 1); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "ci_cancelled" ||
		len(g.f.Reruns()) != 0 {
		t.Fatalf("%s %s reruns %v", tk.Status.Phase, tk.Status.Reason, g.f.Reruns())
	}
}

// The agents mode is untouched: an adopted class's evidence is never asked of an agent PR, and
// the existing arm/merge tests stay byte-identical.
```

The rig builds the adopted Task directly (Source kind `adopt`, class `deps-patch` in shadow as the
`depsPatch` fixture of Task 12, `status.pullRequest` set, phase `AwaitingCI`), with the fake's
`SetPR` (author `renovate[bot]`, `HeadRef`), `SetCommits`, `SetActivity` (one `branch_creation`
and one `push`, both Renovate's), `SetChecks` (green, policy-bot SUCCESS, and the
`renovate/stability-days` status by `renovate[bot]`), `SetFiles` (a patch bump of `depFile` =
`security/base/kyverno/helmrelease-controller.yaml`) and `SetFileStats` (modified, 1+1). The small
helpers `setHeadRef`, `setAuthor`, `setStatus`, `approve` edit the fake's PR or checks;
`cancelOne` marks one required check `CANCELLED`, as `cancelled_test.go` does (reuse its helper
and the fake's rerun record, whatever their names there). `noEmptyRoom` embeds the rig's
`RoomLog` and returns an error from every method called with room `""` (`EventsSince`, `LastSeq`,
`Queue`, …): the fake log answers an empty name, the broker does not (M12).

- [ ] **Step 2: Run.** `ulimit -v 4000000; timeout 300 go test ./internal/factory/reconciler/ -run 'TestAdoptedGate|TestAnAdopted' -p 1` — Expected: FAIL, `undefined: adoptRig` once the rig is written; then FAIL on the reasons.

- [ ] **Step 3: Implement.** `arm.go`, `DecideArm` after `baseChecks`:

```go
	class := in.Task.Spec.PredictedClass
	cl := gateOf(rc, class)
	if cl.Adopted() {
		if reason := adoptEvidence(in, cl); reason != "" {
			return human(reason, class)
		}
	} else if !forge.SameLogin(in.PR.Author, in.Cfg.AgentsLogin) {
		return human("not_agent_authored", "")
	}
	for _, rv := range in.PR.Reviews { … unchanged: maintainer_approved … }
	if !cl.Live && !cl.Shadow { … }
	if !cl.Adopted() {
		if reason := agentEvidence(in); reason != "" {
			return human(reason, class)
		}
	}
```

(the class lookup moves above the author check; for an agents class the order of refusals is
today's, which `arm_test.go` holds). `adoptEvidence`: author in `Logins` (`SameLogin`) else
`not_adopted_author`; `HeadRef` matches one of `Branches` (`compile(pats, false)`, then
`MatchString`: Validate made every pattern start with `^`, and a branch pattern is a prefix, so it
is not wrapped in `^(?:p)$` as `allowedFiles` is), else `branch_not_allowed`; until `in.Adoption.Read`, hold
(return ""); then every `CommitAuthors` entry in `Logins` and `Pushed == pushFactory`, else
`human_commit_in_pr`. `gate.go`: `Conditions()` gains, after `diffShape`,
`{"releaseAgeCheck", func(cl) bool { return cl.ReleaseAgeCheck != "" }, releaseAge}`, where
`releaseAge` reads `in.Checks.StatusOf(cl.ReleaseAgeCheck)`: creator not an adopt login →
`release_age_red`; `SUCCESS` → ""; `""` or `PENDING` → (`release_age_pending`, wait); else
`release_age_red`. `diffShape` gains `semver-patch` → `semverdiff.Patch(in.Files.Base, in.Files.Head)`
and `semver-minor` → `semverdiff.Minor`, each refusing `class_mismatch`. `onDiff` is a static key
list and cannot tell the shapes apart (L8): `declared` treats the `diffShape` condition as
on-diff when the class's shape is `semver-patch` or `semver-minor`, so the first, unread pass
holds instead of running `semverdiff.Patch(empty, empty)`, which refuses and would read
`class_mismatch` before any diff was read. `links-only` keeps its vacuous first pass (R52).

`merge.go`, `awaitingCI`: the second pass's condition `ReadsDiff` already holds (a shape is
declared). For an adopted class, before `DecideArm`'s second pass, read
`in.Adoption = {Read: true, CommitAuthors: authors of PullRequestCommits(n), Pushed: r.pushedBy(ctx, pr.HeadRef, logins, pr.HeadSHA)}`
(a truncated commit list → `pushStranger`); `pushedBy` is `factoryPushed` generalised over the
branch and the logins (`factoryPushed(ctx, t, head)` becomes `r.pushedBy(ctx, "agent/"+t.Name, []string{r.Cfg.AgentsLogin}, head)`,
its tests unchanged). The `if d.Verdict == "arm" || "shadow"` block that reads `factoryPushed`
skips adopted classes (their pushes are read above). For an adopted class `awaitingCI` never
calls `reportedHead` (merge.go:329, through `roomTail` on `t.Status.RoomRef`, `""` here) nor
`verifiers(t)`: there is no room and no run (M12); the head is `pr.HeadSHA` as read. `case
"ci_red"` (merge.go:138): an adopted task hands off (`AwaitingHuman`, `ci_red`) instead of
`fixCI` (R45). `case "ci_cancelled"` (l.140): an adopted task hands off (`AwaitingHuman`,
`ci_cancelled`) instead of `rerunCancelled`: re-running Renovate's CI is Renovate's (L15). `narrate.go` and `waiting.go`: prose for the
four reasons (`release_age_pending` in the summary's waits).

- [ ] **Step 4: Run.** `ulimit -v 4000000; KUBEBUILDER_ASSETS=/home/smana/.local/share/kubebuilder-envtest/k8s/1.36.2-linux-amd64 timeout 1200 go test ./internal/factory/... -p 1` — Expected: `ok` everywhere; `sha256sum internal/factory/reconciler/{arm,merge,ledger}_test.go` unchanged from before the task.

- [ ] **Step 5: Commit.**

```bash
git add internal/factory
git commit -m "feat(factory): the adopted author mode and the release age at the merge gate"
```

### Task 16: The adopt poller and the adopted lifecycle

**Files:**
- Create: `internal/factory/intake/adopt.go`
- Modify: `internal/factory/forge/forge.go` + `github.go` + fake (`PRSummary.HeadRef`, `HeadSHA`), `internal/factory/reconciler/reconciler.go` (`received`, `admit`), `internal/factory/reconciler/implement.go` (`awaitingHuman`, `remind`), `internal/factory/reconciler/merge.go` (`changesRequested` path, `case "shadow"`), `internal/factory/reconciler/monitor.go` (`followUps`' adopt-login skip, deferred from Task 5, M3), `internal/factory/intake/schedule.go` (`ScheduleMerger` gains `Diff`, L9), `internal/factory/fmetrics/fmetrics.go` (`intakeSources()`), `api/factory/v1alpha1/person.go` (`Payer()`, L15), `internal/factory/narrate/narrate.go`, `internal/app/factory.go`
- Test: `internal/factory/intake/adopt_test.go`, `internal/factory/reconciler/adopt_lifecycle_test.go`

**Interfaces:**
- Produces: `intake.Adopter{Clients func(repo string) (ScheduleForge, ScheduleMerger, bool); Client client.Client; Namespace string; Cfg *config.Config; Owned func(string) bool; Stopped func(context.Context) bool; Now func() time.Time; Errors errorCounter; Log *slog.Logger}` (leader-only, every minute); `intake.ScheduleMerger` gains `Diff(ctx, base, head string) (forge.Diff, error)` (L9: `conns.schedule` returns a `ScheduleMerger`, so the Adopter takes the same connections; the merger and the fake already implement `Diff`); `const maxOpenAdopted = 10`; `taskid` key `adopt:<repo>#<n>`; `narrate.WouldArmAdopted(t, class, head) Event`.

- [ ] **Step 1: Write the failing tests.** `adopt_test.go`:

```go
// MC10, R39: an open Renovate PR whose diff passes semver-patch becomes one run-less Task of
// deps-patch; a minor goes to deps-minor when it is declared on; anything else, another author,
// another branch, a class off, or the 11th open adopted Task, creates nothing. A second tick
// creates nothing more (the name is the dedup).
func TestAdopter(t *testing.T) {
	for name, c := range map[string]struct {
		pr      forge.PRSummary
		base    string
		head    string
		minorOn bool
		want    string // the class, "" for no Task
	}{
		"a patch":         {renovatePR(30, "renovate/kyverno-3.x"), "version: 3.9.1\n", "version: 3.9.2\n", false, "deps-patch"},
		"a minor, off":    {renovatePR(31, "renovate/es-2.x"), "version: 2.12.4\n", "version: 2.13.0\n", false, ""},
		"a minor, on":     {renovatePR(31, "renovate/es-2.x"), "version: 2.12.4\n", "version: 2.13.0\n", true, "deps-minor"},
		"a digest":        {renovatePR(32, "renovate/x-digest"), "x@" + digA + "\n", "x@" + digB + "\n", false, ""},
		"a human's PR":    {forge.PRSummary{Number: 33, Author: "Smana", HeadRef: "renovate/x", HeadSHA: "h"}, "v: 1.2.3\n", "v: 1.2.4\n", false, ""},
		"another branch":  {forge.PRSummary{Number: 34, Author: "renovate[bot]", HeadRef: "main-x", HeadSHA: "h"}, "v: 1.2.3\n", "v: 1.2.4\n", false, ""},
	} {
		t.Run(name, func(t *testing.T) { … build the Adopter on a fake client and a fake forge, Tick twice, list Tasks … })
	}
}
```

with three more cases in the same file: the class neither live nor shadow (MC14) adopts nothing;
ten non-terminal adopted Tasks block the eleventh; the kill switch (`Stopped`) adopts nothing.
The created Task: `Source{Kind: "adopt", Ref: "Smana/cloud-native-ref#30", Key: "adopt:Smana/cloud-native-ref#30",
RequestedBy: "github:renovate[bot]", Trust: "untrusted"}`, `Text` "Adopted pull request #30 from
renovate[bot] (renovate/kyverno-3.x)." with its `ContentSHA256`, `PredictedClass`, no template, the
repository's default data class.

`adopt_lifecycle_test.go` (R39, R45, R46):

```go
// R39: received moves an adopted Task to AwaitingCI with its pull request, without triage, a
// room, a run, or a place in the daily task cap.
func TestAnAdoptedTaskGoesStraightToTheGate(t *testing.T) { … reconcile once from Received: phase AwaitingCI,
	status.pullRequest.number 30, no AgentRun created, no Room created, the classifier never called;
	with caps.tasksPerDay already reached by other tasks it still goes to AwaitingCI; tk.Payer() is ""
	with perPerson on, so renovate[bot] becomes no principal series (L15) … }

// R45: in AwaitingHuman an adopted Task never reminds nor stale-closes Renovate's PR, and a new
// head (Renovate's rebase) returns it to AwaitingCI; a merge by Renovate ends it Done "merged"
// and, with a monitor declared, under watch.
func TestTheAdoptedLifecycle(t *testing.T) { … 15 days in AwaitingHuman: no comment, PR not closed;
	head moved: AwaitingCI, GateReentries unchanged; MERGED by renovate[bot]: Done, merged, monitorDue … }

// R46: a shadow decision on an adopted PR posts the marker the tally reads, and no verdict claim.
func TestWouldArmAdopted(t *testing.T) { … the comment holds "event=would-arm-0 head=<sha>" and
	"would auto-merge: `deps-patch`", and not "verdict approve" … }
```

Add to Task 5's `TestFollowUps` table the case it deferred (offline proof 4's third clause):
`"the lane's next bump"`, a later PR by `renovate[bot]` on `renovate/foo-2.x` rewriting line 7
(blame `mSHA`), with the rig's class given `Author: &config.Author{Adopt: &config.Adopt{Logins: []string{"renovate[bot]"}, Branches: []string{"^renovate/"}}}`:
not a follow-up (R35). Task 5 left this skip out because `config.Author` did not exist (M3); it
lands now, in `monitor.go`.

- [ ] **Step 2: Run.** `ulimit -v 4000000; timeout 600 go test ./internal/factory/intake/ ./internal/factory/reconciler/ -run 'Adopt|WouldArmAdopted|FollowUps' -p 1` — Expected: FAIL, `undefined: Adopter`.

- [ ] **Step 3: Implement.**
- `forge`: `PRSummary` gains `HeadRef`, `HeadSHA` (`p.GetHead().GetRef()`, `GetSHA()`); the fake keeps what `SetOpenPRs` sets.
- `schedule.go`: `ScheduleMerger` gains `Diff` (L9); every implementer already has it (`forge.Merger`, the fake).
- `monitor.go`, `followUps`: when `cl.Adopted()`, skip a later PR whose author is one of
  `cl.Author.Adopt.Logins` (`forge.SameLogin`), beside the `RevertOfLabel(m)` skip (R35, M3).
- `person.go`: `Payer()` returns `""` for `Spec.Source.Kind == "adopt"`, before the
  `RequestedBy` branch: no run, no spend, and `github:renovate[bot]` is no person (L15).
- `adopt.go`: `Tick` returns at once when `Stopped`; per owned repository with a class that is
  `Adopted()` and live or shadow: count non-terminal Tasks with `Source.Kind == "adopt"` on it
  (stop at `maxOpenAdopted`); `OpenPullRequests`; per PR whose author is an adopt login of some
  such class and whose `HeadRef` matches its branches, and whose Task name
  (`taskid.Name("adopt:" + repo + "#" + n)`) does not exist: `Diff(ctx, "main", HeadSHA)` through the
  repository's merger (an `ErrDiffTooLarge` skips the PR); try the shapes in order `semver-patch`
  then `semver-minor` against the classes declaring them (on, adopted, the PR's author among their
  logins); the first that holds names the class; create the Task (`AlreadyExists` is the dedup).
  A failed tick is counted (`IntakeError(ctx, "adopt")`; `intakeSources()` (fmetrics.go:56) gains `adopt` after `api`) and
  retried. The poller decides membership only: the gate decides everything again on the head it
  reads.
- `reconciler.go`: `admit` (l.670-685) skips the daily task cap for `Source.Kind == "adopt"`, kind-gated
  inside `admit` (the text cap stays); `received` (l.687) for `adopt`: after `admit` and before
  `Triage`, read the PR (`Source.Ref`'s number), set
  `Spec.Template = ""`, `status.pullRequest = {Number, URL, NodeID, HeadSHA, Author}`,
  `ConfigHash`, and `r.to(t, PhaseAwaitingCI, "")`, never calling `Triage`.
- `implement.go`: `awaitingHuman` for an adopted Task: `prEnded` as today; no `revise` (a
  maintainer's Request changes is theirs to act on with Renovate); a head unlike
  `status.pullRequest.headSHA` → `r.to(t, PhaseAwaitingCI, "adopted_head_moved")` (no
  `GateReentries++`); never `remind`.
- `merge.go`: `case "shadow"` narrates `narrate.WouldArmAdopted` for an adopted class.
- `narrate.go`: `WouldArmAdopted` with `WouldArm`'s key and "Agent factory: would auto-merge:
  `%s`, checks green, the diff a %s bump within the class's files and sizes, released %s ago at
  least. The class is in shadow: nothing is merged by the factory."
- `internal/app/factory.go`: the `Adopter` beside the `Scheduler`, with `conns.schedule`'s
  connections (`ScheduleMerger`, now with `Diff`).

- [ ] **Step 4: Run.** `ulimit -v 4000000; KUBEBUILDER_ASSETS=/home/smana/.local/share/kubebuilder-envtest/k8s/1.36.2-linux-amd64 timeout 1200 go test ./internal/... ./api/... -p 1` — Expected: `ok` everywhere.

- [ ] **Step 5: Commit.**

```bash
git add internal api
git commit -m "feat(factory): adopt Renovate's pull requests into run-less tasks, off by default"
```

### Task 17: The tally's `--author`, the docs, PR H and the release

- [ ] **Step 1: The tally.** `shadow-merge-tally.sh` gains `--author <login>` (default
  `app/ogenki-agents`); `scripts/shadow-merge-tally_test.sh` (the existing suite, M8) gains a
  fixture under `scripts/testdata/tally/` of two Renovate PRs with `WouldArmAdopted` comments, one merged by `renovate[bot]` at the judged head
  (`merged_unchanged`), one after a rebase (`changed`): `--class deps-patch --author app/renovate`
  prints `merged_unchanged=1 changed=1`. Run: `timeout 120 task scripts` — Expected: `ok`.
- [ ] **Step 2: Docs.** `docs/factory.md`: "Adopted classes" (the poller, the Task, the gate's
  author mode, the lifecycle, R45), the shapes (R41 verbatim), `releaseAgeCheck`, the reasons, and
  the `deps-patch` table from the top of this F2 section. Under the Monitor section, the merge-commit
  timing (L18): a merge before the factory decides (Renovate's, or a maintainer's) still ends
  `Done "merged"` and is monitored, so the breaker counts outcomes of merges the factory never
  decided, which the tally does not count; a demotion can follow from one.
- [ ] **Step 3: The gate.** `timeout 600 task lint`, `timeout 300 task crd:check`, then with Docker
  started `timeout 2400 task check` — Expected: 0 issues, the check passes, exit 0.
- [ ] **Step 4: Commit, push, PR H.**

```bash
git add scripts docs
git commit -m "docs(factory): adopted classes, the semver shapes, the tally by author"
git fetch origin && git rebase origin/main && git push -u origin feat/deps-patch
gh pr create --title "feat(factory): adopted Renovate pull requests and semver-patch, off by default (merge gate F2)" --body "Merge gate classes F2 (MC10–MC12): Source kind adopt, author.adopt with its evidence (author, branch, every commit and push the adopted login's), diffShape semver-patch and semver-minor computed from the diff (semver-minor shadow only, M2), releaseAgeCheck, the adopt poller, the adopted lifecycle (no runs, no reminders, no stale close of Renovate's PR), WouldArmAdopted and the tally's --author. Offline proofs 14 (replay summary below) and 15. Nothing is adopted until a class declares author.adopt and is live or shadow. task check: exit 0."
```

- [ ] **Step 5: Release**, once PR H is merged (the next minor after F's). PR J pins it.

---

## F2, PR I — cloud-native-ref: the `renovate` rule with the gate list

### Task 18: R47

**Files:** Modify: `.policy.yml`, `scripts/ci/check-policy-gate-coverage.sh`, `scripts/ci/tests/test-policy-gate-coverage.sh`

- [ ] **Step 1: Write the failing check** (M13). `check-policy-gate-coverage.sh` has no list of
  rule names: it holds every rule whose `has_author_in` names a `[bot]` login to the gate list,
  except `renovate[bot]`, which it skips explicitly (l.28:
  `if not any(agent_logins.search(u) and u != "renovate[bot]" for u in users)`). Remove the
  exclusion (`if not any(agent_logins.search(u) for u in users)`), update the comment above the
  loop ("every bot rule, Renovate's included: the factory's deps-patch reads it, merge gate R47"),
  and run it: `timeout 120 bash scripts/ci/check-policy-gate-coverage.sh` — Expected: FAIL,
  `FAIL: rule 'renovate' lacks gate paths: [...]`.

  L13: in `scripts/ci/tests/test-policy-gate-coverage.sh`, a fixture case beside "a rule missing
  a gate regex fails": the `tree()` policy gains a `renovate` rule
  (`if: {has_author_in: {users: ["renovate[bot]"]}}`, no `no_changed_files`), then
  `out="$(REPO_ROOT="$d" SENTINELS=".policy.yml" bash "$SUBJECT" 2>&1)" && fail "a renovate rule without the gate list fails (R47)"`
  and `grep -q "rule 'renovate' lacks gate paths" <<<"$out" || fail "the failure names the renovate rule"`;
  a second tree with the full list on it passes.
- [ ] **Step 2: The rule.** The `renovate` rule's `if:` gains the `no_changed_files.paths` block,
  copied from `low-risk: docs-links` without the last two lines (`^docs/(superpowers|specs)/`,
  `^website/content/docs/decisions/` are docs-links' exclusions, not gate paths; check the
  canonical list in `agent change approved by a maintainer` and copy exactly that). Its comment:
  "informative for Renovate's own merges (it bypasses the ruleset); the factory's deps-patch reads
  it: a gate path then skips the rule, so policy-bot posts error (merge gate R47)".
- [ ] **Step 3: Run.** `timeout 120 bash scripts/ci/check-policy-gate-coverage.sh && timeout 120 bash scripts/ci/tests/test-policy-gate-coverage.sh && timeout 600 scripts/ci/tests/run.sh` — Expected: all pass.
- [ ] **Step 4: Commit, push, PR I.**

```bash
git add .policy.yml scripts/ci/check-policy-gate-coverage.sh scripts/ci/tests/test-policy-gate-coverage.sh
git commit -m "feat(policy): the renovate rule skips gate paths, for the factory's deps-patch lane"
git fetch origin && git rebase origin/main && git push -u origin feat/renovate-rule-gate-list
gh pr create --title "feat(policy): the renovate rule skips gate paths" --body "ai-native-ref merge gate classes design, PR 4's Renovate part: a Renovate PR that touches a gate path now reads policy-bot error, so the agent factory's deps-patch class (shadow first) never counts it. Renovate's own merges bypass the ruleset and are unaffected. check-policy-gate-coverage.sh now holds the renovate rule to the gate list."
```

  PR I **merges on green** (ledger: GitHub-side only, no cluster). From the merge, policy-bot reads
  the new rule from `main`: a Renovate PR on a gate path reads policy `error`, which is informative
  only, since Renovate bypasses the ruleset (OD-7).

---

## F2, PR J — ai-native-ref: `deps-patch` declared off, the parity suite reads `renovate.json`

### Task 19: Pin, declare, parity (offline proofs 11 and 16)

**Files:**
- Modify: `flux/sources/ocirepo-agent-factory.yaml`, `tooling/base/agent-factory/helm-values-configmap.yaml`, `HOST_REF` (PR I's merge or later), `scripts/ci/tests/test-agent-merge-gate-parity.py`

- [ ] **Step 1: Write the failing suite part** (offline proof 16), appended to the parity suite
  before its summary:

```python
# Offline proof 16 (MC11): every file a never-automerge rule of the host's renovate.json reaches
# is the lane's to refuse: in the gate list or in deps-patch's protectedFiles, or outside its
# allowedFiles. A rule by file names its files; a rule by package reaches every host file that
# references the package as a dependency, never a bare mention (M14). A new never-automerge rule
# is covered or this fails.
import json, re, subprocess, tempfile

HOST_TREE = OWN / ".host"  # the host's own checkout: the composite also holds this repository's files (L5)
KEYED = r"^[^#]*(chart|image|repository|url|name):[[:space:]]*['\"]?[^'\"]*"


def ere(s):
    return re.sub(r"([.\[\]()*+?{}|^$\\])", r"\\\1", s)


def reach_pattern(name):
    """The ERE of a dependency reference to one matchPackageNames entry. A regex entry loses its
    anchors (grep's ^ is the line's start, not the package name's); a path is a whole token
    wherever it sits, open-ended when the regex was a prefix; a bare name is a word in the value
    of a dependency key."""
    regex = name.startswith("/") and name.endswith("/")
    body = name[1:-1] if regex else ere(name)
    exact = not regex or body.endswith("$")
    body = body.removeprefix("^").removesuffix("$")
    if "/" in body:
        return rf"(^|[[:space:]'\"=/@])({body})" + (r"([[:space:]'\"@:]|$)" if exact else "")
    return rf"{KEYED}\b({body})\b"


def never_automerge(renovate):
    return [r for r in renovate.get("packageRules", []) if r.get("automerge") is False or r.get("enabled") is False]


def reached(rule, tree=HOST_TREE):
    files = set(rule.get("matchFileNames", []))
    for name in rule.get("matchPackageNames", []):
        out = subprocess.run(["git", "-C", str(tree), "grep", "-lE", reach_pattern(name), "--", "*.yaml", "*.yml"],
                             capture_output=True, text=True).stdout
        files |= set(out.split())
    return files


def refused(path, cl, gates):
    allowed = cl.get("allowedFiles")
    if allowed and not any(re.match(f"^(?:{p})$", path) for p in allowed):
        return True
    return any(re.search(p, path) for p in list(gates) + cl.get("protectedFiles", []))


# Self-test of the reach (M14), on a fixture tree: a bare name reaches the file that deploys it,
# not one that mentions it; an anchored registry regex reaches its reference; a repository that
# only shares the package's prefix is not the package.
with tempfile.TemporaryDirectory() as d:
    fx = pathlib.Path(d)
    for path, text in {
        "deploy.yaml": "spec:\n  chart:\n    spec:\n      chart: zitadel\n",
        "mention.yaml": "# zitadel never started\nspec:\n  description: waits for zitadel\n",
        "pkg.yaml": "spec:\n  package: ghcr.io/smana/crossplane-configuration-aws:v0.9.20\n",
        "inference.yaml": "spec:\n  url: https://github.com/kubernetes-sigs/gateway-api-inference-extension\n",
        "gateway.yaml": "spec:\n  url: https://github.com/kubernetes-sigs/gateway-api\n",
    }.items():
        (fx / path).write_text(text)
    subprocess.run(["git", "init", "-q", d], check=True)
    subprocess.run(["git", "-C", d, "add", "."], check=True)
    for names, want in [(["zitadel"], {"deploy.yaml"}),
                        (["/^ghcr\\.io/smana/crossplane-configuration-/"], {"pkg.yaml"}),
                        (["kubernetes-sigs/gateway-api"], {"gateway.yaml"})]:
        if (got := reached({"matchPackageNames": names}, fx)) != want:
            fails.append(f"self-test: {names} reaches {sorted(got)}, want {sorted(want)}")

renovate = json.loads((HOST_TREE / ".github/renovate.json").read_text())
host_classes = next(r for r in config["repositories"] if r["name"] == "Smana/cloud-native-ref")["classes"]
deps = host_classes.get("deps-patch")
gates = rules_of(host)[CANON]["if"]["no_changed_files"]["paths"]
if deps is None:
    fails.append("deps-patch: not declared on the host")
else:
    for rule in never_automerge(renovate):
        for path in sorted(reached(rule)):
            if not refused(path, deps, gates):
                fails.append(f"deps-patch: never-automerge {rule.get('matchPackageNames') or rule.get('matchFileNames')} reaches {path}, which the lane would take")
    # self-test: drop zitadel's protection and the suite must notice
    probe = dict(deps, protectedFiles=[p for p in deps.get("protectedFiles", []) if "zitadel" not in p])
    if all(refused(p, probe, gates) for r in never_automerge(renovate) for p in reached(r)):
        fails.append("self-test: a dropped protectedFiles entry went unnoticed")
```

(`json`, `re`, `subprocess` and `tempfile` join the suite's imports; `host`, `config`, `OWN`,
`rules_of`, `CANON` and `fails` are the suite's existing names.) `git grep` runs in `.host/`, the
host's own checkout under the working directory, which `task check` provides, never in `HOST`,
the composite, which also holds this repository's files that Renovate never sees (L5). The
fixture self-test pins M14's two errors: bare `zitadel` matched 37 YAML files of the host, most
unrelated (harbor, homepage, external-dns, cloudnative-pg buckets, headlamp), and the anchored
`/^ghcr\.io/smana/crossplane-configuration-/` reached none of its 3 files. Measured with this
`reach_pattern` on the host's `origin/main` `8fe4df99`: `zitadel` 14 files (ZITADEL's own under
`security/`, `clusters/*/security/zitadel.yaml`, `flux/sources/helmrepo-zitadel.yaml`, and
`website/data/stack.yaml`, outside `allowedFiles`), `kubernetes-sigs/gateway-api` 1
(`gitrepo-gateway-api.yaml`), the crossplane regex 3 of 3. Run:
`bash $SCRATCH/check.sh "$PWD"` — Expected: the parity suite FAILs, `deps-patch: not declared on the host`.

- [ ] **Step 2: Pin H's release** (digests as in Task 10) and `HOST_REF` (PR I's merge).
- [ ] **Step 3: Declare**, in the host's `classes` after `dead-links`:

```yaml
            # Declared off (merge gate classes MC14): no switch, so no adoption. A session's first
            # commit sets shadow: true (runbook 16); Renovate keeps merging meanwhile (MC12). w0:
            # patch bumps of what Flux deploys (R42), Renovate's never-automerge files protected
            # (test-agent-merge-gate-parity.py derives them from renovate.json).
            deps-patch:
              author: {adopt: {logins: ["renovate[bot]"], branches: ['^renovate/']}}
              diffShape: semver-patch
              allowedFiles: ['^(apps|flux|infrastructure|observability|security|tooling)/.+\.ya?ml$']
              protectedFiles:
                - '^security/base/zitadel/'
                - '^flux/sources/gitrepo-gateway-api\.yaml$'
                - '^observability/base/victoria-metrics-k8s-stack/vm-common-helm-values-configmap\.yaml$'
              modifyOnly: true
              maxPatchFiles: 3
              maxModifiedLines: 10
              releaseAgeCheck: renovate/stability-days
              maxPerDay: 2
              monitor: {flux: {source: flux-system/flux-system, within: 1h}, followUpWindow: 168h}
            # M2 (a): Renovate's minors observed in shadow at no cost; semver-minor never goes live.
            deps-minor:
              author: {adopt: {logins: ["renovate[bot]"], branches: ['^renovate/']}}
              diffShape: semver-minor
              allowedFiles: ['^(apps|flux|infrastructure|observability|security|tooling)/.+\.ya?ml$']
              protectedFiles:
                - '^security/base/zitadel/'
                - '^flux/sources/gitrepo-gateway-api\.yaml$'
                - '^observability/base/victoria-metrics-k8s-stack/vm-common-helm-values-configmap\.yaml$'
              modifyOnly: true
              maxPatchFiles: 3
              maxModifiedLines: 10
              releaseAgeCheck: renovate/stability-days
              monitor: {flux: {source: flux-system/flux-system, within: 1h}, followUpWindow: 168h}
```

  (`gitrepo-agent-sandbox.yaml` is an AI path, absent from the host tree, L5: no entry names it.)
  Then run the suite and add to `protectedFiles` every path it names until it passes; record each
  one with the package that reaches it in the commit message. (The OpenBao, factory, MCP and
  AI-pin packages live under `opentofu/`, `.github/`, `flux/sources/ocirepo-(agent-factory|ai-native-ref)`
  and `infrastructure/base/agent-mcp/`: outside `allowedFiles` or on the gate list.)
- [ ] **Step 4: Run.** `bash $SCRATCH/check.sh "$PWD"` — Expected: `==> merge gate parity: 2 repositories, 0 problem(s)` (offline proof 11 holds `deps-patch` with no `low-risk: deps-patch` rule because it is not live), the pins suite `0 problem(s)`, `exit=0`. Then the strict parse at H's tag (M16), as Task 10 Step 5 with `vX.Y.0` = H's release: Expected `--- PASS: TestParse`; quote it in PR J's body (`author`, `releaseAgeCheck`, `semver-*` and `monitor.flux` are new keys).
- [ ] **Step 5: Commit.**

```bash
git add HOST_REF flux/sources/ocirepo-agent-factory.yaml tooling/base/agent-factory/helm-values-configmap.yaml \
  scripts/ci/tests/test-agent-merge-gate-parity.py
git commit -m "feat(agent-factory): pin adopted classes, declare deps-patch and deps-minor off"
```

### Task 20: Runbook 16, the docs, PR J

- [ ] **Step 1: Runbook 16**, `docs/runbooks/agent-factory/16-deps-patch.md` (L2: 16 is absent on
  `main`, #110 added 15, 17 and 18 and skipped it, and the spec reserves it for this drill):
  1. *Prerequisites:* H's release pinned; PR I on the host's `main`; the merger App installed with
     pull requests, contents and checks read on the host, and `issues: write` on both repositories
     (B1: `factory/merged:deps-patch` and the outcome labels are written in shadow, and a missing
     permission only logs; an owner action, checked before the gcp-0 session); the Flux ClusterRole rendered on gcp-0
     (`kubectl auth can-i list helmreleases.helm.toolkit.fluxcd.io --as system:serviceaccount:agent-system:agent-factory -A` → `yes`).
  2. *Shadow, L3 (M3: at the rebuild):* one commit, `deps-patch: {…, shadow: true}` and
     `deps-minor: {…, shadow: true}`. Proof: the next eligible Renovate PR gets an adopted Task
     (`kubectl -n agent-system get tasks -o wide | grep adopt`), a `would-arm head=<sha>` comment
     before Renovate merges it, `factory/merged:deps-patch` on the merge, a monitor `flux:` reading
     within 2 h; `shadow-merge-tally.sh --class deps-patch --author app/renovate` counts it. Record
     how many eligible PRs Renovate merged before the factory decided (not counted).
  3. *The drill, L4 (owner's go, after 20 shadow decisions merged unchanged and 0 bad mature):*
     PR K merged first; `deps-patch: {…, live: true}`; the next patch PR is merged by the merger at
     its head; monitor `main: healthy`, `flux: healthy`; then `factory/revert` on its PR: the revert
     PR opens, merges (once PR K's revert rule is live) and the next decision reads
     `class_paused`. Back to `shadow: true` unless the owner keeps it live.
  4. *What fails closed:* a lockfile, a digest-only update, a minor in `deps-patch`, a gate path,
     a protected file, a fourth file, an 11th line, a human's commit or push on the branch, a
     maintainer's approval, a stability status by anyone but Renovate: no merge.
  Add its row to the runbooks README.
- [ ] **Step 2: Docs.** `factory.md`: "Dependency bumps" (what the lane takes and how it proves
  it: the F2 table, shortened); `status.md`: "`deps-patch` ships switched off (vX.Y.0)". The spec's
  status line names this plan (if Task 11 did not already).
- [ ] **Step 3: The gate.** `bash $SCRATCH/check.sh "$PWD"` — Expected: `exit=0`.
- [ ] **Step 4: Commit, push, PR J.**

```bash
git add docs website/content/docs/platform/ai-platform
git commit -m "docs(agent-factory): runbook 16, the deps-patch drill"
git fetch origin && git rebase origin/main && git push -u origin feat/deps-patch-declared-off
gh pr create --title "feat(agent-factory): deps-patch and deps-minor declared off (merge gate F2)" --body "Pins agent-platform vX.Y.0 (adopted classes, semver-patch). deps-patch declared off on the host at w0: patch bumps of Flux-deployed YAML, 3 files, 10 lines, Renovate's stability days, 2 a day, monitored on Flux; deps-minor shadow-only beside it (M2). The parity suite derives the never-automerge files from the host's renovate.json (offline proof 16), reading only dependency references in the host's own tree. Runbook 16. task check: exit 0. Strict parse at vX.Y.0: <the TestParse output>."
```

---

## F2, PR K — cloud-native-ref: the live switch (held)

### Task 21: Patch auto-merge to the factory, the lane's rules

**Run only after:** L3's evidence (≥ 20 shadow decisions merged unchanged, 0 bad once mature,
printed by `shadow-merge-tally.sh --class deps-patch --author app/renovate --outcomes`), the owner's
answer to OD-F2-1 (approved: the owner's approval), and the owner's go for L4. PR K **stays held**
(ledger, 2026-10-23): no branch is pushed before that go.

**Files:** Modify: `.github/renovate.json`, `.policy.yml`

- [ ] **Step 1: `renovate.json`**, a rule after the automerge rule (later rules win):

```json
    {
      "description": "The agent factory's deps-patch lane (ai-native-ref merge gate classes, MC11): patch updates of what Flux deploys are merged by the factory's merger at the head it decided, after the post-merge monitor's evidence, not by Renovate. Three days of release age, read at the gate from renovate/stability-days. Minors keep Renovate's auto-merge (M2). Renovate's never-automerge rules below still win for their packages.",
      "matchUpdateTypes": ["patch"],
      "matchFileNames": ["apps/**/*.{yaml,yml}", "flux/**/*.{yaml,yml}", "infrastructure/**/*.{yaml,yml}", "observability/**/*.{yaml,yml}", "security/**/*.{yaml,yml}", "tooling/**/*.{yaml,yml}"],
      "automerge": false,
      "minimumReleaseAge": "3 days"
    },
```

  Run `timeout 300 npx --yes --package renovate -- renovate-config-validator .github/renovate.json` — Expected: `Config validated successfully`.
- [ ] **Step 2: `.policy.yml`**: (a) `low-risk: deps-patch`, before `agent change approved by a
  maintainer`: `has_author_in: renovate[bot]`, `only_has_contributors_in: renovate[bot]`,
  `from_branch: '^renovate/'`, `only_changed_files` = the class's `allowedFiles`,
  `no_changed_files` = the gate list plus the class's `protectedFiles` (exactly: the parity suite
  checks it), `file_not_added`/`file_not_deleted` `['.*']`, `modified_lines: {total: "< 11"}`,
  `has_workflow_result` on `ci.yaml`; added to `policy.approval`'s `or`. (b) R48, as the owner
  decides: recommended, the `renovate` rule gains `requires: {count: 1, users: [Smana]}`. (c) A
  `low-risk: factory revert (deps-patch)` rule: the merger's `Revert "` PRs with the lane's
  `only_changed_files`, the same `no_changed_files`, `< 11` lines. Run
  `timeout 120 bash scripts/ci/check-policy-gate-coverage.sh` and `timeout 600 scripts/ci/tests/run.sh` — Expected: pass.
- [ ] **Step 3: Commit, push, PR K** with the tally's output in the description, then in
  ai-native-ref the switch commit (`deps-patch: {…, live: true}`, `autoMergesPerDay` as the owner
  sets it) and the parity suite's run, which now holds `deps-patch` to `low-risk: deps-patch`.
  The same switch commit bumps `HOST_REF` to PR K's merge commit (L12): at an older `HOST_REF`
  the composite's `.policy.yml` has no `low-risk: deps-patch`, and the parity suite fails the
  live class with no rule. Land it right after PR K merges, or the lane's patches pile up
  unmerged (Renovate no longer merges them).

```bash
git add .github/renovate.json .policy.yml
git commit -m "feat(renovate): patch bumps of Flux-deployed files go to the factory's deps-patch lane"
```

---

## F3, PR L — ai-native-ref: its own `dead-links` lane

Prerequisite: the combined pin PR (first-slice PR D: `dead-links` declared off on both
repositories, the merger's artifact egress, the parity suite) on ai-native-ref's `main`, and
`HOST_REF` at or after cloud-native-ref #2542 (the scripts exist there). Both met: #110, and
`HOST_REF` `8fe4df99` (#2554). PR L **merges on green** (ledger: GitHub-side only, OD-F3-1 approved).

### Task 22: `dead-links.yaml` from the host's scripts

**Files:**
- Create: `.github/workflows/dead-links.yaml`
- Modify: `.github/agent-checks.yaml` (the two jobs' `ciOnly` rows, M15; a gate path, so a human PR, which PR L is), `scripts/ci/tests/test-agent-repo-files.py` (if it lists the workflows). `.gitignore` needs nothing: `.host/` is ignored (l.2)

- [ ] **Step 1: Prove the host's scripts run from `.host/`** on a scratch copy, before writing the
  workflow (R49):

```bash
cd "$SCRATCH" && rm -rf f3 && git clone -q --no-local /home/smana/Sources/ai-native-ref f3 && cd f3
git clone -q https://github.com/Smana/cloud-native-ref .host && git -C .host checkout -q "$(cat HOST_REF)"
export LYCHEE_VERSION="$(yq '.env.LYCHEE_VERSION' .host/.github/workflows/dead-links.yaml)"
export LYCHEE_SHA256="$(yq '.env.LYCHEE_SHA256' .host/.github/workflows/dead-links.yaml)"
# install-lychee.sh runs under set -u and writes to $RUNNER_TEMP and $GITHUB_PATH, a runner's (L4)
mkdir -p "$SCRATCH/rt" && export RUNNER_TEMP="$SCRATCH/rt" GITHUB_PATH="$SCRATCH/path"
ulimit -v 4000000; timeout 300 .host/.github/scripts/dead-links/install-lychee.sh
export PATH="$(cat "$GITHUB_PATH"):$PATH" && lychee --version
printf '[x](https://example.com/new-target)\n' >> docs/runbooks/agent-factory/README.md && git commit -qam probe
timeout 120 .host/.github/scripts/dead-links/changed-targets.sh HEAD~1
```

  Expected: `lychee 0.24.2`; the last command prints `https://example.com/new-target` alone. An
  unset `RUNNER_TEMP` is the probe's fault, not the host's: never read it as "host change first".
  If either script assumes it runs from the host's root (a relative path inside it), stop:
  that is a host change first (AGENTS.md: "A host change you need is a cloud-native-ref PR first"),
  a one-line `cd "$(dirname "$0")"`-free fix there, then a `HOST_REF` bump here.

- [ ] **Step 2: The workflow.** `.github/workflows/dead-links.yaml`:

```yaml
# Dead external links in this repository's Markdown (agent factory, merge gate classes MC4–MC6,
# F3). The detector and the check are the host's (.github/scripts/dead-links/ at HOST_REF, R49):
# this file only checks them out and runs them, as ci.yaml runs the host's gates. A gate path
# (^\.github/), so no pull request of the class can change its own proof.
#
# scan: nightly on main, and by hand; uploads findings.json, `[]` when nothing is found (a missing
# artifact makes the factory's probe retry every minute).
# external-links ("External links 🔗"): dead-links' holdout, not a required check.
name: Dead links

on:
  schedule:
    - cron: "41 4 * * *"  # the host's runs at 04:23: apart, so the two never share a rate limit
  workflow_dispatch:
  pull_request:
    branches: [main]
    paths: ["**.md"]

permissions:
  contents: read

concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}

jobs:
  scan:
    name: Scan external links 🕸
    if: github.event_name != 'pull_request'
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7
        with:
          persist-credentials: false
      - id: host
        run: echo "ref=$(cat HOST_REF)" >> "$GITHUB_OUTPUT"
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7
        with:
          repository: Smana/cloud-native-ref
          ref: ${{ steps.host.outputs.ref }}
          path: .host
          persist-credentials: false
      - name: Install lychee (the host's pin)
        run: |
          LYCHEE_VERSION="$(yq '.env.LYCHEE_VERSION' .host/.github/workflows/dead-links.yaml)" \
          LYCHEE_SHA256="$(yq '.env.LYCHEE_SHA256' .host/.github/workflows/dead-links.yaml)" \
            .host/.github/scripts/dead-links/install-lychee.sh
      - name: Scan
        id: scan
        env:
          GITHUB_TOKEN: ${{ github.token }}
        run: |
          echo '[]' >findings.json
          set +e
          lychee --no-progress --format json --output lychee.json --max-retries 3 --timeout 20 \
            --scheme https --scheme http \
            --exclude-path docs/superpowers --exclude-path docs/specs --exclude-path website/content/docs/decisions \
            'docs/**/*.md' 'website/content/**/*.md'
          rc=$?
          [ "$rc" -eq 0 ] || [ "$rc" -eq 2 ] || { echo "lychee exited $rc" >&2; exit 1; }
      - name: Findings
        if: steps.scan.outcome == 'success'
        run: |
          python3 .host/.github/scripts/dead-links/findings.py lychee.json >findings.new
          mv findings.new findings.json
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        if: always()
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
          persist-credentials: false
      # HOST_REF is read from the base, never the head: the pull request cannot pick the checker.
      - id: host
        env:
          BASE: ${{ github.event.pull_request.base.sha }}
        run: echo "ref=$(git show "$BASE:HOST_REF")" >> "$GITHUB_OUTPUT"
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7
        with:
          repository: Smana/cloud-native-ref
          ref: ${{ steps.host.outputs.ref }}
          path: .host
          persist-credentials: false
      - name: Install lychee (the host's pin)
        run: |
          LYCHEE_VERSION="$(yq '.env.LYCHEE_VERSION' .host/.github/workflows/dead-links.yaml)" \
          LYCHEE_SHA256="$(yq '.env.LYCHEE_SHA256' .host/.github/workflows/dead-links.yaml)" \
            .host/.github/scripts/dead-links/install-lychee.sh
      - name: The pull request's external targets
        env:
          BASE: ${{ github.event.pull_request.base.sha }}
        run: .host/.github/scripts/dead-links/changed-targets.sh "$BASE" >targets.txt
      - name: Each one answers 2xx
        env:
          GITHUB_TOKEN: ${{ github.token }}
        run: |
          if [ ! -s targets.txt ]; then echo "no external link target added or changed"; exit 0; fi
          cat targets.txt
          lychee --no-progress --max-retries 3 --timeout 20 --accept '200..=299' targets.txt
```

  `HOST_REF` read from the base on the pull request job is this plan's addition over the host's
  workflow: here `HOST_REF` is a file the pull request could change (it is a gate path, so an
  agent PR cannot, but a holdout must not depend on that). The scan job runs on `main`, where
  `HOST_REF` is the merged one. If the host's workflow uses `yq` and the runner image lacks it, use
  `mikefarah/yq` as `ci.yaml` installs its tools (mise), not a new action.

- [ ] **Step 2b: Declare the two jobs** (M15). `test-agent-checks.py` requires every job of a
  `pull_request` workflow to be named by a step's `ci` or by `ciOnly`; neither job runs in the
  agent's sandbox. Append to `ciOnly` in `.github/agent-checks.yaml` the host's two rows
  verbatim (host `.github/agent-checks.yaml:33-34`):

```yaml
  - {check: "Scan external links 🕸", part: all, why: "runs on schedule and workflow_dispatch only; a nightly lychee scan of every external link"}
  - {check: "External links 🔗", part: all, why: "fetches external sites, beyond the egress"}
```

- [ ] **Step 3: Check.** `actionlint .github/workflows/dead-links.yaml && shellcheck` on the
  extracted `run:` blocks (`actionlint` runs shellcheck itself when it is on the PATH), then
  `bash $SCRATCH/check.sh "$PWD"` — Expected: actionlint silent, `exit=0` (`test-agent-checks.py`
  passes with both jobs declared; the repo-files suite passes; add the workflow to its list if it
  enumerates them).

- [ ] **Step 4: Commit.**

```bash
git add .github/workflows/dead-links.yaml .github/agent-checks.yaml
git commit -m "feat(ci): dead-links' detector and holdout here, from the host's scripts at HOST_REF"
```

### Task 23: The policy rule, the schedule, PR L

**Files:**
- Modify: `.policy.yml`, `tooling/base/agent-factory/helm-values-configmap.yaml` (the schedule, `dead-links`' monitor, L16), `scripts/ci/tests/test-agent-merge-gate-parity.py` (only if its self-test needs the second repository), `docs/runbooks/agent-factory/15-dead-links.md`
- Scratch, not committed: the strict parse's worktree and `zz_parse_test.go` (M16, as Task 10)

- [ ] **Step 1: The rule (R50, OD-F3-1).** In `.policy.yml`, after `human-authored`, and named in
  `policy.approval`'s `or`:

```yaml
  - name: "low-risk: dead-links"  # shadow until its own 20 decisions (v2 D9): approves, merges nothing (merge gate R50)
    if:
      has_author_in:
        users: ["ogenki-agents[bot]"]
      only_has_contributors_in:
        users: ["ogenki-agents[bot]"]
      from_branch:
        pattern: '^agent/[a-z2-7]{8}$'
      only_changed_files:
        paths: ['^(docs|website/content)/.+\.md$']
      no_changed_files:
        paths:
          # the gate list of `agent change approved by a maintainer`, copied whole, then:
          - '^docs/(superpowers|specs)/'
          - '^website/content/docs/decisions/'
      file_not_added:
        paths: ['.*']
      file_not_deleted:
        paths: ['.*']
      modified_lines:
        total: "< 21"
      has_workflow_result:
        conclusions: ["success"]
        workflows: [".github/workflows/ci.yaml"]
```

  The header comment's "No low-risk rule" paragraph becomes "One low-risk rule, `dead-links`'
  (merge gate R50): it approves the class's fence; the merger merges only a live class, and none is
  live here until its own 20 shadow decisions (v2 D9)." Run `bash $SCRATCH/check.sh "$PWD"` —
  Expected: the parity suite passes (ai-native-ref's `dead-links` fence equals its rule: the
  suite's `check()` already reads this repository's `.policy.yml`), `test-agent-repo-files.py`
  passes (the gate list unchanged).

- [ ] **Step 2: The schedule.** In ai-native-ref's repository entry, `schedules:` (new):

```yaml
          schedules:
            # dead-links' intake here (MC6, F3): what two nightly runs of this repository's
            # dead-links.yaml agree on. Inert while the class is off (MC14).
            - name: dead-links
              cron: "15 7 * * *"  # after the 04:41 scan; the host's slot is 07:00
              class: dead-links
              probe: dead-links
              text: Fix the dead external links listed below.
```

  and the comment above ai-native-ref's `dead-links` class (l.123) loses "No schedule here yet".
  The same class gains its monitor now that its detector exists (L16, moved here from Task 10):
  `monitor: {oracleRuns: 2, followUpWindow: 168h}` (no `flux`: its changes reach a cluster only
  through a host pin bump). The key needs F's release pinned: if PR G is not on `main` yet, rebase
  PR L onto it once it is (at v0.20.0 the strict decoder refuses `monitor`, and the rollout would
  fail). Run the check again — Expected: `exit=0`. No suite parses the config
  with the factory's decoder (M16: `test-agent-factory-pins.py` reads tags, digests and
  `renovate.json` only), so then run the strict parse as Task 10 Step 5, at the agent-platform
  tag ai-native-ref pins at this PR's base (`schedules:` and the monitor are the new keys here):
  Expected `--- PASS: TestParse`; quote it in PR L's body.

- [ ] **Step 3: Runbook 15.** A section *ai-native-ref*: L1 on this repository (the two scheduled
  runs `gh run list -R Smana/ai-native-ref --workflow dead-links.yaml --branch main`, the merger
  App's installation here granted `actions: read` and `issues: write` (B1: the monitor's labels)
  — accept the permission update on the installation if GitHub asks — then `shadow: true` on this
  repository's entry); its Flux signal is
  absent by design (its changes reach a cluster only through a host pin bump).

- [ ] **Step 4: Commit, push, PR L.**

```bash
git add .policy.yml tooling/base/agent-factory/helm-values-configmap.yaml docs/runbooks/agent-factory/15-dead-links.md
git commit -m "feat(agent-factory): ai-native-ref's own dead-links lane, its policy rule and schedule"
git fetch origin && git rebase origin/main && git push -u origin feat/dead-links-own-lane
gh pr create --title "feat(agent-factory): ai-native-ref's own dead-links lane (merge gate F3)" --body "Merge gate classes F3: dead-links.yaml runs the host's detector and External links check from .host at HOST_REF (never vendored; on a pull request HOST_REF is read from the base), a low-risk: dead-links rule so a shadow decision here can reach would-arm (first-slice R22, owner to confirm: it approves, it merges nothing), and the schedule entry, inert while the class is off, and dead-links' monitor here. Note (L17): from the merge, low-risk: dead-links approves any agents PR inside its fence on this repository, docs-links and docs shadow PRs included, so their shadow decisions reach would-arm and count in this repository's docs-links tally; read that tally with this in mind. task check: exit 0. Strict parse at vX.Y.0: <the TestParse output>."
```

  PR L **merges on green** (ledger). From the merge, `dead-links.yaml` runs nightly here and
  `low-risk: dead-links` approves agent PRs inside its fence: GitHub-side only, no cluster.

---

## Offline proofs

| # | Proof | Task | Evidence |
|---|---|---|---|
| 4 | Follow-ups on a fake forge: overlap labelled; elsewhere not; the lane's next bump not; a maintainer's label counts | 5 | `TestFollowUps` |
| 5 | One follow-up or regression in 10 demotes; still after every Task deleted | 3 | `TestFollowUpsAndRegressionsDemote`, `TestADemotionOnFollowUpSurvivesEveryTaskDeleted` |
| 6 | Flux on a fake: HelmRelease not Ready → regression; suspended → inconclusive; all Ready → healthy | 7, 8 | `TestNotReadySince`, `TestSource`, `TestFluxSignal` |
| 7 | The bound: 0 in 28 fails, 0 in 29 passes at 10%; immature and inconclusive left out | 6 | `scripts/shadow-merge-tally_test.sh` (`task scripts`) |
| 11 | Parity: each class's fence equals its rule; a mismatch fails | 19, 23 | the parity suite on `deps-patch`, `deps-minor` and ai-native-ref's `low-risk: dead-links` |
| 12 | `task check` on the composite with every class declared off | 11, 20, 23 | `check.sh` logs ending `exit=0` |
| 13 | promtool: `AgentFactoryMergeRegression` fires on one regression | 10 | `test-agent-merge-regression-alert.sh` |
| 14 | `semver-patch` replays the host's Renovate merges since 2026-08-18: no minor, major or digest accepted | 13, 14 | `TestShapes`, `TestReplay` (committed cases), the by-hand replay's summary in PR H |
| 15 | Adopted PR: a second committer, a branch outside `renovate/*`, a pending stability status refuse or wait | 15 | `TestAdoptedGate`, `TestAnAdoptedTaskReadsNoRoom`, `TestAnAdoptedCancelledCheckHandsOff`; `TestEnvtestAnAdoptedTask` (12) |
| 16 | Parity fails when a never-automerge rule reaches a file the lane would take | 19 | the parity suite, its `protectedFiles` self-test and its reach fixture (M14) |
| #182 | A wait past `awaitingCIFor` hands off naming the missing checks | 1 | `TestAwaitingCIIsBounded` |

## Next gcp-0 session: live proofs

Batch these in one session (MEMORY: offline before platform). Prerequisites: PRs E–J merged and
released, the combined pin PR and PR G and J on ai-native-ref's `main`, PR I on the host's `main`,
gcp-0 rebuilt from them, and the **owner's App check (B1)**: the merger App's installation grants
`issues: write` on both repositories (and `actions: read` on ai-native-ref, runbook 15), accepted
in GitHub's installation settings before the rebuild. Without it every outcome label fails
silently (`r.label` only logs), L3 gathers no countable evidence and PR K could go on none. Each
drill needs the owner's go, as runbook 12 did.

| # | Drill | Commit | Expected | Fails closed if |
|---|---|---|---|---|
| S0 | Readiness | — | `kubectl auth can-i list helmreleases.helm.toolkit.fluxcd.io -A --as system:serviceaccount:agent-system:agent-factory` → `yes`; `… create helmreleases …` → `no`; the factory's log shows the monitor reading no Task (no class has merges yet); `agent_factory_monitor_verdicts_total` exported; the merger mints with `issues: write` (B1: no mint error in the log, the installation granted it) | the ClusterRole renders with a write verb (stop, fix the chart); a mint refused for `issues` (stop: the owner grants it first) |
| L3 | `deps-patch` and `deps-minor` shadow (M3: first, at the rebuild) | `shadow: true` on both | every eligible Renovate PR gets an adopted Task and `would-arm head=<sha>`; ineligible ones (mise.toml, a gate path, a digest) get no Task; Renovate's merge gets `factory/merged:deps-patch` and a Flux reading within 2 h; the tally counts it with `--author app/renovate`. Measure how many eligible PRs Renovate merged before the factory decided | a Renovate PR on a gate path gets a Task decided `shadow` (R47 missing); a merge with no `factory/merged:deps-patch` and a `label the merge ledger` error in the log (B1: stop, the owner grants `issues: write`) |
| L1 | `dead-links` shadow, both repositories (waits for F1, R1) | `shadow: true` on both entries; the host's `link-rot` schedule removed (M4) | a finding becomes a Task and a PR, `would-arm`; after a maintainer merges at that head, `factory/merged:dead-links` and, after two detector runs, `status.monitor.oracle: healthy` | the monitor posts a verdict before the follow-up window ends (R31) |
| M1 | The monitor's regression path, on a scratch merge | a maintainer merges a planted docs PR of a monitored class whose required check is red on `main` afterwards (or `factory/monitor:regression` set by hand on a merged PR, then the factory restarted) | the Task's `monitor.main: regression`, the label, the control-issue narration, `AgentFactoryMergeRegression` fires, the next decision of that class reads `class_paused`, and nothing is reverted | a revert PR opens (MC7 broken) |
| M2 | Follow-up detection | a maintainer merges a PR that rewrites a line the L1 merge wrote | the later PR gets `factory/follow-up-of:#<n>`, `outcome="follow_up"` counted, the class `class_paused` | an unrelated later merge is labelled |
| #182 | AwaitingCI's bound | a holdout name misspelt in a scratch class in shadow, `awaitingCIFor: 1h` for the session | after 1 h, `ci_wait_expired` naming the check | the task merges or waits silently |
| L2 | `dead-links` drill (runbook 15) | as the first slice planned, with PR L's rule on ai-native-ref only if OD-F3-1 is yes | the merger's merge at the head, the monitor `oracle: healthy`, `factory/revert`, `class_paused` | — |
| L4 | `deps-patch` drill (runbook 16), only after 20 shadow decisions merged unchanged and 0 bad mature | PR K merged; `live: true` | one patch PR merged by the merger at its head, `flux: healthy`, `factory/revert` reverts it, the class demoted | the merger merges a PR Renovate's never-automerge list names |
| L5 | Rebuild survival | delete every Task, restart the factory | `class_paused` still read for the class demoted in M1, M2, L2 or L4 (the labels, R30) | the class arms again |

L3 and L1 cost no human time beyond the commits; M1, M2 and #182 can run on planted PRs in one
afternoon; L2 needs a planted dead link and two detector runs (dispatch them); L4 waits weeks for
its evidence (about three, the design's estimate).

## Decisions for the owner

| # | Question | Recommended |
|---|---|---|
| OD-F2-1 | At the live switch, the host's `renovate` rule approves every Renovate PR off the gate list, so policy-bot is no independent check of `deps-patch`'s fence (MC2). Make the `renovate` rule require your approval (Renovate's own merges bypass and are unaffected; your hand merges of Renovate PRs then need your review, or the bypass), or accept the factory's fence alone for the lane | **Require the approval (R48)**, in PR K |
| OD-F2-2 | `deps-patch`'s w0 adds an allow list (Flux-deployed YAML) and `modifyOnly`, beyond the design's illustrative config, leaving `mise.toml` (42 of 251 touches), `container-images/` and `crds/` to Renovate | **Yes (R42)**: the monitor can only watch what Flux deploys; `mise.toml` is the design's later width |
| OD-F3-1 | ai-native-ref's first `low-risk` rule, `dead-links`', so its shadow can reach `would-arm` (first-slice R22) | **Yes (R50)**: it approves a fence, it merges nothing while the class is shadow |
| OD-F1-1 | `merge.awaitingCIFor` value | **24h** on both repositories (R27) |
| B1 (owner action) | The monitor's outcome labels are written in shadow, so the merger App's installation must grant `issues: write` on both repositories (and `actions: read` on ai-native-ref) before the gcp-0 session. Not a design choice: the code asks for it once a class declares a monitor (Task 2) | **Grant it** in GitHub's installation settings before the rebuild; checked by runbooks 15 and 16 and S0 |

OD-F2-1, OD-F2-2, OD-F3-1 and OD-F1-1 were approved as recommended on 2026-10-23 (the ledger);
PRs I and L then merge on green, and PR K stays held until L3's evidence and the owner's go.
B1's App permission is the one owner action left, for the gcp-0 session.

## Self-review

- **Follow-ups.** F1: Tasks 1–11 (PRs E, F, G). F2: Tasks 12–21 (PRs H, I, J, K; K held). F3:
  Tasks 22–23 (PR L). #182: Task 1, decided as not the monitor's (R27) but fixed in F1's first PR.
- **Design decisions.** MC1: the vocabulary gains `author`, `releaseAgeCheck`, `monitor`, the semver
  shapes (Tasks 2, 12, 15). MC2: parity for `deps-patch`/`deps-minor` (19) and ai-native-ref's rule
  (23); its gap at live is OD-F2-1 (R48). MC3: unchanged, the merger stays the only merge actor
  (15, 21). MC4–MC6: F3's lane (22–23) and the oracle signal (8). MC7: the monitor (5, 7, 8),
  never reverting (`TestMainSignal`). MC8: `maxFollowUps` and follow-ups (2, 3, 5). MC9: demotion
  automatic (3), widening by the tally's bound and the owner's PR (6, 21). MC10: the adopt poller,
  no authored bump, `renovate-red` unchanged (16, R45). MC11: `semver-patch` from the diff (13),
  release age (15), never-automerge parity (19), 3 days at live (21). MC12: shadow observes
  Renovate (16, runbook 16). MC13: labels and comments, the tally (3, 5, 6, 17). MC14: every class
  and key off by default (1, 2, 12, 16, 19). M1: unchanged (dead-links first). M2: `deps-minor`
  shadow-only (12, 19, R43). M3: L3 at the rebuild. M4: L1 removes `link-rot`. M5: `maxFollowUps:
  1` (10). M6: budget 10% (6).
- **Offline proofs** 4–7, 11–16 and #182: the table above. Live L1–L5 plus S0, M1, M2 and #182's
  drill: the session table.
- **Not planned, left to the builder or open:** the dashboard panels for the monitor's verdicts and
  the adopt reasons (the design leaves them to the builder; one panel per kind fits PR G and J);
  whether the monitor comment and `would-arm` share a marker format (they do not: `monitor-<n>`
  and `would-arm-<n>`); the exact `protectedFiles` additions the parity suite will demand
  (computed, Task 19 Step 3); the blame calls' measured cost (cached per (SHA, path), bounded by the
  files *m* touched; measure in L3); whether Renovate's `renovate/stability-days` status is present
  on PRs it opens after the age has passed (`internalChecksFilter`): if absent, every adopted Task
  waits and R27 hands it off, so L3 measures it first and, if absent, `releaseAgeCheck` stays off
  in shadow and the age rests on Renovate's `minimumReleaseAge` alone (an owner note in PR J);
  monitor verdicts on webhooks and the Slack App (L19): reasons added to `summary` reach webhooks
  through `summary.Reasons()` (`events.go:39-41`), but a verdict emits no room event, so neither
  sees a regression; the PR's label, its comment, the control issue and the alert carry it.
- **Placeholders.** `X.Y.0` stands for the two releases cut in Tasks 9 and 17 (read at the time,
  as the first slice read v0.17.0's digests). `adoptRig` and the lifecycle tests' bodies are named
  with their setup and assertions in prose, on the existing rig's helpers.
- **Names.** `MergedLabel`, `FollowUpOfLabel`, `RegressionLabel`, `Merge.FollowUp/Regression`,
  `MonitorStatus`, `monitorDue`, `Worst`, `config.Monitor`, `config.FluxWatch`, `config.Author`,
  `config.Adopt`, `Class.Adopted`, `semverdiff.Patch/Minor`, `intake.Adopter`, `Merger.MergedSince`,
  `PullFiles`, `BlameAtParent`, `IsAncestor`, `forge.BaseLines`, `fluxwatch.Reader`, and the
  amendments' `config.AnyMonitored`, `Merge.Auto`, `intake.DetectorRun`, `intake.Gone`,
  `intake.ErrFindingsRefused`, `ScheduleMerger.Diff` are used under the same names in every task.
