# Code Health Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the 2026-10-15 review's medium findings, split the four files that outgrew one head, make the test suite fast and timer-free, and remove dead code, one pinned test per fix.

**Architecture:** No design changes. Each theme is one pull request; each finding is one commit whose first step is the test that fails today. File splits move code within a package and change no exported API.

**Tech Stack:** Go 1.27 (`task check`: spdx, tidy, golangci-lint v2, govulncheck, `go test -race`), testcontainers for `internal/store`, controller-runtime's fake client, Helm 4 and `yq` for the chart test.

**Spec:** [`docs/superpowers/specs/2026-10-15-code-health-design.md`](../specs/2026-10-15-code-health-design.md)

## Global Constraints

- `AGENTS.md` rules hold for every change: `ctx` first, `%w` wrapping, `errors.Is`/`As`, `log/slog` injected, no package-level state beyond `version.Version`, no `time.Sleep` outside tests, one egress client (`internal/httpx`), SQL only in `internal/store` and parameterised, the broker role append-only, no payload text in logs or errors.
- stdlib `testing` only; table-driven `t.Run`; hand-written fakes; `-race` always.
- Every new `.go` file opens with `// SPDX-License-Identifier: Apache-2.0`.
- A migration is never edited once released: add a new file and re-hash with `atlas migrate hash --dir file://internal/store/migrations`; list it in `internal/store/migrations/kustomization.yaml`.
- `task check` exits 0 before every commit; conventional commits, one concern each; one pull request per theme (Tasks 1-2, 3-7, 8-12, 13-17, 18-23, 24-29, 30-34, 35-40, 41-47).

## Review Focus

1. A run whose first typed reading arrives after the ledger has failed for several ticks: the run must still be revoked at its USD cap on the first tick the reading lands, and the ledger must settle on the next successful tick without double-counting. Pinned in Task 2.
2. A `hello` from a bridge whose run the informer has not yet seen, while a live holder owns the lease: the lease must stay with the holder and the newcomer must get `409 room_busy`, not take over. Pinned in Task 8.
3. A fork whose source prefix holds a queued message that was later consumed (after the fork point): the fork's queue must hold the message (it was queued at the fork point), and the source's later consume must not leak into it. Pinned in Task 11.
4. The store suite on a template database: a test that forges a sealed room must not see another test's rows, and a test that drops its database while a pool connection is open must not hang `TestMain`. Pinned in Task 35.
5. `OnStatus` under back-pressure with a `human`-class action pending: the bridge must not answer it locally; it stays `waiting_for_confirmation` until the broker's decision arrives after the outage. Pinned in Task 13.

---

## Theme 1: money and revocation (`internal/factory/meter`)

### Task 1: Pin the gateway's token-series contract

**Files:**
- Modify: `internal/factory/meter/vm.go:95-122` (`RunUsage` and the `Usage` doc), `internal/factory/meter/meter.go:124-134` (`cost`)
- Test: `internal/factory/meter/vm_test.go`
- Create: `internal/factory/meter/testdata/query_reply.json`

**Interfaces:**
- Produces: `Usage`'s doc comment states, once: "`Input` is the gateway's `input` series, which **includes** `cached_input` and `cache_creation_input`; `cost` subtracts them to price fresh input."

- [ ] **Step 1: Record a reply.** From a live run (the owner supplies it), save the JSON of `GET api/v1/query` for the meter's query with all four `gen_ai_token_type` values for one run into `testdata/query_reply.json`. Until the owner provides it, write the fixture by hand from `docs/operations.md:195-197`'s stated semantics: `input` 1,000,000, `cached_input` 800,000, `cache_creation_input` 50,000, `output` 100,000, and mark the file's first line `"_note": "hand-written from docs/operations.md; replace with a recorded reply"`.

- [ ] **Step 2: Write the failing test** in `vm_test.go`:

```go
// The gateway's input series includes the cache tokens (docs/operations.md, run and task budgets):
// cost prices the difference as fresh input. A reply read from testdata pins the contract end to end.
func TestCostFollowsTheGatewaySeries(t *testing.T) {
	raw, err := os.ReadFile("testdata/query_reply.json")
	if err != nil {
		t.Fatal(err)
	}
	usage, err := parseUsage(raw) // the function RunUsage calls on the HTTP body; export it for the test if needed
	if err != nil {
		t.Fatal(err)
	}
	u := usage["cf4ato2x"]
	if u.Input != 1_000_000 || u.CacheRead != 800_000 || u.CacheWrite != 50_000 || u.Output != 100_000 || !u.Typed {
		t.Fatalf("usage %+v", u)
	}
	p := config.Price{Input: 1.40, CacheRead: 0.26, CacheWrite: 1.40, Output: 4.40}
	// fresh 150,000 × 1.40 + 800,000 × 0.26 + 50,000 × 1.40 + 100,000 × 4.40 = 928,000 micro-USD
	if got := cost(p, u); got != 928_000 {
		t.Fatalf("cost = %d micro-USD, want 928000", got)
	}
}
```

- [ ] **Step 3: Run it**: `go test ./internal/factory/meter/ -run TestCostFollowsTheGatewaySeries`. Expected: FAIL (fixture missing, or `parseUsage` unexported).

- [ ] **Step 4: Implement**: factor the body parsing of `RunUsage` into `parseUsage(body []byte) (map[string]Usage, error)` (unexported, same file), used by `RunUsage`; write the fixture; rewrite `Usage`'s doc comment as above and delete any other sentence in the package that restates the series' meaning.

- [ ] **Step 5: Run the package**: `go test -race -count=1 ./internal/factory/meter/...`. Expected: PASS.

- [ ] **Step 6: Commit**:

```bash
git add internal/factory/meter
git commit -m "test(meter): pin the gateway's token series and the cost formula on one recorded reply"
```

### Task 2: Revocations do not wait for the ledger

**Files:**
- Modify: `internal/factory/meter/meter.go:393-420` (`Tick`)
- Test: `internal/factory/meter/meter_test.go`

