// SPDX-License-Identifier: Apache-2.0

import type { Frame, Snapshot } from "./conn";
import type { RoomState } from "./room-state";

// Human copy for an ack's rejected reason (docs/api.md, Actions).
const rejections: Record<string, string> = {
  not_permitted: "Not allowed: your role in this room, or this client, does not permit it; or the factory refused you a run.",
  stale_epoch: "Someone else holds the driver token now. The page shows the new holder; try again if it still applies.",
  rate_limited: "Too many actions at once (10 a second). Wait a moment and retry.",
  no_running_run: "No run is running, so there is nothing to steer or interrupt. Queue it for the next run instead.",
  room_busy: "A run is already running here, or one was just requested and has not joined yet.",
  reviewer_needs_pr: "A reviewer needs a pull request: give its URL, since no agent handoff or verdict names one.",
  over_budget: "The factory refused the run: you are over your budget.",
  factory_unavailable: "The run could not be requested right now. Retry.",
  bad_action: "The broker refused the request as malformed. Check the fields: a give goes to a member who can drive, or to the room's system holder.",
  not_queued: "That message was already delivered or removed.",
  sealed: "This room is sealed: it takes no more actions.",
  conflict: "The room changed at the same moment. Retry.",
  log_unavailable: "The room's log is unavailable right now. Retry.",
  already_decided: "Another approver decided first.",
  four_eyes: "This room needs an approver who did not prompt the turn.",
};

export function rejection(reason: string): string {
  return rejections[reason] ?? `Refused: ${reason}`;
}

// The part of a RoomConnection the controls use.
export interface Sender { send(frame: Record<string, unknown>): boolean }

let clientSeq = 0;

function el<K extends keyof HTMLElementTagNameMap>(tag: K, props: Partial<HTMLElementTagNameMap[K]> = {}): HTMLElementTagNameMap[K] {
  return Object.assign(document.createElement(tag), props);
}

function button(label: string, act: string, onClick: () => void): HTMLButtonElement {
  const b = el("button", { type: "button", textContent: label });
  b.dataset.act = act;
  b.onclick = onClick;
  return b;
}

// named gives a field its accessible name: a placeholder is not one (R10).
function named<T extends HTMLElement>(e: T, label: string): T {
  e.setAttribute("aria-label", label);
  return e;
}

function select(name: string, label: string, options: [string, string][]): HTMLSelectElement {
  const s = named(el("select", { name }), label);
  for (const [value, text] of options) s.append(el("option", { value, textContent: text }));
  return s;
}

interface Row { key: string; sig: string; build: () => HTMLElement }

// keyed renders rows into list, rebuilding a row only when its sig changes and
// moving none already in place: the row a human is in keeps its focus (R10).
function keyed(list: HTMLElement) {
  const held = new Map<string, { sig: string; node: HTMLElement }>();
  return (rows: Row[]) => {
    const keys = new Set(rows.map((r) => r.key));
    for (const [key, h] of held) if (!keys.has(key)) { h.node.remove(); held.delete(key); }
    let prev: HTMLElement | null = null;
    for (const r of rows) {
      let h = held.get(r.key);
      if (!h || h.sig !== r.sig) {
        const node = r.build();
        h?.node.replaceWith(node);
        held.set(r.key, (h = { sig: r.sig, node }));
      }
      if ((prev ? prev.nextElementSibling : list.firstElementChild) !== h.node) {
        if (prev) prev.after(h.node); else list.prepend(h.node);
      }
      prev = h.node;
    }
  };
}

// Every act carries the epoch it was decided on; the broker fences driver-only
// actions on it (§2). Returns its clientSeq, or 0 when the socket is not open.
export function act(conn: Sender, state: RoomState, action: Record<string, unknown>): number {
  const seq = ++clientSeq;
  return conn.send({ type: "act", clientSeq: seq, driverEpoch: state.driverEpoch, action }) ? seq : 0;
}

