# Adoption, part 1: the story and the live demo, implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the README into a landing page for platform teams ("run coding agents on your Kubernetes cluster without trusting them"), explain a room by analogy, move the internals to the architecture page, and publish a live replay of a recorded run.

**Architecture:**

- **Docs:** README, concepts and architecture pages. `hack/docs.sh` keeps them honest.
- **Demo:** the existing offline preview, factored into one shared page-mounting module and fed by a paced `Replay` player with an injected clock. A static site is assembled by one script, and a pinned GitHub Pages workflow publishes it on release.

**Tech Stack:** Markdown and mermaid; TypeScript 5.9, esbuild, vitest + jsdom; GitHub Actions (`upload-pages-artifact`, `deploy-pages`).

**Spec:** [`docs/superpowers/specs/2026-10-18-adoption-story-design.md`](../specs/2026-10-18-adoption-story-design.md). Read it first: the README's first screen and the room paragraph are copied from it verbatim.

**Base:** start only after PR #148 (room UI polish) has merged. Rebase `docs/adoption-story` onto that `main` first: `git fetch origin && git rebase origin/main`. The demo shows #148's room page, and Task 2 refactors #148's `web/preview/full.ts`.

**Provenance:** Task 1's code was run in isolation (9/9 tests passing, `tsc` clean). The other tasks are written against the files as they stand on `feat/room-ui-polish` and `main` (2026-10-18).

## Global Constraints

- **Copy:**
  - Headline: `Run coding agents on your Kubernetes cluster without trusting them.`
  - The three proof points, the room paragraph and the demo banner: verbatim from the spec.
  - The README's first screen names no SP code, binary, Postgres, digest or CRD group.
  - Every first-screen claim must hold against `docs/security.md` (spec, "Every claim…").
- **Status:** stated once, in the README's Status section. Reader pages carry no status vocabulary (`hack/docs.sh`).
- **The name:** "agent-platform" stays. The project's name appears in the README's H1, the demo's `<title>` and the banner. Part 2 renames it.
- **Demo:**
  - No network beyond its own static files.
  - Every act is inert and says `Demo: nothing is sent.`
  - Every asset path in `demo.html` is relative (Pages serves under `/agent-platform/`).
