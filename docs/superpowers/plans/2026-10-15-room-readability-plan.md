# Room Readability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the room page answer, without scrolling, what the task is, what the agent is doing now, what needs the viewer and what they can do; make the room list scannable; keep every owner decision (vanilla TS, strict CSP, deterministic summary, web-only approvals).

**Architecture:** The lead's four cards become a sticky title strip (`web/src/strip.ts`) fed by the summary and the live state; the approval cards move into a needs-you panel (`web/src/needs.ts`) in the lead; the composer docks at the bottom; `chat.ts` folds plumbing rows; `controls.ts` regroups under names; `view.ts` renders a table. Two behavioural fixes land in `internal/summary` under the same `summary/v1`, and `GET /api/rooms` gains `lastEventAt` through one new method on the human API's store seam.

**Tech Stack:** TypeScript (ES2022, esbuild, vitest + jsdom), CSS custom properties, Go 1.27 (`internal/summary`, `internal/humanapi`, `internal/store`, `internal/roomctl`).

**Spec:** [`docs/superpowers/specs/2026-10-15-room-readability-design.md`](../specs/2026-10-15-room-readability-design.md)

## Global Constraints

- No framework, no new runtime dependency: `web/package.json` keeps `markdown-it` as its only dependency.
- CSP stays `default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'` (`internal/humanapi/server.go:82`): no inline `style=` attributes in markup, no `<style>` elements; set widths with `el.style.setProperty(...)` (CSSOM is permitted).
- Untrusted text (every agent- or human-written string) enters the DOM as `textContent` or through `markdown()` in `render.ts`; never `innerHTML` (T10).
- Every input, select and textarea carries an accessible name (`named()` in `controls.ts`); no text below 12 px; every text token pair meets WCAG AA 4.5:1 in both themes, informational borders 3:1.
- `summary/v1` keeps its shape: no field added or removed in `internal/summary.Summary`.
- The committed bundle must match the build: every commit that touches `web/` runs `task ui:build` and adds `internal/humanapi/ui/dist/`.
- Go tests: stdlib `testing`, table-driven, hand-written fakes, `-race`. UI tests: vitest in jsdom, driving `mountRoom` through `FakeSocket` (`web/test/fakes.ts`).
- `task check` exits 0 before every commit; conventional commits, one concern each.

## Review Focus

1. A room whose summary fetch fails on mount: the strip must still render from the state frame (room id, phase, driver, you) and the stream must still flow. Pinned in Task 4.
2. A `tool_call` whose `tool_result` arrives after the page dropped the call row at the 5,000-row cap: the Now line must not throw and must show the result alone. Pinned in Task 5.
3. A `decide` act for an approval that was superseded between render and click: the ack says `already_decided`; the card must disappear on the `approval_decided` event, not stay with a dead button. Pinned in Task 6.
4. Markers arriving out of seq order on a clamped resume (a seq the page already holds): folding must replace in place and never double a line. Pinned in Task 7.
5. A viewer who loses the driver token while typing a steering message: the text stays, the select resets, the notice appears beside the select, Send stays enabled. Pinned in Task 8.

---

### Task 1: The summary fold stops offering what a room cannot do

**Files:**
- Modify: `internal/summary/summary.go:233-245` (`View`), `internal/summary/summary.go:60-70` (`State`), `internal/summary/summary.go:135-180` (`Add`)
- Test: `internal/summary/summary_test.go`
- Modify: `internal/summary/testdata/sealed.json`, `internal/summary/testdata/no_facts.json` (only if their `want` encodes the old behaviour)

**Interfaces:**
- Consumes: `envelope.Participant` events with `principal: "agent:<id>"`, `change: joined | left`.
- Produces: `State.Running bool` (an agent run has joined and not left). `View` sets no `Status.Reason`/`Status.Next` for a sealed room and adds the `steer` action only when `st.Facts.Run != nil || st.Running`.

- [ ] **Step 1: Write the failing tests** in `internal/summary/summary_test.go`:

```go
// A sealed room refuses new facts, so its last wait reason is history, not a status.
func TestASealedRoomCarriesNoWaitingLine(t *testing.T) {
	factory := envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"}
	broker := envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"}
	evs := []envelope.Event{
		ev(1, envelope.StateChanged, factory, envelope.TaskStatePayload(envelope.TaskFacts{Phase: "AwaitingHuman", Reason: "policy_pending"})),
		ev(2, envelope.StateChanged, broker, envelope.StatePayload("room_phase", map[string]any{"phase": "Closed", "reason": "owner"})),
	}
	s := summary.View(summary.Fold(evs), "r", "u", owner(), 0, time.Now())
	if s.Status.Reason != "" || s.Status.Next != "" {
		t.Fatalf("a sealed room waits: reason %q next %q", s.Status.Reason, s.Status.Next)
	}
}

// Steering needs something to steer: a run the facts name, or an agent that joined and has not left.
func TestSteerNeedsARunningRun(t *testing.T) {
	broker := envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"}
	join := ev(1, envelope.Participant, broker, map[string]any{"principal": "agent:cf4ato2x", "change": "joined", "role": "implementer"})
	leave := ev(2, envelope.Participant, broker, map[string]any{"principal": "agent:cf4ato2x", "change": "left"})
	cases := []struct {
		name string
		evs  []envelope.Event
		want bool
	}{
		{"empty room", nil, false},
		{"agent joined", []envelope.Event{join}, true},
		{"agent left", []envelope.Event{join, leave}, false},
		{"facts name a run", []envelope.Event{ev(3, envelope.StateChanged, envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"},
			envelope.TaskStatePayload(envelope.TaskFacts{Phase: "Implementing", Run: &envelope.RunFact{ID: "cf4ato2x", Role: "implementer"}}))}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := slices.Contains(actionKinds(summary.View(summary.Fold(tc.evs), "r", "u", driverWeb(), 0, time.Now())), "steer")
			if got != tc.want {
				t.Fatalf("steer offered = %v, want %v", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run them**: `go test ./internal/summary/ -run 'TestASealedRoomCarriesNoWaitingLine|TestSteerNeedsARunningRun' -v`. Expected: both FAIL (`a sealed room waits: reason "the policy needs…"`; `steer offered = true, want false` for "empty room").

- [ ] **Step 3: Implement.** In `State` add `Running bool // an agent run joined and has not left`. In `Add`, a new case:

```go
case envelope.Participant:
	var p struct {
		Principal string `json:"principal"`
		Change    string `json:"change"`
	}
	if json.Unmarshal(ev.Payload, &p) == nil && strings.HasPrefix(p.Principal, "agent:") {
		st.Running = p.Change == "joined"
	}
```

In `foldState`'s `forked_from` case keep `Running` false (a fork starts with no run). In `View`, move the waiting line under the sealed check and gate steer:

```go
if st.Sealed != "" {
	s.Status.Phase = st.Sealed
	return s // read-only: no wait, no needs, no actions
}
s.Status.Reason, s.Status.Next = waiting(st.Facts)
…
if policy.Allowed(sub, policy.Steer) && (st.Facts != nil && st.Facts.Run != nil || st.Running) {
```

- [ ] **Step 4: Run the package**: `go test -race -count=1 ./internal/summary/...`. Expected: the two new tests PASS. If `TestFoldGolden/sealed` fails because `testdata/sealed.json`'s `want.status` carries `reason`/`next`, delete those two keys from its `want`; if `TestFoldGolden/no_facts` fails because `want.actions` lists `steer` with no run, remove that action. Re-run until green. `TestAddingPagesIsFoldingTheLog` must stay green: `Running` folds the same page by page.

- [ ] **Step 5: Commit**:

```bash
git add internal/summary
git commit -m "fix(summary): a sealed room carries no waiting line, and steer needs a run"
```

---

### Task 2: `GET /api/rooms` carries each room's last activity

**Files:**
- Modify: `internal/humanapi/rooms.go:147-162` (`roomRow`), the `listRooms` handler, the `Log` interface humanapi declares (search `type Log interface` in `internal/humanapi/*.go`)
- Modify: `internal/store/rooms.go` (new method)
- Test: `internal/store/rooms_test.go` or `internal/store/store_test.go`, `internal/humanapi/rooms_list_test.go`
- Modify: `docs/api.md:574` (the row description; one clause)

**Interfaces:**
- Produces: `func (s *Store) LastEventAt(ctx context.Context, roomIDs []string) (map[string]time.Time, error)` in `internal/store`; the same method on humanapi's `Log` interface and on its test fake; `roomRow.LastEventAt time.Time \`json:"lastEventAt,omitzero"\``.

- [ ] **Step 1: Write the failing store test** (in `internal/store/store_test.go`, next to the other room tests):