- [ ] **Step 1: Write the failing test** (use the file's existing fakes: a `fakeRuns` store with an `Annotate` recorder and a `fakeLedger` whose `settle` returns an error when `fail` is set; add the field if absent):

```go
// The per-run cap is always enforced: a ledger that cannot be written delays the daily budget,
// never a run's own revocation (review 2026-10-15, F2).
func TestARunIsRevokedEvenWhenTheLedgerFails(t *testing.T) {
	runs := &fakeRuns{runs: []runs.Run{{ID: "cf4ato2x", Principal: "human:a", Phase: "Running", MaxTokens: 1000, Model: "agent-default"}}}
	src := fakeSource{usage: map[string]Usage{"cf4ato2x": {Tokens: 1500, Input: 1500, Typed: true}}}
	ledger := &fakeLedger{fail: errors.New("configmap busy")}
	m := &Meter{Runs: runs, Source: src, Ledger: ledger, Now: time.Now, Log: slog.New(slog.DiscardHandler)}
	err := m.Tick(context.Background())
	if err == nil {
		t.Fatal("the ledger failure is still reported")
	}
	if got := runs.annotations["cf4ato2x"][runs.AnnRevoked]; got != ReasonBudgetRun {
		t.Fatalf("revoked = %q, want %q: the run cap waited for the ledger", got, ReasonBudgetRun)
	}
}
```

- [ ] **Step 2: Run it**. Expected: FAIL (`revoked = ""`).

- [ ] **Step 3: Implement**: in `Tick`, move the `settle` call and the `pending` bookkeeping after the revocation loop; keep `spent` for `budget-principal` by reading it from the ledger first with a read-only call (`m.Ledger.Spent(ctx, day)`; add it to the ledger interface and fake) so `revocation` still sees the day's spend; on a settle error, return `errors.Join(errs…, err)` after the loop. Review focus 1: add a second case to the test where `fail` is cleared on the second `Tick` and assert the ledger's recorded delta equals the increase once, not twice (`pending` is applied only after a successful settle, as today).

- [ ] **Step 4: Run**: `go test -race -count=1 ./internal/factory/meter/...`. Expected: PASS.

- [ ] **Step 5: Commit**:

```bash
git add internal/factory/meter
git commit -m "fix(meter): a run's cap is enforced before the day's ledger is settled"
```

---

## Theme 2: defaults, gates and comparisons

### Task 3: The permission matrix defaults closed for system actors

**Files:**
- Modify: `internal/policy/policy.go:91-112`
- Test: `internal/policy/policy_test.go`

- [ ] **Step 1: Write the failing test**:

```go
// Every action has an explicit answer for every actor kind: nothing is granted by omission (T7).
func TestEveryActionHasARowPerKind(t *testing.T) {
	all := []Action{Read, Chat, Queue, Steer, Interrupt, Decide, DriverRequest, DriverGive, DriverTake, PromoteQueued,
		RemoveQueued, StartRun, Fork, Invite, Close}
	sys := Subject{Kind: envelope.ActorSystem, ID: "system:factory"}
	for _, a := range all {
		if Allowed(sys, a) && !slices.Contains([]Action{Read, Chat, Queue, StartRun, Fork, Close}, a) {
			t.Errorf("a system caller is granted %v by default", a)
		}
	}
}
```

(The allowed list is what the wired callers use today: `internal/bridgeapi/system.go` reads, posts `task_state`, queues, and the factory starts runs, forks nothing and closes finished rooms. Check each against `grep -rn 'policy.Allowed' internal/bridgeapi internal/factory` before finalising the list, and amend the test.)

- [ ] **Step 2: Run it**: `go test ./internal/policy/`. Expected: FAIL for `Invite`, `RemoveQueued`, …

- [ ] **Step 3: Implement**: replace `default: return true` with `case Read, Chat, Queue, StartRun, Fork, Close: return true` and `default: return false`.

- [ ] **Step 4: Run**: `go test -race -count=1 ./internal/policy/... ./internal/bridgeapi/... ./internal/humanapi/... ./internal/factory/...`. Expected: PASS (a failure in a wired caller names an action to add to the explicit list; add it to both the code and the test).

- [ ] **Step 5: Commit**:

```bash
git add internal/policy
git commit -m "fix(policy): a system actor is granted nothing by default"
```

### Task 4: Logins compare case-insensitively everywhere

**Files:**
- Create: `internal/factory/forge/login.go`
- Modify: `internal/factory/reconciler/merge.go:35,212,401,543`, `internal/factory/reconciler/arm.go:90,100`
- Test: `internal/factory/forge/login_test.go`, `internal/factory/reconciler/merge_test.go`

- [ ] **Step 1: Write the failing tests.** `login_test.go`:

```go
func TestSameLoginIgnoresCase(t *testing.T) {
	for _, tc := range []struct{ a, b string; want bool }{
		{"Merger-Bot[bot]", "merger-bot[bot]", true}, {"smana", "Smana", true}, {"smana", "smana2", false}, {"", "", true},
	} {
		if got := SameLogin(tc.a, tc.b); got != tc.want {
			t.Errorf("SameLogin(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}
```

In `merge_test.go`, copy the existing case that merges as the merger App and asserts `AutoMerged` (search `AutoMerged`), and change the fake PR's `MergedBy` to the config login with its first letter upper-cased; assert `AutoMerged == true` and the task phase `Verifying`, not `Done`.

- [ ] **Step 2: Run them**. Expected: FAIL.

- [ ] **Step 3: Implement** `login.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package forge

import "strings"

// SameLogin compares GitHub logins, which GitHub treats as case-insensitive; every login
// comparison in the factory goes through it (review 2026-10-15, F7).
func SameLogin(a, b string) bool { return strings.EqualFold(a, b) }
```

Replace each `== r.Cfg.Merge.MergerLogin`, `== r.Cfg.Merge.PolicyBotLogin`, `== r.Cfg.AgentsLogin` (and the `!=` forms) in `merge.go` and `arm.go` with `forge.SameLogin(x, y)`; `grep -n 'Login ==\|Login !=\|== r.Cfg.*Login' internal/factory` must print nothing.

- [ ] **Step 4: Run**: `go test -race -count=1 ./internal/factory/...`. Expected: PASS.

- [ ] **Step 5: Commit**:

```bash
git add internal/factory
git commit -m "fix(factory): compare GitHub logins case-insensitively at the merge gate"
```

### Task 5: The chart's RBAC says what it bounds, and the test checks structure

**Files:**
- Modify: `charts/agent-factory/templates/rbac.yaml:17-29`, `charts/agent-factory/tests/render.sh`

- [ ] **Step 1: Write the failing assertion** in `render.sh`, replacing the `grep -F` lines around `:48-51` with `yq`:

```bash
rendered=$(helm template agent-factory charts/agent-factory --set image.tag='v0.0.0@sha256:0000')
role=$(printf '%s' "$rendered" | yq eval-all 'select(.kind == "Role" and .metadata.namespace == null)' -)
# One configmaps rule, bounded by the namespace and saying so; no resourceNames rule that a wider one subsumes.
n=$(printf '%s' "$role" | yq '[.rules[] | select(.resources[] == "configmaps")] | length' -)
[ "$n" = 1 ] || { echo "want one configmaps rule, got $n"; exit 1; }
printf '%s' "$role" | yq -e '.rules[] | select(.resources[] == "configmaps") | .verbs | sort == ["create","delete","get","list","update","watch"]' - >/dev/null
```

- [ ] **Step 2: Run it**: `bash charts/agent-factory/tests/render.sh`. Expected: FAIL (`want one configmaps rule, got 2`).

- [ ] **Step 3: Implement**: delete the `resourceNames: [agent-factory-stop]` rule and its comment; merge `watch` into the remaining rule's verbs (the stop object's cache needs it); rewrite the comment: "ConfigMaps: the stop object, R50's day ledgers (names carry the date, so resourceNames cannot bound them) and nothing else. The Role's namespace is the bound: the factory can read and write every ConfigMap in it, its own config included, so the namespace holds no ConfigMap it must not touch."