- **Code:** stdlib vitest only. An injected clock, never `setTimeout` in logic under test. DOM nodes and `textContent` only. No new npm dependency. SPDX header on every new `.ts` file.
- **Actions:** pinned by full commit SHA with the version in a comment, like `.github/workflows/ci.yaml`. `permissions: {}` at the top; least privilege per job.
- **Gates:** `bash hack/docs.sh` and `task check` exit 0. In `web/`, `npm test && npm run typecheck`.
- **Commits:** conventional commits in English, with no `Co-Authored-By` or "Generated with" trailers (the owner's rule).

## File map

| File | Responsibility | Task |
|---|---|---|
| `web/preview/replay.ts` (new) | the paced player: gaps, cap, speed, pause, skip, injected clock | 1 |
| `web/test/replay.test.ts` (new) | its tests, with a fake clock | 1 |
| `web/preview/page.ts` (new) | mounts the room page's regions for an offline page; shared by `full.ts` and `demo.ts` | 2 |
| `web/preview/full.ts` | the developer preview, now on `page.ts`, unchanged in output | 2 |
| `web/test/fixtures/demo-{session.jsonl,snapshot.json,summary.json}` (new) | the curated demo run | 3 |
| `web/test/demo-fixture.test.ts` (new) | the run is gapless, well-formed, inert and tells its story | 3 |
| `web/preview/demo.{html,ts}`, `web/preview/replay-bar.ts` (new) | the demo page, its banner and its controls | 4 |
| `web/test/replay-bar.test.ts` (new) | the controls bar | 4 |
| `web/scripts/demo-site.sh` (new), `web/preview/serve.mjs`, `web/package.json`, `.gitignore` | assemble `_site/`; serve it locally | 5 |
| `.github/workflows/demo.yaml` (new) | publish the site to GitHub Pages on a release tag | 5 |
| `docs/concepts.md`, `docs/architecture.md`, `docs/README.md` | the analogy, the glossary pass, the internals copied in | 6 |
| `README.md`, `docs/assets/room-{dark,light}.png` (new) | the landing page | 7 |

---

### Task 1: The replay player

**Files:**
- Create: `web/preview/replay.ts`
- Test: `web/test/replay.test.ts`

**Interfaces:**
- Consumes: `RoomEvent` from `web/src/conn.ts`.
- Produces:
  - `interface Clock { setTimer(fn, ms): unknown; clearTimer(id): void }` and `windowClock: Clock`
  - `interface ReplayOptions { speed?: number; maxGapMs?: number; clock?: Clock; onChange?: () => void }`
  - `class Replay`: `constructor(events, apply, opts?)`; `play()`, `pause()`, `setSpeed(n)` (throws `RangeError` unless n > 0) and `skip()`; getters `playing`, `done`, `speed` and `applied`
  - defaults: speed 8, maxGapMs 10 000

- [ ] **Step 1: Write the failing test** `web/test/replay.test.ts`

```ts
import { describe, expect, it } from "vitest";
import type { RoomEvent } from "../src/conn";
import { Replay, type Clock } from "../preview/replay";

// fakeClock runs timers when the test advances it, in due order: no real time passes.
function fakeClock() {
  let now = 0;
  let id = 0;
  const timers = new Map<number, { at: number; fn: () => void }>();
  const clock: Clock = {
    setTimer: (fn, ms) => { timers.set(++id, { at: now + ms, fn }); return id; },
    clearTimer: (t) => { timers.delete(t as number); },
  };
  const advance = (ms: number) => {
    const end = now + ms;
    for (;;) {
      const due = [...timers.entries()].filter(([, t]) => t.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
      if (!due) break;
      timers.delete(due[0]);
      now = due[1].at;
      due[1].fn();
    }
    now = end;
  };
  return { clock, advance, pending: () => timers.size };
}

// ev is an event recorded `s` seconds into the run.
const ev = (seq: number, s: number): RoomEvent => ({ v: 1, id: `e${seq}`, seq, roomId: "r", actor: { kind: "agent", id: "agent:x" },
  type: "turn", origin: "harness", ts: new Date(Date.UTC(2026, 9, 18, 7, 0, 0) + s * 1000).toISOString(), redactions: [], payload: {} });

function setup(seconds: number[], speed = 1, maxGapMs = 10_000) {
  const { clock, advance, pending } = fakeClock();
  const seen: number[] = [];
  let changes = 0;
  const r = new Replay(seconds.map((s, i) => ev(i + 1, s)), (e) => seen.push(e.seq), { speed, maxGapMs, clock, onChange: () => changes++ });
  return { r, seen, advance, pending, changes: () => changes };
}

describe("Replay", () => {
  it("applies the first event at once, then each after its recorded gap", () => {
    const t = setup([0, 2, 5]);
    t.r.play();
    t.advance(0);
    expect(t.seen).toEqual([1]);
    t.advance(1999);
    expect(t.seen).toEqual([1]);
    t.advance(1);
    expect(t.seen).toEqual([1, 2]);
    t.advance(3000);
    expect(t.seen).toEqual([1, 2, 3]);
    expect([t.r.done, t.r.playing, t.pending()]).toEqual([true, false, 0]);
  });
  it("divides each gap by the speed", () => {
    const t = setup([0, 8], 8);
    t.r.play();
    t.advance(999);
    expect(t.seen).toEqual([1]);
    t.advance(1);
    expect(t.seen).toEqual([1, 2]);
  });
  it("caps a long idle gap before the speed applies", () => {
    const t = setup([0, 600], 2, 10_000);
    t.r.play();
    t.advance(5000);
    expect(t.seen).toEqual([1, 2]);
  });
  it("treats an out-of-order timestamp as no gap", () => {
    const t = setup([5, 1]);
    t.r.play();
    t.advance(0);
    expect(t.seen).toEqual([1, 2]);
  });
  it("pauses with nothing pending and resumes with the next gap", () => {
    const t = setup([0, 2, 4]);
    t.r.play();
    t.advance(2000);
    t.r.pause();
    expect([t.r.playing, t.pending()]).toEqual([false, 0]);
    t.advance(60_000);
    expect(t.seen).toEqual([1, 2]);
    t.r.play();
    t.advance(2000);
    expect(t.seen).toEqual([1, 2, 3]);
  });
  it("restarts a wait under way at a new speed", () => {
    const t = setup([0, 10]);
    t.r.play();
    t.advance(1000);
    t.r.setSpeed(10); // the 10 s gap is now 1 s, counted from the change
    t.advance(999);
    expect(t.seen).toEqual([1]);
    t.advance(1);
    expect(t.seen).toEqual([1, 2]);
  });
  it("refuses a speed that is not positive", () => {
    expect(() => setup([0]).r.setSpeed(0)).toThrow(RangeError);
  });
  it("skips to the end in order and stays done", () => {
    const t = setup([0, 30, 60]);
    t.r.play();
    t.advance(0);
    t.r.skip();
    expect(t.seen).toEqual([1, 2, 3]);
    expect([t.r.done, t.r.playing, t.pending()]).toEqual([true, false, 0]);
    t.r.play(); // a finished replay does not restart
    t.advance(60_000);
    expect(t.seen).toEqual([1, 2, 3]);
  });
  it("tells its listener after every event and every control", () => {
    const t = setup([0, 1]);
    t.r.play(); // 1
    t.advance(0); // 2
    t.r.pause(); // 3
    t.r.setSpeed(4); // 4
    expect(t.changes()).toBe(4);
  });
});
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd web && npx vitest run test/replay.test.ts`
Expected: FAIL, `Failed to resolve import "../preview/replay"`.

- [ ] **Step 3: Write the implementation** `web/preview/replay.ts`

```ts
// SPDX-License-Identifier: Apache-2.0

// The demo's player: a recorded run's events, applied at the pace they were recorded,
// sped up. A long idle gap is capped, so a demo never stalls on a wait the visitor cannot
// see the reason for. The clock is injected: tests drive it, the page passes the window's.

import type { RoomEvent } from "../src/conn";

export interface Clock {
  setTimer(fn: () => void, ms: number): unknown;
  clearTimer(id: unknown): void;
}

export const windowClock: Clock = {
  setTimer: (fn, ms) => setTimeout(fn, ms),
  clearTimer: (id) => clearTimeout(id as ReturnType<typeof setTimeout>),
};

export interface ReplayOptions {
  speed?: number; // 8 by default: a twenty-minute run plays in under three
  maxGapMs?: number; // the longest recorded gap honoured, before speed: 10 s
  clock?: Clock;
  onChange?: () => void; // after every applied event and every play, pause, speed or skip
}

export class Replay {
  private next = 0; // the index of the next event to apply
  private timer: unknown;
  private speedNow: number;
  private readonly maxGapMs: number;
  private readonly clock: Clock;
  private readonly onChange: () => void;

  constructor(private readonly events: RoomEvent[], private readonly apply: (e: RoomEvent) => void, o: ReplayOptions = {}) {
    this.speedNow = o.speed ?? 8;
    this.maxGapMs = o.maxGapMs ?? 10_000;
    this.clock = o.clock ?? windowClock;
    this.onChange = o.onChange ?? (() => {});
  }

  get playing(): boolean { return this.timer !== undefined; }
  get done(): boolean { return this.next >= this.events.length; }
  get speed(): number { return this.speedNow; }
  get applied(): number { return this.next; }

  // play applies the first event now and schedules the rest; a finished replay stays done.
  play() {
    if (this.playing || this.done) return;
    this.schedule(this.next === 0 ? 0 : this.wait(this.next));
    this.onChange();
  }

  pause() {
    if (!this.playing) return;
    this.clock.clearTimer(this.timer);
    this.timer = undefined;
    this.onChange();
  }

  // setSpeed takes effect from the next wait; a wait under way restarts at the new speed.
  setSpeed(speed: number) {
    if (!(speed > 0)) throw new RangeError(`speed must be positive, got ${speed}`);
    this.speedNow = speed;
    if (this.playing) {
      this.clock.clearTimer(this.timer);
      this.timer = undefined;
      this.schedule(this.wait(this.next));
    }
    this.onChange();
  }

  // skip applies every remaining event now, in order, and stops.
  skip() {
    if (this.playing) this.clock.clearTimer(this.timer);
    this.timer = undefined;
    while (!this.done) this.apply(this.events[this.next++]);
    this.onChange();
  }

  // wait is the pause before event i: its recorded gap, capped, then sped up.
  private wait(i: number): number {
    if (i === 0) return 0;
    const gap = Date.parse(this.events[i].ts) - Date.parse(this.events[i - 1].ts);
    return Math.min(Math.max(gap, 0), this.maxGapMs) / this.speedNow;
  }

  private schedule(ms: number) {
    this.timer = this.clock.setTimer(() => {
      this.timer = undefined;
      this.apply(this.events[this.next++]);
      if (!this.done) this.schedule(this.wait(this.next));
      this.onChange();
    }, ms);
  }
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `cd web && npx vitest run test/replay.test.ts && npm run typecheck`
Expected: PASS (9 tests), no type errors.

- [ ] **Step 5: Commit**

```bash
git add web/preview/replay.ts web/test/replay.test.ts
git commit -m "feat(web): a paced player for replaying a recorded room"
```

---

### Task 2: One page-mounting module for offline pages

`full.ts` builds the room page's regions inline. The demo needs the same regions. This task moves them into `page.ts` without changing what `full.html` shows.

**Files:**
- Create: `web/preview/page.ts`
- Modify: `web/preview/full.ts` (it becomes: fetch the fixtures, `mountPreviewPage`, apply every event, `settle()`). `page.ts` needs no entry in `preview:build`: esbuild bundles it through `full.ts`'s import

**Interfaces:**
- Consumes: the same `web/src` modules `full.ts` imports today.
- Produces:
  - ```ts
    interface PreviewPage {
      apply(e: RoomEvent): void;
      settle(): void; // refresh needs, controls, composer, strip and notes
      say(text: string): void; // the dock's act notice
      setStatus(text: string): void;
      header: HTMLElement;
      footer: HTMLElement;
    }
    ```
  - `mountPreviewPage(app: HTMLElement, o: { snap: Snapshot; summary: Summary; count: number; nothingSent: string; sessionOpen: boolean }): PreviewPage`
  - The inert sender (`send` returns false and says `o.nothingSent`) lives inside `page.ts`.

- [ ] **Step 1: Capture the reference rendering**

```bash
cd web && npm run preview:build && (PORT=4194 node preview/serve.mjs & echo $! > /tmp/serve.pid) && sleep 1
for t in light dark; do chromium --headless=new --hide-scrollbars --virtual-time-budget=4000 --window-size=1280,1600 \
  --screenshot=/tmp/full-before-$t.png "http://localhost:4194/preview/full.html?theme=$t"; done
kill "$(cat /tmp/serve.pid)"
```

- [ ] **Step 2: Move the mounting into `page.ts`**

Move `full.ts`'s DOM building, the inert sender, the mounting of views, needs, controls, composer and fork, and the final refresh-and-`strip.update` block into `mountPreviewPage`. Keep every element, class name and text exactly as in `full.ts`. `apply(e)` calls `views.apply(e)`, `state.apply(e)` and bumps the counters line (`seq N · gaps 0 · dups 0`). `settle()` is the old tail:

- needs: `refresh`, `setSummary`
- controls: `setSummary`, `refresh`
- composer: `refresh`
- `renderNotes`
- `strip.update({ … now: state.latest(), … })`

`full.ts` keeps only the `get` helper, the fetches, the theme handling and:

```ts
const page = mountPreviewPage(app, { snap, summary, count: events.length, nothingSent: "Offline preview: nothing was sent.",
  sessionOpen: new URLSearchParams(location.search).has("session") });
for (const e of events) page.apply(e);
page.setStatus(`live · ${events.length} events (offline fixture)`);
page.settle();
```

- [ ] **Step 3: Verify the output is unchanged, pixel for pixel**

Repeat Step 1's commands with `full-after-$t.png`, then:

```bash
for t in light dark; do compare -metric AE /tmp/full-before-$t.png /tmp/full-after-$t.png /tmp/diff-$t.png; echo " $t"; done
```

Expected: `0 light` and `0 dark`. A non-zero count means the refactor changed the page. Open `/tmp/diff-*.png`, fix, and repeat. Relative times such as "328 h ago" move with the clock: run both captures within the same minute, or accept differences confined to those spans and say so in the report.

- [ ] **Step 4: Gate and commit**

Run: `cd web && npm test && npm run typecheck`. Expected: PASS.

```bash
git add web/preview/page.ts web/preview/full.ts
git commit -m "refactor(web): mount offline preview pages from one module"
```

---

### Task 3: The demo run

A hand-written recording of one plausible task, in the shape of `web/test/fixtures/live-session.jsonl`. Read that fixture, `docs/event-envelope.md` and `docs/room-log.md` first, and copy their payload shapes exactly.

**Files:**
- Create: `web/test/fixtures/demo-session.jsonl`, `demo-snapshot.json`, `demo-summary.json`
- Test: `web/test/demo-fixture.test.ts`

**The story** (one task, issue `#212 "Retry the meter's gateway call on a 429"`, on a fictional repository `acme/billing-api`). There are no real names, repositories or secrets. Humans are `human:` plus 12 digits. The run is `agent:` plus a C2 id.

1. The room opens. The implementer joins, and turn t1 starts.
2. The agent says what it will do. Two or three `terminal` calls follow (`git status`, `rg "429" internal/meter`), ok.
3. The agent explains the fix. Calls follow: it edits a file with a non-terminal tool (`file_editor`), then `go test ./internal/meter/...` **errors**, then the agent fixes it and the test passes.
4. The agent tries `curl https://api.example.com/health`, and policy **denies** it (rejected, `policy: deny`).
5. A human steers: *"Don't call external APIs; the gateway is mocked in tests."* (`delivery: steering`).
6. The agent acknowledges, commits, and requests `git push origin agent/<runId>`. An `approval_requested` follows, and an `approval_decided` (approved) by a second human 20–40 s later.
7. The agent opens the PR (`gh pr create --fill --draft`, ok), posts a progress note, and hands off to a `reviewer` (`handoff` with a summary and the branch).
8. The reviewer joins, runs two calls, and posts a `review_verdict` (`approve`).
9. The run ends.

- **Size:** 80–120 events, gapless from seq 1.
- **Timestamps:** span 12–20 minutes, with realistic gaps of 1–40 s. Include at least one idle gap over 10 s, which the player caps.
- **`demo-snapshot.json`:** the viewer is a collaborator who is an approver, `driver: false`. `runs` are empty after the end.
- **`demo-summary.json`:** the end state, `summary/v1` shaped like `summary.json`: phase, PR link, the reviewer's verdict, notes.

- [ ] **Step 1: Write the failing test** `web/test/demo-fixture.test.ts`

```ts
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { ChatView } from "../src/chat";
import type { RoomEvent } from "../src/conn";

const dir = join(process.cwd(), "test/fixtures");
const events: RoomEvent[] = readFileSync(join(dir, "demo-session.jsonl"), "utf8").trim().split("\n").map((l) => JSON.parse(l));
const types = (t: string) => events.filter((e) => e.type === t);

describe("the demo run", () => {
  it("is a gapless seq order from 1, 80 to 120 events, one room", () => {
    expect(events.length).toBeGreaterThanOrEqual(80);
    expect(events.length).toBeLessThanOrEqual(120);
    events.forEach((e, i) => expect(e.seq).toBe(i + 1));
    expect(new Set(events.map((e) => e.roomId)).size).toBe(1);
  });
  it("carries every envelope field, with timestamps that never go back", () => {
    for (const e of events) {
      expect([e.v, typeof e.id, typeof e.actor?.id, typeof e.type, typeof e.ts, Array.isArray(e.redactions)]).toEqual([1, "string", "string", "string", "string", true]);
      expect(Number.isNaN(Date.parse(e.ts))).toBe(false);
    }
    for (let i = 1; i < events.length; i++) expect(Date.parse(events[i].ts)).toBeGreaterThanOrEqual(Date.parse(events[i - 1].ts));
  });
  it("spans 12 to 20 minutes, with at least one idle gap over 10 s", () => {
    const span = (Date.parse(events.at(-1)!.ts) - Date.parse(events[0].ts)) / 60_000;
    expect(span).toBeGreaterThanOrEqual(12);
    expect(span).toBeLessThanOrEqual(20);
    expect(events.some((e, i) => i > 0 && Date.parse(e.ts) - Date.parse(events[i - 1].ts) > 10_000)).toBe(true);
  });
  it("tells the story: an error, a denial, a steer, an approval, a handoff and a verdict", () => {
    expect(types("tool_result").some((e) => e.payload.status === "error")).toBe(true);
    expect(types("state_changed").some((e) => e.payload.kind === "policy_decision" && e.payload.decision === "deny")).toBe(true);
    expect(types("message").some((e) => e.actor.kind === "human" && e.payload.delivery === "steering")).toBe(true);
    expect(types("approval_requested")).toHaveLength(1);
    expect(types("approval_decided")).toHaveLength(1);
    expect(types("handoff")).toHaveLength(1);
    expect(types("message").some((e) => e.payload.kind === "review_verdict")).toBe(true);
    expect(types("tool_call").some((e) => e.payload.tool !== "terminal")).toBe(true);
  });
  it("names no real person, repository or secret", () => {
    const text = JSON.stringify(events);
    expect(text).not.toMatch(/ogenki|smana|github_pat_|ghp_|ghs_|sk-[A-Za-z0-9]{8}/i);
    for (const e of events) if (e.actor.kind === "human") expect(e.actor.id).toMatch(/^human:\d{12}$/);
  });
  it("renders in the chat view with every call paired and no unknown row", () => {
    const el = document.createElement("div");
    const v = new ChatView(el);
    for (const e of events) v.apply(e);
    expect(el.querySelectorAll("details.tool")).toHaveLength(types("tool_call").length);
    expect(el.querySelectorAll("details.tool.st-running")).toHaveLength(0); // the run ended: every call has its result
    expect(el.querySelectorAll(".step").length).toBeGreaterThanOrEqual(4);
  });
  it("comes with a snapshot and a summary for the same room", () => {
    const snap = JSON.parse(readFileSync(join(dir, "demo-snapshot.json"), "utf8"));
    const summary = JSON.parse(readFileSync(join(dir, "demo-summary.json"), "utf8"));
    expect(snap.roomId).toBe(events[0].roomId);
    expect(summary.room).toBe(events[0].roomId);
    expect(snap.you.approver).toBe(true);
  });
});
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd web && npx vitest run test/demo-fixture.test.ts`
Expected: FAIL, `ENOENT … demo-session.jsonl`.

- [ ] **Step 3: Write the three fixture files**, following the story. Iterate until the test passes. The run first appears on screen in Task 4; until then, the chat-view test is the check.

- [ ] **Step 4: Run it to verify it passes**

Run: `cd web && npx vitest run test/demo-fixture.test.ts && npm test`
Expected: PASS (7 tests), and the suite stays green.

- [ ] **Step 5: Commit**

```bash
git add web/test/fixtures/demo-session.jsonl web/test/fixtures/demo-snapshot.json web/test/fixtures/demo-summary.json web/test/demo-fixture.test.ts
git commit -m "test(web): a curated demo run: an error, a denial, a steer, an approval, a handoff"
```

---

### Task 4: The demo page

**Files:**
- Create: `web/preview/replay-bar.ts`, `web/preview/demo.ts`, `web/preview/demo.html`
- Test: `web/test/replay-bar.test.ts`
- Modify: `web/package.json` (`preview:build` adds `preview/demo.ts`), `.gitignore` (add `web/preview/demo.js`)

**Interfaces:**
- Consumes: `Replay` (Task 1); `mountPreviewPage` and `PreviewPage` (Task 2); the demo fixtures (Task 3).
- Produces: `mountReplayBar(root: HTMLElement, r: Pick<Replay, "play" | "pause" | "setSpeed" | "skip" | "playing" | "done" | "speed" | "applied">, total: number): { refresh(): void }`.

The bar's DOM:

- `div.replay-bar`, `role="group"`, `aria-label="Replay controls"`
- a play/pause `button`, labelled `Pause` while playing and `Play` otherwise; disabled once done
- a speed `select` with options `1×`, `8×`, `32×` (values 1, 8, 32), named `aria-label="Replay speed"`
- a `button` labelled `Skip to the end`, disabled once done
- a progress `span.replay-progress` reading `<applied> / <total> events`, with `role="status"` and `aria-live="off"`, updated by `refresh()`

- [ ] **Step 1: Write the failing test** `web/test/replay-bar.test.ts`

```ts
import { describe, expect, it } from "vitest";
import { mountReplayBar } from "../preview/replay-bar";

// fakeReplay records the bar's calls and exposes the state it reads.
function fakeReplay() {
  const calls: string[] = [];
  const r = {
    playing: false, done: false, speed: 8, applied: 0,
    play() { calls.push("play"); r.playing = true; },
    pause() { calls.push("pause"); r.playing = false; },
    setSpeed(n: number) { calls.push(`speed ${n}`); r.speed = n; },
    skip() { calls.push("skip"); r.applied = 90; r.done = true; r.playing = false; },
  };
  return { r, calls };
}

function setup() {
  const root = document.createElement("div");
  const { r, calls } = fakeReplay();
  const bar = mountReplayBar(root, r, 90);
  const q = <T extends Element>(s: string) => root.querySelector<T>(s)!;
  return { root, r, calls, bar, toggle: () => q<HTMLButtonElement>("button[data-act=toggle]"),
    skip: () => q<HTMLButtonElement>("button[data-act=skip]"), speed: () => q<HTMLSelectElement>("select"), progress: () => q(".replay-progress") };
}

describe("mountReplayBar", () => {
  it("is a named group with play, speed, skip and progress", () => {
    const t = setup();
    const group = t.root.querySelector(".replay-bar")!;
    expect([group.getAttribute("role"), group.getAttribute("aria-label")]).toEqual(["group", "Replay controls"]);
    expect(t.toggle().textContent).toBe("Play");
    expect([...t.speed().options].map((o) => [o.value, o.textContent])).toEqual([["1", "1×"], ["8", "8×"], ["32", "32×"]]);
    expect(t.speed().value).toBe("8");
    expect(t.speed().getAttribute("aria-label")).toBe("Replay speed");
    expect(t.skip().textContent).toBe("Skip to the end");
    expect(t.progress().textContent).toBe("0 / 90 events");
  });
  it("toggles play and pause, and names the button after the next act", () => {
    const t = setup();
    t.toggle().click();
    expect(t.calls).toEqual(["play"]);
    expect(t.toggle().textContent).toBe("Pause");
    t.toggle().click();
    expect(t.calls).toEqual(["play", "pause"]);
    expect(t.toggle().textContent).toBe("Play");
  });
  it("changes the speed from the select", () => {
    const t = setup();
    t.speed().value = "32";
    t.speed().dispatchEvent(new Event("change"));
    expect(t.calls).toEqual(["speed 32"]);
  });
  it("skips to the end, then disables play and skip and shows the full count", () => {
    const t = setup();
    t.skip().click();
    expect(t.calls).toEqual(["skip"]);
    expect([t.toggle().disabled, t.skip().disabled]).toEqual([true, true]);
    expect(t.progress().textContent).toBe("90 / 90 events");
  });
  it("reflects the replay's state on refresh", () => {
    const t = setup();
    t.r.applied = 12;
    t.r.playing = true;
    t.bar.refresh();
    expect([t.progress().textContent, t.toggle().textContent]).toEqual(["12 / 90 events", "Pause"]);
  });
});
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd web && npx vitest run test/replay-bar.test.ts`
Expected: FAIL, `Failed to resolve import "../preview/replay-bar"`.

- [ ] **Step 3: Write `replay-bar.ts`**

Build the DOM above with `document.createElement` and `textContent` only. Each control calls the replay, then `refresh()`. `refresh()` sets:

- the toggle's text from `playing`
- `disabled` on the toggle and skip from `done`
- the select's value from `speed`
- the progress text from `applied`

Add `// SPDX-License-Identifier: Apache-2.0` and a contract comment.

- [ ] **Step 4: Run it to verify it passes**

Run: `cd web && npx vitest run test/replay-bar.test.ts`
Expected: PASS (5 tests).

- [ ] **Step 5: Write `demo.html` and `demo.ts`**

`demo.html`: the `full.html` skeleton with `<title>agent-platform: live demo</title>`, a meta description, `<link rel="stylesheet" href="app.css">` and `<script type="module" src="demo.js"></script>`. All paths are **relative**.

`demo.ts`:
1. Fetch `fixtures/demo-session.jsonl`, `fixtures/demo-snapshot.json` and `fixtures/demo-summary.json` (relative). Apply the theme as `full.ts` does; with no `?theme`, follow the OS through `initTheme()`.
2. Mount the page with `mountPreviewPage(app, { …, nothingSent: "Demo: nothing is sent.", sessionOpen: false })`.
3. Insert a banner `div.banner.demo-banner` right after the header. Its text is `Demo: a recorded run, replayed. Nothing here reaches a cluster. `, followed by a link `What is this?` to `https://github.com/Smana/agent-platform#readme`.
4. Insert the replay bar right before the footer.
5. **Timestamps:** each event's displayed `ts` is rebased to the moment it is applied (`{ ...e, ts: new Date().toISOString() }`), so "5 s ago" reads true. Step durations then show playback time. Say so in a code comment, and keep the original order.
6. Create the `Replay` with `apply: (e) => { page.apply(rebased(e)); page.settle(); }` and `onChange: () => bar.refresh()`. Set the status to `demo · <applied> / <total> events` on each change, then call `play()`.
7. Add `.replay-bar` and `.demo-banner` styles to `web/src/app.css`, using existing tokens only. The bar is a compact row in the dock's style, with 40 px targets, under the D8 contrast rules. If any new colour pair appears, add it to `contrast.test.ts`.

- [ ] **Step 6: Look at it**

Task 5 adds the local site. Until then, run `npm run preview:build` and serve `web/preview/` with any static server from a directory laid out like `_site/` (Task 5's script). Check the following:

- the first step opens while its call runs
- the error, then the fix
- the denial turns amber
- the steer bubble
- the approval card appears in Needs you, then clears
- the handoff and the verdict
- every act says `Demo: nothing is sent.`

- [ ] **Step 7: Gate and commit**

Run: `cd web && npm test && npm run typecheck && npm run build`. Then commit the rebuilt `internal/humanapi/ui/dist` only if `app.css` changed.

```bash
git add web/preview/replay-bar.ts web/preview/demo.ts web/preview/demo.html web/test/replay-bar.test.ts web/package.json web/src/app.css .gitignore internal/humanapi/ui/dist
git commit -m "feat(web): a live demo page replaying a recorded room"
```

---

### Task 5: The site and its publishing workflow

**Files:**
- Create: `web/scripts/demo-site.sh`, `.github/workflows/demo.yaml`
- Modify: `web/preview/serve.mjs` (serve from `SERVE_ROOT` when set, else `web/`; print the demo URL), `web/package.json` (`"demo": "bash scripts/demo-site.sh .demo-site && SERVE_ROOT=.demo-site node preview/serve.mjs"`), `.gitignore` (`web/.demo-site/`, `_site/`)

**Interfaces:** `bash web/scripts/demo-site.sh <out-dir>` builds the demo bundle and writes a flat site: `index.html` (from `demo.html`), `demo.js`, `app.css` and `fixtures/demo-*.{jsonl,json}`. It runs from any cwd and fails on any missing input.

- [ ] **Step 1: Write the script**

```bash
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# demo-site.sh OUT: the live demo as a flat static site, every path relative, so it serves
# from a domain root or from GitHub Pages' /<repo>/ prefix alike.
set -euo pipefail
out=${1:?usage: demo-site.sh OUT}
web=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$out/fixtures"
out=$(cd "$out" && pwd)
cd "$web"
[ -d node_modules ] || npm ci --no-audit --no-fund
npx esbuild preview/demo.ts --bundle --minify --format=esm --target=es2022 --outfile="$out/demo.js"
cp preview/demo.html "$out/index.html"
cp src/app.css "$out/app.css"
cp test/fixtures/demo-session.jsonl test/fixtures/demo-snapshot.json test/fixtures/demo-summary.json "$out/fixtures/"
echo "demo site: $out"
```

- [ ] **Step 2: Check it locally**

Run: `cd web && npm run demo`, then open the printed URL and let the replay finish. Then check the relative paths from a sub-path:

```bash
mkdir -p /tmp/pages/agent-platform && bash web/scripts/demo-site.sh /tmp/pages/agent-platform
(cd /tmp/pages && python3 -m http.server 4195 >/dev/null 2>&1 & echo $! > /tmp/pages.pid); sleep 1
chromium --headless=new --hide-scrollbars --virtual-time-budget=20000 --window-size=1280,1600 \
  --screenshot=/tmp/demo-subpath.png "http://localhost:4195/agent-platform/"
kill "$(cat /tmp/pages.pid)"
```

Expected: `/tmp/demo-subpath.png` shows the room page mid-replay (a strip, steps, the replay bar), not an error.

- [ ] **Step 3: Resolve the action SHAs**

Resolve each action's latest major-version tag to a commit, and record it in the workflow with the version in a comment:

```bash
for a in actions/checkout actions/upload-pages-artifact actions/deploy-pages actions/configure-pages; do
  tag=$(gh api "repos/$a/releases/latest" --jq .tag_name); sha=$(gh api "repos/$a/commits/$tag" --jq .sha); echo "$a@$sha # $tag"; done
```

Reuse the SHA `ci.yaml` already pins for `actions/checkout` and `jdx/mise-action`. Pin a newer one only if the latest major differs, and say so in the report.

- [ ] **Step 4: Write `.github/workflows/demo.yaml`**

```yaml
name: demo
# The live demo (web/preview/demo.ts) on GitHub Pages, rebuilt for each release so it shows the
# released room page. Never on a pull request: a PR's code does not reach the public page.
on:
  push:
    tags: ["v*"]
  workflow_dispatch:
permissions: {}
concurrency:
  group: pages
  cancel-in-progress: false

jobs:
  build:
    runs-on: ubuntu-latest
    timeout-minutes: 10
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@<sha> # <version>
        with:
          persist-credentials: false
      - uses: jdx/mise-action@<sha> # <version>
      - run: bash web/scripts/demo-site.sh _site
      - uses: actions/configure-pages@<sha> # <version>
      - uses: actions/upload-pages-artifact@<sha> # <version>
        with:
          path: _site
  deploy:
    needs: build
    runs-on: ubuntu-latest
    timeout-minutes: 5
    permissions:
      pages: write
      id-token: write
    environment:
      name: github-pages
      url: ${{ steps.deploy.outputs.page_url }}
    steps:
      - id: deploy
        uses: actions/deploy-pages@<sha> # <version>
```

Replace each `<sha> # <version>` with Step 3's output. A committed placeholder is a defect.

- [ ] **Step 5: Lint**

Run: `actionlint .github/workflows/demo.yaml`
Expected: no output, exit 0.

- [ ] **Step 6: Commit**

```bash
git add web/scripts/demo-site.sh web/preview/serve.mjs web/package.json .gitignore .github/workflows/demo.yaml
git commit -m "ci: publish the live demo to GitHub Pages on each release"
```

---

### Task 6: Concepts, architecture and the docs index

**Files:**
- Modify: `docs/architecture.md`, `docs/concepts.md`, `docs/README.md` (if it indexes README sections)

- [ ] **Step 1: Copy the internals into `docs/architecture.md`**

Task 7 removes them from the README. Copying them first keeps every commit's links whole.

Copy into a new section `## Binaries, images and releases` the README's former Architecture mermaid (keep it if `architecture.md` has none like it; otherwise drop the duplicate and say which you kept), Binaries and images, Releases with its cosign block verbatim, and Where it plugs in. Put the SP-codes sentence after it: *The programme's sub-project codes, where you meet them in the specs: SP2 is the rooms, SP3 the factory.* Re-point every link that targets those README anchors to the new section, so Task 7 can remove them:

```bash
grep -rn "README.md#\(architecture\|binaries-and-images\|releases\|where-it-plugs-in\)" --include='*.md' . | grep -v docs/superpowers
```

- [ ] **Step 2: Lead `docs/concepts.md` with the analogy**

Replace the first paragraph with the README's room paragraph, then add one concrete example sentence:

> *e.g. you see the agent's `git push` wait for approval, approve it from the room, and a month later read exactly who approved what.*

After it, before `## Glossary`, add `## In one picture`: a mermaid diagram of one room with its participants and what flows in and out:

- humans (watch, steer, approve)
- a run through its bridge (events in)
- the factory (opens the room, posts the brief)
- out: the PR and the record

- [ ] **Step 3: The glossary pass**

Add two rows to the "Rooms and runs" table:

- **Step**: an agent's message and the tool calls it ran next, shown as one collapsible line on the room page.
- **Call line**: one tool call in a step: its command, its status, its output behind the toggle.

Define on first use, in the row where each first appears, or link to the row that defines it:

- **C2 id**: already a row; make earlier mentions link to it.
- **principal**
- **driver epoch**

- [ ] **Step 4: Gate and commit**

Run: `bash hack/docs.sh && task check`. Expected: both exit 0.

```bash
git add docs/architecture.md docs/concepts.md docs/README.md
git commit -m "docs: rooms by analogy in concepts; the internals move to architecture"
```

---

### Task 7: The README as a landing page

**Files:**
- Modify: `README.md`
- Create: `docs/assets/room-dark.png`, `docs/assets/room-light.png` (1280×800 frames of the demo near its end, from headless chromium on `npm run demo`)

The demo URL is `https://smana.github.io/agent-platform/`.

- [ ] **Step 1: Make the screenshots**

Use `npm run demo`, then headless chromium at 1280×800 with `--virtual-time-budget=60000`, light and dark (`?theme=…`). Each image must show the following:

- steps
- a rejected step
- the composer
- the replay bar

Keep each PNG under 400 KB (`pngquant` or `optipng` if available, else as is).

- [ ] **Step 2: Write the new top of the README**, replacing everything above `## Start here` (the current reader table, re-titled):

```markdown
# agent-platform

**Run coding agents on your Kubernetes cluster without trusting them.**

- **Sandboxed:** every run is an isolated pod holding only short-lived, scoped credentials, inside a budget it cannot raise.
- **Supervised:** approvals, steering and the merge gate sit outside the sandbox, with humans.
- **On the record:** every run streams into a room that nobody can rewrite, the agent included.

<a href="https://smana.github.io/agent-platform/">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/room-dark.png">
    <img alt="A room replaying an agent run: steps of tool calls, a denied command, an approval, the composer" src="docs/assets/room-light.png" width="1280">
  </picture>
</a>

**[Watch a run in the live demo →](https://smana.github.io/agent-platform/)** · [How it works](#how-it-works) · [What's a room?](#whats-a-room)

## What's a room?

A room is a flight recorder and a group chat for one piece of agent work. While the run is live,
you watch every step, steer the agent, and approve what it may not do alone. Afterwards the room
stays: an ordered, redacted record that nobody can edit, which you audit, fork from any message, or
hand to the next run.

Why you can trust that record:

- **Append-only.** The broker's database role can insert and read events, never update or delete
  them ([the room log](docs/room-log.md#the-guarantees)).
- **Redacted first.** Secrets are detected and removed before an event is stored, logged or
  counted ([security](docs/security.md)).
- **Attributed.** Every event carries an identity the broker stamped from a verified credential,
  never one the sender claimed ([principals](docs/security.md)).

## How it works
```

After the heading `## How it works`, add a six-box mermaid `flowchart LR`: *labelled issue → triage → sandboxed run → room → pull request → human merge*. Under it, add one sentence per box, adapted from the factory and room docs:

- the `factory/ready` label
- tier and budget
- the gVisor pod and short-lived credentials
- live and permanent
- the draft PR
- the merge gate, live or in shadow, with a human merge otherwise

Then add these sections, in order:

- `## Who it's for`. Two lines: *For platform teams who want coding agents on their cluster, under their own rules.* and *Not yet for: a laptop tool or a hosted service.*
- `## Status`. One paragraph: the version from `charts/agent-factory/Chart.yaml`'s `appVersion` (or the latest tag), daily use on one cloud for one public repository, and the merge gate in shadow. Say *It runs today on [cloud-native-ref](https://github.com/Smana/cloud-native-ref); a portable install for any Kubernetes cluster is the next milestone.* and link the platform's status page as the current README does.
- `## Start here`. The current reader table, unchanged, followed by the `docs/` index sentence. Drop the SP-codes sentence; Task 7 moves it to the architecture page.
- `## Develop` and `## License`, unchanged.

Remove `## Architecture`, `## Binaries and images`, `## Releases` and `## Where it plugs in` from the README: Task 6 already copied them to `docs/architecture.md` and re-pointed their links.

- [ ] **Step 3: Check the first screen**

Render the README the way GitHub does: `gh markdown-preview` if installed, else push the branch and open it on GitHub. Take a 1280×800 screenshot. Expected: the headline, the three points, the image and the demo link are visible without scrolling. If the image pushes the link below the fold, set the `<img>` `width` to 960.

- [ ] **Step 4: Gate and commit**

Run: `bash hack/docs.sh && task check`. Expected: both exit 0.

```bash
git add README.md docs/assets/room-dark.png docs/assets/room-light.png
git commit -m "docs: the README as a landing page, with a live demo"
```

---

## After the tasks

1. **Visual evidence:** screenshots of the demo at 10 s, 30 s and the end, light and dark, at 390 and 1280 px, plus the README's first screen.
2. **PR:** open the PR from `docs/adoption-story`. Title and description in English, with no "Generated with" line (the owner's rule). List the two owner steps:
   - enable GitHub Pages, with GitHub Actions as the source;
   - after merge, run the `demo` workflow once (`workflow_dispatch`) so the README's link works before the next release.
3. **Out of scope:** the name (part 2) and the portable install (part 3).