```go
func TestLastEventAtReadsEveryRoomInOneCall(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := context.Background()
	if _, err := s.EnsureRoom(ctx, NewRoom{ID: "aaaaaaa2", Driver: "system:factory", Retention: time.Hour}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LastEventAt(ctx, []string{room, "aaaaaaa2", "nosuchrm"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[room].IsZero() || got["aaaaaaa2"].IsZero() {
		t.Fatalf("got %v: want the two rooms' seq-1 times, nothing for the unknown id", got)
	}
}
```

- [ ] **Step 2: Run it**: `go test ./internal/store/ -run TestLastEventAtReadsEveryRoomInOneCall`. Expected: FAIL to compile (`s.LastEventAt undefined`).

- [ ] **Step 3: Implement** in `internal/store/rooms.go`:

```go
// LastEventAt is when each room's log last moved, for the rooms in ids that exist. One query:
// the room list reads it for every row it shows.
func (s *Store) LastEventAt(ctx context.Context, ids []string) (map[string]time.Time, error) {
	rows, err := s.pool.Query(ctx, `SELECT room_id, last_event_at FROM rooms WHERE room_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: last event at: %w", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, fmt.Errorf("store: last event at: %w", err)
		}
		out[id] = at
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run the store test**: same command. Expected: PASS (needs Docker; if `testcontainers` cannot reach the daemon, set `DOCKER_HOST` to the Colima socket and `TESTCONTAINERS_RYUK_DISABLED=true`).

- [ ] **Step 5: Write the failing humanapi test** in `internal/humanapi/rooms_list_test.go`, in the style of its neighbours (a fake `Log` with a `lastEventAt map[string]time.Time` field): request `GET /api/rooms` as an admin with one room whose fake answers `2026-10-15T10:00:00Z`, decode the rows, and assert `rows[0].LastEventAt.Equal(time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC))`. Run it; expected: FAIL (`LastEventAt` missing on the row or the interface).

- [ ] **Step 6: Implement**: add `LastEventAt(ctx context.Context, ids []string) (map[string]time.Time, error)` to humanapi's `Log` interface and to every fake implementing it in `internal/humanapi/*_test.go` and `internal/app` wiring tests (the compiler lists them). In `listRooms`, after the readable rooms are known and before the rows are built, call it once with their ids; on error log at Warn with `"err", err` only and leave the field zero (the list must not fail on it). Set `row.LastEventAt = at[room.Name]`. Add the field to `roomRow` with `json:"lastEventAt,omitzero"`.

- [ ] **Step 7: Run**: `go test -race -count=1 ./internal/humanapi/... ./internal/app/...`. Expected: PASS.

- [ ] **Step 8: Document**: in `docs/api.md`, the `GET /api/rooms` row (line 574), after "last `seq`", add "`lastEventAt` (when the log last moved)".

- [ ] **Step 9: Commit**:

```bash
git add internal/store internal/humanapi internal/app docs/api.md
git commit -m "feat(humanapi): the room list says when each room last moved"
```

---

### Task 3: Scope the page chrome and make every token pair pass AA

**Files:**
- Modify: `web/src/app.css` (the `header, footer` rules at lines 111-123; the tokens; the 11 px sizes)
- Modify: `web/src/room.ts:20,38` (the page header and footer get classes), `web/src/view.ts` (the list page header, Task 9 adds it; here only the class on any header it already creates), `web/preview/preview.ts:873-885`
- Create: `web/test/contrast.test.ts`

**Interfaces:**
- Produces: CSS classes `.page-header`, `.page-dock`; tokens `--link`, `--success-text`, `--warning-border` in both themes.

- [ ] **Step 1: Write the failing contrast test** `web/test/contrast.test.ts`:

```ts
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

// Every text pair the page paints must meet WCAG AA (4.5:1); meaningful borders 3:1.
// color-mix(in srgb, A p%, B) is a channel-wise lerp, which is how the chips are tinted.
const css = readFileSync(join(__dirname, "../src/app.css"), "utf8");
function tokens(block: string): Record<string, string> {
  const m = css.match(new RegExp(`${block.replace(".", "\\.")}\\s*{([^}]*)}`))!;
  return Object.fromEntries([...m[1].matchAll(/(--[\w-]+):\s*(#[0-9a-f]{6})/gi)].map((x) => [x[1], x[2].toLowerCase()]));
}
const rgb = (h: string) => [1, 3, 5].map((i) => parseInt(h.slice(i, i + 2), 16));
const lum = (h: string) => rgb(h).map((c) => { c /= 255; return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4; })
  .reduce((s, c, i) => s + c * [0.2126, 0.7152, 0.0722][i], 0);
export const ratio = (a: string, b: string) => { const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p); return (x + 0.05) / (y + 0.05); };
const mix = (a: string, b: string, pa: number) => "#" + rgb(a).map((c, i) => Math.round(c * pa + rgb(b)[i] * (1 - pa)).toString(16).padStart(2, "0")).join("");

const light = tokens(":root"), dark = tokens(".dark");
const pairs = (t: Record<string, string>) => [
  ["body text on the page", t["--foreground"], t["--muted"], 4.5],
  ["muted text on a card", t["--muted-foreground"], t["--card"], 4.5],
  ["placeholder and meta text on a card", t["--placeholder"], t["--card"], 4.5],
  ["placeholder and meta text on the page", t["--placeholder"], t["--muted"], 4.5],
  ["links on the page", t["--link"], t["--muted"], 4.5],
  ["links on a card", t["--link"], t["--card"], 4.5],
  ["success text on a card", t["--success-text"], t["--card"], 4.5],
  ["ok chip text on its tint", t["--success-text"], mix(t["--success"], t["--card"], 0.12), 4.5],
  ["running chip text on its tint", t["--link"], mix(t["--primary"], t["--card"], 0.10), 4.5],
  ["destructive text on a card", t["--destructive"], t["--card"], 4.5],
  ["warning text on its tint", t["--warning-foreground"], mix(t["--warning"], t["--card"], 0.18), 4.5],
  ["primary button text", t["--primary-foreground"], t["--primary"], 4.5],
  ["the header's text", t["--brand-navy-foreground"], t["--brand-navy"], 4.5],
  ["the needs-you border", t["--warning-border"], t["--card"], 3],
] as const;

describe.each([["light", light], ["dark", dark]])("%s theme", (_name, t) => {
  it.each(pairs(t))("%s", (_what, fg, bg, min) => {
    expect(fg, "token defined").toBeDefined();
    expect(ratio(fg, bg)).toBeGreaterThanOrEqual(min);
  });
});
```

- [ ] **Step 2: Run it**: `cd web && npx vitest run test/contrast.test.ts`. Expected: FAIL (`--link`, `--success-text`, `--warning-border` undefined; placeholder 3.45 < 4.5).

- [ ] **Step 3: Add the tokens and fix the values** in `web/src/app.css`. Light (`:root`): `--placeholder: #6b6b6b;` (was `#8a8a8a`), add `--link: #00639c;`, `--success-text: #3a7410;`, `--warning-border: #b36f00;`. Dark (`.dark`): `--placeholder: #9db0bc;` (was `#7f93a1`), add `--link: #4db3e8;`, `--success-text: #7fcf3a;`, `--warning-border: #ed9d13;`. Then route text through them: `a { color: var(--link); }`; `.t-name { color: var(--link); }`; `.chip-ok, .pol-allow, .pol-approved, .verdict-approve, footer[data-status="live"] .status { color: var(--success-text); }`; `.chip-running { color: var(--link); }`; `.summary-needs-you { border-color: var(--warning-border); }`. Leave `--primary` and `--success` for fills, rings and buttons (3:1 is enough for UI). Raise every `font-size: 11px` to `12px` (`.msg-head time, .t-time`, `.t-sec`, `.t-meta`, `.chip, .pol`, `.redacted`, `.fork-here`). Run the test again until every pair passes.

- [ ] **Step 4: Scope the chrome.** Replace lines 111-123:

```css
/* The page chrome: a brand-navy header, a docked footer. Class-scoped: the chat's
   <header class="msg-head"> and the raw view's <header class="ev-head"> are headers too,
   and the bare selector made each of them a sticky navy bar (review 2026-10-15, U1). */
.page-header, .page-dock { padding: 8px 16px; position: sticky; }
.page-header {
  top: 0; z-index: 2;
  display: flex; gap: 12px; align-items: baseline;
  background: var(--brand-navy); color: var(--brand-navy-foreground);
}
.page-header .title { overflow-wrap: anywhere; }
.page-dock {
  bottom: 0; z-index: 2;
  background: var(--card); color: var(--card-foreground);
  border-top: 1px solid var(--border);
}
.page-dock .status { float: right; color: var(--muted-foreground); font-family: ui-monospace, monospace; }
.page-dock[data-status="live"] .status { color: var(--success-text); }
.page-dock .notice { margin-left: 12px; color: var(--destructive); }
```

