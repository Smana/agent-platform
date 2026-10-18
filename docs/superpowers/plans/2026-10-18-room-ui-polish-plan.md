# Room UI polish Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the room page's composer findable, its tool calls followable, and its dark mode layered, per the 2026-10-18 spec.

**Architecture:** Vanilla TypeScript in `web/`, rendered with DOM nodes only, and embedded into the broker from `internal/humanapi/ui/dist/`.

- **Steps:** a second layout layer over `ChatView`'s seq-ordered rows, built the same way folds are today. Each step keeps a per-step tally, so a live event costs constant time.
- **Composer:** gains a segmented delivery radio group, a labelled Send button and a Ctrl/⌘+Enter shortcut.
- **Session:** Run, Driver, the queue and Room move into a `Session` disclosure in the sticky dock.
- **Colours:** dark surfaces are retuned in `app.css` and checked by `contrast.test.ts`.

**Tech Stack:** TypeScript 5.9, esbuild, vitest 5 + jsdom, plain CSS with tokens on `:root` / `.dark`.

**Spec:** [`docs/superpowers/specs/2026-10-18-room-ui-polish-design.md`](../specs/2026-10-18-room-ui-polish-design.md). Read it before starting.

**Provenance:** every diff below was applied commit by commit in a throwaway worktree from `ddac60a`. Each commit passed `tsc --noEmit` and `vitest run` on its own (371 → 414 tests). The last commit also passed `task ui:test ui:check docs`. The diffs are the code to write, not a sketch.

## Global Constraints

