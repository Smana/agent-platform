# The loop policy, part 2: everything outside the harness that switches a class's loop on — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A row `loops.<class>` in the factory config switches the loop policy on for that class's
implementer runs, and nothing else does. The factory resolves the row at triage, writes it on each
implementer claim as `spec.loop`, and the composition hands it to `agent-run` as `AGENT_LOOP`. When
the harness ends a run `judge_impossible`, `stalled` or `not_met`, the bridge records that reason in
the room, the broker names the run's end with it, and the factory escalates the task under it, with
its narration, its room summary and a counter. With no row, every run is today's run.

**Architecture:** four pull requests in four repositories, in this order:

1. **agent-platform** (this plan's branch). The bridge's `/final-read` reads agent-run's
   `{"endReason": …}` and records it once as `state_changed{harness_end, reason}` on the status
   stream; the broker accepts the kind. runwatch reads the run's last `harness_end` and, for a
   `Failed` run, names the end with it before the harness status. The factory gains `loops`,
   copies a class's resolved row onto the Task as `spec.loop` at triage, and writes it on every
   implementer claim. The end reason then flows through the existing path (room `run_phase` →
   `finished()` → `Escalated`, reason); the factory adds the three reasons' prose, their tier fit
   and `agent_factory_loop_runs_total`.
2. **crossplane-configuration.** The XRD's `spec.loop` gains `stallSteps` and `continuations`; the
   composition gives the harness `AGENT_LOOP`, the whole `spec.loop` as JSON with `judgeModel`
   always named, only when the claim carries `spec.loop`. A second commit pins the harness and
   room-bridge images of the agent-platform release that carries part 1 and PR 1. Released.
3. **cloud-native-ref** pins that release.
4. **ai-native-ref** pins the agent-platform release (factory and broker), moves `HOST_REF`, and
   documents the switch. It sets no row (D4). A live check in the next platform session sets one
   row, reads the result and removes it.

**Tech Stack:** Go 1.27.3 (controller-runtime, OpenTelemetry metrics, pgx), controller-gen through
`task crd:gen`, PostgreSQL through testcontainers for the store test, KCL 0.11.3 and Crossplane 2.1.3
in crossplane-configuration, Python 3 with PyYAML for an offline golden check.

**Spec:** [the loop policy design](https://github.com/Smana/ai-native-ref/blob/main/docs/superpowers/specs/2026-10-21-loops-and-teams-design.md):
L2, L3, "The policy, in the factory config", "How a run ends, and what leaves the room" and "Where
each piece runs". Part 1, the harness: [`2026-10-22-loop-policy-harness-plan.md`](2026-10-22-loop-policy-harness-plan.md)
on branch `feat/loop-policy-harness` (its R1, R2, R13 and R16 are this plan's contract). Capability
profiles P4 (ai-native-ref `docs/superpowers/plans/2026-10-22-capability-profiles-p2-p4-plan.md`):
`spec.loop.judgeModel` and the proxy's `JUDGE_MODEL`, released in crossplane-configuration v0.9.17.
The scoper design (ai-native-ref `docs/superpowers/specs/2026-10-22-scoper-design.md`, SC2), whose
`loops.<class>.scope` joins the same rows later. The roadmap's D4: every feature ships behind a
per-class switch, off by default. Read the design and part 1's rulings before Task 1.

**Out of part 2,** each a follow-up plan:

| Follow-up | What | Depends on |
|---|---|---|
| F1 Reviewer brief (L4) | The factory's reviewer brief (task, diff, checks' output, handoff, no transcript); `room_read` argument-scoped for the reviewer | Profiles P3 (argument constraints, SDK ≥ 1.55 harness) |
| F2 Evidence and abstain (L5) | `room_verdict.evidence`; the broker records an `approve` with none as `abstain`; the gate reads it as `no_approving_verdict`; few-shot calibration | F1 |
| F3 Deferred findings (L6) | A `changes` verdict's PR comment waits for the rounds to end | F2 (abstain counts as a round's end) |
| F4 Holdouts (L7) | `classes.<class>.holdoutChecks` that must pass before the gate arms | Nothing in part 2; a class's CI or scenario suite |
| F5 Per-role models (L8) | `roles.<dataClass>.<role>` and `loops.<class>.roles`, retiring `tiers.*.model` and `templates.*.reviewerModel` | The bench (deferred): it names the implementer alias per class |
| F6 Deduplicated schedules (L9) | A schedule skips its slot while a PR from it is open | Nothing |
| F7 Native runtime | `spec.loop` on agent-platform's own `AgentRun` CRD and `AGENT_LOOP` in `runctl`'s pod, keeping the 3a contract test with the XRD at v0.9.18 | This plan's PR 2 released |
| F8 AG14 reads `loops.*.judge` | ai-native-ref's AG14 (profiles P2–P4 R12) also requires every `loops.*.judge` other than `none` in the implementer profile's models | Profiles P2–P4 PR A merged, and this plan's PR 4 |
| F9 The wandering-share panel | A panel on `agent_factory_loop_runs_total`: `stalled` over all ends, per class | This plan's PR 4 |
| F10 Search subagent (L10) | The child conversation and the bridge's mirroring | SDK ≥ 1.56 harness |

### This plan's rulings

| # | Ruling | Why |
|---|---|---|
| R1 | **A row is whole, and a class reads its own row, else `default`'s.** `loops.<class>` when present, else `loops.default`; the two are never merged key by key. A row switches the loop on when it sets any of `stallSteps`, `continuations`, `judge`. An unset key in a row that is on takes the design's default: 80, 2, `tier-light`. `judge: none` is no judge | The design's example writes every row whole, and the scoper's SC2 says "`loops.default` covers `review` and every class without a row". A key-level merge would make a class row inherit a switch it never wrote, which D4 forbids. Switching on by "any loop key" gives `{}` and `{scope: …}` no loop |
| R2 | **The scoper's key slots in as one more field of the same row.** Rows are a Go struct, `config.Loop`, decoded strictly; the scoper's PR adds `Scope string json:"scope,omitempty"`. `Loop.On()` reads the three loop keys only, so `{scope: steering}` never switches the loop on, and `LoopFor` keeps its signature | No schema break: a file written for part 2 parses under the scoper's release unchanged. Before it, `scope` is an unknown key and fails the start, which is the strict config rule (AGENTS.md) |
| R3 | **Validation:** a row's key is `default`, `review` or a class some repository declares. `stallSteps` 1 to 500, `continuations` 0 to 5 (the harness's R16, which the XRD repeats). `judge` is `none` or one of the four aliases. A row that is on is refused when `runs.group` is not `cloud.ogenki.io` | The scoper's validator rule, written once here. A bound the harness would refuse must fail the factory's start, not leave a run silently off. The native `AgentRun` (adoption 3a) has no `spec.loop` (F7): its API server would prune the field, and the switch would read on while nothing changed |
| R4 | **The row is resolved at triage onto `Task.spec.loop`** (`stallSteps`, `continuations`, `judgeModel`, every value explicit). A config change never moves a task already triaged, and a task triaged before this release has none | Scoper SC2 records its mode on the Task for the same reason. The bench pairs runs by their Task records, so the record must say which policy ran. An in-flight task never switches mid-flight |
| R5 | **Every implementer claim of the task carries it:** the first run, revisions, CI fixes, retries and resumes; never a reviewer, tester or triager. It is set in `startRun`, the one place a claim is made | L2: "a completion judge closes every implementer run that ends on its own". Part 1's harness ignores `AGENT_LOOP` on any other role; the claim does not carry what no one reads. `investigate` tasks run only the triager, so they never carry it |
| R6 | **The claim's `spec.loop` holds every value; `judgeModel` is left out for no judge.** The composition writes `judgeModel: none` into `AGENT_LOOP` when the claim names none | The XRD's `judgeModel` enum is the four aliases (v0.9.17). Writing every value makes the claim record the policy, so a later default change in the harness never changes what a recorded run ran under |
| R7 | **The bridge records the end once per value, as `state_changed{harness_end, reason}` on the status stream,** before it reads the log in the final read. Only `judge_impossible`, `stalled` and `not_met` are recorded. The body is at most 256 bytes; no body, a body that does not parse, or another value records nothing, and the read still runs. The broker allows the kind | The final read is part 1's contract (R13) and runs on loopback before agent-server stops. A status-stream item keeps one numbering and needs no new wire type. The harness's main path and its signal path may both send the reason: recording once keeps the log clean |
| R8 | **runwatch reads the run's last `harness_end` and names a `Failed` run's end with it, before the harness status.** Deletion, `BudgetExhausted`, `Revoked` and `room_busy` keep their precedence. A reason with any phase but `Failed` is ignored | A stalled conversation is `paused`, not `finished` (part 1, R13), and today's rules would call it `pod_lost` or `deadline`. A loop end exits 1, which the composition maps to `Failed`. The agent shares the pod's loopback and can post a final read itself: every reason it could forge escalates its own run, and a run that exits 0 ignores one |
| R9 | **The factory's outcome needs no new path.** The broker's `run_phase` reason flows through `finished()` to `Escalated` with that reason, as `agent_stuck` does today. The factory adds each reason's prose to the narration and to the room summary (the reason **category**, never the judge's text: C7). `stalled` and `not_met` score the tier `under`; `judge_impossible` is unscored | The design's table: "`Escalated`, reason `judge_impossible`" (and the two others), "the fixed narration with the reason category only". A task the judge finds impossible says nothing about the tier, as `agent_error` says nothing (R53) |
| R10 | **One counter, `agent_factory_loop_runs_total{class, end, repository}`,** once per ended implementer run of a task with `spec.loop`; `end` is `judge_impossible`, `stalled`, `not_met`, `finished` (the run succeeded) or `other` | The roadmap's "wandering share" is `stalled` over all ends, per class, and needs both counts. Like `agent_factory_human_interventions_total`, it is recorded on the reconcile that reads the end: a replay after a conflicting status write may count a run twice, which a share tolerates |
| R11 | **`AGENT_LOOP` is written when the claim has the key `loop`, `{}` included,** as `json.encode(..., sort_keys=True)` of `stallSteps` and `continuations` when present, plus `judgeModel` (the claim's, else `none`) | Part 1's R1: "writes the whole object, `judgeModel` always included". `{}` is falsy in KCL, so the test is `"loop" in _spec`. Sorted keys make the golden byte-stable; the harness reads keys in any order |
| R12 | **The bridge and the harness reach runs only through the composition's image pins.** PR 2's second commit pins the agent-platform release carrying part 1 and PR 1 | `main.k` pins `agent-harness` and `room-bridge` by digest (crossplane-configuration #54). The broker and the factory reach the cluster through ai-native-ref's pins (PR 4) |
| R13 | **Release order is free; switching on is last.** No release changes a run alone: the factory writes `spec.loop` only from a row, ai-native-ref ships none, and the composition writes `AGENT_LOOP` only from `spec.loop`. A v0.9.17 composition prunes `stallSteps` and `continuations` and writes no `AGENT_LOOP`. A bridge newer than its broker sees `harness_end` refused and stubbed (`harness_event{refused}`), which no run reaches while no row is set | Part 1's R2, steps 3 to 6. The live check (Task 11) sets the first row only once PRs 1 to 4 are applied |

**Measured 2026-10-22** on prototypes, since discarded: an agent-platform worktree at `0717cd6`
(`main` with #168) under `$SCRATCH/loop2/ap`, and a copy of crossplane-configuration `origin/main`
`3f3147a` (v0.9.17) under `$SCRATCH/loop2/xpc`. The code below is theirs.

- **agent-platform, every package but `internal/store`:** `go test -race -count=1` passes in all 48
  packages that have tests, the new tests included. `hack/lint.sh` (golangci-lint 2.14.0):
  `0 issues`. `go vet ./internal/store/` passes; the store test (Task 2) needs Docker or a
  PostgreSQL server, and neither runs here, so it ran only as vetted code: CI runs it.
- **The bridge:** a final read carrying `stalled` twice, then `met`, then `bogus` stores exactly one
  `harness_end`, `stalled`, on the status stream. The handler passes `stalled` from
  `{"endReason": "stalled"}`, and nothing from no body, a bare word or a 300-byte value, each
  answering 200.
- **The broker:** `TestABridgePushesWhatItsMappingProduces` with `harness_end` added passes with the
  allow-list change and fails without it.
- **runwatch:** a `Failed` run with status `paused` and end `stalled` ends `stalled`; `met` is
  ignored; a `Succeeded` run ignores `stalled`; `BudgetExhausted` and `Revoked` keep theirs.
- **The config:** `loops: {default: {}, docs: {stallSteps: 40, continuations: 0, judge: none},
  tests: {judge: tier-standard}}` resolves `docs` to 40/0/no judge, `tests` to 80/2/`tier-standard`,
  `dashboards` and `review` to off. `{default: {stallSteps: 90}}` covers `review`. Each refusal of
  Task 3 fails with its message, `scope` with `unknown field "scope"`.
- **The factory:** with `loops.review.stallSteps: 40`, the task records `{40, 2, tier-light}` and the
  implementer's claim spec carries the same; with the config's rows then removed, a `Failed` run whose
  room says `stalled` (and each of the two other reasons) escalates the task under that reason,
  narrates it, and records `loop_run review <reason>`. With no row, neither the task nor the claim
  carries a loop. `runs.Build` writes `spec.loop` only when set, and `judgeModel` only for a judge.
- **The docs:** `hack/docs.sh` passes with Task 6's rows applied.
- **The CRD:** `go tool controller-gen` renders `spec.loop` on the Task CRD with `stallSteps` 1–500,
  `continuations` 0–5 and the alias enum.
- **crossplane-configuration:** `task generate-sync test schema` passes: `agentrun` `PASS: 110/110`
  (v0.9.17's 109 and the new test), `REJECTED settings-reject-internal-egress.yaml`, `25 example
  claim(s) valid`. The example with `stallSteps: 501` is refused (`maximum: got 501, want 500`).
  `task render` needs Docker: each example went through `main.k` with `kcl run` instead, and its
  harness container equals the changed golden's (`MATCH` ×4, `AGENT_LOOP` in the complete one only).

## Global Constraints

- **Default behaviour is unchanged.** A config with no `loops` creates the same claims as today
  (`TestNoLoopRowIsTodaysRun`); every existing test passes unmodified but for the signature changes
  this plan names (`FinalRead`, `healthHandler`'s `finalRead`, `EndReason`, the two fakes).
- **`$SCRATCH`** is a scratch directory outside the repositories.
- **Worktrees:** agent-platform `/home/smana/Sources/agent-platform-loop-part2`, branch
  `feat/loop-policy-factory`, carrying this plan on `origin/main`. crossplane-configuration,
  cloud-native-ref and ai-native-ref each in a sibling worktree from a freshly fetched `origin/main`,
  never in the main checkout. Run `mise trust mise.toml` once in each. Rebase onto a freshly fetched
  `origin/main` before every push.
- **`internal/store` needs Docker** (testcontainers). Without it, run `go vet ./internal/store/` and
  let CI's `check` job run the store test. Say so in the PR.
- **Part 1 is a sibling, not a base.** PR 1 does not need `feat/loop-policy-harness` merged: the
  bridge reads a body nothing sends yet. PR 2's image pin (Task 8) needs a release holding both.
- **Comments carry *why*,** in the codebase's terse style. Exported symbols carry doc comments.
- **SPDX:** every new Go file starts with `// SPDX-License-Identifier: Apache-2.0` (`hack/spdx.sh`).
- **Commits** are conventional, in English, with **no trailers** and no generated-with lines.
- **Verbatim from the spec:**
  - "How a run ends": Judge `impossible` → "`Escalated`, reason `judge_impossible`"; Stall →
    "`Escalated`, reason `stalled`"; Continuations spent → "`Escalated`, reason `not_met`"; on the
    issue, "the fixed narration with the reason **category** only; no judge prose (C7: a public
    task's room text stays in the room)".
  - "The per-class values reach the run through the claim: `spec.loop: {stallSteps, continuations,
    judgeModel}`, an XRD field crossplane-configuration adds and the composition turns into the
    harness's and the sidecar's environment."
  - D4: "each feature ships behind a per-class switch, off by default".

## File structure

| Repository · file | Change | Responsibility |
|---|---|---|
| **PR 1** agent-platform `internal/envelope/loopend.go` | create | `HarnessEnd` kind, the three end reasons, `LoopEnd` |
| `internal/bridge/finalread.go`, `bridge.go` | modify | `FinalRead(ctx, end)`, `recordEnd` |
| `internal/app/bridge.go` | modify | `/final-read` reads `{"endReason"}` |
| `internal/bridgeapi/allow.go` | modify | The broker accepts `harness_end` from a bridge |
| `internal/store/store.go`, `harnessend_test.go` | modify, create | `LastHarnessEnd` |
| `internal/runwatch/events.go`, `run.go` | modify | `EndReason` reads the loop end first |
| `internal/factory/config/config.go`, `loops_test.go` | modify, create | `loops`, `Loop`, `LoopFor`, validation |
| `api/factory/v1alpha1/task_types.go` + generated | modify, regenerate | `Task.spec.loop` |
| `internal/factory/runs/runs.go` | modify | `Spec.Loop`, `spec.loop` on the claim |
| `internal/factory/reconciler/reconciler.go`, `implement.go`, `outcome.go`, `loop_test.go` | modify, create | Triage resolves the row; `startRun` writes it; the end's metric and fit |
| `internal/factory/narrate/narrate.go`, `internal/summary/waiting.go` | modify | Three reasons' prose |
| `internal/factory/fmetrics/fmetrics.go` | modify | `agent_factory_loop_runs_total` |
| `docs/event-envelope.md`, `concepts.md`, `integration.md`, `factory.md`, `operations.md` | modify | The kind, the reasons, the body, the config key, the CRD field, the counter |
| **PR 2** crossplane-configuration `apis/agentrun/definition.yaml`, `kcl/main.k`, `kcl/main_test.k`, `composition.yaml` | modify, regenerate | The two fields and `AGENT_LOOP`; then the image pins |
| `examples/agentrun-complete.yaml`, `tests/golden/*.yaml` | modify | The example's loop and the goldens |
| **PR 3** cloud-native-ref (four files, as P4's PR H) | modify | The release pin |
| **PR 4** ai-native-ref pins, `HOST_REF`, docs | modify | The release on the clusters; the switch documented |

---

## PR 1 — agent-platform

### Task 1: The bridge records the loop end, and the broker takes it

**Files:**
- Create: `internal/envelope/loopend.go`
- Modify: `internal/bridge/finalread.go`, `internal/bridge/bridge.go`, `internal/app/bridge.go`, `internal/bridgeapi/allow.go`
- Test: `internal/bridge/bridge_test.go`, `internal/app/bridge_test.go`, `internal/bridgeapi/server_test.go`

**Interfaces:**
- Produces: `envelope.HarnessEnd = "harness_end"`; `envelope.EndJudgeImpossible`, `EndStalled`,
  `EndNotMet`; `envelope.LoopEnd(reason string) bool`; `(*bridge.Bridge).FinalRead(ctx, end string)`;
  the room event `state_changed{kind: harness_end, reason}` with the run's id.
- Consumes: part 1's `POST /final-read` body `{"endReason": "<reason>"}`.

- [ ] **Step 1: Write the failing tests**

In `internal/bridge/bridge_test.go`, change the three calls `r.b.FinalRead(ctx)` (twice) and
`r.b.FinalRead(call)` to `r.b.FinalRead(ctx, "")` and `r.b.FinalRead(call, "")`, and append:

```go
// Part 2 of the loop policy: a final read carrying a loop end records it once as harness_end on
// the status stream, before it answers; any other value records nothing.
func TestAFinalReadRecordsTheLoopEnd(t *testing.T) {
	f := &fakeAgentServer{pageSize: 2, status: "paused"}
	fb := &fakeBroker{}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	r.b.Interval = time.Hour
	ctx, _ := r.run(t)
	eventually(ctx, t, "the first step mirrors the status", func() bool { return len(fb.stored(wire.StreamStatus)) > 0 })
	for _, end := range []string{"stalled", "stalled", "met", "bogus"} {
		if _, err := r.b.FinalRead(ctx, end); err != nil {
			t.Fatal(err)
		}
	}
	var ends []string
	for _, it := range fb.stored(wire.StreamStatus) {
		var p struct{ Kind, Reason string }
		if json.Unmarshal(it.Payload, &p) == nil && p.Kind == envelope.HarnessEnd {
			ends = append(ends, p.Reason)
		}
	}
	if !slices.Equal(ends, []string{"stalled"}) {
		t.Fatalf("harness_end recorded %v, want [stalled] once", ends)
	}
}
```

In `internal/app/bridge_test.go`, replace every `func(context.Context) (bridge.FinalReadResult, error)`
with `func(context.Context, string) (bridge.FinalReadResult, error)`, and
`never := func(ctx context.Context) (bridge.FinalReadResult, error)` with
`never := func(ctx context.Context, _ string) (bridge.FinalReadResult, error)`. Append:

```go
// Part 2 of the loop policy: agent-run's {"endReason": …} reaches the bridge's final read; no body,
// or one that does not parse, reads as none and the read still runs.
func TestFinalReadCarriesTheLoopEnd(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"the end reason", `{"endReason": "stalled"}`, "stalled"},
		{"no body", "", ""},
		{"not JSON", "stalled", ""},
		{"too long to be one", `{"endReason": "` + strings.Repeat("x", 300) + `"}`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := "unset"
			read := func(_ context.Context, end string) (bridge.FinalReadResult, error) {
				got = end
				return bridge.FinalReadResult{}, nil
			}
			h := healthHandler(func(time.Time) bool { return true }, func() bridge.Admission { return bridge.Admission{} }, read, time.Now)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/final-read", strings.NewReader(c.body))
			req.RemoteAddr = "127.0.0.1:41234"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || got != c.want {
				t.Fatalf("POST %q = %d, end %q; want 200 and %q", c.body, rec.Code, got, c.want)
			}
		})
	}
}
```

In `internal/bridgeapi/server_test.go`, `TestABridgePushesWhatItsMappingProduces`, replace
`"delivered", "interrupted", "undeliverable", "policy_decision", "decision_applied"} {` with
`"delivered", "interrupted", "undeliverable", "policy_decision", "decision_applied", envelope.HarnessEnd} {`.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -count=1 ./internal/bridge/ ./internal/app/ ./internal/bridgeapi/`
Expected: build failures, `undefined: envelope.HarnessEnd` and `too many arguments in call to r.b.FinalRead`.

- [ ] **Step 3: The end reasons**

`internal/envelope/loopend.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package envelope

import "slices"

// HarnessEnd is the state_changed kind a bridge records when agent-run reports why the loop
// policy ended its run (loops design, "How a run ends"): {"kind":"harness_end","reason":…}.
const HarnessEnd = "harness_end"

// The loop policy's end reasons, as agent-run sends them on its final read (harness R13).
const (
	EndJudgeImpossible = "judge_impossible"
	EndStalled         = "stalled"
	EndNotMet          = "not_met"
)

// LoopEnd reports whether reason is one of the loop policy's end reasons.
func LoopEnd(reason string) bool {
	return slices.Contains([]string{EndJudgeImpossible, EndStalled, EndNotMet}, reason)
}
```

- [ ] **Step 4: The bridge**

In `internal/bridge/finalread.go`, add the import block's second group
`"github.com/Smana/agent-platform/internal/envelope"`, then replace:

```go
type finalRead struct {
	ctx  context.Context
	done chan FinalReadResult
}
```

with:

```go
type finalRead struct {
	ctx  context.Context
	end  string
	done chan FinalReadResult
}
```

Replace:

```go
func (b *Bridge) FinalRead(ctx context.Context) (FinalReadResult, error) {
	req := finalRead{ctx: ctx, done: make(chan FinalReadResult, 1)}
```

with:

```go
// end is the loop policy's end reason agent-run reported, "" for none: one of envelope.LoopEnd's
// reasons is recorded on the status stream as harness_end before the log is read, any other ignored.
func (b *Bridge) FinalRead(ctx context.Context, end string) (FinalReadResult, error) {
	req := finalRead{ctx: ctx, end: end, done: make(chan FinalReadResult, 1)}
```

Replace `req.done <- b.serveFinalRead(req.ctx)` with `req.done <- b.serveFinalRead(req.ctx, req.end)`,
`func (b *Bridge) serveFinalRead(ctx context.Context) FinalReadResult {` with
`func (b *Bridge) serveFinalRead(ctx context.Context, end string) FinalReadResult {`, and inside it:

```go
		b.takeInbox(ctx)
		b.readLog(ctx)
```

with:

```go
		b.takeInbox(ctx)
		b.recordEnd(ctx, end)
		b.readLog(ctx)
```

Append to the file:

```go
// recordEnd records the loop policy's end reason once per value: agent-run's final read and its
// signal path's may both carry it.
func (b *Bridge) recordEnd(ctx context.Context, end string) {
	if !envelope.LoopEnd(end) || end == b.endSent {
		return
	}
	b.endSent = end
	b.pushStatus(ctx, b.status.Emit(state(envelope.HarnessEnd, map[string]string{"reason": end})))
}
```

In `internal/bridge/bridge.go`, after the field `lastStatus string    // the status the previous step read, "" if it failed`, add:

```go
	endSent    string    // the loop end reason recorded, "" if none
```

- [ ] **Step 5: The handler and the broker**

In `internal/app/bridge.go`, after `finalReadWait = 3 * time.Second` in the constant block, add:

```go
	// maxFinalReadBody bounds the final read's body: {"endReason": "judge_impossible"} with room to spare.
	maxFinalReadBody = 256
```

Replace `finalRead func(context.Context) (bridge.FinalReadResult, error), now func() time.Time,`
with `finalRead func(context.Context, string) (bridge.FinalReadResult, error), now func() time.Time,`,
and in the `POST /final-read` handler:

```go
		ctx, cancel := context.WithTimeout(r.Context(), finalReadWait)
		defer cancel()
		res, err := finalRead(ctx)
```

with:

```go
		// agent-run's loop end (harness R13): {"endReason": …}, or no body. A body that does not
		// parse records nothing; the read itself still runs, as F11 needs.
		var body struct {
			EndReason string `json:"endReason"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFinalReadBody)).Decode(&body)
		ctx, cancel := context.WithTimeout(r.Context(), finalReadWait)
		defer cancel()
		res, err := finalRead(ctx, body.EndReason)
```

In `internal/bridgeapi/allow.go`, replace `"decision_applied": true}` with
`"decision_applied": true, envelope.HarnessEnd: true}`. `reason` is already in `knownFields`.

- [ ] **Step 6: Run the tests**

Run: `go test -race -count=1 ./internal/bridge/ ./internal/app/ ./internal/bridgeapi/`
Expected: `ok` three times.

- [ ] **Step 7: Commit**

```bash
git add internal/envelope/loopend.go internal/bridge internal/app/bridge.go internal/app/bridge_test.go internal/bridgeapi
git commit -m "feat(bridge): record the loop policy's end reason from agent-run's final read"
```

### Task 2: The broker names the run's end with it

**Files:**
- Modify: `internal/store/store.go`, `internal/runwatch/events.go`, `internal/runwatch/run.go`
- Create: `internal/store/harnessend_test.go`
- Test: `internal/runwatch/runwatch_test.go`, `internal/app/runs_test.go` (the fake)

**Interfaces:**
- Produces: `(*store.Store).LastHarnessEnd(ctx, roomID, runID string) (string, error)`;
  `runwatch.EndReason(r Run, harnessStatus, loopEnd string, refused bool) string`; the broker's
  `run_phase{phase: Failed, reason: judge_impossible | stalled | not_met}`.
- Consumes: Task 1's `harness_end` events.

- [ ] **Step 1: Write the failing tests**

`internal/store/harnessend_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"

	"github.com/Smana/agent-platform/internal/envelope"
)

// Loop policy part 2: the run's last harness_end, read the way runwatch reads its harness_status.
func TestLastHarnessEnd(t *testing.T) {
	s, _, _, _ := open(t)
	put := func(n int64, kind string, fields map[string]any) {
		t.Helper()
		if _, _, err := s.Append(t.Context(), envelope.Draft{RoomID: room, RunID: "7f3cq2xz",
			Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:7f3cq2xz", Role: "implementer"},
			Type:  envelope.StateChanged, Origin: envelope.OriginHarness, OriginClient: "bridge:7f3cq2xz", OriginSeq: n,
			Payload: envelope.StatePayload(kind, fields)}); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.LastHarnessEnd(t.Context(), room, "7f3cq2xz"); err != nil || got != "" {
		t.Fatalf("no end yet: %q, %v", got, err)
	}
	put(1, envelope.HarnessEnd, map[string]any{"reason": "not_met"})
	put(2, envelope.HarnessEnd, map[string]any{"reason": "stalled"})
	put(3, "harness_status", map[string]any{"status": "paused", "previous": "running"})
	if got, err := s.LastHarnessEnd(t.Context(), room, "7f3cq2xz"); err != nil || got != "stalled" {
		t.Fatalf("got %q, %v: want the last, stalled", got, err)
	}
	if got, err := s.LastHarnessEnd(t.Context(), room, "zzzzzzzz"); err != nil || got != "" {
		t.Fatalf("another run: %q, %v", got, err)
	}
}
```

In `internal/runwatch/runwatch_test.go`: replace `EndReason(c.r, c.status, c.refused)` with
`EndReason(c.r, c.status, "", c.refused)` and `EndReason(r, "", false)` with
`EndReason(r, "", "", false)`; add the field `end       string` after `status    string` in
`fakeStore`; add before `func (f *fakeStore) Cursor(`:

```go
func (f *fakeStore) LastHarnessEnd(context.Context, string, string) (string, error) {
	return f.end, nil
}
```

and append:

```go
// Loop policy part 2: a recorded loop end names a Failed run's end before its harness status
// (a stalled conversation is paused); the platform's own ends, and any other phase, keep theirs.
func TestEndReasonReadsTheLoopEndFirst(t *testing.T) {
	start := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	run := func(phase, revoked string) Run {
		return Run{Phase: phase, Revoked: revoked, StartedAt: start, FinishedAt: start.Add(20 * time.Minute), MaxMinutes: 60}
	}
	for _, c := range []struct {
		name        string
		r           Run
		status, end string
		want        string
	}{
		{"a stall ends a paused conversation", run("Failed", ""), "paused", "stalled", "stalled"},
		{"the judge said impossible", run("Failed", ""), "finished", "judge_impossible", "judge_impossible"},
		{"the continuations are spent", run("Failed", ""), "finished", "not_met", "not_met"},
		{"an unknown end is ignored", run("Failed", ""), "finished", "met", "agent_finished"},
		{"a succeeded run ignores an end", run("Succeeded", ""), "finished", "stalled", "agent_finished"},
		{"the budget wins", run("BudgetExhausted", "budget-run"), "paused", "stalled", "budget-run"},
		{"a revocation wins", run("Revoked", "manual"), "paused", "stalled", "revoked"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := EndReason(c.r, c.status, c.end, false); got != c.want {
				t.Fatalf("got %s want %s", got, c.want)
			}
		})
	}
}
```

In `internal/app/runs_test.go`, add before `func (f *fakeRunLog) Cursor(`:

```go
func (f *fakeRunLog) LastHarnessEnd(context.Context, string, string) (string, error) {
	return "", nil
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -count=1 ./internal/runwatch/ && go vet ./internal/store/`
Expected: `too many arguments in call to EndReason`, and `s.LastHarnessEnd undefined`.

- [ ] **Step 3: The store**

In `internal/store/store.go`, before `// unfinishedSQL reads only`, add:

```go
// LastHarnessEnd is the run's last loop end reason the bridge recorded (harness_end), "" for none.
func (s *Store) LastHarnessEnd(ctx context.Context, roomID, runID string) (string, error) {
	var reason string
	err := s.pool.QueryRow(ctx, `SELECT payload->>'reason' FROM events WHERE room_id = $1 AND run_id = $2
		AND type = 'state_changed' AND payload->>'kind' = 'harness_end' ORDER BY seq DESC LIMIT 1`,
		roomID, runID).Scan(&reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return reason, err
}
```

- [ ] **Step 4: runwatch**

In `internal/runwatch/events.go`, add to `Appender` after `LastHarnessStatus`:

```go
	// LastHarnessEnd is the loop policy's end reason the bridge recorded for the run, "" if none.
	LastHarnessEnd(ctx context.Context, roomID, runID string) (string, error)
```

In `Observe`, replace:

```go
	busy, err := e.Store.Cursor(ctx, r.Room, BusyScope+r.ID)
```

with:

```go
	end, err := e.Store.LastHarnessEnd(ctx, r.Room, r.ID)
	if err != nil {
		return fmt.Errorf("runwatch: loop end of run %s: %w", r.ID, err)
	}
	busy, err := e.Store.Cursor(ctx, r.Room, BusyScope+r.ID)
```

then `reason, rules := e.endReason(r, status, busy > 0)` with
`reason, rules := e.endReason(r, status, end, busy > 0)`, and:

```go
func (e *Events) endReason(r Run, status string, refused bool) (string, []string) {
	reason := EndReason(r, status, refused)
```

with:

```go
func (e *Events) endReason(r Run, status, end string, refused bool) (string, []string) {
	reason := EndReason(r, status, end, refused)
```

In `internal/runwatch/run.go`, in `EndReason`'s comment replace `that did was admitted later, and ends on what it did. It is pure: a run with no end time (no finishedAt,` with:

```go
// that did was admitted later, and ends on what it did. loopEnd is the loop policy's end reason the
// bridge recorded (harness_end): it names a Failed run's end before the harness status does, since a
// stalled conversation is paused, not finished (loop policy harness R13). It counts on a Failed run
// only, which is what agent-run's exit 1 becomes. It is pure: a run with no end time (no finishedAt,
```

Replace `func EndReason(r Run, harnessStatus string, refused bool) string {` with
`func EndReason(r Run, harnessStatus, loopEnd string, refused bool) string {`, and after the
`room_busy` block:

```go
	if refused && harnessStatus == "" && r.Phase == "Failed" {
		return RoomBusy
	}
```

add:

```go
	if r.Phase == "Failed" && envelope.LoopEnd(loopEnd) {
		return loopEnd
	}
```

- [ ] **Step 5: Run the tests**

Run: `go test -race -count=1 ./internal/runwatch/ ./internal/app/ && go vet ./internal/store/`
Expected: `ok` twice; vet silent. With Docker: `go test -count=1 -run TestLastHarnessEnd ./internal/store/` → `ok`.

- [ ] **Step 6: Commit**

```bash
git add internal/store internal/runwatch internal/app/runs_test.go
git commit -m "feat(runwatch): a loop end names a failed run's end before the harness status"
```

### Task 3: `loops` in the factory config

**Files:**
- Modify: `internal/factory/config/config.go`
- Create: `internal/factory/config/loops_test.go`

**Interfaces:**
- Produces: `Config.Loops map[string]Loop`; `Loop{StallSteps, Continuations *int32; Judge string}`
  with `On() bool`; `LoopPolicy{StallSteps, Continuations int32; JudgeModel string}`;
  `(*Config).LoopFor(class string) *LoopPolicy` (nil: off); the constants `DefaultStallSteps`,
  `DefaultContinuations`, `DefaultJudge`, `NoJudge`, `MaxStallSteps`, `MaxContinuations`.
- The scoper's PR adds `Scope string` to `Loop` and its own accessor; `On()` and `LoopFor` keep their meaning (R2).

- [ ] **Step 1: Write the failing tests**

`internal/factory/config/loops_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// Loop policy part 2: a class's row, else default's, switches its loop on; an unset key takes
// the design's default; no row, or a row with none of the three keys, is today's run (D4).
func TestLoopFor(t *testing.T) {
	cfg, err := Parse([]byte(good + `loops:
  default: {}
  docs: {stallSteps: 40, continuations: 0, judge: none}
  tests: {judge: tier-standard}
`))
	if err != nil {
		t.Fatal(err)
	}
	for class, want := range map[string]*LoopPolicy{
		"docs":       {StallSteps: 40, Continuations: 0, JudgeModel: ""},
		"tests":      {StallSteps: 80, Continuations: 2, JudgeModel: "tier-standard"},
		"dashboards": nil, // default's row sets no key: off
		"review":     nil,
	} {
		got := cfg.LoopFor(class)
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Errorf("LoopFor(%s) = %+v, want %+v", class, got, want)
		}
	}
	on, err := Parse([]byte(good + "loops: {default: {stallSteps: 90}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := on.LoopFor("review"); got == nil || *got != (LoopPolicy{90, 2, "tier-light"}) {
		t.Errorf("default's row covers review and every class without a row: %+v", got)
	}
	none, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if got := none.LoopFor("docs"); got != nil {
		t.Errorf("no loops: %+v, want off", got)
	}
}

func TestLoopsAreRefused(t *testing.T) {
	for name, c := range map[string]struct{ loops, want string }{
		"an undeclared class":   {"{scripts: {stallSteps: 40}}", `loops.scripts: "scripts" is not default, review or a class`},
		"stallSteps 0":          {"{docs: {stallSteps: 0}}", "loops.docs.stallSteps must be 1..500"},
		"stallSteps 501":        {"{docs: {stallSteps: 501}}", "loops.docs.stallSteps must be 1..500"},
		"continuations 6":       {"{docs: {continuations: 6}}", "loops.docs.continuations must be 0..5"},
		"a model, not an alias": {"{docs: {judge: claude-haiku-5-5}}", `loops.docs.judge "claude-haiku-5-5" is none or one of`},
		"an unknown key":        {"{docs: {scope: steering}}", `unknown field "scope"`},
		"the native runtime":    {"{docs: {judge: none}}\nruns: {group: agents.ogenki.io}", "this runtime's has no spec.loop"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(good + "loops: " + c.loops + "\n"))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}
```

`good` declares `docs-links`, `revert`, `docs`, `tests` and `dashboards`, not `scripts`.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -count=1 ./internal/factory/config/`
Expected: `undefined: LoopPolicy` (build failure).

- [ ] **Step 3: The config**

In `internal/factory/config/config.go`, after the `Runs runwatch.Kind` field of `Config`, add:

```go
	// Loops is the loop policy per task class (loops design L2, L3), keyed by a class, review
	// or default. Each row is whole: a class with no row reads default's, and a row is read
	// alone, never merged with default's (LoopFor).
	Loops map[string]Loop `json:"loops,omitempty"`
```

In `Validate`, replace the closing:

```go
	if c.Tracing.OTLPEndpoint != "" && !hostPortRE.MatchString(c.Tracing.OTLPEndpoint) {
		bad("tracing.otlpEndpoint %q is not host:port", c.Tracing.OTLPEndpoint)
	}
	return errors.Join(errs...)
}
```

with:

```go
	if c.Tracing.OTLPEndpoint != "" && !hostPortRE.MatchString(c.Tracing.OTLPEndpoint) {
		bad("tracing.otlpEndpoint %q is not host:port", c.Tracing.OTLPEndpoint)
	}
	c.validateLoops(bad)
	return errors.Join(errs...)
}
```

Append to the file (the `maps` and `slices` imports are already there):

```go
// Loop is one row of loops. A row switches its class's loop on when it sets any of the three
// keys (D4: no row, or a row without them, is today's run); an unset key then takes the
// design's default. Rows are keyed by class so the scoper's switch (scoper design SC2,
// loops.<class>.scope) joins them as one more key.
type Loop struct {
	// StallSteps ends a run with no commit by this many steps (L3): 1 to 500, default 80.
	StallSteps *int32 `json:"stallSteps,omitempty"`
	// Continuations is how many not_yet verdicts continue the conversation (L2): 0 to 5, default 2.
	Continuations *int32 `json:"continuations,omitempty"`
	// Judge is the completion judge's alias, or none for no judge (L2): default tier-light.
	Judge string `json:"judge,omitempty"`
}

// The loop policy's defaults (loops design, "The policy, in the factory config") and the
// harness's bounds (loop policy harness R16), which the XRD repeats.
const (
	DefaultStallSteps    = 80
	DefaultContinuations = 2
	DefaultJudge         = "tier-light"
	NoJudge              = "none"
	MaxStallSteps        = 500
	MaxContinuations     = 5
)

// On reports whether the row switches the loop policy on.
func (l Loop) On() bool { return l.StallSteps != nil || l.Continuations != nil || l.Judge != "" }

// LoopPolicy is a class's loop policy, every value resolved; JudgeModel "" is no judge.
type LoopPolicy struct {
	StallSteps, Continuations int32
	JudgeModel                string
}

// LoopFor is class's loop policy, nil when its loop is off: its row, else default's.
func (c *Config) LoopFor(class string) *LoopPolicy {
	row, ok := c.Loops[class]
	if !ok {
		row = c.Loops["default"]
	}
	if !row.On() {
		return nil
	}
	p := &LoopPolicy{StallSteps: DefaultStallSteps, Continuations: DefaultContinuations, JudgeModel: DefaultJudge}
	if row.StallSteps != nil {
		p.StallSteps = *row.StallSteps
	}
	if row.Continuations != nil {
		p.Continuations = *row.Continuations
	}
	switch row.Judge {
	case "":
	case NoJudge:
		p.JudgeModel = ""
	default:
		p.JudgeModel = row.Judge
	}
	return p
}

// validateLoops: a row's key is default, review or a class some repository declares; its values
// are within the harness's bounds; and only cloud-native-ref's AgentRun carries spec.loop.
func (c *Config) validateLoops(bad func(string, ...any)) {
	for _, name := range slices.Sorted(maps.Keys(c.Loops)) {
		row := c.Loops[name]
		if name != "default" && name != "review" && !slices.ContainsFunc(c.Repos(), func(r Repository) bool {
			_, ok := r.Classes[name]
			return ok
		}) {
			bad("loops.%s: %q is not default, review or a class a repository declares", name, name)
		}
		if s := row.StallSteps; s != nil && (*s < 1 || *s > MaxStallSteps) {
			bad("loops.%s.stallSteps must be 1..%d", name, MaxStallSteps)
		}
		if n := row.Continuations; n != nil && (*n < 0 || *n > MaxContinuations) {
			bad("loops.%s.continuations must be 0..%d", name, MaxContinuations)
		}
		if j := row.Judge; j != "" && j != NoJudge && !slices.Contains(models(), j) {
			bad("loops.%s.judge %q is none or one of %v", name, j, models())
		}
		if row.On() && c.Runs.Group != runwatch.DefaultKind.Group {
			bad("loops.%s: the loop policy needs cloud-native-ref's AgentRun (runs.group %s): this runtime's has no spec.loop", name, runwatch.DefaultKind.Group)
		}
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race -count=1 ./internal/factory/config/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/factory/config
git commit -m "feat(config): loops, the loop policy per task class, off without a row"
```

### Task 4: The task records its class's row, and every implementer claim carries it

**Files:**
- Modify: `api/factory/v1alpha1/task_types.go`; regenerate `api/factory/v1alpha1/zz_generated.deepcopy.go`, `config/crd/agents.ogenki.io_tasks.yaml`, `charts/agent-factory/crds/agents.ogenki.io_tasks.yaml`
- Modify: `internal/factory/runs/runs.go`, `internal/factory/reconciler/reconciler.go`, `internal/factory/reconciler/implement.go`
- Create: `internal/factory/reconciler/loop_test.go`
- Test: `internal/factory/runs/runs_test.go`

**Interfaces:**
- Consumes: Task 3's `LoopFor`.
- Produces: `v1alpha1.Loop{StallSteps, Continuations int32; JudgeModel string}` at `TaskSpec.Loop`;
  `runs.Loop` at `runs.Spec.Loop`; the claim's `spec.loop: {stallSteps, continuations[, judgeModel]}`
  (PR 2's XRD fields; `judgeModel` exists since v0.9.17).

- [ ] **Step 1: Write the failing tests**

In `internal/factory/runs/runs_test.go`, add `"reflect"` to the imports and append:

```go
// Loop policy part 2: spec.loop is written only when set, judgeModel only when the task names a judge.
func TestBuildWritesTheLoopOnlyWhenSet(t *testing.T) {
	base := Spec{RunID: "7f3cq2xz", Role: "implementer", Repository: "Smana/cloud-native-ref"}
	if _, found, _ := unstructured.NestedMap(Build(base).Object, "spec", "loop"); found {
		t.Fatal("no loop: no spec.loop (D4)")
	}
	for _, c := range []struct {
		loop Loop
		want map[string]any
	}{
		{Loop{StallSteps: 80, Continuations: 2, JudgeModel: "tier-light"}, map[string]any{"stallSteps": int64(80), "continuations": int64(2), "judgeModel": "tier-light"}},
		{Loop{StallSteps: 40, Continuations: 0}, map[string]any{"stallSteps": int64(40), "continuations": int64(0)}},
	} {
		s := base
		s.Loop = &c.loop
		got, _, err := unstructured.NestedMap(Build(s).Object, "spec", "loop")
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Fatalf("spec.loop = %v (%v), want %v", got, err, c.want)
		}
	}
}
```

`internal/factory/reconciler/loop_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// Loop policy part 2, D4: a class with no row gets today's claim, with no spec.loop.
func TestNoLoopRowIsTodaysRun(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "# Fix the link"))
	tk := g.reconcile(t, "3buqdlot", 3)
	if tk.Spec.Loop != nil || g.runs.specs[rid(0)].Loop != nil {
		t.Fatalf("task %+v, claim %+v: want no loop", tk.Spec.Loop, g.runs.specs[rid(0)].Loop)
	}
}

// loopRig is a rig whose review class (the rig's default) has a loop row read at triage.
func loopRig(t *testing.T) (*rig, *v1alpha1.Task) {
	t.Helper()
	g := newRig(t, issueTask("3buqdlot", 7, "# Fix the link"))
	steps := int32(40)
	g.r.Cfg.Loops = map[string]config.Loop{"review": {StallSteps: &steps}}
	return g, g.reconcile(t, "3buqdlot", 3)
}

// The row read at triage reaches the task and the implementer's claim, every value resolved.
func TestALoopRowReachesTheImplementersClaim(t *testing.T) {
	g, tk := loopRig(t)
	if tk.Spec.Loop == nil || *tk.Spec.Loop != (v1alpha1.Loop{StallSteps: 40, Continuations: 2, JudgeModel: "tier-light"}) {
		t.Fatalf("task loop %+v", tk.Spec.Loop)
	}
	if l := g.runs.specs[rid(0)].Loop; l == nil || *l != (runs.Loop{StallSteps: 40, Continuations: 2, JudgeModel: "tier-light"}) {
		t.Fatalf("claim loop %+v", l)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -count=1 ./internal/factory/runs/ ./internal/factory/reconciler/`
Expected: `undefined: Loop` and `tk.Spec.Loop undefined` (build failures).

- [ ] **Step 3: The Task field**

In `api/factory/v1alpha1/task_types.go`, replace:

```go
	Budget Budget `json:"budget,omitempty"`
}

// Source is where the task came from and how far its text is trusted.
```

with:

```go
	Budget Budget `json:"budget,omitempty"`
	// The loop policy its implementer runs get as spec.loop, resolved from loops.<class> at
	// triage so a config change never moves a task already triaged (loops design L2, L3). Unset:
	// the loop is off for the task's class.
	// +optional
	Loop *Loop `json:"loop,omitempty"`
}

// Loop is a task's loop policy, every value resolved at triage.
type Loop struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=500
	StallSteps int32 `json:"stallSteps"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=5
	Continuations int32 `json:"continuations"`
	// The judge's alias; empty: no judge.
	// +kubebuilder:validation:Enum=agent-default;tier-light;tier-standard;tier-frontier
	// +optional
	JudgeModel string `json:"judgeModel,omitempty"`
}

// Source is where the task came from and how far its text is trusted.
```

Run: `task crd:gen`
Expected: the deepcopy, `config/crd/agents.ogenki.io_tasks.yaml` and the chart's copy gain `loop`
(with `stallSteps` 1–500, `continuations` 0–5, the alias enum, `continuations` and `stallSteps`
required); `api/v1alpha1/zz_generated.deepcopy.go` keeps its SPDX line (the task re-adds it).

- [ ] **Step 4: The claim**

In `internal/factory/runs/runs.go`, replace:

```go
	// Suspendable is spec.suspendable (v2 suspend), written only when true: it is immutable.
	Suspendable bool
}
```

with:

```go
	// Suspendable is spec.suspendable (v2 suspend), written only when true: it is immutable.
	Suspendable bool
	// Loop is spec.loop (loops design L2, L3), written only when set: it is immutable.
	Loop *Loop
}

// Loop is the claim's spec.loop; JudgeModel "" writes no judgeModel, which the composition reads
// as none.
type Loop struct {
	StallSteps, Continuations int32
	JudgeModel                string
}
```

and in `Build`, after:

```go
	if s.Suspendable {
		spec["suspendable"] = true
	}
```

add:

```go
	if l := s.Loop; l != nil {
		loop := map[string]any{"stallSteps": int64(l.StallSteps), "continuations": int64(l.Continuations)}
		if l.JudgeModel != "" {
			loop["judgeModel"] = l.JudgeModel
		}
		spec["loop"] = loop
	}
```

- [ ] **Step 5: Triage and `startRun`**

In `internal/factory/reconciler/reconciler.go`, `received`, after
`t.Spec.Template, t.Spec.PredictedClass, t.Spec.DataClass, t.Spec.Budget = d.Template, d.PredictedClass, d.DataClass, d.Budget`, add:

```go
	t.Spec.Loop = nil
	if p := r.Cfg.LoopFor(d.PredictedClass); p != nil {
		t.Spec.Loop = &v1alpha1.Loop{StallSteps: p.StallSteps, Continuations: p.Continuations, JudgeModel: p.JudgeModel}
	}
```

In `internal/factory/reconciler/implement.go`, `startRun`, after
`s.RunID, s.StartSeq, s.ChargedTo = runID(t.Name, len(t.Status.Runs)), last, r.person(t)`, add:

```go
	// The loop policy closes implementer runs only (L2); every one of them, revisions and CI fixes
	// included, carries the task's policy, read at triage.
	if l := t.Spec.Loop; l != nil && s.Role == "implementer" {
		s.Loop = &runs.Loop{StallSteps: l.StallSteps, Continuations: l.Continuations, JudgeModel: l.JudgeModel}
	}
```

- [ ] **Step 6: Run the tests and the CRD check**

Run: `go test -race -count=1 ./internal/factory/... ./api/... && task crd:check`
Expected: `ok` for every package; `crd:check` exits 0.

- [ ] **Step 7: Commit**

```bash
git add api config/crd charts/agent-factory/crds internal/factory/runs internal/factory/reconciler/reconciler.go \
  internal/factory/reconciler/implement.go internal/factory/reconciler/loop_test.go
git commit -m "feat(factory): a class's loop row, read at triage, on every implementer claim"
```

### Task 5: The end's narration, summary, tier fit and counter

**Files:**
- Modify: `internal/factory/narrate/narrate.go`, `internal/summary/waiting.go`, `internal/factory/reconciler/outcome.go`, `internal/factory/reconciler/reconciler.go` (the `metrics` interface), `internal/factory/reconciler/implement.go`, `internal/factory/fmetrics/fmetrics.go`
- Test: `internal/factory/reconciler/loop_test.go`, `internal/factory/reconciler/reconciler_test.go` (the fake), `internal/factory/reconciler/outcome_test.go`

**Interfaces:**
- Consumes: Task 2's `run_phase` reasons, Task 4's `Task.spec.loop`.
- Produces: `(*fmetrics.Set).LoopRun(ctx, class, end string)` and `agent_factory_loop_runs_total{class, end, repository}`.

- [ ] **Step 1: Write the failing tests**

In `internal/factory/reconciler/reconciler_test.go`, before `func (m *fakeMetrics) Resumed(`, add:

```go
func (m *fakeMetrics) LoopRun(_ context.Context, class, end string) {
	m.add("loop_run " + class + " " + end)
}
```

Append to `internal/factory/reconciler/loop_test.go` (add `"slices"` and `"strings"` to its imports):

```go
// A loop end becomes the task's reason, its narration (the category, no judge prose) and its
// counter, whatever the config says after triage.
func TestALoopEndEscalatesWithItsReason(t *testing.T) {
	prose := map[string]string{"judge_impossible": "cannot be done as asked", "stalled": "made no commit",
		"not_met": "after the last continuation"}
	for end, words := range prose {
		t.Run(end, func(t *testing.T) {
			g, _ := loopRig(t)
			g.r.Cfg.Loops = nil // a config change never moves a triaged task
			g.runs.set(rid(0), "Failed")
			g.log.end(rid(0), "Failed", end)
			tk := g.reconcile(t, "3buqdlot", 1)
			if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != end {
				t.Fatalf("%s %s, want Escalated %s", tk.Status.Phase, tk.Status.Reason, end)
			}
			if c := strings.Join(g.f.Comments(7), "\n"); !strings.Contains(c, words) {
				t.Fatalf("the narration names the category: %q", c)
			}
			if !slices.Contains(g.metrics.recorded, "loop_run review "+end) {
				t.Fatalf("metrics %v", g.metrics.recorded)
			}
		})
	}
}
```

In `internal/factory/reconciler/outcome_test.go`, append:

```go
// Loop policy part 2: a stall or spent continuations is a capability failure after a full attempt;
// a task the judge finds impossible says nothing of the tier.
func TestLoopEndsScoreTheTier(t *testing.T) {
	for reason, want := range map[string]string{"stalled": "under", "not_met": "under", "judge_impossible": ""} {
		if got := Fit(&v1alpha1.Task{Status: v1alpha1.TaskStatus{Phase: v1alpha1.PhaseEscalated, Reason: reason}}); got != want {
			t.Errorf("Fit(%s) = %q, want %q", reason, got, want)
		}
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -count=1 ./internal/factory/reconciler/`
Expected: `TestALoopEndEscalatesWithItsReason` fails on the narration (`Reason` has no prose for the
three) and on the missing `loop_run` line; `TestLoopEndsScoreTheTier` fails on `stalled` and `not_met`.

- [ ] **Step 3: The prose**

In both `internal/factory/narrate/narrate.go` (`reasons()`) and `internal/summary/waiting.go`
(`reasons()`), after the line
`"agent_stuck":               "the agent reported that it was stuck",`, add:

```go
		"judge_impossible":          "the completion judge found the task cannot be done as asked",
		"stalled":                   "the run made no commit within its step limit, or two continuations in a row added none",
		"not_met":                   "the completion judge still found the acceptance checks unmet after the last continuation",
```

- [ ] **Step 4: The tier fit and the counter**

In `internal/factory/reconciler/outcome.go`, replace:

```go
// agent_error is not here: it includes gateway and provider failures, which score nothing (R53).
var underReasons = []string{"stuck", "agent_stuck", "review_rounds_exhausted", "ci_red", "budget-run", "budget-task"}
```

with:

```go
// agent_error is not here: it includes gateway and provider failures, which score nothing (R53).
// Nor is judge_impossible: a task the judge finds impossible says nothing of the tier.
var underReasons = []string{"stuck", "agent_stuck", "review_rounds_exhausted", "ci_red", "budget-run", "budget-task",
	"stalled", "not_met"}
```

In `internal/factory/reconciler/reconciler.go`, add to the `metrics` interface after `RoomClosedAtCap`:

```go
	LoopRun(ctx context.Context, class, end string)
```

In `internal/factory/reconciler/implement.go`, `implementing`, replace:

```go
	current(t).Reason = reason
	r.interventions(ctx, t)
	if infraLost(run) {
```

with:

```go
	current(t).Reason = reason
	r.interventions(ctx, t)
	if t.Spec.Loop != nil && current(t).Role == "implementer" {
		end := "other"
		switch {
		case envelope.LoopEnd(reason):
			end = reason
		case run.Phase == "Succeeded":
			end = "finished"
		}
		record(ctx, func(ctx context.Context) { r.Metrics.LoopRun(ctx, t.Spec.PredictedClass, end) })
	}
	if infraLost(run) {
```

In `internal/factory/fmetrics/fmetrics.go`, add the field `loopRuns        metric.Int64Counter`
after `resumes         metric.Int64Counter`; before
`s.roomsCapped, err = meter.Int64Counter(` add:

```go
	s.loopRuns, err = meter.Int64Counter("agent_factory_loop_runs_total",
		metric.WithDescription("Implementer runs under the loop policy, by class and how they ended: judge_impossible, stalled, not_met, finished or other (loops design L2, L3)."))
	check(err)
```

and before `// RoomClosedAtCap counts`:

```go
// LoopRun counts an implementer run under the loop policy once it ends, by its task's class and end.
func (s *Set) LoopRun(ctx context.Context, class, end string) {
	s.loopRuns.Add(ctx, 1, metric.WithAttributes(attribute.String("class", class),
		attribute.String("end", oneOf(end, []string{"judge_impossible", "stalled", "not_met", "finished"})), s.repoOf(ctx)))
}
```

- [ ] **Step 5: Run the tests**

Run: `go test -race -count=1 ./internal/factory/... ./internal/summary/`
Expected: `ok` for every package (`merge_how_test.go` holds the narration to the summary's reasons:
both maps gained the same three).

- [ ] **Step 6: Commit**

```bash
git add internal/factory internal/summary
git commit -m "feat(factory): escalate a loop end under its reason, narrate its category, count it"
```

### Task 6: Docs, the whole check, the pull request

**Files:** `docs/event-envelope.md`, `docs/concepts.md`, `docs/integration.md`, `docs/factory.md`, `docs/operations.md`

- [ ] **Step 1: The docs**

`docs/event-envelope.md`, after the `harness_paused` row:

```markdown
| `harness_end` | `reason`: `judge_impossible`, `stalled` or `not_met`, the loop policy's end of an implementer run, from agent-run's final read; recorded once per value | Bridge |
```

`docs/concepts.md`, in the **End reason** row, replace
``, `room_busy` (the room refused the run, which never started), `` with
``, `room_busy` (the room refused the run, which never started), `judge_impossible`, `stalled`, `not_met` (the loop policy ended a failed implementer run: the bridge's `harness_end`, read before the harness status), ``.

`docs/integration.md`, in the `HEALTH_ADDR` row, after `within 3 s (`504` past it).`, insert:
`Its body may be `{"endReason": "<reason>"}` (at most 256 bytes), the reason one of `judge_impossible`, `stalled` and `not_met`: the bridge records it once as `state_changed{harness_end}` before it reads the log.`
(No `|` inside the row: GitHub splits a table cell on it, code span or not.)

`docs/factory.md`:
- in the escalation reasons table, after the `agent_error`, `agent_stuck` row:

  ```markdown
  | `judge_impossible`, `stalled`, `not_met` | Under the [loop policy](#the-config-file) (`loops`): the completion judge found the task impossible, the run made no commit within its step limit, or the judge still found the acceptance checks unmet after the last continuation. The issue gets the category; the judge's text stays in the room |
  ```
- in the Task CRD table, after the `spec.budget` row:

  ```markdown
  | `spec.loop` | `stallSteps`, `continuations`, `judgeModel` (empty: no judge): the class's `loops` row, resolved at triage; every implementer run of the task gets it as `spec.loop`. Unset: the loop is off |
  ```
- in the config table, after the `sandbox` row:

  ```markdown
  | `loops` | `<class>: {stallSteps (1–500), continuations (0–5), judge (an alias or none)}`, keyed by a declared class, `review` or `default` (every class without a row). A row is read whole, never merged with `default`'s; it switches the class's [loop policy](https://github.com/Smana/ai-native-ref/blob/main/docs/superpowers/specs/2026-10-21-loops-and-teams-design.md) on when it sets any of the three, and an unset one is 80, 2 or `tier-light`. Needs cloud-native-ref's `AgentRun` (`runs` left at its default) | None: every run as before |
  ```

`docs/operations.md`, after the `agent_factory_resumes_total` row:

```markdown
| `agent_factory_loop_runs_total` | counter | `class`, `end`, `repository` | Implementer runs under the loop policy as they end: `judge_impossible`, `stalled`, `not_met`, `finished`, `other`. The wandering share is `stalled` over all |
```

- [ ] **Step 2: The whole check**

With Docker: `task check`. Expected: exit 0.

Without it: run every step of `task check` but `test` (`task spdx tidy lint vuln crd:check chart:check migrations docs scripts ui:test ui:check`), then
`go test -race -count=1 $(go list ./... | grep -v internal/store)` and `go vet ./internal/store/`.
Expected: each exits 0; the Go tests print `ok` for 48 packages.

- [ ] **Step 3: Rebase, push, open the pull request**

```bash
git fetch origin && git rebase origin/main
git push -u origin feat/loop-policy-factory
```

Title: `feat: the loop policy, part 2: the end reason in the room and the factory's loops rows, off by default`.
Body (English): the outcome; the switch (R1–R5) and that no row ships (D4); the end's path
(R7–R9) and the counter (R10); that part 1's harness sends the body and this PR reads it, in either
merge order (R13); the proofs with their counts; that `TestLastHarnessEnd` ran only in CI when
pushed without Docker. No attribution line. Wait for `check` and `analyze`: `gh pr checks --watch`.

- [ ] **Step 4: Release** (the owner's step, after part 1 and this PR are merged)

The next agent-platform release (`v0.17.0` if none came between; Task 8 and Task 10 use the tag it
really gets) carries the harness, the bridge, the broker and the factory.

---

## PR 2 — crossplane-configuration

### Task 7: `spec.loop.stallSteps`, `spec.loop.continuations` and `AGENT_LOOP`

**Files** (crossplane-configuration):
- Modify: `apis/agentrun/definition.yaml`, `apis/agentrun/kcl/main.k`, `examples/agentrun-complete.yaml`, `tests/golden/agentrun-complete.yaml`
- Regenerate: `apis/agentrun/composition.yaml`
- Test: `apis/agentrun/kcl/main_test.k`

**Interfaces:**
- Consumes: v0.9.17's `spec.loop.judgeModel` and `_judgeModel` (`spec.loop.judgeModel or "none"`).
- Produces: the harness container's `AGENT_LOOP`, exactly `json.encode(<stallSteps and continuations
  as present> | {judgeModel: <spec.loop.judgeModel or none>}, sort_keys=True)`, present iff the
  claim has `spec.loop` (part 1's R1).

- [ ] **Step 1: The worktree**

```bash
git -C /home/smana/Sources/crossplane-configuration fetch origin
git -C /home/smana/Sources/crossplane-configuration worktree add /home/smana/Sources/crossplane-configuration-loop-policy -b feat/agentrun-loop-policy origin/main
cd /home/smana/Sources/crossplane-configuration-loop-policy && mise trust mise.toml
```

- [ ] **Step 2: Write the failing test**

In `apis/agentrun/kcl/main_test.k`, after `test_proxy_pins_the_run_model`'s closing `}`, add
(`kcl fmt`'s layout):

```kcl
# ---- Loop policy part 2: the claim's spec.loop reaches agent-run as AGENT_LOOP, only when present ----
_harnessEnv = lambda spec: any -> any {
    {e.name: e.value for e in _pod(_run(spec)).containers[0].env}
}

test_loop_reaches_the_harness = lambda {
    assert "AGENT_LOOP" not in _harnessEnv({}), "no spec.loop: today's run"
    assert _harnessEnv({
        loop = {}
    }).AGENT_LOOP == '{"judgeModel": "none"}', "an empty loop: the harness's defaults, and no judge the proxy would refuse"
    assert _harnessEnv({
        loop = {stallSteps = 40, continuations = 0}
    }).AGENT_LOOP == '{"continuations": 0, "judgeModel": "none", "stallSteps": 40}'
    _full = _harnessEnv({
        loop = {stallSteps = 80, continuations = 2, judgeModel = "tier-light"}
    })
    assert _full.AGENT_LOOP == '{"continuations": 2, "judgeModel": "tier-light", "stallSteps": 80}'
    assert _proxyEnv({
        loop = {stallSteps = 80, judgeModel = "tier-light"}
    }).JUDGE_MODEL == "tier-light", "the harness and the proxy name one judge"
}
```

- [ ] **Step 3: Run it and watch it fail**

```bash
cd apis/agentrun/kcl && kcl test . -Y settings-example.yaml 2>&1 | grep -E 'test_loop|PASS:|FAIL:'; cd -
```

Expected: `test_loop_reaches_the_harness: FAIL`, `FAIL: 1/110`.

- [ ] **Step 4: The composition**

In `apis/agentrun/kcl/main.k`, after `_judgeModel = _spec.loop?.judgeModel or "none"`, add:

```kcl

    # The loop policy (ai-native-ref loops design L2, L3): agent-run reads the claim's spec.loop as
    # AGENT_LOOP, judgeModel always named so the harness and the proxy agree, and only when the
    # claim carries one: without it the harness runs as before (agent-platform harness R1).
    _loopEnv = [{
        name = "AGENT_LOOP"
        value = json.encode({k: v for k, v in _spec.loop if k in ["stallSteps", "continuations"]} | {judgeModel = _judgeModel}, sort_keys=True)
    }] if "loop" in _spec else []
```

and in the harness container, replace
`+ _footerEnv + _bridgeEnv + _gitEnv + ([{name = "AGENT_SUSPENDABLE", value = "true"}] if _suspendable else [])`
with
`+ _footerEnv + _bridgeEnv + _gitEnv + ([{name = "AGENT_SUSPENDABLE", value = "true"}] if _suspendable else []) + _loopEnv`.

- [ ] **Step 5: The XRD**

In `apis/agentrun/definition.yaml`, replace the `loop` property:

```yaml
                loop:
                  description: >-
                    The run's loop policy (ai-native-ref loops design L2). judgeModel is the alias the
                    completion judge calls through the identity proxy's /judge/v1 path, which the proxy pins to
                    it (capability profiles P4); unset, the gateway refuses that path.
                  type: object
                  properties:
                    judgeModel:
                      type: string
                      enum: [agent-default, tier-light, tier-standard, tier-frontier]
```

with:

```yaml
                loop:
                  description: >-
                    The run's loop policy (ai-native-ref loops design L2, L3), set by the factory on an
                    implementer of a class whose loop is on. The harness gets the whole object as
                    AGENT_LOOP. judgeModel is the alias the completion judge calls through the identity
                    proxy's /judge/v1 path, which the proxy pins to it (capability profiles P4); unset,
                    the gateway refuses that path and the harness calls no judge.
                  type: object
                  properties:
                    judgeModel:
                      type: string
                      enum: [agent-default, tier-light, tier-standard, tier-frontier]
                    stallSteps:
                      description: The run ends stalled with no commit by this many steps (L3); the harness's default is 80.
                      type: integer
                      minimum: 1
                      maximum: 500
                    continuations:
                      description: How many not_yet verdicts continue the conversation (L2); the harness's default is 2.
                      type: integer
                      minimum: 0
                      maximum: 5
```

`spec.loop` stays immutable as a whole (v0.9.17's rule `has(self.loop) == has(oldSelf.loop) && …`):
the factory writes it at CREATE only.

- [ ] **Step 6: The example and the golden**

In `examples/agentrun-complete.yaml`, replace:

```yaml
  loop:
    judgeModel: tier-light
```

with:

```yaml
  loop:
    stallSteps: 80
    continuations: 2
    judgeModel: tier-light
```

In `tests/golden/agentrun-complete.yaml`, replace:

```yaml
        - name: BRIDGE_URL
          value: http://127.0.0.1:8085
        image: ghcr.io/smana/agent-harness:
```

with:

```yaml
        - name: BRIDGE_URL
          value: http://127.0.0.1:8085
        - name: AGENT_LOOP
          value: '{"continuations": 2, "judgeModel": "tier-light", "stallSteps": 80}'
        image: ghcr.io/smana/agent-harness:
```

The three other goldens' claims have no `spec.loop` and do not change.

- [ ] **Step 7: Generate and run the checks**

```bash
(cd apis/agentrun/kcl && kcl fmt .) && task generate && git add apis examples tests
task generate-sync test schema 2>&1 | grep -E 'test_loop|PASS:|FAIL|REJECTED|valid against'; echo "exit=${PIPESTATUS[0]}"
```

Expected: `test_loop_reaches_the_harness: PASS`, `PASS: 110/110` for agentrun,
`REJECTED settings-reject-internal-egress.yaml`, `25 example claim(s) valid`, `exit=0`.
`task render` needs Docker. Without it, compare the harness container of each example rendered by
`kcl run` with its golden:

```bash
cat > "$SCRATCH/golden_harness.py" <<'PY'
"""golden_harness.py <xpc-root>: renders each AgentRun example through main.k with `kcl run` (crossplane render needs
Docker here) and compares the harness container with the golden's, field for field."""
import pathlib, subprocess, sys, tempfile, yaml
root = pathlib.Path(sys.argv[1])
fails = 0
for ex in sorted((root / "examples").glob("agentrun-*.yaml")):
    claim = next(d for d in yaml.safe_load_all(ex.read_text()) if d)
    dxr = {"apiVersion": claim["apiVersion"], "kind": claim["kind"], "metadata": {"name": claim["metadata"]["name"], "namespace": "agents"}}
    settings = {"kcl_options": [{"key": "params", "value": {"oxr": claim, "ocds": {}, "dxr": dxr}}]}
    with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as f:
        yaml.safe_dump(settings, f)
    out = subprocess.run(["kcl", "run", "main.k", "-Y", f.name], cwd=root / "apis/agentrun/kcl", capture_output=True, text=True)
    if out.returncode:
        print("ERROR", ex.name, out.stderr[-300:]); fails += 1; continue
    items = yaml.safe_load(out.stdout)["items"]
    pick = lambda docs: next(c for d in docs if d and d.get("kind") == "Sandbox" for c in d["spec"]["podTemplate"]["spec"]["containers"] if c["name"] == "harness")
    got, gold = pick(items), pick(yaml.safe_load_all((root / "tests/golden" / ex.name).read_text()))
    ok = got == gold
    fails += not ok
    print("MATCH " if ok else "DIFFER", ex.name, [e for e in got["env"] if e["name"] == "AGENT_LOOP"])
sys.exit(1 if fails else 0)
PY
python3 "$SCRATCH/golden_harness.py" .
```

Expected: four `MATCH` lines, `AGENT_LOOP` in `agentrun-complete.yaml` only. The comparison is on
parsed values; CI's `task render` holds the bytes, quoting included.

- [ ] **Step 8: Commit**

```bash
git add apis/agentrun examples/agentrun-complete.yaml tests/golden/agentrun-complete.yaml
git commit -m "feat(agentrun): spec.loop's stall limit and continuations, and AGENT_LOOP for the harness"
```

### Task 8: Pin the agent-platform release's harness and room-bridge, release

**Files** (crossplane-configuration): `apis/agentrun/kcl/main.k`, `apis/agentrun/kcl/main_test.k`,
`apis/agentrun/composition.yaml`, the four goldens. The shape of #54.

- [ ] **Step 1: The digests** (after Task 6, Step 4)

```bash
V=v0.17.0   # the release Task 6 Step 4 really tagged
for img in agent-harness room-bridge; do
  echo "$img $(skopeo inspect --format '{{.Digest}}' docker://ghcr.io/smana/$img:$V)"
done
```

- [ ] **Step 2: The pins**

Replace every `ghcr.io/smana/agent-harness:v0.15.2@sha256:<old>` and
`ghcr.io/smana/room-bridge:v0.15.2@sha256:<old>` (the two pins in `main.k`, the test that names the
harness image, the goldens) with the new tag and digest:

```bash
grep -rln 'smana/agent-harness:v0\.15\.2\|smana/room-bridge:v0\.15\.2' apis tests
```

Then `task generate`, `git add`, and Step 7 of Task 7's checks. Expected: the same lines, `exit=0`,
four `MATCH`.

- [ ] **Step 3: Commit, push, open PR 2**

```bash
git commit -am "feat(agentrun): pin agent-platform $V harness and room-bridge, the loop policy"
git fetch origin && git rebase origin/main && git push -u origin feat/agentrun-loop-policy
```

Title: `feat(agentrun): the loop policy's claim fields, AGENT_LOOP, and the harness and bridge that read them`.
Body: the outcome; R6, R11, R12 and R13 (inert until the factory writes `spec.loop`, which it does
only from a row); the checks; that CI's `task render` must match the goldens. No attribution line.

- [ ] **Step 4: Release** (the owner's step, after the merge): tag the next release, `v0.9.18` if
none came between.

---

## PR 3 — cloud-native-ref

### Task 9: Pin the release

As the P2–P4 plan's Task 8 (PR H), with that release: worktree
`/home/smana/Sources/cloud-native-ref-xpc-loop-policy`, branch `chore/crossplane-configuration-loop-policy`,
the four files that name the crossplane-configuration version
(`grep -rn 'crossplane-configuration-.*:v0\.9\.\|--branch=v0\.9\.' infrastructure/base/crossplane apps/platform/app-wizard/app.yaml`),
the host's own checks, the commit
`chore(deps): pin crossplane-configuration v0.9.18, the loop policy's claim fields`, the PR (body:
`spec.loop.stallSteps`, `spec.loop.continuations`, `AGENT_LOOP`, the agent-platform harness and
bridge of that release, inert until a factory row), no attribution line.

---

## PR 4 — ai-native-ref

### Task 10: Pin the release, move `HOST_REF`, document the switch

**Files** (ai-native-ref, worktree from `origin/main`, `mise trust mise.toml`):
- `flux/sources/ocirepo-agent-factory.yaml` and `tooling/base/agent-factory/helm-values-configmap.yaml`: the chart and image of the release (tag and digest move together: `test-agent-factory-pins.py`).
- `infrastructure/base/room-broker/app.yaml`, `crd-rooms.yaml` and the retention CronJob's image: the broker of the release, which accepts `harness_end`.
- `HOST_REF`: a host commit holding PR 3.
- `website/content/docs/platform/ai-platform/agents/harness.md`: a "Loop policy" section (the switch, `AGENT_LOOP`, the three ends and where each shows: room, Task, issue, counter).
- The factory page (`website/content/docs/platform/ai-platform/…`, the page that documents the factory config): the `loops` key, with "no row ships (D4)".
- `website/content/docs/platform/ai-platform/status.md#roadmap`: feature 2 built, off.

No `loops` key in the values: D4. The values must still parse under the pinned release's
`config.Parse` (the suite that checks it, as the scoper's gate item 7 names).

- [ ] **Step 1: The pins and `HOST_REF`**, with the digests read as in Task 8 Step 1 for
  `agent-factory`, the chart, and `room-broker`.
- [ ] **Step 2: The docs**, in the site's style (short declarative sentences, tables).
- [ ] **Step 3: The check:** `bash $SCRATCH/check.sh <worktree>`. Expected: exit 0, the pins suite
  `PASS`, links and the site build pass.
- [ ] **Step 4: Commit, push, open PR 4:** `chore(deps): agent-platform <release> and crossplane-configuration v0.9.18, the loop policy off by default`.
  Merge only once the host's Flux has applied PR 3 on the running clusters, or with none running.

### Task 11: The live check (the next platform session, after the offline proofs)

Run it on gcp-0 with PRs 1–4 applied. N is part 1's `scripts/stall-steps.sh` (80 until 20 accepted
runs have a first commit).

- [ ] **Step 1: Switch one class on.** In a branch of ai-native-ref, add to the factory's values
  `loops: {docs: {stallSteps: <N>, judge: none}}` (the judge waits for P4's PR B on the cluster;
  with it applied, `judge: tier-light`). Merge; wait for the factory's rollout.
- [ ] **Step 2: Read the run.** On a real `docs` issue labelled `factory/ready`:

```bash
kubectl -n agent-system get task <id> -o jsonpath='{.spec.loop}'; echo
kubectl -n agents get agentrun xplane-run-<run> -o jsonpath='{.spec.loop}'; echo
kubectl -n agents get sandbox xplane-run-<run>-sandbox -o jsonpath='{.spec.podTemplate.spec.containers[?(@.name=="harness")].env[?(@.name=="AGENT_LOOP")].value}'; echo
```

Expected: the same three values each time; a `review` issue's claim has no `spec.loop`.
- [ ] **Step 3: Read the end.** When a run ends by the policy, the room shows
  `state_changed{harness_end}` then `run_phase{Failed, <reason>}`, the task is `Escalated` with that
  reason, the issue's comment names the category only, and
  `agent_factory_loop_runs_total{class="docs"}` counts it.
- [ ] **Step 4: Switch it off.** Remove the row. A task triaged meanwhile keeps its policy (R4); a
  new one has none.

## Self-review against the spec

| Spec | Where |
|---|---|
| "How a run ends": `judge_impossible`, `stalled`, `not_met` → `Escalated` with the reason | Tasks 1, 2 (bridge, broker), Task 5 (`TestALoopEndEscalatesWithItsReason`), R8, R9 |
| "On the issue: the fixed narration with the reason category only" | Task 5 (the prose; the judge's text never reaches the factory) |
| "Room: `state_changed{run_phase}` with the reason" | Task 2 (runwatch's `run_phase`); the notes are part 1's |
| `spec.loop: {stallSteps, continuations, judgeModel}` on the claim, turned into the harness's environment | Task 4 (the claim), Task 7 (the XRD and `AGENT_LOOP`); `JUDGE_MODEL` is v0.9.17's |
| "The policy, in the factory config": `loops.<class>` and `loops.default` | Task 3, R1–R3 |
| "Loop policy … the factory config … reaches runs through a Flux release" | Task 10 |
| D4: off by default, per class | R1, R13, `TestNoLoopRowIsTodaysRun`, Task 10 ships no row |
| The scoper's `loops.<class>.scope` joins without a break | R2, Task 3's `Loop` |
| Gate: "Wandering share … down" | R10's counter; F9's panel |
| L4–L9 | Out: F1–F6 |