and change `header button { … }` to `.page-header button { … }`, `footer .status`/`footer .notice`/`footer[data-status="live"]` to their `.page-dock` forms. In `room.ts:20` and `:38` set `header.className = "page-header"` and `footer.className = "page-dock"`; the same in `web/preview/preview.ts:873,881`.

- [ ] **Step 5: Add a regression test** to `web/test/room.test.ts`:

```ts
it("styles only the page chrome as a header: a message head is not sticky navy (U1)", () => {
  const p = page();
  p.join(snapshot(), 0);
  p.last().recv(event(1, "message", { kind: "chat", text: "hi", delivery: "none" }));
  expect(p.app.querySelector("header.page-header")).not.toBeNull();
  expect(p.app.querySelector("header.msg-head")?.classList.contains("page-header")).toBe(false);
});
```

- [ ] **Step 6: Run all UI gates**: `cd web && npm run typecheck && npm test`, then `task ui:build`. Expected: PASS; the bundle rebuilt.

- [ ] **Step 7: Commit**:

```bash
git add web internal/humanapi/ui/dist
git commit -m "fix(web): scope the page chrome to its own class, and every text pair meets AA"
```

---

### Task 4: The title strip replaces the status card

**Files:**
- Create: `web/src/strip.ts`, `web/test/strip.test.ts`
- Modify: `web/src/room.ts:19-66` (header, `renderHeader`), `web/src/summary.ts` (remove `statusBlock` and `actionsBlock`; export `needsItems`, `renderNotes`), `web/test/summary.test.ts` (the status and actions block tests move to `strip.test.ts`), `web/src/app.css` (strip styles)

**Interfaces:**
- Produces:
  ```ts
  export interface StripInput { roomId: string; snap?: Snapshot; phase: string; driver: string; sealedBy?: { who: string; at: string };
    summary?: Summary; needs: number; connection: string }
  export function mountStrip(header: HTMLElement): { update(i: StripInput): void; onNeedsClick(f: () => void): void }
  export function fmtTokens(n: number): string          // 189093 → "189 k", 1500000 → "1.5 M"
  export function shortPrincipal(p: string): string      // "human:291847362183" → "human:2918…"
  ```
- Consumes: `Summary` from `summary.ts`, `Snapshot` from `conn.ts`.

- [ ] **Step 1: Write the failing tests** `web/test/strip.test.ts`:

```ts
import { describe, expect, it } from "vitest";
import { fmtTokens, mountStrip, shortPrincipal } from "../src/strip";
import type { Summary } from "../src/summary";

const summary = (over: Partial<Summary["status"]> = {}): Summary => ({
  apiVersion: "summary/v1", room: "3kq7x2ma", url: "https://rooms.example/r/3kq7x2ma",
  status: { phase: "Implementing", run: { id: "ikely2yk", role: "implementer", startedAt: "2026-10-04T14:00:02Z" },
    budget: { usedTokens: 189093, limitTokens: 1500000 }, pr: { number: 77, url: "https://github.com/o/r/pull/77" },
    issue: { number: 74, url: "https://github.com/o/r/issues/74", title: "Chat view: pair tool calls" }, lastVerdict: null,
    reason: "the policy needs a maintainer's approval", next: "an approver decides", ...over },
  needsYou: [], actions: [], notes: { untrusted: true, items: [] }, cursor: "seq:27",
});
const snap = { roomId: "3kq7x2ma", phase: "Active", driver: "human:291847362183", driverEpoch: 3, dataClass: "internal",
  you: { principal: "human:291847362183", role: "owner", approver: true, driver: true, webUI: true } };
const mount = () => { const h = document.createElement("header"); return { h, strip: mountStrip(h) }; };

describe("the title strip", () => {
  it("heads the page with the issue title and sets the tab title", () => {
    const { h, strip } = mount();
    strip.update({ roomId: "3kq7x2ma", snap, phase: "Active", driver: snap.driver, summary: summary(), needs: 0, connection: "live" });
    expect(h.querySelector("h1")?.textContent).toBe("#74 Chat view: pair tool calls");
    expect(document.title).toBe("#74 Chat view: pair tool calls · Implementing");
  });
  it("falls back to the room id before the summary arrives", () => {
    const { h, strip } = mount();
    strip.update({ roomId: "3kq7x2ma", snap, phase: "Open", driver: snap.driver, needs: 0, connection: "connecting…" });
    expect(h.querySelector("h1")?.textContent).toBe("Room 3kq7x2ma");
    expect(h.querySelector(".strip-conn")?.textContent).toBe("connecting…");
  });
  it("shows the task phase as a chip, the budget as a meter, and you as 'you'", () => {
    const { h, strip } = mount();
    strip.update({ roomId: "3kq7x2ma", snap, phase: "Active", driver: snap.driver, summary: summary(), needs: 0, connection: "live" });
    const chips = [...h.querySelectorAll(".chip")].map((c) => c.textContent);
    expect(chips).toContain("Implementing");
    expect(chips.some((c) => c?.includes("189 k / 1.5 M · 13 %"))).toBe(true);
    expect(h.querySelector(".meter-bar")?.getAttribute("style")).toBeNull(); // CSP: no style attribute
    expect((h.querySelector(".meter-bar") as HTMLElement).style.getPropertyValue("--fill")).toBe("13%");
    expect(h.querySelector(".chip-driver")?.textContent).toBe("driver: you");
  });
  it("says why the task waits and what comes next, on one line", () => {
    const { h, strip } = mount();
    strip.update({ roomId: "3kq7x2ma", snap, phase: "Active", driver: snap.driver, summary: summary(), needs: 0, connection: "live" });
    expect(h.querySelector(".waiting")?.textContent).toBe("Waiting: the policy needs a maintainer's approval → an approver decides");
    strip.update({ roomId: "3kq7x2ma", snap, phase: "Active", driver: snap.driver, summary: summary({ reason: undefined, next: undefined }), needs: 0, connection: "live" });
    expect(h.querySelector(".waiting")).toBeNull();
  });
  it("shows a needs-you pill only when something does, and a closed line when sealed", () => {
    const { h, strip } = mount();
    strip.update({ roomId: "3kq7x2ma", snap, phase: "Active", driver: snap.driver, summary: summary(), needs: 2, connection: "live" });
    expect(h.querySelector(".needs-pill")?.textContent).toBe("Needs you · 2");
    strip.update({ roomId: "3kq7x2ma", snap, phase: "Closed", driver: snap.driver, summary: summary(), needs: 0, connection: "live",
      sealedBy: { who: "human:291847362183", at: "2026-10-04T14:05:00Z" } });
    expect(h.querySelector(".needs-pill")).toBeNull();
    expect(h.querySelector(".closed")?.textContent).toMatch(/^Closed .* by you$/);
  });
});

describe("formatting", () => {
  it.each([[0, "0"], [999, "999"], [189093, "189 k"], [1500000, "1.5 M"], [4000000, "4 M"]])("fmtTokens(%d) = %s", (n, s) => expect(fmtTokens(n)).toBe(s));
  it("shortens a principal and keeps a short one", () => {
    expect(shortPrincipal("human:291847362183")).toBe("human:2918…");
    expect(shortPrincipal("system:factory")).toBe("system:factory");
  });
});
```

- [ ] **Step 2: Run them**: `npx vitest run test/strip.test.ts`. Expected: FAIL (module not found).

- [ ] **Step 3: Implement `web/src/strip.ts`.** Build with `el()` from `controls.ts` and `textContent` only. Layout: `<h1 class="strip-title">`, a `div.strip-chips` row, `div.waiting` (or absent), `div.closed` (or absent), `button.needs-pill` (or absent), `span.strip-conn`. Chips: phase (`summary.status.phase` else `phase`; the other as `title`), run (`${run.role} · ${run.id} · ${since}` with `since` from `startedAt` as `4 min`), budget (`span.chip.chip-budget` containing the text and a `span.meter-bar` whose `--fill` custom property is set with `style.setProperty("--fill", pct + "%")`; CSS draws the bar from `var(--fill)`), PR and issue as `safeLink` from `summary.ts` (export it), data class, `chip-driver` (`driver: you` when `snap.you.principal === driver`, else `driver: ${shortPrincipal(driver)}` with the full id as `title`), your role and ` (approver)`. `document.title` = `${h1 text} · ${phase}`. `fmtTokens`: `< 1000` → the number; `< 1e6` → `Math.round(n / 1e3) + " k"`; else `(n / 1e6)` with at most one decimal, trailing `.0` dropped, `+ " M"`. `shortPrincipal`: keep the kind prefix and the first four characters of the id, then `…`, only when the id is longer than 8.

