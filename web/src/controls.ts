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

function select(name: string, options: [string, string][]): HTMLSelectElement {
  const s = el("select", { name });
  for (const [value, label] of options) s.append(el("option", { value, textContent: label }));
  return s;
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
  li.append(
    el("div", { className: "approval-head", textContent: `approval: ${a.class}${a.runId ? " · run " + a.runId : ""} · call ${a.callId} · expires ${isNaN(due.getTime()) ? "?" : due.toLocaleTimeString()}` }),
    el("pre", { className: "approval-action", textContent: JSON.stringify(a.action ?? null, null, 2) }),
  );
  return li;
}

// mountControls renders what a collaborator and up can do: the composer, the queue,
// the driver token, interrupt, and hand to role. Room text goes in as text only (T10).
// The buttons follow the policy (internal/policy); the broker still decides.
export function mountControls(root: HTMLElement, conn: Sender, state: RoomState, you: Snapshot["you"]) {
  const send = (action: Record<string, unknown>) => act(conn, state, action);
  const isDriver = () => state.driver === you.principal;
  const isOwner = () => you.role === "owner";

  const text = el("textarea", { name: "text", maxLength: 16384, rows: 3, placeholder: "Write to the room" });
  const delivery = select("delivery", [["none", "chat"], ["queued", "queue for the next run"], ["steering", "steer the run now"]]);
  const steering = delivery.querySelector<HTMLOptionElement>('option[value="steering"]')!;
  const composer = el("div", { className: "composer" });
  // The text stays until its own ack accepts it: a rejected message is not lost.
  let sent = { seq: 0, text: "" };
  composer.append(text, delivery, button("send", "message", () => {
    if (!text.value.trim()) return;
    sent = { seq: send({ kind: "message", text: text.value, delivery: delivery.value }), text: text.value };
  }));

  const queue = el("ul", { className: "queue" });
  const renderQueue = () => {
    queue.replaceChildren(...state.queue().map((q) => {
      const li = el("li");
      li.append(el("span", { textContent: `#${q.ref} ${q.author}: ${q.text}` }));
      if (state.sealed) return li; // every action answers sealed
      if (q.author === you.principal || isDriver()) li.append(button("remove", "remove_queued", () => send({ kind: "remove_queued", ref: q.ref })));
      if (isDriver()) li.append(button("steer now", "promote_queued", () => send({ kind: "promote_queued", ref: q.ref })));
      return li;
    }));
  };

  // One card per pending approval (§6): its class, the raw action as text, never
  // agent prose (T3), and its deadline. Approvers and owners decide; the broker
  // still checks four-eyes and who decided first. A typed reason survives re-renders.
  const approvals = el("ul", { className: "approvals" });
  const reasons = new Map<string, HTMLInputElement>();
  const canDecide = () => you.approver || isOwner();
  const renderApprovals = () => {
    const cards = state.approvals();
    for (const id of reasons.keys()) if (!cards.some((a) => a.approvalId === id)) reasons.delete(id);
    approvals.replaceChildren(...cards.map((a) => {
      const li = approvalCard(a, "li");
      if (state.sealed || !canDecide()) return li; // the broker answers sealed or not_permitted
      let reason = reasons.get(a.approvalId);
      if (!reason) reasons.set(a.approvalId, (reason = el("input", { name: "decisionReason", maxLength: 1024, placeholder: "reason (optional)" })));
      const decide = (decision: string) => () => {
        const action: Record<string, unknown> = { kind: "decide", approvalId: a.approvalId, decision };
        if (reason.value.trim()) action.reason = reason.value.trim();
        send(action);
      };
      li.append(reason, button("approve", "approve", decide("approved")), button("deny", "deny", decide("denied")));
      return li;
    }));
  };

  const giveTo = el("input", { name: "giveTo", placeholder: "human:<sub> or system:factory" });
  const takeReason = el("input", { name: "takeReason", maxLength: 256, placeholder: "why take the token" });
  const driver = el("div", { className: "driver" });
  const renderDriver = () => {
    driver.replaceChildren(el("span", { textContent: `driver: ${state.driver}${isDriver() ? " (you)" : ""} · epoch ${state.driverEpoch}` }));
    if (isDriver()) {
      driver.append(giveTo, button("give", "driver_give", () => {
        const to = giveTo.value.trim();
        if (to && send({ kind: "driver_give", to })) giveTo.value = "";
      }), button("interrupt", "interrupt", () => send({ kind: "interrupt" })));
      return;
    }
    driver.append(button("request the token", "driver_request", () => send({ kind: "driver_request" })));
    if (isOwner()) {
      driver.append(takeReason, button("take", "driver_take", () => {
        const reason = takeReason.value.trim();
        if (reason && send({ kind: "driver_take", reason })) takeReason.value = "";
      }));
    }
  };

  const hand = el("form", { className: "hand" });
  const role = select("role", [["implementer", "implementer"], ["reviewer", "reviewer"], ["tester", "tester"], ["triager", "triager"]]);
  const prUrl = el("input", { name: "prUrl", type: "url", placeholder: "PR URL (reviewer only)" });
  const egress = el("input", { name: "egress", placeholder: "egress profiles: pypi, npm" });
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
      if (steering.disabled && delivery.value === "steering") delivery.value = "none";
      hand.hidden = !(isDriver() || isOwner());
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
