import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Frame, Snapshot } from "../src/conn";
import { confirmButton, hasControls, mountComposer, mountControls, rejection } from "../src/controls";
import { RoomState } from "../src/room-state";
import { newRoomForm } from "../src/view";

type You = Snapshot["you"];
const shown = (e: Element) => !e.closest("[hidden]");
const you = (principal: string, role: string): You => ({ principal, role, approver: false, driver: false, webUI: true });

function setup(who: You, driver = "human:a", epoch = 4) {
  const sent: Record<string, any>[] = [];
  const notices: string[] = [];
  const conn = { send: (f: Record<string, unknown>) => { sent.push(f); return true; } };
  const state = new RoomState(driver, epoch);
  // The controls' section and the page's dock, as room.ts mounts them.
  const root = document.createElement("section");
  const dock = document.createElement("footer");
  const wrap = document.createElement("div");
  wrap.append(root, dock);
  const ctl = mountControls(root, conn, state, who, (t) => notices.push(t));
  const composer = mountComposer(dock, conn, state, who);
  const controls = {
    ...ctl,
    refresh() { ctl.refresh(); composer.refresh(); },
    onAck(f: Frame) { ctl.onAck(f); composer.onAck(f); },
  };
  controls.refresh();
  // A button counts when the human can see it: sections toggle hidden (R10).
  const btn = (act: string) => [...wrap.querySelectorAll<HTMLButtonElement>(`button[data-act="${act}"]`)].find(shown) ?? null;
  const field = <T extends HTMLElement>(name: string) => wrap.querySelector<T>(`[name="${name}"]`)!;
  const deliveryNotice = () => dock.querySelector("#delivery-notice")!.textContent;
  return { sent, notices, state, root, dock, wrap, controls, composer, btn, field, deliveryNotice, acts: () => sent.map((f) => f.action) };
}
const mountOwner = () => setup(you("human:a", "owner"));

const queued = (seq: number, author: string, text: string) => ({ v: 1, id: String(seq), seq, roomId: "3kq7x2ma",
  actor: { kind: "human", id: author }, type: "message", origin: "client", ts: "2026-09-27T10:00:00Z", redactions: [],
  payload: { kind: "chat", text, delivery: "queued" } });
const ev = (seq: number, type: string, payload: unknown) => ({ ...queued(seq, "human:a", ""), type, payload });