- [ ] **Step 4: Wire it into `room.ts`.** Replace the `title` span and `renderHeader` with `const strip = mountStrip(header)` and a `refreshStrip()` that calls `strip.update({...})` from `snap`, `state`, the last `Summary`, the needs count (`state.approvals().length` for approvers plus merge and hold items from the summary: Task 6 refines this), and the connection status. Call it from `onState`, `onEvent`, after `loadSummary` renders, and from `onStatus`. Keep `sealedBy` from `RoomState` (Task 8 adds it; until then pass `undefined`). In `summary.ts`, delete `statusBlock` and `actionsBlock`; `renderSummary` now renders `needsYouBlock` and `notesBlock` only (Task 6 replaces it with `needs.ts`; Task 4 keeps the page working). Move the status-block assertions from `web/test/summary.test.ts` into `strip.test.ts` or delete the ones the strip tests already cover; keep the needs-you and notes tests.

- [ ] **Step 5: Pin review focus 1** in `web/test/room.test.ts`:

```ts
it("renders the strip from the state frame when the summary cannot be fetched", async () => {
  const p = page(() => Promise.resolve(new Response("", { status: 503 })));
  p.join(snapshot(), 0);
  await vi.runOnlyPendingTimersAsync();
  expect(p.app.querySelector("h1")?.textContent).toBe("Room 3kq7x2ma");
  expect(p.app.querySelector(".summary-error")?.textContent).toMatch(/unavailable/);
  p.last().recv(event(1, "message", { kind: "chat", text: "still flowing", delivery: "none" }));
  expect(p.app.textContent).toContain("still flowing");
});
```

- [ ] **Step 6: Styles** in `app.css`: `.strip-title { font-size: 18px; margin: 0; font-weight: 600; }`, `.strip-chips { display: flex; flex-wrap: wrap; gap: 6px; align-items: center; }`, `.meter-bar { display: inline-block; width: 64px; height: 6px; border-radius: 3px; background: color-mix(in srgb, var(--brand-navy-foreground) 20%, transparent); position: relative; }`, `.meter-bar::after { content: ""; position: absolute; inset: 0; width: var(--fill, 0%); background: var(--accent); border-radius: 3px; }`, `.waiting, .closed { font-size: 13px; color: var(--brand-navy-foreground); opacity: .9; }`, `.needs-pill { background: var(--warning); color: var(--warning-foreground); border: 0; border-radius: 999px; padding: 2px 10px; font-weight: 600; }`. The header becomes `display: grid; gap: 4px;` for its rows.

- [ ] **Step 7: Run everything**: `npm run typecheck && npm test`, `task ui:build`. Expected: PASS.

- [ ] **Step 8: Commit**:

```bash
git add web internal/humanapi/ui/dist
git commit -m "feat(web): a title strip heads the room with the task, its phase, budget and what needs you"
```

---

### Task 5: The Now line

**Files:**
- Modify: `web/src/room-state.ts` (track the latest activity), `web/src/strip.ts` (render it), `web/src/room.ts` (the 1 s relative-time refresh)
- Test: `web/test/room-state.test.ts`, `web/test/strip.test.ts`, `web/test/room.test.ts`

**Interfaces:**
- Produces: `RoomState.latest(): { seq: number; text: string; status: "running" | "ok" | "error" | "rejected" | "message"; ts: string } | undefined`; `StripInput.now?: ReturnType<RoomState["latest"]>`; `Strip.onNowClick(f: (seq: number) => void)`.

- [ ] **Step 1: Write the failing tests.** In `web/test/room-state.test.ts`:

```ts
it("tracks the latest activity: a running call, then its result, then a message", () => {
  const s = new RoomState();
  s.apply(ev(5, "tool_call", { callId: "c1", tool: "terminal", args: { command: "git status --short" } }));
  expect(s.latest()).toMatchObject({ seq: 5, text: "terminal git status --short", status: "running" });
  s.apply(ev(6, "tool_result", { callId: "c1", status: "ok", output: "" }));
  expect(s.latest()).toMatchObject({ seq: 5, status: "ok" });
  s.apply(ev(7, "message", { kind: "chat", text: "Two files changed.", delivery: "none" }));
  expect(s.latest()).toMatchObject({ seq: 7, text: "Two files changed.", status: "message" });
});
it("shows a result whose call it never saw as the result alone (review focus 2)", () => {
  const s = new RoomState();
  s.apply(ev(9, "tool_result", { callId: "lost", status: "error", output: "boom" }));
  expect(s.latest()).toMatchObject({ seq: 9, text: "boom", status: "error" });
});
```

