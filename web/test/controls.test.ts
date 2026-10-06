import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it, vi } from "vitest";
import type { Snapshot } from "../src/conn";
import { hasControls, mountControls, rejection } from "../src/controls";
import { RoomState } from "../src/room-state";

type You = Snapshot["you"];
const you = (principal: string, role: string): You => ({ principal, role, approver: false, driver: false, webUI: true });

function setup(who: You, driver = "human:a", epoch = 4) {
  const sent: Record<string, any>[] = [];
  const notices: string[] = [];
  const conn = { send: (f: Record<string, unknown>) => { sent.push(f); return true; } };
  const state = new RoomState(driver, epoch);
  const root = document.createElement("section");
  const controls = mountControls(root, conn, state, who, (t) => notices.push(t));
  controls.refresh();
  const btn = (act: string) => root.querySelector<HTMLButtonElement>(`button[data-act="${act}"]`);
  const field = <T extends HTMLElement>(name: string) => root.querySelector<T>(`[name="${name}"]`)!;
  return { sent, notices, state, root, controls, btn, field, acts: () => sent.map((f) => f.action) };
}

const queued = (seq: number, author: string, text: string) => ({ v: 1, id: String(seq), seq, roomId: "3kq7x2ma",
  actor: { kind: "human", id: author }, type: "message", origin: "client", ts: "2026-09-27T10:00:00Z", redactions: [],
  payload: { kind: "chat", text, delivery: "queued" } });