describe("mountControls", () => {
  it("lets an owner invite a member, with a role and the approver flag", () => {
    const c = setup(you("human:o", "owner"));
    c.btn("invite")!.click(); // nobody named: nothing sent
    expect(c.sent).toHaveLength(0);
    c.field<HTMLInputElement>("invitee").value = "human:new@ogenki.io";
    c.field<HTMLSelectElement>("memberRole").value = "collaborator";
    c.field<HTMLInputElement>("inviteApprover").checked = true;
    c.btn("invite")!.click();
    expect(c.acts()).toEqual([{ kind: "invite", principal: "human:new@ogenki.io", memberRole: "collaborator", approver: true }]);
    expect(c.field<HTMLInputElement>("invitee").value).toBe("human:new@ogenki.io"); // cleared by its ack, not before
    c.controls.onAck({ type: "ack", clientSeq: c.sent[0].clientSeq, seq: 5 });
    expect(c.field<HTMLInputElement>("invitee").value).toBe("");
  });

  it("closes the room after a confirmation, and only for an owner", () => {
    const c = setup(you("human:o", "owner"));
    c.btn("close")!.click();
    expect(c.sent).toHaveLength(0); // the first click asks
    c.btn("close")!.click();
    expect(c.acts()).toEqual([{ kind: "close" }]);
  });

  it("offers neither invite nor close to anyone but an owner, nor in a sealed room", () => {
    for (const role of ["collaborator", "watcher"]) {
      const c = setup(you("human:c", role));
      expect(c.btn("invite")).toBeNull();
      expect(c.btn("close")).toBeNull();
    }
    const o = setup(you("human:o", "owner"));
    o.state.sealed = true;
    o.controls.refresh();
    expect(o.btn("invite")).toBeNull();
    expect(o.btn("close")).toBeNull();
  });

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
    const steer = c.wrap.querySelector<HTMLOptionElement>('option[value="steering"]')!;
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
    expect(c.deliveryNotice()).toBe(choose); // beside the select, not the page's act notice
    expect(c.notices).toEqual([]);
    c.field<HTMLTextAreaElement>("text").value = "steer this";
    c.btn("message")!.click();
    expect(c.sent).toHaveLength(0);
    expect(c.deliveryNotice()).toBe(choose);
    delivery.value = "none";
    delivery.dispatchEvent(new Event("change")); // a choice answers the notice
    expect(c.deliveryNotice()).toBe("");
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
    d.btn("promote_queued")!.click(); // steer now asks first: it prompts the running agent
    expect(d.sent).toHaveLength(0);
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
    d.btn("interrupt")!.click(); // asks first
    expect(d.sent).toHaveLength(1);
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
    c.btn("driver_take")!.click(); // confirmed, but no reason given: nothing sent
    expect(c.sent).toHaveLength(0);
    expect(c.notices.at(-1)).toBe("Say why you take the driver token.");
    c.field<HTMLInputElement>("takeReason").value = "the driver is away";
    c.btn("driver_take")!.click();
    expect(c.sent).toHaveLength(0); // the first click asks
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
    expect(o.root.querySelector<HTMLElement>(".group-run")!.hidden).toBe(true);
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

describe("the control groups", () => {
  afterEach(() => vi.useRealTimers());

  it("groups the controls under Run, Driver and Room, with Room behind a toggle", () => {
    const { root } = mountOwner();
    expect([...root.querySelectorAll(":scope > section > h2, :scope > details > summary")].map((h) => h.textContent)).toEqual(["Run", "Driver", "Room"]);
    expect(root.querySelector<HTMLDetailsElement>("details.group-room")?.open).toBe(false);
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
    for (const a of ["driver_take", "promote_queued"]) {
      const b = root.querySelector<HTMLElement>(`[data-act="${a}"]`);
      expect(b === null || b.classList.contains("destructive"), a).toBe(true);
    }
  });

  it("puts the confirm back after 10 s, and the cancel returns the focus to the button", () => {
    vi.useFakeTimers();
    let done = 0;
    const span = confirmButton("interrupt", "interrupt", () => done++);
    document.body.append(span);
    const b = span.querySelector<HTMLButtonElement>('button[data-act="interrupt"]')!;
    b.click();
    expect(span.textContent).toBe("confirm: interruptcancel");
    expect(span.getAttribute("aria-live")).toBe("polite"); // the confirm is announced
    vi.advanceTimersByTime(10_000);
    expect(b.textContent).toBe("interrupt");
    expect(span.querySelector('[data-act="interrupt-cancel"]')).toBeNull();
    b.click();
    span.querySelector<HTMLButtonElement>('[data-act="interrupt-cancel"]')!.focus();
    span.querySelector<HTMLButtonElement>('[data-act="interrupt-cancel"]')!.click();
    expect(document.activeElement).toBe(b);
    expect(done).toBe(0);
    b.click(); b.click();
    expect(done).toBe(1);
    vi.advanceTimersByTime(10_000); // the timer of a confirmed click is gone: nothing resets twice
    expect(b.textContent).toBe("interrupt");
    span.remove();
  });

  it("shows the PR URL only for a reviewer and the egress field under advanced", () => {
    const { root } = mountOwner();
    const role = root.querySelector('select[name="role"]') as HTMLSelectElement;
    expect((root.querySelector('input[name="prUrl"]') as HTMLInputElement).hidden).toBe(true);
    role.value = "reviewer"; role.dispatchEvent(new Event("change"));
    expect((root.querySelector('input[name="prUrl"]') as HTMLInputElement).hidden).toBe(false);
    expect(root.querySelector('input[name="egress"]')?.closest("details")?.querySelector("summary")?.textContent).toBe("advanced");
  });

  it("labels its fields with visible text, not placeholders", () => {
    const { wrap } = mountOwner();
    for (const name of ["role", "prUrl", "giveTo", "takeReason", "invitee", "memberRole", "delivery"]) {
      const f = wrap.querySelector<HTMLElement>(`[name="${name}"]`)!;
      expect(f.closest("label")?.textContent?.trim(), name).toBeTruthy();
      expect(f.getAttribute("placeholder"), name).toBeNull();
    }
    expect(wrap.querySelector('select[name="role"]')!.closest("label")!.firstChild!.textContent).toBe("Start a run as");
    expect(wrap.querySelector('select[name="delivery"]')!.closest("label")!.firstChild!.textContent).toBe("Deliver to");
  });

  it("hides Run and Driver once the room is sealed", () => {
    const { root, state, controls } = mountOwner();
    state.apply(ev(9, "state_changed", { kind: "room_phase", phase: "Closed", reason: "owner" }));
    controls.refresh();
    expect((root.querySelector(".group-run") as HTMLElement).hidden).toBe(true);
    expect((root.querySelector(".group-driver") as HTMLElement).hidden).toBe(true);
  });

  it("names the driver without its epoch", () => {
    const { root } = mountOwner();
    expect(root.querySelector(".driver-holder")?.textContent).toBe("driver: human:a (you)");
  });

  it("lists the members as text and hides the Room group from anyone but an owner", () => {
    const hostile = 'human:<img src=x onerror="alert(1)">';
    const o = mountOwner();
    o.state.apply(ev(2, "participant", { principal: hostile, change: "joined", role: "collaborator", approver: true }));
    o.controls.refresh();
    expect(o.root.querySelector("ul.members")?.textContent).toBe(`${hostile} · collaborator · approver`);
    expect(o.root.querySelector("img")).toBeNull();
    const c = setup(you("human:c", "collaborator"));
    expect(c.root.querySelector<HTMLElement>(".group-room")!.hidden).toBe(true);
  });

  it("says how to stop the task, with the issue's link, once the summary names one", () => {
    const o = mountOwner();
    expect(o.root.querySelector(".stop")).toBeNull();
    o.controls.setSummary({ status: { issue: { number: 74, url: "https://github.com/Smana/x/issues/74" } } } as never);
    const stop = o.root.querySelector(".group-room .stop")!;
    expect(stop.textContent).toBe("Stop the task: label the issue #74 factory/stop");
    expect(stop.querySelector("a")?.getAttribute("href")).toBe("https://github.com/Smana/x/issues/74");
    expect(stop.querySelector("code")?.textContent).toBe("factory/stop");
    o.controls.setSummary({ status: { issue: null } } as never);
    expect(o.root.querySelector(".stop")).toBeNull();
  });
});

describe("mountComposer", () => {
  it("disables Send while not live, and hides the composer in a sealed room", () => {
    const c = mountOwner();
    const send = c.dock.querySelector<HTMLButtonElement>('button[data-act="message"]')!;
    c.composer.setLive(false);
    expect(send.disabled).toBe(true);
    expect(c.field<HTMLTextAreaElement>("text").hidden).toBe(false); // nothing hides: the draft stays in view
    c.composer.setLive(true);
    expect(send.disabled).toBe(false);
    c.state.apply(ev(9, "state_changed", { kind: "room_phase", phase: "Closed", reason: "owner" }));
    c.controls.refresh();
    for (const f of [c.field("text"), c.field("delivery"), send]) expect(shown(f)).toBe(false);
  });

  it("ties the notice to the select", () => {
    const c = mountOwner();
    expect(c.field("delivery").getAttribute("aria-describedby")).toBe("delivery-notice");
  });
});

describe("refresh", () => {
  // R10: every event refreshes the controls. The field the human is in keeps its
  // focus: refresh used to re-mount every section, and the driver's fields with it.
  it("keeps the focused field across refreshes", () => {
    const approvals = [{ approvalId: "ap1", runId: "7f3cq2xz", callId: "c3", class: "forge.pr", action: {},
      expiresAt: "2026-10-01T10:30:00Z", seq: 3 }];
    // The approval's reason field moved to the needs-you panel (needs.test.ts).
    for (const [who, name] of [["human:a", "text"], ["human:a", "giveTo"], ["human:o", "takeReason"]]) {
      const c = setup(you(who, "owner"));
      c.state.reset({ driver: "human:a", driverEpoch: 4, approvals }, 10);
      c.controls.refresh();
      document.body.append(c.wrap);
      const f = c.field<HTMLInputElement>(name);
      f.focus();
      expect(document.activeElement, name).toBe(f);
      c.controls.refresh();
      c.state.apply(queued(11, "human:c", "an event between")); // re-renders the queue
      c.controls.refresh();
      expect(document.activeElement, name).toBe(f);
      c.wrap.remove();
    }
  });
});

describe("accessible names", () => {
  // R10: a placeholder is not a name; every field says what it is for.
  it("names every form control", () => {
    const approvals = [{ approvalId: "ap1", runId: "7f3cq2xz", callId: "c3", class: "forge.pr", action: {},
      expiresAt: "2026-10-01T10:30:00Z", seq: 3 }];
    const roots: HTMLElement[] = [newRoomForm()];
    for (const who of ["human:a", "human:o"]) { // the driver's fields, then an owner's who does not drive
      const c = setup(you(who, "owner"));
      c.state.reset({ driver: "human:a", driverEpoch: 4, approvals }, 10);
      c.controls.refresh();
      roots.push(c.wrap);
    }
    const fields = roots.flatMap((r) => [...r.querySelectorAll<HTMLElement>("input, select, textarea")]);
    expect(fields.length).toBeGreaterThanOrEqual(10);
    for (const f of fields) expect(f.getAttribute("aria-label")?.trim(), f.getAttribute("name") ?? f.tagName).toBeTruthy();
  });
});

// The approval cards moved to the needs-you panel (needs.test.ts); the controls keep none.
describe("approvals", () => {
  const pending = { approvalId: "ap1", runId: "7f3cq2xz", callId: "call_3", class: "forge.pr", action: {},
    expiresAt: "2026-10-01T10:30:00Z", seq: 3 };

  it("leave the controls: no card, no decide button", () => {
    const c = setup(you("human:own", "owner"));
    c.state.reset({ driver: "human:a", driverEpoch: 4, approvals: [pending] }, 10);
    c.controls.refresh();
    expect(c.root.querySelector(".approval")).toBeNull();
    expect(c.btn("approve")).toBeNull();
  });

  // Review 5.3 I2: policy Decide is the approver flag, whatever the room role; a watcher
  // who approves decides in the panel and gets nothing in the controls.
  it("give a watcher who approves nothing else", () => {
    const c = setup({ ...you("human:wat", "watcher"), approver: true });
    c.state.reset({ driver: "human:a", driverEpoch: 4, approvals: [pending] }, 10);
    c.controls.refresh();
    expect([...c.root.children].filter(shown).map((e) => e.className)).toEqual([]);
    expect(shown(c.field("text"))).toBe(false);
    expect(c.btn("driver_request")).toBeNull();
    expect(hasControls({ ...you("human:wat", "watcher"), approver: true })).toBe(true);
    expect(hasControls(you("human:wat", "watcher"))).toBe(false); // a plain watcher gets nothing
    expect(hasControls(you("human:col", "collaborator"))).toBe(true);
    // Promoted to collaborator by an invite: the state frame updates you in place.
    const who = { ...you("human:wat", "watcher"), approver: true };
    const p = setup(who);
    who.role = "collaborator";
    p.controls.refresh();
    expect(shown(p.field("text"))).toBe(true);
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
