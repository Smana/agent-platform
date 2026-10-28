import { describe, expect, it, vi } from "vitest";
import type { Snapshot } from "../src/conn";
import { RULES } from "../src/approval-help";
import { approvalCard } from "../src/controls";
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
  const row = document.createElement("details"); row.dataset.callId = "c9"; row.dataset.seq = "12";
  const needs = mountNeeds(root, { send: (f) => { sent.push(f); return true; } }, state, () => you, { rowFor: (id) => (id === "c9" ? row : undefined) });
  return { root, needs, sent, row };
}

describe("the needs-you panel", () => {
  it("leads an approval card with its command, keeps the JSON behind a toggle, links the call", () => {
    const state = new RoomState(); state.reset({ driver: "human:a", driverEpoch: 1, approvals: [approval] }, 20);
    const { root, needs } = mount(state); needs.refresh();
    const card = root.querySelector("#approval-01M4A")!;
    expect(card.querySelector(".approval-command")?.textContent).toBe("git push origin agent/ibbay5ud");
    expect(card.querySelector("details.approval-json pre")?.textContent).toContain('"timeout": 600');
    expect(card.querySelector("a.approval-call")?.textContent).toBe("see the call #12");
    expect(root.querySelector("h2")?.textContent).toBe("Needs you");
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
    // The pill counts what the viewer may decide: a read-only card is shown, not counted.
    expect(needs.count()).toBe(0);
    expect(root.hidden).toBe(false);
    expect(root.querySelector("h2")?.textContent).toBe("Waiting for an approver");
  });

  // Fix round 1: no empty heading before the first refresh, nor in a room that never connects.
  it("starts hidden, before any refresh", () => {
    expect(mount().root.hidden).toBe(true);
  });

  // A hold or a merge needs this viewer even when it may not decide the approvals.
  it("heads the panel Needs you for a merge or a hold, whoever decides the approvals", () => {
    const state = new RoomState(); state.reset({ driver: "human:a", driverEpoch: 1, approvals: [approval] }, 20);
    const root = document.createElement("section");
    const needs = mountNeeds(root, { send: () => true }, state, () => ({ ...you, role: "collaborator", approver: false }), { rowFor: () => undefined });
    needs.setSummary(sum([{ kind: "hold", id: "waiting_human_driver", what: "hand the driver back", url: "u" }]));
    expect(root.querySelector("h2")?.textContent).toBe("Needs you");
  });

  // The strip's pill and the panel agree: approver OR owner may decide.
  it("counts an approval for an approver and for an owner, never for a sealed room", () => {
    const cases: [string, Snapshot["you"], boolean, number][] = [
      ["an approver collaborator", { ...you, role: "collaborator", approver: true }, false, 1],
      ["an owner who is no approver", { ...you, role: "owner", approver: false }, false, 1],
      ["a collaborator who is neither", { ...you, role: "collaborator", approver: false }, false, 0],
      ["an owner in a sealed room", you, true, 0],
    ];
    for (const [name, who, sealed, want] of cases) {
      const state = new RoomState(); state.reset({ driver: "human:a", driverEpoch: 1, approvals: [approval], sealed }, 20);
      const needs = mountNeeds(document.createElement("section"), { send: () => true }, state, () => who, { rowFor: () => undefined });
      needs.refresh();
      expect(needs.count(), name).toBe(want);
    }
  });

  it("takes the call link to its row, opened, and hides the link while the row is gone", () => {
    const state = new RoomState(); state.reset({ driver: "human:a", driverEpoch: 1, approvals: [approval] }, 20);
    const { root, needs, row } = mount(state); needs.refresh();
    const scrolled = vi.fn();
    row.scrollIntoView = scrolled;
    const link = root.querySelector<HTMLAnchorElement>("a.approval-call")!;
    expect(link.getAttribute("href")).toBe("#");
    link.click();
    expect(scrolled).toHaveBeenCalled();
    expect(row.open).toBe(true);
    const gone = new RoomState(); gone.reset({ driver: "human:a", driverEpoch: 1, approvals: [{ ...approval, callId: "c-old" }] }, 20);
    const g = mount(gone); g.needs.refresh();
    expect(g.root.querySelector<HTMLAnchorElement>("a.approval-call")!.hidden).toBe(true);
  });

  it("names every control, and ties the decide buttons to the command they decide", () => {
    const state = new RoomState(); state.reset({ driver: "human:a", driverEpoch: 1, approvals: [approval] }, 20);
    const { root, needs } = mount(state); needs.refresh();
    for (const f of root.querySelectorAll("input")) expect(f.getAttribute("aria-label")?.trim()).toBeTruthy();
    for (const b of root.querySelectorAll("button")) {
      expect(b.textContent?.trim()).toBeTruthy();
      expect(root.querySelector(`#${b.getAttribute("aria-describedby")}`)).toBe(root.querySelector(".approval-command"));
    }
    expect(root.querySelector('button[data-act="deny"]')?.classList.contains("deny")).toBe(true);
  });
});