describe("mountControls", () => {
  it("sends the composer's text with its delivery and the epoch it was decided on", () => {
    const c = setup(you("human:a", "owner"));
    c.field<HTMLTextAreaElement>("text").value = "look at the flaky test";
    c.field<HTMLSelectElement>("delivery").value = "steering";
    c.btn("message")!.click();
    expect(c.sent[0]).toMatchObject({ type: "act", driverEpoch: 4,
      action: { kind: "message", text: "look at the flaky test", delivery: "steering" } });
    const seq = c.sent[0].clientSeq;
    expect(seq).toBeGreaterThan(0);
    const text = c.field<HTMLTextAreaElement>("text");
    // The text waits for its own ack (review 4.5 M1): a rejection keeps it.
    expect(text.value).toBe("look at the flaky test");
    c.controls.onAck({ type: "ack", clientSeq: seq, rejected: "stale_epoch" });
    expect(text.value).toBe("look at the flaky test");
    c.controls.onAck({ type: "ack", clientSeq: seq + 100, seq: 9 }); // another act's
    expect(text.value).toBe("look at the flaky test");
    c.controls.onAck({ type: "ack", clientSeq: seq, seq: 9 });
    expect(text.value).toBe("");
    c.btn("message")!.click(); // blank: nothing sent
    expect(c.sent).toHaveLength(1);
    // Text edited while its ack was in flight is the human's next message: kept.
    text.value = "first";
    c.btn("message")!.click();
    text.value = "second, typed meanwhile";
    c.controls.onAck({ type: "ack", clientSeq: c.sent[1].clientSeq, seq: 10 });
    expect(text.value).toBe("second, typed meanwhile");
  });

  it("offers steering to the driver only", () => {
    const c = setup(you("human:b", "collaborator"));
    const steer = c.root.querySelector<HTMLOptionElement>('option[value="steering"]')!;
    expect(steer.disabled).toBe(true);
    c.state.apply({ ...queued(1, "human:a", ""), type: "driver", payload: { from: "human:a", to: "human:b", epoch: 5, reason: "given" } });
    c.controls.refresh();
    expect(steer.disabled).toBe(false);
    // Losing the token picks nothing for the human: queued text enters an LLM prompt,
    // so neither the room nor the queue is a safe guess (R09, reversing review 4.5 I3).
    const delivery = c.field<HTMLSelectElement>("delivery");
    delivery.value = "steering";
    c.state.apply({ ...queued(2, "human:a", ""), type: "driver", payload: { from: "human:b", to: "human:c", epoch: 6, reason: "given" } });
    c.controls.refresh();
    expect(steer.disabled).toBe(true);
    expect(delivery.value).toBe("");
    const choose = "You no longer hold the driver token: choose where this message goes.";
    expect(c.notices).toEqual([choose]);
    c.field<HTMLTextAreaElement>("text").value = "steer this";
    c.btn("message")!.click();
    expect(c.sent).toHaveLength(0);
    expect(c.notices).toEqual([choose, choose]);
    delivery.value = "none";
    c.btn("message")!.click();
    expect(c.acts()).toEqual([{ kind: "message", text: "steer this", delivery: "none" }]);
  });

  // R09: each option says what it does; the room is the default, since it prompts no agent.
  it("names where each delivery goes, the room by default", () => {
    const delivery = setup(you("human:a", "owner")).field<HTMLSelectElement>("delivery");
    expect(delivery.value).toBe("none");
    expect([...delivery.options].map((o) => [o.value, o.textContent])).toEqual([
      ["none", "post to the room (no agent is prompted)"],
      ["queued", "queue for the next run's brief"],
      ["steering", "steer the running agent now"],
      ["", "choose where this goes"],
    ]);
    expect(delivery.options[3].disabled).toBe(true);
  });

  it("shows the queue as text, with remove for its author and steer for the driver", () => {
    const hostile = '<img src=x onerror="alert(1)"> **bold**';
    const c = setup(you("human:b", "collaborator"));
    c.state.apply(queued(7, "human:b", hostile));
    c.state.apply(queued(8, "human:c", "someone else's"));
    c.controls.refresh();
    const items = [...c.root.querySelectorAll("ul.queue li")];
    expect(items).toHaveLength(2);
    expect(c.root.querySelector("img")).toBeNull();
    expect(items[0].textContent).toContain(hostile);
    expect(items[0].querySelector('[data-act="remove_queued"]')).not.toBeNull();
    expect(items[1].querySelector('[data-act="remove_queued"]')).toBeNull();
    expect(c.root.querySelector('[data-act="promote_queued"]')).toBeNull();
    items[0].querySelector<HTMLButtonElement>('[data-act="remove_queued"]')!.click();
    expect(c.acts()).toEqual([{ kind: "remove_queued", ref: 7 }]);

    const d = setup(you("human:a", "owner"));
    d.state.apply(queued(8, "human:c", "promote me"));
    d.controls.refresh();
    d.btn("promote_queued")!.click();
    d.btn("remove_queued")!.click(); // the driver removes anyone's
    expect(d.acts()).toEqual([{ kind: "promote_queued", ref: 8 }, { kind: "remove_queued", ref: 8 }]);
  });

  // A sealed room answers every action sealed: its queue shows, with no actions (review 4.5 M3).
  it("offers no queue action in a sealed room", () => {
    const c = setup(you("human:a", "owner"));
    c.state.reset({ driver: "human:a", driverEpoch: 4, queue: [{ ref: 3, author: "human:a", text: "late" }], sealed: true }, 10);
    c.controls.refresh();
    expect(c.root.querySelector("ul.queue li")?.textContent).toContain("late");
    expect(c.btn("remove_queued")).toBeNull();
    expect(c.btn("promote_queued")).toBeNull();
  });

  it("gives the driver give and interrupt, and others a request", () => {
    const d = setup(you("human:a", "collaborator"));
    expect(d.btn("driver_request")).toBeNull();
    expect(d.btn("driver_take")).toBeNull();
    d.btn("driver_give")!.click(); // no target: nothing sent
    d.field<HTMLInputElement>("giveTo").value = " system:factory ";
    d.btn("driver_give")!.click();
    d.btn("interrupt")!.click();
    expect(d.acts()).toEqual([{ kind: "driver_give", to: "system:factory" }, { kind: "interrupt" }]);

    const o = setup(you("human:b", "collaborator"));
    expect(o.btn("driver_give")).toBeNull();
    expect(o.btn("interrupt")).toBeNull();
    expect(o.btn("driver_take")).toBeNull();
    o.btn("driver_request")!.click();
    expect(o.acts()).toEqual([{ kind: "driver_request" }]);
  });

  it("lets an owner take the token, with a reason", () => {
    const c = setup(you("human:o", "owner"));
    expect(c.btn("driver_request")).not.toBeNull(); // asking first is still open to an owner
    c.btn("driver_take")!.click();
    expect(c.sent).toHaveLength(0);
    c.field<HTMLInputElement>("takeReason").value = "the driver is away";
    c.btn("driver_take")!.click();
    expect(c.acts()).toEqual([{ kind: "driver_take", reason: "the driver is away" }]);
  });

  it("hands to a role, with a PR for a reviewer only and the egress profiles listed", () => {
    const c = setup(you("human:a", "collaborator"));
    const form = c.root.querySelector<HTMLFormElement>("form.hand")!;
    c.field<HTMLSelectElement>("role").value = "implementer";
    c.field<HTMLInputElement>("prUrl").value = "https://github.com/Smana/cloud-native-ref/pull/9";
    c.field<HTMLInputElement>("egress").value = "pypi, npm,,";
    form.dispatchEvent(new Event("submit", { cancelable: true }));
    c.field<HTMLSelectElement>("role").value = "reviewer";
    c.field<HTMLInputElement>("egress").value = "";
    form.dispatchEvent(new Event("submit", { cancelable: true }));
    expect(c.acts()).toEqual([
      { kind: "start_run", role: "implementer", egressProfiles: ["pypi", "npm"] },
      { kind: "start_run", role: "reviewer", prUrl: "https://github.com/Smana/cloud-native-ref/pull/9" },
    ]);
    // A collaborator who does not drive cannot start a run (policy StartRun).
    const o = setup(you("human:b", "collaborator"));
    expect(o.root.querySelector<HTMLFormElement>("form.hand")!.hidden).toBe(true);
  });

  it("shows the claim as a command to copy, and copies only the command", async () => {
    const writeText = vi.fn(() => Promise.resolve());
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    const c = setup(you("human:a", "owner"));
    const box = c.root.querySelector<HTMLElement>(".manifest")!;
    expect(box.hidden).toBe(true); // nothing to show before a claim (review 4.5 I3)
    const claim = { apiVersion: "cloud.ogenki.io/v1alpha1", kind: "AgentRun", metadata: { name: "a2b3c4d5" },
      spec: { task: { text: "line one\nEOF\n<script>x</script>" } } };
    c.controls.onAck({ type: "ack", clientSeq: 1, seq: 12, result: claim });
    expect(box.hidden).toBe(false);
    const pre = c.root.querySelector(".manifest pre")!;
    expect(pre.textContent).toMatch(/^# Before SP3 the owner creates the run \(C3\):\nkubectl create -f - <<'EOF'\n\{/);
    expect(pre.textContent).toMatch(/\n\}\nEOF$/);
    // A line that is exactly EOF would end the heredoc early: JSON escapes the newline.
    expect(pre.textContent!.split("\n").filter((l) => l === "EOF")).toHaveLength(1);
    expect(c.root.querySelector("script")).toBeNull();
    c.root.querySelector<HTMLButtonElement>(".manifest button")!.click();
    expect(writeText).toHaveBeenCalledWith(pre.textContent);
    c.controls.showResult(undefined); // a factory run returns no claim
    expect(pre.textContent).toMatch(/kubectl create/);
    expect(box.hidden).toBe(false);
  });
});

