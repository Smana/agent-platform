// SPDX-License-Identifier: Apache-2.0

import type { RoomEvent } from "./conn";
import { maxRows } from "./limits";

export interface Queued { ref: number; author: string; text: string }
// The latest activity, for the strip's Now line: a tool call (running until its result
// says ok, error or rejected, or ended when its run ended first), a result alone, or an
// agent's or a human's message.
export interface Latest { seq: number; text: string; status: "running" | "ok" | "error" | "rejected" | "ended" | "message"; ts: string }
// A pending approval: its card. action is the raw, redacted call (T3).
// A human member as the log's participant events last named them.
export interface Member { principal: string; role: string; approver: boolean }
export interface Approval { approvalId: string; runId: string; callId: string; class: string; action: unknown; expiresAt: string; seq: number }

// What the controls need: the state frame's projection of the log (driver, queue,
// seal), then the log's events past its mark (§1).
export class RoomState {
  private queued = new Map<number, Queued>();
  private pending = new Map<string, Approval>();
  private through = 0;
  private last?: Latest;
  private lastAt = 0; // the seq of the event that set last: a result's, for a call's entry
  // The calls still open to a result, by callId, oldest first, at most maxCalls of them,
  // each with the principal of the run that owns it (agent:<runId>).
  private calls = new Map<string, { l: Latest; owner: string }>();
  private members_ = new Map<string, Member>();
  // The agents present now (Q2): agent principals that joined and have not left. A
  // role change neither starts nor ends a run. ran_ says one ever was, in what the page saw.
  private agents = new Set<string>();
  private ran_ = false;
  // Whether the latest state frame named the runs: at or below its mark, it decides who is present.
  private runsAtMark = false;
  // Runs the controller placed without gVisor (their Unsandboxed condition): from the state
  // frame's runs and from a Running run_phase that says so. Never cleared: the page keeps saying it.
  private unsandboxed_ = new Set<string>();
  unsandboxed(): string[] { return [...this.unsandboxed_].sort(); }
  sealed = false;
  // Who closed the room and when: the close's own event. A limit seals with no closer.
  closed?: { who: string; at: string };
  phase = "";
  constructor(public driver = "", public driverEpoch = 0) {}

