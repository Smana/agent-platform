// SPDX-License-Identifier: Apache-2.0

import type { RoomEvent } from "./conn";
import { maxRows } from "./view";

export interface Queued { ref: number; author: string; text: string }
// The latest activity, for the strip's Now line: a tool call (running until its result
// says ok, error or rejected), a result alone, or an agent's or a human's message.
export interface Latest { seq: number; text: string; status: "running" | "ok" | "error" | "rejected" | "message"; ts: string }
// A pending approval: its card. action is the raw, redacted call (T3).
export interface Approval { approvalId: string; runId: string; callId: string; class: string; action: unknown; expiresAt: string; seq: number }

// What the controls need: the state frame's projection of the log (driver, queue,
// seal), then the log's events past its mark (§1).
export class RoomState {
  private queued = new Map<number, Queued>();
  private pending = new Map<string, Approval>();
  private through = 0;
  private last?: Latest;
  private lastAt = 0; // the seq of the event that set last: a result's, for a call's entry
  // The calls still open to a result, by callId, oldest first, at most maxCalls of them.
  private calls = new Map<string, Latest>();
  sealed = false;
  phase = "";
  constructor(public driver = "", public driverEpoch = 0) {}

  // reset takes a state frame. Its queue is the store's at the mark: a page's tail
  // of 500 events rarely reaches back to what was queued for the next run.
  reset(s: { driver: string; driverEpoch: number; phase?: string; queue?: Queued[]; sealed?: boolean; approvals?: Approval[] },
    throughSeq: number) {
    this.driver = s.driver;
    this.phase = s.phase ?? "";
    this.driverEpoch = s.driverEpoch;
    this.sealed = !!s.sealed;
    this.queued = new Map((s.queue ?? []).map((q) => [q.ref, { ref: q.ref, author: q.author, text: q.text }]));
    this.pending = new Map((s.approvals ?? []).map((a) => [a.approvalId, { ...a }]));
    this.through = throughSeq;
  }

  apply(ev: RoomEvent) {
    const p = ev.payload ?? {};
    switch (ev.type) {
      case "driver":
        // Epochs only grow: the replay after a state frame resends changes the
        // snapshot already holds, and must not move the token back.
        if (typeof p.epoch === "number" && p.epoch > this.driverEpoch) {
          this.driver = String(p.to ?? "");
          this.driverEpoch = p.epoch;
        }
        break;
      case "tool_call": {
        const args = p.args;
        const what = args?.command !== undefined ? String(args.command) : args === undefined ? "" : JSON.stringify(args);
        const call: Latest = { seq: ev.seq, text: firstLine(`${String(p.tool ?? "")} ${what}`), status: "running", ts: ev.ts };
        this.set(call, ev.seq);
        if (typeof p.callId === "string") {
          this.calls.delete(p.callId); // a resent call moves to the newest end
          this.calls.set(p.callId, call);
          if (this.calls.size > maxCalls) this.calls.delete(this.calls.keys().next().value!);
        }
        break;
      }
      case "tool_result": {
        const status = resultStatus(p.status);
        const call = typeof p.callId === "string" ? this.calls.get(p.callId) : undefined;
        // The result is the latest activity, shown as its call's line with the result's
        // status. A result whose call the page never saw (its tail began after it, or the
        // row went at the cap) stands alone, its output's first line as the text.
        if (call) {
          call.status = status;
          this.calls.delete(p.callId);
          this.set(call, ev.seq);
        } else {
          this.set({ seq: ev.seq, text: firstLine(String(p.output ?? "")) || "tool result", status, ts: ev.ts }, ev.seq);
        }
        break;
      }
      case "message":
        if (ev.actor.kind === "agent" || ev.actor.kind === "human") {
          this.set({ seq: ev.seq, text: firstLine(String(p.text ?? "")), status: "message", ts: ev.ts }, ev.seq);
        }
        // At or below the mark, the snapshot's queue decides: a message refused at
        // the room's event limit stays in the log, unqueued.
        if (p.delivery === "queued" && ev.seq > this.through) {
          this.queued.set(ev.seq, { ref: ev.seq, author: ev.actor.id, text: String(p.text ?? "") });
        }
        if (p.delivery === "steering" && ev.causedBy) this.queued.delete(ev.causedBy); // promoted
        break;
      case "approval_requested":
        // At or below the mark the snapshot decides: it holds the request unless it was decided.
        if (ev.seq > this.through && typeof p.approvalId === "string") {
          this.pending.set(p.approvalId, { approvalId: p.approvalId, runId: ev.runId ?? "", callId: String(p.callId ?? ""),
            class: String(p.class ?? ""), action: p.action, expiresAt: String(p.expiresAt ?? ""), seq: ev.seq });
        }
        break;
      case "approval_decided":
        this.pending.delete(p.approvalId);
        break;
      case "state_changed":
        if (p.kind === "queued_removed") this.queued.delete(p.ref);
        // Only what the brief quoted is consumed; a reviewer's request consumes nothing.
        if (p.kind === "run_requested" && Array.isArray(p.consumed)) for (const ref of p.consumed) this.queued.delete(ref);
        // Active, Idle and AwaitingHuman never reach the log (roomctrl derives them), so at
        // or below the mark only the seal beats the state frame's phase, which lags it.
        if (p.kind === "room_phase" && typeof p.phase === "string" && (ev.seq > this.through || p.phase === "Closed")) {
          this.phase = p.phase;
        }
        // The seals (internal/store sealTx): a close, or the room's event or byte limit.
        if ((p.kind === "room_phase" && p.phase === "Closed") || (p.kind === "limit" && p.events !== undefined)) this.sealed = true;
        break;
    }
  }

  queue(): Queued[] { return [...this.queued.values()].sort((a, b) => a.ref - b.ref); }

  // latest is the newest call still running, which holds the line over any later
  // message until its result lands; with none running, the latest activity.
  latest(): Latest | undefined {
    let running: Latest | undefined;
    for (const c of this.calls.values()) if (!running || c.seq > running.seq) running = c;
    const l = running ?? this.last;
    return l && { ...l };
  }

  // set makes l the latest activity unless a resend of an older event replays it.
  private set(l: Latest, at: number) {
    if (at < this.lastAt) return;
    this.last = l;
    this.lastAt = at;
  }

  approvals(): Approval[] { return [...this.pending.values()].sort((a, b) => a.seq - b.seq); }
}

const maxCalls = maxRows; // the chat's row cap: an open call older than that is gone from the page too

// firstLine keeps the line short: an edit's args can be a whole file on one line.
function firstLine(s: string): string {
  const l = s.trim().split("\n", 1)[0].trim();
  return l.length > maxText ? `${l.slice(0, maxText - 1)}…` : l;
}
const maxText = 120;

function resultStatus(s: unknown): Latest["status"] {
  return s === "error" || s === "rejected" ? s : "ok";
}
