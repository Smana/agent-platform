// SPDX-License-Identifier: Apache-2.0

import type { Frame, Snapshot } from "./conn";
import type { RoomState } from "./room-state";
import { safeLink, type Summary } from "./summary";

// Human copy for an ack's rejected reason (docs/api.md, Actions).
const rejections: Record<string, string> = {
  not_permitted: "Not allowed: your role in this room, or this client, does not permit it; or the factory refused you a run.",
  stale_epoch: "Someone else holds the driver token now. The page shows the new holder; try again if it still applies.",
  rate_limited: "Too many actions at once (10 a second), or forks (3, then one a minute). Wait and retry.",
  no_running_run: "No run is running, so there is nothing to steer or interrupt. Queue it for the next run instead.",
  room_busy: "A run is already running here, or one was just requested and has not joined yet.",
  reviewer_needs_pr: "A reviewer needs a pull request: give its URL, since no agent handoff or verdict names one.",
  over_budget: "The factory refused the run: you are over your budget.",
  factory_unavailable: "The run could not be requested right now. Retry.",
  bad_action: "The broker refused the request as malformed. Check the fields: a give goes to a member who can drive, or to the room's system holder.",
  not_queued: "That message was already delivered or removed.",
  sealed: "This room is sealed: it takes no more actions, and only its owner or an admin may fork it.",
  conflict: "The room changed at the same moment. Retry.",
  log_unavailable: "The room's log is unavailable right now. Retry.",
  already_decided: "Another approver decided first.",
  four_eyes: "This room needs an approver who did not prompt the turn.",
  too_large: "Forking here copies over 5,000 events or 32 MiB. Fork from an earlier message.",
};

// The element id of an approval's card in the needs-you panel: the broker's
// <room>#<approvalId> link lands here.
export const approvalAnchor = (approvalId: string) => `approval-${approvalId}`;

export function rejection(reason: string): string {
  return rejections[reason] ?? `Refused: ${reason}`;
}

// The part of a RoomConnection the controls use.
export interface Sender { send(frame: Record<string, unknown>): boolean }

let clientSeq = 0;

export function el<K extends keyof HTMLElementTagNameMap>(tag: K, props: Partial<HTMLElementTagNameMap[K]> = {}): HTMLElementTagNameMap[K] {
  return Object.assign(document.createElement(tag), props);
}

export function button(label: string, act: string, onClick: () => void): HTMLButtonElement {
  const b = el("button", { type: "button", textContent: label });
  b.dataset.act = act;
  b.onclick = onClick;
  return b;
}

// shortPrincipal keeps the kind and the first four characters of a long id; show the full
// id as the element's title beside it (principalSpan).
export function shortPrincipal(p: string): string {
  const i = p.indexOf(":");
  const id = p.slice(i + 1);
  return id.length > 8 ? `${p.slice(0, i + 1)}${id.slice(0, 4)}…` : p;
}

// principalSpan is a principal as the page shows one: short, the full id as its title.
export function principalSpan(p: string, cls = "principal"): HTMLSpanElement {
  const s = el("span", { className: cls, textContent: shortPrincipal(p) });
  if (s.textContent !== p) s.title = p;
  return s;
}

// named gives a field its accessible name: a placeholder is not one (R10).
export function named<T extends HTMLElement>(e: T, label: string): T {
  e.setAttribute("aria-label", label);
  return e;
}

export function select(name: string, label: string, options: [string, string][]): HTMLSelectElement {
  const s = named(el("select", { name }), label);
  for (const [value, text] of options) s.append(el("option", { value, textContent: text }));
  return s;
}

interface Row { key: string; sig: string; build: () => HTMLElement }