// Moved from controls.test.ts: the cards left the controls for the panel.
describe("approval cards", () => {
  const owner = (principal: string, role: string): Snapshot["you"] => ({ principal, role, approver: false, driver: false, webUI: true });
  const pending = (id: string, seq: number, action: unknown = { command: "gh pr create" }) => ({ approvalId: id, runId: "7f3cq2xz",
    callId: "call_" + seq, class: "forge.pr", action, expiresAt: "2026-10-01T10:30:00Z", seq });
  const withApprovals = (who: Snapshot["you"], ...approvals: ReturnType<typeof pending>[]) => {
    const sent: Record<string, any>[] = [];
    const state = new RoomState("human:a", 4);
    state.reset({ driver: "human:a", driverEpoch: 4, approvals }, 10);
    const root = document.createElement("section");
    const needs = mountNeeds(root, { send: (f) => { sent.push(f); return true; } }, state, () => who, { rowFor: () => undefined });
    needs.refresh();
    const btn = (act: string) => root.querySelector<HTMLButtonElement>(`button[data-act="${act}"]`);
    return { root, state, needs, btn, acts: () => sent.map((f) => f.action) };
  };

  it("shows the raw action as text, never markup (T3)", () => {
    const hostile = { command: '<img src=x onerror="alert(1)">', body: "**not** markdown" };
    const c = withApprovals({ ...owner("human:apr", "collaborator"), approver: true }, pending("ap1", 3, hostile));
    const card = c.root.querySelector("ul.approvals li")!;
    expect(c.root.querySelector("img")).toBeNull();
    expect(card.querySelector("strong")).toBeNull();
    expect(card.querySelector(".approval-command")!.textContent).toBe('<img src=x onerror="alert(1)">');
    expect(card.querySelector(".approval-json pre")!.textContent).toBe(JSON.stringify(hostile, null, 2));
    expect(card.querySelector(".approval-meta")!.textContent).toMatch(/^forge\.pr · run 7f3cq2xz · expires /);
  });

  it("leads with the class when the action has no command", () => {
    const c = withApprovals(owner("human:own", "owner"), pending("ap1", 3, { kind: "FileEditorAction", path: "a.go" }));
    expect(c.root.querySelector(".approval-command")!.textContent).toBe("forge.pr");
  });

  it("lets approvers and owners approve or deny, with a reason", () => {
    const c = withApprovals({ ...owner("human:apr", "collaborator"), approver: true }, pending("ap1", 3), pending("ap2", 5));
    const cards = [...c.root.querySelectorAll("ul.approvals li")];
    expect(cards).toHaveLength(2);
    cards[0].querySelector<HTMLButtonElement>('[data-act="approve"]')!.click();
    cards[1].querySelector<HTMLInputElement>('[name="decisionReason"]')!.value = "  wrong branch ";
    c.needs.refresh(); // a typed reason survives a re-render
    c.root.querySelectorAll("ul.approvals li")[1].querySelector<HTMLButtonElement>('[data-act="deny"]')!.click();
    expect(c.acts()).toEqual([{ kind: "decide", approvalId: "ap1", decision: "approved" },
      { kind: "decide", approvalId: "ap2", decision: "denied", reason: "wrong branch" }]);

    const o = withApprovals(owner("human:own", "owner"), pending("ap1", 3));
    expect(o.btn("approve")).not.toBeNull();
  });

  it("offers no decision to others, nor in a sealed room", () => {
    const c = withApprovals(owner("human:col", "collaborator"), pending("ap1", 3));
    expect(c.root.querySelector("ul.approvals li pre")).not.toBeNull();
    expect(c.btn("approve")).toBeNull();
    expect(c.btn("deny")).toBeNull();
    const s = withApprovals(owner("human:own", "owner"));
    s.state.reset({ driver: "human:a", driverEpoch: 4, approvals: [pending("ap1", 3)], sealed: true }, 10);
    s.needs.refresh();
    expect(s.root.querySelector("ul.approvals li pre")).not.toBeNull();
    expect(s.btn("approve")).toBeNull();
  });

  // Review 5.3 I2: policy Decide is the approver flag, whatever the room role.
  it("gives a watcher who approves the cards and their buttons", () => {
    const c = withApprovals({ ...owner("human:wat", "watcher"), approver: true }, pending("ap1", 3));
    c.btn("approve")!.click();
    expect(c.acts()).toEqual([{ kind: "decide", approvalId: "ap1", decision: "approved" }]);
  });

  // R11: a deadline on another day shows its date, not a bare time.
  it("dates a deadline that is not today", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(2026, 9, 1, 9, 0));
    try {
      const meta = (due: Date) => approvalCard({ class: "forge.pr", callId: "c", action: {}, expiresAt: due.toISOString() })
        .querySelector(".approval-meta")!.textContent;
      const today = new Date(2026, 9, 1, 10, 30);
      const tomorrow = new Date(2026, 9, 2, 10, 30);
      expect(meta(today)).toBe(`forge.pr · expires ${today.toLocaleTimeString()}`);
      expect(meta(tomorrow)).toBe(`forge.pr · expires ${tomorrow.toLocaleString()}`);
    } finally {
      vi.useRealTimers();
    }
  });

  it("drops a card once decided", () => {
    const c = withApprovals(owner("human:own", "owner"), pending("ap1", 3));
    c.state.apply({ v: 1, id: "11", seq: 11, roomId: "r", actor: { kind: "human", id: "human:own" }, type: "approval_decided",
      origin: "client", ts: "2026-09-27T10:00:00Z", redactions: [], payload: { approvalId: "ap1", decision: "approved" } });
    c.needs.refresh();
    expect(c.root.querySelectorAll("ul.approvals li")).toHaveLength(0);
  });

  // R10: a refresh never takes the focus of the reason field the human is in.
  it("keeps the focused reason field across refreshes", () => {
    const c = withApprovals(owner("human:own", "owner"), pending("ap1", 3));
    document.body.append(c.root);
    try {
      const f = c.root.querySelector<HTMLInputElement>('[name="decisionReason"]')!;
      f.focus();
      c.needs.refresh();
      c.state.apply({ v: 1, id: "11", seq: 11, roomId: "r", actor: { kind: "human", id: "human:c" }, type: "message",
        origin: "client", ts: "2026-09-27T10:00:00Z", redactions: [], payload: { kind: "chat", text: "between", delivery: "queued" } });
      c.needs.refresh();
      expect(document.activeElement).toBe(f);
    } finally {
      c.root.remove();
    }
  });
});