  // reset takes a state frame. Its queue is the store's at the mark: a page's tail
  // of 500 events rarely reaches back to what was queued for the next run.
  reset(s: { driver: string; driverEpoch: number; phase?: string; queue?: Queued[]; sealed?: boolean; approvals?: Approval[];
    runs?: { id: string; role?: string; phase: string; unsandboxed?: boolean }[] }, throughSeq: number) {
    this.driver = s.driver;
    this.phase = s.phase ?? "";
    this.driverEpoch = s.driverEpoch;
    this.sealed = !!s.sealed;
    this.queued = new Map((s.queue ?? []).map((q) => [q.ref, { ref: q.ref, author: q.author, text: q.text }]));
    this.pending = new Map((s.approvals ?? []).map((a) => [a.approvalId, { ...a }]));
    this.through = throughSeq;
    // A seal is for good: who closed it still holds while the frame says sealed.
    if (!this.sealed) this.closed = undefined;
    // A clamped replay may never resend the results of calls at or below the mark: they end.
    this.release((c) => c.l.seq <= throughSeq);
    this.runsAtMark = s.runs !== undefined;
    if (s.runs) {
      for (const r of s.runs) if (r.unsandboxed) this.unsandboxed_.add(r.id);
      this.agents = new Set(s.runs.filter((r) => !terminal.has(r.phase)).map((r) => `agent:${r.id}`));
      this.ran_ ||= s.runs.length > 0;
    }
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
          this.calls.set(p.callId, { l: call, owner: ev.runId ? `agent:${ev.runId}` : ev.actor.id });
          if (this.calls.size > maxCalls) { // gone from the page: no result will reach its row
            const [id, oldest] = this.calls.entries().next().value!;
            oldest.l.status = "ended";
            this.calls.delete(id);
          }
        }
        break;
      }
      case "tool_result": {
        const status = resultStatus(p.status);
        const call = typeof p.callId === "string" ? this.calls.get(p.callId)?.l : undefined;
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
        if (p.kind === "run_phase" && p.unsandboxed === true && ev.runId) this.unsandboxed_.add(ev.runId);
        if (p.kind === "queued_removed") this.queued.delete(p.ref);
        // Only what the brief quoted is consumed; a reviewer's request consumes nothing.
        if (p.kind === "run_requested" && Array.isArray(p.consumed)) for (const ref of p.consumed) this.queued.delete(ref);
        // Active, Idle and AwaitingHuman never reach the log (roomctrl derives them), so at
        // or below the mark only the seal beats the state frame's phase, which lags it.
        if (p.kind === "room_phase" && typeof p.phase === "string" && (ev.seq > this.through || p.phase === "Closed")) {
          this.phase = p.phase;
        }
        // The seals (internal/store sealTx): a close, or the room's event or byte limit.
        if (p.kind === "room_phase" && p.phase === "Closed") this.closed = { who: ev.actor.id, at: ev.ts };
        if ((p.kind === "room_phase" && p.phase === "Closed") || (p.kind === "limit" && p.events !== undefined)) {
          this.sealed = true;
          this.release(() => true);
        }
        // A run's terminal phase (runwatch: run_phase <terminal>, then its agent leaves).
        if (p.kind === "run_phase" && p.phase !== "Running" && ev.runId) this.release((c) => c.owner === `agent:${ev.runId}`);
        break;
      case "participant":
        if (p.change === "left" && typeof p.principal === "string") this.release((c) => c.owner === p.principal);
        if (typeof p.principal === "string" && p.principal.startsWith("agent:") && !(this.runsAtMark && ev.seq <= this.through)) {
          if (p.change === "joined") {
            this.agents.add(p.principal);
            this.ran_ = true;
          } else if (p.change === "left") this.agents.delete(p.principal);
        }
        // The members list is humans only: a run's agent is in the strip and the stream.
        if (typeof p.principal === "string" && p.principal.startsWith("human:")) {
          if (p.change === "left") this.members_.delete(p.principal);
          else this.members_.set(p.principal, { principal: p.principal, role: String(p.role ?? ""), approver: p.approver === true });
        }
        break;
    }
  }

  // running is whether an agent is present now; ran, whether one ever was.
  running(): boolean { return this.agents.size > 0; }
  ran(): boolean { return this.ran_; }

  members(): Member[] { return [...this.members_.values()].sort((a, b) => a.principal.localeCompare(b.principal)); }

  queue(): Queued[] { return [...this.queued.values()].sort((a, b) => a.ref - b.ref); }

  // latest is the newest call still running, which holds the line over any later
  // message until its result lands; with none running, the latest activity.
  latest(): Latest | undefined {
    let running: Latest | undefined;
    for (const { l } of this.calls.values()) if (!running || l.seq > running.seq) running = l;
    const l = running ?? this.last;
    return l && { ...l };
  }

  // release ends the open calls that match: a run that ended before its result leaves
  // the call ended, so it no longer holds the Now line as running.
  private release(match: (c: { l: Latest; owner: string }) => boolean) {
    for (const [id, c] of this.calls) {
      if (!match(c)) continue;
      c.l.status = "ended";
      this.calls.delete(id);
    }
  }

  // set makes l the latest activity unless a resend of an older event replays it.
  private set(l: Latest, at: number) {
    if (at < this.lastAt) return;
    this.last = l;
    this.lastAt = at;
  }

  approvals(): Approval[] { return [...this.pending.values()].sort((a, b) => a.seq - b.seq); }
}

// A run's terminal phases (internal/runwatch Terminal): its agent is gone.
const terminal = new Set(["Succeeded", "Failed", "BudgetExhausted", "Revoked"]);

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