- [ ] **Step 4: Run**: `task chart:check`. Expected: PASS.

- [ ] **Step 5: Commit**:

```bash
git add charts/agent-factory
git commit -m "fix(chart): one ConfigMap rule that says the namespace is its bound; render.sh asserts structure"
```

### Task 6: A heredoc body is scanned for forge writes under every head

**Files:**
- Modify: `internal/bridge/classify.go:330-350` (`segment`'s final scan)
- Test: `internal/bridge/classify_battery_test.go:134` (the residual case)

- [ ] **Step 1: Flip the battery case**: in `classify_battery_test.go`, find the case whose command is `cat <<'EOF' > x.sh` … `git push origin main` … `EOF` … `bash x.sh` marked as a residual (`Plain`); change its expected class to `forge.push` and remove the residual marker.

- [ ] **Step 2: Run it**: `go test ./internal/bridge/ -run TestBattery`. Expected: FAIL (classified `Plain`).

- [ ] **Step 3: Implement**: in `segment`, where `mentionsForge` scans the head's words, also scan `s.bodies` (the heredoc bodies already parsed) for every head, not only interpreters: `for _, b := range s.bodies { if class := mentionsForge(b); class != Plain { return class } }` placed before the final `return Plain`.

- [ ] **Step 4: Run**: `go test -race -count=1 ./internal/bridge/...`. Expected: PASS.

- [ ] **Step 5: Commit**:

```bash
git add internal/bridge
git commit -m "fix(bridge): a forge write inside a heredoc is classified whatever runs the file"
```

### Task 7: Blank text is refused on the system message route as on the queue

**Files:**
- Modify: `internal/bridgeapi/system.go:148`
- Test: `internal/bridgeapi/server_test.go` (next to the `/messages` tests)

- [ ] **Step 1: Write the failing test**: `POST /v1/rooms/{id}/messages` with `{"kind":"task_state","text":"   ","clientSeq":1}` as a system caller; expect `400` and `bad_message`.
- [ ] **Step 2: Run it**. Expected: FAIL (`201`).
- [ ] **Step 3: Implement**: `if strings.TrimSpace(in.Text) == "" { fail(w, http.StatusBadRequest, wire.ReasonBadMessage); return }` where the upper bound is checked.
- [ ] **Step 4: Run**: `go test -race -count=1 ./internal/bridgeapi/...`. Expected: PASS.
- [ ] **Step 5: Commit**: `git commit -am "fix(bridgeapi): a blank task_state is refused like a blank queued message"`.

---

## Theme 3: liveness, leases and idempotency

### Task 8: An unknown run is not a dead run

**Files:**
- Modify: `internal/runwatch/watcher.go:118-122`, `internal/bridgeapi/server.go:297-300`
- Test: `internal/runwatch/watcher_test.go`, `internal/bridgeapi/server_test.go`

**Interfaces:**
- Produces: `func (w *Watcher) Live(id string) (run Run, live, known bool)`; the `Liveness` interface in `bridgeapi` changes accordingly; every fake implementing it (the compiler lists them) returns `known`.

- [ ] **Step 1: Write the failing tests.** `watcher_test.go`:

```go
func TestLiveTellsUnknownFromDead(t *testing.T) {
	w := newTestWatcher(t) // the file's constructor
	if _, live, known := w.Live("nosuchrn"); live || known {
		t.Fatalf("unknown run: live %v known %v", live, known)
	}
	w.upsert(Run{ID: "deadrun1", Phase: "Failed"}) // the file's way of seeding a run
	if _, live, known := w.Live("deadrun1"); live || !known {
		t.Fatalf("terminal run: live %v known %v", live, known)
	}
}
```

`server_test.go` (review focus 2): a hello from run B while run A holds the lease and the fake `Liveness` answers `known=false` for A: expect `409 room_busy` and the lease still A's.

- [ ] **Step 2: Run them**. Expected: FAIL to compile.

- [ ] **Step 3: Implement**: `Live` returns the three values; in `hello`, the callback becomes `func(_ context.Context, id string) bool { _, live, known := s.Watch.Live(id); return live || !known }` with the comment "an unknown holder is kept: this replica's cache may lag the API server (review 2026-10-15, B8)". Update the other call sites `grep -rn '\.Live(' internal` lists.

- [ ] **Step 4: Run**: `go test -race -count=1 ./internal/runwatch/... ./internal/bridgeapi/... ./internal/app/... ./internal/mcp/...`. Expected: PASS.

- [ ] **Step 5: Commit**: `git commit -am "fix(bridgeapi): a lease holder this replica has not seen yet keeps its lease"`.

### Task 9: Approval requests use the server's clock and a replayable key

**Files:**
- Modify: `internal/bridgeapi/approvals.go:110-114`
- Test: `internal/bridgeapi/approvals_test.go`

- [ ] **Step 1: Write the failing test**: post the same approval request twice (same `eventId`) with `Server.Now` fixed; assert both answers carry the same `approvalId` and that the fake log recorded one draft whose `OriginSeq` equals the second call's. Expected today: two different `OriginSeq` values.
- [ ] **Step 2: Run it**. Expected: FAIL.
- [ ] **Step 3: Implement**: `ExpiresAt: s.now().Add(…)`; `OriginSeq: originSeq(in.EventID)` where `originSeq` is `int64(fnv.New64a().Sum64(eventID) >> 1)` (positive, stable). Add a doc line: "the draft's key is the harness event id, so a retried request replays its event (review 2026-10-15, B7)".
- [ ] **Step 4: Run**: `go test -race -count=1 ./internal/bridgeapi/...`. Expected: PASS.
- [ ] **Step 5: Commit**: `git commit -am "fix(bridgeapi): an approval request keys its event on the harness event id and the server clock"`.

### Task 10: The room policy seam carries the request context

**Files:**
- Modify: `internal/bridgeapi/server.go:105`, `internal/bridgeapi/approvals.go:57-62`, `internal/app/approvals.go:99-110`, their tests

- [ ] **Step 1: Change the type**: `RoomPolicy func(ctx context.Context, roomID string) wire.ApprovalPolicy`; pass `r.Context()` at both call sites; in `internal/app/approvals.go`, `roomPolicy` uses the given `ctx` with `policyTimeout` as a child deadline instead of `context.Background()`.
- [ ] **Step 2: Add a test** in `internal/app/approvals_test.go`: the policy reader blocks until its ctx ends; call with an already-cancelled ctx; assert it returns within the test's timeout with the attended default.
- [ ] **Step 3: Run**: `go test -race -count=1 ./internal/bridgeapi/... ./internal/app/...`. Expected: PASS.
- [ ] **Step 4: Commit**: `git commit -am "fix(bridgeapi): the room policy lookup runs under the request's context"`.

### Task 11: A fork keeps the messages still queued at its point

**Files:**
- Modify: `internal/store/fork.go:86-104`
- Test: `internal/store/fork_test.go`

- [ ] **Step 1: Write the failing test** (review focus 3):

```go
func TestAForkCarriesTheQueueAtItsPoint(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := context.Background()
	q := appendQueued(t, s, room, "address L42") // the file's helper for message{delivery: queued}, else write one with Append
	fork, err := s.Fork(ctx, ForkRequest{Source: room, At: q.Seq, New: "bbbbbbb2", Owner: "human:o"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume(ctx, room, []int64{q.Seq}, "cf4ato2x"); err != nil { // the source consumes after the fork point
		t.Fatal(err)
	}
	got, err := s.Queue(ctx, fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Ref != q.Seq {
		t.Fatalf("fork queue %+v: want the message queued at the fork point, untouched by the source's consume", got)
	}
}
```

- [ ] **Step 2: Run it**. Expected: FAIL (empty queue).
- [ ] **Step 3: Implement**: in the fork transaction, after the events copy, `INSERT INTO queue (room_id, ref, author, text, state) SELECT $2, ref, author, text, 'queued' FROM queue WHERE room_id = $1 AND ref <= $3 AND state = 'queued'` (match the table's real columns from `20261001120000_driver_queue.sql`); the trigger `queue_is_its_event` validates each row against the copied events.
- [ ] **Step 4: Run**: `go test -race -count=1 ./internal/store/...`. Expected: PASS.
- [ ] **Step 5: Commit**: `git commit -am "fix(store): a fork carries the messages still queued at its point"`.

### Task 12: `/factory retry` tolerates clock skew

**Files:**
- Modify: `internal/factory/reconciler/watch.go:276-277`
- Test: `internal/factory/reconciler/watch_test.go`

- [ ] **Step 1: Write the failing test**: a task escalated at `T`, a maintainer's `/factory retry` comment stamped `T - 30s` by GitHub; expect the retry to be honoured.
- [ ] **Step 2: Run it**. Expected: FAIL.
- [ ] **Step 3: Implement**: `const retrySkew = time.Minute`; compare `c.At.After(t.Status.PhaseSince.Add(-retrySkew))` with a comment naming the two clocks.
- [ ] **Step 4: Run**: `go test -race -count=1 ./internal/factory/reconciler/...`. Expected: PASS.
- [ ] **Step 5: Commit**: `git commit -am "fix(factory): a retry comment within a minute of the escalation counts"`.

---

## Theme 4: bounds

### Task 13: The bridge answers local verdicts under back-pressure

**Files:**
- Modify: `internal/bridge/bridge.go:580-610` (`step`, `pollEvents`), the `MaxBuffer` doc at `:266-270`
- Test: `internal/bridge/confirm_test.go`

- [ ] **Step 1: Write the failing test** (and review focus 5):

```go
// A broker outage never stops the sandbox (design §3): with the buffer full, the bridge still
// reads the harness and answers Allow and Deny itself; only a human decision waits for the broker.
func TestAFullBufferStillAnswersLocalVerdicts(t *testing.T) {
	h := newFakeHarness(t) // the file's harness fake
	b := newTestBridge(t, h, brokerDown()) // the file's constructors; MaxBuffer: 1 << 10
	h.emit(bigEvents(2 << 10)) // past MaxBuffer
	b.step(ctx)               // buffers what it can
	h.pending("git status", "LOW")
	h.status("waiting_for_confirmation")
	b.step(ctx)
	b.step(ctx)
	if got := h.confirmations(); len(got) != 1 || got[0].accept != true {
		t.Fatalf("confirmations %+v: want the allow-class action accepted locally", got)
	}
	h.pending("git push origin main", "HIGH")
	h.status("waiting_for_confirmation")
	b.step(ctx)
	if got := h.confirmations(); len(got) != 1 {
		t.Fatalf("a human-class action was answered without the broker: %+v", got)
	}
}
```

Adapt the helpers to the file's actual fakes (`grep -n '^func new\|^func (f \*fakeHarness)' internal/bridge/confirm_test.go`).

- [ ] **Step 2: Run it**. Expected: FAIL (no confirmation while the buffer is full).

- [ ] **Step 3: Implement**: in `step`, call `OnStatus` whenever `b.lastStatus != ""` and the events poll either caught up **or** was skipped for back-pressure (`pollEvents` returns a second value `skipped bool`); in `pollEvents`, when `bufBytes >= MaxBuffer`, still call `b.Harness.next` for the confirmation path's benefit but discard mapped items and leave the cursor where it is (the harness keeps its store), returning `(false, true)`. Document in `MaxBuffer`'s comment: "Past the cap the bridge stops buffering, not reading: local policy verdicts are still answered; human decisions wait for the broker."

- [ ] **Step 4: Run**: `go test -race -count=1 ./internal/bridge/...`. Expected: PASS, including `TestTheWorstCaseHeapFitsTheMemoryLimit`.

- [ ] **Step 5: Commit**: `git commit -am "fix(bridge): a full buffer stops buffering, not the local policy verdicts"`.

### Task 14: `EventsSince` keeps only what the caller wants

**Files:**
- Modify: `internal/factory/rooms/client.go:185-206`, its callers (`grep -rn 'EventsSince(' internal/factory`)
- Test: `internal/factory/rooms/client_test.go`

- [ ] **Step 1: Write the failing test**: a fake broker serving 300 events of which 3 match `keep`; call `EventsSince(ctx, room, 0, keep)`; assert 3 returned and the cursor at seq 300.
- [ ] **Step 2: Run it**. Expected: FAIL to compile.
- [ ] **Step 3: Implement**: `func (c *Client) EventsSince(ctx context.Context, room string, afterSeq int64, keep func(envelope.Event) bool) ([]envelope.Event, int64, error)`; append only kept events; `nil` keep means all. `roomTail` passes its existing filter.
- [ ] **Step 4: Run**: `go test -race -count=1 ./internal/factory/...`. Expected: PASS.
- [ ] **Step 5: Commit**: `git commit -am "fix(factory): the rooms client retains only the events its caller keeps"`.

### Task 15: A subscriber's channel is small; the budget is the bound

**Files:**
- Modify: `internal/fanout/hub.go:145` and the `Budget` doc
- Test: `internal/fanout/hub_test.go`

- [ ] **Step 1: Write the failing test**: `cap(sub.C) <= 256` after `Subscribe`.
- [ ] **Step 2: Run it**. Expected: FAIL (`4096`).
- [ ] **Step 3: Implement**: `const subChan = 256`; `make(chan envelope.Event, subChan)`; the `Budget` doc says "the bound on bytes pending per subscriber; the channel holds at most 256 events and a full one counts as slow". Run the slow-consumer tests: they must still pass (the `Dropped` path triggers on the budget, not the channel).
- [ ] **Step 4: Run**: `go test -race -count=1 ./internal/fanout/... ./internal/humanapi/... ./internal/bridgeapi/...`. Expected: PASS.
- [ ] **Step 5: Commit**: `git commit -am "fix(fanout): a viewer's channel holds 256 events; Budget is the bound"`.

### Task 16: Retention purges in ranges

**Files:**
- Modify: `internal/store/retention.go:60-61`
- Test: `internal/store/retention_test.go`

- [ ] **Step 1: Write the failing test**: a sealed, expired room with 25,000 events (append in a loop through the store's own `Append`; set `statement_timeout` to `200ms` on the retention connection for the test through the URL's `options=-c statement_timeout=200ms`); `Purge` must delete every row and return 25,000.
- [ ] **Step 2: Run it**. Expected: FAIL (`canceling statement due to statement timeout`), or pass too quickly on a fast machine: in that case raise the count until a single `DELETE` exceeds 200 ms, then fix.
- [ ] **Step 3: Implement**: `const purgeBatch = 10_000`; loop `DELETE FROM events WHERE room_id = $1 AND seq <= $2 AND room_id IN (…expired…)` with `$2` stepping from `purgeBatch` to the room's `last_seq`, summing `RowsAffected`, inside the same transaction.
- [ ] **Step 4: Run**: `go test -race -count=1 ./internal/store/...`. Expected: PASS.
- [ ] **Step 5: Commit**: `git commit -am "fix(store): retention deletes a room's events in ranges under the statement timeout"`.

### Task 17: Lock scopes in the hub and the GitHub App client

**Files:**
- Modify: `internal/fanout/hub.go:309-326` (`fetch`), `internal/github/app.go:283-337` (`installation`)
- Test: `internal/fanout/hub_test.go`, `internal/github/app_test.go`

- [ ] **Step 1: Write the failing tests**: (a) in `hub_test.go`, a subscriber whose `offer` blocks (a full channel and a slow `Dropped` drain) while `notify` is called from another goroutine: `notify` must return within 100 ms (use a channel and `select` with a timer, no sleep as the assertion). (b) in `app_test.go`, two installations for two repositories minted concurrently against a fake GitHub that delays 200 ms per mint: total wall time under 350 ms.
- [ ] **Step 2: Run them**. Expected: FAIL (b at ~400 ms; a may pass or fail depending on scheduling: keep it as a guard).
- [ ] **Step 3: Implement**: `fetch` copies `rm.subs` under `h.mu`, unlocks, offers, re-locks only to advance `rm.last`. `installation` keys a `sync.Mutex` per repository in a `map[string]*sync.Mutex` guarded by `a.mu` for the map alone; the token mint runs under the per-repo lock.
- [ ] **Step 4: Run**: `go test -race -count=1 ./internal/fanout/... ./internal/github/...`. Expected: PASS.
- [ ] **Step 5: Commit**: `git commit -am "fix(fanout,github): offer pages and mint tokens outside the shared lock"`.

---

## Theme 5: seams and the egress client

### Task 18: The C7 classifier goes through `httpx`

**Files:**
- Modify: `internal/factory/triage/c7.go:50-75`, `internal/app/factory.go:163`
- Test: `internal/factory/triage/c7_test.go`, `internal/app/factory_test.go`

- [ ] **Step 1: Write the failing tests**: `HTTPClassifier{URL: u}.Classify(ctx, req)` with `HC == nil` returns an error naming the missing client; a reply body of 2 MiB is refused with `httpx`'s size error when `HC` is set.
- [ ] **Step 2: Run them**. Expected: FAIL.
- [ ] **Step 3: Implement**: `if h.HC == nil { return C7Response{}, errors.New("classifier: no HTTP client") }`; read the body with `httpx.ReadBody(resp.Body, maxC7Reply)` (`const maxC7Reply = 64 << 10`) then `json.Unmarshal`. In `internal/app/factory.go`, build `triage.HTTPClassifier{URL: cfg.Triage.ClassifierURL, HC: httpx.New(httpx.Options{Timeout: 3 * time.Second})}` (match `httpx.New`'s real signature: `grep -n '^func New' internal/httpx/*.go`).
- [ ] **Step 4: Run**: `go test -race -count=1 ./internal/factory/triage/... ./internal/app/...` and `task lint`. Expected: PASS.
- [ ] **Step 5: Commit**: `git commit -am "fix(factory): the C7 classifier calls through the audited egress client with a bounded reply"`.

### Task 19: The read-only MCP allowlist comes from the environment

**Files:**
- Modify: `internal/bridge/classify.go:76-100`, `cmd/room-bridge` or `internal/app/bridge.go` (where `EGRESS_PROFILES` is read), `docs/integration.md` (the bridge environment table)
- Test: `internal/bridge/classify_test.go`, `internal/app/bridge_test.go`

- [ ] **Step 1: Write the failing test**: `NewClassifier(ClassifierOptions{ReadOnlyMCP: []string{"flux-operator-mcp__get_flux_instance"}})` classes that tool as read and `mcp-victorialogs__query` as `mcp.write`; the zero options classify every MCP tool as `mcp.write`.
- [ ] **Step 2: Run it**. Expected: FAIL (the table is a package var).
- [ ] **Step 3: Implement**: the table becomes a field on the classifier, built from `ClassifierOptions.ReadOnlyMCP []string` (`server__tool` names); `internal/app` reads `MCP_READ_ONLY_TOOLS` (comma-separated) from the bridge's environment, as `EGRESS_PROFILES`; the platform's list moves to the composition (`docs/integration.md` documents the variable and lists today's values as the platform's defaults). Delete the `var readOnlyMCP` literal.
- [ ] **Step 4: Run**: `go test -race -count=1 ./internal/bridge/... ./internal/app/...`. Expected: PASS.
- [ ] **Step 5: Commit**: `git commit -am "fix(bridge): the read-only MCP tool set is configuration, not a platform constant"`.

### Task 20: The kill switch has no package state

**Files:**
- Modify: `internal/factory/killswitch/killswitch.go:28-45`, `internal/factory/intake/issues.go:153`
- Test: `internal/factory/killswitch/killswitch_test.go`

- [ ] **Step 1: Write the failing test**: with a fake reader holding no stop object, `Engaged` is false even after a `PublishIssue(…, on=true)` to a different client; with the object present, true. (The test fails today because `SetIssue(true)` would make `Engaged` true regardless of the reader; delete `SetIssue` and the test enforces that `Engaged` reads only the object.)
- [ ] **Step 2: Implement**: delete `issueStop`, `SetIssue`, and the `issueStop.Load()` branch; delete the `killswitch.SetIssue(on)` call in `issues.go` (the poller already calls `PublishIssue`).
- [ ] **Step 3: Run**: `go test -race -count=1 ./internal/factory/...`. Expected: PASS.
- [ ] **Step 4: Commit**: `git commit -am "fix(factory): the kill switch reads the published stop object only"`.

### Task 21: Duplicated seam constants derive from their owners

**Files:**
- Modify: `internal/factory/meter/vm.go:27`, `internal/app/factory.go:193-195`, `internal/factory/intake/schedule.go:166`, `internal/factory/config/config.go` (two keys), `charts/agent-factory/values.yaml` (the config example comment)
- Test: `internal/factory/config/config_test.go`

- [ ] **Step 1: Write the failing config test**: a config with `api.groups: {admin: agents-admin, member: agents-member}` and `schedules[0].probe.author: renovate[bot]` loads; one missing `api.groups.member` fails `Validate` with a message naming the key.
- [ ] **Step 2: Implement**: `API.Groups struct{ Admin, Member string }` and `Schedule.Probe.Author string` in `config.go` with defaults `agents-admin`/`agents-member`/`renovate[bot]` in `ApplyDefaults` (so existing files keep working) and non-empty checks in `Validate`; `internal/app/factory.go` reads them; `schedule.go` reads the author from its schedule. In `vm.go`, `runSA = regexp.MustCompile("^system:serviceaccount:" + runs.Namespace + ":" + runwatch.ClaimPrefix + "([a-z2-7]{8})$")` (confirm both exported names exist; export them if not, with doc comments).
- [ ] **Step 3: Run**: `go test -race -count=1 ./internal/factory/... ./internal/app/...`; `task chart:check`. Expected: PASS.
- [ ] **Step 4: Commit**: `git commit -am "refactor(factory): deployment facts come from config; the run SA pattern from runwatch's seam"`.

### Task 22: A streaming client is a named thing

**Files:**
- Modify: `internal/httpx/httpx.go` (new constructor), `internal/app/roomctl.go:67-68`
- Test: `internal/httpx/httpx_test.go`

- [ ] **Step 1: Write the failing test**: `httpx.NewStreaming(opts).Timeout == 0` and the redirect cap and metadata refusal still apply (reuse the existing tests' fake server for the metadata address).
- [ ] **Step 2: Implement**: `NewStreaming` builds through `New` and clears `Timeout`, with a doc comment: "for a WebSocket or SSE exchange, whose length no timeout fits; every other invariant of New holds". `roomctl.go` calls it instead of mutating.
- [ ] **Step 3: Run**: `go test -race -count=1 ./internal/httpx/... ./internal/app/...`. Expected: PASS.
- [ ] **Step 4: Commit**: `git commit -am "refactor(httpx): a streaming client is constructed, not mutated"`.

### Task 23: `replayPage` is a field

**Files:**
- Modify: `internal/bridgeapi/server.go:53-55` and its uses, `internal/bridgeapi/server_test.go` (where the test sets it)

- [ ] **Step 1: Implement**: `ReplayPage int // deliveries a stream's replay reads at once; 0 means 500` on `Server`; `s.replayPage()` returns it or 500; the test sets the field on its `Server`. Delete the package var.
- [ ] **Step 2: Run**: `go test -race -count=1 ./internal/bridgeapi/...`. Expected: PASS.
- [ ] **Step 3: Commit**: `git commit -am "refactor(bridgeapi): the replay page size is a Server field"`.

---

## Theme 6: robustness and logs

### Task 24: `bridgeapi.Server` refuses to start without its dependencies

**Files:**
- Modify: `internal/bridgeapi/tls.go:113-132` (`ListenAndServeTLS`)
- Test: `internal/bridgeapi/tls_test.go`

- [ ] **Step 1: Write the failing test**: `(&Server{}).ListenAndServeTLS(ctx, …)` returns an error containing "Log, Redactor, Runs, Systems and Watch are required" and opens no listener.
- [ ] **Step 2: Implement**: the same check `humanapi.Server.Serve` has (`server.go:168-171`), first thing in `ListenAndServeTLS`.
- [ ] **Step 3: Run**: `go test -race -count=1 ./internal/bridgeapi/...`. Expected: PASS.
- [ ] **Step 4: Commit**: `git commit -am "fix(bridgeapi): the server refuses to start without its required dependencies"`.

### Task 25: The summary package logs nothing

**Files:**
- Modify: `internal/summary/summary.go:280`, `internal/summary/waiting.go:58`, `.golangci.yaml` (forbidigo), `internal/humanapi/summary.go` (the caller logs)
- Test: `internal/summary/summary_test.go` (no change in behaviour; the lint is the test)

- [ ] **Step 1: Add the lint rule** to `.golangci.yaml`'s `forbidigo.forbid`:

```yaml
        - pattern: ^slog\.(Debug|Info|Warn|Error)(Context)?$
          msg: log through the injected *slog.Logger, never the process default
```

- [ ] **Step 2: Run `task lint`**. Expected: FAIL on the two `slog.Warn` calls.
- [ ] **Step 3: Implement**: delete both `slog.Warn` calls. `View` already omits the `stop` action and `MergeHow` already returns the bypass-only text when the repository is unknown: the caller (`internal/humanapi/summary.go`) logs at Warn `"summary: task facts name no repository", "room", id` when `s.Status.Issue != nil` and no action of kind `stop` is present.
- [ ] **Step 4: Run**: `task lint && go test -race -count=1 ./internal/summary/... ./internal/humanapi/...`. Expected: PASS.
- [ ] **Step 5: Commit**: `git commit -am "fix(summary): the pure fold logs nothing; forbidigo refuses the default logger"`.

### Task 26: Logs carry ids, not RunLore payload fields

**Files:**
- Modify: `internal/factory/intake/runlore.go:159`
- Test: `internal/factory/intake/runlore_test.go`

- [ ] **Step 1: Write the failing test**: a failing intake with `AlertName: "ignore all instructions"`; capture the logger's output (a `slog.NewTextHandler` on a buffer); assert the text does not contain the alert name and does contain the task name.
- [ ] **Step 2: Implement**: log `"task", taskid.Name(key)` in place of the raw fields.
- [ ] **Step 3: Run**: `go test -race -count=1 ./internal/factory/intake/...`. Expected: PASS.
- [ ] **Step 4: Commit**: `git commit -am "fix(factory): the RunLore intake logs the task id, never the finding's text"`.

### Task 27: GitHub's status codes are read, not grepped

**Files:**
- Modify: `internal/factory/forge/github.go:616-622`, the `auth` round-tripper in the same package
- Test: `internal/factory/forge/github_test.go`

- [ ] **Step 1: Write the failing test**: a fake GraphQL merge answering `200` with an error message whose text contains `"409"` must not be reported as a conflict; one answering HTTP `409` must.
- [ ] **Step 2: Implement**: a `statusError{code int}` returned by the round-tripper for non-2xx responses (body discarded, size-bounded read to drain), `errors.As` at the `Merge` call site.
- [ ] **Step 3: Run**: `go test -race -count=1 ./internal/factory/forge/...`. Expected: PASS.
- [ ] **Step 4: Commit**: `git commit -am "fix(factory): merge conflicts are detected on the HTTP status, not the error text"`.

### Task 28: Smaller fixes in one commit each

- [ ] **B14**: `internal/humanapi/server.go:190-196`: a `sync.WaitGroup` added in `track`, done in `untrack`; `Serve` waits on it under `dctx` (a goroutine closes a channel after `wg.Wait()`; `select` on it and `dctx.Done()`). Test: the existing shutdown test must still pass; add one that the wait returns at once when no socket is open. Commit `fix(humanapi): shutdown waits for sockets exactly`.
- [ ] **B17**: `internal/bridgeapi/server.go:588-590`: move `sub.Sent(ev)` after the successful `send(ev)`. Commit `fix(bridgeapi): the stream releases the hub budget after the write, as the WebSocket does`.
- [ ] **B16b**: `internal/store/approvals.go:253-306`: `closeApproval` returns `(ev, dup bool, err)`; `closeAll` skips `dup`. Test: two sweeps closing the same approval yield one event in `out`. Commit `fix(store): a replayed close is not reported as a new event`.
- [ ] **B15d**: `internal/store/driver.go:84,111`: `appendTx` takes `checked bool`; `changeDriver` passes `true`. Commit `refactor(store): one idempotency lookup per driver change`.
- [ ] **B27**: `internal/brief/brief.go:74,100`, `internal/mcp/tools.go:208`: compile the pull-request pattern once per repository in a small `sync.Map` cache keyed by repository. Commit `perf(brief,mcp): compile the PR pattern once per repository`.

### Task 29: Theme 6 gate

- [ ] `task check` exits 0; open the theme's pull request.

---

## Theme 7: structure

### Task 30: Split `internal/bridge/classify.go`

**Files:**
- Create: `internal/bridge/classify_tables.go`, `internal/bridge/shlex.go`, `internal/bridge/policy.go`
- Modify: `internal/bridge/classify.go`

- [ ] **Step 1: Record the baseline**: `go test -count=1 ./internal/bridge/ > /tmp/bridge-before.txt`.
- [ ] **Step 2: Move**: lines 62-250 (the allowlist tables and their builders) to `classify_tables.go`; 942-1146 (the lexer: `lex`, `readDelim`, `bodies`, `word`, `segment`'s helpers) to `shlex.go`; 1148-1202 (`Decide`, `Stricter`, the profile tables) to `policy.go`. Each new file opens with the SPDX line and `package bridge`; `goimports` fixes imports. No identifier changes.
- [ ] **Step 3: Verify**: `task lint && go test -count=1 ./internal/bridge/ > /tmp/bridge-after.txt && diff <(grep -E '^(ok|---|PASS|FAIL)' /tmp/bridge-before.txt) <(grep -E '^(ok|---|PASS|FAIL)' /tmp/bridge-after.txt)`. Expected: no differences but the timings.
- [ ] **Step 4: Commit**: `git commit -am "refactor(bridge): classify.go splits into tables, the shell lexer and the policy matrix"`.

### Task 31: Split `internal/bridge/bridge.go`

- [ ] Move lines 101-254 (batch encoding, stubs, `Retry-After`, backoff) to `batch.go` and 636-712 (the final read) to `finalread.go`; the SSE consumer (954-992) joins `stream.go` if the package has one, else stays. Verify as in Task 30. Commit `refactor(bridge): the loop file keeps Run, step, flush and shutdown`.

### Task 32: Split `internal/humanapi/acts.go`

- [ ] Move `startRun`, `busy`, `validRun`, `forkPoint` (587-665 and their helpers) to `startrun.go`; `fork` (682-747) to `fork.go`. Verify with the package's tests. Commit `refactor(humanapi): start_run and fork live with their tests`.

### Task 33: Split `internal/bridgeapi/server.go`

- [ ] Move `events` (349-428) to `events.go` and `stream` (486-597) to `stream.go`. Verify. Commit `refactor(bridgeapi): the events push and the SSE stream in their own files`.

### Task 34: The forge fake leaves the production build

**Files:**
- Create: `internal/factory/forge/forgetest/fake.go`
- Modify: every test importing `forge.Fake` (`grep -rln 'forge.Fake\|&forge.Fake' internal`)

- [ ] **Step 1: Move** `internal/factory/forge/fake.go` to `forgetest/fake.go` (`package forgetest`), exporting `Fake` and its constructors; update the imports.
- [ ] **Step 2: In `internal/factory/reconciler/watch_test.go:884`**, replace the `Calls()` order assertion with state assertions (which PR was commented, which check re-run), keeping one ordering check if the order is the contract (say so in the comment).
- [ ] **Step 3: Verify**: `go build ./... && go test -race -count=1 ./internal/factory/...`; `go list -deps ./cmd/agent-factory | grep forgetest` prints nothing.
- [ ] **Step 4: Commit**: `git commit -am "refactor(factory): the forge fake is a test package"`.

---

## Theme 8: tests

### Task 35: One Postgres for the store suite

**Files:**
- Modify: `internal/store/testdb_test.go`, `internal/store/store_test.go:20-30`
- Create: `internal/store/main_test.go`

- [ ] **Step 1: Record the baseline**: `time go test -count=1 ./internal/store/` (≈ 207 s).

- [ ] **Step 2: Write `main_test.go`**:

```go
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"os"
	"testing"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// One PostgreSQL for the whole suite: the roles are created once, the migrations run once into a
// template database, and each test gets its own database from that template (review 2026-10-15, B29).
var shared struct{ super, tpl string }

func TestMain(m *testing.M) {
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("postgres"), tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		fmt.Fprintln(os.Stderr, "store tests: start postgres:", err)
		os.Exit(1)
	}
	shared.super, _ = c.ConnectionString(ctx, "sslmode=disable")
	if err := bootstrap(shared.super); err != nil { // roles, the rooms_tpl database, every migration
		fmt.Fprintln(os.Stderr, "store tests: bootstrap:", err)
		_ = c.Terminate(ctx)
		os.Exit(1)
	}
	code := m.Run()
	_ = c.Terminate(ctx)
	os.Exit(code)
}
```

- [ ] **Step 3: Rewrite `testDB`**: `bootstrap` does what `testDB` did (roles once; `CREATE DATABASE rooms_tpl OWNER rooms_owner`; the migrations against `rooms_tpl` with the owner URL pointing at it; then `UPDATE pg_database SET datallowconn = false WHERE datname = 'rooms_tpl'`, which is how a template stays untouched). `testDB(t)` becomes: `name := "t_" + strings.ToLower(regexp.MustCompile("[^a-z0-9]+").ReplaceAllString(t.Name(), "_"))` truncated to 50 and suffixed with a random 6-char base32; `exec(t, super, "CREATE DATABASE " + pgx.Identifier{name}.Sanitize() + " TEMPLATE rooms_tpl OWNER rooms_owner")`; `t.Cleanup` runs `DROP DATABASE … WITH (FORCE)` (PostgreSQL 13+ ends open sessions: review focus 4); return the three role URLs and the super URL with `dbname=name` swapped in (`url.Parse`, set `u.Path = "/" + name`). The identifier goes through `pgx.Identifier.Sanitize`, never string concatenation of the raw name.

- [ ] **Step 4: Run**: `time go test -race -count=1 ./internal/store/...`. Expected: PASS, well under 30 s. `forge()` still works: it takes the test's super URL.

- [ ] **Step 5: Commit**: `git commit -am "test(store): one PostgreSQL for the suite, a template database per test"`.

### Task 36: `humanapi` has a clock

**Files:**
- Modify: `internal/humanapi/server.go` (`Now func() time.Time` on `Server`), `ws.go:190`, `server.go:217`, `summary.go:58`, `acts.go:464`
- Test: `internal/humanapi/ws_test.go:1280` (`TestTheLifetimeCap`), `internal/humanapi/access_test.go:556`

- [ ] **Step 1: Add the field** with the doc bridgeapi uses (`server.go:122-124`), and `s.now()` returning it or `time.Now`. Replace the four direct calls.
- [ ] **Step 2: Rewrite the two tests** to step a fake clock (`now := time.Date(…); s.Now = func() time.Time { return now }`), advancing `now` past `MaxLifetime` and past the access TTL plus one ping, and triggering the ping path directly where the test used `time.Sleep`.
- [ ] **Step 3: Run**: `go test -race -count=1 ./internal/humanapi/...` three times. Expected: PASS each time, faster than before.
- [ ] **Step 4: Commit**: `git commit -am "test(humanapi): an injectable clock replaces the real timers in the lifetime and access tests"`.

### Task 37: The bridge's negative assertions step a ticker

**Files:**
- Modify: `internal/bridge/bridge.go` (`Ticker func(d time.Duration) (<-chan time.Time, func())` as `bridgeapi.Server.Ticker`), `internal/bridge/bridge_test.go:446,490,513,618,654`, `internal/bridge/confirm_test.go:704,755,791`

- [ ] **Step 1: Add the field** and use it in `Run`'s loop; `nil` means `time.NewTicker`.
- [ ] **Step 2: Rewrite the eight sleeps** as ticks on a test channel: `tick <- time.Now()` then an assertion on the fake harness's counters, with no `time.Sleep`.
- [ ] **Step 3: Run**: `go test -race -count=1 ./internal/bridge/...` three times. Expected: PASS; `grep -c 'time.Sleep' internal/bridge/*_test.go` prints 0.
- [ ] **Step 4: Commit**: `git commit -am "test(bridge): the loop ticks on an injected ticker; no sleeps in the negative assertions"`.

### Task 38: Hooks replace the remaining sleeps

- [ ] `internal/bridgeapi/server_test.go:1058`, `internal/bridgeapi/deliver_test.go:160`, `internal/fanout/hub_test.go:455,500`, `internal/app/bridge_test.go:86`: give each fake a channel it signals on the event the test waits for (a read, an offer, a subscribe) and `select` on it with a 2 s timer that fails the test. Commit `test: fakes signal the events the tests wait on`.

### Task 39: The summary goldens update by a flag

**Files:**
- Modify: `internal/summary/summary_test.go:20-43`

- [ ] **Step 1: Add** `var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/*.json want fields from the current fold")`; in `TestFoldGolden`, when set, marshal `got` back into the file's `want` and write the file, then pass.
- [ ] **Step 2: Run** `go test ./internal/summary/ -update-golden` then `git diff --exit-code internal/summary/testdata`. Expected: no diff (the goldens are current).
- [ ] **Step 3: Commit**: `git commit -am "test(summary): the goldens are rewritten only by -update-golden"`.

### Task 40: Assertions on effects, not call strings

- [ ] `internal/app/approvals_test.go:30-90`: assert which approvals were closed and the gauge value; keep one check that supersede runs before expiry, with a comment saying why the order is the contract. Commit `test(app): the sweep test asserts effects`.
- [ ] `charts/agent-factory/tests/render.sh`: every remaining `grep -F` on rendered YAML becomes a `yq` assertion on a path (Task 5 did the RBAC ones). Commit `test(chart): render.sh asserts structure with yq`.

---

## Theme 9: hygiene

### Task 41: Untrack the Python caches

- [ ] `git rm --cached -r images/agent-harness/__pycache__ images/agent-harness/site/__pycache__ images/agent-harness/tests/__pycache__`; append `__pycache__/` and `*.pyc` to `.gitignore`; `git ls-files '*.pyc'` prints nothing. Commit `chore(images): untrack Python caches`.

### Task 42: Drop the dead `bridge_seen_at`

**Files:**
- Create: `internal/store/migrations/20261016120000_drop_bridge_seen_at.sql`
- Modify: `internal/store/bridges.go:48,80,89`, `internal/store/migrations/kustomization.yaml`, `internal/store/migrations/atlas.sum`, `docs/room-log.md` (the migration table)

- [ ] **Step 1: Write the migration**: `ALTER TABLE rooms DROP COLUMN bridge_seen_at;` with a header comment: "written on every claim and renewal, read nowhere since ruling SBB made freshness irrelevant (review 2026-10-15, B11)". Check the broker's `UPDATE` column grant in `20260927120000_rooms.sql` still names existing columns only; if it listed `bridge_seen_at`, the drop removes it implicitly.
- [ ] **Step 2: Update the store**: `ClaimBridge` and `claimFree` set `bridge_run` only; `TouchBridge` becomes `SELECT 1 FROM rooms WHERE room_id = $1 AND bridge_run = $2` (held = one row). `atlas migrate hash --dir file://internal/store/migrations`; list the file in `kustomization.yaml`; add its row to `docs/room-log.md`.
- [ ] **Step 3: Run**: `task migrations && go test -race -count=1 ./internal/store/...`. Expected: PASS (`TestBrokerRoleIsAppendOnly` included).
- [ ] **Step 4: Commit**: `git commit -am "refactor(store): drop the unread bridge_seen_at column"`.

### Task 43: `rooms_participants` is fed

- [ ] `internal/metrics/metrics.go:95-96` stays; `internal/runwatch` sets it from the count of live runs with a `roomRef` and `internal/humanapi` from the open WebSocket count, through the injected `metrics.Set` (two `Set` calls on the gauge's two label values `kind=run|human`, if the metric is labelled; else one sum). Test: the fake meter records a value after a connection opens. Commit `fix(metrics): rooms_participants counts live runs and open viewers`.

### Task 44: The rooms client's spans are real or gone

- [ ] `internal/app/factory.go:108`: pass the task sink's `TracerProvider` to the rooms client (it already carries the OTLP exporter); a test asserts a span is recorded on a client call with an in-memory exporter. Commit `fix(factory): the rooms client's spans reach the trace collector`.

### Task 45: The run-request API's port says what it is

- [ ] `charts/agent-factory/templates/deployment.yaml:42` and `Service`: container port `api` becomes `8080`; `internal/app/factory.go:66`'s comment reads ":8080, plaintext inside the mesh; the broker's :8443 is the TLS listener"; `internal/factory/config` default `api.listen` follows; `docs/integration.md` and the chart README (docs plan) name it. `task chart:check`. Commit `fix(chart): the run-request API listens on 8080, named for what it is`.

### Task 46: Theme 9 gate

- [ ] `task check` exits 0; open the theme's pull request.

### Task 47: Done

- [ ] Every theme's pull request merged; `go test -race -count=1 ./...` under 3 minutes end to end; `grep -rn 'time.Sleep' --include='*_test.go' internal | wc -l` reduced to the `eventually` loops with no hook, each with a comment naming why.