- **Stack (D1):** no framework, no new npm dependency, and the strict CSP stays (`style-src 'self'`: no `style` attribute; CSSOM only).
- **Untrusted text:** DOM nodes and `textContent` only; never `innerHTML` (render.ts's rule, T10).
- **Tests:** stdlib vitest only. Hand-written fakes, no mocking library beyond `vi.spyOn` / `vi.stubGlobal`, as the suite already does.
- **Contrast (D8):** every text pair the page paints is ≥ 4.5:1, a meaningful boundary ≥ 3:1, in both schemes. 12 px is the minimum text size.
- **R09:** a message is never silently re-routed. A lost token or an ended run unchecks Steer, keeps the draft and says why.
- **Copy, verbatim:**
  - placeholder `Message the room…`
  - segments `Room` / `Queue` / `Steer`
  - hints `Posted to the room; no agent is prompted.`, `Queued for the next run's brief.`, `Sent to the running agent now.`
  - `Ctrl+Enter to send`, `Send`
  - session line `Session · driver: <you|short id|nobody> · <a run is on|no run>`
- **Gates:** `task check` exits 0 before the PR. Each task runs `npm test` and `npm run typecheck` in `web/`.
- **Commits:** conventional commits, English, no co-author or "Generated with" trailers (the owner's rule).

## File map

| File | Responsibility | Task |
|---|---|---|
| `web/src/step.ts` (new) | a step's tally, state and summary line; pure | 1 |
| `web/src/icons.ts` (new) | the inline SVG icon set, `createElementNS` | 2 |
| `web/src/chat.ts` | call lines (task 3); the step layer over `ChatView` (task 4) | 3, 4 |
| `web/src/controls.ts` | `mountComposer`: radios, Send, shortcut | 5 |
| `web/src/room.ts` | the Session disclosure in the dock, the skeleton | 6 |
| `web/preview/full.ts` | the offline preview mirrors room.ts's dock | 6 |
| `web/src/app.css` | tokens, dock, composer, steps, call lines, polish | 7 |
| `internal/humanapi/ui/dist/*` | the committed bundle `ui:check` compares | 7 |

## How to run things

```bash
cd web
npm ci                      # once
npx vitest run test/step.test.ts      # one file
npm test && npm run typecheck         # the task's gate
npm run build                         # rebuild internal/humanapi/ui/dist (task 7)
npm run preview                       # http://localhost:4180/preview/full.html[?theme=dark][&session]
```

Run `npm ci` inside the repository's own `web/`. Never run it through a symlinked `node_modules`: it replaces the link and empties the target.

---

### Task 1: The step tally

**Files:**
- Create: `web/src/step.ts`
- Test: `web/test/step.test.ts`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `interface StepCall { tool: string; status: string; start: string; end?: string }`
  - `interface StepTally { calls; running; rejected; failed; tools; quiet; start; end }` (numbers; start/end in ms)
  - `emptyTally(): StepTally`
  - `count(t: StepTally, c: StepCall | undefined, sign: 1 | -1): void` (undefined counts a plumbing row)
  - `type StepState = "running" | "failed" | "done"`
  - `stepState(t): StepState`
  - `interface StepLine { head: string; exceptions: string; duration: string }`
  - `stepLine(t): StepLine`

Why a tally rather than a list: a rescan per event made a 5,000-event replay of one step take 63 s in jsdom. The tally takes 0.9 s.

- [ ] **Step 1: Write the failing test**

```ts
import { describe, expect, it } from "vitest";
import { count, emptyTally, stepLine, stepState, type StepCall, type StepTally } from "../src/step";

const c = (status: string, start: string, end?: string, tool = "terminal"): StepCall => ({ tool, status, start, end });
const t = (s: number) => `2026-10-18T07:54:${String(s).padStart(2, "0")}Z`;
const tally = (...calls: (StepCall | undefined)[]): StepTally => {
  const x = emptyTally();
  for (const k of calls) count(x, k, 1);
  return x;
};

describe("stepState", () => {
  it("is running while any call has no result, whatever failed before", () => {
    expect(stepState(tally(c("error", t(1), t(2)), c("running", t(3))))).toBe("running");
  });
  it("is failed once every call ended and one errored or was rejected", () => {
    expect(stepState(tally(c("ok", t(1), t(2)), c("rejected", t(3), t(4))))).toBe("failed");
    expect(stepState(tally(c("error", t(1), t(2))))).toBe("failed");
  });
  it("is done when every call is ok, or the step holds plumbing alone", () => {
    expect(stepState(tally(c("ok", t(1), t(2)), c("ok", t(3), t(4))))).toBe("done");
    expect(stepState(tally(undefined))).toBe("done");
  });
});

describe("stepLine", () => {
  it("counts commands while running, with no duration until one ends", () => {
    expect(stepLine(tally(c("running", t(1)), c("running", t(2))))).toEqual({ head: "Running 2 commands", exceptions: "", duration: "" });
  });
  it("names a single call in the singular and gives it no duration", () => {
    expect(stepLine(tally(c("ok", t(1), t(9))))).toEqual({ head: "Ran 1 command", exceptions: "", duration: "" });
  });
  it("says tool calls once any call is not the terminal", () => {
    expect(stepLine(tally(c("ok", t(1), t(2)), c("ok", t(3), t(4), "room_read"))).head).toBe("Ran 2 tool calls");
  });
  it("names rejected then failed calls, and spans first start to last end as m:ss", () => {
    const line = stepLine(tally(c("ok", t(1), t(2)), c("error", t(3), t(4)), c("rejected", t(5), t(16))));
    expect(line).toEqual({ head: "Ran 3 commands", exceptions: "1 rejected · 1 failed", duration: "0:15" });
  });
  it("rolls seconds over into minutes", () => {
    expect(stepLine(tally(c("ok", "2026-10-18T07:54:00Z", t(1)), c("ok", t(2), "2026-10-18T07:56:05Z"))).duration).toBe("2:05");
  });
  it("counts plumbing alone as changes", () => {
    expect(stepLine(tally(undefined, undefined)).head).toBe("2 changes");
    expect(stepLine(tally(undefined)).head).toBe("1 change");
  });
});

describe("count", () => {
  it("takes a replaced call out and its result in, the way a landing result does", () => {
    const x = tally(c("running", t(1)));
    count(x, c("running", t(1)), -1);
    count(x, c("rejected", t(1), t(4)), 1);
    expect(x).toMatchObject({ calls: 1, running: 0, rejected: 1 });
    expect(stepLine(x).head).toBe("Ran 1 command");
  });
});
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd web && npx vitest run test/step.test.ts`
Expected: FAIL, `Failed to resolve import "../src/step"`.

- [ ] **Step 3: Write the implementation**

```ts
// SPDX-License-Identifier: Apache-2.0

// A step's summary: what the tool calls an agent ran after one of its messages add up
// to, as the one line a reader scans (`Ran 3 commands · 1 rejected · 0:15`), and the
// state that decides whether the step opens itself. A tally, not a list: chat.ts counts
// each row in as it lands and out as it leaves, so a step of thousands of calls costs
// constant time per event (a rescan per event made a 5,000-event replay take a minute).

export interface StepCall {
  tool: string;
  status: string; // "running" until the result lands, else the result's status
  start: string; // the call's ts
  end?: string; // the result's ts
}

export interface StepTally {
  calls: number;
  running: number;
  rejected: number;
  failed: number; // error, or any status the page does not know
  tools: number; // calls to a tool other than the terminal
  quiet: number; // plumbing rows: they count toward no call
  start: number; // ms; the earliest call's start
  end: number; // ms; the latest result's end
}

export const emptyTally = (): StepTally =>
  ({ calls: 0, running: 0, rejected: 0, failed: 0, tools: 0, quiet: 0, start: Infinity, end: -Infinity });

// count adds a call (sign 1) or takes one away (sign -1); undefined is a plumbing row.
// Taking a call away leaves start and end: a bound cannot be undone without a rescan,
// and the one removal on the hot path, a call's row replaced when its result lands,
// keeps its start and only moves its end later.
export function count(t: StepTally, c: StepCall | undefined, sign: 1 | -1) {
  if (!c) {
    t.quiet += sign;
    return;
  }
  t.calls += sign;
  if (c.status === "running") t.running += sign;
  else if (c.status === "rejected") t.rejected += sign;
  else if (c.status !== "ok") t.failed += sign;
  if (c.tool !== "terminal") t.tools += sign;
  if (sign === 1) {
    t.start = Math.min(t.start, Date.parse(c.start));
    if (c.end) t.end = Math.max(t.end, Date.parse(c.end));
  }
}

// running: a call has no result yet; failed: one errored or was rejected; done: all ok.
export type StepState = "running" | "failed" | "done";

export function stepState(t: StepTally): StepState {
  if (t.running > 0) return "running";
  if (t.rejected + t.failed > 0) return "failed";
  return "done";
}

export interface StepLine {
  head: string; // "Running 2 commands", "Ran 1 tool call", "3 changes" for plumbing alone
  exceptions: string; // "1 rejected · 2 failed", or ""
  duration: string; // "0:15", or "" for one call or none finished
}

export function stepLine(t: StepTally): StepLine {
  if (t.calls === 0) return { head: `${t.quiet} ${t.quiet === 1 ? "change" : "changes"}`, exceptions: "", duration: "" };
  const one = t.calls === 1;
  const noun = t.tools === 0 ? (one ? "command" : "commands") : (one ? "tool call" : "tool calls");
  const exceptions = [[t.rejected, "rejected"], [t.failed, "failed"]].filter(([k]) => (k as number) > 0)
    .map(([k, word]) => `${k} ${word}`).join(" · ");
  return {
    head: `${t.running > 0 ? "Running" : "Ran"} ${t.calls} ${noun}`,
    exceptions,
    duration: one || t.end < t.start ? "" : span(t.end - t.start),
  };
}

// span is a duration in ms as m:ss.
function span(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `cd web && npx vitest run test/step.test.ts && npm run typecheck`
Expected: PASS (10 tests), no type errors.

- [ ] **Step 5: Commit**

```bash
git add web/src/step.ts web/test/step.test.ts
git commit -m "feat(web): tally a step's tool calls into one summary line"
```

---

### Task 2: The icon set

**Files:**
- Create: `web/src/icons.ts`
- Test: `web/test/icons.test.ts`

**Interfaces:**
- Consumes: nothing.
- Produces: `type IconName = "check" | "x" | "ban" | "spinner" | "chevron" | "send"`, and `icon(name: IconName, label?: string): SVGSVGElement`. Decorative (`aria-hidden="true"`) without a label; `role="img"` + `aria-label` with one.

- [ ] **Step 1: Write the failing test**

```ts
import { describe, expect, it } from "vitest";
import { icon } from "../src/icons";

describe("icon", () => {
  it("draws an outline glyph in currentColor on a 16 px grid", () => {
    const svg = icon("check");
    expect(svg.namespaceURI).toBe("http://www.w3.org/2000/svg");
    expect(svg.getAttribute("viewBox")).toBe("0 0 16 16");
    expect([svg.getAttribute("fill"), svg.getAttribute("stroke"), svg.getAttribute("stroke-width")]).toEqual(["none", "currentColor", "1.5"]);
    expect(svg.getAttribute("class")).toBe("icon icon-check");
    expect(svg.querySelectorAll("path").length).toBeGreaterThan(0);
  });
  it("hides a decorative glyph from assistive tech", () => {
    expect(icon("send").getAttribute("aria-hidden")).toBe("true");
    expect(icon("send").getAttribute("role")).toBeNull();
  });
  it("names a glyph that is the only sign of a state", () => {
    const svg = icon("spinner", "running");
    expect([svg.getAttribute("role"), svg.getAttribute("aria-label"), svg.getAttribute("aria-hidden")]).toEqual(["img", "running", null]);
  });
  it("carries no attribute the CSP or the inert check refuses", () => {
    for (const n of [icon("ban"), ...icon("ban").querySelectorAll("*")]) {
      for (const a of n.attributes) expect(a.name === "style" || a.name.startsWith("on"), a.name).toBe(false);
    }
  });
});
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd web && npx vitest run test/icons.test.ts`
Expected: FAIL, `Failed to resolve import "../src/icons"`.

- [ ] **Step 3: Write the implementation**

```ts
// SPDX-License-Identifier: Apache-2.0

// The page's icons: one outline set, a 1.5 px stroke on a 16 px grid, built with
// createElementNS so the strict CSP needs no markup. Each draws in currentColor: its
// state (hover, disabled, a status) comes from CSS, never from a second asset.

const NS = "http://www.w3.org/2000/svg";

const paths = {
  check: ["M3.5 8.5l3 3 6-7"],
  x: ["M4.5 4.5l7 7M11.5 4.5l-7 7"],
  ban: ["M8 1.75a6.25 6.25 0 1 0 0 12.5a6.25 6.25 0 1 0 0-12.5z", "M3.6 3.6l8.8 8.8"],
  spinner: ["M8 1.75a6.25 6.25 0 1 0 6.25 6.25"],
  chevron: ["M6 3.5l4.5 4.5-4.5 4.5"],
  send: ["M2.5 8h10", "M8.5 4l4 4-4 4"],
} as const;

export type IconName = keyof typeof paths;

// icon is one glyph: decorative (hidden from assistive tech) unless it carries a label,
// for an icon that is the only sign of a state.
export function icon(name: IconName, label?: string): SVGSVGElement {
  const svg = document.createElementNS(NS, "svg");
  for (const [k, v] of Object.entries({
    viewBox: "0 0 16 16", width: "16", height: "16", fill: "none", stroke: "currentColor",
    "stroke-width": "1.5", "stroke-linecap": "round", "stroke-linejoin": "round", class: `icon icon-${name}`,
  })) svg.setAttribute(k, v);
  if (label) {
    svg.setAttribute("role", "img");
    svg.setAttribute("aria-label", label);
  } else {
    svg.setAttribute("aria-hidden", "true");
  }
  for (const d of paths[name]) {
    const p = document.createElementNS(NS, "path");
    p.setAttribute("d", d);
    svg.append(p);
  }
  return svg;
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `cd web && npx vitest run test/icons.test.ts && npm run typecheck`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add web/src/icons.ts web/test/icons.test.ts
git commit -m "feat(web): an inline SVG icon set the strict CSP allows"
```

---

### Task 3: Call lines (P3)

A tool row's summary becomes a call line, with these changes:

- **Prompt:** `$` for a terminal command, the tool name otherwise.
- **Status:** an icon. `ok` and `running` are unworded; `rejected` and `error` are named in words.
- **Body:** the full command once; `ARGUMENTS` only for the fields beyond `command`; a `show all` toggle over outputs longer than 20 lines.
- **Data attributes:** the row carries `data-tool`, `data-status`, `data-start` and `data-end`, which task 4's tally reads.

**Files:**
- Modify: `web/src/chat.ts` (imports; `chip()` removed; `toolRow()` rewritten; new `statuses`, `statusMark`, `capLines`)
- Test: `web/test/chat.test.ts`

**Interfaces:**
- Consumes: `icon`, `IconName` from task 2.
- Produces: on every `details.tool` the attributes `data-tool`, `data-status` (`running` until a result, else the result's status), `data-start` (ts) and `data-end` (the result's ts, when one landed). Inside it: `.t-prompt` or `.t-name`, `.t-cmd`, and `.t-status.st-<status>` holding the icon. In the body: `pre.t-command`, `pre.t-args` (only when args carry more than `command`), `pre.t-out` (with `.capped`) and `button.t-more`.

- [ ] **Step 1: Write the failing tests**

The existing tool-pairing assertions move off the removed `ok`/`running` chip, the inert-tag list gains `BUTTON`, `svg` and `path`, the fixture counts select `details.st-*` (the status span now shares that class), and a new `ChatView call lines (P3)` block is appended:

```diff
diff --git a/web/test/chat.test.ts b/web/test/chat.test.ts
index 4ec842f..f72a9e7 100644
--- a/web/test/chat.test.ts
+++ b/web/test/chat.test.ts
@@ -12,7 +12,7 @@ const ev = (seq: number, type: string, payload: unknown, actor: { kind: string;
 // Every element the chat view may build; anything else in the output is a hole (T10).
 const allowed = new Set(["ARTICLE", "HEADER", "DIV", "P", "H1", "H2", "H3", "H4", "H5", "H6", "UL", "OL", "LI",
   "BLOCKQUOTE", "STRONG", "EM", "S", "A", "SPAN", "CODE", "PRE", "HR", "BR", "TABLE", "THEAD", "TBODY", "TR",
-  "TH", "TD", "DETAILS", "SUMMARY", "TIME"]);
+  "TH", "TD", "DETAILS", "SUMMARY", "TIME", "BUTTON", "svg", "path"]);
 
 function inert(root: HTMLElement) {
   for (const n of [root, ...root.querySelectorAll<HTMLElement>("*")]) {
@@ -80,7 +80,7 @@ describe("ChatView tool pairing", () => {
     const { el } = view([call(1, "call_1", "git push origin agent/ibbay5ud")]);
     const tool = el.querySelector("details.tool")!;
     expect(tool.className).toContain("st-running");
-    expect(tool.querySelector(".chip")?.textContent).toBe("running");
+    expect(tool.querySelector(".t-status svg")?.getAttribute("aria-label")).toBe("running");
     expect(tool.querySelector(".t-cmd")?.textContent).toBe("git push origin agent/ibbay5ud");
     expect(el.children).toHaveLength(1);
   });
@@ -89,9 +89,11 @@ describe("ChatView tool pairing", () => {
     expect(el.children).toHaveLength(1);
     const tool = el.querySelector("details.tool")!;
     expect(tool.className).toContain("st-ok");
-    expect(tool.querySelector(".chip")?.textContent).toBe("ok");
+    expect(tool.querySelector(".t-status svg")?.getAttribute("aria-label")).toBe("ok");
+    expect(tool.querySelector(".chip")).toBeNull(); // ok is the norm: no chip (P3)
     expect(tool.querySelector(".t-out")?.textContent).toBe(" M web/src/render.ts");
-    expect(tool.querySelector(".t-args")?.textContent).toContain("git status");
+    expect(tool.querySelector(".t-command")?.textContent).toBe("git status");
+    expect(tool.querySelector(".t-args")).toBeNull(); // the command was all there was
   });
   it("renders a rejected result in its own state", () => {
     const { el } = view([call(1, "c", "git push"), result(2, "c", "rejected", "refused")]);
@@ -209,9 +211,9 @@ describe("the recorded session fixture", () => {
     const { el } = view(events);
     expect(el.querySelectorAll(".msg")).toHaveLength(5); // four agent, one human
     expect(el.querySelectorAll("details.tool")).toHaveLength(5); // results paired, none doubled
-    expect(el.querySelectorAll(".st-rejected")).toHaveLength(1); // the refused push
-    expect(el.querySelectorAll(".st-ok")).toHaveLength(3);
-    expect(el.querySelectorAll(".st-running")).toHaveLength(1); // git log, still running
+    expect(el.querySelectorAll("details.st-rejected")).toHaveLength(1); // the refused push
+    expect(el.querySelectorAll("details.st-ok")).toHaveLength(3);
+    expect(el.querySelectorAll("details.st-running")).toHaveLength(1); // git log, still running
     expect(el.querySelectorAll(".pol-allow")).toHaveLength(0); // an allowed call is the norm: no badge (U7)
     expect(el.querySelectorAll(".sys")).toHaveLength(2); // owner and agent joined
     expect(el.querySelector(".redacted")?.textContent).toContain("github-app-token");
@@ -372,3 +374,38 @@ describe("ChatView folds", () => {
     expect(fold.open).toBe(true);
   });
 });
+
+describe("ChatView call lines (P3)", () => {
+  const call = (args: Record<string, unknown>, tool = "terminal") => ev(1, "tool_call", { callId: "c", tool, args });
+  const done = (status: string, output = "") => ev(2, "tool_result", { callId: "c", status, output, truncated: false, bytes: output.length });
+
+  it("prompts a shell command with $ and names any other tool", () => {
+    expect(view([call({ command: "ls" })]).el.querySelector("summary .t-prompt")?.textContent).toBe("$");
+    expect(view([call({ limit: 50 }, "room_read")]).el.querySelector("summary .t-name")?.textContent).toBe("room_read");
+  });
+  it("names a rejected or errored call in words beside its icon; ok and running stay unworded", () => {
+    const words = (status: string) => view([call({ command: "x" }), done(status)]).el.querySelector(".t-status")!.textContent;
+    expect(words("rejected")).toBe("rejected");
+    expect(words("error")).toBe("error");
+    expect(words("ok")).toBe("");
+    expect(view([call({ command: "x" })]).el.querySelector(".t-status")!.textContent).toBe("");
+  });
+  it("shows only the arguments the command does not already say", () => {
+    const { el } = view([call({ command: "ls", timeout: 30 })]);
+    expect(el.querySelector(".t-command")?.textContent).toBe("ls");
+    expect(JSON.parse(el.querySelector(".t-args")!.textContent!)).toEqual({ timeout: 30 });
+  });
+  it("caps a long output behind show all, and lifts the cap on click", () => {
+    const { el } = view([call({ command: "seq 40" }), done("ok", Array.from({ length: 40 }, (_, i) => i).join("\n"))]);
+    const out = el.querySelector(".t-out")!;
+    const more = el.querySelector<HTMLButtonElement>(".t-more")!;
+    expect(out.classList.contains("capped")).toBe(true);
+    more.click();
+    expect(out.classList.contains("capped")).toBe(false);
+    expect(more.textContent).toBe("show less");
+  });
+  it("leaves a short output uncapped, with no show all", () => {
+    const { el } = view([call({ command: "ls" }), done("ok", "a\nb")]);
+    expect(el.querySelector(".t-more")).toBeNull();
+  });
+});
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd web && npx vitest run test/chat.test.ts`
Expected: FAIL, 6 of 44. The changed pairing tests and the call-line tests find no `.t-status`, `.t-prompt`, `.t-command` or `.t-more`.

- [ ] **Step 3: Write the implementation**

```diff
diff --git a/web/src/chat.ts b/web/src/chat.ts
index 36ee828..50991f2 100644
--- a/web/src/chat.ts
+++ b/web/src/chat.ts
@@ -11,6 +11,7 @@
 
 import type { RoomEvent } from "./conn";
 import { deliveryChip, handoffHead, markdown, stateHead } from "./render";
+import { icon, type IconName } from "./icons";
 import { maxRows } from "./limits";
 
 function el(tag: string, cls: string, text?: string): HTMLElement {
@@ -102,10 +103,6 @@ function preview(call: RoomEvent | undefined, result: RoomEvent | undefined): st
   return out.split("\n", 1)[0];
 }
 
-function chip(status: string): HTMLElement {
-  return el("span", `chip chip-${cls(status)}`, status);
-}
-
 function polBadge(d: RoomEvent): HTMLElement {
   const p = d.payload ?? {};
   // policy_decision carries `decision`; decision_applied may carry only a ref.
@@ -113,16 +110,42 @@ function polBadge(d: RoomEvent): HTMLElement {
   return el("span", `pol pol-${cls(label)}`, `policy: ${label}`);
 }
 
+// The status a call line shows. ok is the norm: a muted check, no word. The rest are
+// named beside their icon, so a colour is never the only sign.
+const statuses: Record<string, [IconName, boolean]> = {
+  ok: ["check", false], running: ["spinner", false], rejected: ["ban", true], error: ["x", true],
+};
+
+function statusMark(status: string): HTMLElement {
+  const [glyph, worded] = statuses[status] ?? ["x", true];
+  const s = el("span", `t-status st-${cls(status)}`);
+  s.append(icon(glyph, status));
+  if (worded) s.append(el("span", "", status));
+  return s;
+}
+
+// An output longer than this many lines opens capped, with a "show all".
+const capLines = 20;
+
 function toolRow(c: Call): HTMLElement {
   const status = c.result ? String(c.result.payload?.status ?? "ok") : "running";
   const d = el("details", `tool st-${cls(status)}`) as HTMLDetailsElement;
   d.dataset.seq = String(c.seq);
   d.dataset.callId = c.callId;
   d.dataset.lastSeq = String(Math.max(...[c.call, c.result, ...c.decisions].map((e) => e?.seq ?? 0)));
+  const name = c.call ? String(c.call.payload?.tool ?? "tool") : "tool";
+  const args = c.call?.payload?.args;
+  const command = args && typeof args.command === "string" ? args.command : undefined;
+  // What the step's summary line reads off the row (step.ts).
+  d.dataset.tool = name;
+  d.dataset.status = status;
+  d.dataset.start = (c.call ?? c.result ?? c.decisions[0])!.ts;
+  if (c.result) d.dataset.end = c.result.ts;
 
   const sum = el("summary", "");
-  const name = c.call ? String(c.call.payload?.tool ?? "tool") : "tool";
-  sum.append(el("span", "t-name", name), el("code", "t-cmd", preview(c.call, c.result)), chip(status));
+  // A shell command reads as one: a prompt, then the command. Any other tool is named.
+  sum.append(name === "terminal" && command !== undefined ? el("span", "t-prompt", "$") : el("span", "t-name", name));
+  sum.append(el("code", "t-cmd", preview(c.call, c.result)), statusMark(status));
   const by = c.call?.payload?.decidedBy;
   // An allowed call is the norm: only a decision that is not a plain allow, or one a
   // person made, earns a badge.
@@ -131,14 +154,30 @@ function toolRow(c: Call): HTMLElement {
   sum.append(time((c.result ?? c.call ?? c.decisions[0])!.ts));
 
   const body = el("div", "t-body");
-  if (c.call) {
+  if (command !== undefined) body.append(el("pre", "t-command", command));
+  // The arguments the summary does not already show: all of them for a tool with no
+  // command, the rest beside one, nothing when the command was all there was.
+  const rest = args && command !== undefined ? Object.fromEntries(Object.entries(args).filter(([k]) => k !== "command")) : args;
+  if (c.call && (command === undefined || Object.keys(rest ?? {}).length > 0)) {
     body.append(el("div", "t-sec", "arguments"));
-    body.append(el("pre", "t-args", JSON.stringify(c.call.payload?.args ?? null, null, 2)));
+    body.append(el("pre", "t-args", JSON.stringify(rest ?? null, null, 2)));
   }
   if (c.result) {
     const p = c.result.payload ?? {};
+    const output = String(p.output ?? "");
     body.append(el("div", "t-sec", `output · ${p.bytes ?? "?"} B${p.truncated ? " · truncated" : ""}`));
-    body.append(el("pre", "t-out", String(p.output ?? "")));
+    const out = el("pre", "t-out", output);
+    body.append(out);
+    if (output.split("\n").length > capLines) {
+      out.classList.add("capped");
+      const more = el("button", "t-more", "show all") as HTMLButtonElement;
+      more.type = "button";
+      more.onclick = () => {
+        const capped = out.classList.toggle("capped");
+        more.textContent = capped ? "show all" : "show less";
+      };
+      body.append(more);
+    }
   } else {
     body.append(el("div", "t-sec", "waiting for the result…"));
   }
```

- [ ] **Step 4: Run them to verify they pass**

Run: `cd web && npm test && npm run typecheck`
Expected: PASS (380 tests).

- [ ] **Step 5: Commit**

```bash
git add web/src/chat.ts web/test/chat.test.ts
git commit -m "feat(web): read a tool call as a call line, ok left unworded"
```

---

### Task 4: Step blocks (P1, P2)

`ChatView` gains the step layer. The flat seq-ordered model, the O(1) live path and the cap are unchanged.

**Classes** (`put(seq, row, flow)` records each row's as `data-flow`):

| Flow | Rows |
|---|---|
| `owner` | an agent's message |
| `member` | a tool row, or a plumbing row |
| `break` | everything else: a human's or the system's message, a handoff, a harness error, a seal, a driver change naming you |

**Layout:**

- **A step:** `section.step` holds the owner, then `details.step-calls`, made with the first member, with `summary` and `div.step-body`.
- **Folds:** form inside whatever holds their run: a step body, or the stream.
- **`box(i)`:** finds where member `i` belongs from the row before it, in constant time.
- **Slow paths:** an owner or a break landing mid-run moves the following members (`rehome`), and an owner leaving at the cap hands its members to the stream. Both recount the steps they touch.

**Line and open state:**

- `refreshStep` renders the line from the step's tally.
- It opens or closes `step-calls` only when the state (`running` / `failed` / `done`) changes, so a reader's own toggle holds otherwise.

**Files:**
- Modify: `web/src/chat.ts` (imports; new `callsOf`, `stepBodyOf`, `tallies`, `tallyOf`, `callOf`, `recount`, `refreshStep`; the `ChatView` class's layout methods)
- Test: `web/test/chat.test.ts`

**Interfaces:**
- Consumes:
  - from task 1: `count`, `emptyTally`, `stepLine`, `stepState`, `StepCall`, `StepTally`
  - from task 3: the row's `data-*` attributes
- Produces:
  - the DOM above
  - `section.step[data-state]`
  - the line's spans: `.step-head`, `.step-exc`, `.step-dur`
  - `ChatView`'s public surface (`apply`, `rowFor`) and the exported `reveal`, `focusRow`, `plumbing`, `swatchClass`: unchanged

- [ ] **Step 1: Write the failing tests**

The tests change in three ways:

- The fold tests' and the noise-rule test's messages become a **human's**, a break, so those folds still sit in the stream and keep proving fold behaviour.
- The inert list gains `SECTION`.
- New blocks: `ChatView steps (P1, P2)` and `ChatView step cost`. The cost test fails if the line is ever rebuilt by rescanning the step's rows.

```diff
diff --git a/web/test/chat.test.ts b/web/test/chat.test.ts
index f72a9e7..784f835 100644
--- a/web/test/chat.test.ts
+++ b/web/test/chat.test.ts
@@ -12,7 +12,7 @@ const ev = (seq: number, type: string, payload: unknown, actor: { kind: string;
 // Every element the chat view may build; anything else in the output is a hole (T10).
 const allowed = new Set(["ARTICLE", "HEADER", "DIV", "P", "H1", "H2", "H3", "H4", "H5", "H6", "UL", "OL", "LI",
   "BLOCKQUOTE", "STRONG", "EM", "S", "A", "SPAN", "CODE", "PRE", "HR", "BR", "TABLE", "THEAD", "TBODY", "TR",
-  "TH", "TD", "DETAILS", "SUMMARY", "TIME", "BUTTON", "svg", "path"]);
+  "TH", "TD", "DETAILS", "SUMMARY", "TIME", "SECTION", "BUTTON", "svg", "path"]);
 
 function inert(root: HTMLElement) {
   for (const n of [root, ...root.querySelectorAll<HTMLElement>("*")]) {
@@ -227,7 +227,7 @@ describe("ChatView noise rules (U7)", () => {
     view.apply(ev(1, "state_changed", { kind: "room_phase", phase: "Open" }));
     view.apply(ev(2, "participant", { principal: "agent:ikely2yk", change: "joined", role: "implementer" }));
     view.apply(ev(3, "turn", { turnId: "t1", phase: "started" }));
-    view.apply(ev(4, "message", { kind: "chat", text: "hi", delivery: "none" }));
+    view.apply(ev(4, "message", { kind: "chat", text: "hi", delivery: "none" }, { kind: "human", id: "human:me" }));
     view.apply(ev(5, "state_changed", { kind: "harness_status", status: "running", previous: "idle" }));
     const rows = [...el.children];
     expect(rows.map((r) => r.className.split(" ")[0])).toEqual(["fold", "msg", "mark"]);
@@ -283,7 +283,8 @@ describe("ChatView noise rules (U7)", () => {
 
 describe("ChatView folds", () => {
   const turn = (seq: number) => ev(seq, "turn", { turnId: "t1", phase: "started" });
-  const msg = (seq: number) => ev(seq, "message", { kind: "chat", text: `m${seq}`, delivery: "none" });
+  // A human's message: a break, so these folds sit in the stream, outside any step.
+  const msg = (seq: number) => ev(seq, "message", { kind: "chat", text: `m${seq}`, delivery: "none" }, { kind: "human", id: "human:me" });
   const shape = (el: HTMLElement) => [...el.children].map((r) =>
     r.classList.contains("fold") ? `fold(${[...r.querySelectorAll("[data-seq]")].map((x) => (x as HTMLElement).dataset.seq).join(",")})` : (r as HTMLElement).dataset.seq ?? r.querySelector<HTMLElement>("[data-seq]")?.dataset.seq);
 
@@ -375,6 +376,105 @@ describe("ChatView folds", () => {
   });
 });
 
+describe("ChatView steps (P1, P2)", () => {
+  const human = { kind: "human", id: "human:me" };
+  const said = (seq: number, actor?: { kind: string; id: string; role?: string }) =>
+    ev(seq, "message", { kind: "chat", text: `m${seq}`, delivery: "none" }, actor);
+  const call = (seq: number, id: string, command = `cmd${seq}`) =>
+    ev(seq, "tool_call", { callId: id, tool: "terminal", args: { command } });
+  const done = (seq: number, id: string, status = "ok") =>
+    ev(seq, "tool_result", { callId: id, status, output: "", truncated: false, bytes: 0 });
+  const turn = (seq: number) => ev(seq, "turn", { turnId: "t1", phase: "started" });
+  // shape names each child of the stream: a step as step(owner: members), a fold by its seqs.
+  const seqsIn = (n: Element) => [...n.querySelectorAll<HTMLElement>("[data-flow]")].map((x) => x.dataset.seq).join(",");
+  const shape = (el: HTMLElement) => [...el.children].map((r) => {
+    if (r.classList.contains("step")) {
+      const owner = r.querySelector<HTMLElement>(":scope > [data-flow]")?.dataset.seq;
+      const inner = r.querySelector(":scope > .step-calls > .step-body");
+      return `step(${owner}${inner ? ": " + [...inner.children].map((m) => m.classList.contains("fold") ? `fold(${seqsIn(m)})` : (m as HTMLElement).dataset.seq).join(" ") : ""})`;
+    }
+    return r.classList.contains("fold") ? `fold(${seqsIn(r)})` : (r as HTMLElement).dataset.seq;
+  });
+  const line = (el: HTMLElement, owner: number) =>
+    el.querySelector(`[data-seq="${owner}"]`)!.closest(".step")!.querySelector(".step-calls > summary")!.textContent;
+
+  it("gives an agent's message the calls and plumbing after it, up to a human's message", () => {
+    const { el } = view([said(1), call(2, "a"), turn(3), turn(4), call(5, "b"), said(6, human), call(7, "c")]);
+    expect(shape(el)).toEqual(["step(1: 2 fold(3,4) 5)", "6", "7"]);
+  });
+  it("leaves the rows before the first agent message in the stream", () => {
+    const { el } = view([turn(1), call(2, "a"), said(3), call(4, "b")]);
+    expect(shape(el)).toEqual(["1", "2", "step(3: 4)"]);
+  });
+  it("closes a step at a handoff, a harness error and the next agent message", () => {
+    const { el } = view([
+      said(1), call(2, "a"),
+      ev(3, "state_changed", { kind: "harness_error", code: "E1", detail: "boom" }), call(4, "b"),
+      said(5), call(6, "c"),
+      ev(7, "handoff", { from: "implementer", to: "reviewer", summary: "done" }), call(8, "d"),
+    ]);
+    expect(shape(el)).toEqual(["step(1: 2)", "3", "4", "step(5: 6)", "7", "8"]);
+  });
+  it("renders an agent message with nothing after it as a step with no toggle", () => {
+    const { el } = view([said(1)]);
+    expect(shape(el)).toEqual(["step(1)"]);
+    expect(el.querySelector(".step-calls")).toBeNull();
+  });
+  it("sums the step's calls on its line and rewrites it as results land", () => {
+    const { el, v } = view([said(1), call(2, "a"), call(3, "b")]);
+    expect(line(el, 1)).toBe("Running 2 commands");
+    v.apply(done(4, "a"));
+    v.apply(done(5, "b", "rejected"));
+    expect(line(el, 1)).toBe("Ran 2 commands1 rejected0:03");
+    expect(el.querySelector(".step-exc")?.textContent).toBe("1 rejected");
+  });
+  it("opens a running step, keeps a failed one open, and closes a clean one", () => {
+    const { el, v } = view([said(1), call(2, "a")]);
+    const calls = () => el.querySelector<HTMLDetailsElement>(".step-calls")!;
+    expect(calls().open).toBe(true);
+    v.apply(done(3, "a"));
+    expect(calls().open).toBe(false);
+    v.apply(call(4, "b"));
+    expect(calls().open).toBe(true);
+    v.apply(done(5, "b", "error"));
+    expect(calls().open).toBe(true);
+    expect(el.querySelector<HTMLElement>(".step")!.dataset.state).toBe("failed");
+  });
+  it("keeps the reader's toggle while the step's state holds", () => {
+    const { el, v } = view([said(1), call(2, "a"), done(3, "a")]);
+    const calls = el.querySelector<HTMLDetailsElement>(".step-calls")!;
+    calls.open = true; // the reader opens a finished step
+    v.apply(turn(4)); // plumbing joins it; the state is still done
+    expect(calls.open).toBe(true);
+  });
+  it("lays a replay out of order into the right steps, never doubling a row", () => {
+    const events = [said(1), call(2, "a"), turn(3), said(4, human), said(5), call(6, "b")];
+    const { el, v } = view([events[5], events[2], events[0], events[4], events[1], events[3]]);
+    expect(shape(el)).toEqual(["step(1: 2 3)", "4", "step(5: 6)"]);
+    for (const e of events) v.apply(e); // a clamped resume resends them all
+    expect(shape(el)).toEqual(["step(1: 2 3)", "4", "step(5: 6)"]);
+    for (const s of [1, 2, 3, 4, 5, 6]) expect(el.querySelectorAll(`[data-seq="${s}"]`)).toHaveLength(1);
+  });
+  it("splits a step when an agent message lands inside it, folds included", () => {
+    const { el, v } = view([said(1), call(2, "a"), turn(3), turn(5), turn(6), call(7, "b")]);
+    expect(shape(el)).toEqual(["step(1: 2 fold(3,5,6) 7)"]);
+    v.apply(said(4));
+    expect(shape(el)).toEqual(["step(1: 2 3)", "step(4: fold(5,6) 7)"]);
+  });
+  it("hands a step's calls to the stream when the cap evicts its owner", () => {
+    const { el } = view([said(1), call(2, "a"), call(3, "b"), call(4, "c")], 3);
+    expect(shape(el)).toEqual(["2", "3", "4"]);
+  });
+  it("opens the step and the fold around a row, so a link lands on it", () => {
+    const { el } = view([said(1), call(2, "a"), done(3, "a"), turn(4), turn(5)]);
+    const calls = el.querySelector<HTMLDetailsElement>(".step-calls")!;
+    expect(calls.open).toBe(false);
+    reveal(el.querySelector<HTMLElement>('[data-seq="5"]')!);
+    expect(calls.open).toBe(true);
+    expect(el.querySelector<HTMLDetailsElement>(".fold")!.open).toBe(true);
+  });
+});
+
 describe("ChatView call lines (P3)", () => {
   const call = (args: Record<string, unknown>, tool = "terminal") => ev(1, "tool_call", { callId: "c", tool, args });
   const done = (status: string, output = "") => ev(2, "tool_result", { callId: "c", status, output, truncated: false, bytes: output.length });
@@ -409,3 +509,21 @@ describe("ChatView call lines (P3)", () => {
     expect(el.querySelector(".t-more")).toBeNull();
   });
 });
+
+describe("ChatView step cost", () => {
+  // A step's line is a tally counted per row: a rescan per event made a 5,000-event
+  // replay of one step take a minute.
+  it("replays a long step without rescanning its rows", () => {
+    const el = document.createElement("div");
+    const v = new ChatView(el);
+    v.apply(ev(1, "message", { kind: "chat", text: "go", delivery: "none" }));
+    v.apply(ev(2, "tool_call", { callId: "c0", tool: "terminal", args: { command: "ls" } }));
+    const scans = vi.spyOn(el.querySelector<HTMLElement>(".step-body")!, "querySelectorAll");
+    for (let i = 1; i <= 300; i++) {
+      v.apply(ev(1 + 2 * i, "tool_result", { callId: `c${i - 1}`, status: "ok", output: "" }));
+      v.apply(ev(2 + 2 * i, "tool_call", { callId: `c${i}`, tool: "terminal", args: { command: "ls" } }));
+    }
+    expect(scans).not.toHaveBeenCalled();
+    expect(el.querySelector(".step-head")?.textContent).toBe("Running 301 commands");
+  });
+});
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd web && npx vitest run test/chat.test.ts`
Expected: FAIL, 11 of 56: the `ChatView steps` tests (no `section.step`) and the cost test. The cap-eviction test already passes, since without steps the calls sit in the stream anyway.

- [ ] **Step 3: Write the implementation**

```diff
diff --git a/web/src/chat.ts b/web/src/chat.ts
index 50991f2..c152044 100644
--- a/web/src/chat.ts
+++ b/web/src/chat.ts
@@ -13,6 +13,7 @@ import type { RoomEvent } from "./conn";
 import { deliveryChip, handoffHead, markdown, stateHead } from "./render";
 import { icon, type IconName } from "./icons";
 import { maxRows } from "./limits";
+import { count, emptyTally, stepLine, stepState, type StepCall, type StepTally } from "./step";
 
 function el(tag: string, cls: string, text?: string): HTMLElement {
   const e = document.createElement(tag);
@@ -332,6 +333,53 @@ function tidy(fold: HTMLElement) {
 // ChatView holds the log as a conversation, newest rows last. A resume can
 // resend a seq: the row is replaced in place. A tool_call's row is re-rendered
 // when its result or a decision lands, so a result never doubles its call.
+// A step's DOM: the section that holds its owner (the agent's message) and, once a call
+// or plumbing row joins, the toggle over them.
+const callsOf = (step: HTMLElement) => step.querySelector<HTMLDetailsElement>(":scope > details.step-calls");
+const stepBodyOf = (step: HTMLElement) => callsOf(step)?.querySelector<HTMLElement>(":scope > .step-body");
+
+// Each step's tally (step.ts), counted as rows land and leave.
+const tallies = new WeakMap<HTMLElement, StepTally>();
+const tallyOf = (step: HTMLElement) => {
+  let t = tallies.get(step);
+  if (!t) tallies.set(step, (t = emptyTally()));
+  return t;
+};
+
+// callOf is what a member row adds to its step's tally: a call, or nothing (plumbing).
+function callOf(row: HTMLElement): StepCall | undefined {
+  const d = row.dataset;
+  return d.callId === undefined ? undefined : { tool: d.tool!, status: d.status!, start: d.start!, end: d.end };
+}
+
+// recount tallies a step from its rows: for the rare moves (rows changing step on an
+// out-of-order replay), never on the live path.
+function recount(step: HTMLElement) {
+  const t = emptyTally();
+  for (const r of stepBodyOf(step)?.querySelectorAll<HTMLElement>("[data-flow]") ?? []) count(t, callOf(r), 1);
+  tallies.set(step, t);
+}
+
+// refreshStep rewrites a step's summary line from its tally and opens or closes it when
+// its state changes: open while a call runs or after one failed, closed once all are
+// ok. A state that did not change leaves the reader's own toggle alone.
+function refreshStep(step: HTMLElement) {
+  const calls = callsOf(step);
+  if (!calls) return;
+  const t = tallyOf(step);
+  const line = stepLine(t);
+  const sum = calls.querySelector(":scope > summary")!;
+  sum.replaceChildren(el("span", "step-head", line.head));
+  if (line.exceptions) sum.append(el("span", "step-exc", line.exceptions));
+  if (line.duration) sum.append(el("span", "step-dur", line.duration));
+  const state = stepState(t);
+  step.dataset.state = state;
+  if (calls.dataset.state !== state) {
+    calls.dataset.state = state;
+    calls.open = state !== "done";
+  }
+}
+
 export class ChatView {
   private rows = new Map<number, HTMLElement>();
   private seqs: number[] = [];
@@ -355,12 +403,14 @@ export class ChatView {
     const p = ev.payload ?? {};
     const you = this.viewer();
     const put = (row: HTMLElement) => {
-      if (plumbing(ev, you)) row.dataset.plumbing = "1";
-      this.put(ev.seq, row);
+      const quiet = plumbing(ev, you);
+      if (quiet) row.dataset.plumbing = "1";
+      this.put(ev.seq, row, quiet ? "member" : "break");
     };
     switch (ev.type) {
       case "message":
-        this.put(ev.seq, this.tools(bubble(ev, you), ev.seq));
+        // An agent's message opens a step; anyone else's closes the one open.
+        this.put(ev.seq, this.tools(bubble(ev, you), ev.seq), ev.actor.kind === "agent" ? "owner" : "break");
         break;
       case "tool_call":
         this.merge(String(p.callId ?? `seq${ev.seq}`), ev, "call");
@@ -390,10 +440,10 @@ export class ChatView {
         put(sysline(ev));
         break;
       case "handoff":
-        this.put(ev.seq, this.tools(handoff(ev), ev.seq));
+        this.put(ev.seq, this.tools(handoff(ev), ev.seq), "break");
         break;
       default:
-        this.put(ev.seq, fallback(ev));
+        this.put(ev.seq, fallback(ev), "break");
     }
   }
 
@@ -412,17 +462,7 @@ export class ChatView {
     if (part === "call") c.call = ev;
     else if (part === "result") c.result = ev;
     else if (!c.decisions.some((d) => d.seq === ev.seq)) c.decisions.push(ev);
-    this.put(c.seq, toolRow(c));
-  }
-
-  private drop(seq: number) {
-    const i = this.seqs.indexOf(seq);
-    if (i < 0) return;
-    this.detach(this.rows.get(seq)!);
-    this.rows.delete(seq);
-    this.seqs.splice(i, 1);
-    this.regroup(i - 1);
-    this.regroup(i);
+    this.put(c.seq, toolRow(c), "member");
   }
 
   // tools puts a "fork here" beside row, outside any summary, when forking is on.
@@ -442,18 +482,108 @@ export class ChatView {
     return wrap;
   }
 
-  // The fold around a row, if any; top is what sits in the stream for it.
+  // The layout is two layers over the seq order. A step: an agent's message (its owner)
+  // and the member rows after it (tool rows and plumbing), up to the next row that is
+  // neither, inside one section.step. A fold: a run of two or more plumbing rows, inside
+  // whatever holds them (a step's body, or the stream before any step).
+  private flow(i: number): string | undefined { return this.rows.get(this.seqs[i])?.dataset.flow; }
+  private isPlumbing(i: number): boolean { return this.rows.get(this.seqs[i])?.dataset.plumbing === "1"; }
+
+  // The fold around a row, if any.
   private foldOf(n: HTMLElement): HTMLElement | undefined {
     const parent = n.parentElement;
     return parent?.classList.contains("fold-body") ? parent.parentElement ?? undefined : undefined;
   }
-  private top(n: HTMLElement): HTMLElement { return this.foldOf(n) ?? n; }
+  // within is the child of box that holds n, or null when box does not hold it.
+  private within(n: HTMLElement, box: HTMLElement): HTMLElement | null {
+    let c: HTMLElement | null = n;
+    while (c && c.parentElement !== box) c = c.parentElement;
+    return c;
+  }
+  // top is what sits in the stream for a row: its step, its fold, or the row itself.
+  private top(n: HTMLElement): HTMLElement { return this.within(n, this.el) ?? n; }
+  private stepOf(n: HTMLElement): HTMLElement | undefined {
+    const t = this.top(n);
+    return t.classList.contains("step") ? t : undefined;
+  }
+  // members is a step's member box, made on its first member.
+  private members(step: HTMLElement): HTMLElement {
+    const have = stepBodyOf(step);
+    if (have) return have;
+    const calls = el("details", "step-calls") as HTMLDetailsElement;
+    const inner = el("div", "step-body");
+    calls.append(el("summary", ""), inner);
+    step.append(calls);
+    return inner;
+  }
+  // box is where member i belongs: the body of the step its nearest non-member before
+  // it owns, else the stream. Constant time: the row before i already sits in place.
+  private box(i: number): HTMLElement {
+    if (i === 0) return this.el;
+    const prev = this.rows.get(this.seqs[i - 1])!;
+    if (prev.dataset.flow === "owner") return this.members(this.stepOf(prev)!);
+    if (prev.dataset.flow === "member") return (this.foldOf(prev) ?? prev).parentElement as HTMLElement;
+    return this.el;
+  }
+  // settle drops a step's toggle once no member is left in it, and rewrites its line.
+  private settle(step: HTMLElement | undefined) {
+    if (!step) return;
+    const inner = stepBodyOf(step);
+    if (inner && inner.childElementCount === 0) callsOf(step)!.remove();
+    else refreshStep(step);
+  }
   private detach(n: HTMLElement) {
     const fold = this.foldOf(n);
+    const step = this.stepOf(n);
     n.remove();
     if (fold) tidy(fold);
+    if (step) count(tallyOf(step), callOf(n), -1);
+    this.settle(step);
+  }
+
+  // rehome moves the members from index i on (up to the next non-member) into box,
+  // before anchor: an owner landing among them takes them; a break or a lost owner
+  // hands them to the stream. Their folds move whole.
+  private rehome(i: number, box: HTMLElement, anchor: HTMLElement | null) {
+    const touched = new Set<HTMLElement>();
+    for (let j = i; j < this.seqs.length && this.flow(j) === "member"; j++) {
+      const row = this.rows.get(this.seqs[j])!;
+      const home = (this.foldOf(row) ?? row);
+      if (home.parentElement === box) continue;
+      const from = this.stepOf(row);
+      if (from) touched.add(from);
+      box.insertBefore(home, anchor);
+    }
+    const to = this.stepOf(box);
+    if (to && touched.size) touched.add(to);
+    for (const s of touched) {
+      recount(s);
+      this.settle(s);
+    }
+  }
+
+  private drop(seq: number) {
+    const i = this.seqs.indexOf(seq);
+    if (i < 0) return;
+    const row = this.rows.get(seq)!;
+    const flow = row.dataset.flow;
+    const step = flow === "owner" ? this.stepOf(row) : undefined;
+    this.detach(row);
+    this.rows.delete(seq);
+    this.seqs.splice(i, 1);
+    if (step) {
+      // A step without its owner hands its members to whatever holds the row before it.
+      const box = this.box(i);
+      this.rehome(i, box, step);
+      step.remove();
+      this.settle(this.stepOf(box));
+    } else if (flow === "break") {
+      const box = this.box(i);
+      this.rehome(i, box, box === this.el ? (i > 0 ? this.top(this.rows.get(this.seqs[i - 1])!).nextElementSibling as HTMLElement | null : this.el.firstElementChild as HTMLElement | null) : null);
+    }
+    this.regroup(i - 1);
+    this.regroup(i);
   }
-  private isPlumbing(i: number): boolean { return this.rows.get(this.seqs[i])?.dataset.plumbing === "1"; }
 
   // regroup lays out the run of consecutive plumbing rows around index i: one fold for
   // two or more, the row alone for one. A fold that already holds just the run stays
@@ -463,14 +593,15 @@ export class ChatView {
     let a = i, b = i;
     while (a > 0 && this.isPlumbing(a - 1)) a--;
     while (b < this.seqs.length - 1 && this.isPlumbing(b + 1)) b++;
+    const box = this.box(a);
     const items = this.seqs.slice(a, b + 1).map((s) => this.rows.get(s)!);
     const held = new Set<Element>(items);
-    const after = b + 1 < this.seqs.length ? this.top(this.rows.get(this.seqs[b + 1])!) : null;
+    const after = b + 1 < this.seqs.length ? this.within(this.rows.get(this.seqs[b + 1])!, box) : null;
     const sources = new Set(items.map((n) => this.foldOf(n)).filter((f): f is HTMLElement => !!f));
     if (items.length === 1) {
       const fold = this.foldOf(items[0]);
       if (fold) {
-        this.el.insertBefore(items[0], after);
+        box.insertBefore(items[0], after);
         tidy(fold);
       }
       return;
@@ -483,7 +614,7 @@ export class ChatView {
     }
     const inner = body(target);
     if (inner.children.length !== items.length || items.some((n, k) => inner.children[k] !== n)) inner.replaceChildren(...items);
-    if (target.parentElement !== this.el || target.nextElementSibling !== after) this.el.insertBefore(target, after);
+    if (target.parentElement !== box || target.nextElementSibling !== after) box.insertBefore(target, after);
     for (const f of sources) if (f !== target) tidy(f);
     tidy(target);
   }
@@ -499,37 +630,69 @@ export class ChatView {
     return true;
   }
 
-  private put(seq: number, row: HTMLElement) {
+  private put(seq: number, row: HTMLElement, flow: "owner" | "member" | "break") {
+    row.dataset.flow = flow;
     const old = this.rows.get(seq);
-    if (old && old.dataset.plumbing === row.dataset.plumbing) {
-      old.replaceWith(row); // inside its fold, if it is in one: a resend never doubles a line
+    if (old && old.dataset.plumbing === row.dataset.plumbing && old.dataset.flow === flow) {
+      const step = this.stepOf(old);
+      old.replaceWith(row); // inside its fold or step, if in one: a resend never doubles a line
       this.rows.set(seq, row);
+      if (step) {
+        count(tallyOf(step), callOf(old), -1);
+        count(tallyOf(step), callOf(row), 1);
+        refreshStep(step);
+      }
       return;
     }
     if (old) this.drop(seq);
     let i = this.seqs.length;
     while (i > 0 && this.seqs[i - 1] > seq) i--; // from the end: live events land there
-    const next = i < this.seqs.length ? this.top(this.rows.get(this.seqs[i])!) : null;
+    const last = i === this.seqs.length;
     this.seqs.splice(i, 0, seq);
     this.rows.set(seq, row);
-    if (row.dataset.plumbing === "1") {
+    if (flow === "member") {
+      const box = this.box(i);
+      const next = last ? null : this.within(this.rows.get(this.seqs[i + 1])!, box);
       // The live path: a plumbing row after a fold's last row joins it, in constant time.
-      if (next !== null || !this.joinsLastFold(i, row)) {
-        this.el.insertBefore(row, next);
-        this.regroup(i);
+      if (row.dataset.plumbing === "1") {
+        if (next !== null || !this.joinsLastFold(i, row)) {
+          box.insertBefore(row, next);
+          this.regroup(i);
+        }
+      } else {
+        box.insertBefore(row, next);
+        this.regroup(i - 1);
+        this.regroup(i + 1);
+      }
+      const step = this.stepOf(row);
+      if (step) {
+        count(tallyOf(step), callOf(row), 1);
+        refreshStep(step);
       }
     } else {
-      this.el.insertBefore(row, next);
-      this.regroup(i - 1);
-      this.regroup(i + 1);
+      // An owner or a break sits in the stream, right after what holds the row before it.
+      const prev = i > 0 ? this.top(this.rows.get(this.seqs[i - 1])!) : null;
+      const anchor = (prev ? prev.nextElementSibling : this.el.firstElementChild) as HTMLElement | null;
+      let node = row;
+      if (flow === "owner") {
+        node = el("section", "step");
+        node.append(row);
+      }
+      this.el.insertBefore(node, anchor);
+      this.regroup(i - 1); // a plumbing run it lands in splits here
+      if (!last) {
+        // The members after it were the step before's (or the stream's): now they are its own.
+        if (flow === "owner") this.rehome(i + 1, this.members(node), null);
+        else this.rehome(i + 1, this.el, node.nextElementSibling as HTMLElement | null);
+        this.regroup(i + 1);
+        this.settle(node.classList.contains("step") ? node : undefined);
+      }
     }
     while (this.seqs.length > this.cap) {
-      const s = this.seqs.shift()!;
+      const s = this.seqs[0];
       const gone = this.rows.get(s)!;
       if (gone.dataset.callId) this.calls.delete(gone.dataset.callId);
-      this.detach(gone);
-      this.rows.delete(s);
-      this.regroup(0);
+      this.drop(s);
     }
   }
 }
```

- [ ] **Step 4: Run them to verify they pass**

Run: `cd web && npm test && npm run typecheck`
Expected: PASS (392 tests).

- [ ] **Step 5: Check the replay cost by hand (no file kept)**

```ts
// web/test/perf.tmp.test.ts: delete after the run
import { it } from "vitest";
import { ChatView } from "../src/chat";
import type { RoomEvent } from "../src/conn";
it("replays one 5,000-event step", () => {
  const v = new ChatView(document.createElement("div"));
  const mk = (seq: number, type: string, payload: unknown) => ({ v: 1, id: `i${seq}`, seq, roomId: "r", runId: "x",
    actor: { kind: "agent", id: "agent:x" }, type, origin: "harness", ts: "2026-10-04T14:00:00Z", redactions: [], payload }) as RoomEvent;
  const t0 = performance.now();
  v.apply(mk(1, "message", { kind: "chat", text: "m", delivery: "none" }));
  for (let i = 0; i < 2500; i++) {
    v.apply(mk(2 + 2 * i, "tool_call", { callId: `c${i}`, tool: "terminal", args: { command: "ls" } }));
    v.apply(mk(3 + 2 * i, "tool_result", { callId: `c${i}`, status: "ok", output: "x" }));
  }
  console.log("ms", Math.round(performance.now() - t0));
});
```

Run: `npx vitest run test/perf.tmp.test.ts`
Expected: under ~2 s in jsdom (0.9 s measured). Then `rm web/test/perf.tmp.test.ts`.

- [ ] **Step 6: Commit**

```bash
git add web/src/chat.ts web/test/chat.test.ts
git commit -m "feat(web): group an agent's tool calls under its message as one step"
```

---

### Task 5: The composer (P4, P5)

`mountComposer` is rewritten. The text box becomes `textarea[name=text]` with the placeholder, next to a primary `Send` button carrying the send icon (`data-act="message"`, as before). Below them:

- a `fieldset.delivery` with a visually hidden `legend` (`Deliver to`), holding radios `name=delivery`. Each radio has its `aria-label` and `aria-describedby="delivery-notice delivery-hint"`.
- `#delivery-hint`, then `#delivery-notice` (`role=status`, polite), then the key hint.

Behaviour:

- **Steer:** its label is hidden while no run is on, and the radio is disabled for a non-driver.
- **Lost token or ended run:** Steer is unchecked, the hint is cleared, and the notice says why (R09, unchanged).
- **Shortcut:** Ctrl/⌘+Enter clicks Send; Enter alone stays a newline.

**Files:**
- Modify: `web/src/controls.ts` (import `icon`; `deliveries`; `mountComposer` rewritten)
- Test: `web/test/controls.test.ts`, `web/test/room.test.ts` (the tests that set the old `<select>` now check radios)

**Interfaces:**
- Consumes: `icon` (task 2).
- Produces: `mountComposer(root, conn, state, you)`, with the same signature and the same return value `{ refresh, onAck, setLive }`. The DOM described above.

- [ ] **Step 1: Write the failing tests**

`setup()` gains `radio(value)`, `delivery()` and `pick(value)`. The select-based tests move to radios with every R09 assertion kept. New tests cover the shortcut, the Send label and icon, and the hint copy:

```diff
diff --git a/web/test/controls.test.ts b/web/test/controls.test.ts
index d65fb07..ba636e3 100644
--- a/web/test/controls.test.ts
+++ b/web/test/controls.test.ts
@@ -32,7 +32,11 @@ function setup(who: You, driver = "human:a", epoch = 4) {
   const btn = (act: string) => [...wrap.querySelectorAll<HTMLButtonElement>(`button[data-act="${act}"]`)].find(shown) ?? null;
   const field = <T extends HTMLElement>(name: string) => wrap.querySelector<T>(`[name="${name}"]`)!;
   const deliveryNotice = () => dock.querySelector("#delivery-notice")!.textContent;
-  return { sent, notices, state, root, dock, wrap, controls, composer, btn, field, deliveryNotice, acts: () => sent.map((f) => f.action) };
+  const radio = (value: string) => dock.querySelector<HTMLInputElement>(`input[name="delivery"][value="${value}"]`)!;
+  const delivery = () => dock.querySelector<HTMLInputElement>('input[name="delivery"]:checked')?.value ?? "";
+  // pick checks a delivery as a click would: the group hears the change.
+  const pick = (value: string) => { radio(value).checked = true; radio(value).dispatchEvent(new Event("change", { bubbles: true })); };
+  return { sent, notices, state, root, dock, wrap, controls, composer, btn, field, deliveryNotice, radio, delivery, pick, acts: () => sent.map((f) => f.action) };
 }
 const mountOwner = () => setup(you("human:a", "owner"));
 
@@ -80,7 +84,7 @@ describe("mountControls", () => {
   it("sends the composer's text with its delivery and the epoch it was decided on", () => {
     const c = setup(you("human:a", "owner"));
     c.field<HTMLTextAreaElement>("text").value = "look at the flaky test";
-    c.field<HTMLSelectElement>("delivery").value = "steering";
+    c.pick("steering");
     c.btn("message")!.click();
     expect(c.sent[0]).toMatchObject({ type: "act", driverEpoch: 4,
       action: { kind: "message", text: "look at the flaky test", delivery: "steering" } });
@@ -109,44 +113,41 @@ describe("mountControls", () => {
     const c = setup(you("human:b", "collaborator"));
     c.state.apply({ ...queued(0, "system:room-broker", ""), type: "participant", payload: { principal: "agent:r1", change: "joined" } });
     c.controls.refresh();
-    const steer = c.wrap.querySelector<HTMLOptionElement>('option[value="steering"]')!;
+    const steer = c.radio("steering");
     expect(steer.disabled).toBe(true);
     c.state.apply({ ...queued(1, "human:a", ""), type: "driver", payload: { from: "human:a", to: "human:b", epoch: 5, reason: "given" } });
     c.controls.refresh();
     expect(steer.disabled).toBe(false);
     // Losing the token picks nothing for the human: queued text enters an LLM prompt,
     // so neither the room nor the queue is a safe guess (R09, reversing review 4.5 I3).
-    const delivery = c.field<HTMLSelectElement>("delivery");
-    delivery.value = "steering";
+    c.pick("steering");
     c.state.apply({ ...queued(2, "human:a", ""), type: "driver", payload: { from: "human:b", to: "human:c", epoch: 6, reason: "given" } });
     c.controls.refresh();
     expect(steer.disabled).toBe(true);
-    expect(delivery.value).toBe("");
+    expect(c.delivery()).toBe("");
     const choose = "You no longer hold the driver token: choose where this message goes.";
-    expect(c.deliveryNotice()).toBe(choose); // beside the select, not the page's act notice
+    expect(c.deliveryNotice()).toBe(choose); // beside the group, not the page's act notice
     expect(c.notices).toEqual([]);
     c.field<HTMLTextAreaElement>("text").value = "steer this";
     c.btn("message")!.click();
     expect(c.sent).toHaveLength(0);
     expect(c.deliveryNotice()).toBe(choose);
-    delivery.value = "none";
-    delivery.dispatchEvent(new Event("change")); // a choice answers the notice
+    c.pick("none"); // a choice answers the notice
     expect(c.deliveryNotice()).toBe("");
     c.btn("message")!.click();
     expect(c.acts()).toEqual([{ kind: "message", text: "steer this", delivery: "none" }]);
   });
 
-  // R09: each option says what it does; the room is the default, since it prompts no agent.
-  it("names where each delivery goes, the room by default", () => {
-    const delivery = setup(you("human:a", "owner")).field<HTMLSelectElement>("delivery");
-    expect(delivery.value).toBe("none");
-    expect([...delivery.options].map((o) => [o.value, o.textContent])).toEqual([
-      ["none", "post to the room (no agent is prompted)"],
-      ["queued", "queue for the next run's brief"],
-      ["steering", "steer the running agent now"],
-      ["", "choose where this goes"],
-    ]);
-    expect(delivery.options[3].disabled).toBe(true);
+  // R09: each choice says what it does; the room is the default, since it prompts no agent.
+  it("names where each delivery goes, the room by default, and says what the chosen one does", () => {
+    const c = setup(you("human:a", "owner"));
+    expect(c.delivery()).toBe("none");
+    const choices = [...c.dock.querySelectorAll<HTMLInputElement>('input[name="delivery"]')];
+    expect(choices.map((r) => [r.value, r.closest("label")!.textContent])).toEqual([["none", "Room"], ["queued", "Queue"], ["steering", "Steer"]]);
+    const hint = () => c.dock.querySelector("#delivery-hint")!.textContent;
+    expect(hint()).toBe("Posted to the room; no agent is prompted.");
+    c.pick("queued");
+    expect(hint()).toBe("Queued for the next run's brief.");
   });
 
   it("shows the queue as text, with remove for its author and steer for the driver", () => {
@@ -382,13 +383,13 @@ describe("the control groups", () => {
     }
   });
 
-  // Spec: the delivery notice sits beside the select it is about.
-  it("puts the delivery notice right after the Deliver to field", () => {
+  // Spec: the delivery notice sits beside the group it is about.
+  it("puts the delivery notice right after the Deliver to group and its hint", () => {
     const { dock } = mountOwner();
-    const select = dock.querySelector('select[name="delivery"]')!;
-    const notice = dock.querySelector("#delivery-notice")!;
-    expect(select.closest("label")!.nextElementSibling).toBe(notice);
-    expect(select.getAttribute("aria-describedby")).toBe("delivery-notice");
+    const group = dock.querySelector("fieldset.delivery")!;
+    expect(group.nextElementSibling?.id).toBe("delivery-hint");
+    expect(group.nextElementSibling!.nextElementSibling?.id).toBe("delivery-notice");
+    for (const r of dock.querySelectorAll('input[name="delivery"]')) expect(r.getAttribute("aria-describedby")).toBe("delivery-notice delivery-hint");
   });
 
   // Spec: a principal shows short, the full id as its title, as in the strip.
@@ -427,7 +428,7 @@ describe("the control groups", () => {
       expect(f.getAttribute("placeholder"), name).toBeNull();
     }
     expect(wrap.querySelector('select[name="role"]')!.closest("label")!.firstChild!.textContent).toBe("Start a run as");
-    expect(wrap.querySelector('select[name="delivery"]')!.closest("label")!.firstChild!.textContent).toBe("Deliver to");
+    expect(wrap.querySelector("fieldset.delivery > legend")!.textContent).toBe("Deliver to");
   });
 
   it("hides Run and Driver once the room is sealed", () => {
@@ -482,9 +483,29 @@ describe("mountComposer", () => {
     for (const f of [c.field("text"), c.field("delivery"), send]) expect(shown(f)).toBe(false);
   });
 
-  it("ties the notice to the select", () => {
+  it("sends on Ctrl+Enter or Cmd+Enter, and takes Enter alone as a new line", () => {
+    const c = mountOwner();
+    const text = c.field<HTMLTextAreaElement>("text");
+    text.value = "first line";
+    const press = (init: KeyboardEventInit) => {
+      const e = new KeyboardEvent("keydown", { key: "Enter", cancelable: true, ...init });
+      text.dispatchEvent(e);
+      return e;
+    };
+    expect(press({}).defaultPrevented).toBe(false);
+    expect(c.sent).toHaveLength(0);
+    expect(press({ ctrlKey: true }).defaultPrevented).toBe(true);
+    expect(c.acts()).toEqual([{ kind: "message", text: "first line", delivery: "none" }]);
+    press({ metaKey: true });
+    expect(c.sent).toHaveLength(2);
+  });
+
+  it("labels Send in words beside its icon, and says what the field is for", () => {
     const c = mountOwner();
-    expect(c.field("delivery").getAttribute("aria-describedby")).toBe("delivery-notice");
+    const send = c.btn("message")!;
+    expect(send.textContent).toBe("Send");
+    expect(send.querySelector("svg")?.getAttribute("aria-hidden")).toBe("true");
+    expect(c.field("text").getAttribute("placeholder")).toBe("Message the room…");
   });
 });
 
```

```diff
diff --git a/web/test/room.test.ts b/web/test/room.test.ts
index 0a9e0c8..ed5c3da 100644
--- a/web/test/room.test.ts
+++ b/web/test/room.test.ts
@@ -135,20 +135,21 @@ describe("the room page", () => {
     vi.setSystemTime(Date.parse("2026-10-06T10:00:05Z"));
     const p = page();
     p.join(snapshot({ principal: "human:a", role: "owner" }), 0); // human:a drives
-    const steer = () => p.app.querySelector<HTMLOptionElement>('select[name="delivery"] option[value="steering"]')!;
+    const steer = () => p.app.querySelector<HTMLInputElement>('input[name="delivery"][value="steering"]')!;
+    const shown = () => !steer().closest("label")!.hidden;
     p.last().recv(event(1, "message", { kind: "chat", text: "anyone?", delivery: "none" }));
     expect(p.app.querySelector(".now")?.textContent).toBe("No run yet"); // a human's message starts no run
-    expect(steer().hidden).toBe(true);
+    expect(shown()).toBe(false);
     p.last().recv(event(2, "participant", { principal: "agent:ikely2yk", change: "joined", role: "implementer" }));
     p.last().recv(agent(3, "message", { kind: "chat", text: "on it", delivery: "none" }));
     expect(p.app.querySelector(".now")?.textContent).toBe("Now ▸ on it · message · 5 s ago");
-    expect([steer().hidden, steer().disabled]).toEqual([false, false]);
-    p.app.querySelector<HTMLSelectElement>('select[name="delivery"]')!.value = "steering";
+    expect([shown(), steer().disabled]).toEqual([true, false]);
+    steer().checked = true;
     p.last().recv(event(4, "participant", { principal: "agent:ikely2yk", change: "left" }));
     expect(p.app.querySelector(".now")?.textContent).toBe("No run · last: on it · 5 s ago");
     expect(p.app.querySelector(".now-hint")?.textContent).toBe("Start one under Run");
-    expect([steer().hidden, steer().disabled]).toEqual([true, true]);
-    expect(p.app.querySelector<HTMLSelectElement>('select[name="delivery"]')!.value).toBe(""); // a steer with no run: chosen again
+    expect([shown(), steer().disabled]).toEqual([false, true]);
+    expect(p.app.querySelector('input[name="delivery"]:checked')).toBeNull(); // a steer with no run: chosen again
     expect(p.app.querySelector("#delivery-notice")?.textContent).toMatch(/no run/i);
   });
 
@@ -308,7 +309,7 @@ describe("the room page", () => {
     p.join(snapshot({ principal: "human:a", role: "owner" }), 0); // human:a drives
     const notice = () => p.app.querySelector("footer .act-notice")!.textContent;
     const beside = () => p.app.querySelector("#delivery-notice")!.textContent;
-    p.app.querySelector<HTMLSelectElement>('select[name="delivery"]')!.value = "steering";
+    p.app.querySelector<HTMLInputElement>('input[name="delivery"][value="steering"]')!.checked = true;
     p.app.querySelector<HTMLInputElement>('[name="giveTo"]')!.value = "human:c";
     p.app.querySelector<HTMLButtonElement>('[data-act="driver_give"]')!.click();
     const give = p.last().sent.at(-1) as { clientSeq: number };
@@ -356,10 +357,10 @@ describe("the room page", () => {
     const p = page();
     p.join(snapshot({ principal: "human:a", role: "owner" }), 0); // human:a drives
     const notice = () => p.app.querySelector("#delivery-notice")!.textContent;
-    const delivery = p.app.querySelector<HTMLSelectElement>('select[name="delivery"]')!;
+    const radio = (v: string) => p.app.querySelector<HTMLInputElement>(`input[name="delivery"][value="${v}"]`)!;
     const text = p.app.querySelector<HTMLTextAreaElement>('textarea[name="text"]')!;
     const send = p.app.querySelector<HTMLButtonElement>('[data-act="message"]')!;
-    delivery.value = "steering";
+    radio("steering").checked = true;
     p.last().recv(event(1, "driver", { from: "human:a", to: "human:c", epoch: 2, reason: "taken" }));
     const choose = "You no longer hold the driver token: choose where this message goes.";
     expect(notice()).toBe(choose);
@@ -368,7 +369,7 @@ describe("the room page", () => {
     send.click(); // no delivery chosen: refused, and said again
     expect(p.last().sent).toHaveLength(frames);
     expect(notice()).toBe(choose);
-    delivery.value = "none";
+    radio("none").checked = true;
     send.click();
     const msg = p.last().sent.at(-1) as { clientSeq: number; action: unknown };
     expect(msg.action).toEqual({ kind: "message", text: "for the room, then", delivery: "none" });
@@ -471,17 +472,17 @@ describe("the room page", () => {
   });
 
   // Review focus 5.
-  it("keeps a steering draft and moves the notice beside the select when the token is lost (review focus 5)", () => {
+  it("keeps a steering draft and moves the notice beside the delivery group when the token is lost (review focus 5)", () => {
     const p = page();
     p.join(snapshot({ role: "collaborator", driver: true }), 0);
     const dock = p.app.querySelector("footer.page-dock")!;
     const text = dock.querySelector("textarea") as HTMLTextAreaElement;
-    const delivery = dock.querySelector('select[name="delivery"]') as HTMLSelectElement;
-    text.value = "turn left"; delivery.value = "steering";
+    const steer = dock.querySelector('input[name="delivery"][value="steering"]') as HTMLInputElement;
+    text.value = "turn left"; steer.checked = true;
     p.last().recv(event(1, "driver", { from: "human:b", to: "human:c", epoch: 2 }));
     expect(text.value).toBe("turn left");
-    expect(delivery.value).toBe("");
-    expect(delivery.getAttribute("aria-describedby")).toBe("delivery-notice");
+    expect(dock.querySelector('input[name="delivery"]:checked')).toBeNull();
+    expect(steer.getAttribute("aria-describedby")).toBe("delivery-notice delivery-hint");
     expect(dock.querySelector("#delivery-notice")?.textContent).toMatch(/choose where this message goes/);
     expect((dock.querySelector('button[data-act="message"]') as HTMLButtonElement).disabled).toBe(false);
   });
@@ -492,7 +493,7 @@ describe("the room page", () => {
     p.last().recv({ ...event(1, "state_changed", { kind: "room_phase", phase: "Closed", reason: "owner" }), event: { ...event(1, "state_changed", {}).event,
       actor: { kind: "human", id: "human:b" }, payload: { kind: "room_phase", phase: "Closed", reason: "owner" } } });
     expect(p.app.querySelector(".closed")?.textContent).toMatch(/^Closed .* by you$/);
-    expect((p.app.querySelector("footer.page-dock textarea") as HTMLElement).hidden).toBe(true);
+    expect(p.app.querySelector("footer.page-dock textarea")!.closest("[hidden]")).not.toBeNull();
     expect(p.app.querySelector<HTMLElement>(".group-run")!.hidden).toBe(true);
     expect(p.app.querySelector<HTMLElement>(".group-driver")!.hidden).toBe(true);
   });
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd web && npx vitest run test/controls.test.ts test/room.test.ts`
Expected: FAIL, 11 of 74. They find no `input[name="delivery"]`, `#delivery-hint` or `fieldset.delivery`, and see Send's text as `send`.

- [ ] **Step 3: Write the implementation**

```diff
diff --git a/web/src/controls.ts b/web/src/controls.ts
index 0365493..d42a614 100644
--- a/web/src/controls.ts
+++ b/web/src/controls.ts
@@ -1,6 +1,7 @@
 // SPDX-License-Identifier: Apache-2.0
 
 import type { Frame, Snapshot } from "./conn";
+import { icon } from "./icons";
 import type { RoomState } from "./room-state";
 import { safeLink, type Summary } from "./summary";
 
@@ -369,50 +370,82 @@ export function mountControls(root: HTMLElement, conn: Sender, state: RoomState,
   };
 }
 
-// mountComposer renders the dock's composer into root: the text, where it goes, Send.
-// The text stays until its own ack accepts it: a rejected message is not lost. setLive
-// disables Send while the socket is down and hides nothing, so the draft stays in view;
-// a sealed room hides the composer, and a watcher has none.
+// The three places a message can go, the room first: it prompts no agent (R09). Each
+// says what it does in the hint line once chosen.
+const deliveries: [value: string, label: string, hint: string][] = [
+  ["none", "Room", "Posted to the room; no agent is prompted."],
+  ["queued", "Queue", "Queued for the next run's brief."],
+  ["steering", "Steer", "Sent to the running agent now."],
+];
+
+// mountComposer renders the dock's composer into root: the text and Send, then where it
+// goes (a segmented radio group), the hint for that choice, and the notice. The text
+// stays until its own ack accepts it: a rejected message is not lost. setLive disables
+// Send while the socket is down and hides nothing, so the draft stays in view; a sealed
+// room hides the composer, and a watcher has none. Ctrl or Cmd with Enter sends; Enter
+// alone is a new line, so a multi-line message is never sent half-typed.
 export function mountComposer(root: HTMLElement, conn: Sender, state: RoomState, you: Snapshot["you"]) {
   const isDriver = () => state.driver === you.principal;
-  const text = named(el("textarea", { name: "text", maxLength: 16384, rows: 2 }), "message to the room");
-  // The room is the default: queued and steered text enters an agent's prompt (R09).
-  const delivery = select("delivery", "Deliver to", [["none", "post to the room (no agent is prompted)"],
-    ["queued", "queue for the next run's brief"], ["steering", "steer the running agent now"]]);
-  delivery.append(el("option", { value: "", textContent: "choose where this goes", disabled: true, hidden: true }));
-  delivery.setAttribute("aria-describedby", "delivery-notice");
-  const steering = delivery.querySelector<HTMLOptionElement>('option[value="steering"]')!;
+  const text = named(el("textarea", { name: "text", maxLength: 16384, rows: 2, placeholder: "Message the room…" }), "message to the room");
+  const group = el("fieldset", { className: "delivery" });
+  group.append(el("legend", { textContent: "Deliver to" }));
+  const radios = new Map<string, HTMLInputElement>();
+  for (const [value, label] of deliveries) {
+    const r = named(el("input", { type: "radio", name: "delivery", value, checked: value === "none" }), label);
+    r.setAttribute("aria-describedby", "delivery-notice delivery-hint");
+    radios.set(value, r);
+    const l = el("label", { className: "seg" });
+    l.append(r, el("span", { textContent: label }));
+    group.append(l);
+  }
+  const steering = radios.get("steering")!;
+  const chosen = () => [...radios.values()].find((r) => r.checked)?.value ?? "";
+  const hint = el("span", { id: "delivery-hint", className: "hint" });
+  const showHint = () => { hint.textContent = deliveries.find(([v]) => v === chosen())?.[2] ?? ""; };
   const choose = "You no longer hold the driver token: choose where this message goes.";
   const noRun = "No run is on now, so no agent to steer: choose where this message goes.";
-  // The notice sits beside the select it is about, so the page's act notice keeps its own.
+  // The notice sits beside the group it is about, so the page's act notice keeps its own.
   const notice = el("span", { id: "delivery-notice", className: "notice" });
   notice.setAttribute("role", "status");
   notice.setAttribute("aria-live", "polite");
-  delivery.onchange = () => { notice.textContent = ""; };
+  group.onchange = () => { notice.textContent = ""; showHint(); };
   let sent = { seq: 0, text: "" };
-  const sendBtn = button("send", "message", () => {
+  const sendBtn = button("Send", "message", () => {
     if (!text.value.trim()) return;
-    if (!delivery.value) { notice.textContent = choose; return; }
-    sent = { seq: act(conn, state, { kind: "message", text: text.value, delivery: delivery.value }), text: text.value };
+    const delivery = chosen();
+    if (!delivery) { notice.textContent = choose; return; }
+    sent = { seq: act(conn, state, { kind: "message", text: text.value, delivery }), text: text.value };
     if (sent.seq) notice.textContent = ""; // a send that went out supersedes the notice
   });
-  sendBtn.classList.add("primary");
+  sendBtn.classList.add("primary", "send");
+  sendBtn.append(icon("send"));
+  text.onkeydown = (e) => {
+    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
+      e.preventDefault();
+      sendBtn.click();
+    }
+  };
+  const write = el("div", { className: "composer-write" });
+  write.append(text, sendBtn);
   const row = el("div", { className: "composer-row" });
-  row.append(labelled("Deliver to", delivery), notice, sendBtn);
-  root.replaceChildren(text, row);
+  row.append(group, hint, notice, el("span", { className: "hint keys", textContent: "Ctrl+Enter to send" }));
+  root.replaceChildren(write, row);
+  showHint();
   return {
     refresh() {
       // Steering needs the token and an agent present now (Q2): with no run it is not offered.
-      steering.hidden = !state.running();
-      steering.disabled = !isDriver() || steering.hidden;
+      const label = steering.closest("label")!;
+      label.hidden = !state.running();
+      steering.disabled = !isDriver() || label.hidden;
       // A lost token or an ended run picks no other delivery for the human: both prompt or
       // skip an agent the human meant to reach (R09, reversing review 4.5 I3). The draft stays.
-      if (steering.disabled && delivery.value === "steering") {
-        delivery.value = "";
+      if (steering.disabled && steering.checked) {
+        steering.checked = false;
+        showHint();
         notice.textContent = isDriver() ? noRun : choose;
       }
       root.hidden = you.role === "watcher";
-      text.hidden = row.hidden = state.sealed;
+      write.hidden = row.hidden = state.sealed;
     },
     onAck(f: Frame) {
       if (!f.rejected && f.clientSeq && f.clientSeq === sent.seq && text.value === sent.text) text.value = "";
```

- [ ] **Step 4: Run them to verify they pass**

Run: `cd web && npm test && npm run typecheck`
Expected: PASS (393 tests).

- [ ] **Step 5: Commit**

```bash
git add web/src/controls.ts web/test/controls.test.ts web/test/room.test.ts
git commit -m "feat(web): a composer you can find: segmented delivery, Send, Ctrl+Enter"
```

---

### Task 6: The Session disclosure and the skeleton (P7, P9)

`room.ts` makes the following changes:

- **Session:** `section.controls` moves into `details.session` (collapsed), the dock's first child. Its summary line is rewritten on every frame and event by `showSession()`, and it is hidden for a viewer without controls.
- **Skeleton:** three `.sk-msg` shapes (`aria-hidden`) stand where the stream will be, until the first state frame or event.
- **Preview:** `preview/full.ts` mounts the same dock, with the session open when the URL has `?session`.

**Files:**
- Modify: `web/src/room.ts`, `web/preview/full.ts`
- Test: `web/test/room.test.ts`

**Interfaces:**
- Consumes: `shortPrincipal` (already exported by controls.ts), `hasControls`, and `RoomState.running()`.
- Produces:
  - the page order `header, banner, needs, lead, .skeleton, main, fork, footer`
  - the footer's children `details.session, .composer, .dock-meta`

- [ ] **Step 1: Write the failing tests**

```diff
diff --git a/web/test/room.test.ts b/web/test/room.test.ts
index ed5c3da..399f993 100644
--- a/web/test/room.test.ts
+++ b/web/test/room.test.ts
@@ -447,6 +447,36 @@ describe("the room page", () => {
     expect(p.app.querySelector<HTMLElement>("footer .composer")!.hidden).toBe(false);
   });
 
+  it("puts Run and Driver behind a Session toggle in the dock, its line naming the driver and the run", () => {
+    const p = page();
+    p.join(snapshot({ principal: "human:a", role: "owner" }), 0); // human:a drives
+    const session = p.app.querySelector<HTMLDetailsElement>("footer.page-dock > details.session")!;
+    expect(session.open).toBe(false);
+    expect(session.hidden).toBe(false);
+    expect(session.querySelector("section.controls .group-run")).not.toBeNull();
+    expect(session.querySelector("section.controls .group-driver")).not.toBeNull();
+    expect(session.querySelector("summary")!.textContent).toBe("Session · driver: you · no run");
+    p.last().recv(event(1, "participant", { principal: "agent:ikely2yk", change: "joined", role: "implementer" }));
+    expect(session.querySelector("summary")!.textContent).toBe("Session · driver: you · a run is on");
+    expect(session.nextElementSibling?.classList.contains("composer")).toBe(true);
+  });
+
+  it("hides the Session toggle from a watcher, who has no controls", () => {
+    const p = page();
+    p.join(snapshot({ role: "watcher" }), 0);
+    expect(p.app.querySelector<HTMLElement>("details.session")!.hidden).toBe(true);
+  });
+
+  it("shows a skeleton stream until the first frame, then the stream alone", () => {
+    const p = page();
+    const skeleton = p.app.querySelector<HTMLElement>(".skeleton")!;
+    expect(skeleton.hidden).toBe(false);
+    expect(skeleton.querySelectorAll(".sk-msg")).toHaveLength(3);
+    expect(skeleton.getAttribute("aria-hidden")).toBe("true");
+    p.join(snapshot(), 0);
+    expect(skeleton.hidden).toBe(true);
+  });
+
   it("docks the composer at the bottom and disables Send while reconnecting or sealed", () => {
     const p = page();
     p.join(snapshot({ role: "collaborator" }), 0);
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd web && npx vitest run test/room.test.ts`
Expected: FAIL, 3 of 40. They find no `details.session` or `.skeleton`.

- [ ] **Step 3: Write the implementation**

```diff
diff --git a/web/src/room.ts b/web/src/room.ts
index ccf3a5b..48c6e68 100644
--- a/web/src/room.ts
+++ b/web/src/room.ts
@@ -1,13 +1,13 @@
 // SPDX-License-Identifier: Apache-2.0
 
 // The room page: the header, the reconnecting banner, the needs-you panel, the notes,
-// the chat and raw views, the controls and the dock (the composer, the connection), all
-// fed by one RoomConnection.
+// the chat and raw views, and the dock (the session's controls behind a toggle, the
+// composer, the connection), all fed by one RoomConnection.
 
 import { api } from "./api";
 import { focusRow } from "./chat";
 import { RoomConnection, type Options, type Snapshot } from "./conn";
-import { hasControls, mountComposer, mountControls, rejection } from "./controls";
+import { hasControls, mountComposer, mountControls, rejection, shortPrincipal } from "./controls";
 import { mountFork } from "./fork";
 import { mountNeeds } from "./needs";
 import { changesSummary, fetchSummary, focusApproval, renderNotes, type Summary } from "./summary";
@@ -39,12 +39,29 @@ export function mountRoom(app: HTMLElement, id: string, theme: ThemeController,
   summaryError.setAttribute("role", "status");
   summaryError.hidden = true;
   lead.append(blocks, summaryError);
+  // Until the first frame, three grey message shapes stand where the stream will be:
+  // an empty page reads as broken (P9).
+  const skeleton = document.createElement("div");
+  skeleton.className = "skeleton";
+  skeleton.setAttribute("aria-hidden", "true");
+  for (let i = 0; i < 3; i++) skeleton.append(Object.assign(document.createElement("div"), { className: "sk-msg" }));
   const main = document.createElement("main");
   const section = document.createElement("section");
   section.className = "controls";
+  // The session: Run, Driver, the queue and Room, behind a toggle in the dock (P7). Its
+  // line says who drives and whether a run is on, so a closed toggle still answers both.
+  const session = document.createElement("details");
+  session.className = "session";
+  const sessionLine = document.createElement("summary");
+  session.append(sessionLine, section);
+  const showSession = () => {
+    session.hidden = !you || !hasControls(you);
+    const holder = !state.driver ? "nobody" : state.driver === you?.principal ? "you" : shortPrincipal(state.driver);
+    sessionLine.textContent = `Session · driver: ${holder} · ${state.running() ? "a run is on" : "no run"}`;
+  };
   const forkPanel = document.createElement("section");
-  // The dock: the composer (mounted with the first state frame), then the page's act
-  // notice, the theme button and the connection.
+  // The dock: the session, the composer (mounted with the first state frame), then the
+  // page's act notice, the theme button and the connection.
   const footer = document.createElement("footer");
   footer.className = "page-dock";
   const composerEl = document.createElement("div");
@@ -61,8 +78,8 @@ export function mountRoom(app: HTMLElement, id: string, theme: ThemeController,
   meta.append(notice);
   mountThemeButton(meta, theme);
   meta.append(status);
-  footer.append(composerEl, meta);
-  app.replaceChildren(header, banner, needsPanel, lead, main, section, forkPanel, footer);
+  footer.append(session, composerEl, meta);
+  app.replaceChildren(header, banner, needsPanel, lead, skeleton, main, forkPanel, footer);
   const views = mountViews(main, (seq) => fork.open(seq), () => snap?.you.principal);
   const state = new RoomState();
   let you: Snapshot["you"] | undefined;
@@ -199,6 +216,8 @@ export function mountRoom(app: HTMLElement, id: string, theme: ThemeController,
         composer.setLive(connection === "live");
       }
       composer.refresh();
+      showSession();
+      skeleton.hidden = true;
       land();
     },
     onEvent: (e) => {
@@ -209,6 +228,8 @@ export function mountRoom(app: HTMLElement, id: string, theme: ThemeController,
       refreshStrip();
       controls?.refresh();
       composer?.refresh();
+      showSession();
+      skeleton.hidden = true;
       land();
       // A membership change naming you: the page projects none (R10). The reconnect's
       // state frame re-resolves you, and the broker re-checks every act anyway.
```

```diff
diff --git a/web/preview/full.ts b/web/preview/full.ts
index 6dbb1bb..b1f66ac 100644
--- a/web/preview/full.ts
+++ b/web/preview/full.ts
@@ -39,6 +39,11 @@ async function run() {
   const main = document.createElement("main");
   const section = document.createElement("section");
   section.className = "controls";
+  // The session toggle, as room.ts mounts it; open, so the preview shows the controls.
+  const session = document.createElement("details");
+  session.className = "session";
+  session.open = new URLSearchParams(location.search).has("session");
+  session.append(Object.assign(document.createElement("summary"), { textContent: "Session · driver: you · a run is on" }), section);
   const forkPanel = document.createElement("section");
   const footer = document.createElement("footer");
   footer.className = "page-dock";
@@ -55,8 +60,8 @@ async function run() {
   meta.append(notice);
   mountThemeButton(meta, theme);
   meta.append(status);
-  footer.append(composerEl, meta);
-  app.replaceChildren(header, needsPanel, lead, main, section, forkPanel, footer);
+  footer.append(session, composerEl, meta);
+  app.replaceChildren(header, needsPanel, lead, main, forkPanel, footer);
 
   // Acts go nowhere: the preview has no broker.
   const sender = { send: () => { notice.textContent = "Offline preview: nothing was sent."; return false; } };
```

- [ ] **Step 4: Run them to verify they pass**

Run: `cd web && npm test && npm run typecheck`
Expected: PASS (396 tests).

- [ ] **Step 5: Commit**

```bash
git add web/src/room.ts web/test/room.test.ts web/preview/full.ts
git commit -m "feat(web): Run and Driver behind a Session toggle in the dock; a skeleton while connecting"
```

---

### Task 7: Colour, dock, steps and polish in CSS (P4–P10), the bundle, the visual check

**Token changes:**

- **Dark:**
  - `--muted` `#001b2b`
  - `--card` `#002d47`
  - `--border` `#13527d`
  - `--input` `#5b8db0`
  - `--brand-navy` `#003a5c`
  - new `--surface-raised` `#003a5c`
  - new `--shadow-dock`
- **Light:** `--input` `#8f8f8f`, `--surface-raised` `#ffffff`, `--shadow-dock`.

**Other CSS changes:**

- **Meter:** the budget meter's track mix goes from 20% to 12%, which keeps the fill at 3:1 on the raised header.
- **New rules:** the dock (sticky, with a shadow, no border), the session, the composer, the segmented delivery group, the steps, the call lines and the skeleton.
- **Buttons:** a 28 px minimum height (40 px for the composer's and the needs panel's), `scale(.96)` on press, explicit transition properties, a disabled look.

**Files:**
- Modify: `web/src/app.css`, `web/test/contrast.test.ts`
- Regenerate: `internal/humanapi/ui/dist/app.css`, `internal/humanapi/ui/dist/app.js`

**Interfaces:**
- Consumes: the class names from tasks 3–6.
- Produces: the tokens `--surface-raised` and `--shadow-dock`.

- [ ] **Step 1: Write the failing test**

```diff
diff --git a/web/test/contrast.test.ts b/web/test/contrast.test.ts
index 1e01a62..a014a28 100644
--- a/web/test/contrast.test.ts
+++ b/web/test/contrast.test.ts
@@ -35,7 +35,7 @@ const pairs = (t: Record<string, string>) => [
   ["the header's text", t["--brand-navy-foreground"], t["--brand-navy"], 4.5],
   ["the strip's chip borders", mix(t["--brand-navy-foreground"], t["--brand-navy"], 0.45), t["--brand-navy"], 3],
   ["the needs-you pill", t["--brand-navy"], t["--warning"], 4.5],
-  ["the budget meter's fill", t["--accent"], mix(t["--brand-navy-foreground"], t["--brand-navy"], 0.20), 3],
+  ["the budget meter's fill", t["--accent"], mix(t["--brand-navy-foreground"], t["--brand-navy"], 0.12), 3],
   ["the needs-you border", t["--warning-border"], t["--card"], 3],
   ["the deny button's text on a card", t["--destructive-text"], t["--card"], 4.5],
   ["the controls' group names", t["--muted-foreground"], t["--background"], 4.5],
@@ -43,6 +43,17 @@ const pairs = (t: Record<string, string>) => [
   ["the room list's notice on the page", t["--destructive-text"], t["--muted"], 4.5],
   ["destructive text on the page (.status-error, .summary-error, .mk-harness_error)", t["--destructive"], t["--muted"], 4.5],
   ["the focus ring on the header", t["--ring"], t["--brand-navy"], 3],
+  // The dock (P4, P8): its own surface, the composer's boundary, the delivery segments.
+  ["text on the dock", t["--foreground"], t["--surface-raised"], 4.5],
+  ["muted text on the dock (session line, hints)", t["--muted-foreground"], t["--surface-raised"], 4.5],
+  ["placeholder text in the composer", t["--placeholder"], t["--card"], 4.5],
+  ["the composer's boundary on the dock", t["--input"], t["--surface-raised"], 3],
+  ["an unchecked delivery segment", t["--muted-foreground"], t["--muted"], 4.5],
+  ["the checked delivery segment", t["--foreground"], t["--card"], 4.5],
+  ["the focus ring on the dock", t["--ring"], t["--surface-raised"], 3],
+  // A step's line and a call's status (P1, P3).
+  ["a step's exceptions on the page", t["--warning-foreground"], t["--muted"], 4.5],
+  ["a rejected call's word on a card", t["--warning-foreground"], t["--card"], 4.5],
 ] as const;
 
 describe.each([["light", light], ["dark", dark]])("%s theme", (_name, t) => {
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd web && npx vitest run test/contrast.test.ts`
Expected: FAIL, 8 of 70. `--surface-raised` is undefined (`token defined`), the composer boundary is under 3:1 (`#dedede` / `#004070`), and the meter fill fails at the 12% track.

- [ ] **Step 3: Write the stylesheet**

```diff
diff --git a/web/src/app.css b/web/src/app.css
index 618204b..7bb541a 100644
--- a/web/src/app.css
+++ b/web/src/app.css
@@ -1,8 +1,12 @@
 /* SPDX-License-Identifier: Apache-2.0 */
-/* The platform's design tokens, verbatim from the app-wizard (ui/src/index.css):
-   one design language across the platform's UIs. Light is the default; the
-   .dark block applies when theme.ts puts the class on <html> (the wizard's
-   contract — a media query would ignore the user's explicit choice). */
+/* The platform's design tokens, from the app-wizard (ui/src/index.css): one design
+   language across the platform's UIs. Light is the default; the .dark block applies
+   when theme.ts puts the class on <html> (the wizard's contract — a media query would
+   ignore the user's explicit choice).
+   Diverges from the wizard (2026-10-18 room UI polish, P8), to port back there: dark
+   --muted, --card, --border, --input and --brand-navy separate the page, the cards and
+   the header (page and card were both #003552, 1.00:1); --input is a 3:1 boundary in
+   both schemes (WCAG 1.4.11); --surface-raised is new, the dock's surface. */
 :root {
   color-scheme: light;
 
@@ -14,7 +18,8 @@
   --muted: #f2f2f2; /* haze surface */
   --muted-foreground: #575757; /* neutral text — AA on white */
   --border: #dedede;
-  --input: #dedede;
+  --input: #8f8f8f; /* an input's boundary: 3.2:1 on white */
+  --surface-raised: #ffffff; /* the dock: a card, lifted by its shadow */
 
   --primary: #0077b8; /* ocean blue */
   --primary-foreground: #ffffff;
@@ -53,6 +58,8 @@
     0 0 0 1px rgb(0 0 0 / 0.08),
     0 1px 2px -1px rgb(0 0 0 / 0.08),
     0 2px 4px 0 rgb(0 0 0 / 0.06);
+  /* The dock floats over the stream it scrolls past. */
+  --shadow-dock: 0 -1px 0 rgb(0 0 0 / 0.06), 0 -6px 16px -4px rgb(0 0 0 / 0.10);
 }
 
 .dark {
@@ -60,13 +67,14 @@
 
   --background: #002538; /* deep navy */
   --foreground: #f2f2f2;
-  --card: #003552; /* mid navy surface */
+  --card: #002d47; /* a card: 1.23:1 on the page */
   --card-foreground: #f2f2f2;
 
-  --muted: #003552;
+  --muted: #001b2b; /* the page: the deepest navy, so cards and the header lift off it */
   --muted-foreground: #b8c6d0;
-  --border: #004070; /* logo navy */
-  --input: #004070;
+  --border: #13527d; /* structure: 1.7:1 on a card */
+  --input: #5b8db0; /* an input's boundary: 3.3:1 on the dock */
+  --surface-raised: #003a5c; /* the dock: 1.48:1 on the page */
 
   --primary: #2b9fda; /* lightened ocean blue for contrast on navy */
   --primary-foreground: #002538;
@@ -87,7 +95,9 @@
   /* Text on the error chip's 10% tint: --destructive is 4.13:1 there. */
   --destructive-text: #f47e7e;
 
-  --brand-navy: #002538;
+  /* The header: the raised navy, so it lifts off the page (the brand's #002538 is
+     1.11:1 on it). */
+  --brand-navy: #003a5c;
   --brand-navy-foreground: #f2f2f2;
 
   --placeholder: #9db0bc;
@@ -98,6 +108,7 @@
   /* Layered depth shadows are invisible against navy — collapse to the ring. */
   --shadow-border: 0 0 0 1px rgb(255 255 255 / 0.08);
   --shadow-border-hover: 0 0 0 1px rgb(255 255 255 / 0.13);
+  --shadow-dock: 0 -1px 0 rgb(255 255 255 / 0.10), 0 -6px 16px -4px rgb(0 0 0 / 0.45);
 }
 
 html {
@@ -128,21 +139,59 @@ a { color: var(--link); }
 }
 /* The dock: the composer on the left (the text over its row: deliver to, the notice, send),
    the act notice, the theme button and the connection on the right; one column on a phone. */
+/* Sticky, not fixed: it sits in the flow at the page's end, so once scrolled there it
+   covers no row. The shadow, not a border, lifts it over the rows it scrolls past. */
 .page-dock {
   bottom: 0; z-index: 2;
-  display: grid; grid-template-columns: minmax(0, 1fr) auto; gap: 6px 12px; align-items: end;
+  display: grid; grid-template-columns: minmax(0, 1fr) auto; gap: 8px 12px; align-items: end;
+  padding-top: 10px; padding-bottom: 10px;
   font-variant-numeric: tabular-nums;
-  background: var(--card); color: var(--card-foreground);
-  border-top: 1px solid var(--border);
+  background: var(--surface-raised); color: var(--card-foreground);
+  box-shadow: var(--shadow-dock);
+}
+/* The session: Run, Driver, the queue and Room, behind a toggle across the dock (P7).
+   Open, it scrolls inside a bound, so the dock never takes the whole screen. */
+.page-dock > .session { grid-column: 1 / -1; min-width: 0; }
+.session > summary { cursor: pointer; font-size: 13px; color: var(--muted-foreground); width: fit-content; }
+.session[open] > section.controls { max-height: 40vh; overflow: auto; margin-top: 6px; }
+.page-dock .controls { padding: 0; background: transparent; }
+.page-dock .composer { display: grid; gap: 8px; min-width: 0; }
+/* The composer (P4): the text and Send side by side, Send as tall as the field. */
+.composer-write { display: flex; gap: 8px; align-items: stretch; min-width: 0; }
+.composer-write textarea {
+  flex: 1 1 auto; min-width: 0; box-sizing: border-box; font: inherit;
+  padding: 8px 10px; border-radius: 8px; resize: vertical;
+  /* Grows with the text from two lines to eight where the browser can; rows=2 elsewhere. */
+  field-sizing: content; min-height: calc(2lh + 18px); max-height: calc(8lh + 18px);
 }
-.page-dock .composer { display: grid; gap: 6px; min-width: 0; }
-.page-dock textarea { width: 100%; box-sizing: border-box; font: inherit; padding: 6px; resize: vertical; }
+button.send { display: inline-flex; gap: 6px; align-items: center; justify-content: center; padding: 0 16px; font-weight: 600; border-radius: 8px; min-width: 88px; }
 .composer-row, .dock-meta { display: flex; flex-wrap: wrap; gap: 6px 12px; align-items: center; min-width: 0; }
+.composer-row .hint { color: var(--muted-foreground); font-size: 12px; }
+.composer-row .keys { margin-left: auto; }
+/* Where a message goes (P5): a segmented radio group. The radios stay real inputs, for
+   the keyboard and assistive tech; the checked one's label is the raised segment. */
+fieldset.delivery { display: inline-flex; gap: 2px; margin: 0; padding: 2px; border: 0; border-radius: 8px; background: var(--muted); min-width: 0; }
+fieldset.delivery > legend { position: absolute; width: 1px; height: 1px; overflow: hidden; clip-path: inset(50%); white-space: nowrap; }
+.seg { position: relative; display: flex; }
+.seg input { position: absolute; inset: 0; margin: 0; opacity: 0; cursor: pointer; }
+.seg span {
+  padding: 4px 12px; border-radius: 6px; font-size: 13px; color: var(--muted-foreground);
+  transition-property: background-color, color, box-shadow; transition-duration: 150ms; transition-timing-function: ease-out;
+}
+.seg input:checked + span { background: var(--card); color: var(--foreground); font-weight: 600; box-shadow: var(--shadow-border); }
+.seg input:focus-visible + span { outline: 2px solid var(--ring); outline-offset: 1px; }
+.seg input:disabled + span { opacity: .5; }
 .dock-meta { justify-content: flex-end; }
 .page-dock .status { color: var(--muted-foreground); font-family: ui-monospace, monospace; }
 .page-dock[data-status="live"] .status { color: var(--success-text); }
 .page-dock .notice { color: var(--destructive); overflow-wrap: anywhere; }
-@media (max-width: 640px) { .page-dock { grid-template-columns: minmax(0, 1fr); } .dock-meta { justify-content: space-between; } }
+@media (max-width: 640px) {
+  .page-dock { grid-template-columns: minmax(0, 1fr); }
+  .dock-meta { justify-content: space-between; }
+  .composer-write { flex-direction: column; }
+  button.send { min-height: 40px; }
+  .composer-row .keys { display: none; }
+}
 /* Not live: under the strip, why and when the page retries. */
 .banner { background: color-mix(in srgb, var(--warning) 18%, var(--card)); color: var(--warning-foreground); padding: 6px 16px; }
 main { padding: 8px 16px 16px; max-width: 100%; }
@@ -151,8 +200,16 @@ button {
   font: inherit; cursor: pointer;
   background: var(--card); color: var(--card-foreground);
   border: 1px solid var(--border); border-radius: 6px; padding: 2px 10px;
+  min-height: 28px;
+  transition-property: box-shadow, background-color, scale; transition-duration: 150ms; transition-timing-function: ease-out;
 }
 button:hover { box-shadow: var(--shadow-border-hover); }
+/* Press feedback: never below .95, which reads as exaggerated. */
+button:active:not(:disabled) { scale: .96; }
+button:disabled { opacity: .55; cursor: not-allowed; }
+/* The acts a reader comes to do get a full target: Send, approve, deny. */
+.composer button, .needs button { min-height: 40px; }
+.icon { width: 14px; height: 14px; flex: none; vertical-align: -2px; }
 button:focus-visible, input:focus-visible, select:focus-visible, textarea:focus-visible, summary:focus-visible {
   outline: 2px solid var(--ring); outline-offset: 1px;
 }
@@ -170,7 +227,7 @@ button:focus-visible, input:focus-visible, select:focus-visible, textarea:focus-
 .page-header a { color: var(--brand-navy-foreground); text-decoration: underline; }
 .meter-bar {
   display: inline-block; width: 64px; height: 6px; border-radius: 3px; vertical-align: middle;
-  background: color-mix(in srgb, var(--brand-navy-foreground) 20%, transparent); position: relative;
+  background: color-mix(in srgb, var(--brand-navy-foreground) 12%, transparent); position: relative;
 }
 .meter-bar::after { content: ""; position: absolute; inset: 0; width: var(--fill, 0%); background: var(--accent); border-radius: 3px; }
 .waiting, .closed { font-size: 13px; color: var(--brand-navy-foreground); opacity: .9; }
@@ -195,7 +252,7 @@ button:focus-visible, input:focus-visible, select:focus-visible, textarea:focus-
 /* Chat view */
 .msg { margin: 12px 0; max-width: 72ch; }
 .msg-head { color: var(--muted-foreground); font-size: 12px; display: flex; gap: 8px; align-items: baseline; margin-bottom: 3px; }
-.msg-head time, .t-time { margin-left: auto; color: var(--placeholder); font-size: 12px; white-space: nowrap; }
+.msg-head time, .t-time { margin-left: auto; color: var(--placeholder); font-size: 12px; white-space: nowrap; font-variant-numeric: tabular-nums; }
 .bubble {
   background: var(--card); color: var(--card-foreground);
   border-radius: 4px 12px 12px 12px; padding: 6px 12px;
@@ -212,19 +269,34 @@ button:focus-visible, input:focus-visible, select:focus-visible, textarea:focus-
   border-radius: 8px; margin: 6px 0; max-width: 72ch;
   box-shadow: var(--shadow-border);
 }
-.tool summary { display: flex; gap: 8px; align-items: baseline; padding: 6px 10px; cursor: pointer; list-style: none; flex-wrap: wrap; border-radius: 8px; }
+.tool summary { display: flex; gap: 8px; align-items: center; padding: 6px 10px; cursor: pointer; list-style: none; flex-wrap: wrap; border-radius: 8px; }
 .tool summary::before { content: "▸"; color: var(--placeholder); }
 .tool[open] summary::before { content: "▾"; }
 .tool summary::-webkit-details-marker { display: none; }
 .tool.st-rejected, .tool.st-error { box-shadow: var(--shadow-border), inset 3px 0 0 color-mix(in srgb, var(--destructive) 65%, var(--card)); }
 .tool.st-rejected { box-shadow: var(--shadow-border), inset 3px 0 0 color-mix(in srgb, var(--warning) 80%, var(--card)); }
 .t-name { color: var(--link); font-weight: 600; }
+.t-prompt { color: var(--placeholder); font-family: ui-monospace, monospace; font-size: 13px; }
+/* A call's status (P3): ok a muted check, running the link colour, the rest in words. */
+.t-status { display: inline-flex; gap: 4px; align-items: center; font-size: 12px; }
+.t-status.st-ok { color: var(--placeholder); }
+.t-status.st-running { color: var(--link); }
+.t-status.st-rejected { color: var(--warning-foreground); }
+.t-status.st-error { color: var(--destructive-text); }
+@media (prefers-reduced-motion: no-preference) {
+  .icon-spinner { animation: spin 1s linear infinite; }
+  @keyframes spin { to { rotate: 360deg; } }
+}
 .t-cmd { font-family: ui-monospace, monospace; font-size: 13px; min-width: 0; flex: 1 1 12em; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
 .tool[open] .t-cmd { white-space: normal; overflow-wrap: anywhere; }
 .t-body { border-top: 1px solid var(--border); padding: 6px 10px; }
 .t-sec { color: var(--placeholder); font-size: 12px; text-transform: uppercase; letter-spacing: .04em; margin: 6px 0 2px; }
 .t-meta { color: var(--placeholder); font-size: 12px; margin-top: 6px; overflow-wrap: anywhere; }
 .t-body pre, .other pre { background: var(--muted); border-radius: 6px; padding: 6px 8px; }
+.t-command { font-size: 13px; margin: 2px 0 6px; }
+.t-out { max-height: none; }
+.t-out.capped { max-height: 16em; }
+.t-more { font-size: 12px; }
 
 /* Chips and badges: a tinted pill, the status token for its text. */
 .chip, .pol {
@@ -284,6 +356,26 @@ button:focus-visible, input:focus-visible, select:focus-visible, textarea:focus-
 .fold > summary { cursor: pointer; list-style: none; }
 .fold > summary::-webkit-details-marker { display: none; }
 .fold-body { padding-left: 14px; }
+/* A step (P1): an agent's message, then its calls behind one line, on a rail under it. */
+.step { margin: 12px 0; max-width: 72ch; }
+.step > .msg, .step > .row > .msg { margin: 0; }
+.step-calls { margin: 6px 0 0 3px; padding-left: 12px; border-left: 2px solid var(--border); }
+.step[data-state="failed"] > .step-calls { border-left-color: color-mix(in srgb, var(--destructive) 65%, var(--card)); }
+.step-calls > summary {
+  display: flex; gap: 8px; align-items: baseline; padding: 4px 0; cursor: pointer; list-style: none;
+  font-size: 13px; font-weight: 500; color: var(--muted-foreground);
+}
+.step-calls > summary::-webkit-details-marker { display: none; }
+.step-calls > summary::before { content: "▸"; color: var(--placeholder); }
+.step-calls[open] > summary::before { content: "▾"; }
+.step-exc { color: var(--warning-foreground); font-weight: 600; }
+.step-dur { margin-left: auto; color: var(--placeholder); font-size: 12px; font-variant-numeric: tabular-nums; }
+/* Inside a step a call is a line, not a card: the rail groups them. */
+.step-body > .tool { background: transparent; box-shadow: none; margin: 0; }
+.step-body > .tool > summary:hover { background: color-mix(in srgb, var(--card) 70%, transparent); }
+.step-body > .tool[open] { background: var(--card); box-shadow: var(--shadow-border); margin: 4px 0; }
+.step-body > .fold { margin: 2px 0 2px 10px; }
+.step-body .mark { max-width: none; } /* its time aligns with the call lines' */
 /* An actor's colour, keyed on its id: decoration beside the name, which carries the meaning. */
 .swatch { display: inline-block; width: 8px; height: 8px; border-radius: 2px; margin-right: -2px; flex: none; align-self: center; }
 .swatch-0 { background: var(--primary); }
@@ -378,6 +470,14 @@ button.deny { color: var(--destructive-text); border-color: var(--destructive-te
 .notes ul { margin: 4px 0 0; padding-left: 18px; }
 .raw-events { margin-top: 8px; }
 .raw-events > summary { cursor: pointer; padding: 4px 0; }
+/* Until the first frame (P9): three message shapes, pulsing where motion is welcome. */
+.skeleton { padding: 8px 16px; display: grid; gap: 12px; max-width: 72ch; }
+.sk-msg { height: 56px; border-radius: 12px; background: var(--card); box-shadow: var(--shadow-border); }
+.sk-msg:nth-child(2) { width: 70%; }
+.sk-msg:nth-child(3) { width: 85%; }
+@media (prefers-reduced-motion: no-preference) {
+  .sk-msg { animation: pulse 1.6s ease-in-out infinite; }
+}
 
 @media (prefers-reduced-motion: reduce) {
   *, *::before, *::after {
```

- [ ] **Step 4: Run it to verify it passes, then rebuild the bundle**

Run: `cd web && npm test && npm run typecheck && npm run build`
Expected: PASS (414 tests). Then `git status` shows `internal/humanapi/ui/dist/app.css` and `app.js` modified.

- [ ] **Step 5: Look at it**

Run `npm run preview`. Then screenshot `/preview/full.html` and `?theme=dark` (add `&session` once) at 390, 1024 and 1280 px:

```bash
chromium --headless=new --hide-scrollbars --virtual-time-budget=4000 --window-size=1280,1600 \
  --screenshot=/tmp/after-dark-1280.png "http://localhost:4180/preview/full.html?theme=dark"
```

Check against the spec:

- the page, the cards, the header and the dock are visibly distinct in dark mode
- each agent message's calls sit on a rail under one `Ran N commands` line
- the rejected push's step is open with `1 rejected` in amber
- the composer reads as an input, with Send beside it (under it at 390 px)
- the segments show Room checked
- nothing scrolls sideways at 390 px

Keep the six screenshots for the PR.

- [ ] **Step 6: The full gate**

Run: `task check`
Expected: exit 0. The relevant steps are `ui:test`, `ui:check` (the bundle matches what `web/` builds), `docs` and `test`. The Go tests need Docker for testcontainers.

- [ ] **Step 7: Commit**

```bash
git add web/src/app.css web/test/contrast.test.ts internal/humanapi/ui/dist
git commit -m "feat(web): layered dark surfaces, a docked composer and step styling"
```

---

## After the tasks

- **PR:** open a PR from `feat/room-ui-polish`, in English, with before/after screenshots in both themes and a link to the spec. End the description with the attribution line the session's system reminder gives. Commits carry no trailer (the owner's rule).
- **Follow-up (not in this PR):** port the dark tokens back to app-wizard (spec, Out of scope).