// keyed renders rows into list, rebuilding a row only when its sig changes and
// moving none already in place: the row a human is in keeps its focus (R10).
export function keyed(list: HTMLElement) {
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

// claimText is a run's rendered AgentRun as the owner applies it, before SP3 (ruling P14).
// JSON escapes every newline in a string, so no line of it can be a bare EOF.
export function claimText(result: unknown): string {
  return `# Before SP3 the owner creates the run (C3):\nkubectl create -f - <<'EOF'\n${JSON.stringify(result, null, 2)}\nEOF`;
}

// approvalCard shows one approval, read-only: the command it would run first (the
// call's own, else its class), then the class, the run and the deadline, and the raw
// action as text behind a toggle (T3). render.ts shows an approval_requested event with
// it; the needs-you panel (needs.ts) adds the link to the call and the decide controls.
// withCall puts the call id in the meta line, for the raw log, where no link reaches the call.
export function approvalCard(a: { class: string; runId?: string; callId: string; action: unknown; expiresAt: string },
  tag: "li" | "div" = "div", withCall = false): HTMLElement {
  const li = el(tag, { className: "approval" });
  const due = new Date(a.expiresAt);
  // Today's deadline shows its time; another day's, its date too (R11).
  const when = isNaN(due.getTime()) ? "?" : due.toDateString() === new Date().toDateString() ? due.toLocaleTimeString() : due.toLocaleString();
  const command = (a.action as { command?: unknown } | null)?.command;
  const json = el("details", { className: "approval-json" });
  json.append(el("summary", { textContent: "action" }), el("pre", { textContent: JSON.stringify(a.action ?? null, null, 2) }));
  li.append(
    el("pre", { className: "approval-command", textContent: typeof command === "string" && command ? command : a.class }),
    el("div", { className: "approval-meta", textContent: `${a.class}${a.runId ? " · run " + a.runId : ""}${withCall ? " · call " + a.callId : ""} · expires ${when}` }),
    json,
  );
  return li;
}

// hasControls: a collaborator and up acts; a watcher only reads, unless it is an
// approver, which decides whatever its role (policy Decide).
export function hasControls(you: Snapshot["you"]): boolean {
  return you.role !== "watcher" || you.approver;
}

// labelled wraps a field in a visible label: the text a sighted human reads, while
// named() keeps the field's accessible name (R10). No placeholder stands in for either.
export function labelled(text: string, field: HTMLElement, cls = "field"): HTMLLabelElement {
  const l = el("label", { className: cls });
  l.append(el("span", { textContent: text }), field);
  return l;
}

// A confirm button's span, with disarm: whoever hides it puts the ask back.
export type Confirm = HTMLSpanElement & { disarm(): void };

// confirmButton is a destructive act's button: the first click asks ("confirm: label")
// beside a cancel, the second does it. The ask goes back after 10 s, on cancel, or on
// disarm (the control was hidden), so a stale confirm never fires on a stray click. ready,
// when given, is checked before the ask: false keeps the button unarmed. The span is a
// polite live region: the ask is announced.
export function confirmButton(label: string, act: string, onConfirm: () => void, ready: () => boolean = () => true): Confirm {
  const span = el("span", { className: "confirm" });
  span.setAttribute("aria-live", "polite");
  const cancel = button("cancel", `${act}-cancel`, () => { reset(); main.focus(); });
  let timer: ReturnType<typeof setTimeout> | undefined;
  const reset = () => {
    const focused = document.activeElement === cancel; // the cancel goes: its focus moves to the button
    clearTimeout(timer);
    timer = undefined;
    main.textContent = label;
    cancel.remove();
    if (focused) main.focus();
  };
  const main = button(label, act, () => {
    if (timer === undefined) {
      if (!ready()) return;
      main.textContent = `confirm: ${label}`;
      main.after(cancel);
      timer = setTimeout(reset, confirmFor);
      return;
    }
    reset();
    onConfirm();
  });
  main.classList.add("destructive");
  span.append(main);
  return Object.assign(span, { disarm: () => { if (timer !== undefined) reset(); } });
}

// hiddenWithin is whether e, or an ancestor up to root, is hidden.
function hiddenWithin(e: HTMLElement, root: HTMLElement): boolean {
  for (let n: HTMLElement | null = e; n && n !== root.parentElement; n = n.parentElement) if (n.hidden) return true;
  return false;
}
const confirmFor = 10_000;

function group(cls: string, title: string): HTMLElement {
  const s = el("section", { className: `group ${cls}` });
  s.append(el("h2", { textContent: title }));
  return s;
}

// mountControls renders what a collaborator and up can do, in named groups: Run (start a
// run as a role), Driver (the token, interrupt), the queue, and Room (members, invite, the
// stop instruction, close), the last behind a toggle and for owners only. The composer
// docks at the page's foot (mountComposer). Approvals are decided in the needs-you panel
// (needs.ts). Room text goes in as text only (T10).
// The buttons follow the policy (internal/policy); the broker still decides.
// say sets the footer's notice.
export function mountControls(root: HTMLElement, conn: Sender, state: RoomState, you: Snapshot["you"], say: (text: string) => void) {
  const send = (action: Record<string, unknown>) => act(conn, state, action);
  const isDriver = () => state.driver === you.principal;
  const isOwner = () => you.role === "owner";

  // Run: start a run as a role. The PR URL shows for a reviewer only; egress is advanced.
  const run = group("group-run", "Run");
  const hand = el("form", { className: "hand" });
  const role = select("role", "Start a run as", [["implementer", "implementer"], ["reviewer", "reviewer"], ["tester", "tester"], ["triager", "triager"]]);
  const prUrl = named(el("input", { name: "prUrl", type: "url" }), "Pull request URL (reviewer only)");
  const prLabel = labelled("Pull request URL", prUrl);
  const egress = named(el("input", { name: "egress" }), "Egress profiles (comma-separated)");
  const advanced = el("details", { className: "advanced" });
  advanced.append(el("summary", { textContent: "advanced" }), labelled("Egress profiles (comma-separated)", egress));
  const showPr = () => { prUrl.hidden = prLabel.hidden = role.value !== "reviewer"; };
  role.onchange = showPr;
  showPr();
  hand.append(labelled("Start a run as", role), prLabel, el("button", { type: "submit", textContent: "start run" }), advanced);
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
  run.append(hand, manifest);

  // Driver: the fields stay mounted and toggle hidden: a refresh never takes the
  // focus or the text of a field the human is in (R10).
  const drv = group("group-driver", "Driver");
  const holder = el("span", { className: "driver-holder" });
  const giveTo = named(el("input", { name: "giveTo" }), "Give to (the driver token, to human:<sub> or system:factory)");
  const giveLabel = labelled("Give to", giveTo);
  const give = button("give", "driver_give", () => {
    const to = giveTo.value.trim();
    if (to && send({ kind: "driver_give", to })) giveTo.value = "";
  });
  const interrupt = confirmButton("interrupt", "interrupt", () => send({ kind: "interrupt" }));
  const request = button("request the token", "driver_request", () => send({ kind: "driver_request" }));
  const takeReason = named(el("input", { name: "takeReason", maxLength: 256 }), "Why take it (the driver token)");
  const takeLabel = labelled("Why take it", takeReason);
  // The reason is checked before the ask: a confirm with nothing to send is no confirm.
  const take = confirmButton("take", "driver_take", () => {
    const reason = takeReason.value.trim();
    if (reason && send({ kind: "driver_take", reason })) takeReason.value = "";
  }, () => {
    if (takeReason.value.trim()) return true;
    say("Say why you take the driver token.");
    return false;
  });
  const driver = el("div", { className: "driver" });
  driver.append(holder, giveLabel, give, interrupt, request, takeLabel, take);
  drv.append(driver);
  const renderDriver = () => {
    holder.replaceChildren("driver: ", isDriver() ? "you" : principalSpan(state.driver));
    for (const e of [giveLabel, give, interrupt]) e.hidden = !isDriver();
    request.hidden = isDriver();
    for (const e of [takeLabel, take]) e.hidden = isDriver() || !isOwner();
  };

  // The queue: what waits for the next run's brief. steer now prompts the running agent
  // with it, so it asks first.
  const queueBox = el("div", { className: "group-queue" });
  const queue = el("ul", { className: "queue" });
  queue.setAttribute("aria-label", "Queued for the next run");
  queueBox.append(el("p", { className: "group-label", textContent: "Queued for the next run" }), queue);
  const queueRows = keyed(queue);
  const renderQueue = () => queueRows(state.queue().map((q) => {
    const remove = !state.sealed && (q.author === you.principal || isDriver()); // sealed: every action answers sealed
    const promote = !state.sealed && isDriver();
    return { key: String(q.ref), sig: `${remove} ${promote}`, build: () => {
      const li = el("li");
      li.append(`#${q.ref} `, principalSpan(q.author), el("span", { textContent: `: ${q.text}` }));
      if (remove) li.append(button("remove", "remove_queued", () => send({ kind: "remove_queued", ref: q.ref })));
      if (promote) li.append(confirmButton("steer now", "promote_queued", () => send({ kind: "promote_queued", ref: q.ref })));
      return li;
    } };
  }));

  // Room: owner-only, and an admin is an owner in every room (policy Invite, Close): the
  // same acts the broker checks again, so a hidden control is a courtesy and never the gate.
  const room = el("details", { className: "group group-room" });
  room.append(el("summary", { textContent: "Room" }));
  const memberList = el("ul", { className: "members" });
  memberList.setAttribute("aria-label", "Members");
  const memberRows = keyed(memberList);
  const inviteForm = el("form", { className: "invite" });
  const invitee = named(el("input", { name: "invitee" }), "Invite (a member, human:<sub>)");
  const memberRole = select("memberRole", "as (the invited member's role)", [["watcher", "watcher"], ["collaborator", "collaborator"], ["owner", "owner"]]);
  memberRole.value = "collaborator";
  const approver = named(el("input", { name: "inviteApprover", type: "checkbox" }), "approver (may decide approvals)");
  const approverLabel = el("label", { textContent: " approver" });
  approverLabel.prepend(approver);
  let invited = { seq: 0, who: "" };
  const invite = () => {
    const principal = invitee.value.trim();
    if (!principal) return;
    invited = { seq: send({ kind: "invite", principal, memberRole: memberRole.value, approver: approver.checked }), who: invitee.value };
  };
  inviteForm.append(labelled("Invite", invitee), labelled("as", memberRole), approverLabel, button("invite", "invite", invite));
  inviteForm.onsubmit = (e) => { e.preventDefault(); invite(); };
  const stopSlot = el("div");
  // Closing seals the room for good: it asks first.
  const closer = confirmButton("close room", "close", () => send({ kind: "close" }));
  room.append(memberList, inviteForm, stopSlot, closer);
  room.addEventListener("toggle", () => { if (!room.open) closer.disarm(); }); // folded away is hidden too

  // Mounted once: refresh toggles hidden, never re-mounts (R10).
  root.replaceChildren(run, drv, queueBox, room);
  const showResult = (result: unknown) => {
    if (!result) return;
    claim.textContent = claimText(result);
    manifest.hidden = false;
  };
  return {
    refresh() {
      // A watcher who is an approver decides in the needs-you panel, and gets nothing here.
      const watcher = you.role === "watcher";
      // A sealed room takes no run and no token: only its queue and its members remain.
      run.hidden = watcher || state.sealed || !(isDriver() || isOwner());
      manifest.hidden = !claim.textContent;
      drv.hidden = watcher || state.sealed;
      queueBox.hidden = watcher || state.queue().length === 0;
      room.hidden = !isOwner();
      for (const e of [inviteForm, closer]) e.hidden = state.sealed;
      renderDriver();
      renderQueue();
      memberRows(state.members().map((m) => {
        const rest = ` · ${m.role}${m.approver ? " · approver" : ""}`;
        return { key: m.principal, sig: rest, build: () => {
          const li = el("li");
          li.append(principalSpan(m.principal), rest);
          return li;
        } };
      }));
      // A confirm armed and then hidden (by the token moving, a seal, a role change) asks again.
      for (const c of [interrupt, take, closer, ...queue.querySelectorAll<Confirm>(".confirm")]) if (hiddenWithin(c, root)) c.disarm();
    },
    // result is a start_run ack's rendered AgentRun, before SP3 (ruling P14).
    showResult,
    // setSummary takes each summary: with an issue, the Room group says how to stop the task.
    setSummary(sum: Summary) {
      const issue = sum.status?.issue;
      if (!issue) return stopSlot.replaceChildren();
      const p = el("p", { className: "stop" });
      p.append("Stop the task: label the issue ", safeLink(`#${issue.number}`, issue.url), " ", el("code", { textContent: "factory/stop" }));
      stopSlot.replaceChildren(p);
    },
    // onAck takes every ack: an accepted one clears the invite it came from, if unedited
    // since, and shows a claim it carries.
    onAck(f: Frame) {
      if (f.rejected) return;
      if (f.clientSeq && f.clientSeq === invited.seq && invitee.value === invited.who) invitee.value = "";
      showResult(f.result);
    },
  };
}

// mountComposer renders the dock's composer into root: the text, where it goes, Send.
// The text stays until its own ack accepts it: a rejected message is not lost. setLive
// disables Send while the socket is down and hides nothing, so the draft stays in view;
// a sealed room hides the composer, and a watcher has none.
export function mountComposer(root: HTMLElement, conn: Sender, state: RoomState, you: Snapshot["you"]) {
  const isDriver = () => state.driver === you.principal;
  const text = named(el("textarea", { name: "text", maxLength: 16384, rows: 2 }), "message to the room");
  // The room is the default: queued and steered text enters an agent's prompt (R09).
  const delivery = select("delivery", "Deliver to", [["none", "post to the room (no agent is prompted)"],
    ["queued", "queue for the next run's brief"], ["steering", "steer the running agent now"]]);
  delivery.append(el("option", { value: "", textContent: "choose where this goes", disabled: true, hidden: true }));
  delivery.setAttribute("aria-describedby", "delivery-notice");
  const steering = delivery.querySelector<HTMLOptionElement>('option[value="steering"]')!;
  const choose = "You no longer hold the driver token: choose where this message goes.";
  const noRun = "No run is on now, so no agent to steer: choose where this message goes.";
  // The notice sits beside the select it is about, so the page's act notice keeps its own.
  const notice = el("span", { id: "delivery-notice", className: "notice" });
  notice.setAttribute("role", "status");
  notice.setAttribute("aria-live", "polite");
  delivery.onchange = () => { notice.textContent = ""; };
  let sent = { seq: 0, text: "" };
  const sendBtn = button("send", "message", () => {
    if (!text.value.trim()) return;
    if (!delivery.value) { notice.textContent = choose; return; }
    sent = { seq: act(conn, state, { kind: "message", text: text.value, delivery: delivery.value }), text: text.value };
    if (sent.seq) notice.textContent = ""; // a send that went out supersedes the notice
  });
  sendBtn.classList.add("primary");
  const row = el("div", { className: "composer-row" });
  row.append(labelled("Deliver to", delivery), notice, sendBtn);
  root.replaceChildren(text, row);
  return {
    refresh() {
      // Steering needs the token and an agent present now (Q2): with no run it is not offered.
      steering.hidden = !state.running();
      steering.disabled = !isDriver() || steering.hidden;
      // A lost token or an ended run picks no other delivery for the human: both prompt or
      // skip an agent the human meant to reach (R09, reversing review 4.5 I3). The draft stays.
      if (steering.disabled && delivery.value === "steering") {
        delivery.value = "";
        notice.textContent = isDriver() ? noRun : choose;
      }
      root.hidden = you.role === "watcher";
      text.hidden = row.hidden = state.sealed;
    },
    onAck(f: Frame) {
      if (!f.rejected && f.clientSeq && f.clientSeq === sent.seq && text.value === sent.text) text.value = "";
    },
    setLive(live: boolean) { sendBtn.disabled = !live; },
  };
}
