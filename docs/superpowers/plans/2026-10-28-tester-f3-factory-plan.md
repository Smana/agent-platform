# The test-first tester (v3 feature 4), F3: the factory — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A class switched `testFirst` starts every public task with a tester run on `agent/<task>`;
the factory reads what the tester pushed from GitHub, records `status.contract`, briefs every
implementer run with it, and the merge gate requires "Test contract 🧪" on that task (row 2b) and
hands any change to a contract file to a human (row 2c). The verifier tester retires: a template
listing `tester` loads without it for one release, with a warning, and `room_verdict` is the
reviewer's alone. A live class without a holdout warns and sets a gauge.

**Architecture:** The tester stage mirrors the scoper's: `testerDue(t)` in `queued` before the
implementer, its end read in `implementing`. The contract's source is GitHub's push record and the
commit's trailers (R14), never the room. Rows 2b and 2c join `baseChecks` (merge gate PR A) through a
`requiredFor(rc, t)` that every reader of `merge.requiredChecks` calls, and a new
`Merger.ChangedFiles` read. Off by default: no class declares `testFirst`, so every existing test
passes unchanged.

**Tech Stack:** Go 1.26 (go.mod), controller-gen (`task crd:gen`), envtest, the forge fake
(`internal/factory/forge/forgetest`), OpenTelemetry metrics (`fmetrics`).

**Parent plan:** `docs/superpowers/plans/2026-10-22-test-first-tester-plan.md` (rulings R14, R17–R21,
R23 and the inherit row F3). **F1:** `docs/superpowers/plans/2026-10-28-tester-f1-harness-plan.md`
(the ends, the note, the trailers; F1-R2 moves `room_verdict` here). **Spec:** ai-native-ref
`docs/superpowers/specs/2026-10-22-test-first-tester-design.md` (TF1–TF4, TF6, TF8, TF10, TO1, TO5;
offline proof 4).

## What this builds on, re-measured on `main` (87c1582, 2026-10-28)

| Piece | Status | Where |
|---|---|---|
| `Class.Checks`, `Repository.ChecksFor`, `withChecks` (implementer and reviewer, public only) | merged (#186) | `config/classes.go`, `config/config.go`, `reconciler/implement.go` |
| `DecideArm` = `baseChecks` + author mode + `declared(cl, in)`; `Conditions()`; `holdoutChecks` (pending → `holdout_pending`, red, another App, a truncated rollup → `holdout_red`); `unreported` | merged (#183) | `reconciler/arm.go`, `reconciler/gate.go` |
| `forge.FileStat`, `Merger.Diff` (contents of every changed file, one page of 100), `TaskForge.BranchActivity`, `pushedBy` | merged | `forge/forge.go`, `forge/github.go`, `reconciler/merge.go` |
| The scoper stage: `scoperDue`, `scoperRun`, `startScoper`, `scoped`, `escalatesScoper` | merged (#190) | `reconciler/scoper.go` |
| `Config.Ignored` and `warnIgnored` (the one-release key warning) | merged | `config/config.go`, `internal/app/factory.go` |
| F1's release (the tester's commit-only mode, the trailers, the end note) | **not merged** | F1 |

**Missing, and planned here** (none blocks writing F3; each is a task below):

1. **No read of one commit by sha.** `TaskForge` reads `PullRequestCommits` (needs a pull request,
   which the tester never opens). Task 4 adds `Commit(ctx, sha)`.
2. **`forge.Commit.Trailer` returns the last value only.** The contract has one
   `Agent-Contract-File` and one `Agent-Contract-Id` per entry. Task 4 adds `Trailers(key) []string`.
3. **No changed-files read without contents.** `Diff` reads every file's content at two commits and
   refuses 100 files. Task 4 adds `Merger.ChangedFiles(ctx, base, head)` (R21).
4. **`review` may not be declared** ("review is the implicit class of everything else; do not
   declare it"). R17 amends it: Task 1.
5. **No `Config.Warnings`**: `Ignored` is key-shaped (`Key`, `Use`). Task 1 adds a list of sentences.
6. **The spec's `holdout_absent` and `{status, creator}` scenario form are not built** and stay
   unbuilt (R20): an absent holdout waits as `holdout_pending`; `awaitingCIFor` bounds the wait.
7. **TF3's second filter** (the scoper's bounded task skipping the tester when its check is not a
   test) has no field to read: `envelope.Acceptance` is free text. Not in F3.

## This plan's rulings

| # | Ruling | Why |
|---|---|---|
| F3-R1 | **`testerDue(t)`:** the class has `testFirst`, the task is public, its template has an implementer, `status.contract` is unset, and no tester record exists — or the current record is a tester's and the next trigger is `resume`. After the scoper (`scoperDue` first) | The scoper's scope belongs in the tester's brief. Records, not a new status field, say the stage ran: a `/factory retry` after the tester's end does not run it twice (TO3 holds the task without one) |
| F3-R2 | **`testerSpec` = `implementerSpec(t, TesterBrief)` with `Role: "tester"`**, the task tier's model and caps (R19), `withChecks` extended to the tester. `phaseFor("tester")` = `Implementing`; `recordExisting` keeps the tester's trigger; `resume` of a tester run requeues it with trigger `resume` (no `requestVerifier`) | TF1: the tester is a writer, and a lost one resumes as one |
| F3-R3 | **The tester's end, in `implementing`, before the `infraLost` block's verdicts:** `escalatesScoper`'s reasons escalate as any run's; an `infraLost` run resumes (resumable) as today. Otherwise the forge read (F3-R4) decides; the end note only names the category | R14 |
| F3-R4 | **The forge read:** `BranchActivity("agent/<task>")`. Empty → read the run's end note: `contract_ready` or none → wait up to `contractPushWait` (5 min after the run's end, `t.Status.Reason = "contract_push_unrecorded"`), then no contract with end `unreported`; any other category → no contract with that end. Non-empty → `pushedBy(branch, [agentsLogin], tip)` must be `pushFactory` and the activity a single `branch_creation`; then `Commit(tip)`: `Agent-Contract` and `Agent-Run` equal the tester run's id, at least one file and one id (the trailers are a claim; GitHub's pusher record is what vouches for them) → `status.contract`; anything else → `Escalated`, `contract_invalid` | R14, made exact. A push the agents App made while a tester run held the room is the tester harness's (the token is minted per run). One push only: a model that pushed past the disabled `pushurl` and then the harness's push would show two |
| F3-R5 | **No contract (TO3)** requeues the implementer with trigger `initial`, the narration "The tester left no contract (<category>): the implementer works without one."; a contract narrates "The tester wrote a contract of N tests: <files>." Both through `narrateLater` (the issue) and counted by `agent_factory_contracts_total{class, end, repository}` (R23) | R23 |
| F3-R6 | **Every implementer brief of a task with a contract carries it**: the first, every revision, a CI fix, a resumed run's (the lost brief already carries it); a `contractBlock` after the scope block, naming the commit, the files, up to 50 ids, the rule, and the dispute path | TF6. The harness reads the trailers itself (F1-R9); the brief is for the model |
| F3-R7 | **`contract_disputed`:** an implementer run of a task with a contract whose room holds its own handoff `toRole: tester` ends the task `Escalated`, `contract_disputed`, whatever its phase | TF6: no dispute round |
| F3-R8 | **Row 2b:** `requiredFor(rc, t)` = `merge.requiredChecks` plus `merge.contractCheck` when `status.contract` is set; every reader of `RequiredChecks` in the reconciler calls it (arm, the adoption and evidence reads, the cancelled re-run, `fixCI`'s failing list, the revert's green merge, `unreported`) | R21. One helper, so a reader cannot be missed |
| F3-R9 | **Row 2c**, right after row 2 in `baseChecks`: `ArmInputs.Contract` (`Read bool`, `Changed []string`), read by `awaitingCI` once CI is green on a task with a contract, through `Merger.ChangedFiles(contract.commit, head)`; unread → `wait`, `contract_unread`; a contract file among the changed → `human`, `contract_modified`. `ChangedFiles` reports every file of the compare and, when GitHub says the head is not `ahead` of the contract commit (`behind`, `diverged`), returns `ErrNotDescendant`, which the read turns into "every contract file changed" | R21: fail closed on a rebase. One compare call per decision |
| F3-R10 | **The verifier tester retires (TO1, R18).** `config.Parse` drops `tester` from a template's roles and adds the warning "template <name>: tester is the test writer since v<release>; dropped from the template, refused from the next minor". `room_verdict`'s roles are `[reviewer]` (F1-R2); `nextVerifier`, `verifiers`, `agentEvidence`, `rooms.verifier`, the store's verdict query, `narrate`, `summary`, the room card treat the reviewer only. The run API and the human API refuse a `tester` run, `tester_is_factory_only`; `roomctl` and the human MCP schema drop it | R18. The factory starts the tester, never a person: a person's tester run would have no contract stage behind it |
| F3-R11 | **Config (R17):** `classes.<c>.testFirst` needs `checks: true` on the class and `merge.contractCheck` on the repository; refused on `docs-links` and `revert`; `review` may be declared with `checks` and `testFirst` only. `merge.contractCheck` may not be one of `merge.requiredChecks` (on a task without a contract the job only reports, TF7) | R17, TF7 |
| F3-R12 | **TO5:** a live class with no `holdoutChecks` adds the warning "class <c> of <repo> is live without holdoutChecks: refused from the next minor (TO5)" and the gauge `agent_factory_class_without_holdout{repository, class}` = 1, set once at start from the parsed config | R20 |
| F3-R13 | **A tester run on an internal task never starts** (`testerDue` false): the class's internal tasks run today's sequence | R17; the XRD refuses checks on an internal claim |

