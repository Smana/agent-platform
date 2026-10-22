# v3 feature 12, spend per identity — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A maintainer opens one Grafana page per charged principal (`github:<login>`, `system:<name>`,
`system:factory`) and sees what the platform spent on the work that principal caused, against its
day and the fleet's, with its tokens, runs, outcomes, refusals, revocations and waits. The page
attributes cost; it scores nobody.

**Architecture:** agent-platform gives every run one payer, `runs.Run.Payer()`: the last ledger key
the meter charges it to. The meter adds each run's settled charge to
`agent_factory_charged_usd_total{principal, kind}` when the write that settles it succeeds (or when
`resolve` finds a write in doubt landed), and hands the live runs to
`agent_factory_run_charge_info{run, principal, task, room}`. The factory's existing series gain
`principal` through the context, as they gained `repository` (`fmetrics.WithPrincipal`), and two
counters are new: `agent_factory_prs_opened_total` and `agent_factory_human_wait_seconds_total`.
ai-native-ref adds the `agent-identity` dashboard, links to it, and two suites: one on the
dashboard's shape and framing, one that runs the panels' own queries under promtool.

**Tech Stack:** Go 1.27 (agent-platform: OpenTelemetry metric API, controller-runtime fake client,
stdlib `testing`), Grafana dashboard JSON in a `GrafanaDashboard`, Python 3 with PyYAML and Bash suites
run by `scripts/ci/run-tests.sh`, promtool 3.15.0, VictoriaMetrics (`increase_pure`).

