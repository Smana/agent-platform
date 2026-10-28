import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { changesSummary, fetchSummary, needsItems, renderNotes, type Summary } from "../src/summary";

const base = (): Summary => ({
  apiVersion: "summary/v1", room: "3kq7x2ma", url: "https://rooms.example/r/3kq7x2ma",
  status: {
    phase: "Running",
    run: { id: "run-1", role: "implementer", trigger: "issue", startedAt: "2026-10-08T10:00:00Z" },
    budget: { usedTokens: 1200, limitTokens: 50000 },
    pr: { number: 7, url: "https://github.com/o/r/pull/7", author: "agent:impl", reviewers: ["human:a"] },
    issue: { number: 3, url: "https://github.com/o/r/issues/3", author: "human:a", labelledBy: "human:a" },
    lastVerdict: { by: "agent:rev", verdict: "approve", at: "2026-10-08T10:05:00Z" },
  },
  needsYou: [{ kind: "approval", id: "ap1", what: "run kubectl apply", deadline: "2026-10-08T12:00:00Z", url: "https://rooms.example/r/3kq7x2ma#ap1" }],
  actions: [{ kind: "steer", what: "Steer the running agent" }, { kind: "message", what: "Write to the room", cli: "maku say 3kq7x2ma hi" }],
  notes: { untrusted: true, items: [{ at: "2026-10-08T10:01:00Z", run: "run-1", text: "reading the repo" }] },
  cursor: "seq:9",
});

const render = (s: Summary) => {
  const root = document.createElement("div");
  renderNotes(root, s);
  return root;
};
const needs = (s: Summary) => {
  const ul = document.createElement("ul");
  ul.append(...needsItems(s));
  return ul;
};