describe("approval cards", () => {
  const pending = (id: string, seq: number, action: unknown = { command: "gh pr create" }) => ({ approvalId: id, runId: "7f3cq2xz",
    callId: "call_" + seq, class: "forge.pr", action, expiresAt: "2026-10-01T10:30:00Z", seq });
  const withApprovals = (who: You, ...approvals: ReturnType<typeof pending>[]) => {
    const c = setup(who);
    c.state.reset({ driver: "human:a", driverEpoch: 4, approvals }, 10);
    c.controls.refresh();
    return c;
  };

  it("shows the raw action as text, never markup (T3)", () => {
    const hostile = { command: '<img src=x onerror="alert(1)">', body: "**not** markdown" };
    const c = withApprovals({ ...you("human:apr", "collaborator"), approver: true }, pending("ap1", 3, hostile));
    const card = c.root.querySelector("ul.approvals li")!;
    expect(c.root.querySelector("img")).toBeNull();
    expect(card.querySelector("strong")).toBeNull();
    expect(card.querySelector("pre")!.textContent).toBe(JSON.stringify(hostile, null, 2));
    expect(card.querySelector(".approval-head")!.textContent).toMatch(/^approval: forge\.pr · run 7f3cq2xz · call call_3 · expires /);
  });

  it("lets approvers and owners approve or deny, with a reason", () => {
    const c = withApprovals({ ...you("human:apr", "collaborator"), approver: true }, pending("ap1", 3), pending("ap2", 5));
    const cards = [...c.root.querySelectorAll("ul.approvals li")];
    expect(cards).toHaveLength(2);
    cards[0].querySelector<HTMLButtonElement>('[data-act="approve"]')!.click();
    cards[1].querySelector<HTMLInputElement>('[name="decisionReason"]')!.value = "  wrong branch ";
    c.controls.refresh(); // a typed reason survives a re-render
    c.root.querySelectorAll("ul.approvals li")[1].querySelector<HTMLButtonElement>('[data-act="deny"]')!.click();
    expect(c.acts()).toEqual([{ kind: "decide", approvalId: "ap1", decision: "approved" },
      { kind: "decide", approvalId: "ap2", decision: "denied", reason: "wrong branch" }]);

    const o = withApprovals(you("human:own", "owner"), pending("ap1", 3));
    expect(o.btn("approve")).not.toBeNull();
  });

  it("offers no decision to others, nor in a sealed room", () => {
    const c = withApprovals(you("human:col", "collaborator"), pending("ap1", 3));
    expect(c.root.querySelector("ul.approvals li pre")).not.toBeNull();
    expect(c.btn("approve")).toBeNull();
    expect(c.btn("deny")).toBeNull();
    const s = setup(you("human:own", "owner"));
    s.state.reset({ driver: "human:a", driverEpoch: 4, approvals: [pending("ap1", 3)], sealed: true }, 10);
    s.controls.refresh();
    expect(s.root.querySelector("ul.approvals li pre")).not.toBeNull();
    expect(s.btn("approve")).toBeNull();
  });

  // Review 5.3 I2: policy Decide is the approver flag, whatever the room role.
  it("gives a watcher who approves the cards and their buttons, and nothing else", () => {
    const c = withApprovals({ ...you("human:wat", "watcher"), approver: true }, pending("ap1", 3));
    expect([...c.root.children].map((e) => e.className)).toEqual(["approvals"]);
    expect(c.root.querySelector("textarea")).toBeNull();
    expect(c.btn("driver_request")).toBeNull();
    c.btn("approve")!.click();
    expect(c.acts()).toEqual([{ kind: "decide", approvalId: "ap1", decision: "approved" }]);
    expect(hasControls({ ...you("human:wat", "watcher"), approver: true })).toBe(true);
    expect(hasControls(you("human:wat", "watcher"))).toBe(false); // a plain watcher gets nothing
    expect(hasControls(you("human:col", "collaborator"))).toBe(true);
    // Promoted to collaborator by an invite: the state frame updates you in place.
    const who = { ...you("human:wat", "watcher"), approver: true };
    const p = withApprovals(who, pending("ap1", 3));
    who.role = "collaborator";
    p.controls.refresh();
    expect(p.root.querySelector("textarea")).not.toBeNull();
  });

  it("drops a card once decided", () => {
    const c = withApprovals(you("human:own", "owner"), pending("ap1", 3));
    c.state.apply({ ...queued(11, "human:own", ""), type: "approval_decided", payload: { approvalId: "ap1", decision: "approved" } });
    c.controls.refresh();
    expect(c.root.querySelectorAll("ul.approvals li")).toHaveLength(0);
  });

  it("explains a refused decision", () => {
    expect(rejection("already_decided")).toBe("Another approver decided first.");
    expect(rejection("four_eyes")).toBe("This room needs an approver who did not prompt the turn.");
  });
});

