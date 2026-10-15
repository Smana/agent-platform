# Code health: design

**Status:** design, approved by the owner on 2026-10-15; the brainstorming answers it rests on are
in the [project review](2026-10-15-project-review.md#what-the-owner-decided).
**Scope:** the Go in `internal/`, `cmd/`, `api/`, the factory chart and the harness image's
hygiene. No behaviour a user sees changes, except where a finding is a bug. **Plan:**
[2026-10-15-code-health-plan.md](../plans/2026-10-15-code-health-plan.md).

## Conclusion

Nothing here holds v0.11.0. The review found no high-severity defect and a codebase that enforces
its invariants in the database and fails closed by reflex. What it found are **16 medium edges**
where a contract rests on an unpinned assumption, a default opens instead of closing, a bound is
not the bound the comment names, or a seam leaks; **four files** that have outgrown one head; a
**test suite** that spends 207 s starting 85 Postgres containers and leans on real sleeps in a dozen
places; and dead code and tracked build artefacts. Each theme becomes one pull request, each
finding one commit with the test that pins it first.

## Decisions

Each rejects a named alternative. What remains the owner's is under "Open to the owner".

| # | Decision | Rejected | Why |
|---|---|---|---|
| D1 | **Fix the medium edges first, one PR per theme** (money and revocation; defaults and gates; liveness and leases; bounds; seams), each finding pinned by a failing test before the fix | One sweeping PR | Each theme is reviewable alone; a money fix and a lock-scope fix should never share a revert |
| D2 | **The bridge keeps answering local policy verdicts when its buffer is full.** `OnStatus` runs on every tick; past `MaxBuffer` the bridge stops *buffering* mapped items but still reads the harness and answers Allow and Deny from the classifier; only `human`-class decisions wait for the broker | Document that back-pressure also freezes the harness | The rooms design §3 says a broker outage never stops the sandbox; today it does once 3 MiB is buffered, for every action class |
| D3 | **Mechanical splits of the four oversized files**, same package, no exported API change, one commit each: `bridge/classify.go` → tables, shell lexer, policy; `bridge/bridge.go` → batch helpers, final read; `humanapi/acts.go` → `startrun.go`, `fork.go`; `bridgeapi/server.go` → `events.go`, `stream.go` | Leave them; rewrite them | They mix concerns a reader must hold at once; a move is cheap and the tests already live in files named for the destination |
| D4 | **The store suite starts one Postgres** in `TestMain`, migrates a template database once, and gives each test `CREATE DATABASE … TEMPLATE` | Keep one container per test | 207 s to seconds; the isolation is the same |
| D5 | **Clocks and tickers are injected where tests sleep**: `humanapi.Server.Now`, a `Bridge.Ticker`, hooks on the fakes the fan-out tests wait on | Longer sleeps | `-race` on a loaded runner is where a sleep-synchronised test fails |
| D6 | **`forbidigo` gains `slog.(Warn\|Info\|Error\|Debug)(`** and the summary package returns its "no repository" condition to the caller instead of logging | A `//nolint` on the two calls | The grep for `slog.Default` missed this spelling once; the linter will not |
| D7 | **Summary goldens gain the `-update-golden` flag** and stay | Replace them with field assertions | `summary/v1` is a rendered contract two renderers read; the goldens are the contract test. The flag is what `AGENTS.md` asks of a golden |
| D8 | **Hygiene in one PR**: untrack `__pycache__`, drop the dead `bridge_seen_at` column and `TouchBridge`'s update, feed or remove `rooms_participants`, wire or remove the rooms client's dead spans, derive the three duplicated seam constants from their owners, rename the run-request API's port | Leave dead code with a comment | Dead code is read as live by the next reviewer |

## Findings by theme

Numbers are the review's (`B` broker side, `F` bridge and factory). **✓** verified by hand.

### Theme 1: money and revocation (`internal/factory/meter`)

| # | File:line | Finding | Fix |
|---|---|---|---|
| F1 ✓ | `meter.go:126-134`, `vm.go:104-119` | `cost()` derives fresh input as `Input − CacheRead − CacheWrite`; `RunUsage` adds only the `input` series to `Input`. Whether the gateway's `input` includes cache tokens is asserted in `docs/operations.md:197` and pinned by no test | A `vm_test.go` case built from a recorded `api/v1/query` reply with all four series, asserting `Usage` and `cost()` together; the `Usage` doc comment becomes the one statement of the contract |
| F2 ✓ | `meter.go:397-399` | `Tick` returns on a failed ledger `settle` before any revocation is written, so the per-run cap stops firing while the ledger ConfigMap is unwritable | Compute and write revocations and usage annotations first, settle the ledger after; a test with a failing ledger store asserts the run is still revoked |

### Theme 2: defaults, gates and comparisons

| # | File:line | Finding | Fix |
|---|---|---|---|
| B1 | `internal/policy/policy.go:96-108` | `Allowed` for `ActorSystem` falls to `default: return true` | Enumerate the system actions; `return false` by default; a table test asserts every `Action` constant has a row per actor kind |
| F7 ✓ | `internal/factory/reconciler/merge.go:35,543`, `arm.go:90,100` | Logins compared with `==` while `IsMaintainer` uses `EqualFold`; a case mismatch on `mergerLogin` skips `Verifying` and the revert watch | One `forge.SameLogin(a, b string) bool` (`EqualFold`) used at every compare; a test merges as `Merger-Bot[bot]` against config `merger-bot[bot]` and expects `AutoMerged` |
| F9 ✓ | `charts/agent-factory/templates/rbac.yaml:19-29`, `tests/render.sh:50` | The unbounded `configmaps` rule subsumes the "name-bounded" stop rule the test asserts | Delete the name-bounded rule and its comment, state the namespace as the bound, and assert the rules with `yq` as sets rather than `grep -F` |
| F11 | `internal/bridge/classify.go:334,344` | A heredoc written to a file then executed hides a `git push`; the battery enshrines it | Scan `s.bodies` in the final `mentionsForge` pass for every head; the battery case flips from residual to classified |
| B24 | `internal/bridgeapi/system.go:148` | `roomMessage` accepts blank text; `enqueue` refuses it | The same `TrimSpace` check |

### Theme 3: liveness, leases and idempotency

| # | File:line | Finding | Fix |
|---|---|---|---|
| B8 | `internal/runwatch/watcher.go:119-122`, `bridgeapi/server.go:297-300` | `Live` conflates unknown with dead; `hello` on a not-yet-synced replica can take a lease from a live holder | `Live` returns `(live, known bool)`; the hello callback keeps the lease when the holder is unknown |
| B7 | `internal/bridgeapi/approvals.go:112,114` | `time.Now()` despite `Server.Now`; `OriginSeq: UnixNano()` so a retried approval never replays its key | `s.now()`; `OriginSeq` from a hash of `(run, eventId)` so the log and the row agree on a replay |
| B6 | `internal/app/approvals.go:99-110`, `bridgeapi/approvals.go:57-62` | `RoomPolicy func(roomID)` drops the request context | `func(ctx context.Context, roomID string)`; `r.Context()` passed |
| B16 | `internal/store/fork.go:86-104` | A fork copies queued messages as events but not their `queue` rows | Copy still-queued rows under `queue_is_its_event`; a store test forks a room with a queued message and reads the fork's queue |
| F18 | `internal/factory/reconciler/watch.go:276-277` | A `/factory retry` within clock skew of the escalation is ignored forever | A one-minute tolerance on the comparison, with a test at skew |

### Theme 4: bounds

| # | File:line | Finding | Fix |
|---|---|---|---|
| F3 | `internal/bridge/bridge.go:589-592, 602-604` | Past `MaxBuffer` the confirmation loop never runs (D2) | `OnStatus` on every tick; past the cap, read without buffering and answer Allow and Deny locally; a test fills the buffer, stops the broker and expects an Allow-class action confirmed |
| F5 | `internal/factory/rooms/client.go:189-206` | `EventsSince` can hold ~800 MiB in a 512 Mi pod | `EventsSince(ctx, room, after, keep func(envelope.Event) bool)` retains only kept events; `roomTail` passes its filter |
| B3 | `internal/fanout/hub.go:145` | A 4096-event channel per subscriber allocates ~0.9 MB eagerly; `Budget` is not the real cap | A 256-event channel; `Budget` documented as the bound; a test asserts the per-subscriber allocation |
| B9 | `internal/store/retention.go:60-61` | A 100k-event room purged in one `DELETE` under a 15 s statement timeout | Delete in `seq` ranges of 10,000 inside the per-room transaction; a test purges a room larger than one range |
| B15 | `internal/fanout/hub.go:309-326`, `internal/github/app.go:283-337` | `fetch` offers a page to every subscriber under `h.mu`; `installation` mints under `a.mu` | Snapshot the subscribers, release, offer; lock per repository key |

### Theme 5: seams and the egress client

| # | File:line | Finding | Fix |
|---|---|---|---|
| F4 | `internal/factory/triage/c7.go:61-64,74`, `internal/app/factory.go:163` | The C7 classifier falls back to `&http.Client{}` and reads an unbounded body | `HC` required (error on nil, as `meter.VL`); `httpx.New` from `app`; the body through `httpx.ReadBody` |
| F8 | `internal/bridge/classify.go:80-97` | The read-only MCP allowlist hard-codes platform backends | The tool set arrives through the bridge's environment, as `EGRESS_PROFILES` does |
| F6 | `internal/factory/killswitch/killswitch.go:28-32` | Package-level `atomic.Bool`, leader-only | Delete `issueStop` and `SetIssue`; `Engaged` reads the published stop object only |
| F17 | `internal/factory/meter/vm.go:27`, `app/factory.go:193-195`, `intake/schedule.go:166` | `system:serviceaccount:agents:xplane-run-`, the two group names and `renovate[bot]` as literals | Derive the first from `runs.Namespace` and `runwatch.ClaimPrefix`; the others become config keys with validation |
| B26 | `internal/app/roomctl.go:67-68` | `stream.Timeout = 0` mutates the audited client after construction | `httpx.NewStreaming` documents the no-timeout client once |
| B13 | `internal/bridgeapi/server.go:53-55` | `var replayPage = 500`, mutable package state | A `Server` field with a default |

### Theme 6: robustness and logs

| # | File:line | Finding | Fix |
|---|---|---|---|
| B4 | `internal/bridgeapi/server.go:95-131` | No required-dependency check; a nil `Watch` panics per request | The same nil check `humanapi.Server.Serve` has, at `ListenAndServeTLS` |
| B2 ✓ | `internal/summary/summary.go:280`, `waiting.go:58` | The pure package logs through the global logger | `View` returns the condition in the `Summary` (a `Warnings []string` field is a contract change, so instead `repoOf` stays silent and the caller logs when `stop` is absent); `forbidigo` rule (D6) |
| F12 | `internal/factory/intake/runlore.go:159` | Raw `AlertName` and `ResourceRef` in a log | Log `taskid.Name(key)` |
| F13 | `internal/factory/forge/github.go:616-622` | GitHub 409 detected by `strings.Contains(err.Error(), "409")` | Record the status in the `auth` round-tripper; branch on it; wrap without the body |
| B14 | `internal/humanapi/server.go:190-196` | Shutdown polls `s.open()` every 10 ms | A `sync.WaitGroup` beside `conns` |
| B17 | `internal/bridgeapi/server.go:588-590` | The SSE stream releases the hub budget before writing; the WebSocket after | `sub.Sent(ev)` after `send(ev)` |
| B16b | `internal/store/approvals.go:299-306` | `closeAll` reports a replayed close as newly appended | Return `dup` from `closeApproval`, skip it |

### Theme 7: structure (D3)

| File | Lines | Split |
|---|---|---|
| `internal/bridge/classify.go` | 1202 | `classify_tables.go` (allowlists, 62-250), `shlex.go` (lexer, 942-1146), `policy.go` (Decide, Stricter, profiles, 1148-1202); `classify.go` keeps the classifier |
| `internal/bridge/bridge.go` | 992 | `batch.go` (encoding, stubs, Retry-After, backoff, 101-254), `finalread.go` (636-712); the SSE consumer (954-992) joins `stream.go` if one exists, else stays |
| `internal/humanapi/acts.go` | 758 | `startrun.go` (`startRun`, `busy`, `validRun`, `forkPoint`), `fork.go` (`fork`) |
| `internal/bridgeapi/server.go` | 612 | `events.go` (349-428), `stream.go` (486-597) |
| `internal/factory/forge/fake.go` | 521, non-test | Moves to `forge/forgetest` so it leaves the production build |

### Theme 8: tests (D4, D5, D7)

| # | File:line | Finding | Fix |
|---|---|---|---|
| B13/B29 | `internal/store/testdb_test.go:22-31` | One container per test, 85 call sites, 207 s | `TestMain` starts one Postgres, migrates `rooms_tpl`; `open(t)` creates a database from the template and drops it in `t.Cleanup` |
| B5 | `internal/humanapi/ws.go:190`, `server.go:217`, `acts.go:464` | No injectable clock; two tests on real timers | `Now func() time.Time` on `Server` and `Actor`, as `bridgeapi` has |
| F20 | `internal/bridge/bridge_test.go:446…654`, `confirm_test.go:704…791` | Negative assertions rest on `time.Sleep` | A `Ticker` on `Bridge`, stepped by the test |
| B10 | `bridgeapi/server_test.go:1058`, `fanout/hub_test.go:455,500`, `app/bridge_test.go:86` | Sleeps as synchronisation | Hooks on the fakes (a read counter, a channel) the tests wait on |
| B17g | `internal/summary/summary_test.go:20-43` | Goldens with no update flag | `-update-golden` (D7) |
| B23 | `internal/app/approvals_test.go:30-90` | Asserts the call string `"supersede expire oldest"` | Assert which approvals closed and the gauge; keep one ordering check |
| F24 | `charts/agent-factory/tests/render.sh:13,48-51` | `grep -F` on rendered YAML | `yq eval-all` assertions on kinds, `resourceNames` and verb sets |

### Theme 9: hygiene (D8)

| # | File:line | Finding | Fix |
|---|---|---|---|
| F14 | `images/agent-harness/**/__pycache__/*.pyc` | Five tracked caches | `git rm --cached`; `__pycache__/` and `*.pyc` in `.gitignore` |
| B11 | `internal/store/bridges.go:48,80,89` | `bridge_seen_at` written, never read | A migration drops the column; `TouchBridge` keeps the `bridge_run = $2` check |
| B12 | `internal/metrics/metrics.go:95-96` | `rooms_participants` never incremented | Fed from the run and human connection counts, or removed with its doc row |
| F15 | `internal/app/factory.go:108` | The rooms client's spans are built on a nil provider | The task sink's provider, or the span code goes until rooms tracing lands |
| F16 | `internal/factory/api/server.go:133-147`, chart `deployment.yaml:42` | Plaintext on a port named `8443` | Port `8080` in the chart and the comment; the owner decides on TLS (open question) |
| B27 | `internal/brief/brief.go:74,100`, `internal/mcp/tools.go:208` | A regexp compiled per call | Compiled once per repository |
| B15d | `internal/store/driver.go:84`, `store.go:204` | The idempotency lookup runs twice per driver change | `appendTx` takes a "key checked" flag |

## Out of scope

- Anything that changes `summary/v1`, the wire frames or the HTTP contracts: the readability spec owns the summary's two fixes.
- OTel spans for the rooms side: already a tracked follow-up in `AGENTS.md`.
- Rewrites. Every change here is a fix, a move or a test.

## Verification

| Check | Proves |
|---|---|
| A failing test precedes every fix, named in the plan | The edge is pinned |
| `go test -race -count=1 ./internal/store/...` under 30 s | D4 |
| `task lint` fails on a `slog.Warn(` outside `cmd/` and tests | D6 |
| `task check` exits 0 | The gate |
| `git ls-files '*.pyc'` prints nothing | F14 |

## Open to the owner

| Question | Default taken here |
|---|---|
| Does the gateway's `input` series include cache tokens (F1)? | The test is written from a recorded reply; the owner supplies the reply from a live run |
| Keep answering local verdicts past `MaxBuffer` (D2), or document the freeze? | Answer them |
| Terminate TLS on the run-request API, or rely on the CNI's WireGuard and rename the port (F16)? | Rename the port; TLS is a separate decision |
| Feed `rooms_participants` or remove it (B12)? | Feed it |
