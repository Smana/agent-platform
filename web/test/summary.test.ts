import { describe, expect, it } from "vitest";
import { fetchSummary, renderSummary, type Summary } from "../src/summary";

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
  actions: [{ kind: "steer", what: "Steer the running agent" }, { kind: "message", what: "Write to the room", cli: "roomctl say 3kq7x2ma hi" }],
  notes: { untrusted: true, items: [{ at: "2026-10-08T10:01:00Z", run: "run-1", text: "reading the repo" }] },
  cursor: "seq:9",
});

const render = (s: Summary) => {
  const root = document.createElement("div");
  renderSummary(root, s);
  return root;
};

describe("renderSummary", () => {
  it("orders the blocks: status, needs you, actions, notes", () => {
    const r = render(base());
    expect([...r.querySelectorAll("section")].map((s) => s.dataset.block)).toEqual(["status", "needs-you", "actions", "notes"]);
    expect(r.querySelector('[data-block="status"]')?.textContent).toContain("Running");
    expect(r.querySelector('[data-block="status"]')?.textContent).toContain("1200 / 50000");
  });

  it("renders a sealed room: Sealed phase and the empty actions state", () => {
    const s = base();
    s.status.phase = "Sealed";
    s.needsYou = [];
    s.actions = [];
    const r = render(s);
    expect(r.querySelector('[data-block="status"]')?.textContent).toContain("Phase: Sealed");
    expect(r.querySelector('[data-block="actions"]')?.textContent).toContain("Nothing here beyond reading.");
    expect(r.querySelector('[data-block="needs-you"]')).toBeNull();
  });

  it("omits Needs you when nothing needs you", () => {
    const s = base();
    s.needsYou = [];
    expect(render(s).querySelector('[data-block="needs-you"]')).toBeNull();
  });

  it("renders agent text as text, never as markup", () => {
    const s = base();
    const evil = "<img src=x onerror=alert(1)>";
    s.notes.items[0].text = evil;
    s.status.lastVerdict!.verdict = evil;
    s.needsYou[0].what = evil;
    s.actions[0].what = evil;
    s.actions[1].cli = evil;
    s.status.pr!.author = evil;
    const r = render(s);
    expect(r.querySelector("img")).toBeNull();
    expect(r.querySelector('[data-block="notes"]')?.textContent).toContain(evil);
  });

  it("labels notes as the agents' claims", () => {
    expect(render(base()).querySelector('[data-block="notes"]')?.textContent).toMatch(/agents' claims/i);
  });

  it("links only https URLs", () => {
    const s = base();
    s.status.pr!.url = "javascript:alert(1)";
    s.status.issue!.url = "http://insecure.example/3";
    const r = render(s);
    const hrefs = [...r.querySelectorAll("a")].map((a) => a.getAttribute("href"));
    expect(hrefs.some((h) => h?.startsWith("javascript:") || h?.startsWith("http:"))).toBe(false);
    expect(r.querySelector('[data-block="status"]')?.textContent).toContain("javascript:alert(1)");
  });

  it("shows a watcher no action buttons", () => {
    const s = base();
    s.actions = [];
    s.needsYou = [];
    expect(render(s).querySelectorAll("button")).toHaveLength(0);
  });

  it("links an approval to the existing control and shows no command", () => {
    const r = render(base());
    const a = r.querySelector<HTMLAnchorElement>('[data-block="needs-you"] a')!;
    expect(a.getAttribute("href")).toBe("#approval-ap1");
    expect(r.querySelector('[data-block="needs-you"] code')).toBeNull();
    expect(r.querySelector('[data-block="needs-you"]')?.textContent).toContain("run kubectl apply");
  });

  it("focuses the existing approval control on click", () => {
    const doc = document.createElement("div");
    document.body.append(doc);
    const card = document.createElement("div");
    card.id = "approval-ap1";
    const approve = document.createElement("button");
    card.append(approve);
    doc.append(card);
    const root = render(base());
    doc.append(root);
    card.scrollIntoView = () => {};
    root.querySelector<HTMLAnchorElement>('[data-block="needs-you"] a')!.click();
    expect(document.activeElement).toBe(approve);
    doc.remove();
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