## Global Constraints

- **Worktree:** `git -C /home/smana/Sources/agent-platform worktree add /home/smana/Sources/agent-platform-tester-f3 -b feat/tester-f3-factory origin/main`, after F1 merges. Rebase onto a freshly fetched `origin/main` before every push.
- **Evidence:** `task check` (Docker running: the store tests use testcontainers; envtest assets
  through `task test`). Per task: `go test -race -count=1 ./internal/factory/<pkg>/ -run <Name>`.
  Never claim done without the output.
- **Off by default (D4).** No existing test changes its expectation except those that start a
  verifier `tester` (they move to the reviewer or to the new refusal) — list each in the PR.
- **Interfaces belong to the consumer** (AGENTS.md): `TaskForge` and `Merger` gain one method each,
  the fake implements both, `*forge.GitHub` asserts both (`var _ Merger = (*GitHub)(nil)` exists).
- **The forge is the source, the room the hint.** Nothing a model can write (a room note, a handoff
  summary, a commit's subject) decides a contract.
- **Commits:** conventional, English, no trailers. `git add` named paths only.

## File structure

| File | Task | Change |
|---|---|---|
| `internal/factory/config/classes.go`, `config.go`, `tester_test.go` (new) | 1 | `TestFirst`, `ContractCheck`, `Warnings`, R17, R18's drop, TO5's warning |
| `internal/app/factory.go`, `internal/factory/fmetrics/fmetrics.go`, tests | 2 | The warnings logged, the gauge, the counter |
| `api/factory/v1alpha1/task_types.go`, `config/crd/…tasks.yaml`, `charts/agent-factory/crds/`, deepcopy | 3 | `status.contract` |
| `internal/factory/forge/forge.go`, `github.go`, `github_test.go`, `forgetest/fake.go` | 4 | `Commit`, `Trailers`, `ChangedFiles`, `ErrNotDescendant` |
| `internal/factory/reconciler/tester.go` (new), `tester_test.go` (new), `implement.go`, `reconciler.go`, `text.go` | 5, 6 | The stage, its spec and brief, its end |
| `internal/factory/reconciler/text.go`, `watch.go`, `implement.go` | 7 | The contract in every implementer brief; `contract_disputed` |
| `team.go`, `arm.go`, `merge.go`, `implement.go`, `rooms/rooms.go`, `store/verdicts.go`, `narrate/narrate.go`, `summary/summary.go`, `roomview/card.go`, `mcp/tools.go`, `factory/api/server.go`, `humanapi/startrun.go`, `humanapi/mcp.go`, `humanapi/mcpact.go`, `app/roomctl.go` | 8 | The verifier tester retired |
| `arm.go`, `merge.go`, `gate.go` | 9, 10 | Rows 2b, 2c |
| `narrate/narrate.go`, `narrate/merge_how_test.go`, `summary/summary.go` | 11 | The reasons' prose |
| `reconciler/tester_envtest_test.go` (new) | 12 | Offline proof 4 end to end |
| `docs/factory.md` | 13 | The tester stage, the contract, rows 2b/2c, the warnings |

---

### Task 1: The config (R17, R18, TO5)

**Files:** Modify `internal/factory/config/classes.go`, `config.go`. Create `internal/factory/config/tester_test.go`.

**Interfaces produced:** `Class.TestFirst bool` (`json:"testFirst,omitempty"`), `Merge.ContractCheck string`
(`json:"contractCheck,omitempty"`), `Config.Warnings []string` (`json:"-"`), `(*Repository).TestFirstFor(class string) bool`.

- [ ] **Step 1: Failing tests** in `tester_test.go`, on `listShaped(t, good)` and `withChecks` from `checks_test.go`:

```go
func withTestFirst(t *testing.T) string {
	t.Helper()
	raw := strings.Replace(withChecks(t, "[pypi]"), "scripts: {}", "scripts: {checks: true, testFirst: true}", 1)
	return strings.Replace(raw, "    merge:\n", "    merge:\n      contractCheck: \"Test contract 🧪\"\n", 1)
}

func TestTestFirstIsPerClassAndNeedsChecksAndTheContractCheck(t *testing.T) {
	c, err := Parse([]byte(withTestFirst(t)))
	if err != nil {
		t.Fatal(err)
	}
	r := c.RepositoryFor("Smana/cloud-native-ref")
	if !r.TestFirstFor("scripts") || r.TestFirstFor("docs") || r.Merge.ContractCheck != "Test contract 🧪" {
		t.Fatalf("scripts %v, docs %v, contractCheck %q", r.TestFirstFor("scripts"), r.TestFirstFor("docs"), r.Merge.ContractCheck)
	}
	for _, tc := range []struct{ name, from, to, want string }{
		{"no checks", "scripts: {checks: true, testFirst: true}", "scripts: {testFirst: true}", "testFirst needs checks: true"},
		{"no contract check", "      contractCheck: \"Test contract 🧪\"\n", "", "testFirst needs merge.contractCheck"},
		{"docs-links", "docs-links: {live: true}", "docs-links: {live: true, checks: true, testFirst: true}", "docs-links: testFirst is refused"},
		{"required", "      contractCheck: \"Test contract 🧪\"\n", "      contractCheck: \"Security scanning 🔒\"\n", "must not be one of merge.requiredChecks"},
	} {
		if _, err := Parse([]byte(strings.Replace(withTestFirst(t), tc.from, tc.to, 1))); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestReviewMayBeDeclaredForChecksAndTestFirstOnly(t *testing.T) {
	ok := strings.Replace(withTestFirst(t), "scripts: {checks: true, testFirst: true}", "scripts: {}\n            review: {checks: true, testFirst: true}", 1)
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse([]byte(strings.Replace(ok, "review: {checks: true, testFirst: true}", "review: {live: true}", 1))); err == nil ||
		!strings.Contains(err.Error(), "review takes checks and testFirst only") {
		t.Fatalf("a live review parsed: %v", err)
	}
}

func TestATemplateListingTheTesterLoadsWithoutItAndWarns(t *testing.T) {
	c, err := Parse([]byte(strings.Replace(listShaped(t, good), "[implementer, reviewer]", "[implementer, tester, reviewer]", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if roles := c.Templates["trio"].Roles; slices.Contains(roles, "tester") {
		t.Fatalf("trio %v still lists the tester", roles)
	}
	if !slices.ContainsFunc(c.Warnings, func(w string) bool { return strings.Contains(w, "template trio: tester is the test writer") }) {
		t.Fatalf("warnings %v", c.Warnings)
	}
}

func TestALiveClassWithoutHoldoutsWarns(t *testing.T) {
	c, err := Parse([]byte(listShaped(t, good)))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(c.Warnings, func(w string) bool { return strings.Contains(w, "class docs-links of Smana/cloud-native-ref is live without holdoutChecks") }) {
		t.Fatalf("warnings %v", c.Warnings)
	}
}
```

  (Read `good`'s `trio` and class lines first and adjust the `Replace` anchors to them; the shapes,
  not the anchors, are the test.)
- [ ] **Step 2:** `go test -count=1 ./internal/factory/config/ -run 'TestFirst|Review|Tester|Holdouts'` → fails to compile.
- [ ] **Step 3: Implement.** `classes.go`: the field with its comment ("TestFirst starts every public
  task of the class with a tester run that writes the failing tests first (v3 feature 4, TF1); needs
  Checks and the repository's merge.contractCheck"). In `validateClass(name, cl, bad)`:

```go
	if cl.TestFirst && (name == "docs-links" || name == "revert") {
		bad("class %s: testFirst is refused: no behaviour to test (TO4)", name)
	}
	if cl.TestFirst && !cl.Checks {
		bad("class %s: testFirst needs checks: true, the red check runs the toolchain (R17)", name)
	}
	if name == "review" && (cl.Live || cl.Shadow || cl.ReadsDiff(name) || len(cl.HoldoutChecks) > 0 || cl.MaxPerDay > 0 ||
		cl.Monitor != nil || cl.Author != nil || cl.ReleaseAgeCheck != "") {
		bad("class review: review takes checks and testFirst only (R17)")
	}
```

  `config.go`: replace the refusal of a declared `review` with nothing (the class check above holds
  it); in the repository's validation, `testFirst` on any class needs `r.Merge.ContractCheck != ""`
  ("class %s: testFirst needs merge.contractCheck"), and `slices.Contains(r.Merge.RequiredChecks,
  r.Merge.ContractCheck)` is refused. After validation, in `Parse`:

```go
	for name, t := range c.Templates {
		if i := slices.Index(t.Roles, "tester"); i >= 0 {
			t.Roles = slices.Delete(slices.Clone(t.Roles), i, i+1)
			c.Templates[name] = t
			c.Warnings = append(c.Warnings, fmt.Sprintf("template %s: tester is the test writer since v3 feature 4; dropped from the template, refused from the next minor (TO1)", name))
		}
	}
	for _, r := range c.Repos() {
		for _, name := range slices.Sorted(maps.Keys(r.Classes)) {
			if cl := r.Classes[name]; cl.Live && len(cl.HoldoutChecks) == 0 {
				c.Warnings = append(c.Warnings, fmt.Sprintf("class %s of %s is live without holdoutChecks: refused from the next minor (TO5)", name, r.Name))
			}
		}
	}
```

  The drop runs **after** the R38 check, so a `[tester]` template is still refused ("only an implementer writes").
  `TestFirstFor(class)` returns `r.Classes[class].TestFirst`.
- [ ] **Step 4:** `go test -race -count=1 ./internal/factory/config/` → `ok`, the existing suite unchanged.
- [ ] **Step 5:** Commit `feat(factory): testFirst per class, merge.contractCheck, the tester out of templates, live classes without holdouts warned (tester F3)`.

### Task 2: The warnings at start, the gauge and the counter

**Files:** Modify `internal/app/factory.go`, `internal/factory/fmetrics/fmetrics.go`, `internal/factory/reconciler/reconciler.go` (the `metrics` interface), `reconciler_test.go` (`fakeMetrics`). Tests: `internal/app/factory_ignored_test.go`, `fmetrics/fmetrics_test.go`.

**Interfaces produced:** `fmetrics.(*Set).ContractEnd(ctx, class, end string)` →
`agent_factory_contracts_total{class, end, repository}` with `end` ∈ `ready`, `not_testable`,
`contract_not_red`, `contract_outside_tests`, `contract_invalid`, `unreported` (others → `other`);
`fmetrics.(*Set).ClassWithoutHoldout(ctx, repository, class string)` → the gauge
`agent_factory_class_without_holdout` = 1; `app.warnConfig(log, cfg)`.

- [ ] **Step 1: Failing tests:** `TestEachConfigWarningIsLogged` (beside `TestEachIgnoredConfigKeyIsWarnedAbout`):
  two `Warnings` → two `level=WARN` lines carrying each sentence. In `fmetrics_test.go`, with the
  existing manual reader: `ContractEnd(ctx, "scripts", "ready")` and `("scripts", "bogus")` → series
  `end="ready"` and `end="other"`; `ClassWithoutHoldout(ctx, "Smana/cloud-native-ref", "docs-links")` → gauge 1 with both labels.
- [ ] **Step 2:** Run → fails.
- [ ] **Step 3: Implement** on `LoopRun`'s and `githubUp`'s patterns. `warnConfig` logs each
  `cfg.Warnings` entry (`log.Warn("config warning", "warning", w)`) and, given the `*fmetrics.Set`,
  calls `ClassWithoutHoldout` for each live class without holdouts; call it beside `warnIgnored(log, cfg)`.
  Add `ContractEnd` to the reconciler's `metrics` interface and `fakeMetrics` (recording `class/end` pairs).
- [ ] **Step 4:** `go test -race -count=1 ./internal/app/ ./internal/factory/fmetrics/ ./internal/factory/reconciler/ -run 'Warning|Ignored|Contract|Holdout'` → `ok`.
- [ ] **Step 5:** Commit `feat(factory): config warnings at start, the contracts counter and the class-without-holdout gauge (tester F3)`.

### Task 3: `status.contract`

**Files:** Modify `api/factory/v1alpha1/task_types.go`; regenerate.

- [ ] **Step 1:** Add, after `Scope`:

```go
	// The test contract (v3 feature 4, TF4, R14): the tester run's commit on agent/<task> as GitHub
	// recorded it, its test files and ids. Set once, when the tester's push is read; every implementer
	// run is held to it and the merge gate requires merge.contractCheck (rows 2b, 2c).
	// +optional
	Contract *ContractStatus `json:"contract,omitempty"`
```

```go
// ContractStatus is the tester's commit, its files and the test ids it names.
type ContractStatus struct {
	// +kubebuilder:validation:MaxLength=253
	RunID string `json:"runId"`
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	Commit string `json:"commit"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:items:MaxLength=512
	Files []string `json:"files"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:items:MaxLength=512
	IDs []string `json:"ids"`
}
```

- [ ] **Step 2:** `task crd:gen`; `task crd:check` → no diff after the commit; `go test -race -count=1 ./api/...` → `ok`.
- [ ] **Step 3:** Commit `feat(factory): status.contract on the Task (tester F3)` with `api/`, `config/crd/`, `charts/agent-factory/crds/`.

### Task 4: The forge reads

**Files:** Modify `internal/factory/forge/forge.go`, `github.go`, `github_test.go`, `forgetest/fake.go`, `reconciler/reconciler.go` (`TaskForge`).

**Interfaces produced:** `(*GitHub).Commit(ctx, sha) (Commit, error)` (in `TaskForge`);
`(Commit).Trailers(key) []string`; `(*GitHub).ChangedFiles(ctx, base, head string) ([]string, error)`
(in `Merger`); `ErrNotDescendant`; fake: `SetCommit(sha, message string)`, `Commit`, `SetChangedFiles(base, head string, files []string, err error)`, `ChangedFiles`.

- [ ] **Step 1: Failing tests** in `github_test.go` on its `httptest` recorder (as `TestBranchActivityReadsBackToTheCreation`):
  - `TestCommitReadsOneCommitsMessage`: `GET /repos/o/r/commits/<sha>` → `Commit{SHA, Author, Message}`.
  - `TestTrailersReadsEveryValueOfTheLastParagraph`: a message with two `Agent-Contract-File` lines,
    a body line `Agent-Contract-File: x` in an earlier paragraph → two values, the body's not among them.
  - `TestChangedFilesListsTheCompare`: compare `ahead` with three files → their paths; status `diverged`
    → `ErrNotDescendant`; 300 files on the first page → an error wrapping `ErrDiffTooLarge` (fail closed).
- [ ] **Step 2:** `go test -count=1 ./internal/factory/forge/ -run 'Commit|Trailers|ChangedFiles'` → fails.
- [ ] **Step 3: Implement.** `Commit` through `g.rest.Repositories.GetCommit` (`contents: read`, the
  factory App's). `Trailers` beside `trailer()`, same paragraph rule, every match. `ChangedFiles`
  through `CompareCommits(base, head, PerPage: 300)`; `cmp.GetStatus()` not in `ahead`, `identical` →
  `ErrNotDescendant`; `len(cmp.Files) == 300` → `ErrDiffTooLarge`; renames list both names (`Previous` too).
  Fake: maps keyed by sha / `base..head`.
- [ ] **Step 4:** `go test -race -count=1 ./internal/factory/forge/...` → `ok`; `go build ./...`.
- [ ] **Step 5:** Commit `feat(factory): a commit by sha, every value of a trailer, and the changed files of a compare (tester F3)`.

### Task 5: The tester stage

**Files:** Create `internal/factory/reconciler/tester.go`, `tester_test.go`. Modify `reconciler.go` (`queued`), `implement.go` (`phaseFor`, `recordExisting`, `withChecks`, `resume`), `text.go` (`TesterBrief`).

**Interfaces produced:** `testerDue(t) bool`, `testerRun(t) bool`, `(r).testerSpec(t, text) runs.Spec`,
`(r).startTester(ctx, t) error`, `TesterBrief(t, nonce string, sc *Scoped) string`.

- [ ] **Step 1: Failing tests** (`tester_test.go`, the scoper tests' rig):

```go
// testerRig is a public scripts task whose class switches testFirst on, triaged and queued once.
func testerRig(t *testing.T, edit func(*v1alpha1.Task)) *rig {
	t.Helper()
	tk := issueTask("3buqdlot", 7, "# Add --dry-run to the script\n\n## Acceptance check\n`scripts/ci/tests/run.sh` passes")
	tk.Spec.PredictedClass = "scripts"
	if edit != nil {
		edit(tk)
	}
	g := newRig(t, tk)
	rc := g.r.Cfg.RepositoryFor(tk.Spec.Repository)
	rc.Classes["scripts"] = config.Class{Checks: true, TestFirst: true}
	rc.Merge.ContractCheck = "Test contract 🧪"
	g.reconcile(t, "3buqdlot", 3)
	return g
}

func TestATestFirstClassStartsWithTheTester(t *testing.T) {
	g := testerRig(t, nil)
	s := g.runs.specs[rid(0)]
	if s.Role != "tester" || s.Branch != "agent/3buqdlot" || s.BaseRef != "main" || !s.Checks || s.TaskURL != "" {
		t.Fatalf("the tester's claim %+v", s)
	}
	if s.Model != g.task(t).Spec.Budget.Model || s.MaxTokens != g.task(t).Spec.Budget.RunTokens {
		t.Fatalf("the tester runs on the implementer's spec (R19): %+v", s)
	}
	for _, want := range []string{"Write failing tests", "never push", "TASK-DATA-"} {
		if !strings.Contains(s.TaskText, want) {
			t.Errorf("the tester's brief lacks %q", want)
		}
	}
	if g.task(t).Status.Phase != v1alpha1.PhaseImplementing {
		t.Fatalf("phase %s: the tester is a writer (TF1)", g.task(t).Status.Phase)
	}
}

func TestTestFirstOffOrAnInternalTaskRunsTodaysSequence(t *testing.T) {
	for name, edit := range map[string]func(*v1alpha1.Task){
		"internal": func(tk *v1alpha1.Task) { tk.Spec.DataClass = "internal" },
		"docs":     func(tk *v1alpha1.Task) { tk.Spec.PredictedClass = "docs" },
	} {
		if s := testerRig(t, edit).runs.specs[rid(0)]; s.Role != "implementer" {
			t.Errorf("%s: first run %s, want the implementer", name, s.Role)
		}
	}
}

func TestTheScoperRunsBeforeTheTester(t *testing.T) { /* scoper mode on scripts: rid(0) triager, then rid(1) tester */ }

func TestALostTesterResumesAsATester(t *testing.T) { /* g.runs.lose(rid(0)); reconcile → rid(1) role tester, trigger resume */ }
```

- [ ] **Step 2:** `go test -count=1 ./internal/factory/reconciler/ -run 'Tester|TestFirst'` → fails.
- [ ] **Step 3: Implement** `tester.go`:

```go
// testerDue (F3-R1): the class writes its tests first, the task is public and holds no contract, and no
// tester ran yet, or the last one was lost and resumes.
func (r *Reconciler) testerDue(t *v1alpha1.Task) bool {
	if !r.repo(t).TestFirstFor(t.Spec.PredictedClass) || t.Spec.DataClass != "public" || t.Status.Contract != nil ||
		!slices.Contains(r.Cfg.Templates[t.Spec.Template].Roles, "implementer") {
		return false
	}
	if len(t.Status.Runs) > 0 && current(t).Role == "tester" {
		return nextTrigger(t) == "resume"
	}
	return !slices.ContainsFunc(t.Status.Runs, func(x v1alpha1.RunRecord) bool { return x.Role == "tester" })
}

// testerRun: the task's current run is its tester's and has not been read into a contract or its absence.
func testerRun(t *v1alpha1.Task) bool { return len(t.Status.Runs) > 0 && current(t).Role == "tester" }

func (r *Reconciler) testerSpec(t *v1alpha1.Task, text string) runs.Spec {
	s := r.implementerSpec(t, text)
	s.Role = "tester"
	r.withChecks(&s, t)
	return s
}

func (r *Reconciler) startTester(ctx context.Context, t *v1alpha1.Task) error {
	text, ok, err := r.resumeBrief(ctx, t)
	if err != nil {
		return err
	}
	if !ok {
		sc, err := r.taskScope(ctx, t)
		if err != nil {
			return err
		}
		text = TesterBrief(t, r.Nonce(), sc)
	}
	return r.startRun(ctx, t, r.testerSpec(t, text), nextTrigger(t))
}
```

  In `queued`, after `if scoperDue(t) { … }`: `if r.testerDue(t) { return r.startTester(ctx, t) }`.
  `withChecks`: allow `s.Role == "tester"`. `phaseFor`: only the reviewer is `Reviewing`.
  `recordExisting`: only the reviewer's trigger is `review`. `resume`: only the reviewer goes through
  `requestVerifier`. `TesterBrief` in `text.go`, on `firstBrief`'s pattern (snapshot, steering, scope):

```go
// TesterBrief is the tester's task (TF1, TF3): the issue and its scope, and the one job, failing tests.
func TesterBrief(t *v1alpha1.Task, nonce string, sc *Scoped) string {
	head := "You are the tester. Write failing tests that pin down what this task asks for, before anyone changes " +
		"the code: they must fail on the current code and pass once the task is done. Write them only under the " +
		"paths your rules' Test contract section names, commit them on your branch and never push: the platform " +
		"checks that they fail and pushes them. Never change the code under test. If the task has no behaviour a " +
		"test can pin down, commit nothing and say why in the room with room_post, then finish.\n\n"
	return fit(head, scopeBlock(sc, nonce), snapshotBlock(t, "TASK-DATA-"+nonce), t)
}
```

  (Use `firstBrief`'s real helpers and their argument order as they stand in `text.go`; the wording
  above is the contract the test checks.)
- [ ] **Step 4:** `go test -race -count=1 ./internal/factory/reconciler/` → `ok`.
- [ ] **Step 5:** Commit `feat(factory): the tester stage runs first on a testFirst class (tester F3)`.

### Task 6: The tester's end, read from GitHub (R14)

**Files:** Modify `tester.go`, `tester_test.go`, `implement.go` (`implementing`), `narrate/narrate.go` (two events).

**Interfaces produced:** `contractPushWait = 5 * time.Minute`, `(r).testerEnded(ctx, t, run, reason) error`,
`(r).readContract(ctx, t) (*v1alpha1.ContractStatus, string, error)` (the contract, or the end when none),
`(r).testerNote(ctx, t) string` (the end note's category, `""` for none),
`narrate.ContractReady(t, c)`, `narrate.NoContract(t, end)`.

- [ ] **Step 1: Failing tests** (fake forge: `SetActivity`, `SetCommit`; the room log: a progress
  message `Harness (tester): <end>: …` from `rid(0)`):

| Test | Given | Then |
|---|---|---|
| `TestARedContractIsRecordedAndTheImplementerIsBriefed` | one `branch_creation` by `ogenki-agents[bot]` to `C`; `Commit(C)` with `Agent-Run`/`Agent-Contract: rid(0)`, two files, three ids | `status.contract` = `{rid(0), C, files, ids}`; `rid(1)` implementer; counter `scripts/ready`; the issue narration "a contract of 3 tests" |
| `TestNoBranchIsNoContractWithTheNotesCategory` | no activity; note `not_testable: …` | no contract, `rid(1)` implementer, counter `not_testable`, narration names `not_testable` only |
| `TestAReadyNoteWithoutABranchWaitsThenIsUnreported` | no activity; note `contract_ready: …` | `Reason` `contract_push_unrecorded`, no run; after `contractPushWait` → implementer, counter `unreported` |
| `TestAForeignPushOrTrailerIsContractInvalid` | (a) the creation by another login; (b) two pushes; (c) `Agent-Contract: rid(9)`; (d) no `Agent-Contract-Id` | each: `Escalated`, `contract_invalid`, no implementer |
| `TestAStoppedTesterEscalatesAndALostOneResumes` | `revoke(rid(0), "manual")`; `lose(rid(0))` | `Escalated manual`; `rid(1)` tester, trigger `resume` |
| `TestAFailedTesterRunWithoutABranchIsNoContract` | run `Failed`, reason `agent_error`, no branch | no contract (TO3), counter `unreported` (no note) |

- [ ] **Step 2:** Run → fails.
- [ ] **Step 3: Implement.** In `implementing`, the tester branch goes **after** the `infraLost` block
  (a lost tester resumes there, F3-R3) and before the loop metric:

```go
	if testerRun(t) {
		return r.testerEnded(ctx, t, run, reason)
	}
```

```go
// testerEnded (R14, F3-R3..R5): a person or a budget stopping it escalates as any run; otherwise GitHub
// says whether the tester's harness pushed a contract.
func (r *Reconciler) testerEnded(ctx context.Context, t *v1alpha1.Task, run runs.Run, reason string) error {
	if slices.Contains(escalatesScoper, reason) {
		return r.end(ctx, t, v1alpha1.PhaseEscalated, reason)
	}
	c, end, err := r.readContract(ctx, t)
	switch {
	case err != nil:
		return err
	case end == "wait":
		t.Status.Reason = "contract_push_unrecorded"
		return nil
	case end == "contract_invalid":
		record(ctx, func(ctx context.Context) { r.Metrics.ContractEnd(ctx, t.Spec.PredictedClass, end) })
		return r.end(ctx, t, v1alpha1.PhaseEscalated, end)
	case c != nil:
		t.Status.Contract = c
		end = "ready"
		narrateLater(t, narrate.ContractReady(t, c))
	default:
		narrateLater(t, narrate.NoContract(t, end))
	}
	record(ctx, func(ctx context.Context) { r.Metrics.ContractEnd(ctx, t.Spec.PredictedClass, end) })
	t.Status.NextTrigger = "initial"
	r.to(t, v1alpha1.PhaseQueued, "")
	return nil
}
```

  `readContract` follows F3-R4 exactly: `BranchActivity`; empty → `testerNote`: `"contract_ready"`
  or `""` while `r.Now()` is within `contractPushWait` of `current(t).Finished` → `"wait"`; then
  `"unreported"` (or the note's category). Non-empty → `pushedBy(branch, []string{r.Cfg.AgentsLogin}, ps[0].After)`
  must be `pushFactory` with `len(ps) == 1`; `Commit(ps[0].After)`; `Trailer("Agent-Contract") == cur.ID`
  and `Trailer("Agent-Run") == cur.ID`; `Trailers("Agent-Contract-File")` and `…-Id` non-empty, each at
  most 50 and 512 bytes (the CRD's bounds: over them is `contract_invalid`, never a truncated contract).
  `testerNote` reads the room after `cur.StartSeq` for `envelope.Message` events of `cur.ID` whose
  payload kind is `progress` and whose text starts `Harness (tester): `; the category is the word
  before the next `:` when it is one of the F1 ends.
- [ ] **Step 4:** `go test -race -count=1 ./internal/factory/reconciler/ ./internal/factory/narrate/` → `ok`.
- [ ] **Step 5:** Commit `feat(factory): the tester's contract read from GitHub's push record and its trailers (tester F3)`.

### Task 7: The contract in every implementer brief, and `contract_disputed`

**Files:** Modify `text.go` (`contractBlock`, used by `firstBrief` and `reviseBrief`), `watch.go`, `implement.go`, `tester_test.go`, `text_test.go`.

- [ ] **Step 1: Failing tests:** `TestEveryImplementerBriefCarriesTheContract` — the first brief, a
  revision after a reviewer's `changes`, a CI fix (`fixCI`) — each holds the commit's first 12, every
  file, every id, "may not change" and "room_handoff to tester"; a task without a contract → no block
  (`text_test.go` golden unchanged). `TestAHandoffToTheTesterIsContractDisputed`: the implementer's
  run ends `Succeeded` with its own handoff `toRole: tester` in the room → `Escalated`,
  `contract_disputed`; the same handoff on a task without a contract → today's path.
- [ ] **Step 2:** Run → fails.
- [ ] **Step 3: Implement.** `contractBlock(c *v1alpha1.ContractStatus, nonce string) string` —
  "## The test contract" with the files and ids **between `CONTRACT-<nonce>` fence lines** (they are
  the tester's text), the rule, and "If the contract is wrong, hand off to tester with room_handoff
  and say why: the task goes to a maintainer." In `implementing`, before `case run.Phase == "Succeeded" && t.Status.PullRequest != nil`:

```go
	if t.Status.Contract != nil && current(t).Role == "implementer" {
		evs, _, err := r.roomTail(ctx, t.Status.RoomRef, current(t).StartSeq, func(e envelope.Event) bool {
			return handoffTo("tester")(e) && e.RunID == current(t).ID
		})
		if err != nil {
			return err
		}
		if len(evs) > 0 {
			return r.end(ctx, t, v1alpha1.PhaseEscalated, "contract_disputed")
		}
	}
```

- [ ] **Step 4:** `go test -race -count=1 ./internal/factory/reconciler/` → `ok`.
- [ ] **Step 5:** Commit `feat(factory): every implementer brief carries the contract; a handoff to the tester escalates contract_disputed (tester F3)`.

### Task 8: The verifier tester retired (TO1, R18, F1-R2)

**Files:** Modify every file of the row "Task 8" in the file table, and their tests.

- [ ] **Step 1: Failing tests,** one per surface (each a small table test where one exists):
  `nextVerifier` on `[implementer, reviewer]` returns `reviewer` and never `tester`;
  `verifiers(t)` skips a tester record; `agentEvidence` ignores a tester's approve; `rooms.verifier("tester")`
  is false; `internal/mcp`: `room_verdict` is listed for the reviewer only (`tools_test.go`'s role table);
  the store's `UnpostedVerdicts` returns no tester verdict (its testcontainers test); `api.validate`
  of a tester run → `tester_is_factory_only`; `humanapi.validRun` of a tester → false, and the human
  MCP's `role` enum lacks `tester`; `roomview` card: a tester run is shown as a writer, not a review.
- [ ] **Step 2:** `go test -count=1 ./internal/... -run '<those names>'` → fails.
- [ ] **Step 3: Implement** each: replace `role == "reviewer" || role == "tester"` by
  `role == "reviewer"` at `team.go:38`, `implement.go` (already done in Task 5 for three of them),
  `merge.go` `verifiers`, `arm.go` `agentEvidence`, `rooms/rooms.go` `verifier`, `narrate.go:479`,
  `roomview/card.go:429`; `store/verdicts.go`: `e.actor_role = 'reviewer'`; `mcp/tools.go`:
  `room_verdict` roles `[]string{"reviewer"}`; `api/server.go`: a `case in.Role == "tester": return
  "tester_is_factory_only"` before the URL case; `humanapi/startrun.go`: drop `tester` from
  `runRoles`; `humanapi/mcp.go`'s schema enum and `mcpact.go`'s message; `app/roomctl.go`'s flag help.
  Update every comment that says "reviewer or tester".
- [ ] **Step 4:** `task test` → `ok`. List in the PR every pre-existing test whose expectation moved.
- [ ] **Step 5:** Commit `feat(factory)!: the verifier tester retires, room_verdict is the reviewer's (TO1, tester F3)`.

### Task 9: Row 2b, the contract check required on a task with a contract

**Files:** Modify `arm.go`, `merge.go`, `gate.go`, `arm_test.go`, `merge_test.go`.

**Interfaces produced:** `requiredFor(rc *config.Repository, t *v1alpha1.Task) []string`.

- [ ] **Step 1: Failing tests** in `arm_test.go`, on its `DecideArm` table: a task with
  `status.contract` and every required check green: the contract check missing → `wait ci_pending`;
  `PENDING` → `wait`; `FAILURE` → `ci_red`; `SUCCESS` → the next row. The same task without a contract
  and the contract check `FAILURE` → not `ci_red` (report-only, TF7). In `merge_test.go`: `fixCI` on a
  red contract check names it in the fix run's message; `unreported` lists it while pending.
- [ ] **Step 2:** Run → fails.
- [ ] **Step 3: Implement.**

```go
// requiredFor is row 2's list (R21, F3-R8): merge.requiredChecks, and merge.contractCheck on a task whose
// tester wrote a contract. Every reader of the required checks calls this, never the config's list.
func requiredFor(rc *config.Repository, t *v1alpha1.Task) []string {
	if t.Status.Contract == nil || rc.Merge.ContractCheck == "" {
		return rc.Merge.RequiredChecks
	}
	return append(slices.Clone(rc.Merge.RequiredChecks), rc.Merge.ContractCheck)
}
```

  Replace `rc.Merge.RequiredChecks` / `r.repo(t).Merge.RequiredChecks` at `arm.go:170`,
  `merge.go:101,107,297,608,643,838` and `gate.go:155` (`in.Cfg.RepositoryFor(…)` → `requiredFor(rc, in.Task)`).
  Then `grep -rn "Merge.RequiredChecks" internal/factory/reconciler --include=*.go | grep -v _test` →
  only `requiredFor` itself.
- [ ] **Step 4:** `go test -race -count=1 ./internal/factory/reconciler/` → `ok`.
- [ ] **Step 5:** Commit `feat(factory): row 2b, the contract check required on a task with a contract (tester F3)`.

### Task 10: Row 2c, `contract_modified`

**Files:** Modify `arm.go` (`ArmInputs.Contract`, `baseChecks`), `merge.go` (`awaitingCI` reads it), `arm_test.go`, `merge_test.go`.

- [ ] **Step 1: Failing tests** (`DecideArm` table, CI green, contract set): `Contract.Read` false →
  `wait contract_unread`; `Changed` holds a contract file → `human contract_modified`; holds other
  files only → the next row; no contract → the row is skipped whatever `Changed` says. In `merge_test.go`:
  `awaitingCI` calls `ChangedFiles(contract.commit, head)` once per decision, only when CI is green
  and a contract exists; `ErrNotDescendant` → every contract file counts as changed → `contract_modified`;
  another error → the error is returned (the pass retries), never a pass.
- [ ] **Step 2:** Run → fails.
- [ ] **Step 3: Implement.** `ArmInputs.Contract struct{ Read bool; Changed []string }`. In `baseChecks`,
  after the CI `switch` and before `NotOwned`:

```go
	if c := in.Task.Status.Contract; c != nil {
		if !in.Contract.Read {
			return ArmDecision{Verdict: "wait", Reason: "contract_unread"}, true
		}
		if slices.ContainsFunc(c.Files, func(f string) bool { return slices.Contains(in.Contract.Changed, f) }) {
			return ArmDecision{Verdict: "human", Reason: "contract_modified"}, true // TF8 2c: never a fix run
		}
	}
```

  In `awaitingCI`, beside the adoption read, when `CIState(checks, requiredFor(rc, t)) == "SUCCESS"`
  and `t.Status.Contract != nil`: `files, err := r.merger(ctx).ChangedFiles(ctx, c.Commit, pr.HeadSHA)`;
  `errors.Is(err, forge.ErrNotDescendant)` → `files = c.Files`; other errors return; then
  `in.Contract = struct{...}{true, files}`. `ArmDecision` `human` reasons go through the existing
  hand-off narration (`WaitingForHuman`).
- [ ] **Step 4:** `go test -race -count=1 ./internal/factory/reconciler/` → `ok`.
- [ ] **Step 5:** Commit `feat(factory): row 2c, a changed contract file goes to a human (tester F3)`.

### Task 11: The reasons' prose

**Files:** Modify `narrate/narrate.go` (`reasons()`, `headlines()`/`hints()` where an escalation needs one), `narrate/merge_how_test.go`, `internal/summary/summary.go`.

- [ ] **Step 1:** Add `contract_invalid`, `contract_disputed`, `contract_modified`, `contract_unread`,
  `contract_push_unrecorded` to `waitReasons` in `merge_how_test.go` → `TestSummaryReasonsTrackNarrate` fails.
- [ ] **Step 2: Implement** the prose in both packages:
  - `contract_invalid`: "the tester's branch was not the tester's alone, or its contract commit was malformed"
  - `contract_disputed`: "the implementer says the tester's tests are wrong; a maintainer decides"
  - `contract_modified`: "the pull request changes a test the tester wrote as the task's contract"
  - `contract_unread`: "the factory has not yet read which files changed since the test contract"
  - `contract_push_unrecorded`: "GitHub has not yet recorded the tester's push"
- [ ] **Step 3:** `go test -race -count=1 ./internal/factory/narrate/ ./internal/summary/` → `ok`.
- [ ] **Step 4:** Commit `feat(factory): the contract reasons in the narration and the summary (tester F3)`.

### Task 12: Offline proof 4, end to end on envtest

**Files:** Create `internal/factory/reconciler/tester_envtest_test.go` (on `adopt_envtest_test.go`'s / `split_envtest_test.go`'s rig).

- [ ] **Step 1: Write** the journeys, each through the real API server:
  1. `testFirst` off: the rig's existing `pair` journey's role sequence and every status field are
     unchanged (compare to the same journey on a config without the keys).
  2. On: tester → contract (fake push record and commit) → implementer whose claim's task text holds
     the contract → reviewer `approve` → `AwaitingCI`: the contract check pending waits, red starts a
     fix run naming it, green with a changed contract file → `AwaitingHuman contract_modified`; green
     and unchanged → the class rows.
  3. On, the tester leaves nothing: the implementer runs without a contract and the contract check is
     not required (red there arms nothing differently).
  4. A shadow class with `holdoutChecks` and a contract: rows 2b and 2c decide before `holdout_pending`.
  5. Startup: a config with `trio: [implementer, tester, reviewer]` and a live class without holdouts
     loads; the warnings are logged; the gauge reads 1.
- [ ] **Step 2:** `task test` → `ok`.
- [ ] **Step 3:** Commit `test(factory): offline proof 4, the tester's journeys end to end (tester F3)`.

### Task 13: The docs, `task check`, the pull request, the release

**Files:** Modify `docs/factory.md`.

- [ ] **Step 1:** `docs/factory.md`: in "A task's life", the tester stage after the scoper; in "The
  merge gate", rows 2b and 2c in the table; in "Triage: tiers, templates, classes", `trio` without the
  tester and the one-release drop; in "The config file", `testFirst`, `merge.contractCheck`, the
  `review` declaration; in "The Task CRD", `status.contract`; the counter and the gauge in the metrics
  reference; the five reasons. No per-row status tags (`task docs`).
- [ ] **Step 2:** `task check` → exit 0. Paste its tail in the pull request.
- [ ] **Step 3:** `git fetch origin && git rebase origin/main && git push -u origin feat/tester-f3-factory`;
  `gh pr create --title "feat(factory): the test-first tester stage, status.contract, rows 2b and 2c, the verifier tester retired (tester F3)"`,
  the body listing F3-R1..R13, the gaps closed (Tasks 1, 4) and every moved test (Task 8). After the
  merge, the owner cuts the release F5 pins; the breaking change (`!`) is the API's refusal of a person's tester run.

## Cross-task conflicts

| Between | Conflict | Resolution |
|---|---|---|
| Tasks 5, 6, 7, 8 | All edit `implement.go` (`phaseFor`, `recordExisting`, `resume`, `implementing`) | Sequential, in task order; Task 8 only touches what Task 5 left |
| Tasks 9 and 10 | Both edit `baseChecks` and `awaitingCI` | 9 first (the list), 10 inserts after the CI switch |
| Task 8 and the store | `store/verdicts.go`'s query is covered by testcontainers | Docker running for `task test` |
| Task 1 vs ai-native-ref's config | The deployed config lists `tester` in `trio` and two live host classes without holdouts | Both load with warnings (by design); F5 removes the warnings |
| F3 vs F1 | F1's end note format and trailers | F3 reads them as F1 writes them; Task 6's tests use F1's literal strings |
| F3 vs merge gate PR D (holdouts declared in ai-native-ref) | F5 adds the same `holdoutChecks` | F5 adds them only if PR D did not |
| F3 vs open PRs | #239 (UI), #165 (draft, install-anywhere) | No reconciler overlap; #165 touches charts: rebase the CRD copy |
| F3 vs RunLore stage 2 | ai-native-ref #168 is config only | None in agent-platform |

## Self-review

- **Inherit row F3:** `Class.TestFirst`, `Merge.ContractCheck`, `Config.Warnings` (Task 1);
  `TaskStatus.Contract` (Task 3); the tester stage and spec, `withChecks`, `phaseFor` (Task 5); the
  forge read before the implementer and the requeue (Task 6); every brief, `contract_disputed` (Task 7);
  the verifier sets, `room_verdict` (Task 8, F1-R2); rows 2b, 2c with `ChangedFiles` (Tasks 4, 9, 10);
  the reasons (Task 11); the counter and gauge (Task 2); `docs/factory.md` (Task 13).
- **Offline proof 4:** Tasks 5–12; "a template listing `tester` warns" and "a live class with no
  holdout warns and sets the metric": Tasks 1, 2, 12.
- **Deviations from the spec, carried from the parent plan:** R14 (the forge, not the end note),
  R20 (no `holdout_absent`). New here: F3-R4's single-push rule and F3-R9's `contract_unread`.