// approvalCard shows one approval, read-only: the class, the run, the deadline,
// and the raw action as text (T3). render.ts shows an approval_requested event with it.
export function approvalCard(a: { class: string; runId?: string; callId: string; action: unknown; expiresAt: string },
  tag: "li" | "div" = "div"): HTMLElement {
  const li = el(tag, { className: "approval" });
  const due = new Date(a.expiresAt);
  // Today's deadline shows its time; another day's, its date too (R11).
  const when = isNaN(due.getTime()) ? "?" : due.toDateString() === new Date().toDateString() ? due.toLocaleTimeString() : due.toLocaleString();
  li.append(
    el("div", { className: "approval-head", textContent: `approval: ${a.class}${a.runId ? " · run " + a.runId : ""} · call ${a.callId} · expires ${when}` }),
    el("pre", { className: "approval-action", textContent: JSON.stringify(a.action ?? null, null, 2) }),
  );
  return li;
}

// hasControls: a collaborator and up acts; a watcher only reads, unless it is an
// approver, which decides whatever its role (policy Decide).
export function hasControls(you: Snapshot["you"]): boolean {
  return you.role !== "watcher" || you.approver;
}

// mountControls renders what a collaborator and up, or an approver, can do: the composer, the queue,
// the driver token, interrupt, and hand to role. Room text goes in as text only (T10).
// The buttons follow the policy (internal/policy); the broker still decides.
// say sets the footer's notice.
export function mountControls(root: HTMLElement, conn: Sender, state: RoomState, you: Snapshot["you"], say: (text: string) => void) {
  const send = (action: Record<string, unknown>) => act(conn, state, action);
  const isDriver = () => state.driver === you.principal;
  const isOwner = () => you.role === "owner";

  const text = named(el("textarea", { name: "text", maxLength: 16384, rows: 3, placeholder: "Write to the room" }), "message");
  // The room is the default: queued and steered text enters an agent's prompt (R09).
  const delivery = select("delivery", "where this message goes", [["none", "post to the room (no agent is prompted)"],
    ["queued", "queue for the next run's brief"], ["steering", "steer the running agent now"]]);
  delivery.append(el("option", { value: "", textContent: "choose where this goes", disabled: true, hidden: true }));
  const steering = delivery.querySelector<HTMLOptionElement>('option[value="steering"]')!;
  const choose = "You no longer hold the driver token: choose where this message goes.";
  const composer = el("div", { className: "composer" });
  // The text stays until its own ack accepts it: a rejected message is not lost.
  let sent = { seq: 0, text: "" };
  composer.append(text, delivery, button("send", "message", () => {
    if (!text.value.trim()) return;
    if (!delivery.value) return say(choose);
    sent = { seq: send({ kind: "message", text: text.value, delivery: delivery.value }), text: text.value };
  }));

  const queue = el("ul", { className: "queue" });
  const queueRows = keyed(queue);
  const renderQueue = () => queueRows(state.queue().map((q) => {
    const remove = !state.sealed && (q.author === you.principal || isDriver()); // sealed: every action answers sealed
    const promote = !state.sealed && isDriver();
    return { key: String(q.ref), sig: `${remove} ${promote}`, build: () => {
      const li = el("li");
      li.append(el("span", { textContent: `#${q.ref} ${q.author}: ${q.text}` }));
      if (remove) li.append(button("remove", "remove_queued", () => send({ kind: "remove_queued", ref: q.ref })));
      if (promote) li.append(button("steer now", "promote_queued", () => send({ kind: "promote_queued", ref: q.ref })));
      return li;
    } };
  }));

  // One card per pending approval (§6): its class, the raw action as text, never
  // agent prose (T3), and its deadline. Approvers and owners decide; the broker
  // still checks four-eyes and who decided first.
  const approvals = el("ul", { className: "approvals" });
  const approvalRows = keyed(approvals);
  const canDecide = () => !state.sealed && (you.approver || isOwner()); // else the broker answers sealed or not_permitted
  const renderApprovals = () => approvalRows(state.approvals().map((a) => ({ key: a.approvalId, sig: String(canDecide()), build: () => {
    const li = approvalCard(a, "li");
    if (!canDecide()) return li;
    const reason = named(el("input", { name: "decisionReason", maxLength: 1024, placeholder: "reason (optional)" }), "reason for the decision (optional)");
    const decide = (decision: string) => () => {
      const action: Record<string, unknown> = { kind: "decide", approvalId: a.approvalId, decision };
      if (reason.value.trim()) action.reason = reason.value.trim();
      send(action);
    };
    li.append(reason, button("approve", "approve", decide("approved")), button("deny", "deny", decide("denied")));
    return li;
  } })));

  // The driver's fields stay mounted and toggle hidden: a refresh never takes the
  // focus or the text of a field the human is in (R10).
  const holder = el("span");
  const giveTo = named(el("input", { name: "giveTo", placeholder: "human:<sub> or system:factory" }), "give the driver token to");
  const give = button("give", "driver_give", () => {
    const to = giveTo.value.trim();
    if (to && send({ kind: "driver_give", to })) giveTo.value = "";
  });
  const interrupt = button("interrupt", "interrupt", () => send({ kind: "interrupt" }));
  const request = button("request the token", "driver_request", () => send({ kind: "driver_request" }));
  const takeReason = named(el("input", { name: "takeReason", maxLength: 256, placeholder: "why take the token" }), "why take the driver token");
  const take = button("take", "driver_take", () => {
    const reason = takeReason.value.trim();
    if (reason && send({ kind: "driver_take", reason })) takeReason.value = "";
  });
  const driver = el("div", { className: "driver" });
  driver.append(holder, giveTo, give, interrupt, request, takeReason, take);
  const renderDriver = () => {
    holder.textContent = `driver: ${state.driver}${isDriver() ? " (you)" : ""} · epoch ${state.driverEpoch}`;
    for (const e of [giveTo, give, interrupt]) e.hidden = !isDriver();
    request.hidden = isDriver();
    for (const e of [takeReason, take]) e.hidden = isDriver() || !isOwner();
  };

  const hand = el("form", { className: "hand" });
  const role = select("role", "role of the run to start", [["implementer", "implementer"], ["reviewer", "reviewer"], ["tester", "tester"], ["triager", "triager"]]);
  const prUrl = named(el("input", { name: "prUrl", type: "url", placeholder: "PR URL (reviewer only)" }), "pull request URL (reviewer only)");
  const egress = named(el("input", { name: "egress", placeholder: "egress profiles: pypi, npm" }), "egress profiles, comma-separated");
  hand.append(role, prUrl, egress, el("button", { type: "submit", textContent: "hand to role" }));
  hand.onsubmit = (e) => {
    e.preventDefault();
    const action: Record<string, unknown> = { kind: "start_run", role: role.value };
    if (role.value === "reviewer" && prUrl.value.trim()) action.prUrl = prUrl.value.trim(); // reviewer only (docs/api.md)
    const profiles = egress.value.split(",").map((s) => s.trim()).filter(Boolean);
    if (profiles.length) action.egressProfiles = profiles;
    send(action);
  };

  const manifest = el("div", { className: "manifest", hidden: true });
  const claim = el("pre");
  manifest.append(claim, button("copy", "copy", () => void navigator.clipboard?.writeText(claim.textContent ?? "")));

  // Mounted once: refresh toggles hidden, never re-mounts (R10).
  root.replaceChildren(approvals, driver, composer, queue, hand, manifest);
  const showResult = (result: unknown) => {
    if (!result) return;
    // JSON escapes every newline in a string, so no line of it can be a bare EOF.
    claim.textContent = `# Before SP3 the owner creates the run (C3):\nkubectl create -f - <<'EOF'\n${JSON.stringify(result, null, 2)}\nEOF`;
    manifest.hidden = false;
  };
  return {
    refresh() {
      steering.disabled = !isDriver();
      // A lost token picks no other delivery for the human: both prompt or skip an
      // agent the human meant to reach (R09, reversing review 4.5 I3).
      if (steering.disabled && delivery.value === "steering") {
        delivery.value = "";
        say(choose);
      }
      // A watcher who is an approver gets the approval cards only (policy Decide).
      const watcher = you.role === "watcher";
      for (const e of [driver, composer, queue]) e.hidden = watcher;
      hand.hidden = watcher || !(isDriver() || isOwner());
      manifest.hidden = watcher || !claim.textContent;
      renderDriver();
      renderQueue();
      renderApprovals();
    },
    // result is a start_run ack's rendered AgentRun, before SP3 (ruling P14).
    showResult,
    // onAck takes every ack: an accepted one clears the composer it came from, if
    // unedited since, and shows a claim it carries.
    onAck(f: Frame) {
      if (f.rejected) return;
      if (f.clientSeq && f.clientSeq === sent.seq && text.value === sent.text) text.value = "";
      showResult(f.result);
    },
  };
}
