// SPDX-License-Identifier: Apache-2.0

// The chat view: the event log read as a conversation. Messages are bubbles, a
// tool_call pairs with its tool_result into one collapsed row (by callId), the
// policy's decisions hang off that row as badges, and state_changed/turn become
// slim timeline markers. The raw stream stays one toggle away (roomview.ts).
// The same untrusted-text rules as render.ts hold: DOM nodes and textContent only.

import type { RoomEvent } from "./conn";
import { deliveryChip, markdown, stateHead } from "./render";
import { maxRows } from "./view";

function el(tag: string, cls: string, text?: string): HTMLElement {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

// A class name from a payload value: letters, digits, _ and - only.
const cls = (v: unknown) => String(v ?? "").replace(/[^A-Za-z0-9_-]/g, "");

const time = (ts: string) => el("time", "t-time", new Date(ts).toLocaleTimeString());

function redacted(ev: RoomEvent): HTMLElement | null {
  return ev.redactions?.length ? el("div", "redacted", `redacted: ${ev.redactions.join(", ")}`) : null;
}

// who names the actor by its room role when it has one, else by id.
function who(ev: RoomEvent): string {
  return ev.actor.role ? `${ev.actor.role} (${ev.actor.id})` : ev.actor.id;
}

// bubble renders a message as the agent's or human's prose.
function bubble(ev: RoomEvent): HTMLElement {
  const p = ev.payload ?? {};
  const row = el("article", `msg from-${cls(ev.actor.kind)}`);
  row.dataset.seq = String(ev.seq);
  const head = el("header", "msg-head", who(ev));
  head.append(deliveryChip(p));
  if (p.kind === "review_verdict" && p.verdict) {
    head.append(el("span", `chip verdict-${cls(p.verdict)}`, `verdict: ${p.verdict}`));
  }
  head.append(time(ev.ts));
  const body = el("div", "bubble");
  body.append(markdown(String(p.text ?? "")));
  row.append(head, body);
  const r = redacted(ev);
  if (r) row.append(r);
  return row;
}

// One tool row's state: the call, its result when it lands, and the policy's
// decisions about it, re-rendered together whenever one changes.
interface Call {
  seq: number; // the row's place: the call's seq, or an orphan result's own
  callId: string;
  call?: RoomEvent;
  result?: RoomEvent;
  decisions: RoomEvent[];
}

// preview is the one-line summary of a call: its command, or its args compacted.
function preview(call: RoomEvent | undefined, result: RoomEvent | undefined): string {
  const args = call?.payload?.args;
  if (args && typeof args.command === "string") return args.command;
  if (args) return JSON.stringify(args);
  const out = String(result?.payload?.output ?? "");
  return out.split("\n", 1)[0];
}

function chip(status: string): HTMLElement {
  return el("span", `chip chip-${cls(status)}`, status);
}

function polBadge(d: RoomEvent): HTMLElement {
  const p = d.payload ?? {};
  // policy_decision carries `decision`; decision_applied may carry only a ref.
  const label = p.decision ?? (p.kind === "decision_applied" ? "applied" : "?");
  return el("span", `pol pol-${cls(label)}`, `policy: ${label}`);
}

function toolRow(c: Call): HTMLElement {
  const status = c.result ? String(c.result.payload?.status ?? "ok") : "running";
  const d = el("details", `tool st-${cls(status)}`) as HTMLDetailsElement;
  d.dataset.seq = String(c.seq);
  d.dataset.callId = c.callId;

  const sum = el("summary", "");
  const name = c.call ? String(c.call.payload?.tool ?? "tool") : "tool";
  sum.append(el("span", "t-name", name), el("code", "t-cmd", preview(c.call, c.result)), chip(status));
  for (const dec of c.decisions) sum.append(polBadge(dec));
  const by = c.call?.payload?.decidedBy;
  if (by) sum.append(el("span", "pol pol-by", `decided by ${cls(by)}`));
  sum.append(time((c.result ?? c.call ?? c.decisions[0])!.ts));

  const body = el("div", "t-body");
  if (c.call) {
    body.append(el("div", "t-sec", "arguments"));
    body.append(el("pre", "t-args", JSON.stringify(c.call.payload?.args ?? null, null, 2)));
  }
  if (c.result) {
    const p = c.result.payload ?? {};
    body.append(el("div", "t-sec", `output · ${p.bytes ?? "?"} B${p.truncated ? " · truncated" : ""}`));
    body.append(el("pre", "t-out", String(p.output ?? "")));
  } else {
    body.append(el("div", "t-sec", "waiting for the result…"));
  }
  const meta = [`callId ${c.callId}`];
  if (c.call?.payload?.class) meta.push(`class ${c.call.payload.class}`);
  if (c.call?.payload?.risk) meta.push(`risk ${c.call.payload.risk}`);
  body.append(el("div", "t-meta", meta.join(" · ")));
  for (const e of [c.call, c.result, ...c.decisions]) {
    const r = e ? redacted(e) : null;
    if (r) body.append(r);
  }
  d.append(sum, body);
  return d;
}

// marker renders a state_changed or turn as one slim timeline line.
function marker(ev: RoomEvent): HTMLElement {
  const p = ev.payload ?? {};
  const kind = ev.type === "turn" ? "turn" : String(p.kind ?? "");
  const row = el("div", `mark mk-${cls(kind)}`);
  row.dataset.seq = String(ev.seq);
  let text: string;
  switch (ev.type === "turn" ? "turn" : p.kind) {
    case "turn":
      text = `turn ${p.turnId ?? "?"} ${p.phase ?? "?"}`;
      break;
    case "harness_status":
      text = `harness ${p.previous ? p.previous + " → " : ""}${p.status ?? "?"}`;
      break;
    case "run_phase":
      text = `run ${String(p.phase ?? "?").toLowerCase()}${p.reason ? ": " + p.reason : ""}`;
      break;
    case "room_phase":
      text = `room ${String(p.phase ?? "?").toLowerCase()}${p.reason ? ": " + p.reason : ""}`;
      break;
    case "run_requested":
      text = `run requested: ${p.role ?? "?"}${p.via ? " via " + p.via : ""}`;
      break;
    case "policy_decision":
    case "decision_applied":
      text = `policy: ${p.decision ?? "?"}${p.class ? " · " + p.class : ""}${p.callId ? " · " + p.callId : ""}`;
      break;
    case "limit":
      text = `room sealed${p.events ? `: ${p.events} events` : p.reason ? ": " + p.reason : ""}`;
      break;
    case "harness_error":
      text = `harness error ${p.code ?? ""}${p.detail ? ": " + p.detail : ""}`;
      break;
    case "delivered":
    case "undeliverable":
    case "interrupted":
    case "queued_removed":
      text = `${stateHead(p)}${p.code ? " · " + p.code : ""}`;
      break;
    case "harness_paused":
      text = "harness paused";
      break;
    case "harness_event":
      text = `harness event the UI does not know: ${p.harnessKind ?? p.detail ?? "?"}`;
      break;
    default:
      text = [kind, ...Object.entries(p).filter(([k]) => k !== "kind").map(([k, v]) => `${k}=${typeof v === "object" ? JSON.stringify(v) : v}`)].join(" · ");
  }
  row.append(el("span", "dot"), el("span", "mark-text", text), time(ev.ts));
  return row;
}

// sysline renders membership and coordination as a subtle single line.
function sysline(ev: RoomEvent): HTMLElement {
  const p = ev.payload ?? {};
  let text: string;
  switch (ev.type) {
    case "participant":
      text = `${p.principal ?? "?"} ${p.change ?? "?"}${p.role && p.change !== "left" ? " as " + p.role : ""}${p.approver ? " (approver)" : ""}`;
      break;
    case "driver":
      text = `driver: ${p.from ?? "?"} → ${p.to ?? "?"}${p.reason ? " · " + p.reason : ""}`;
      break;
    case "handoff":
      text = `handoff ${p.fromRole ?? "?"} → ${p.toRole ?? "?"}${p.branch ? " · " + p.branch : ""}${p.summary ? ": " + p.summary : ""}`;
      break;
    default:
      text = `${ev.type}: ${JSON.stringify(p)}`;
  }
  const row = el("div", "sys", text);
  row.dataset.seq = String(ev.seq);
  row.append(time(ev.ts));
  return row;
}

// fallback keeps a type the view does not know visible, behind a toggle.
function fallback(ev: RoomEvent): HTMLElement {
  const d = el("details", "other");
  d.dataset.seq = String(ev.seq);
  d.append(el("summary", "", `${ev.type} · ${who(ev)}`), el("pre", "", JSON.stringify(ev.payload ?? null, null, 2)));
  return d;
}

// ChatView holds the log as a conversation, newest rows last. A resume can
// resend a seq: the row is replaced in place. A tool_call's row is re-rendered
// when its result or a decision lands, so a result never doubles its call.
export class ChatView {
  private rows = new Map<number, HTMLElement>();
  private seqs: number[] = [];
  private calls = new Map<string, Call>();
  // A decision whose call the page never saw (the tail started between them)
  // renders as a marker; if the call still arrives, the marker folds into it.
  private pending = new Map<string, RoomEvent[]>();

  constructor(private el: HTMLElement, private cap = maxRows) {}

  apply(ev: RoomEvent) {
    const p = ev.payload ?? {};
    switch (ev.type) {
      case "message":
        this.put(ev.seq, bubble(ev));
        break;
      case "tool_call":
        this.merge(String(p.callId ?? `seq${ev.seq}`), ev, "call");
        break;
      case "tool_result":
        this.merge(String(p.callId ?? `seq${ev.seq}`), ev, "result");
        break;
      case "state_changed":
        if ((p.kind === "policy_decision" || p.kind === "decision_applied") && p.callId) {
          const callId = String(p.callId);
          if (this.calls.has(callId)) this.merge(callId, ev, "decision");
          else {
            const list = this.pending.get(callId) ?? [];
            if (!list.some((d) => d.seq === ev.seq)) list.push(ev);
            this.pending.set(callId, list);
            this.put(ev.seq, marker(ev));
          }
        } else {
          this.put(ev.seq, marker(ev));
        }
        break;
      case "turn":
        this.put(ev.seq, marker(ev));
        break;
      case "participant":
      case "driver":
      case "handoff":
        this.put(ev.seq, sysline(ev));
        break;
      default:
        this.put(ev.seq, fallback(ev));
    }
  }

  private merge(callId: string, ev: RoomEvent, part: "call" | "result" | "decision") {
    let c = this.calls.get(callId);
    if (!c) {
      c = { seq: ev.seq, callId, decisions: [] };
      this.calls.set(callId, c);
      // Adopt decisions rendered as markers while the call was unseen.
      for (const d of this.pending.get(callId) ?? []) {
        c.decisions.push(d);
        this.drop(d.seq);
      }
      this.pending.delete(callId);
    }
    if (part === "call") c.call = ev;
    else if (part === "result") c.result = ev;
    else if (!c.decisions.some((d) => d.seq === ev.seq)) c.decisions.push(ev);
    this.put(c.seq, toolRow(c));
  }

  private drop(seq: number) {
    const row = this.rows.get(seq);
    if (!row) return;
    row.remove();
    this.rows.delete(seq);
    this.seqs.splice(this.seqs.indexOf(seq), 1);
  }

  private put(seq: number, row: HTMLElement) {
    const old = this.rows.get(seq);
    if (old) {
      old.replaceWith(row);
      this.rows.set(seq, row);
      return;
    }
    let i = this.seqs.length;
    while (i > 0 && this.seqs[i - 1] > seq) i--; // from the end: live events land there
    const next = i < this.seqs.length ? this.rows.get(this.seqs[i]) : undefined;
    if (next) next.before(row); else this.el.append(row);
    this.seqs.splice(i, 0, seq);
    this.rows.set(seq, row);
    while (this.seqs.length > this.cap) {
      const s = this.seqs.shift()!;
      const gone = this.rows.get(s);
      if (gone?.dataset.callId) this.calls.delete(gone.dataset.callId);
      gone?.remove();
      this.rows.delete(s);
    }
  }
}