(`ev` is the file's existing event helper; add one if the file has none, with `actor: { kind: "agent", id: "agent:ikely2yk", role: "implementer" }`.) In `strip.test.ts`:

```ts
it("prints the Now line with a relative time and links to the row", () => {
  const { h, strip } = mount();
  let clicked = 0;
  strip.onNowClick((seq) => { clicked = seq; });
  strip.update({ roomId: "3kq7x2ma", snap, phase: "Active", driver: snap.driver, needs: 0, connection: "live",
    now: { seq: 27, text: "terminal git log --oneline -3", status: "running", ts: new Date(Date.now() - 12_000).toISOString() } });
  expect(h.querySelector(".now")?.textContent).toBe("Now ▸ terminal git log --oneline -3 · running · 12 s ago");
  (h.querySelector(".now button") as HTMLButtonElement).click();
  expect(clicked).toBe(27);
});
it("says there is no run when nothing is running", () => {
  const { h, strip } = mount();
  strip.update({ roomId: "3kq7x2ma", snap, phase: "Open", driver: snap.driver, needs: 0, connection: "live" });
  expect(h.querySelector(".now")?.textContent).toBe("No run yet");
});
```

- [ ] **Step 2: Run them**. Expected: FAIL (`latest` not a function; `.now` null).

- [ ] **Step 3: Implement.** In `RoomState`, keep `private last?: Latest` and `private calls = new Map<string, Latest>()`. On `tool_call`: `last = { seq, text: `${tool} ${args.command ?? JSON.stringify(args)}`.trim(), status: "running", ts, callId }` and store in `calls` by `callId`. On `tool_result`: if `calls.has(callId)`, update that entry's `status` to `payload.status ?? "ok"` and, if it is `last`, keep `last` pointing at it; else `last = { seq, text: first line of output, status, ts }`. On `message` from an agent or human: `last = { seq, text, status: "message", ts }`. `latest()` returns `last`. In `strip.ts`, render `div.now` with the text `Now ▸ ${text} · ${status} · ${ago}` where `ago` is `Nowish(ts)` (`12 s ago`, `4 min ago`, `2 h ago`), inside a `button` so it is focusable; `onNowClick` stores the handler. With no `now`, the line reads `No run yet` and, when `snap.you.role` is `owner` or `snap.you.driver`, ` · start one under Run`. In `room.ts`, `onNowClick` does `document.querySelector<HTMLElement>(\`[data-seq="${seq}"]\`)?.scrollIntoView({ block: "center" })` and opens it if it is a `<details>`; a `setInterval(refreshStrip, 1000)` keeps the relative time current, cleared when the page stops.

- [ ] **Step 4: Run**: `npm test`; `task ui:build`. Expected: PASS.

- [ ] **Step 5: Commit**:

```bash
git add web internal/humanapi/ui/dist
git commit -m "feat(web): a Now line says what the agent is doing, with a link to the row"
```

---

### Task 6: The needs-you panel holds the approval cards, command first

**Files:**
- Create: `web/src/needs.ts`, `web/test/needs.test.ts`
- Modify: `web/src/controls.ts:101-114` (`approvalCard` layout), `:160-178` (remove the approvals list from the controls), `web/src/summary.ts` (export `needsItems(s)`, `renderNotes(root, s)`; delete `renderSummary`, `needsYouBlock`), `web/src/room.ts` (mount the panel between the strip and `main`), `web/src/render.ts:837-839` (unchanged call, new layout), `web/test/summary.test.ts`, `web/test/controls.test.ts` (approval tests move to `needs.test.ts`)

**Interfaces:**
- Produces:
  ```ts
  export function mountNeeds(root: HTMLElement, conn: Sender, state: RoomState, you: () => Snapshot["you"] | undefined,
    chat: { rowFor(callId: string): HTMLElement | undefined }): { refresh(): void; setSummary(s: Summary): void; count(): number }
  ```
  `approvalCard(a, tag)` keeps its signature; its first child is now `pre.approval-command` holding `a.action.command` when it is a string, else the class.
- Consumes: `RoomState.approvals()`, `approvalAnchor`, `focusApproval`, `ChatView.rowFor` (add: `rowFor(callId) { return this.rows.get(this.calls.get(callId)?.seq ?? -1); }` and expose it through `Views`).

- [ ] **Step 1: Write the failing tests** `web/test/needs.test.ts`:

```ts
import { describe, expect, it } from "vitest";
import { mountNeeds } from "../src/needs";
import { RoomState } from "../src/room-state";
import type { Summary } from "../src/summary";

const you = { principal: "human:a", role: "owner", approver: true, driver: true, webUI: true };
const approval = { approvalId: "01M4A", runId: "ikely2yk", callId: "c9", class: "forge.other",
  action: { kind: "TerminalAction", command: "git push origin agent/ibbay5ud", timeout: 600 }, expiresAt: "2026-10-15T15:18:52Z", seq: 13 };
const sum = (needs: Summary["needsYou"]): Summary => ({ apiVersion: "summary/v1", room: "r", url: "u",
  status: { phase: "Implementing", run: null, budget: null, pr: null, issue: null, lastVerdict: null }, needsYou: needs, actions: [],
  notes: { untrusted: true, items: [] }, cursor: "seq:1" });
function mount(state = new RoomState(), sent: Record<string, unknown>[] = []) {
  const root = document.createElement("section");
  const row = document.createElement("details"); row.dataset.callId = "c9";
  const needs = mountNeeds(root, { send: (f) => { sent.push(f); return true; } }, state, () => you, { rowFor: (id) => (id === "c9" ? row : undefined) });
  return { root, needs, sent };
}

describe("the needs-you panel", () => {
  it("leads an approval card with its command, keeps the JSON behind a toggle, links the call", () => {
    const state = new RoomState(); state.reset({ driver: "human:a", driverEpoch: 1, approvals: [approval] }, 20);
    const { root, needs } = mount(state); needs.refresh();
    const card = root.querySelector("#approval-01M4A")!;
    expect(card.querySelector(".approval-command")?.textContent).toBe("git push origin agent/ibbay5ud");
    expect(card.querySelector("details.approval-json pre")?.textContent).toContain('"timeout": 600');
    expect(card.querySelector("a.approval-call")?.textContent).toBe("see the call");
    expect(card.querySelector('button[data-act="approve"]')?.classList.contains("primary")).toBe(true);
    expect(card.querySelector('input[name="decisionReason"]')?.getAttribute("aria-label")).toBe("reason for the decision (optional)");
    expect(needs.count()).toBe(1);
  });
  it("sends a decide act and drops the card when the log decides it (review focus 3)", () => {
    const state = new RoomState(); state.reset({ driver: "human:a", driverEpoch: 1, approvals: [approval] }, 20);
    const { root, needs, sent } = mount(state); needs.refresh();
    (root.querySelector('button[data-act="approve"]') as HTMLButtonElement).click();
    expect(sent.at(-1)).toMatchObject({ type: "act", action: { kind: "decide", approvalId: "01M4A", decision: "approved" } });
    state.apply({ v: 1, id: "x", seq: 21, roomId: "r", actor: { kind: "system", id: "system:policy" }, type: "approval_decided", origin: "broker",
      ts: "2026-10-15T15:00:00Z", redactions: [], payload: { approvalId: "01M4A", decision: "approved" } });
    needs.refresh();
    expect(root.querySelector("#approval-01M4A")).toBeNull();
    expect(needs.count()).toBe(0);
  });
  it("lists a merge and a hold from the summary, and is empty when nothing needs you", () => {
    const { root, needs } = mount();
    needs.setSummary(sum([{ kind: "merge", id: "77", what: "a maintainer with admin merges PR #77", url: "https://github.com/o/r/pull/77" },
      { kind: "hold", id: "waiting_human_driver", what: "hand the driver back", url: "u" }]));
    expect([...root.querySelectorAll("li")].map((l) => l.textContent)).toEqual(["Merge: a maintainer with admin merges PR #77 · open the PR", "Hold: hand the driver back"]);
    expect(needs.count()).toBe(2);
    needs.setSummary(sum([])); needs.refresh();
    expect(root.hidden).toBe(true);
  });
  it("renders read-only for a watcher who may not decide", () => {
    const state = new RoomState(); state.reset({ driver: "human:a", driverEpoch: 1, approvals: [approval] }, 20);
    const root = document.createElement("section");
    const needs = mountNeeds(root, { send: () => true }, state, () => ({ ...you, role: "watcher", approver: false }), { rowFor: () => undefined });
    needs.refresh();
    expect(root.querySelector("button")).toBeNull();
    expect(root.querySelector(".approval-command")?.textContent).toBe("git push origin agent/ibbay5ud");
  });
});
```

- [ ] **Step 2: Run them**. Expected: FAIL (module not found).

- [ ] **Step 3: Implement `web/src/needs.ts`**: a `section.needs` with `h2` "Needs you", a `ul.approvals` keyed by approval id (reuse `keyed()` from `controls.ts`: export it), and a `ul.needs-other` for merge and hold from the last summary. Each approval card is `approvalCard(a, "li")` with `id = approvalAnchor(a.approvalId)`, plus for a viewer who may decide (`you.approver || you.role === "owner"`, and not sealed) the labelled reason input, `button.primary[data-act=approve]` and `button[data-act=deny]`, sending `act(conn, state, { kind: "decide", approvalId, decision, reason? })`. `approvalCard` becomes: `pre.approval-command` (`action.command` if a string, else `a.class`), `div.approval-meta` (`${class} · run ${runId} · expires ${when}`), `a.approval-call` ("see the call", `href="#"`, `onclick` → `chat.rowFor(callId)?.scrollIntoView(); preventDefault()`, only when a row exists), `details.approval-json > summary "action" + pre` (the JSON). `count()` = cards + other items. `root.hidden = count() === 0`. In `room.ts`, mount it after the strip, call `needs.refresh()` from `onState`, `onEvent` and `onAck`, `needs.setSummary(sum)` after each summary fetch, and feed `needs.count()` to the strip. Remove the approvals list and `renderApprovals` from `controls.ts` (keep `approvalCard`, now shared). In `summary.ts`, replace `renderSummary` with `renderNotes(root, s)` (Task 7 styles it) and `needsItems(s)` returning merge and hold entries; drop `needsYouBlock` and `statusBlock`. Update `web/test/summary.test.ts` accordingly (keep the hold and merge expectations against `needsItems`).

- [ ] **Step 4: Styles**: `.needs { padding: 8px 16px; }`, `.approval { border: 1px solid var(--warning-border); border-radius: 6px; padding: 8px 12px; display: grid; gap: 6px; }`, `.approval-command { font: 14px ui-monospace, monospace; margin: 0; white-space: pre-wrap; overflow-wrap: anywhere; }`, `.approval-meta { color: var(--muted-foreground); font-size: 12px; }`, `button.primary { background: var(--primary); color: var(--primary-foreground); border-color: var(--primary); }`.

- [ ] **Step 5: Run**: `npm run typecheck && npm test`; `task ui:build`. Expected: PASS.

- [ ] **Step 6: Commit**:

```bash
git add web internal/humanapi/ui/dist
git commit -m "feat(web): approvals are decided where they are read, command first"
```

---

### Task 7: The stream folds its plumbing

**Files:**
- Modify: `web/src/chat.ts` (fold groups, chips, the fork toolbar, actor and time), `web/src/render.ts:793-799` (`deliveryChip` returns `null` for `none`), `web/src/summary.ts` (`renderNotes`: latest inline + `details`), `web/src/app.css`
- Test: `web/test/chat.test.ts`, `web/test/render.test.ts`, `web/test/summary.test.ts`

**Interfaces:**
- Produces: `ChatView.rowFor(callId)`; `deliveryChip(p): HTMLElement | null`; CSS classes `.fold`, `.row-tools`, `.swatch`.
- Rule: a `state_changed` or `turn` or `participant` or `driver` row is "plumbing" unless `payload.kind` is `harness_error` or `limit`, or the event is a `driver` change where `from` or `to` is the viewer.

- [ ] **Step 1: Write the failing tests** in `web/test/chat.test.ts` (use the file's existing `apply`/`ev` helpers):

```ts
it("folds consecutive plumbing rows into one line per run of them", () => {
  const { el, view } = mount();
  view.apply(ev(1, "state_changed", { kind: "room_phase", phase: "Open" }));
  view.apply(ev(2, "participant", { principal: "agent:ikely2yk", change: "joined", role: "implementer" }));
  view.apply(ev(3, "turn", { turnId: "t1", phase: "started" }));
  view.apply(ev(4, "message", { kind: "chat", text: "hi", delivery: "none" }));
  view.apply(ev(5, "state_changed", { kind: "harness_status", status: "running", previous: "idle" }));
  const rows = [...el.children];
  expect(rows.map((r) => r.className.split(" ")[0])).toEqual(["fold", "msg", "mark"]);
  expect(rows[0].querySelector("summary")?.textContent).toBe("· 3 changes");
  expect(rows[0].querySelectorAll(".mark, .sys").length).toBe(3);
});
it("never folds a harness error or a seal, and keeps a replaced seq in place (review focus 4)", () => {
  const { el, view } = mount();
  view.apply(ev(1, "turn", { turnId: "t1", phase: "started" }));
  view.apply(ev(2, "state_changed", { kind: "harness_error", code: "E1", detail: "boom" }));
  view.apply(ev(1, "turn", { turnId: "t1", phase: "started" })); // a clamped resume resends seq 1
  expect([...el.children].map((r) => r.className.split(" ")[0])).toEqual(["mark", "mark"]);
  expect(el.querySelectorAll('[data-seq="1"]').length).toBe(1);
  expect(el.querySelector(".mk-harness_error")?.textContent).toContain("harness error E1: boom");
});
it("hides the policy chip on an allowed call and the chat chip on a room message", () => {
  const { el, view } = mount();
  view.apply(ev(1, "message", { kind: "chat", text: "hi", delivery: "none" }));
  view.apply(ev(2, "tool_call", { callId: "c1", tool: "terminal", args: { command: "ls" } }));
  view.apply(ev(3, "state_changed", { kind: "policy_decision", decision: "allow", callId: "c1" }));
  expect(el.querySelector(".msg .chip-delivery")).toBeNull();
  expect(el.querySelector(".tool .pol")).toBeNull();
  view.apply(ev(4, "state_changed", { kind: "policy_decision", decision: "deny", callId: "c1" }));
  expect(el.querySelector(".tool .pol")?.textContent).toBe("policy: deny");
});
it("puts one fork action beside a message, none on a tool row or a marker, outside the summary", () => {
  const forks: number[] = [];
  const { el, view } = mount((seq) => forks.push(seq));
  view.apply(ev(1, "message", { kind: "chat", text: "hi", delivery: "none" }));
  view.apply(ev(2, "tool_call", { callId: "c1", tool: "terminal", args: { command: "ls" } }));
  view.apply(ev(3, "turn", { turnId: "t1", phase: "started" }));
  const buttons = [...el.querySelectorAll("button.fork-here")];
  expect(buttons.length).toBe(1);
  expect(buttons[0].closest("summary")).toBeNull();
  expect(buttons[0].getAttribute("aria-label")).toBe("fork here, at #1");
  (buttons[0] as HTMLButtonElement).click();
  expect(forks).toEqual([1]);
});
it("names the actor as role · id with a swatch, and 'you' for the viewer", () => {
  const { el, view } = mount(undefined, "human:291847362183");
  view.apply(ev(1, "message", { kind: "chat", text: "hi", delivery: "none" }, { kind: "agent", id: "agent:ikely2yk", role: "implementer" }));
  view.apply(ev(2, "message", { kind: "chat", text: "yo", delivery: "none" }, { kind: "human", id: "human:291847362183" }));
  const heads = [...el.querySelectorAll(".msg-head .who")].map((h) => h.textContent);
  expect(heads).toEqual(["implementer · ikely2yk", "you"]);
  expect(el.querySelector(".msg-head .swatch")).not.toBeNull();
  expect(el.querySelector("time")?.getAttribute("datetime")).toBe("2026-10-06T10:00:00Z");
});
```

(`mount(fork?, you?)` is the file's helper, extended with a `you` principal passed to `new ChatView(el, undefined, fork, you)`.)

- [ ] **Step 2: Run them**. Expected: FAIL.

- [ ] **Step 3: Implement in `chat.ts`.** Add `plumbing(ev): boolean` per the rule above. In `put(seq, row)`, before inserting a plumbing row: find the neighbour at `seqs[i - 1]`; if its element (`this.rows.get(prev)`) sits inside a `.fold-body`, append the new row there; else if that element is itself plumbing (`row.dataset.plumbing === "1"`), create `details.fold > summary + div.fold-body`, move the neighbour in, insert the fold at the neighbour's position, append the new row; else insert normally. `rows.get(seq)` keeps pointing at the inner row, so a replace-in-place (`old.replaceWith(row)`) still works inside a fold; after a replace or an append, re-count the fold's summary: `· ${n} changes`. `drop(seq)` removes an inner row and dissolves a fold left with one child. `toolRow` appends `polBadge` only when `label !== "allow"` or `decidedBy` is set. `bubble` appends `deliveryChip(p)` only when it is not `null`. The fork action: `put` wraps `bubble` and `handoff` rows in `div.row` with a sibling `div.row-tools` holding `button.fork-here` (`aria-label` `fork here, at #${seq}`, `type=button`); tool rows and markers get no button. `who(ev)` returns `you` when `ev.actor.id === this.you`, else `${role} · ${idTail}` where `idTail` is the id after `agent:`/`human:`/`system:`, shortened as `shortPrincipal` does for humans; a `span.swatch` precedes it, coloured by `swatchClass(id)` (one of six classes, `hash(id) % 6`). `time()` sets `datetime = ts`. Add `rowFor(callId)`. In `render.ts`, `deliveryChip` returns `null` for `delivery === "none"`; `renderEvent` guards the append. `renderNotes` in `summary.ts`: a `div.notes` with `p.note-latest` (`${run} · ${ago} · ${text}` plus a `span.hint` "the agent's claim") and `details > summary "Notes (N)" + ul` for the rest; absent when there are no notes.

- [ ] **Step 4: Styles**: `.fold { color: var(--muted-foreground); font-size: 12px; }`, `.fold > summary { cursor: pointer; list-style: none; }`, `.fold-body { padding-left: 14px; }`, `.row { position: relative; }`, `.row-tools { position: absolute; right: -4px; top: 0; opacity: 0; }`, `.row:hover .row-tools, .row:focus-within .row-tools { opacity: 1; }`, `.swatch { display: inline-block; width: 8px; height: 8px; border-radius: 2px; margin-right: 6px; }`, `.swatch-0 … .swatch-5` from `--primary`, `--accent`, `--warning`, `--destructive`, `--link`, `--muted-foreground`. `.mark { max-width: 72ch; }` so marker times align with bubbles.

- [ ] **Step 5: Run**: `npm run typecheck && npm test`; `task ui:build`. Expected: PASS (fix the existing `chat.test.ts` and `render.test.ts` expectations that asserted the old `chat` chip and the per-row fork button).

- [ ] **Step 6: Commit**:

```bash
git add web internal/humanapi/ui/dist
git commit -m "feat(web): the stream folds plumbing, drops redundant chips and names actors"
```

---

### Task 8: Named control groups, the dock, the sealed room and the states

**Files:**
- Modify: `web/src/controls.ts` (groups, `confirmButton`, the composer leaves), `web/src/room.ts` (the dock, the banner, `sealedBy`), `web/src/room-state.ts` (`closed`), `web/src/app.css`
- Test: `web/test/controls.test.ts`, `web/test/room.test.ts`, `web/test/room-state.test.ts`

**Interfaces:**
- Produces: `RoomState.closed?: { who: string; at: string }` set from `state_changed{room_phase: Closed}` (`actor.id`, `ts`); `export function confirmButton(label: string, act: string, onConfirm: () => void): HTMLSpanElement` (renders `[label]`; on click, `[confirm: label] [cancel]`, resetting after 10 s or on cancel); `mountComposer(root, conn, state, you, say): { refresh(): void; onAck(f: Frame): void; setLive(live: boolean): void }` split out of `mountControls`; `mountControls` keeps `refresh()` and `onAck()`.

- [ ] **Step 1: Write the failing tests.** In `web/test/controls.test.ts`:

```ts
it("groups the controls under Run, Driver and Room, with Room behind a toggle", () => {
  const { root } = mountOwner();
  expect([...root.querySelectorAll(":scope > section > h2, :scope > details > summary")].map((h) => h.textContent)).toEqual(["Run", "Driver", "Room"]);
  expect(root.querySelector("details.group-room")?.open).toBe(false);
});
it("confirms interrupt, take, close and steer-now inline, with a cancel", () => {
  const { root, sent } = mountOwner();
  const close = root.querySelector('button[data-act="close"]') as HTMLButtonElement;
  close.click();
  expect(root.querySelector('button[data-act="close"]')?.textContent).toBe("confirm: close room");
  (root.querySelector('button[data-act="close-cancel"]') as HTMLButtonElement).click();
  expect(root.querySelector('button[data-act="close"]')?.textContent).toBe("close room");
  expect(sent).toEqual([]);
  close.click(); (root.querySelector('button[data-act="close"]') as HTMLButtonElement).click();
  expect(sent.at(-1)).toMatchObject({ action: { kind: "close" } });
  expect(root.querySelector('button[data-act="interrupt"]')?.classList.contains("destructive")).toBe(true);
});
it("shows the PR URL only for a reviewer and the egress field under advanced", () => {
  const { root } = mountOwner();
  const role = root.querySelector('select[name="role"]') as HTMLSelectElement;
  expect((root.querySelector('input[name="prUrl"]') as HTMLInputElement).hidden).toBe(true);
  role.value = "reviewer"; role.dispatchEvent(new Event("change"));
  expect((root.querySelector('input[name="prUrl"]') as HTMLInputElement).hidden).toBe(false);
  expect(root.querySelector('input[name="egress"]')?.closest("details")?.querySelector("summary")?.textContent).toBe("advanced");
});
it("hides Run and Driver once the room is sealed", () => {
  const { root, state, controls } = mountOwner();
  state.apply(ev(9, "state_changed", { kind: "room_phase", phase: "Closed", reason: "owner" }));
  controls.refresh();
  expect((root.querySelector(".group-run") as HTMLElement).hidden).toBe(true);
  expect((root.querySelector(".group-driver") as HTMLElement).hidden).toBe(true);
});
```

In `web/test/room.test.ts`:

```ts
it("docks the composer at the bottom and disables Send while reconnecting or sealed", () => {
  const p = page();
  p.join(snapshot({ role: "collaborator" }), 0);
  const dock = p.app.querySelector("footer.page-dock")!;
  expect(dock.querySelector("textarea")).not.toBeNull();
  expect((dock.querySelector('button[data-act="message"]') as HTMLButtonElement).disabled).toBe(false);
  p.last().drop(1006);
  expect((dock.querySelector('button[data-act="message"]') as HTMLButtonElement).disabled).toBe(true);
  expect(p.app.querySelector(".banner")?.textContent).toMatch(/^Reconnecting in \d+ s: connection lost$/);
});
it("keeps a steering draft and moves the notice beside the select when the token is lost (review focus 5)", () => {
  const p = page();
  p.join(snapshot({ role: "collaborator", driver: true }), 0);
  const dock = p.app.querySelector("footer.page-dock")!;
  const text = dock.querySelector("textarea") as HTMLTextAreaElement;
  const delivery = dock.querySelector('select[name="delivery"]') as HTMLSelectElement;
  text.value = "turn left"; delivery.value = "steering";
  p.last().recv(event(1, "driver", { from: "human:b", to: "human:c", epoch: 2 }));
  expect(text.value).toBe("turn left");
  expect(delivery.value).toBe("");
  expect(delivery.getAttribute("aria-describedby")).toBe("delivery-notice");
  expect(dock.querySelector("#delivery-notice")?.textContent).toMatch(/choose where this message goes/);
  expect((dock.querySelector('button[data-act="message"]') as HTMLButtonElement).disabled).toBe(false);
});
it("reads Closed by whom in the strip and hides the composer", () => {
  const p = page();
  p.join(snapshot({ role: "owner" }), 0);
  p.last().recv({ ...event(1, "state_changed", { kind: "room_phase", phase: "Closed", reason: "owner" }), event: { ...event(1, "state_changed", {}).event,
    actor: { kind: "human", id: "human:b" }, payload: { kind: "room_phase", phase: "Closed", reason: "owner" } } });
  expect(p.app.querySelector(".closed")?.textContent).toMatch(/^Closed .* by you$/);
  expect((p.app.querySelector("footer.page-dock textarea") as HTMLElement).hidden).toBe(true);
});
```

- [ ] **Step 2: Run them**. Expected: FAIL.

- [ ] **Step 3: Implement.** `controls.ts`: `confirmButton` as specified (the confirm state uses `data-act` unchanged plus a sibling `button[data-act="<act>-cancel"]`; `setTimeout` 10 s resets). Groups: `section.group-run > h2 "Run" + form.hand` (the role select labelled "Start a run as", `prUrl` hidden unless `role.value === "reviewer"`, `egress` inside `details > summary "advanced"`); `section.group-driver > h2 "Driver" + div.driver` (holder text without `epoch`; `give` with the member input; `request`; `take` through `confirmButton`; `interrupt` through `confirmButton` with class `destructive`); `ul.queue` with `steer now` through `confirmButton`; `details.group-room > summary "Room"` holding the members list (`ul.members` from `state.members()`: add to `RoomState` from `participant` events for humans, `principal`, `role`, `approver`), the invite form, the stop instruction (`p`: "Stop the task: label the issue " + link `#74` from the summary + " `factory/stop`", rendered when `setSummary` sees an issue) and `confirmButton("close room", "close", …)`. `refresh()` hides `group-run` and `group-driver` when `state.sealed`, `group-room` when not owner. The composer moves to `mountComposer`, mounted by `room.ts` into `footer.page-dock` (`textarea`, `select[name=delivery]` labelled "Deliver to" with `aria-describedby="delivery-notice"`, `span#delivery-notice.notice`, `button[data-act=message]`, then the theme button and `span.status`); `setLive(false)` disables Send and `hidden`s nothing; `state.sealed` hides the textarea, select and Send. `room.ts`: `div.banner` under the header, text from `onStatus` when not `live`, hidden when live; `onStatus` also calls `composer.setLive(s === "live")`; the counters move to the Raw events `summary` text (`Raw events · seq 27 · gaps 0 · dups 0`); the dock's status reads `live · ${last} events`. `RoomState.apply` records `closed = { who: ev.actor.id, at: ev.ts }` on `room_phase: Closed`; the strip's `sealedBy` reads it (Task 4's `closed` line prints `Closed ${when(at)} by ${who === you ? "you" : shortPrincipal(who)}`).

- [ ] **Step 4: Styles**: `.controls section, .controls details { border-top: 1px solid var(--border); padding: 8px 0; }`, `.controls h2, .controls summary { font-size: 13px; text-transform: uppercase; letter-spacing: .04em; color: var(--muted-foreground); margin: 0 0 6px; }`, `button.destructive { color: var(--destructive); border-color: color-mix(in srgb, var(--destructive) 45%, var(--border)); }`, `.banner { background: color-mix(in srgb, var(--warning) 18%, var(--card)); color: var(--warning-foreground); padding: 6px 16px; }`, `.page-dock { display: grid; grid-template-columns: 1fr auto; gap: 6px 12px; align-items: center; }`, `.page-dock textarea { grid-column: 1 / -1; }`.

- [ ] **Step 5: Run**: `npm run typecheck && npm test`; `task ui:build`. Expected: PASS (update the controls tests that asserted the old flat layout and the composer inside `.controls`).

- [ ] **Step 6: Commit**:

```bash
git add web internal/humanapi/ui/dist
git commit -m "feat(web): named control groups, a docked composer, confirms on destructive acts, a sealed room that says so"
```

---

### Task 9: The room list is a table

**Files:**
- Modify: `web/src/view.ts:36-78` (`RoomRow`, `rowText` → `rowCells`, `listRooms`), `web/src/app.css`
- Test: `web/test/view.test.ts`

**Interfaces:**
- Produces: `RoomRow.lastEventAt?: string`; `export function rowCells(row: RoomRow, now: Date): { needs: string; task: string; repo: string; phase: string; pr: string; last: string; id: string }`; `export function sortRooms(rows: RoomRow[]): RoomRow[]` (needs-you count desc, then `lastEventAt` desc, then id); `listRooms(app, get, filters?: { mine?: boolean; needsMe?: boolean; repo?: string })`.

- [ ] **Step 1: Write the failing tests** in `web/test/view.test.ts`:

```ts
describe("the room table", () => {
  const rows = [
    { id: "d66mo4b4", phase: "Idle", owner: "human:5", driver: "human:5", dataClass: "public", lastSeq: 95, repository: "o/a", needsYou: 0, lastEventAt: "2026-10-15T09:00:00Z" },
    { id: "3kq7x2ma", phase: "Active", owner: "human:2", driver: "human:2", dataClass: "internal", lastSeq: 142, repository: "o/b", issue: 74, title: "Chat view", task: "Implementing", pr: 77, needsYou: 2, lastEventAt: "2026-10-15T08:00:00Z" },
    { id: "k2p9rt4v", phase: "Active", owner: "human:2", driver: "system:factory", dataClass: "public", lastSeq: 318, repository: "o/a", issue: 81, title: "Bump mako", task: "Reviewing", pr: 83, needsYou: 0, lastEventAt: "2026-10-15T09:30:00Z" },
  ];
  it("sorts needs-you first, then the most recent activity", () => {
    expect(sortRooms(rows).map((r) => r.id)).toEqual(["3kq7x2ma", "k2p9rt4v", "d66mo4b4"]);
  });
  it("renders one row per room with the task first and a needs pill", async () => {
    const get = vi.fn(async (url: string) => new Response(JSON.stringify(url.includes("needs_me=1") ? [rows[1]] : rows), { status: 200 }));
    const root = app();
    await listRooms(root, get as unknown as typeof fetch);
    const cells = [...root.querySelectorAll("tbody tr")].map((tr) => [...tr.querySelectorAll("td")].map((td) => td.textContent));
    expect(cells[0][0]).toBe("2");
    expect(cells[0][1]).toBe("#74 Chat view");
    expect(cells[0][2]).toBe("o/b");
    expect(cells[2][1]).toBe("Room d66mo4b4");
    expect(root.querySelector("thead")?.textContent).toContain("Last activity");
    (root.querySelector('input[name="needsMe"]') as HTMLInputElement).click();
    await vi.waitFor(() => expect(root.querySelectorAll("tbody tr").length).toBe(1));
    expect(get).toHaveBeenLastCalledWith(expect.stringContaining("needs_me=1"), expect.anything());
  });
  it("keeps the new-room form behind a button and the CLI setup below the table", async () => {
    const root = app();
    await listRooms(root, (async () => new Response("[]", { status: 200 })) as unknown as typeof fetch);
    expect((root.querySelector("form.new-room") as HTMLElement).hidden).toBe(true);
    (root.querySelector('button[data-act="new-room"]') as HTMLButtonElement).click();
    expect((root.querySelector("form.new-room") as HTMLElement).hidden).toBe(false);
    expect(root.lastElementChild?.className).toBe("cli-setup");
  });
});
```

- [ ] **Step 2: Run them**. Expected: FAIL.

- [ ] **Step 3: Implement.** `listRooms` renders `header.page-header` (h1 "Rooms", the theme button), a `div.list-tools` (two checkboxes `mine`, `needsMe` labelled "mine" and "needs me", a `select[name=repo]` built from the rows' repositories with an "all repositories" option, and `button[data-act=new-room]`), the hidden `newRoomForm`, a `table.rooms` with `thead` (Needs you · Task · Repository · Phase · PR · Last activity · Room) and one `tr` per `sortRooms(rows)` with `rowCells`, then `cliSetup`. A filter change refetches with `?mine=1`, `?needs_me=1`, `?repo=owner/name` and re-renders `tbody` only. `rowCells`: `needs` = the count or `""` (rendered as `span.needs-pill` when non-empty), `task` = `#${issue} ${title}` else `Room ${id}` (a link to `/r/${id}`), `phase` = `task ?? phase`, `pr` = `#${pr}` or `""`, `last` = relative from `lastEventAt` with the absolute as `title` or `""`, `id` in `code`. Delete `rowText` and its tests. The error texts become sentences: `The room list is unavailable (503). Retry.`

- [ ] **Step 4: Styles**: `table.rooms { width: 100%; border-collapse: collapse; background: var(--card); box-shadow: var(--shadow-border); border-radius: 8px; }`, `.rooms th, .rooms td { text-align: left; padding: 6px 10px; border-bottom: 1px solid var(--border); vertical-align: baseline; }`, `.rooms th { font-size: 12px; color: var(--muted-foreground); text-transform: uppercase; letter-spacing: .04em; }`, `.list-tools { display: flex; gap: 12px; align-items: center; padding: 8px 16px; flex-wrap: wrap; }`. At 390 px: `.rooms th:nth-child(n+4), .rooms td:nth-child(n+4) { display: none; }` under `@media (max-width: 600px)`.

- [ ] **Step 5: Run**: `npm run typecheck && npm test`; `task ui:build`. Expected: PASS.

- [ ] **Step 6: Commit**:

```bash
git add web internal/humanapi/ui/dist
git commit -m "feat(web): the room list is a table, needs-you first, with the API's filters"
```

---

### Task 10: A full-page offline preview

**Files:**
- Create: `web/preview/full.ts`, `web/preview/full.html`, `web/test/fixtures/summary.json`, `web/test/fixtures/snapshot.json`
- Modify: `web/package.json` (scripts), `web/preview/serve.mjs:850` (print both URLs), `.gitignore` (`web/preview/full.js`), `docs/development.md` (one section)

**Interfaces:**
- Consumes: `mountRoom` is not reusable offline (it dials a socket), so `full.ts` composes the modules as `room.ts` does: `mountStrip`, `mountNeeds`, `mountViews`, `mountControls`, `mountComposer`, `mountFork`, fed by the fixtures.

- [ ] **Step 1: Write the fixtures.** `summary.json`: the object from `strip.test.ts`'s `summary()` with two `needsYou` items (one approval `01M4A`, one merge) and four notes. `snapshot.json`: `{ roomId, phase: "Active", driver, driverEpoch: 3, dataClass: "internal", you: {owner, approver, driver}, queue: [two items], approvals: [the 01M4A approval], sealed: false }`.

- [ ] **Step 2: Write `full.ts`**: load the three fixtures, mount every region in the order of `room.ts`, apply the event fixture through `views.apply` and `state.apply`, call `needs.setSummary`, `strip.update`; `?theme=dark` adds the class; `?width=390` is for the reader (resize the window). `full.html` mirrors `preview.html` with `full.js`. `package.json`: `"preview:build": "esbuild preview/preview.ts preview/full.ts --bundle --format=esm --target=es2022 --outdir=preview"`; `serve.mjs` prints `/preview/preview.html` and `/preview/full.html`.

- [ ] **Step 3: Check it by hand**: `cd web && npm run preview`, open `/preview/full.html` at 1280 and 390 px, light and dark. The five questions of the spec must answer above the fold at 1280×800; no horizontal scroll at 390.

- [ ] **Step 4: Document** in `docs/development.md`, a section `## The UI preview`: two sentences (what `npm run preview` serves, that it needs no broker) and the two URLs.

- [ ] **Step 5: Run**: `npm run typecheck && npm test`; `task ui:check` (the preview outputs are git-ignored, so the bundle is unchanged). Expected: PASS.

- [ ] **Step 6: Commit**:

```bash
git add web .gitignore docs/development.md
git commit -m "feat(web): an offline full-page preview of the room and the list"
```

---

### Task 11: `roomctl` tells the same story

**Files:**
- Modify: `internal/roomctl/status.go:34-50`, `internal/app/roomctl.go:43,148`, `internal/roomctl/watch.go` (the `Line` function that prints `id (role)`)
- Test: `internal/roomctl/roomctl_test.go` (`TestRenderSummary`), `internal/roomctl/watch_test.go`

- [ ] **Step 1: Write the failing test** in `internal/roomctl/roomctl_test.go`, next to `TestRenderSummary`:

```go
func TestStatusLeadsWithTheTaskAndReadsTheBudget(t *testing.T) {
	raw := []byte(`{"apiVersion":"summary/v1","room":"r","url":"u","status":{"phase":"Implementing","run":{"id":"cf4ato2x","role":"implementer"},
		"budget":{"usedTokens":189093,"limitTokens":1500000},"pr":null,"issue":{"number":74,"url":"https://github.com/o/r/issues/74","title":"Chat view: pair tool calls"},
		"lastVerdict":null},"needsYou":[],"actions":[],"notes":{"untrusted":true,"items":[]},"cursor":"seq:27"}`)
	var out strings.Builder
	if err := roomctl.RenderSummary(&out, raw, time.Now()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if lines[0] != `task: #74 "Chat view: pair tool calls"` {
		t.Fatalf("first line %q", lines[0])
	}
	if !strings.Contains(lines[1], "budget: 189 k / 1.5 M (13 %)") {
		t.Fatalf("budget line %q", lines[1])
	}
}
```

- [ ] **Step 2: Run it**: `go test ./internal/roomctl/ -run TestStatusLeadsWithTheTaskAndReadsTheBudget`. Expected: FAIL.

- [ ] **Step 3: Implement.** In `RenderSummary`, when `s.Status.Issue != nil`, write `task: #%d %q\n` first (the title through `oneLine`; without a title, `task: #%d`), and drop the later `issue #%d …` line's title (keep the URL line). Budget: `fmt.Sprintf("budget: %s / %s (%d %%)", tokens(u.UsedTokens), tokens(u.LimitTokens), pct)` with `tokens(n int64) string` mirroring `fmtTokens` (`189 k`, `1.5 M`) and `pct = 100 * used / limit` when `limit > 0`. Update `TestRenderSummary`'s expected text. In `internal/app/roomctl.go:43` and `:148`, the help reads "an approval you could decide, a task held on your room, or a PR waiting for your merge". In the watch line, print `role (id)` when the actor has a role; update its test.

- [ ] **Step 4: Run**: `go test -race -count=1 ./internal/roomctl/... ./internal/app/...`. Expected: PASS.

- [ ] **Step 5: Commit**:

```bash
git add internal/roomctl internal/app
git commit -m "fix(roomctl): status leads with the task and reads its budget; help names every need"
```

---

### Task 12: The gate, end to end

- [ ] **Step 1: Run `task check`**. Expected: exit 0. A failing `ui:check` means a bundle was not rebuilt in an earlier task: run `task ui:build`, commit `internal/humanapi/ui/dist` as `chore(web): rebuild the bundle`.
- [ ] **Step 2: Open `npm run preview`'s `/preview/full.html`** at 1280×800 and 390 px wide, both themes, and walk the spec's five-second table: each question answers above the fold.
- [ ] **Step 3: Push the branch and open the PR** with the spec linked and the gate's exit code cited, per `CONTRIBUTING.md`.