describe("renderNotes", () => {
  it("renders the notes block alone: needs you lives in the panel (needs.ts)", () => {
    const r = render(base());
    expect([...r.children].map((s) => (s as HTMLElement).dataset.block)).toEqual(["notes"]);
  });

  // U7: the latest note on one line, labelled the agent's claim; the rest behind a toggle.
  it("shows the latest note inline and the rest behind Notes (N)", () => {
    const s = base();
    s.notes.items = [
      { at: "2026-10-08T10:01:00Z", run: "run-1", text: "reading the repo" },
      { at: "2026-10-08T10:03:00Z", run: "run-1", text: "running the tests" },
      { at: "2026-10-08T10:05:00Z", run: "ikely2yk", text: "Quoting the last three commits" },
    ];
    const root = document.createElement("div");
    renderNotes(root, s, Date.parse("2026-10-08T10:07:00Z"));
    const latest = root.querySelector(".notes > p.note-latest")!;
    expect(latest.firstChild?.textContent).toBe("ikely2yk · 2 min ago · Quoting the last three commits");
    expect(latest.querySelector(".hint")?.textContent).toBe("the agent's claim");
    const d = root.querySelector<HTMLDetailsElement>(".notes > details")!;
    expect(d.open).toBe(false);
    expect(d.querySelector("summary")?.textContent).toBe("Notes (2)");
    expect([...d.querySelectorAll("li")].map((l) => l.textContent)).toEqual([
      expect.stringContaining("run-1: running the tests"), expect.stringContaining("run-1: reading the repo")]);
  });
  it("has no toggle for a single note, and no block at all without notes", () => {
    const one = render(base());
    expect(one.querySelector(".note-latest")).not.toBeNull();
    expect(one.querySelector("details")).toBeNull();
    const s = base();
    s.notes.items = [];
    expect(render(s).children).toHaveLength(0);
  });

  it("renders agent text as text, never as markup", () => {
    const s = base();
    const evil = "<img src=x onerror=alert(1)>";
    s.notes.items[0].text = evil;
    const r = render(s);
    expect(r.querySelector("img")).toBeNull();
    expect(r.querySelector('[data-block="notes"]')?.textContent).toContain(evil);
  });

  it("labels notes as the agent's claim", () => {
    expect(render(base()).querySelector('[data-block="notes"]')?.textContent).toMatch(/agent's claim/i);
  });

  it("shows a watcher no action buttons", () => {
    const s = base();
    s.actions = [];
    s.needsYou = [];
    expect(render(s).querySelectorAll("button")).toHaveLength(0);
  });
});

describe("needsItems", () => {
  // N3: a hold is no approval: it says what releases it, with no decide link.
  it("renders a hold as what releases it", () => {
    const s = base();
    s.needsYou = [{ kind: "hold", id: "waiting_human_driver", what: "hand the driver back", url: "https://rooms.example/r/3kq7x2ma" }];
    const block = needs(s);
    expect(block.textContent).toContain("Hold: hand the driver back");
    expect(block.textContent).not.toMatch(/Approval|Review and decide/);
    expect(block.querySelector("a")).toBeNull();
  });

  // #51: a merge waiting on the maintainer, with how to merge under the ruleset and a link to the PR.
  it("renders a merge with its instruction and the PR link", () => {
    const s = base();
    s.needsYou = [{ kind: "merge", id: "7", what: "merge PR #7: tick bypass", url: "https://github.com/o/r/pull/7" }];
    const block = needs(s);
    expect(block.textContent).toContain("Merge: merge PR #7: tick bypass");
    expect(block.querySelector("a")?.getAttribute("href")).toBe("https://github.com/o/r/pull/7");
    expect(block.textContent).not.toMatch(/Review and decide/);
  });

  // The approvals come from the live log, as cards (needs.test.ts): the summary's are skipped.
  it("skips approvals, and is empty when nothing needs you", () => {
    expect(needsItems(base())).toHaveLength(0);
    const s = base();
    s.needsYou = [];
    expect(needsItems(s)).toHaveLength(0);
  });

  it("renders agent text as text, never as markup", () => {
    const s = base();
    const evil = "<img src=x onerror=alert(1)>";
    s.needsYou = [{ kind: "merge", id: "7", what: evil, url: "https://github.com/o/r/pull/7" }, { kind: "hold", id: "h", what: evil, url: "u" }];
    const block = needs(s);
    expect(block.querySelector("img")).toBeNull();
    expect(block.textContent).toContain(evil);
  });

  it("links only https URLs", () => {
    const s = base();
    s.needsYou = [{ kind: "merge", id: "7", what: "merge PR #7", url: "javascript:alert(1)" }];
    const r = needs(s);
    const hrefs = [...r.querySelectorAll("a")].map((a) => a.getAttribute("href"));
    expect(hrefs.some((h) => h?.startsWith("javascript:") || h?.startsWith("http:"))).toBe(false);
    expect(r.textContent).toContain("javascript:alert(1)");
  });
});

describe("changesSummary", () => {
  const ev = (type: string, payload: unknown = {}) => ({ v: 1, id: "e", seq: 1, roomId: "r", actor: { kind: "system", id: "s" },
    type, origin: "broker", ts: "2026-10-08T10:00:00Z", redactions: [], payload });
  it.each([
    ["state_changed", { kind: "task" }, true],
    ["state_changed", { kind: "room_phase" }, true],
    ["state_changed", { kind: "limit" }, true],
    ["message", { kind: "progress" }, true],
    ["message", { kind: "review_verdict" }, true],
    ["approval_requested", {}, true],
    ["approval_decided", {}, true],
    ["state_changed", { kind: "harness_status" }, false],
    ["state_changed", { kind: "run_phase" }, false],
    ["message", { kind: "chat" }, false],
    ["tool_call", {}, false],
    ["tool_result", {}, false],
    ["turn", {}, false],
    ["message", null, false],
  ] as const)("%s %j: %s", (type, payload, want) => {
    expect(changesSummary(ev(type, payload))).toBe(want);
  });
});

// One fixture for Go and TS: the broker's own golden summaries, read where internal/summary keeps them.
describe("the broker's golden summaries", () => {
  const dir = join(__dirname, "../../internal/summary/testdata");
  const files = readdirSync(dir).filter((f) => f.endsWith(".json"));

  it("are there", () => expect(files.length).toBeGreaterThan(0));
  it.each(files)("%s renders every part it carries", (f) => {
    const { want } = JSON.parse(readFileSync(join(dir, f), "utf8")) as { want: Summary };
    const r = render(want);
    r.append(needs(want));
    for (const n of want.needsYou) if (n.kind !== "approval") expect(r.textContent).toContain(n.what);
    for (const n of want.notes.items) expect(r.textContent).toContain(n.text);
    expect(r.textContent).not.toMatch(/undefined|null|NaN|Invalid Date/);
  });
});

describe("fetchSummary", () => {
  const get = (status: number, body: unknown) => (async () => new Response(JSON.stringify(body), { status })) as unknown as typeof fetch;
  it("returns the summary", async () => {
    expect((await fetchSummary("3kq7x2ma", get(200, base()))).cursor).toBe("seq:9");
  });
  it("explains a 404 the same for missing and unreadable", async () => {
    await expect(fetchSummary("x", get(404, {}))).rejects.toThrow(/no such room, or you cannot read it/i);
  });
  it("explains a 503", async () => {
    await expect(fetchSummary("x", get(503, {}))).rejects.toThrow(/cannot be verified|unreadable/i);
  });
});