describe("rejection", () => {
  // Every reason docs/api.md lists for an ack gets its own copy, not the fallback.
  it("has human copy for every rejection the actor can return", () => {
    const api = readFileSync(join(__dirname, "../../docs/api.md"), "utf8");
    const head = api.indexOf("| `rejected` | Meaning |");
    const table = api.slice(api.indexOf("\n", head) + 1);
    const reasons = [...table.slice(0, table.indexOf("\n\n")).matchAll(/^\| `([a-z_]+)` \|/gm)].map((m) => m[1]);
    expect(reasons.length).toBeGreaterThanOrEqual(15);
    for (const r of reasons) expect(rejection(r), r).not.toBe(`Refused: ${r}`);
    expect(rejection("something_new")).toBe("Refused: something_new");
  });
});

describe("the UI's sources", () => {
  // T10: room text reaches the page as text nodes only, and no URL or handler
  // attribute is set from a string (review 4.5 M5).
  const sinks = /innerHTML|outerHTML|insertAdjacentHTML|document\.write|srcdoc|DOMParser|createContextualFragment|setAttribute\(\s*["'](?:href|src|on)/;
  it("never build HTML from a string", () => {
    const dir = join(__dirname, "../src");
    for (const f of readdirSync(dir).filter((n) => n.endsWith(".ts"))) {
      expect(readFileSync(join(dir, f), "utf8"), f).not.toMatch(sinks);
    }
  });
  it("catch each sink the scan names", () => {
    for (const s of ["e.innerHTML = x", "f.srcdoc = x", "new DOMParser()", "r.createContextualFragment(x)",
      "a.setAttribute('href', x)", 'i.setAttribute( "src", x)', 'b.setAttribute("onclick", x)']) expect(s).toMatch(sinks);
    expect('a.setAttribute("title", x)').not.toMatch(sinks);
  });
});