**Spec:** ai-native-ref [`docs/superpowers/specs/2026-10-22-spend-per-identity-design.md`](https://github.com/Smana/ai-native-ref/blob/main/docs/superpowers/specs/2026-10-22-spend-per-identity-design.md)
(on ai-native-ref `main` since #100, approved 2026-10-22 with OD-A to OD-C at their defaults):
decisions SI1–SI11, "Interfaces", the Gate's offline proofs 1–8, and "Pull requests, in order".

**Three pull requests, in order:**

| PR | Repository, branch | Tasks | Offline proofs | Depends on |
|---|---|---|---|---|
| 1 | agent-platform `feat/spend-per-identity` (this plan's branch) | 1–6 | 1–5 | Truthful costs PR 2 (`feat/daily-caps-usd`) merged; rebased onto `main` then |
| 2 | ai-native-ref `feat/spend-identity-dashboard` | 7–9 | 6–8 | Nothing: until PR 3, the panels read No data |
| 3 | ai-native-ref, the factory pin | 10 | the pins suite | PR 1 released: folded into truthful costs PR 3 when PR 1 rides v0.16.0 |

**Out of this plan:** F4's `agent_factory_principal_sponsor_info` and `agent_factory_max_cost_usd_total`
(SI9: the Sponsored row reads No data until F4 records them). Alerts per principal. Approval waits
in human rooms (OD-B). Backfill: the series start with PR 1's release.

### This plan's rulings

| # | Ruling | Why |
|---|---|---|
| R1 | **The payer lives in `runs`:** `func (r Run) Payer() string` (`ChargedTo`, else `Principal`), `func (r Run) Kind() string` (`KindTask` when `TaskID` is set, else `KindRoom`), and `func TaskPayer(t *v1alpha1.Task, perPerson bool) string` (`t.Payer()` while per-person caps are on, else `system:factory`) | Left to the builder. The meter, the reconciler, the sweep and fmetrics all import `runs` already (fmetrics through `killswitch`), so one home serves all four with no new import edge. `charged(r)`'s last key is `Payer()` by construction |
| R2 | **The meter's two callbacks:** `OnCharge func(ctx, principal, kind string, microUSD int64)`, once per run per successful write (and per write `resolve` finds landed), wired to `fmetrics.Set.Charged`; `OnCharges func(live []runs.Run)`, each tick that listed the runs, before the usage read, wired to `fmetrics.Set.RunCharges` | Left to the builder. As `OnSpend` hands `RunSpend` a fresh map, `OnCharges` hands a fresh slice: no state shared across the two goroutines. Called before the usage read, the charge info never freezes on a usage outage |
| R3 | **How the meter remembers a run's payer for `resolve`:** the run's `account` gains `payer, kind string`, set when the run's charge enters `inc`. The counter counts what `spent` gains, never R10's lift: on a successful write `n - a.lift`, and in `resolve` `gain - a.lift` when the write landed | Left to the builder. PR 2's head (`871ced8`) records a lifted run's lift in its `settled.<runId>` with its next charge and never in `spent`, so "the counter equals `settled`" holds only without a lift. The counter follows `spent`, which is what the design's partition and ledger match mean: per person key its day's increase is `spent`, over payers the fleet's |
| R4 | **The charge info's window** is read from `status.finishedAt`: a terminal run is named while `now − finishedAt < endSettle` (2 min); one without a `finishedAt` never | The spec says "finished less than `endSettle` ago" and "never for an ended run a new leader first sees after that window". `finishedAt` is set by both runtimes (the composition and `internal/runctl`), so the rule needs no memory and two leaders agree |
| R5 | **The principal's shape:** `^(system:[a-z0-9][a-z0-9-]{0,62}\|github:[a-z0-9][a-z0-9-]{0,38}\|human:[A-Za-z0-9][A-Za-z0-9_-]{0,63})$`, else `other`; an unbound context reads `other`. The shadow's key is bounded the same way | Left to the builder. `v1alpha1.Person` lowercases logins (GitHub's limit is 39); ZITADEL subjects are numeric. A key the ledger holds always matches, so bounding the shadow changes no value it records today |
| R6 | **API refusals name the requester as authenticated** (`p.ID`, `human:<sub>` for a human), and `person_budget_exhausted` names the person whose day is spent. The intake's refusal names the labeller | The spec: "the API (the requester) and the intake (the labeller)". Naming a linked caller's `github:<login>` on a refusal would need the identity lookup before the local refusals, the call `mayWrite` is ordered last to save. So the framing table's "`human:<sub>` only for an API caller with no GitHub link" holds for spend, and refusals are its one documented exception |
| R7 | **A wait is measured from the status, after its write:** `Reconcile` compares the status before the pass with the written one; leaving `AwaitingHuman` or `Escalated` records `new phaseSince − old phaseSince` under the payer bound at the pass's start | SI8 says "recorded after the status write as task metrics are". `to()` sets `phaseSince` on every phase change, so the difference is exact and needs no clock read. The payer at the pass's start is the work that waited: a `/factory retry` that ends a wait does not move it to the retrier |
| R8 | **Revocations name the run's payer:** the meter binds `r.Payer()` on `OnRevoke`'s context; the reconciler's stop binds each run's; the kill-switch sweep returns the runs it revoked (`Sweep(ctx) ([]runs.Run, error)`, `OnSweep func([]runs.Run)`) and the wiring counts each under its payer. The reconciler's own `budget-task` and `budget-person` records, made before a run exists, take the task's payer (bound for the pass) | SI4: "on every revocation the factory or the meter records". The sweep counted `n` with no run, so its signature changes; it is internal |
| R9 | **The tasks gauge's payer:** `fmetrics.Set.ChargePeople(on bool)`, set at startup from `budgets.perPerson.enabled`; the collector names `runs.TaskPayer(t, on)` | The collector has no config, and `ListRepositories` is the existing setter pattern |
| R10 | **Openings:** `fmetrics.Set.PROpened(ctx)`, recorded in the same `record` closure as `TimeToPR` | SI4: "where `TimeToPR` records". One closure keeps them on the same status write |
| R11 | **The dashboard's variable is written `$${principal}`**, as every agent dashboard writes its variables. Time zone `utc`, default range 30 days. "Spend per day": `"interval": "1d"` and `increase_pure(…[1d])`. Each Runs-table target is restricted with `and on (run_id)` the payer's charge info | The design wrote `$principal`, which Flux also leaves alone; one convention per folder, and the observability suite's escape check covers the braced form. A 1-day step aligns to UTC days, the ledger's. `and on` keeps the KSM columns and drops every other principal's run, where `merge` alone would add their rows |
| R12 | **No recording rules.** Every panel reads raw series | The series are few (principals × 2 for spend) and per-page; a rule would add a name to keep in step and nothing a page needs |
| R13 | **The suites:** `test-agent-identity-dashboard.py` (offline proof 6), `test-agent-identity-queries.sh` (offline proof 7) | Left to the builder. Named after the dashboard, as `test-agent-observability` is after its files |
| R14 | **Docs.** agent-platform: `docs/operations.md` (rows and a "Spend per identity" section) and AGENTS.md's label rule amended for `room` on per-live-run series (OD-C). ai-native-ref: `observability.md`, the dashboard row in `routing.md`, and the status row's "how long runs waited on that person" corrected to "how long its tasks waited for a human" (SI8) | OD-C: "PR 1 amends the sentence". The status row's wording timed a person, which SI7 and SI8 rule out |
| R15 | **Release.** PR 1 is not breaking: it adds labels that every existing reader sums over, and two counters. It rides v0.16.0 when it merges before that tag (Task 6, Step 6), else ships as v0.16.1 | CONTRIBUTING.md: pre-1.0, only a breaking change bumps the minor |

**Measured 2026-10-22** on prototypes, discarded since (the code below is theirs):

- **agent-platform**, on `feat/daily-caps-usd` at `97d4994` (PR 2's head, after its review commits)
  with Tasks 1–6 applied: `go build ./...`; `go test -race -count=1 ./internal/factory/...
  ./internal/app/... ./api/...` all `ok`; `task lint` `0 issues`; `hack/docs.sh` exit 0;
  `hack/spdx.sh`, `go mod tidy -diff` and `task crd:check` clean. The whole `task check` was not run:
  Docker was stopped, and `internal/store`'s testcontainers need it. CI runs it.
- **Each new test failed first:** compile errors (`Payer`, `OnCharge`, `Charged`, `boundPrincipal`
  undefined), then `[]` bound payers in the reconciler and missing series in the API scrape.
- **The partition** (offline proofs 1, 2): five runs ticked through a day settle
  2,188,000 + 464,000 + 255,000 + 156,000 + 34,000 = 3,097,000 micro-USD, which is
  `spent[system:factory]`; the counter holds alice 2,188,000 (task), `system:factory` 719,000
  (task), bob 156,000 (room), `human:292` 34,000 (room).
- **The lift** (R3): a switch-day ended run's late reading writes 1,470,000 into its settled entry
  and 70,000 into `spent`; the counter counts 70,000. With the counter fed `n` instead of
  `n − lift`, the test fails with `counted 1470000`.
- **ai-native-ref**, a clone of `main` at `76e0add` with Tasks 7–9 applied: `task check` exit 0,
  `30 passed, 2 skipped, 0 failed` (both skips are Docker suites; `main` has 28 and 2),
  `assert-agent-gateway: 14 checks, 0 violations`, `114 PromQL expression(s) in 18 group(s) parse`
  (no new rule), links, doc paths and sources pass. `test-agent-identity-dashboard` `PASS`;
  `test-agent-identity-queries` `SUCCESS`.
- **The suites bite:** a `sum by (kind, principal) (increase(…))` panel and an All value fail the
  dashboard suite with four messages; promtool given 0.5 for alice's cache share reports
  `got: {} 8.571428571428571E-01`, and given 4 runs reports `got: {} 3E+00`.

## Global Constraints

- **Worktrees.** agent-platform: `/home/smana/Sources/agent-platform-spend-identity`, branch
  `feat/spend-per-identity`, cut from `feat/daily-caps-usd`; this plan is its first commit.
  ai-native-ref: Task 7 creates `/home/smana/Sources/ai-native-ref/.claude/worktrees/spend-identity`
  on `feat/spend-identity-dashboard` from a freshly fetched `origin/main`; run `mise trust mise.toml`
  once in each new worktree.
- **PR 2 first (agent-platform).** While truthful costs PR 2 is open, keep this branch on its head:
  `git fetch origin && git rebase origin/feat/daily-caps-usd` before each task (the review added
  `871ced8`, the lift, between this plan's reading and its prototype). Once PR 2 is squash-merged,
  move only this branch's commits: `git rebase --onto origin/main <PR 2's last head> feat/spend-per-identity`.
  Never merge `feat/daily-caps-usd` into `main` from here.
- **`$SCRATCH`** is a scratch directory outside both repositories. It holds two helpers:
  `check.sh <worktree>` runs `task check` with a `python3` that has PyYAML, logging to
  `$SCRATCH/check-<worktree directory name>.log` ending in `exit=N`; `run-suite.sh <command…>` runs
  one command with that `python3`. Every ai-native-ref Python or suite command goes through one of them.
- **`task check` is the evidence** in both repositories. agent-platform's `test` step needs a running
  Docker daemon (testcontainers); without it, run the steps below per task and let CI run the whole.
- **agent-platform conventions** (its AGENTS.md): SPDX first line on every `.go` file; stdlib
  `testing`, table-driven, hand-written fakes; `-race` always; exported symbols carry doc comments;
  label values bounded; `task lint`, never a bare `golangci-lint`.
- **ai-native-ref conventions** (its AGENTS.md): suites' line 1 is the shebang and line 2
  `# requires: <tools>`, exit 77 is a SKIP, a suite counts only once committed; `$${…}` for every
  Grafana variable (Flux's escape), `$__range` and `$1` raw; `git add` named paths only;
  `export PYTHONDONTWRITEBYTECODE=1`.
- **Commits** are conventional, in English, with **no trailers**. PR bodies carry no attribution line.
- **Verbatim from the spec:**
  - SI1: "`payer(r)` is `r.ChargedTo`, else `r.Principal`: the last key of `charged(r)`. A task's
    payer is `person(t)` …, else `system:factory`: the `chargedTo` its next run gets."
  - SI2: "`agent_factory_charged_usd_total{principal, kind}`, `kind` ∈ `task` (a run of a `Task`),
    `room` (a run started through `POST /v1/runs`). The meter adds each run's charge under its payer
    when the write that settles it succeeds, or when `resolve` finds a write in doubt landed."
  - SI3: "`agent_factory_run_charge_info{run, principal, task, room}` = 1 … set by the leader for each
    run not terminal, or finished less than `endSettle` ago."
  - SI4: "`principal` on `agent_factory_pr_outcomes_total`, `agent_factory_admission_refusals_total`,
    `agent_factory_tasks`, and on every revocation the factory or the meter records. Two new
    counters: `agent_factory_prs_opened_total{principal, repository}` where `TimeToPR` records, and
    `agent_factory_human_wait_seconds_total{principal, phase}`."
  - SI5: "`principal` takes the shape of a ledger key …, else `other`. … `run`, `task` and `room`
    appear only on the per-live-run series. The shadow keeps its `principal`, the key over."
  - SI6/SI7: "`agent-identity`, 'Spend per identity', in the folder `agents`, VictoriaMetrics only,
    UTC. A `principal` variable from `label_values(agent_factory_charged_usd_total, principal)`,
    single value, no All." "no panel groups, sorts or ranks by `principal` (`by (principal)`, `topk`,
    `bottomk`, `sort`). The suite fails on any."
  - SI8: "adds `now − status.phaseSince` when a task leaves `AwaitingHuman` or `Escalated`".
  - SI10: "`auto_merged` plus `human_merged`, minus `reverted`, in the range … No data when it is 0".
  - SI11: "`increase_pure` on every per-principal counter and per-run series."
  - Offline proofs: 1–3 in Task 3, 4 in Tasks 2 and 3, 5 in Tasks 2, 4, 5 and 6, 6 in Task 7,
    7 in Task 8, 8 in Task 9.

## File structure

**agent-platform (PR 1)**

| File | Change | Responsibility |
|---|---|---|
| `internal/factory/runs/payer.go` | create | `KindTask`, `KindRoom`, `Run.Payer`, `Run.Kind`, `TaskPayer` |
| `internal/factory/runs/payer_test.go` | create | One payer and one kind per run; a task's payer |
| `internal/factory/fmetrics/fmetrics.go` | modify | `WithPrincipal`, `PrincipalOf`, the shape bound, `principal` on four series, `Charged`, `PROpened`, `HumanWait`, `RunCharges` and the charge-info gauge, `ChargePeople` |
| `internal/factory/fmetrics/principal_test.go` | create | The new series and labels, scraped |
| `internal/factory/fmetrics/{fmetrics,repository}_test.go` | modify | Existing expectations gain `principal` |
| `internal/factory/meter/meter.go` | modify | `OnCharge`, `OnCharges`, the account's payer, `charge`, `chargeInfo`, `OnRevoke`'s payer |
| `internal/factory/meter/payer_test.go` | create | Offline proofs 1–4 at the meter |
| `internal/factory/reconciler/{reconciler,implement}.go` | modify | The pass binds the task's payer; waits; openings; the stop's revocations |
| `internal/factory/reconciler/payer_test.go` | create | Waits and the stop's revocations by payer |
| `internal/factory/reconciler/reconciler_test.go` | modify | The fake records bound payers; `fakeRuns` keeps `ChargedTo` |
| `internal/factory/api/server.go`, `internal/factory/intake/issues.go` | modify | Refusals bind their principal |
| `internal/factory/api/payer_test.go` | create | The API's refusals, scraped |
| `internal/factory/intake/{issues,person}_test.go` | modify | The labeller on the intake's refusal |
| `internal/factory/killswitch/sweep.go`, `sweep_test.go` | modify | The sweep returns the runs it revoked |
| `internal/app/factory.go` | modify | Wiring: `OnCharge`, `OnCharges`, `ChargePeople`, the sweep's payers |
| `docs/operations.md`, `AGENTS.md` | modify | The series and the label rule |

**ai-native-ref (PR 2)**

| File | Change | Responsibility |
|---|---|---|
| `observability/base/agent-platform/grafana-dashboard-agent-identity.yaml` | create | The page |
| `observability/base/agent-platform/kustomization.yaml` | modify | Lists it |
| `observability/base/agent-platform/grafana-dashboard-agent-{fleet,factory}.yaml` | modify | Links to it |
| `scripts/ci/tests/test-agent-identity-dashboard.py` | create | Offline proof 6 |
| `scripts/ci/tests/test-agent-identity-queries.sh` | create | Offline proof 7 |
| `website/content/docs/platform/ai-platform/{observability,routing,status}.md` | modify | The page, its series, the status row's wording |

---

## PR 1: agent-platform

### Task 1: One payer and one kind per run

**Files:**
- Create: `internal/factory/runs/payer.go`
- Test: `internal/factory/runs/payer_test.go`

**Interfaces:**
- Consumes: `runs.Run` (`ID`, `TaskID`, `RoomRef`, `Principal`, `ChargedTo`), `runs.PrincipalFactory`,
  `(*v1alpha1.Task).Payer() string`.
- Produces (Tasks 2–5): `const KindTask = "task"`, `const KindRoom = "room"`,
  `func (r Run) Payer() string`, `func (r Run) Kind() string`,
  `func TaskPayer(t *v1alpha1.Task, perPerson bool) string`.

- [ ] **Step 1: Write the failing test**

`internal/factory/runs/payer_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"testing"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
)

// Spend per identity SI1: one payer per run, the person it is charged to beside the fleet, else its
// own principal; and one kind, task or room.
func TestARunHasOnePayerAndOneKind(t *testing.T) {
	for name, c := range map[string]struct {
		run         Run
		payer, kind string
	}{
		"a labelled task's run":       {Run{TaskID: "3buqdlot", Principal: PrincipalFactory, ChargedTo: "github:alice"}, "github:alice", KindTask},
		"a schedule's run":            {Run{TaskID: "4buqdlot", Principal: PrincipalFactory}, PrincipalFactory, KindTask},
		"a linked human's API run":    {Run{Principal: "human:291", ChargedTo: "github:bob"}, "github:bob", KindRoom},
		"an unlinked human's API run": {Run{Principal: "human:292", ChargedTo: "human:292"}, "human:292", KindRoom},
		"per-person caps off":         {Run{Principal: "human:293", RoomRef: "5buqdlot"}, "human:293", KindRoom},
	} {
		t.Run(name, func(t *testing.T) {
			if got := c.run.Payer(); got != c.payer {
				t.Errorf("payer %q, want %q", got, c.payer)
			}
			if got := c.run.Kind(); got != c.kind {
				t.Errorf("kind %q, want %q", got, c.kind)
			}
		})
	}
}

// SI1: a task's payer is the key its next run is charged to beside the fleet: the retrier, else the
// labeller, while per-person caps are on; the fleet for a task no person started, or with them off.
func TestATaskPaysAsItsNextRunWouldBeCharged(t *testing.T) {
	labelled := &v1alpha1.Task{Spec: v1alpha1.TaskSpec{Source: v1alpha1.Source{RequestedBy: "github:Alice"}}}
	retried := labelled.DeepCopy()
	retried.Status.RetriedBy = "github:bob"
	scheduled := &v1alpha1.Task{Spec: v1alpha1.TaskSpec{Source: v1alpha1.Source{RequestedBy: "system:scheduler"}}}
	for name, c := range map[string]struct {
		task *v1alpha1.Task
		on   bool
		want string
	}{
		"labelled":           {labelled, true, "github:alice"},
		"retried":            {retried, true, "github:bob"},
		"scheduled":          {scheduled, true, PrincipalFactory},
		"labelled, caps off": {labelled, false, PrincipalFactory},
	} {
		t.Run(name, func(t *testing.T) {
			if got := TaskPayer(c.task, c.on); got != c.want {
				t.Fatalf("%q, want %q", got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -count=1 ./internal/factory/runs/`
Expected: FAIL, `c.run.Payer undefined (type Run has no field or method Payer)` and `undefined: TaskPayer`.

- [ ] **Step 3: Write the implementation**

`internal/factory/runs/payer.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"cmp"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
)

// The kinds of a charge (spend per identity SI2): a run of a Task, or one started through
// POST /v1/runs, a room's.
const (
	KindTask = "task"
	KindRoom = "room"
)

// Payer is the one ledger key the run's charge is attributed to (spend per identity SI1): the
// person it is charged to beside the fleet, else its own principal. It is the last key the meter
// settles the run to, so the payers' charges partition the ledger's.
func (r Run) Payer() string { return cmp.Or(r.ChargedTo, r.Principal) }

// Kind is KindTask for a run of a Task, KindRoom for any other.
func (r Run) Kind() string {
	if r.TaskID != "" {
		return KindTask
	}
	return KindRoom
}

// TaskPayer is the key the task's next run is charged to beside the fleet while per-person caps
// are on, else the fleet's own (SI1): the payer of what the task costs and yields.
func TaskPayer(t *v1alpha1.Task, perPerson bool) string {
	if perPerson {
		if p := t.Payer(); p != "" {
			return p
		}
	}
	return PrincipalFactory
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test -race -count=1 ./internal/factory/runs/ && task lint`
Expected: `ok`, then `0 issues.`

- [ ] **Step 5: Commit**

```bash
git add internal/factory/runs/payer.go internal/factory/runs/payer_test.go
git commit -m "feat(runs): one payer and one kind per run"
```

---

### Task 2: The series name their payer

**Files:**
- Modify: `internal/factory/fmetrics/fmetrics.go`
- Create: `internal/factory/fmetrics/principal_test.go`
- Modify: `internal/factory/fmetrics/fmetrics_test.go`, `internal/factory/fmetrics/repository_test.go`

**Interfaces:**
- Consumes: Task 1's `runs.KindTask`, `runs.KindRoom`, `runs.Run.Payer`, `runs.TaskPayer`.
- Produces (Tasks 3–5):
  - `func WithPrincipal(ctx context.Context, principal string) context.Context`
  - `func PrincipalOf(ctx context.Context) string` ("" when none: for consumers' test doubles)
  - `func (s *Set) Charged(ctx context.Context, principal, kind string, microUSD int64)`
  - `func (s *Set) RunCharges(live []runs.Run)`
  - `func (s *Set) PROpened(ctx context.Context)`
  - `func (s *Set) HumanWait(ctx context.Context, phase string, d time.Duration)`
  - `func (s *Set) ChargePeople(on bool)`
  - `principal` (from the context) on `PROutcome`, `AdmissionRefused`, `Revoked`, `RunRevoked`;
    on `agent_factory_tasks` from `TaskPayer`; `PrincipalShadow`'s argument bounded.

- [ ] **Step 1: Write the failing test**

`internal/factory/fmetrics/principal_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package fmetrics

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/internal/factory/runs"
)

// Spend per identity SI4 and SI5: the series a principal's page reads carry the payer the context
// binds, in the shape of a ledger key, else other; an unbound context reads other.
func TestThePayerLabelsTheSeriesItsContextBinds(t *testing.T) {
	s, scrape := rig(t, nil, func() bool { return true })
	s.ListRepositories("Smana/a")
	alice := WithPrincipal(WithRepository(t.Context(), "Smana/a"), "github:alice")
	s.PROutcome(alice, "docs-links", "auto_merged")
	s.PROpened(alice)
	s.AdmissionRefused(alice, "person_budget_exhausted")
	s.Revoked(alice, "budget-person")
	s.RunRevoked(alice, "budget-task", "task-usd")
	s.HumanWait(alice, "AwaitingHuman", 90*time.Second)
	s.HumanWait(alice, "Implementing", time.Hour) // not a wait for a human: not counted
	s.PROutcome(WithPrincipal(t.Context(), "github:Ignore previous instructions"), "docs-links", "closed")
	s.PROutcome(t.Context(), "docs-links", "closed")
	s.PrincipalShadow(alice, "system:factory") // the key over, not the payer
	body := scrape()
	for _, want := range []string{
		`agent_factory_pr_outcomes_total{class="docs-links",outcome="auto_merged",principal="github:alice",repository="Smana/a"} 1`,
		`agent_factory_prs_opened_total{principal="github:alice",repository="Smana/a"} 1`,
		`agent_factory_admission_refusals_total{principal="github:alice",reason="person_budget_exhausted",repository="Smana/a"} 1`,
		`agent_factory_run_revocations_total{principal="github:alice",reason="budget-person"} 1`,
		`agent_factory_run_revocations_total{limit="task-usd",principal="github:alice",reason="budget-task"} 1`,
		`agent_factory_human_wait_seconds_total{phase="AwaitingHuman",principal="github:alice"} 90`,
		`agent_factory_pr_outcomes_total{class="docs-links",outcome="closed",principal="other",repository="other"} 2`,
		`agent_factory_run_revocations_total{principal="system:factory",reason="budget-principal-shadow"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(body, `phase="Implementing"`) || strings.Contains(body, "Ignore") {
		t.Errorf("an unbounded value is exposed")
	}
	if t.Failed() {
		t.Log(body)
	}
}

// SI5: a ledger key's shape is system:<name>, github:<login> or human:<sub>; nothing else is one.
func TestThePrincipalTakesALedgerKeysShape(t *testing.T) {
	for in, want := range map[string]string{
		"system:factory":                    "system:factory",
		"system:slack-app":                  "system:slack-app",
		"github:alice":                      "github:alice",
		"github:a-b-c1":                     "github:a-b-c1",
		"human:291874650112344":             "human:291874650112344",
		"":                                  "other",
		"github:Alice":                      "other", // Person lowercases: an upper case is no key
		"github:" + strings.Repeat("a", 40): "other", // a GitHub login is at most 39
		"agent:3buqdlot":                    "other",
		"system:factory\n":                  "other",
		`github:alice",principal="x`:        "other",
	} {
		if got := boundPrincipal(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// SI2: the charged counter adds a run's settled micro-USD in dollars under its payer and kind.
func TestTheChargedCounterIsInDollars(t *testing.T) {
	s, scrape := rig(t, nil, func() bool { return true })
	s.Charged(t.Context(), "github:alice", runs.KindTask, 1_444_000)
	s.Charged(t.Context(), "github:alice", runs.KindTask, 56_000)
	s.Charged(t.Context(), "human:292", runs.KindRoom, 1_700)
	s.Charged(t.Context(), "github:alice", "a kind", 1)
	body := scrape()
	for _, want := range []string{
		`agent_factory_charged_usd_total{kind="task",principal="github:alice"} 1.5`,
		`agent_factory_charged_usd_total{kind="room",principal="human:292"} 0.0017`,
		`agent_factory_charged_usd_total{kind="other",principal="github:alice"} 1e-06`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s\n%s", want, body)
		}
	}
}

// SI3: one series per run the meter hands over, naming its payer, its task or its room; a run
// whose id is not a C2 id is not exposed, and the next hand-over replaces the last.
func TestTheChargeInfoNamesEachRunsPayer(t *testing.T) {
	s, scrape := rig(t, nil, func() bool { return true })
	s.RunCharges([]runs.Run{
		{ID: "aaaaaaaa", TaskID: "3buqdlot", RoomRef: "3buqdlot", Principal: runs.PrincipalFactory, ChargedTo: "github:alice"},
		{ID: "bbbbbbbb", RoomRef: "5buqdlot", Principal: "human:291", ChargedTo: "github:bob"},
		{ID: "cccccccc", Principal: "human:292"},
		{ID: "not a run", Principal: "human:292"},
	})
	body := scrape()
	for _, want := range []string{
		`agent_factory_run_charge_info{principal="github:alice",room="3buqdlot",run="aaaaaaaa",task="3buqdlot"} 1`,
		`agent_factory_run_charge_info{principal="github:bob",room="5buqdlot",run="bbbbbbbb"} 1`,
		`agent_factory_run_charge_info{principal="human:292",run="cccccccc"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s\n%s", want, body)
		}
	}
	if strings.Contains(body, "not a run") {
		t.Errorf("a run id outside C2 is exposed")
	}
	s.RunCharges(nil)
	if body := scrape(); strings.Contains(body, "agent_factory_run_charge_info{") {
		t.Errorf("an ended run's series outlives the hand-over that dropped it\n%s", body)
	}
}

// SI4: the tasks gauge names each task's payer: its labeller or retrier while per-person caps are
// on, the fleet for a task no person started or with them off.
func TestTheTasksGaugeNamesThePayer(t *testing.T) {
	labelled, scheduled := tk("3buqdlot", "AwaitingHuman"), tk("4buqdlot", "AwaitingHuman")
	labelled.Spec.Source.RequestedBy, scheduled.Spec.Source.RequestedBy = "github:Alice", "system:scheduler"
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(labelled, scheduled).Build()
	s, scrape := rig(t, c, func() bool { return true })
	gauge := func(who string, n int) string {
		return fmt.Sprintf(`agent_factory_tasks{phase="AwaitingHuman",predicted_class="review",principal="%s",repository="other",source="issue",tier="standard"} %d`, who, n)
	}
	if body := scrape(); !strings.Contains(body, gauge("system:factory", 2)+"\n") {
		t.Errorf("per-person caps off: both tasks are the fleet's\n%s", body)
	}
	s.ChargePeople(true)
	body := scrape()
	for _, want := range []string{gauge("github:alice", 1), gauge("system:factory", 1)} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s\n%s", want, body)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -count=1 ./internal/factory/fmetrics/`
Expected: FAIL at build: `undefined: boundPrincipal`, `s.Charged undefined`, `undefined: WithPrincipal`.

- [ ] **Step 3: Write the implementation**

In `internal/factory/fmetrics/fmetrics.go`, make these replacements, each of a text that occurs once.

Import `runs`:

```go
	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/killswitch"
	"github.com/Smana/agent-platform/internal/factory/runs"
)
```

After `var modelRE = …`, add the shape and the two vocabularies:

```go
// principalRE is a ledger key's shape (spend per identity SI5): a sponsored or system principal, a
// person's lowercased GitHub login (v1alpha1.Person), or an API caller's subject with no GitHub
// link. Its values are the people who may start runs and the principals in the config.
var principalRE = regexp.MustCompile(`^(system:[a-z0-9][a-z0-9-]{0,62}|github:[a-z0-9][a-z0-9-]{0,38}|human:[A-Za-z0-9][A-Za-z0-9_-]{0,63})$`)

func charges() []string     { return []string{runs.KindTask, runs.KindRoom} }
func humanPhases() []string { return []string{v1alpha1.PhaseAwaitingHuman, v1alpha1.PhaseEscalated} }
```

In `type Set struct`, replace from `	modelCost       metric.Float64Counter` to the struct's closing brace with:

```go
	modelCost       metric.Float64Counter
	charged         metric.Float64Counter
	prsOpened       metric.Int64Counter
	humanWait       metric.Float64Counter

	spendMu sync.Mutex
	spend   map[string]map[string]float64 // the meter's last live spend, run then model
	live    []runs.Run                    // the meter's last charge info: the runs live or ended within endSettle

	// repos is the config's repository list, the repository label's only values (v2 D10).
	mu    sync.RWMutex
	repos []string
	// perPerson: a task's payer is its person (budgets.perPerson.enabled), else the fleet.
	perPerson bool
}

// ChargePeople sets whether tasks pay as their person, per-person caps being on: the tasks
// gauge's principal (spend per identity SI1).
func (s *Set) ChargePeople(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.perPerson = on
}

func (s *Set) taskPayer(t *v1alpha1.Task) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return runs.TaskPayer(t, s.perPerson)
}

type principalKey struct{}

// WithPrincipal names the payer the series recorded under ctx belong to (spend per identity SI4):
// the reconciler binds its task's, the API its requester's, the meter and the sweep a run's.
// Recorded without one, a series reads other.
func WithPrincipal(ctx context.Context, principal string) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}

// PrincipalOf is the payer ctx binds, "" when none: for a consumer's test double.
func PrincipalOf(ctx context.Context) string {
	p, _ := ctx.Value(principalKey{}).(string)
	return p
}

func boundPrincipal(p string) string {
	if !principalRE.MatchString(p) {
		return other
	}
	return p
}

func principalOf(ctx context.Context) attribute.KeyValue {
	return attribute.String("principal", boundPrincipal(PrincipalOf(ctx)))
}
```

In `New`, replace

```go
	check(s.registerSpend(meter))
	if tasks != nil {
		check(registerCollected(meter, tasks, ns, leader, s.repository))
	}
```

with

```go
	s.charged, err = meter.Float64Counter("agent_factory_charged_usd_total",
		metric.WithDescription("USD the day ledgers charged, by the run's payer and kind, added when the write that settles it succeeds (spend per identity SI2)."))
	check(err)
	s.prsOpened, err = meter.Int64Counter("agent_factory_prs_opened_total",
		metric.WithDescription("Task pull requests opened, by the task's payer."))
	check(err)
	s.humanWait, err = meter.Float64Counter("agent_factory_human_wait_seconds_total",
		metric.WithDescription("Seconds tasks waited for a human, by the task's payer, added when a task leaves AwaitingHuman or Escalated."))
	check(err)
	check(s.registerSpend(meter))
	check(s.registerCharges(meter))
	if tasks != nil {
		check(registerCollected(meter, tasks, ns, leader, s.repository, s.taskPayer))
	}
```

Replace `registerCollected`'s signature line with

```go
func registerCollected(meter metric.Meter, r client.Reader, ns string, leader func() bool, repo func(string) string,
	payer func(*v1alpha1.Task) string) error {
```

and its counting with

```go
			counts := map[[6]string]int64{}
			for i := range l.Items {
				t := &l.Items[i]
				counts[[6]string{t.Status.Phase, t.Spec.Source.Kind, t.Spec.PredictedClass, t.Spec.Budget.Tier, repo(t.Spec.Repository),
					boundPrincipal(payer(t))}]++
			}
			for k, n := range counts {
				o.ObserveInt64(tasks, n, metric.WithAttributes(attribute.String("phase", k[0]), attribute.String("source", k[1]),
					attribute.String("predicted_class", k[2]), attribute.String("tier", k[3]), attribute.String("repository", k[4]),
					attribute.String("principal", k[5])))
			}
```

In `AdmissionRefused`, the `Add` becomes

```go
	s.refusals.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason), s.repoOf(ctx), principalOf(ctx)))
```

Replace `PROutcome` with `PROpened`, `PROutcome`, `HumanWait` and `Charged`:

```go
// PROpened counts a task's pull request opening, by its repository and payer: where TimeToPR
// records, without the histogram's buckets times every principal (spend per identity SI4).
func (s *Set) PROpened(ctx context.Context) {
	s.prsOpened.Add(ctx, 1, metric.WithAttributes(s.repoOf(ctx), principalOf(ctx)))
}

// PROutcome counts a task PR's outcome: auto_merged, human_merged, closed or reverted.
func (s *Set) PROutcome(ctx context.Context, class, outcome string) {
	s.prOutcomes.Add(ctx, 1, metric.WithAttributes(attribute.String("class", class),
		attribute.String("outcome", oneOf(outcome, outcomes())), s.repoOf(ctx), principalOf(ctx)))
}

// HumanWait adds the time a task waited in AwaitingHuman or Escalated, as it leaves the phase, to
// its payer's waits (spend per identity SI8). Any other phase is no wait for a human.
func (s *Set) HumanWait(ctx context.Context, phase string, d time.Duration) {
	if !slices.Contains(humanPhases(), phase) || d <= 0 {
		return
	}
	s.humanWait.Add(ctx, d.Seconds(), metric.WithAttributes(attribute.String("phase", phase), principalOf(ctx)))
}

// Charged adds a run's settled micro-USD, in dollars, to its payer's charges by kind: the run
// meter's OnCharge, once the write that settles it succeeded (spend per identity SI2).
func (s *Set) Charged(ctx context.Context, principal, kind string, microUSD int64) {
	s.charged.Add(ctx, float64(microUSD)/1e6, metric.WithAttributes(attribute.String("principal", boundPrincipal(principal)),
		attribute.String("kind", oneOf(kind, charges()))))
}
```

The three revocation methods' `Add` calls become:

```go
// Revoked:
	s.revocations.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", oneOf(reason, revocations())), principalOf(ctx)))
// PrincipalShadow:
	s.revocations.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "budget-principal-shadow"),
		attribute.String("principal", boundPrincipal(principal))))
// RunRevoked:
	s.revocations.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", oneOf(reason, revocations())),
		attribute.String("limit", oneOf(limit, revocationLimits())), principalOf(ctx)))
```

Before `func boundModel`, add the charge info:

```go
// RunCharges replaces the charge info's runs: the run meter's OnCharges, each tick that listed them.
func (s *Set) RunCharges(live []runs.Run) {
	s.spendMu.Lock()
	defer s.spendMu.Unlock()
	s.live = live
}

// registerCharges exposes agent_factory_run_charge_info{run,principal,task,room} = 1 from the
// meter's last hand-over (spend per identity SI3): the leader's live runs and those ended within
// endSettle, so a panel joins a run's gateway series to its payer. task is absent for a room's
// run, room for a run without one. Every id is a C2 id, or the run is left out.
func (s *Set) registerCharges(meter metric.Meter) error {
	g, err := meter.Int64ObservableGauge("agent_factory_run_charge_info",
		metric.WithDescription("1 for each run the meter charges now or charged within its settle window, naming its payer, its task and its room."))
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s.spendMu.Lock()
		defer s.spendMu.Unlock()
		for _, r := range s.live {
			if !runRE.MatchString(r.ID) {
				continue
			}
			at := []attribute.KeyValue{attribute.String("run", r.ID), attribute.String("principal", boundPrincipal(r.Payer()))}
			if runRE.MatchString(r.TaskID) {
				at = append(at, attribute.String("task", r.TaskID))
			}
			if runRE.MatchString(r.RoomRef) {
				at = append(at, attribute.String("room", r.RoomRef))
			}
			o.ObserveInt64(g, 1, metric.WithAttributes(at...))
		}
		return nil
	}, g)
	return err
}
```

The existing tests pin exact label sets, which now hold `principal` (the exporter sorts labels).
Update these strings, each once:

| File | Old | New |
|---|---|---|
| `fmetrics_test.go` | `` `agent_factory_tasks{phase="Implementing",predicted_class="review",repository="other",source="issue",tier="standard"} 2` `` | `` `agent_factory_tasks{phase="Implementing",predicted_class="review",principal="system:factory",repository="other",source="issue",tier="standard"} 2` `` |
| `fmetrics_test.go` | `` `agent_factory_tasks{phase="Done",predicted_class="review",repository="other",source="issue",tier="standard"} 1` `` | `` `agent_factory_tasks{phase="Done",predicted_class="review",principal="system:factory",repository="other",source="issue",tier="standard"} 1` `` |
| `fmetrics_test.go` | `` `agent_factory_pr_outcomes_total{class="docs-links",outcome="human_merged",repository="other"} 1` `` | `` `agent_factory_pr_outcomes_total{class="docs-links",outcome="human_merged",principal="other",repository="other"} 1` `` |
| `fmetrics_test.go` | `` `agent_factory_run_revocations_total{reason="budget-run"} 1` `` | `` `agent_factory_run_revocations_total{principal="other",reason="budget-run"} 1` `` |
| `fmetrics_test.go` | `` `agent_factory_run_revocations_total{limit="run-usd",reason="budget-run"} 1` `` | `` `agent_factory_run_revocations_total{limit="run-usd",principal="other",reason="budget-run"} 1` `` |
| `fmetrics_test.go` | `` `agent_factory_run_revocations_total{limit="task-usd",reason="budget-task"} 1` `` | `` `agent_factory_run_revocations_total{limit="task-usd",principal="other",reason="budget-task"} 1` `` |
| `fmetrics_test.go` | `` `agent_factory_run_revocations_total{reason="other"} 1` `` | `` `agent_factory_run_revocations_total{principal="other",reason="other"} 1` `` |
| `fmetrics_test.go` | `` `agent_factory_pr_outcomes_total{class="docs-links",outcome="other",repository="other"} 1` `` | `` `agent_factory_pr_outcomes_total{class="docs-links",outcome="other",principal="other",repository="other"} 1` `` |
| `fmetrics_test.go` | `` `agent_factory_run_revocations_total{limit="other",reason="budget-run"} 1` `` | `` `agent_factory_run_revocations_total{limit="other",principal="other",reason="budget-run"} 1` `` |
| `repository_test.go` | `` `agent_factory_pr_outcomes_total{class="docs-links",outcome="closed",repository="Smana/b"` `` | `` `agent_factory_pr_outcomes_total{class="docs-links",outcome="closed",principal="other",repository="Smana/b"` `` |
| `repository_test.go` | `` `agent_factory_pr_outcomes_total{class="docs-links",outcome="closed",repository="other"` `` | `` `agent_factory_pr_outcomes_total{class="docs-links",outcome="closed",principal="other",repository="other"` `` |
| `repository_test.go` | `` `predicted_class="review",repository="Smana/a"` `` | `` `predicted_class="review",principal="system:factory",repository="Smana/a"` `` |
| `repository_test.go` | `` `predicted_class="review",repository="other"` `` | `` `predicted_class="review",principal="system:factory",repository="other"` `` |
| `repository_test.go` | `` `agent_factory_admission_refusals_total{reason="repository_not_owned",repository="Smana/b"} 1` `` | `` `agent_factory_admission_refusals_total{principal="other",reason="repository_not_owned",repository="Smana/b"} 1` `` |
| `repository_test.go` | `` `agent_factory_admission_refusals_total{reason="repository_permission",repository="Smana/a"} 1` `` | `` `agent_factory_admission_refusals_total{principal="other",reason="repository_permission",repository="Smana/a"} 1` `` |

- [ ] **Step 4: Run it to verify it passes**

Run: `go test -race -count=1 ./internal/factory/fmetrics/ && go build ./... && task lint`
Expected: `ok`, a clean build (the reconciler's `metrics` interface does not need the new methods
yet), `0 issues.`

- [ ] **Step 5: Commit**

```bash
git add internal/factory/fmetrics/
git commit -m "feat(fmetrics): the payer on the factory's series, the charged counter and the charge info"
```

---

### Task 3: The meter counts each settled charge under its payer, and names the live runs' payers

**Files:**
- Modify: `internal/factory/meter/meter.go`
- Test: `internal/factory/meter/payer_test.go`

**Interfaces:**
- Consumes: Task 1's `runs.Run.Payer`, `runs.Run.Kind`; Task 2's `fmetrics.WithPrincipal`,
  `fmetrics.PrincipalOf` (tests). The test helpers already in the package: `store`, `source`,
  `typed`, `broken`, `byModel`, `glmRun`, `glmUsage`, `usdLedgerMeter`, `cmScheme`, `flaky`,
  `spentOf`, `settledOf`.
- Produces (Task 5 wires them): `Meter.OnCharge func(ctx context.Context, principal, kind string, microUSD int64)`,
  `Meter.OnCharges func(live []runs.Run)`; `OnRevoke`'s context binds the run's payer.

- [ ] **Step 1: Write the failing test**

`internal/factory/meter/payer_test.go` (offline proofs 1–4; the R10 cases rest on PR 2's lift, `871ced8`):

```go
// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/fmetrics"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// tally is what OnCharge was told, in micro-USD by payer and kind.
type tally map[[2]string]int64

func (c tally) add(_ context.Context, principal, kind string, microUSD int64) {
	c[[2]string{principal, kind}] += microUSD
}

func (c tally) by(principal string) (n int64) {
	for k, v := range c {
		if k[0] == principal {
			n += v
		}
	}
	return n
}

func (c tally) total() (n int64) {
	for _, v := range c {
		n += v
	}
	return n
}

func sum(m map[string]int64) (n int64) {
	for _, v := range m {
		n += v
	}
	return n
}

// Spend per identity, offline proofs 1 and 2: runs paid by a labeller, by the fleet (a schedule's
// and RunLore's tasks), by a linked and an unlinked API caller, priced and not, ticked through a
// day. The payers' charges sum to what the ledger settled run by run, each run its total as the
// meter counts it; per person key they equal the day's spent, and over every payer the fleet's.
func TestThePayersChargesPartitionTheLedger(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	run := func(id, task, room, principal, chargedTo string) runs.Run {
		return runs.Run{ID: id, TaskID: task, RoomRef: room, Phase: "Running", Model: "agent-default",
			Principal: principal, ChargedTo: chargedTo, Created: now}
	}
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{
		run("aaaaaaaa", "3buqdlot", "3buqdlot", runs.PrincipalFactory, "github:alice"), // a labelled task: GLM, priced
		run("bbbbbbbb", "4buqdlot", "4buqdlot", runs.PrincipalFactory, ""),             // a schedule's task: priced
		run("cccccccc", "5buqdlot", "5buqdlot", runs.PrincipalFactory, ""),             // RunLore's task: untyped
		run("dddddddd", "", "6buqdlot", "human:291", "github:bob"),                     // a linked human's API run: Opus
		run("eeeeeeee", "", "", "human:292", "human:292"),                              // an unlinked one: a model with no price
	}}
	readings := []typed{
		{
			"aaaaaaaa": byModel(map[string]ModelUsage{"glm-5.3": {Input: 1_000_000, Output: 10_000}}),
			"bbbbbbbb": byModel(map[string]ModelUsage{"glm-5.3": {Input: 200_000, Output: 10_000}}),
			"cccccccc": {Tokens: 100_000},
			"dddddddd": byModel(map[string]ModelUsage{"claude-opus-5-5": {Input: 100_000, CacheRead: 90_000, Output: 1_000}}),
			"eeeeeeee": byModel(map[string]ModelUsage{"claude-fable-5-1": {Input: 9_000, Output: 1_000}}),
		},
		{
			"aaaaaaaa": byModel(map[string]ModelUsage{"glm-5.3": {Input: 1_500_000, Output: 20_000}}),
			"bbbbbbbb": byModel(map[string]ModelUsage{"glm-5.3": {Input: 300_000, Output: 10_000}}),
			"cccccccc": {Tokens: 150_000},
			"dddddddd": byModel(map[string]ModelUsage{"claude-opus-5-5": {Input: 200_000, CacheRead: 180_000, Output: 2_000}}),
			"eeeeeeee": byModel(map[string]ModelUsage{"claude-fable-5-1": {Input: 18_000, Output: 2_000}}),
		},
	}
	c := fake.NewClientBuilder().WithScheme(cmScheme()).Build()
	src := typed{}
	got := tally{}
	m := usdLedgerMeter(st, src, c, &now)
	m.OnCharge = got.add
	for i := range 3 {
		maps.Copy(src, readings[min(i, 1)])
		if i == 2 {
			st.runs[1].Phase, st.runs[1].Finished = "Succeeded", now // the schedule's run ended: nothing new
		}
		if err := m.Tick(t.Context()); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		now = now.Add(time.Hour)
	}
	// GLM: 1.5 M x 1.40 + 20 k x 4.40, and 300 k x 1.40 + 10 k x 4.40. Untyped: 150 k x 1.7. Opus:
	// 20 k fresh x 4 + 180 k read x 0.20 + 2 k x 20. Unpriced: 20 k tokens x 1.7.
	run1 := map[string]int64{"aaaaaaaa": 2_188_000, "bbbbbbbb": 464_000, "cccccccc": 255_000, "dddddddd": 156_000, "eeeeeeee": 34_000}
	settled := settledOf(t, c, "20261022")
	if !maps.Equal(settled, run1) {
		t.Fatalf("settled %v, want each run's total %v", settled, run1)
	}
	if got.total() != sum(settled) {
		t.Fatalf("the payers' charges sum to %d, the ledger settled %d", got.total(), sum(settled))
	}
	want := tally{{"github:alice", runs.KindTask}: 2_188_000, {runs.PrincipalFactory, runs.KindTask}: 719_000,
		{"github:bob", runs.KindRoom}: 156_000, {"human:292", runs.KindRoom}: 34_000}
	if !maps.Equal(got, want) {
		t.Fatalf("charges %v, want %v", got, want)
	}
	spent, _ := spentOf(t, c, "20261022")
	for _, person := range []string{"github:alice", "github:bob", "human:292"} {
		if got.by(person) != spent[person] {
			t.Errorf("%s: charged %d, the day spent %d", person, got.by(person), spent[person])
		}
	}
	if got.total() != spent[runs.PrincipalFactory] {
		t.Errorf("every payer's charges %d, the fleet's day %d", got.total(), spent[runs.PrincipalFactory])
	}
	if cost, _ := m.Costs.Get("aaaaaaaa"); got.by("github:alice") != cost.MicroUSD {
		t.Errorf("alice's run cost %d, charged to her %d: the task record's costMicroUSD lands under its chargedTo",
			cost.MicroUSD, got.by("github:alice"))
	}
}

// Offline proof 3: the counter follows the ledger through a leader change, a write in doubt that
// landed, a failed write, a run across midnight and the switch day's ended run.
func TestTheChargesFollowTheLedgerThroughItsFailures(t *testing.T) {
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	t.Run("a leader change: two counters together", func(t *testing.T) {
		now := at
		c := fake.NewClientBuilder().WithScheme(cmScheme()).Build()
		first, next := tally{}, tally{}
		m := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}},
			typed{"aaaaaaaa": glmUsage(1_000_000)}, c, &now)
		m.OnCharge = first.add
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		r := glmRun("aaaaaaaa", now)
		r.Tokens = 1_000_000
		n := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{r}},
			typed{"aaaaaaaa": glmUsage(1_500_000)}, c, &now)
		n.OnCharge = next.add
		if err := n.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		if s := settledOf(t, c, "20261022"); first.total()+next.total() != s["aaaaaaaa"] || first.total() != 1_400_000 {
			t.Fatalf("charged %d then %d, settled %d", first.total(), next.total(), s["aaaaaaaa"])
		}
	})
	for what, landed := range map[string]bool{"a failed write": false, "a write in doubt that landed": true} {
		t.Run(what, func(t *testing.T) {
			now := at
			fail := true
			c := flaky(&fail, &landed)
			got := tally{}
			src := typed{"aaaaaaaa": glmUsage(1_000_000)}
			m := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}}, src, c, &now)
			m.OnCharge = got.add
			if err := m.Tick(t.Context()); err == nil {
				t.Fatal("the failed write is reported")
			}
			if got.total() != 0 {
				t.Fatalf("a write that reported an error counted %d", got.total())
			}
			fail, src["aaaaaaaa"] = false, glmUsage(1_100_000)
			if err := m.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if s := settledOf(t, c, "20261022"); got.total() != s["aaaaaaaa"] || got.total() != 1_540_000 {
				t.Fatalf("charged %d, settled %d", got.total(), s["aaaaaaaa"])
			}
		})
	}
	t.Run("a run across midnight", func(t *testing.T) {
		now := time.Date(2026, 10, 22, 23, 59, 30, 0, time.UTC)
		c := fake.NewClientBuilder().WithScheme(cmScheme()).Build()
		got := tally{}
		src := typed{"aaaaaaaa": glmUsage(1_000_000)}
		m := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}}, src, c, &now)
		m.OnCharge = got.add
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		now, src["aaaaaaaa"] = now.Add(time.Minute), glmUsage(1_300_000)
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		d1, d2 := settledOf(t, c, "20261022"), settledOf(t, c, "20261023")
		if got.total() != d1["aaaaaaaa"]+d2["aaaaaaaa"] || d2["aaaaaaaa"] != 420_000 {
			t.Fatalf("charged %d, the days settled %v and %v", got.total(), d1, d2)
		}
	})
	// R10: the switch day's ended run is charged nothing, and its late reading only that reading,
	// though the write that carries it also records the lift in the run's settled entry.
	for what, doubt := range map[string]bool{"written": false, "in doubt, landed": true} {
		t.Run("the switch day's ended run, then its late reading, "+what, func(t *testing.T) {
			now := at
			fail, landed := false, true
			c := flaky(&fail, &landed)
			got := tally{}
			r := glmRun("aaaaaaaa", now.Add(-time.Hour))
			r.Phase, r.Tokens = "Succeeded", 1_000_000 // the token ledgers charged it
			src := typed{"aaaaaaaa": glmUsage(1_000_000)}
			m := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{r}}, src, c, &now)
			m.OnCharge = got.add
			if err := m.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got.total() != 0 {
				t.Fatalf("nothing charged, nothing counted; counted %d", got.total())
			}
			src["aaaaaaaa"], fail = glmUsage(1_050_000), doubt // a late reading: 70 k more
			if err := m.Tick(t.Context()); (err != nil) != doubt {
				t.Fatalf("the late reading's tick: %v", err)
			}
			fail = false
			if doubt {
				if err := m.Tick(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			spent, _ := spentOf(t, c, "20261022")
			if got.total() != 70_000 || spent[runs.PrincipalFactory] != 70_000 || settledOf(t, c, "20261022")["aaaaaaaa"] != 1_470_000 {
				t.Fatalf("counted %d, spent %v, settled %v: the counter follows spent, not the lift",
					got.total(), spent, settledOf(t, c, "20261022"))
			}
		})
	}
}

// Offline proof 4: the charge info names every live run, and an ended one until endSettle after
// its finishedAt; never one a new leader first sees ended longer ago, nor one with no finishedAt.
// It follows the listed runs, so a failed usage read does not freeze it.
func TestTheChargeInfoNamesLiveRunsAndThoseJustEnded(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	live := glmRun("aaaaaaaa", now)
	ended := glmRun("bbbbbbbb", now)
	ended.Phase, ended.Finished = "Succeeded", now.Add(-time.Minute)
	old := glmRun("cccccccc", now)
	old.Phase, old.Finished = "Failed", now.Add(-5*time.Minute)
	bare := glmRun("dddddddd", now)
	bare.Phase = "Succeeded"
	var named []string
	m := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{live, ended, old, bare}}, typed{},
		fake.NewClientBuilder().WithScheme(cmScheme()).Build(), &now)
	m.OnCharges = func(rs []runs.Run) {
		named = nil
		for _, r := range rs {
			named = append(named, r.ID)
		}
		slices.Sort(named)
	}
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(named, []string{"aaaaaaaa", "bbbbbbbb"}) {
		t.Fatalf("named %v, want the live run and the one ended a minute ago", named)
	}
	now = now.Add(90 * time.Second)
	m.Source = broken{}
	if err := m.Tick(t.Context()); err == nil {
		t.Fatal("the failed read is reported")
	}
	if !slices.Equal(named, []string{"aaaaaaaa"}) {
		t.Fatalf("named %v, want the live run alone: bbbbbbbb ended 2.5 minutes ago", named)
	}
}

// SI4: a revocation the meter writes is recorded under the run's payer.
func TestARevocationNamesTheRunsPayer(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{
		{ID: "aaaaaaaa", Phase: "Running", MaxTokens: 100, Principal: runs.PrincipalFactory, ChargedTo: "github:alice", Created: now}}}
	var who, why string
	m := &Meter{Runs: st, Source: source{"aaaaaaaa": 200}, Budgets: config.Budgets{}, Now: func() time.Time { return now },
		OnRevoke: func(ctx context.Context, reason, _ string) { who, why = fmetrics.PrincipalOf(ctx), reason }}
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if who != "github:alice" || why != ReasonBudgetRun {
		t.Fatalf("revoked %q under %q, want budget-run under github:alice", why, who)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -count=1 ./internal/factory/meter/`
Expected: FAIL at build: `m.OnCharge undefined (type *Meter has no field or method OnCharge)`.

- [ ] **Step 3: Write the implementation**

In `internal/factory/meter/meter.go`:

Import `fmetrics` (no cycle: fmetrics imports neither the meter nor `api`):

```go
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/fmetrics"
	"github.com/Smana/agent-platform/internal/factory/runs"
```

Replace the `OnRevoke` field and its comment with:

```go
	// OnRevoke is told of each revocation written, with its reason and the limit that fired, under
	// a context that binds the run's payer (fmetrics.WithPrincipal): fmetrics.Set.RunRevoked.
	OnRevoke func(ctx context.Context, reason, limit string)
	// OnCharge is told of each run's charge once the write that settles it succeeded, or once
	// resolve finds a write in doubt landed: its payer (runs.Run.Payer), its kind and the
	// micro-USD (spend per identity SI2). Its sum over payers is what the day's spent columns
	// gained: R10's lift reaches a run's settled entry, never spent, and is never counted.
	OnCharge func(ctx context.Context, principal, kind string, microUSD int64)
	// OnCharges receives, each tick that listed the runs, those the charge info names (SI3):
	// every run not ended, and each ended one until endSettle after its status.finishedAt.
	OnCharges func(live []runs.Run)
```

In `type account struct`, after the `lift` field, add:

```go
	payer, kind          string // whom and what its charges count for (runs.Run.Payer, Kind)
```

In `resolve`, replace

```go
		gain := max(0, entries[id]-a.was)
		a.settled += gain
		if gain > 0 {
			a.lift = 0
		}
		a.doubt, a.was = "", 0
		m.accounts[id] = a
```

with

```go
		gain := max(0, entries[id]-a.was)
		a.settled += gain
		charge := gain
		if gain > 0 { // the write landed whole: the run's charge, and its lift, which spent never counts
			charge = max(0, gain-a.lift)
			a.lift = 0
		}
		a.doubt, a.was = "", 0
		m.accounts[id] = a
		m.charge(ctx, a, charge)
```

In `Tick`, right after the runs are listed:

```go
	all, err := m.Runs.List(ctx)
	if err != nil {
		return fmt.Errorf("meter: list runs: %w", err)
	}
	if m.OnCharges != nil {
		m.OnCharges(m.chargeInfo(all))
	}
```

where a run's charge enters `inc`:

```go
			for _, k := range charged(r) {
				delta[k] += n
			}
			inc[r.ID] = n + m.accounts[r.ID].lift
			a := m.accounts[r.ID]
			a.payer, a.kind = r.Payer(), r.Kind()
			m.accounts[r.ID] = a
		}
```

in the loop that settles a successful write:

```go
		for id, n := range inc { // in the day's ledger: those runs are settled up
			a := m.accounts[id]
			charge := n - a.lift // the lift reaches the run's settled entry, never spent
			a.settled += n
			a.lift = 0
			m.accounts[id] = a
			m.charge(ctx, a, charge)
		}
```

and the revocation's callback:

```go
			if m.OnRevoke != nil {
				m.OnRevoke(fmetrics.WithPrincipal(ctx, r.Payer()), why, limit)
			}
```

Before `// charged are the ledger keys a run's tokens settle to`, add:

```go
// charge tells OnCharge of n micro-USD the day's spent column now holds for a run, under its payer.
func (m *Meter) charge(ctx context.Context, a account, n int64) {
	if n > 0 && m.OnCharge != nil {
		m.OnCharge(ctx, a.payer, a.kind, n)
	}
}

// chargeInfo is the runs the charge info names (SI3): each run not ended, and each ended one
// while its status.finishedAt is less than endSettle ago, so a panel joins the gateway's last
// readings of a short run to its payer. A run that ended without a finishedAt is not named: no
// meter, a new leader included, could tell how long ago.
func (m *Meter) chargeInfo(all []runs.Run) []runs.Run {
	now := m.now()
	var out []runs.Run
	for _, r := range all {
		if !runs.Terminal(r.Phase) || (!r.Finished.IsZero() && now.Sub(r.Finished) < endSettle) {
			out = append(out, r)
		}
	}
	return out
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test -race -count=1 ./internal/factory/meter/ && task lint`
Expected: `ok` (the new tests and every existing meter test, PR 2's lift tests included), `0 issues.`

- [ ] **Step 5: Prove the lift case bites**

Change `charge := n - a.lift` to `charge := n` and run
`go test -count=1 -run TestTheChargesFollowTheLedgerThroughItsFailures ./internal/factory/meter/`.
Expected: FAIL, `counted 1470000, spent map[github:alice:70000 system:factory:70000], settled map[aaaaaaaa:1470000]`.
Restore the line and run the package again: `ok`.

- [ ] **Step 6: Commit**

```bash
git add internal/factory/meter/meter.go internal/factory/meter/payer_test.go
git commit -m "feat(meter): count each settled charge under its payer, and name the live runs' payers"
```

---

### Task 4: A task's series name its payer, and its waits for a human count

**Files:**
- Modify: `internal/factory/reconciler/reconciler.go`, `internal/factory/reconciler/implement.go`
- Modify: `internal/factory/reconciler/reconciler_test.go` (the fakes and one assertion)
- Test: `internal/factory/reconciler/payer_test.go`

**Interfaces:**
- Consumes: Task 1's `runs.TaskPayer`, `runs.Run.Payer`; Task 2's `fmetrics.WithPrincipal`,
  `fmetrics.PrincipalOf`, and the `*fmetrics.Set` methods `PROpened(ctx)` and
  `HumanWait(ctx, phase, d)`. Test fixtures already in the package: `newRig`, `issueTask`,
  `escalatedTask`, `roomOf`, `retryBy`, `personBudgets`, `rid`, `killswitch.ConfigMap`.
- Produces: the reconciler's `metrics` interface gains `PROpened(ctx context.Context)` and
  `HumanWait(ctx context.Context, phase string, d time.Duration)`; `fakeMetrics.bound []string`
  holds `"<series> <payer>"` for the series that name one.

- [ ] **Step 1: Write the failing tests**

In `internal/factory/reconciler/reconciler_test.go`, import fmetrics:

```go
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/fmetrics"
	"github.com/Smana/agent-platform/internal/factory/forge"
```

`fakeRuns.Create` keeps the run's `ChargedTo`, as the claim does:

```go
	f.runs[s.RunID] = runs.Run{ID: s.RunID, TaskID: s.TaskID, Role: s.Role, Principal: s.Principal, ChargedTo: s.ChargedTo, RoomRef: s.RoomRef,
		Phase: "Pending", MaxTokens: s.MaxTokens, StartSeq: s.StartSeq, Head: s.Head, Created: created, TaskText: s.TaskText}
```

Replace `fakeMetrics` from its type to `PROutcome`, and `Revoked` and `RunRevoked`, with:

```go
type fakeMetrics struct {
	mu       sync.Mutex
	recorded []string
	bound    []string // the series that name a payer: "<series> <the payer ctx binds>" (spend per identity SI4)
}

func (m *fakeMetrics) add(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recorded = append(m.recorded, s)
}

func (m *fakeMetrics) who(ctx context.Context, s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bound = append(m.bound, s+" "+fmetrics.PrincipalOf(ctx))
}
func (m *fakeMetrics) TimeToPR(_ context.Context, _ time.Duration, source, tier, template string) {
	m.add("time_to_pr " + source + " " + tier + " " + template)
}
func (m *fakeMetrics) PROpened(ctx context.Context) { m.who(ctx, "pr_opened") }
func (m *fakeMetrics) PROutcome(ctx context.Context, class, outcome string) {
	m.add("pr " + class + " " + outcome)
	m.who(ctx, "pr "+outcome)
}
func (m *fakeMetrics) HumanWait(ctx context.Context, phase string, d time.Duration) {
	m.who(ctx, fmt.Sprintf("wait %s %.0f", phase, d.Seconds()))
}
```

```go
func (m *fakeMetrics) Revoked(ctx context.Context, reason string) {
	m.add("revoked " + reason)
	m.who(ctx, "revoked "+reason)
}
```

```go
func (m *fakeMetrics) RunRevoked(ctx context.Context, reason, limit string) {
	m.add("revoked " + reason)
	m.add("limit " + limit)
	m.who(ctx, "revoked "+reason)
}
```

`recorded` keeps every string it held, so no existing expectation moves. At the end of
`TestLabelToNarratedRun`, after its `recorded` check, add:

```go
	// Spend per identity SI4: per-person caps off, the task is the fleet's.
	if want := "pr_opened system:factory|pr human_merged system:factory"; strings.Join(g.metrics.bound, "|") != want {
		t.Fatalf("%q, want %q", g.metrics.bound, want)
	}
```

`internal/factory/reconciler/payer_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/killswitch"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// Spend per identity SI8: a task leaving Escalated adds the time since its phaseSince to its
// payer's waits, the payer of the work that waited: the labeller, not the maintainer whose retry
// ended the wait; the fleet with per-person caps off.
func TestAWaitCountsForTheTasksPayer(t *testing.T) {
	for enabled, payer := range map[bool]string{true: "github:smana", false: runs.PrincipalFactory} {
		g := newRig(t, escalatedTask(false), roomOf("3buqdlot"))
		g.r.Cfg.Budgets = personBudgets(enabled, true)
		g.r.Cfg.Maintainers = append(g.r.Cfg.Maintainers, "Alice")
		g.f.SetComments(7, retryBy(77, "Alice", time.Minute))
		if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseQueued {
			t.Fatalf("enabled %v: %s", enabled, tk.Status.Phase)
		}
		if want := "wait Escalated 600 " + payer; !slices.Contains(g.metrics.bound, want) {
			t.Fatalf("enabled %v: %q, want %q", enabled, g.metrics.bound, want)
		}
		g.reconcile(t, "3buqdlot", 1) // Queued → Implementing: no wait for a human
		if n := len(slices.DeleteFunc(slices.Clone(g.metrics.bound), func(s string) bool { return !strings.HasPrefix(s, "wait ") })); n != 1 {
			t.Fatalf("enabled %v: %d waits, want the one: %q", enabled, n, g.metrics.bound)
		}
	}
}

// SI4: a stop's revocations name each run's payer, as the ledger charged it, whatever the task's
// payer is now.
func TestAStopsRevocationsNameEachRunsPayer(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.r.Cfg.Budgets = personBudgets(true, true)
	g.reconcile(t, "3buqdlot", 3)
	g.runs.set(rid(0), "Running")
	g.runs.runs["aaaaaaa2"] = runs.Run{ID: "aaaaaaa2", TaskID: "3buqdlot", Phase: "Pending", Principal: runs.PrincipalFactory,
		ChargedTo: "github:bob"}
	_ = g.c.Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: killswitch.ConfigMap, Namespace: "agent-system"}})
	g.reconcile(t, "3buqdlot", 1)
	got := slices.DeleteFunc(slices.Clone(g.metrics.bound), func(s string) bool { return !strings.HasPrefix(s, "revoked manual") })
	slices.Sort(got)
	if want := []string{"revoked manual github:bob", "revoked manual github:smana"}; !slices.Equal(got, want) {
		t.Fatalf("%q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run 'TestAWaitCountsForTheTasksPayer|TestAStopsRevocationsNameEachRunsPayer|TestLabelToNarratedRun' ./internal/factory/reconciler/`
Expected: FAIL: `enabled true: [], want "wait Escalated 600 github:smana"` (or its `enabled false`
twin, whichever the map yields first);
`["revoked manual " "revoked manual "], want ["revoked manual github:bob" "revoked manual github:smana"]`;
`["pr human_merged "], want "pr_opened system:factory|pr human_merged system:factory"`.

- [ ] **Step 3: Write the implementation**

In `internal/factory/reconciler/reconciler.go`, the `metrics` interface:

```go
type metrics interface {
	TimeToPR(ctx context.Context, d time.Duration, source, tier, template string)
	PROpened(ctx context.Context)
	PROutcome(ctx context.Context, class, outcome string)
	HumanWait(ctx context.Context, phase string, d time.Duration)
```

(the rest of the interface unchanged). In `Reconcile`, the pass binds the payer beside the
repository:

```go
	// v2 D10 and spend per identity SI4: the pass's series are the task's repository's and its payer's.
	ctx, err := r.bind(fmetrics.WithPrincipal(fmetrics.WithRepository(ctx, t.Spec.Repository), r.payer(&t)), &t)
```

and after the deferred records run, the wait:

```go
		for _, f := range fx.after {
			f(ctx)
		}
		r.waited(ctx, before, &t.Status)
	}
```

Before `func (r *Reconciler) to(`, add:

```go
// payer is the key the task's costs and yield count for (spend per identity SI1): the person its
// next run is charged to, else the fleet.
func (r *Reconciler) payer(t *v1alpha1.Task) string {
	return runs.TaskPayer(t, r.Cfg.Budgets.PerPerson.Enabled)
}

// waited adds a written exit from AwaitingHuman or Escalated to the payer's waits (SI8): the time
// from the phase's start to the next phase's, both as the status records them.
func (r *Reconciler) waited(ctx context.Context, before, after *v1alpha1.TaskStatus) {
	if before.Phase == after.Phase || before.PhaseSince == nil || after.PhaseSince == nil {
		return
	}
	if before.Phase == v1alpha1.PhaseAwaitingHuman || before.Phase == v1alpha1.PhaseEscalated {
		r.Metrics.HumanWait(ctx, before.Phase, after.PhaseSince.Sub(before.PhaseSince.Time))
	}
}
```

In `stop`, each revoked run counts under its own payer, as the ledger charged it:

```go
			if run.Revoked == "" {
				r.Metrics.Revoked(fmetrics.WithPrincipal(ctx, run.Payer()), "manual") // as the ledger charged it
			}
```

In `internal/factory/reconciler/implement.go`, `detectPR`'s record:

```go
	record(ctx, func(ctx context.Context) {
		r.Metrics.TimeToPR(ctx, d, source, tier, tmpl)
		r.Metrics.PROpened(ctx)
	})
```

- [ ] **Step 4: Run the package**

Run: `go test -race -count=1 ./internal/factory/reconciler/ && go build ./... && task lint`
Expected: `ok` (about 75 s with `-race`), a clean build, `0 issues.`

- [ ] **Step 5: Commit**

```bash
git add internal/factory/reconciler/
git commit -m "feat(reconciler): a task's series name its payer, and its waits for a human count"
```

---

### Task 5: Refusals and swept revocations name their payer; the wiring

**Files:**
- Modify: `internal/factory/api/server.go`, `internal/factory/intake/issues.go`
- Modify: `internal/factory/killswitch/sweep.go`, `internal/app/factory.go`
- Test: `internal/factory/api/payer_test.go` (create), `internal/factory/intake/issues_test.go`,
  `internal/factory/intake/person_test.go`, `internal/factory/killswitch/sweep_test.go`

**Interfaces:**
- Consumes: Task 2's `fmetrics.WithPrincipal`, `PrincipalOf`, `Set.Charged`, `Set.RunCharges`,
  `Set.ChargePeople`; Task 3's `Meter.OnCharge`, `Meter.OnCharges`; Task 1's `runs.Run.Payer`.
  API test fixtures already in the package: `personServer`, `personLinks`, `ledgerCM`, `post`,
  `base`, `alice`, `store`.
- Produces: `func (s *Sweeper) Sweep(ctx context.Context) ([]runs.Run, error)` and
  `Sweeper.OnSweep func(revoked []runs.Run)`.

- [ ] **Step 1: Write the failing tests**

`internal/factory/api/payer_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/Smana/agent-platform/internal/factory/fmetrics"
	"github.com/Smana/agent-platform/internal/metrics"
)

// scraped gives s a Set on a real exporter and returns its scrape.
func scraped(t *testing.T, s *Server) func() string {
	t.Helper()
	exp, err := metrics.NewExporter(metrics.FactoryBuildInfo, "v0.0.0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exp.Shutdown(t.Context()) })
	if s.Metrics, err = fmetrics.New(exp.Meter(), nil, "agent-system", func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	s.Metrics.ListRepositories("Smana/cloud-native-ref")
	return func() string {
		rec := httptest.NewRecorder()
		exp.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
		b, _ := io.ReadAll(rec.Body)
		return string(b)
	}
}

// Spend per identity SI4: the API's refusals name the requester as authenticated, and a refusal on
// a person's day the person whose day it is, the key over.
func TestARefusalNamesItsRequesterOrThePersonOverTheirDay(t *testing.T) {
	people := personLinks{by: map[string]string{"human:291": "alice"}}
	spent := func() *corev1.ConfigMap { return ledgerCM("20260927", map[string]int64{alice: 4_000_000}) }
	s := personServer(t, &store{}, true, true, people, spent())
	scrape := scraped(t, s)
	if w := post(s.Handler(), "alice", base()); w.Code != http.StatusTooManyRequests {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	s.Owned = func(string) bool { return false }
	if w := post(s.Handler(), "alice", base()); w.Code != http.StatusConflict {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	body := scrape()
	for _, want := range []string{
		`agent_factory_admission_refusals_total{principal="github:alice",reason="person_budget_exhausted",repository="Smana/cloud-native-ref"} 1`,
		`agent_factory_admission_refusals_total{principal="human:291",reason="repository_not_owned",repository="Smana/cloud-native-ref"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s\n%s", want, body)
		}
	}
}
```

In `internal/factory/intake/issues_test.go`, import `"github.com/Smana/agent-platform/internal/factory/fmetrics"`
(between `config` and `forge`) and let the fake keep the principal:

```go
func (c *counter) AdmissionRefused(ctx context.Context, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refused = append(c.refused, reason+" "+fmetrics.PrincipalOf(ctx))
}
```

In `internal/factory/intake/person_test.go`:

```go
				len(refusals) != 1 || refusals[0] != "person_budget_exhausted github:smana") { // spend per identity SI4: the labeller
```

In `internal/factory/killswitch/sweep_test.go`, `TestTheStopSweepsEveryRun` reads the revoked runs:

```go
	if revoked, err := s.Sweep(context.Background()); len(revoked) != 0 || err != nil || len(st.runs) != 4 {
		t.Fatal("no stop object: nothing is touched")
	}
	_ = c.Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMap, Namespace: "agent-system"}})
	revoked, err := s.Sweep(context.Background())
	ids := []string{}
	for _, r := range revoked {
		ids = append(ids, r.ID)
	}
	slices.Sort(ids)
	if err != nil || !slices.Equal(ids, []string{"aaaaaaaa", "bbbbbbbb"}) || st.revoked["bbbbbbbb"] != "manual" || st.revoked["aaaaaaaa"] != "manual" {
		t.Fatalf("every live run, the human's included, each revoked run counted once: %v %v %v", ids, st.revoked, err)
	}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 ./internal/factory/api/ ./internal/factory/intake/ ./internal/factory/killswitch/`
Expected: FAIL: the API's two `missing agent_factory_admission_refusals_total{principal=…}` lines;
the intake's `["person_budget_exhausted "]`; the sweep's build error
`invalid argument: revoked (variable of type int) for built-in len`.

- [ ] **Step 3: Write the implementation**

`internal/factory/api/server.go`, in `createRun`:

```go
	// Spend per identity SI4: a refusal names the requester as authenticated. Resolving their
	// GitHub link here would cost the call mayWrite saves for a request every local rule admits.
	ctx := fmetrics.WithPrincipal(fmetrics.WithRepository(r.Context(), in.Repository), p.ID)
```

and the person's day:

```go
		if reason == reasonPersonOver {
			s.refuseRepository(fmetrics.WithPrincipal(ctx, person), w, code, reason) // the key over, as the shadow names it
			return
		}
```

`internal/factory/intake/issues.go`, in `personSpent`:

```go
	if p.Errors != nil {
		p.Errors.AdmissionRefused(fmetrics.WithPrincipal(ctx, person), "person_budget_exhausted") // the labeller (SI4)
	}
```

`internal/factory/killswitch/sweep.go`:

```go
	// OnSweep is told of the runs a sweep revoked, each to count under its payer.
	OnSweep func(revoked []runs.Run)
```

```go
		if revoked, err := s.Sweep(ctx); err != nil {
			s.Log.Warn("stop sweep failed", "err", err)
		} else if len(revoked) > 0 && s.OnSweep != nil {
			s.OnSweep(revoked)
		}
```

and `Sweep` returns the runs it revoked, where it returned how many:

```go
// Sweep revokes and deletes every non-terminal run while the stop is engaged: the annotation
// first, so the composition sees the revoke even if the delete then fails. A run already gone
// mid-sweep is done, not an error. It returns the runs it revoked: a run that already carried a
// revoke, such as one the task reconciler's stop revoked seconds earlier, was counted by whoever
// wrote it (F30).
func (s *Sweeper) Sweep(ctx context.Context) ([]runs.Run, error) {
	on, err := Engaged(ctx, s.Reader, s.Namespace)
	if err != nil || !on {
		return nil, err
	}
	all, err := s.Runs.List(ctx)
	if err != nil {
		return nil, err
	}
	var revoked []runs.Run
```

with `n++` becoming `revoked = append(revoked, r)` and `return n, errors.Join(errs...)` becoming
`return revoked, errors.Join(errs...)`.

`internal/app/factory.go`: after `m.ListRepositories(conns.names()...)`,

```go
	m.ChargePeople(cfg.Budgets.PerPerson.Enabled)
```

the meter's two callbacks,

```go
		&meter.Meter{Runs: rc, Source: vm, Every: cfg.Poll.Meter.Duration, OnRevoke: m.RunRevoked, Log: log,
			OnCharge: m.Charged, OnCharges: m.RunCharges,
```

and the sweep's payers:

```go
			OnSweep: func(revoked []runs.Run) {
				for _, r := range revoked {
					m.Revoked(fmetrics.WithPrincipal(ctx, r.Payer()), "manual")
				}
			}, Log: log},
```

- [ ] **Step 4: Run them to verify they pass**

Run:

```bash
go test -race -count=1 ./internal/factory/... ./internal/app/... ./api/...
go build ./... && task lint
```

Expected: every package `ok`, a clean build, `0 issues.`

- [ ] **Step 5: Commit**

```bash
git add internal/factory/api/ internal/factory/intake/ internal/factory/killswitch/ internal/app/factory.go
git commit -m "feat(factory): refusals and swept revocations name their payer, and the meter's charges are wired"
```

---

### Task 6: The docs, the gate, the pull request and the release

**Files:**
- Modify: `docs/operations.md`, `AGENTS.md`

- [ ] **Step 1: The series in `docs/operations.md`**

In the factory metrics table, replace the rows of `agent_factory_tasks`, `agent_factory_pr_outcomes_total`,
`agent_factory_run_revocations_total` and the first cells of `agent_factory_admission_refusals_total`,
and add four rows:

```markdown
| `agent_factory_tasks` | gauge | `phase`, `source`, `predicted_class`, `tier`, `repository`, `principal` | Tasks by state and payer ([spend per identity](#spend-per-identity)) |
```

```markdown
| `agent_factory_pr_outcomes_total` | counter | `class`, `outcome`, `repository`, `principal` | `auto_merged`, `human_merged`, `closed`, `reverted` |
| `agent_factory_prs_opened_total` | counter | `principal`, `repository` | Task pull requests opened, where `agent_factory_time_to_pr_seconds` records |
```

```markdown
| `agent_factory_run_revocations_total` | counter | `reason`, `limit`, `principal` | Runs the factory revoked; `limit` when the run meter did it (below); `principal` the run's payer, or the key over for `budget-principal-shadow` |
```

after the `agent_factory_model_cost_usd_total` row:

```markdown
| `agent_factory_charged_usd_total` | counter | `principal`, `kind` | USD the day ledgers charged, by the run's payer and `kind` (`task`, `room`), added when the write that settles it succeeds ([spend per identity](#spend-per-identity)) |
| `agent_factory_run_charge_info` | gauge | `run`, `principal`, `task`, `room` | 1 for each run not ended, and each ended one for two minutes after its `finishedAt`; leader only. `task` is absent for a room's run |
| `agent_factory_human_wait_seconds_total` | counter | `principal`, `phase` | Seconds tasks waited in `AwaitingHuman` or `Escalated`, added as each leaves the phase, under the task's payer |
```

and `agent_factory_admission_refusals_total`'s labels cell becomes `` `repository`, `reason`, `principal` ``.

After the paragraph ending "`agent_factory_model_cost_usd_total` can miss what a run spent before
such a reset.", add:

```markdown
### Spend per identity

Each run has one payer, the last ledger key the meter charges it to: the person in its
`charged-to` annotation, else its own principal. A labelled task's runs pay as the labeller (or the
maintainer whose `/factory retry` sent it back), a schedule's and RunLore's as `system:factory`, an
API run as its caller's `github:<login>`, or `human:<sub>` with no GitHub link. A task's payer is
the key its next run is charged to: `system:factory` with per-person caps off.

| Series | What it answers |
|---|---|
| `agent_factory_charged_usd_total{principal, kind}` | What the ledgers charged for the work a principal caused. Per person key, a day's increase is that day's `spent`; summed over payers, the fleet's `spent` with per-person caps on. R10's lift reaches a run's `settled.<runId>`, never `spent`, and is never counted. A leader that exits loses the charges of its last scrape interval from its counter, never from the ledger. Read it with `increase_pure` on VictoriaMetrics: a payer's series is born at its first charge |
| `agent_factory_run_charge_info{run, principal, task, room}` | Which runs, tasks and rooms a principal paid for: join a run's gateway series (`ar_agent` ends in `xplane-run-<run>`) or kube-state-metrics' `agentrun_*{run_id}` on it |
| `principal` on outcomes, openings, refusals, revocations, waits and `agent_factory_tasks` | The payer the series is recorded under. An API refusal names the caller as authenticated (`human:<sub>`), and a refusal on a person's day that person. The shadow keeps the key over |

`principal` takes a ledger key's shape (`system:<name>`, `github:<login>`, `human:<sub>`), else
`other`. `run`, `task` and `room` appear only on the per-live-run series.
```

- [ ] **Step 2: The label rule in `AGENTS.md` (OD-C)**

```markdown
  Label values are bounded: never a payload field; `room` only on the per-Active-room gauge and
  on per-live-run series such as `agent_factory_run_charge_info`, whose `run` label already makes
  one series per run.
```

- [ ] **Step 3: Check and commit the docs**

Run: `bash hack/docs.sh; echo "exit=$?"`
Expected: `exit=0` (the anchor `#spend-per-identity` resolves).

```bash
git add docs/operations.md AGENTS.md
git commit -m "docs(operations): spend per identity's series, and room on per-live-run series"
```

- [ ] **Step 4: The whole gate**

With Docker running (the store's testcontainers):

```bash
git fetch origin && git rebase origin/feat/daily-caps-usd   # or --onto origin/main once PR 2 merged
task check; echo "exit=$?"
git grep -n 'OnSweep(n\|Sweep(ctx) (int' -- internal
```

Expected: `exit=0`, and no `git grep` output.

- [ ] **Step 5: Push and open the pull request**

PR 2 (agent-platform #168) must be merged first: GitHub then shows only this branch's commits once
it is rebased `--onto origin/main`. Until then, open it as a draft against `feat/daily-caps-usd`.

```bash
git push -u origin feat/spend-per-identity
gh pr create --draft --base feat/daily-caps-usd --title "feat(factory): spend per identity, every run's payer on the factory's series" --body-file - <<'EOF'
## What

v3 feature 12 (ai-native-ref spend-per-identity design, SI1–SI11): every run has one payer, the
last ledger key the meter charges it to, and the factory's series say who it is.

- `agent_factory_charged_usd_total{principal, kind}`: what the day ledgers charged, under the
  run's payer, when the write that settles it succeeds or when `resolve` finds a write in doubt
  landed. R10's lift is never counted.
- `agent_factory_run_charge_info{run, principal, task, room}`: the live runs, and those ended less
  than two minutes ago, so the gateway's per-run tokens join to their payer.
- `principal` on PR outcomes, admission refusals, every revocation and `agent_factory_tasks`;
  `agent_factory_prs_opened_total` and `agent_factory_human_wait_seconds_total` are new.
- `docs/operations.md` lists them; AGENTS.md lets `room` sit on per-live-run series (OD-C).

Not breaking: the labels are new, and every reader sums over them.

## Verification

`task check`: exit 0. Offline proofs 1–5: `TestThePayersChargesPartitionTheLedger`,
`TestTheChargesFollowTheLedgerThroughItsFailures`, `TestTheChargeInfoNamesLiveRunsAndThoseJustEnded`,
`TestThePayerLabelsTheSeriesItsContextBinds`, `TestAWaitCountsForTheTasksPayer`,
`TestAStopsRevocationsNameEachRunsPayer`, `TestARefusalNamesItsRequesterOrThePersonOverTheirDay`.
EOF
```

Once PR 2 merges: `git rebase --onto origin/main <PR 2's last head> feat/spend-per-identity`,
`task check`, `git push --force-with-lease`, `gh pr edit --base main`, then verify the base as its
own step (`gh pr view --json baseRefName -q .baseRefName` prints `main`), and `gh pr ready`.

- [ ] **Step 6: The release (owner)**

To ride v0.16.0: the owner holds PR 2's tag (its plan's Task 6, Step 5) until this PR merges, then
tags `main` once: `git tag -a v0.16.0 -m "v0.16.0" && git push origin v0.16.0`. ai-native-ref's
truthful costs PR 3 then pins one release that carries both. If v0.16.0 is tagged without it, tag
this one `v0.16.1` after it merges, and Task 10 is its own pin PR.

---

## PR 2: ai-native-ref

### Task 7: The `agent-identity` dashboard, its links and its suite

**Files:**
- Create: `observability/base/agent-platform/grafana-dashboard-agent-identity.yaml`
- Modify: `observability/base/agent-platform/kustomization.yaml`
- Modify: `observability/base/agent-platform/grafana-dashboard-agent-fleet.yaml`, `grafana-dashboard-agent-factory.yaml`
- Test: `scripts/ci/tests/test-agent-identity-dashboard.py`

**Interfaces:**
- Consumes: PR 1's series by name (`agent_factory_charged_usd_total{principal, kind}`,
  `agent_factory_run_charge_info{run, principal, task, room}`, `agent_factory_prs_opened_total`,
  `agent_factory_human_wait_seconds_total`, `principal` on outcomes, refusals, revocations and
  `agent_factory_tasks`), PR 2 of truthful costs' `agent_factory_budget_{spent,cap}_usd{principal}`,
  F4's `agent_factory_principal_sponsor_info` and `agent_factory_max_cost_usd_total`, the gateway's
  `gen_ai_client_token_usage_sum{ar_agent, gen_ai_token_type}`, KSM's `agentrun_*{run_id}`.
- Produces (Task 8 reads them by panel title and `refId`): the panels "Spend in the range, by kind"
  (A), "The fleet's day" (A, B, C), "Tokens by type" (A), "Cache-read share" (A),
  "Runs · rooms · tasks" (A runs, B rooms, C tasks), "Cost per accepted PR" (A), and the variable
  `$${principal}`.

- [ ] **Step 1: Create the worktree**

```bash
git -C /home/smana/Sources/ai-native-ref fetch origin
git -C /home/smana/Sources/ai-native-ref worktree add /home/smana/Sources/ai-native-ref/.claude/worktrees/spend-identity -b feat/spend-identity-dashboard origin/main
cd /home/smana/Sources/ai-native-ref/.claude/worktrees/spend-identity && mise trust mise.toml
export PYTHONDONTWRITEBYTECODE=1
```

- [ ] **Step 2: Write the failing suite**

`scripts/ci/tests/test-agent-identity-dashboard.py` (mode 755):

```python
#!/usr/bin/env python3
# requires: python3
"""Spend per identity, offline proof 6 (docs/superpowers/specs/2026-10-22-spend-per-identity-design.md):
the agent-identity dashboard on the real tree. One principal at a time and no ranking of people
(SI6, SI7): a single-value variable without All, the framing text, and no panel that groups, sorts or
ranks by principal. Every figure is VictoriaMetrics, filtered on the page's principal, and every
counter or per-run series reads through increase_pure (SI11). The fleet and factory pages link to it."""
import json
import pathlib
import re
import sys

try:
    import yaml
except ImportError:
    print("SKIP: pyyaml not installed")
    sys.exit(77)

ROOT = pathlib.Path(__file__).resolve().parents[3]
DASHBOARDS = "observability/base/agent-platform"
errors = []


def check(ok, message):
    if not ok:
        errors.append(message)


def board(name):
    rel = f"{DASHBOARDS}/grafana-dashboard-{name}.yaml"
    raw = (ROOT / rel).read_text()
    check(not re.search(r"(?<!\$)\$\{(?!private_domain_name\})", raw),
          f"{rel}: every ${{…}} but ${{private_domain_name}} is written $${{…}} for Flux")
    docs = [d for d in yaml.safe_load_all(raw) if d and d.get("kind") == "GrafanaDashboard"]
    if len(docs) != 1:
        errors.append(f"{rel}: expected one GrafanaDashboard, found {len(docs)}")
        return {}, {}
    return docs[0], json.loads(docs[0]["spec"]["json"].replace("$$", "$"))


def flat(b):
    for p in b.get("panels", []):
        yield p
        yield from p.get("panels", [])


PRINCIPAL = 'principal="${principal}"'
FLEET = 'principal="system:factory"'
# A grouping, sort or rank that would line principals up against each other (SI7).
RANKS = re.compile(r"\b(by|without)\s*\([^)]*\bprincipal\b|\b(topk|bottomk|sort|sort_desc|sort_by_label|sort_by_label_desc|limitk)\s*\(")
# Counters and per-run series the page reads over a range: never increase() or rate() (SI11).
COUNTED = ("agent_factory_charged_usd_total", "agent_factory_prs_opened_total", "agent_factory_pr_outcomes_total",
           "agent_factory_admission_refusals_total", "agent_factory_run_revocations_total",
           "agent_factory_human_wait_seconds_total", "agent_factory_max_cost_usd_total", "gen_ai_client_token_usage_sum")
WANT = {"What this page is", "Today, against its day", "The fleet's day", "Spend in the range, by kind", "Spend per day",
        "Tokens by type", "Cache-read share", "Runs · rooms · tasks", "Runs", "Pull requests opened · merged · reverted",
        "Cost per accepted PR", "Refusals", "Revocations", "Waiting for a human now", "Time waited", "Sponsor",
        "Ceilings against spend", "Stopped at the ceiling"}


def check_identity():
    doc, b = board("agent-identity")
    if not b:
        return
    check(doc["spec"].get("folderRef") == "agents", "agent-identity sits in the agents folder (OD-A)")
    check(b.get("uid") == "agent-identity" and b.get("title") == "Spend per identity", "uid agent-identity, titled Spend per identity")
    check(b.get("timezone") == "utc", "the page is in UTC, the ledger's day")
    check("not a score" in b.get("description", ""), "the dashboard's description frames it as cost attribution")
    variables = {v["name"]: v for v in b.get("templating", {}).get("list", [])}
    var = variables.get("principal", {})
    check(var.get("query") == "label_values(agent_factory_charged_usd_total, principal)", "principal comes from the charged counter")
    check(var.get("multi") is False and var.get("includeAll") is False, "principal is single-valued, without All")
    check(set(variables) == {"datasource", "principal"}, f"no other variable: {sorted(variables)}")
    panels = {p["title"]: p for p in flat(b)}
    check(WANT <= set(panels), f"missing panels: {sorted(WANT - set(panels))}")
    text = panels.get("What this page is", {})
    check(text.get("type") == "text" and "not a score" in text.get("options", {}).get("content", ""),
          "the text panel says the page attributes cost and scores nobody")
    for p in flat(b):
        if p.get("type") in ("row", "text"):
            continue
        title = p["title"]
        check(p.get("datasource", {}).get("type") == "prometheus", f"{title}: VictoriaMetrics only")
        for t in p.get("targets", []):
            expr = t.get("expr", "")
            check(t.get("datasource") in (None, p.get("datasource")), f"{title} {t['refId']}: the panel's datasource")
            check(not RANKS.search(expr), f"{title} {t['refId']}: groups, sorts or ranks by principal")
            check(PRINCIPAL in expr or (title == "The fleet's day" and FLEET in expr),
                  f"{title} {t['refId']}: filters on the page's principal")
            check(not re.search(r"(?<!_)\b(increase|rate|irate)\(", expr), f"{title} {t['refId']}: increase_pure, never increase or rate")
            for series in COUNTED:
                for m in re.finditer(re.escape(series), expr):
                    check(expr[:m.start()].rstrip().endswith("increase_pure("),
                          f"{title} {t['refId']}: {series} is read through increase_pure")
        for tr in p.get("transformations", []):
            check("principal" not in json.dumps(tr), f"{title}: a transformation names principal")
    runs = panels.get("Runs", {})
    charge = 'max_over_time(agent_factory_run_charge_info{principal="${principal}"}[$__range])'
    for t in runs.get("targets", []):
        check(charge in t["expr"], f"Runs {t['refId']}: restricted to this principal's runs by the charge info")
    check("/d/agent-run/agent-run?var-run=${__value.text}" in json.dumps(runs.get("fieldConfig", {})),
          "a run_id opens its Agent run page")
    resources = yaml.safe_load((ROOT / DASHBOARDS / "kustomization.yaml").read_text()).get("resources", [])
    check("grafana-dashboard-agent-identity.yaml" in resources, f"{DASHBOARDS}/kustomization.yaml lists the dashboard")


def check_links():
    _, fleet = board("agent-fleet")
    check(any(link.get("url", "").startswith("/d/agent-identity/agent-identity") for link in fleet.get("links", [])),
          "agent-fleet links to Spend per identity")
    _, factory = board("agent-factory")
    budget = next((p for p in flat(factory) if p.get("title") == "Budget left today"), {})
    check("/d/agent-identity/agent-identity?var-principal=${__field.labels.principal}" in json.dumps(budget.get("fieldConfig", {})),
          "each bar of agent-factory's budget panel opens its principal's page")


for run in (check_identity, check_links):
    run()
if errors:
    print("\n".join(errors), file=sys.stderr)
    sys.exit(1)
print("PASS")
```

Run: `bash $SCRATCH/run-suite.sh python3 scripts/ci/tests/test-agent-identity-dashboard.py`
Expected: FAIL with a traceback opening `grafana-dashboard-agent-identity.yaml`: the page does not exist.

- [ ] **Step 3: The dashboard**

`observability/base/agent-platform/grafana-dashboard-agent-identity.yaml`:

```yaml
---
# Spend per identity (v3 feature 12, docs/superpowers/specs/2026-10-22-spend-per-identity-design.md):
# one charged principal at a time, what the platform spent on the work it caused. It attributes
# cost; it does not score a person (SI7): no panel groups, sorts or ranks by principal, and the
# variable has no All. Every figure reads VictoriaMetrics, in UTC, the ledger's day:
# - the factory's per-payer series (agent-platform fmetrics): agent_factory_charged_usd_total,
#   the day gauges agent_factory_budget_*_usd, outcomes, openings, refusals, revocations, waits;
# - the gateway's per-run tokens and kube-state-metrics' agentrun_*, joined to the payer through
#   agent_factory_run_charge_info{run, principal, task, room} (SI3).
# increase_pure everywhere (SI11): a payer's series is born inside the range, and increase() drops
# a large first value. $${…} survives Flux's postBuild substitution; $1 and $__range pass untouched.
apiVersion: grafana.integreatly.org/v1beta1
kind: GrafanaDashboard
metadata:
  name: agent-identity
  namespace: observability
spec:
  allowCrossNamespaceImport: true
  folderRef: "agents"
  instanceSelector:
    matchLabels:
      dashboards: "grafana"
  json: |
    {
      "title": "Spend per identity",
      "uid": "agent-identity",
      "description": "What the platform spent on the work one principal caused, against the daily caps the owner set. Outcomes are the factory's yield on that work, not a score of the person.",
      "schemaVersion": 39,
      "timezone": "utc",
      "time": {"from": "now-30d", "to": "now"},
      "links": [
        {"title": "Agent fleet", "type": "link", "url": "/d/agent-fleet/agent-fleet", "keepTime": true},
        {"title": "Agent factory", "type": "link", "url": "/d/agent-factory/agent-factory", "keepTime": true}
      ],
      "templating": {"list": [
        {"name": "datasource", "type": "datasource", "query": "prometheus"},
        {"name": "principal", "label": "Principal", "type": "query", "refresh": 2, "sort": 1, "multi": false, "includeAll": false,
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "definition": "label_values(agent_factory_charged_usd_total, principal)", "query": "label_values(agent_factory_charged_usd_total, principal)"}
      ]},
      "panels": [
        {"id": 1, "type": "text", "title": "What this page is", "gridPos": {"x": 0, "y": 0, "w": 24, "h": 3},
         "options": {"mode": "markdown", "content": "What the platform spent on the work **$${principal}** caused: a run started by a label counts for whoever labelled it, a schedule's or RunLore's for `system:factory`, against the daily caps the owner set. The outcomes are the factory's yield on that work, not a score of the person: an agent wrote the code."}},
        {"id": 2, "type": "row", "title": "Spend", "collapsed": false, "gridPos": {"x": 0, "y": 3, "w": 24, "h": 1}, "panels": []},
        {"id": 3, "type": "stat", "title": "Today, against its day", "gridPos": {"x": 0, "y": 4, "w": 6, "h": 6},
         "description": "The ledger key's day, UTC. For system:factory that key is the fleet.",
         "fieldConfig": {"defaults": {"unit": "currencyUSD", "decimals": 2}, "overrides": []},
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "targets": [
           {"refId": "A", "instant": true, "legendFormat": "spent today", "expr": "max(agent_factory_budget_spent_usd{principal=\"$${principal}\"})"},
           {"refId": "B", "instant": true, "legendFormat": "daily cap", "expr": "max(agent_factory_budget_cap_usd{principal=\"$${principal}\"})"}
         ]},
        {"id": 4, "type": "stat", "title": "The fleet's day", "gridPos": {"x": 6, "y": 4, "w": 6, "h": 6},
         "fieldConfig": {"defaults": {"unit": "currencyUSD", "decimals": 2}, "overrides": [
           {"matcher": {"id": "byFrameRefID", "options": "C"}, "properties": [{"id": "unit", "value": "percentunit"}, {"id": "decimals", "value": 1}]}
         ]},
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "targets": [
           {"refId": "A", "instant": true, "legendFormat": "fleet spent today", "expr": "max(agent_factory_budget_spent_usd{principal=\"system:factory\"})"},
           {"refId": "B", "instant": true, "legendFormat": "fleet cap", "expr": "max(agent_factory_budget_cap_usd{principal=\"system:factory\"})"},
           {"refId": "C", "instant": true, "legendFormat": "this principal's share", "expr": "max(agent_factory_budget_spent_usd{principal=\"$${principal}\"}) / max(agent_factory_budget_spent_usd{principal=\"system:factory\"})"}
         ]},
        {"id": 5, "type": "bargauge", "title": "Spend in the range, by kind", "gridPos": {"x": 12, "y": 4, "w": 6, "h": 6},
         "description": "What the day ledgers charged in the range: task runs and room runs (POST /v1/runs).",
         "fieldConfig": {"defaults": {"unit": "currencyUSD", "decimals": 2}, "overrides": []},
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "targets": [{"refId": "A", "instant": true, "legendFormat": "{{kind}}", "expr": "sum by (kind) (increase_pure(agent_factory_charged_usd_total{principal=\"$${principal}\"}[$__range]))"}]},
        {"id": 6, "type": "timeseries", "title": "Spend per day", "gridPos": {"x": 18, "y": 4, "w": 6, "h": 6}, "interval": "1d",
         "fieldConfig": {"defaults": {"unit": "currencyUSD", "custom": {"drawStyle": "bars", "stacking": {"mode": "normal"}, "fillOpacity": 80}}, "overrides": []},
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "targets": [{"refId": "A", "legendFormat": "{{kind}}", "expr": "sum by (kind) (increase_pure(agent_factory_charged_usd_total{principal=\"$${principal}\"}[1d]))"}]},
        {"id": 7, "type": "row", "title": "Tokens", "collapsed": false, "gridPos": {"x": 0, "y": 10, "w": 24, "h": 1}, "panels": []},
        {"id": 8, "type": "bargauge", "title": "Tokens by type", "gridPos": {"x": 0, "y": 11, "w": 12, "h": 6},
         "description": "The gateway's per-run counters of this principal's runs. input counts the cache reads and writes inside it.",
         "fieldConfig": {"defaults": {"unit": "short"}, "overrides": []},
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "targets": [{"refId": "A", "instant": true, "legendFormat": "{{gen_ai_token_type}}", "expr": "sum by (gen_ai_token_type) (label_replace(sum by (ar_agent, gen_ai_token_type) (increase_pure(gen_ai_client_token_usage_sum{ar_agent=~\"system:serviceaccount:agents:xplane-run-.*\", gen_ai_token_type=~\"input|output|cached_input|cache_creation_input\"}[$__range])), \"run\", \"$1\", \"ar_agent\", \"system:serviceaccount:agents:xplane-run-(.*)\") * on (run) group_left () max by (run) (max_over_time(agent_factory_run_charge_info{principal=\"$${principal}\"}[$__range])))"}]},
        {"id": 9, "type": "stat", "title": "Cache-read share", "gridPos": {"x": 12, "y": 11, "w": 12, "h": 6},
         "description": "cached_input over input, as the agent-gateway dashboard's prompt-cache panel computes it.",
         "fieldConfig": {"defaults": {"unit": "percentunit", "decimals": 1, "min": 0, "max": 1}, "overrides": []},
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "targets": [{"refId": "A", "instant": true, "expr": "sum(label_replace(sum by (ar_agent) (increase_pure(gen_ai_client_token_usage_sum{ar_agent=~\"system:serviceaccount:agents:xplane-run-.*\", gen_ai_token_type=\"cached_input\"}[$__range])), \"run\", \"$1\", \"ar_agent\", \"system:serviceaccount:agents:xplane-run-(.*)\") * on (run) group_left () max by (run) (max_over_time(agent_factory_run_charge_info{principal=\"$${principal}\"}[$__range]))) / sum(label_replace(sum by (ar_agent) (increase_pure(gen_ai_client_token_usage_sum{ar_agent=~\"system:serviceaccount:agents:xplane-run-.*\", gen_ai_token_type=\"input\"}[$__range])), \"run\", \"$1\", \"ar_agent\", \"system:serviceaccount:agents:xplane-run-(.*)\") * on (run) group_left () max by (run) (max_over_time(agent_factory_run_charge_info{principal=\"$${principal}\"}[$__range])))"}]},
        {"id": 10, "type": "row", "title": "Work", "collapsed": false, "gridPos": {"x": 0, "y": 17, "w": 24, "h": 1}, "panels": []},
        {"id": 11, "type": "stat", "title": "Runs · rooms · tasks", "gridPos": {"x": 0, "y": 18, "w": 6, "h": 10},
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "targets": [
           {"refId": "A", "instant": true, "legendFormat": "runs", "expr": "count(max by (run) (max_over_time(agent_factory_run_charge_info{principal=\"$${principal}\"}[$__range])))"},
           {"refId": "B", "instant": true, "legendFormat": "rooms", "expr": "count(max by (room) (max_over_time(agent_factory_run_charge_info{principal=\"$${principal}\", room!=\"\"}[$__range])))"},
           {"refId": "C", "instant": true, "legendFormat": "tasks", "expr": "count(max by (task) (max_over_time(agent_factory_run_charge_info{principal=\"$${principal}\", task!=\"\"}[$__range])))"}
         ]},
        {"id": 12, "type": "table", "title": "Runs", "gridPos": {"x": 6, "y": 18, "w": 18, "h": 10},
         "description": "This principal's runs in the range. Spend is a priced run's, until its cost is reported. Each run opens its Agent run page.",
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "targets": [
           {"refId": "A", "instant": true, "format": "table", "expr": "label_replace(max by (run, task, room) (max_over_time(agent_factory_run_charge_info{principal=\"$${principal}\"}[$__range])), \"run_id\", \"$1\", \"run\", \"(.*)\")"},
           {"refId": "B", "instant": true, "format": "table", "expr": "topk by (run_id) (1, tlast_over_time(agentrun_info{namespace=\"agents\"}[$__range])) and on (run_id) label_replace(max by (run) (max_over_time(agent_factory_run_charge_info{principal=\"$${principal}\"}[$__range])), \"run_id\", \"$1\", \"run\", \"(.*)\")"},
           {"refId": "C", "instant": true, "format": "table", "expr": "max by (run_id, phase) (last_over_time(agentrun_status_phase{namespace=\"agents\"}[$__range]) == 1) and on (run_id) label_replace(max by (run) (max_over_time(agent_factory_run_charge_info{principal=\"$${principal}\"}[$__range])), \"run_id\", \"$1\", \"run\", \"(.*)\")"},
           {"refId": "D", "instant": true, "format": "table", "expr": "topk by (run_id) (1, tlast_over_time(agentrun_pull_request_info{namespace=\"agents\"}[$__range])) and on (run_id) label_replace(max by (run) (max_over_time(agent_factory_run_charge_info{principal=\"$${principal}\"}[$__range])), \"run_id\", \"$1\", \"run\", \"(.*)\")"},
           {"refId": "E", "instant": true, "format": "table", "expr": "(max by (run_id) (last_over_time(agentrun_started_timestamp_seconds{namespace=\"agents\"}[$__range]) > 0) * 1000) and on (run_id) label_replace(max by (run) (max_over_time(agent_factory_run_charge_info{principal=\"$${principal}\"}[$__range])), \"run_id\", \"$1\", \"run\", \"(.*)\")"},
           {"refId": "F", "instant": true, "format": "table", "expr": "label_replace(sum by (run) (max_over_time(agent_factory_run_spend_usd[$__range])), \"run_id\", \"$1\", \"run\", \"(.*)\") and on (run_id) label_replace(max by (run) (max_over_time(agent_factory_run_charge_info{principal=\"$${principal}\"}[$__range])), \"run_id\", \"$1\", \"run\", \"(.*)\")"}
         ],
         "transformations": [
           {"id": "merge", "options": {}},
           {"id": "organize", "options": {"renameByName": {"Value #E": "started", "Value #F": "spend"}}},
           {"id": "filterFieldsByName", "options": {"include": {"names": ["run_id", "role", "task", "room", "phase", "pull_request", "started", "spend"]}}},
           {"id": "sortBy", "options": {"sort": [{"field": "started", "desc": true}]}}
         ],
         "fieldConfig": {"defaults": {}, "overrides": [
           {"matcher": {"id": "byName", "options": "run_id"}, "properties": [{"id": "links", "value": [{"title": "Open this run", "url": "/d/agent-run/agent-run?var-run=$${__value.text}&$${__url_time_range}"}]}]},
           {"matcher": {"id": "byName", "options": "started"}, "properties": [{"id": "unit", "value": "dateTimeAsIso"}]},
           {"matcher": {"id": "byName", "options": "spend"}, "properties": [{"id": "unit", "value": "currencyUSD"}, {"id": "decimals", "value": 2}]},
           {"matcher": {"id": "byName", "options": "pull_request"}, "properties": [{"id": "links", "value": [{"title": "Pull request", "url": "$${__value.text}", "targetBlank": true}]}]}
         ]}},
        {"id": 13, "type": "row", "title": "Outcomes", "collapsed": false, "gridPos": {"x": 0, "y": 28, "w": 24, "h": 1}, "panels": []},
        {"id": 14, "type": "bargauge", "title": "Pull requests opened · merged · reverted", "gridPos": {"x": 0, "y": 29, "w": 12, "h": 6},
         "fieldConfig": {"defaults": {"unit": "short", "decimals": 0}, "overrides": []},
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "targets": [
           {"refId": "A", "instant": true, "legendFormat": "opened", "expr": "sum(increase_pure(agent_factory_prs_opened_total{principal=\"$${principal}\"}[$__range]))"},
           {"refId": "B", "instant": true, "legendFormat": "{{outcome}}", "expr": "sum by (outcome) (increase_pure(agent_factory_pr_outcomes_total{principal=\"$${principal}\"}[$__range]))"}
         ]},
        {"id": 15, "type": "stat", "title": "Cost per accepted PR", "gridPos": {"x": 12, "y": 29, "w": 12, "h": 6},
         "description": "Task spend over pull requests merged (auto or by a human) less those reverted, in the range (SI10). No data when none was accepted.",
         "fieldConfig": {"defaults": {"unit": "currencyUSD", "decimals": 2}, "overrides": []},
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "targets": [{"refId": "A", "instant": true, "expr": "sum(increase_pure(agent_factory_charged_usd_total{principal=\"$${principal}\", kind=\"task\"}[$__range])) / ((sum(increase_pure(agent_factory_pr_outcomes_total{principal=\"$${principal}\", outcome=~\"auto_merged|human_merged\"}[$__range])) - (sum(increase_pure(agent_factory_pr_outcomes_total{principal=\"$${principal}\", outcome=\"reverted\"}[$__range])) or vector(0))) > 0)"}]},
        {"id": 16, "type": "row", "title": "Limits", "collapsed": false, "gridPos": {"x": 0, "y": 35, "w": 24, "h": 1}, "panels": []},
        {"id": 17, "type": "bargauge", "title": "Refusals", "gridPos": {"x": 0, "y": 36, "w": 12, "h": 6},
         "description": "Run requests and labels refused, by reason, and runs admitted past a day's cap in shadow that name this key.",
         "fieldConfig": {"defaults": {"unit": "short", "decimals": 0}, "overrides": []},
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "targets": [
           {"refId": "A", "instant": true, "legendFormat": "{{reason}}", "expr": "sum by (reason) (increase_pure(agent_factory_admission_refusals_total{principal=\"$${principal}\"}[$__range]))"},
           {"refId": "B", "instant": true, "legendFormat": "over the day (shadow)", "expr": "sum(increase_pure(agent_factory_run_revocations_total{principal=\"$${principal}\", reason=\"budget-principal-shadow\"}[$__range]))"}
         ]},
        {"id": 18, "type": "bargauge", "title": "Revocations", "gridPos": {"x": 12, "y": 36, "w": 12, "h": 6},
         "fieldConfig": {"defaults": {"unit": "short", "decimals": 0}, "overrides": []},
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "targets": [{"refId": "A", "instant": true, "legendFormat": "{{reason}} {{limit}}", "expr": "sum by (reason, limit) (increase_pure(agent_factory_run_revocations_total{principal=\"$${principal}\", reason!=\"budget-principal-shadow\"}[$__range]))"}]},
        {"id": 19, "type": "row", "title": "Waits", "collapsed": false, "gridPos": {"x": 0, "y": 42, "w": 24, "h": 1}, "panels": []},
        {"id": 20, "type": "stat", "title": "Waiting for a human now", "gridPos": {"x": 0, "y": 43, "w": 12, "h": 6},
         "description": "This principal's tasks in AwaitingHuman or Escalated: each holds one of the factory's awaitingHumanWIP slots.",
         "fieldConfig": {"defaults": {"unit": "short", "decimals": 0}, "overrides": []},
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "targets": [{"refId": "A", "instant": true, "expr": "sum(agent_factory_tasks{principal=\"$${principal}\", phase=~\"AwaitingHuman|Escalated\"}) or vector(0)"}]},
        {"id": 21, "type": "bargauge", "title": "Time waited", "gridPos": {"x": 12, "y": 43, "w": 12, "h": 6},
         "description": "How long this principal's tasks waited for a human in the range, added as each wait ended. It counts for the work, never for whoever answered.",
         "fieldConfig": {"defaults": {"unit": "s"}, "overrides": []},
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "targets": [{"refId": "A", "instant": true, "legendFormat": "{{phase}}", "expr": "sum by (phase) (increase_pure(agent_factory_human_wait_seconds_total{principal=\"$${principal}\"}[$__range]))"}]},
        {"id": 22, "type": "row", "title": "Sponsored", "collapsed": false, "gridPos": {"x": 0, "y": 49, "w": 24, "h": 1}, "panels": []},
        {"id": 23, "type": "stat", "title": "Sponsor", "gridPos": {"x": 0, "y": 50, "w": 8, "h": 5},
         "description": "A sponsored principal's sponsor, from the factory's config (F4). No data until F4 ships, and for a person.",
         "options": {"textMode": "name", "reduceOptions": {"calcs": ["lastNotNull"]}},
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "targets": [{"refId": "A", "instant": true, "legendFormat": "{{sponsor}}", "expr": "max by (sponsor) (agent_factory_principal_sponsor_info{principal=\"$${principal}\"})"}]},
        {"id": 24, "type": "stat", "title": "Ceilings against spend", "gridPos": {"x": 8, "y": 50, "w": 8, "h": 5},
         "fieldConfig": {"defaults": {"unit": "currencyUSD", "decimals": 2}, "overrides": []},
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "targets": [
           {"refId": "A", "instant": true, "legendFormat": "ceilings admitted", "expr": "sum(increase_pure(agent_factory_max_cost_usd_total{principal=\"$${principal}\"}[$__range]))"},
           {"refId": "B", "instant": true, "legendFormat": "task spend", "expr": "sum(increase_pure(agent_factory_charged_usd_total{principal=\"$${principal}\", kind=\"task\"}[$__range]))"}
         ]},
        {"id": 25, "type": "stat", "title": "Stopped at the ceiling", "gridPos": {"x": 16, "y": 50, "w": 8, "h": 5},
         "fieldConfig": {"defaults": {"unit": "short", "decimals": 0}, "overrides": []},
         "datasource": {"type": "prometheus", "uid": "$${datasource}"},
         "targets": [{"refId": "A", "instant": true, "expr": "sum(increase_pure(agent_factory_run_revocations_total{principal=\"$${principal}\", reason=\"budget-task\", limit=\"task-usd\"}[$__range]))"}]}
      ]
    }
```

`observability/base/agent-platform/kustomization.yaml`, after the gateway dashboard:

```yaml
  - grafana-dashboard-agent-gateway.yaml
  - grafana-dashboard-agent-identity.yaml
```

- [ ] **Step 4: The links**

`grafana-dashboard-agent-fleet.yaml`, the dashboard links:

```json
      "links": [
        {"title": "Agent platform", "type": "link", "url": "/d/agent-platform/agent-platform", "keepTime": true},
        {"title": "Spend per identity", "type": "link", "url": "/d/agent-identity/agent-identity", "keepTime": true}
      ],
```

`grafana-dashboard-agent-factory.yaml`, the "Budget left today" panel's `fieldConfig`: each bar
opens its own principal's page.

```json
         "fieldConfig": {"defaults": {"unit": "percentunit", "min": 0, "max": 1,
           "links": [{"title": "Spend per identity", "url": "/d/agent-identity/agent-identity?var-principal=$${__field.labels.principal}&$${__url_time_range}"}]}, "overrides": []},
```

Truthful costs PR 3 changes the same panel's `expr` to `remaining_usd / cap_usd`; the link sits in
`fieldConfig`, two lines above, and the division keeps `principal`. If PR 3 merged first, rebase and
keep both.

- [ ] **Step 5: Run the suites**

```bash
bash $SCRATCH/run-suite.sh python3 scripts/ci/tests/test-agent-identity-dashboard.py
bash $SCRATCH/run-suite.sh python3 scripts/ci/tests/test-agent-observability.py
```

Expected: `PASS` twice (the second holds the fleet and factory pages to their existing checks).

- [ ] **Step 6: Prove the suite bites**

In a scratch copy of the dashboard, change "Spend in the range, by kind"'s
`sum by (kind) (increase_pure(` to `sum by (kind, principal) (increase(` and `"includeAll": false`
to `true`, then run the suite against it. Expected: exit 1 with
`principal is single-valued, without All`, `groups, sorts or ranks by principal`,
`increase_pure, never increase or rate` and `agent_factory_charged_usd_total is read through increase_pure`.
Restore the file (`git diff --stat` shows only this task's files).

- [ ] **Step 7: Commit**

```bash
chmod 755 scripts/ci/tests/test-agent-identity-dashboard.py
git add observability/base/agent-platform/grafana-dashboard-agent-identity.yaml observability/base/agent-platform/kustomization.yaml \
  observability/base/agent-platform/grafana-dashboard-agent-fleet.yaml observability/base/agent-platform/grafana-dashboard-agent-factory.yaml \
  scripts/ci/tests/test-agent-identity-dashboard.py
git commit -m "feat(observability): the agent-identity dashboard, one charged principal at a time"
```

---

### Task 8: The panels' own queries under promtool

**Files:**
- Test: `scripts/ci/tests/test-agent-identity-queries.sh`

**Interfaces:**
- Consumes: Task 7's panels by title and `refId` (listed there).
- Produces: offline proof 7.

- [ ] **Step 1: Write the suite**

`scripts/ci/tests/test-agent-identity-queries.sh` (mode 755):

```bash
#!/usr/bin/env bash
# requires: promtool python3
#
# Spend per identity, offline proof 7: the agent-identity panels' own queries, read from the
# dashboard JSON, evaluated by promtool on synthetic series shaped as the factory and the gateway
# export them, for two principals at once.
# - Two payers' spends in the range sum to the fleet's day, and alice's share of it reads hers.
# - Alice's run reading 1050 input with 900 cached gives her a 0.857 cache-read share, and bob, who
#   ran nothing, no data; a fleet run's tokens stay out of hers.
# - Three runs in two rooms of one task read 3 runs, 2 rooms, 1 task, though no run is live at once.
# - $3 of task spend with 2 merged and 1 reverted reads $3.00 per accepted PR; with 0 accepted, no data.
# The panels run on VictoriaMetrics, where increase_pure counts a series from 0; promtool has none,
# and agrees with increase() on series that start at 0, as these do (test-agent-cost-rules does the same).
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
python3 -c 'import yaml' 2>/dev/null || { echo "SKIP: pyyaml not installed"; exit 77; }
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

ROOT="$ROOT" OUT="$tmp" python3 - <<'PY' || exit 1
import json
import os
import sys

import yaml

root, out = os.environ["ROOT"], os.environ["OUT"]
rel = "observability/base/agent-platform/grafana-dashboard-agent-identity.yaml"
doc = next(d for d in yaml.safe_load_all(open(os.path.join(root, rel))) if d and d.get("kind") == "GrafanaDashboard")
board = json.loads(doc["spec"]["json"])
panels = {p["title"]: p for p in board["panels"]}


def q(title, ref, principal):
    """The panel's query as Grafana runs it for principal over a 1h range ($${…} is Flux's escape)."""
    expr = next(t["expr"] for t in panels[title]["targets"] if t["refId"] == ref)
    expr = expr.replace("$${principal}", principal).replace("$__range", "1h").replace("increase_pure(", "increase(")
    if "$" in expr.replace("$1", ""):
        sys.exit(f"{title} {ref}: a variable this test does not set: {expr}")
    return expr


A, B, F = "github:alice", "github:bob", "system:factory"
X = 'namespace="agent-system", pod="agent-factory-0"'
G = 'gen_ai_client_token_usage_sum{namespace="agent-gateway", ar_agent="system:serviceaccount:agents:xplane-run-'


def charge(principal, run, task, room):
    labels = f'{X}, run="{run}", principal="{principal}"' + (f', task="{task}"' if task else "") + (f', room="{room}"' if room else "")
    return "agent_factory_run_charge_info{" + labels + "}"


def tokens(run, kind):
    return G + f'{run}", gen_ai_request_model="glm-5.3", gen_ai_token_type="{kind}"' + "}"


tests = [
    # Spend: alice $2 of tasks and $0.50 of rooms, bob $1 of rooms; the fleet's day is the sum.
    {"interval": "1m",
     "input_series": [
         {"series": f'agent_factory_charged_usd_total{{{X}, principal="{A}", kind="task"}}', "values": "0 2x50"},
         {"series": f'agent_factory_charged_usd_total{{{X}, principal="{A}", kind="room"}}', "values": "0 0.5x50"},
         {"series": f'agent_factory_charged_usd_total{{{X}, principal="{B}", kind="room"}}', "values": "0 1x50"},
         {"series": f'agent_factory_budget_spent_usd{{{X}, principal="{A}"}}', "values": "0 2.5x50"},
         {"series": f'agent_factory_budget_spent_usd{{{X}, principal="{B}"}}', "values": "0 1x50"},
         {"series": f'agent_factory_budget_spent_usd{{{X}, principal="{F}"}}', "values": "0 3.5x50"},
     ],
     "promql_expr_test": [
         {"expr": q("Spend in the range, by kind", "A", A), "eval_time": "50m",
          "exp_samples": [{"labels": '{kind="task"}', "value": 2}, {"labels": '{kind="room"}', "value": 0.5}]},
         {"expr": f'sum({q("Spend in the range, by kind", "A", A)}) + sum({q("Spend in the range, by kind", "A", B)})',
          "eval_time": "50m", "exp_samples": [{"value": 3.5}]},
         {"expr": q("The fleet's day", "A", A), "eval_time": "50m", "exp_samples": [{"value": 3.5}]},
         {"expr": q("The fleet's day", "C", A), "eval_time": "50m", "exp_samples": [{"value": 2.5 / 3.5}]},
     ]},
    # Tokens: alice's run reads 1050 input with 900 cached; a fleet run's tokens are not hers.
    {"interval": "1m",
     "input_series": [
         {"series": tokens("aaaaaaaa", "input"), "values": "0 1050x50"},
         {"series": tokens("aaaaaaaa", "cached_input"), "values": "0 900x50"},
         {"series": tokens("aaaaaaaa", "output"), "values": "0 10x50"},
         {"series": tokens("cccccccc", "input"), "values": "0 5000x50"},
         {"series": tokens("cccccccc", "cached_input"), "values": "0 100x50"},
         {"series": charge(A, "aaaaaaaa", "3buqdlot", "3buqdlot"), "values": "1x50"},
         {"series": charge(F, "cccccccc", "4buqdlot", "4buqdlot"), "values": "1x50"},
     ],
     "promql_expr_test": [
         {"expr": q("Cache-read share", "A", A), "eval_time": "50m", "exp_samples": [{"value": 900 / 1050}]},
         {"expr": q("Cache-read share", "A", B), "eval_time": "50m", "exp_samples": []},
         {"expr": q("Tokens by type", "A", A), "eval_time": "50m",
          "exp_samples": [{"labels": '{gen_ai_token_type="input"}', "value": 1050},
                          {"labels": '{gen_ai_token_type="cached_input"}', "value": 900},
                          {"labels": '{gen_ai_token_type="output"}', "value": 10}]},
     ]},
    # Work: three runs of one task in two rooms, one after the other; bob's room run is not alice's.
    {"interval": "1m",
     "input_series": [
         {"series": charge(A, "aaaaaaaa", "3buqdlot", "3buqdlot"), "values": "1x20"},
         {"series": charge(A, "bbbbbbbb", "3buqdlot", "3buqdlot"), "values": "_x20 1x20"},
         {"series": charge(A, "cccccccc", "3buqdlot", "4buqdlot"), "values": "_x40 1x10"},
         {"series": charge(B, "dddddddd", "", "5buqdlot"), "values": "1x50"},
     ],
     "promql_expr_test": [
         {"expr": q("Runs · rooms · tasks", "A", A), "eval_time": "50m", "exp_samples": [{"value": 3}]},
         {"expr": q("Runs · rooms · tasks", "B", A), "eval_time": "50m", "exp_samples": [{"value": 2}]},
         {"expr": q("Runs · rooms · tasks", "C", A), "eval_time": "50m", "exp_samples": [{"value": 1}]},
         {"expr": q("Runs · rooms · tasks", "A", B), "eval_time": "50m", "exp_samples": [{"value": 1}]},
         {"expr": q("Runs · rooms · tasks", "C", B), "eval_time": "50m", "exp_samples": []},
     ]},
    # Outcomes: alice's $3 of tasks, 2 merged and 1 reverted; bob's $4, 1 merged and 1 reverted.
    {"interval": "1m",
     "input_series": [
         {"series": f'agent_factory_charged_usd_total{{{X}, principal="{A}", kind="task"}}', "values": "0 3x50"},
         {"series": f'agent_factory_charged_usd_total{{{X}, principal="{A}", kind="room"}}', "values": "0 7x50"},
         {"series": f'agent_factory_charged_usd_total{{{X}, principal="{B}", kind="task"}}', "values": "0 4x50"},
     ] + [
         {"series": f'agent_factory_pr_outcomes_total{{{X}, principal="{who}", class="review", outcome="{o}", repository="Smana/cloud-native-ref"}}',
          "values": "0 1x50"}
         for who, o in ((A, "auto_merged"), (A, "human_merged"), (A, "reverted"), (B, "human_merged"), (B, "reverted"))
     ],
     "promql_expr_test": [
         {"expr": q("Cost per accepted PR", "A", A), "eval_time": "50m", "exp_samples": [{"value": 3}]},
         {"expr": q("Cost per accepted PR", "A", B), "eval_time": "50m", "exp_samples": []},
     ]},
]
open(os.path.join(out, "rules.yaml"), "w").write("groups: []\n")
yaml.safe_dump({"rule_files": ["rules.yaml"], "evaluation_interval": "1m", "tests": tests},
               open(os.path.join(out, "tests.yaml"), "w"), sort_keys=False, width=1000)
PY

cd "$tmp" && promtool test rules tests.yaml
```

- [ ] **Step 2: Run it**

Run: `bash $SCRATCH/run-suite.sh bash scripts/ci/tests/test-agent-identity-queries.sh`
Expected: `SUCCESS`. Task 7's dashboard already holds the queries, so this step proves them rather
than drives them.

- [ ] **Step 3: Prove it bites**

Set alice's cache-share expectation to `0.5` and her runs count to `4`, run again. Expected:
`FAILED`, with `got: {} 8.571428571428571E-01` and `got: {} 3E+00`. Restore both.

- [ ] **Step 4: Commit**

```bash
chmod 755 scripts/ci/tests/test-agent-identity-queries.sh
git add scripts/ci/tests/test-agent-identity-queries.sh
git commit -m "test(observability): the identity panels' queries under promtool"
```

---

### Task 9: The docs, the gate and the pull request

**Files:**
- Modify: `website/content/docs/platform/ai-platform/observability.md`
- Modify: `website/content/docs/platform/ai-platform/routing.md`
- Modify: `website/content/docs/platform/ai-platform/status.md`

- [ ] **Step 1: `observability.md`**

The Agents table's dashboards row:

```markdown
| Dashboards | [Grafana](https://grafana.com) | `agent-run`, one page per run, `agent-fleet`, the overview, and `agent-identity`, what one charged principal's work cost |
```

and before "The full transcript (prompts and outputs) stays in the":

```markdown
`agent-identity` (*Spend per identity*) shows one charged principal at a time: `github:<login>` for a
person, `system:<name>` for a sponsored principal, `system:factory` for work no person caused. A run
started by a label counts for whoever labelled it. The page reads the factory's per-payer series
(`agent_factory_charged_usd_total{principal, kind}`, the day gauges, outcomes, refusals, revocations
and waits) and joins the gateway's per-run tokens through `agent_factory_run_charge_info`. It
attributes cost; it does not score a person: no panel ranks principals, and waits count for the work,
never for whoever answered
([design](https://github.com/Smana/ai-native-ref/blob/main/docs/superpowers/specs/2026-10-22-spend-per-identity-design.md)).
The series start with the factory release that records them; earlier days live only in the ledger.

```

- [ ] **Step 2: `routing.md` and `status.md`**

`routing.md`, the dashboards table, after the `agent-fleet` row:

```markdown
| `agent-identity` | One charged principal: dollars against its day and the fleet's, tokens and cache-read share, runs, outcomes, cost per accepted pull request, refusals, revocations and waits |
```

`status.md`, the "Spend per identity" roadmap row (SI8):

```markdown
cost per accepted pull request, refusals, and how long its tasks waited for a human.
```

in place of "cost per accepted pull request, refusals, and how long runs waited on that person."

- [ ] **Step 3: Commit the docs**

```bash
bash $SCRATCH/run-suite.sh ./scripts/ci/validate-links.sh
git add website/content/docs/platform/ai-platform/observability.md website/content/docs/platform/ai-platform/routing.md \
  website/content/docs/platform/ai-platform/status.md
git commit -m "docs(ai-platform): the spend per identity page"
```

Expected: `==> All relative Markdown links resolve`.

- [ ] **Step 4: The whole check**

```bash
bash $SCRATCH/check.sh "$PWD"
grep -E "test-agent-identity|test-agent-observability|PromQL expression|assert-agent-gateway|passed,|resolve|exists|exit=" \
  "$SCRATCH/check-$(basename "$PWD").log"
```

Expected: `PASS  test-agent-identity-dashboard`, `PASS  test-agent-identity-queries`,
`PASS  test-agent-observability`, `114 PromQL expression(s) in 18 group(s) parse` (or more if `main`
gained rules: this PR adds none), `assert-agent-gateway: 14 checks, 0 violations`,
`30 passed, 2 skipped, 0 failed` (two more passes than `main`'s; what must hold is `0 failed`),
`All relative Markdown links resolve`, `Every repository path named in the docs site exists`, `exit=0`.

- [ ] **Step 5: Rebase, push, open the pull request**

```bash
git fetch origin && git rebase origin/main
git push -u origin feat/spend-identity-dashboard
```

If the rebase touched a file of this PR, run Step 4 again first. Title:
`feat(observability): spend per identity, the agent-identity dashboard`. The body, in English,
covers: the outcome (one page per charged principal, cost attribution, not a score); rulings R11,
R12 and R13 of this plan with a link to it on agent-platform; the suites with their offline proofs
(6 and 7) and the `task check` summary (8); that the panels read No data until PR 3 pins a factory
release that records the series, and the Sponsored row until F4; the live proofs waiting for one
gcp-0 session after PR 3. No attribution line.

---

## PR 3: ai-native-ref, the pin

### Task 10: Pin the release that carries PR 1

**Files** (the six #82 moved for v0.15.6, `git show bc11b34 --stat`):
- Modify: `flux/sources/ocirepo-agent-factory.yaml`, `tooling/base/agent-factory/helm-values-configmap.yaml`
- Modify: `infrastructure/base/room-broker/{app,crd-rooms,retention-cronjob,sqlinstance}.yaml`

- [ ] **Step 1: Which pull request carries the pin**

If PR 1 rode v0.16.0 (Task 6, Step 6), truthful costs PR 3 pins v0.16.0 and this task adds nothing
to it but Step 3's check. Otherwise, once `v0.16.1` is tagged, create a worktree on
`chore/pin-agent-platform-v0.16.1` from a freshly fetched `origin/main` and run `mise trust mise.toml`.

- [ ] **Step 2: Move every agent-platform pin to the release, by digest, as #82 did**

```bash
oras resolve ghcr.io/smana/charts/agent-factory:0.16.1
skopeo inspect --format '{{.Digest}}' docker://ghcr.io/smana/agent-factory:v0.16.1
skopeo inspect --format '{{.Digest}}' docker://ghcr.io/smana/room-broker:v0.16.1
gh release download v0.16.1 --repo Smana/agent-platform --pattern crd-rooms.yaml --output "$SCRATCH/crd-rooms.yaml"
```

Write the chart's `tag: "0.16.1"` and its digest in `ocirepo-agent-factory.yaml`, the factory image
`v0.16.1@<digest>` in the values (and the version in its comment), the room-broker image in `app.yaml`
and the retention CronJob, `ref: v0.16.1` in `sqlinstance.yaml`. In `crd-rooms.yaml`, keep the three
comment lines (the second naming `v0.16.1`) and replace everything from the first `---` with
`$SCRATCH/crd-rooms.yaml`. Then `git grep -n 'v0\.15\.6' -- flux tooling infrastructure/base/room-broker`
prints nothing.

- [ ] **Step 3: Check, commit, open the pull request**

```bash
bash $SCRATCH/run-suite.sh python3 scripts/ci/tests/test-agent-factory-pins.py
bash $SCRATCH/check.sh "$PWD"
git add flux/sources/ocirepo-agent-factory.yaml tooling/base/agent-factory/helm-values-configmap.yaml \
  infrastructure/base/room-broker/app.yaml infrastructure/base/room-broker/crd-rooms.yaml \
  infrastructure/base/room-broker/retention-cronjob.yaml infrastructure/base/room-broker/sqlinstance.yaml
git commit -m "chore(agent-platform): pin v0.16.1"
```

Expected: `PASS` from the pins suite and `exit=0` in the check log. Title:
`chore(agent-platform): pin v0.16.1`, the body naming spend per identity's series as what it
brings. After it merges, the spec's live proofs 1–5 run in one gcp-0 session, with truthful costs'.

---

## Done when

| Offline proof (spec) | Where |
|---|---|
| 1. The partition: labelled, schedule, RunLore, linked and unlinked API runs, priced and not, through a day | `TestThePayersChargesPartitionTheLedger` (Task 3) |
| 2. The ledger match: per person key the counter is `spent`; over every payer, `spent[system:factory]` | `TestThePayersChargesPartitionTheLedger` (Task 3) |
| 3. Through a leader change, a write in doubt that landed, a failed write, midnight, the switch day's ended run (R10, and its late reading with the lift) | `TestTheChargesFollowTheLedgerThroughItsFailures` (Task 3) |
| 4. The charge info: live, `endSettle` after its end, then absent; never a run a new leader first sees later; another shape reads `other` | `TestTheChargeInfoNamesLiveRunsAndThoseJustEnded` (Task 3), `TestTheChargeInfoNamesEachRunsPayer`, `TestThePrincipalTakesALedgerKeysShape` (Task 2) |
| 5. The labels; a wait equals `phaseSince` to leaving; the shadow keeps its key; `docs/operations.md`; `task check` | `TestThePayerLabelsTheSeriesItsContextBinds`, `TestTheTasksGaugeNamesThePayer` (Task 2), `TestARevocationNamesTheRunsPayer` (Task 3), `TestAWaitCountsForTheTasksPayer`, `TestAStopsRevocationsNameEachRunsPayer`, `TestLabelToNarratedRun` (Task 4), `TestARefusalNamesItsRequesterOrThePersonOverTheirDay`, `TestALabellerPastTheirDayIsRefusedAtIntake`, `TestTheStopSweepsEveryRun` (Task 5), Task 6 |
| 6. The dashboard: uid, folder, single variable without All, the text, no ranking by principal, `increase_pure`, VictoriaMetrics only | `test-agent-identity-dashboard` (Task 7) |
| 7. promtool on the panels: two payers sum to the fleet; 0.857 and nothing; 3 runs, 2 rooms, 1 task; $3.00 per accepted PR, and nothing | `test-agent-identity-queries` (Task 8) |
| 8. `validate-links`, the doc-path check, `task check` | Task 9 |

## Self-review against the spec

| Spec item | Task |
|---|---|
| SI1 one payer per run; a task's payer | 1 (`Payer`, `TaskPayer`), 4 (`Reconciler.payer`) |
| SI2 the charged counter, `kind`, at the successful write or a landed write in doubt | 2 (`Charged`), 3 (`OnCharge`, `resolve`), 5 (wiring) |
| SI3 the charge info, leader only, `endSettle` after the end | 2 (`RunCharges`, the gauge), 3 (`chargeInfo`), 5 (wiring) |
| SI4 `principal` on outcomes, refusals, the tasks gauge, every revocation; openings; waits | 2, 3 (`OnRevoke`), 4, 5 |
| SI5 the shape, `other`; `run`, `task`, `room` only on the per-live-run series; the shadow's key | 2 (`principalRE`, `PrincipalShadow`) |
| SI6 the dashboard: uid, folder, variable, links from `agent-fleet` and `agent-factory` | 7 |
| SI7 framing: text panel, description, no ranking by principal | 7 (the page and its suite) |
| SI8 waits after the status write, under the task's payer | 4 (`waited`) |
| SI9 F4's two series read, No data until F4 | 7 (the Sponsored row) |
| SI10 cost per accepted PR | 7 (the query), 8 (promtool) |
| SI11 `increase_pure` everywhere | 7 (the queries and the suite's check) |
| OD-A folder `agents` · OD-B factory waits only · OD-C `room` on the charge info, AGENTS.md amended | 7 · 4 · 2 and 6 |
| Gate offline proofs 1–8 | "Done when" above |
| PR order and the v0.16.0 question | the PR table, Task 6 Step 6, Task 10 |

Placeholders: none. The `<PR 2's last head>` in Task 6 is a value read at execution time
(`git rev-parse origin/feat/daily-caps-usd` just before PR 2's merge, or the PR's last commit).
Names used across tasks match: `Payer`, `Kind`, `TaskPayer`, `KindTask`, `KindRoom`,
`WithPrincipal`, `PrincipalOf`, `Charged`, `RunCharges`, `PROpened`, `HumanWait`, `ChargePeople`,
`OnCharge`, `OnCharges`, `Sweep(ctx) ([]runs.Run, error)`, `OnSweep func([]runs.Run)`.
