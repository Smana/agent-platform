// SPDX-License-Identifier: Apache-2.0

// The chat view: the event log read as a conversation. Messages are bubbles, a
// tool_call pairs with its tool_result into one collapsed row (by callId), the
// policy's decisions hang off that row as badges, and state_changed/turn become
// slim timeline markers. The raw stream stays one toggle away (roomview.ts).
// Plumbing (markers and system lines that say nothing the reader acts on) folds:
// a run of consecutive plumbing rows reads as one "· turn <id> · N changes" line (the
// turn named when the run holds one), open on click.
// The same untrusted-text rules as render.ts hold: DOM nodes and textContent only.

import type { RoomEvent } from "./conn";
import { deliveryChip, handoffHead, markdown, stateHead } from "./render";
import { icon, type IconName } from "./icons";
import { maxRows } from "./limits";

function el(tag: string, cls: string, text?: string): HTMLElement {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

// A class name from a payload value: letters, digits, _ and - only.
const cls = (v: unknown) => String(v ?? "").replace(/[^A-Za-z0-9_-]/g, "");

function time(ts: string): HTMLElement {
  const t = el("time", "t-time", new Date(ts).toLocaleTimeString());
  t.setAttribute("datetime", ts);
  return t;
}

function redacted(ev: RoomEvent): HTMLElement | null {
  return ev.redactions?.length ? el("div", "redacted", `redacted: ${ev.redactions.join(", ")}`) : null;
}

// idTail is a principal without its kind; a long human id keeps its first four characters.
function idTail(id: string): string {
  const i = id.indexOf(":");
  const tail = id.slice(i + 1);
  return id.startsWith("human:") && tail.length > 8 ? `${tail.slice(0, 4)}…` : tail;
}

// swatchClass picks one of six fixed actor colours, keyed on the principal.
export function swatchClass(id: string): string {
  let h = 0;
  for (const c of id) h = (h * 31 + c.charCodeAt(0)) >>> 0;
  return `swatch-${h % 6}`;
}

// who names the actor: "you" for the viewer, else its room role and short id, the
// full id as the title, after its colour swatch.
function who(ev: RoomEvent, you?: string): HTMLElement[] {
  const id = ev.actor.id;
  const name = id === you ? "you" : ev.actor.role ? `${ev.actor.role} · ${idTail(id)}` : idTail(id);
  const w = el("span", "who", name);
  w.title = id;
  return [el("span", `swatch ${swatchClass(id)}`), w];
}

// whoText is who as one string, for a summary line.
function whoText(ev: RoomEvent): string {
  return ev.actor.role ? `${ev.actor.role} · ${idTail(ev.actor.id)}` : ev.actor.id;
}

// bubble renders a message as the agent's or human's prose.
function bubble(ev: RoomEvent, you?: string): HTMLElement {
  const p = ev.payload ?? {};
  const row = el("article", `msg from-${cls(ev.actor.kind)}`);
  row.dataset.seq = String(ev.seq);
  const head = el("header", "msg-head");
  head.append(...who(ev, you));
  const delivery = deliveryChip(p);
  if (delivery) head.append(delivery);
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

function polBadge(d: RoomEvent): HTMLElement {
  const p = d.payload ?? {};
  // policy_decision carries `decision`; decision_applied may carry only a ref.
  const label = p.decision ?? (p.kind === "decision_applied" ? "applied" : "?");
  return el("span", `pol pol-${cls(label)}`, `policy: ${label}`);
}

// The status a call line shows. ok is the norm: a muted check, no word. The rest are
// named beside their icon, so a colour is never the only sign.
const statuses: Record<string, [IconName, boolean]> = {
  ok: ["check", false], running: ["spinner", false], rejected: ["ban", true], error: ["x", true],
};

function statusMark(status: string): HTMLElement {
  const [glyph, worded] = statuses[status] ?? ["x", true];
  const s = el("span", `t-status st-${cls(status)}`);
  s.append(icon(glyph, status));
  if (worded) s.append(el("span", "", status));
  return s;
}

// An output longer than this many lines opens capped, with a "show all".
const capLines = 20;

function toolRow(c: Call): HTMLElement {
  const status = c.result ? String(c.result.payload?.status ?? "ok") : "running";
  const d = el("details", `tool st-${cls(status)}`) as HTMLDetailsElement;
  d.dataset.seq = String(c.seq);
  d.dataset.callId = c.callId;
  d.dataset.lastSeq = String(Math.max(...[c.call, c.result, ...c.decisions].map((e) => e?.seq ?? 0)));
  const name = c.call ? String(c.call.payload?.tool ?? "tool") : "tool";
  const args = c.call?.payload?.args;
  const command = args && typeof args.command === "string" ? args.command : undefined;
  // What the step's summary line reads off the row (step.ts).
  d.dataset.tool = name;
  d.dataset.status = status;
  d.dataset.start = (c.call ?? c.result ?? c.decisions[0])!.ts;
  if (c.result) d.dataset.end = c.result.ts;

  const sum = el("summary", "");
  // A shell command reads as one: a prompt, then the command. Any other tool is named.
  sum.append(name === "terminal" && command !== undefined ? el("span", "t-prompt", "$") : el("span", "t-name", name));
  sum.append(el("code", "t-cmd", preview(c.call, c.result)), statusMark(status));
  const by = c.call?.payload?.decidedBy;
  // An allowed call is the norm: only a decision that is not a plain allow, or one a
  // person made, earns a badge.
  for (const dec of c.decisions) if (by || dec.payload?.decision !== "allow") sum.append(polBadge(dec));
  if (by) sum.append(el("span", "pol pol-by", `decided by ${cls(by)}`));
  sum.append(time((c.result ?? c.call ?? c.decisions[0])!.ts));

  const body = el("div", "t-body");
  if (command !== undefined) body.append(el("pre", "t-command", command));
  // The arguments the summary does not already show: all of them for a tool with no
  // command, the rest beside one, nothing when the command was all there was.
  const rest = args && command !== undefined ? Object.fromEntries(Object.entries(args).filter(([k]) => k !== "command")) : args;
  if (c.call && (command === undefined || Object.keys(rest ?? {}).length > 0)) {
    body.append(el("div", "t-sec", "arguments"));
    body.append(el("pre", "t-args", JSON.stringify(rest ?? null, null, 2)));
  }
  if (c.result) {
    const p = c.result.payload ?? {};
    const output = String(p.output ?? "");
    body.append(el("div", "t-sec", `output · ${p.bytes ?? "?"} B${p.truncated ? " · truncated" : ""}`));
    const out = el("pre", "t-out", output);
    body.append(out);
    if (output.split("\n").length > capLines) {
      out.classList.add("capped");
      const more = el("button", "t-more", "show all") as HTMLButtonElement;
      more.type = "button";
      more.onclick = () => {
        const capped = out.classList.toggle("capped");
        more.textContent = capped ? "show all" : "show less";
      };
      body.append(more);
    }
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
  if (ev.type === "turn" && p.turnId) row.dataset.turn = String(p.turnId); // names its fold
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
    default:
      text = `${ev.type}: ${JSON.stringify(p)}`;
  }
  const row = el("div", "sys", text);
  row.dataset.seq = String(ev.seq);
  row.append(time(ev.ts));
  return row;
}

// handoff renders work passing between roles: its line, then the agent's summary
// as markdown (untrusted, T10).
function handoff(ev: RoomEvent): HTMLElement {
  const p = ev.payload ?? {};
  const box = el("div", "handoff");
  box.dataset.seq = String(ev.seq);
  const line = el("div", "sys", `${handoffHead(p)}${p.branch ? " · " + p.branch : ""}`);
  line.append(time(ev.ts));
  box.append(line, markdown(String(p.summary ?? "")));
  return box;
}

// fallback keeps a type the view does not know visible, behind a toggle.
function fallback(ev: RoomEvent): HTMLElement {
  const d = el("details", "other");
  d.dataset.seq = String(ev.seq);
  d.append(el("summary", "", `${ev.type} · ${whoText(ev)}`), el("pre", "", JSON.stringify(ev.payload ?? null, null, 2)));
  return d;
}

// plumbing is whether ev is a row the reader need not see one by one: a marker or a
// system line, except a harness error, a seal, and a driver change to or from you.
export function plumbing(ev: RoomEvent, you?: string): boolean {
  const p = ev.payload ?? {};
  if (!["state_changed", "turn", "participant", "driver"].includes(ev.type)) return false;
  if (p.kind === "harness_error" || p.kind === "limit") return false;
  return !(ev.type === "driver" && you !== undefined && (p.from === you || p.to === you));
}

// reveal opens every fold and row around row, so a link to it lands on a visible line.
export function reveal(row: HTMLElement): void {
  for (let n: HTMLElement | null = row; n; n = n.parentElement) if (n instanceof HTMLDetailsElement) n.open = true;
}

// focusRow takes the reader to row: opened, scrolled to, and focused, so a keyboard user
// continues from there. A row is no tab stop: tabindex -1 only lets the link focus it.
export function focusRow(row: HTMLElement): void {
  reveal(row);
  row.scrollIntoView?.({ block: "center" });
  if (!row.hasAttribute("tabindex")) row.tabIndex = -1;
  row.focus({ preventScroll: true });
}

const body = (fold: HTMLElement) => fold.querySelector<HTMLElement>(":scope > .fold-body")!;

// label writes a fold's line: the first turn it holds, when it holds one, and its count.
// The turn stays on the fold, so a row appended to it needs no walk of its rows.
function label(fold: HTMLElement, turn = body(fold).querySelector<HTMLElement>(":scope > [data-turn]")?.dataset.turn) {
  const n = body(fold).childElementCount;
  if (turn) fold.dataset.turn = turn;
  else delete fold.dataset.turn;
  fold.querySelector(":scope > summary")!.textContent = turn ? `· turn ${turn} · ${n} changes` : `· ${n} changes`;
}

// tidy keeps a fold honest after a row left it: an empty one goes, one with a single
// row gives the row back to the stream, else its line is rewritten.
function tidy(fold: HTMLElement) {
  const inner = body(fold);
  if (inner.childElementCount === 0) fold.remove();
  else if (inner.childElementCount === 1) fold.replaceWith(inner.firstElementChild!);
  else label(fold);
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

  // fork, when set, gives every message and handoff a "fork here" at its seq. you, the
  // viewer's principal (or a getter for it, known once the state frame lands), names
  // the viewer's own rows "you".
  constructor(private el: HTMLElement, private cap = maxRows, private fork?: (seq: number) => void,
    private you?: string | (() => string | undefined)) {}

  private viewer(): string | undefined { return typeof this.you === "function" ? this.you() : this.you; }

  // rowFor is the row of a call the page still holds, for a link to it.
  rowFor(callId: string): HTMLElement | undefined { return this.rows.get(this.calls.get(callId)?.seq ?? -1); }

  apply(ev: RoomEvent) {
    const p = ev.payload ?? {};
    const you = this.viewer();
    const put = (row: HTMLElement) => {
      if (plumbing(ev, you)) row.dataset.plumbing = "1";
      this.put(ev.seq, row);
    };
    switch (ev.type) {
      case "message":
        this.put(ev.seq, this.tools(bubble(ev, you), ev.seq));
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
            put(marker(ev));
          }
        } else {
          put(marker(ev));
        }
        break;
      case "turn":
        put(marker(ev));
        break;
      case "participant":
      case "driver":
        put(sysline(ev));
        break;
      case "handoff":
        this.put(ev.seq, this.tools(handoff(ev), ev.seq));
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
    const i = this.seqs.indexOf(seq);
    if (i < 0) return;
    this.detach(this.rows.get(seq)!);
    this.rows.delete(seq);
    this.seqs.splice(i, 1);
    this.regroup(i - 1);
    this.regroup(i);
  }

  // tools puts a "fork here" beside row, outside any summary, when forking is on.
  private tools(row: HTMLElement, seq: number): HTMLElement {
    const fork = this.fork;
    if (!fork) return row;
    const b = el("button", "fork-here", "fork here") as HTMLButtonElement;
    b.type = "button";
    b.dataset.act = "fork";
    b.title = "Branch a room of your own from the log up to here";
    b.setAttribute("aria-label", `fork here, at #${seq}`);
    b.onclick = () => fork(seq);
    const bar = el("div", "row-tools");
    bar.append(b);
    const wrap = el("div", "row");
    wrap.append(row, bar);
    return wrap;
  }

  // The fold around a row, if any; top is what sits in the stream for it.
  private foldOf(n: HTMLElement): HTMLElement | undefined {
    const parent = n.parentElement;
    return parent?.classList.contains("fold-body") ? parent.parentElement ?? undefined : undefined;
  }
  private top(n: HTMLElement): HTMLElement { return this.foldOf(n) ?? n; }
  private detach(n: HTMLElement) {
    const fold = this.foldOf(n);
    n.remove();
    if (fold) tidy(fold);
  }
  private isPlumbing(i: number): boolean { return this.rows.get(this.seqs[i])?.dataset.plumbing === "1"; }

  // regroup lays out the run of consecutive plumbing rows around index i: one fold for
  // two or more, the row alone for one. A fold that already holds just the run stays
  // (its open state with it); otherwise the run moves into a fresh one.
  private regroup(i: number) {
    if (i < 0 || i >= this.seqs.length || !this.isPlumbing(i)) return;
    let a = i, b = i;
    while (a > 0 && this.isPlumbing(a - 1)) a--;
    while (b < this.seqs.length - 1 && this.isPlumbing(b + 1)) b++;
    const items = this.seqs.slice(a, b + 1).map((s) => this.rows.get(s)!);
    const held = new Set<Element>(items);
    const after = b + 1 < this.seqs.length ? this.top(this.rows.get(this.seqs[b + 1])!) : null;
    const sources = new Set(items.map((n) => this.foldOf(n)).filter((f): f is HTMLElement => !!f));
    if (items.length === 1) {
      const fold = this.foldOf(items[0]);
      if (fold) {
        this.el.insertBefore(items[0], after);
        tidy(fold);
      }
      return;
    }
    let target = [...sources].find((f) => [...body(f).children].every((c) => held.has(c)));
    if (!target) {
      target = el("details", "fold");
      (target as HTMLDetailsElement).open = [...sources].some((f) => (f as HTMLDetailsElement).open);
      target.append(el("summary", ""), el("div", "fold-body"));
    }
    const inner = body(target);
    if (inner.children.length !== items.length || items.some((n, k) => inner.children[k] !== n)) inner.replaceChildren(...items);
    if (target.parentElement !== this.el || target.nextElementSibling !== after) this.el.insertBefore(target, after);
    for (const f of sources) if (f !== target) tidy(f);
    tidy(target);
  }

  // joinsLastFold appends row, the newest, to the fold holding the row before it, when
  // that row ends the fold: no regroup, which would walk the whole run (O(n²) on a replay).
  private joinsLastFold(i: number, row: HTMLElement): boolean {
    const prev = i > 0 ? this.rows.get(this.seqs[i - 1]) : undefined;
    const fold = prev && this.foldOf(prev);
    if (!fold || body(fold).lastElementChild !== prev) return false;
    body(fold).append(row);
    label(fold, fold.dataset.turn ?? row.dataset.turn);
    return true;
  }

  private put(seq: number, row: HTMLElement) {
    const old = this.rows.get(seq);
    if (old && old.dataset.plumbing === row.dataset.plumbing) {
      old.replaceWith(row); // inside its fold, if it is in one: a resend never doubles a line
      this.rows.set(seq, row);
      return;
    }
    if (old) this.drop(seq);
    let i = this.seqs.length;
    while (i > 0 && this.seqs[i - 1] > seq) i--; // from the end: live events land there
    const next = i < this.seqs.length ? this.top(this.rows.get(this.seqs[i])!) : null;
    this.seqs.splice(i, 0, seq);
    this.rows.set(seq, row);
    if (row.dataset.plumbing === "1") {
      // The live path: a plumbing row after a fold's last row joins it, in constant time.
      if (next !== null || !this.joinsLastFold(i, row)) {
        this.el.insertBefore(row, next);
        this.regroup(i);
      }
    } else {
      this.el.insertBefore(row, next);
      this.regroup(i - 1);
      this.regroup(i + 1);
    }
    while (this.seqs.length > this.cap) {
      const s = this.seqs.shift()!;
      const gone = this.rows.get(s)!;
      if (gone.dataset.callId) this.calls.delete(gone.dataset.callId);
      this.detach(gone);
      this.rows.delete(s);
      this.regroup(0);
    }
  }
}