describe("the approval card's guidance", () => {
  const mk = (cls: string, who = you, extra: Record<string, unknown> = {}, four?: boolean) => {
    const state = new RoomState(); state.reset({ driver: "human:a", driverEpoch: 1, approvals: [{ ...approval, class: cls, ...extra }] }, 20);
    const root = document.createElement("section");
    const needs = mountNeeds(root, { send: () => true }, state, () => who, { rowFor: () => undefined }, () => four);
    needs.refresh();
    return { root, needs };
  };
  it("says what it wants, why it stopped and what each answer does, per rule code", () => {
    for (const [code, rule] of Object.entries(RULES)) {
      const { root } = mk(code);
      expect(root.querySelector(".approval-intent")?.textContent).toBe(rule.intent);
      expect(root.querySelector(".approval-why")?.textContent).toBe(`Why it stopped: ${rule.why}`);
      const out = [...root.querySelectorAll(".approval-outcomes p")].map((p) => p.textContent);
      expect(out).toEqual([`If you approve: ${rule.approve}`, `If you deny: ${rule.deny}`]);
    }
  });
  it("folds the raw command and action under details, and writes agent text as text", () => {
    const evil = `<img src=x onerror=alert(1)>`;
    const { root } = mk("forge.other", you, { action: { command: evil } });
    const details = root.querySelector("details.approval-details")!;
    expect(details.querySelector("summary")?.textContent).toBe("details");
    expect(details.querySelector(".approval-command")?.textContent).toBe(evil);
    expect(root.querySelector("img")).toBeNull();
    expect(root.querySelector(".approval-intent")?.textContent).not.toContain("img");
  });
  it("shows the deadline, and none when the expiry is missing", () => {
    const soon = new Date(Date.now() + 5 * 60_000 + 5000).toISOString();
    expect(mk("forge.pr", you, { expiresAt: soon }).root.querySelector(".approval-deadline")?.textContent).toMatch(/^Decide by \d\d:\d\d \(in 5 min\); if nobody decides: the request expires/);
    expect(mk("forge.pr", you, { expiresAt: "" }).root.querySelector(".approval-deadline")?.textContent).toBe("No deadline.");
  });
  it("says you can decide, with the buttons", () => {
    const { root } = mk("forge.pr");
    expect(root.querySelector(".approval-status")?.textContent).toBe("You can decide.");
    expect(root.querySelector('button[data-act="approve"]')).not.toBeNull();
  });
  it("says only approvers decide, with no buttons", () => {
    const { root, needs } = mk("forge.pr", { ...you, role: "collaborator", approver: false });
    expect(root.querySelector(".approval-status")?.textContent).toContain("only approvers decide");
    expect(root.querySelector("button")).toBeNull();
    expect(needs.approvalCounts()).toEqual({ total: 1, mine: 0 });
  });
  it("explains four-eyes to the prompter and hides the buttons; others still decide", () => {
    const { root, needs } = mk("forge.pr", you, { prompters: ["human:a"] }, true);
    expect(root.querySelector(".approval-status")?.textContent).toBe("Waiting for another approver: you prompted this turn (four-eyes).");
    expect(root.querySelector("button")).toBeNull();
    expect(needs.count()).toBe(0);
    const other = mk("forge.pr", { ...you, principal: "human:b" }, { prompters: ["human:a"] }, true);
    expect(other.root.querySelector('button[data-act="approve"]')).not.toBeNull();
  });
  it("focuses the first card for the banner's link", () => {
    const { root, needs } = mk("forge.pr");
    document.body.append(root);
    needs.focusFirst();
    expect(document.activeElement).toBe(root.querySelector("li.approval"));
    root.remove();
  });
});
