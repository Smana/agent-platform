// SPDX-License-Identifier: Apache-2.0

import { SeqTracker } from "./seq";

export interface RoomEvent {
  v: number; id: string; seq: number; roomId: string; runId?: string;
  actor: { kind: string; id: string; role?: string }; type: string; causedBy?: number;
  origin: string; ts: string; redactions: string[]; payload: any;
}
export interface Snapshot {
  roomId: string; phase: string; driver: string; driverEpoch: number; dataClass: string;
  you: { principal: string; role: string; approver: boolean; driver: boolean; webUI: boolean };
  runs?: { id: string; role: string; phase: string }[];
  queue?: { ref: number; author: string; text: string }[];
  sealed?: boolean;
  approvals?: { approvalId: string; runId: string; callId: string; class: string; action: unknown; expiresAt: string; seq: number }[];
}
export interface Frame { type: string; throughSeq?: number; fromSeq?: number; snapshot?: Snapshot; event?: RoomEvent;
  clientSeq?: number; seq?: number; rejected?: string; result?: unknown }

export interface Handlers {
  onEvent(e: RoomEvent): void;
  onState(s: Snapshot, throughSeq: number): void;
  onStatus(s: string): void;
  onAck?(f: Frame): void;
  // A socket that closed before it opened: a refused upgrade, an expired session among them.
  onRefused?(): void;
  // After every sync and event frame, delivered or not, so the footer is never stale.
  onCounters?(c: Counters): void;
}

export interface Counters { last: number; gaps: number; duplicates: number }

// The part of a WebSocket the connection uses, so tests can drive it.
export interface SocketLike {
  onopen: (() => void) | null;
  onmessage: ((m: { data: unknown }) => void) | null;
  onclose: ((e: { code: number; reason: string }) => void) | null;
  send(data: string): void;
  close(code?: number, reason?: string): void;
}

export interface Options {
  socket?: (url: string) => SocketLike;
  random?: () => number;
}

const firstWait = 500;
const maxWait = 30_000;
// A connection that lived this long was healthy: the next failure starts the backoff over.
const settled = 30_000;
const pingEvery = 30_000;
const tail = 500;

// Why the broker closed, for the status line (docs/api.md). A write_timeout, a ping
// timeout and a refused upgrade all end without a close frame: 1006.
function why(e: { code: number; reason: string }): string {
  if (e.reason) return e.reason;
  if (e.code === 4001) return "reauth";
  return e.code === 1006 ? "connection lost" : `closed ${e.code}`;
}

export class RoomConnection {
  private ws?: SocketLike;
  private wait = firstWait;
  private openedAt = 0;
  private open = false;
  private pinger?: ReturnType<typeof setInterval>;
  private retry?: ReturnType<typeof setTimeout>;
  private stopped = false;
  private readonly socket: (url: string) => SocketLike;
  private readonly random: () => number;
  readonly tracker = new SeqTracker();

  constructor(private roomId: string, private h: Handlers, o: Options = {}) {
    this.socket = o.socket ?? ((url) => new WebSocket(url) as unknown as SocketLike);
    this.random = o.random ?? Math.random;
  }

  connect() {
    const proto = location.protocol === "https:" ? "wss://" : "ws://";
    const ws = this.socket(`${proto}${location.host}/v1/ws?room=${encodeURIComponent(this.roomId)}`);
    this.ws = ws;
    this.openedAt = 0;
    ws.onopen = () => {
      this.openedAt = Date.now();
      this.open = true;
      const hello: Record<string, unknown> = { type: "hello", roomId: this.roomId };
      if (this.tracker.last > 0) hello.afterSeq = this.tracker.last; else hello.tail = tail;
      ws.send(JSON.stringify(hello));
      this.pinger = setInterval(() => ws.send(JSON.stringify({ type: "ping" })), pingEvery);
      this.h.onStatus("live");
    };
    ws.onmessage = (m) => {
      let f: Frame;
      try { f = JSON.parse(String(m.data)); } catch { return; }
      this.frame(f);
    };
    ws.onclose = (e) => {
      clearInterval(this.pinger);
      this.open = false;
      if (!this.stopped && this.openedAt === 0) this.h.onRefused?.();
      if (this.stopped) return; // by onRefused, among others
      if (this.openedAt > 0 && Date.now() - this.openedAt >= settled) this.wait = firstWait;
      // Jitter spreads a replica's viewers when it shuts down (1001) and they all re-dial.
      const delay = Math.min(this.wait + Math.floor(this.wait * 0.25 * this.random()), maxWait);
      this.wait = Math.min(this.wait * 2, maxWait);
      this.h.onStatus(`reconnecting in ${Math.ceil(delay / 1000)} s: ${why(e)}`);
      this.retry = setTimeout(() => this.connect(), delay);
    };
  }

  // send reports whether the frame went out: an act sent while reconnecting is not queued.
  send(frame: Record<string, unknown>): boolean {
    if (!this.open || !this.ws) return false;
    this.ws.send(JSON.stringify(frame));
    return true;
  }

  // stop ends the connection for good: no re-dial, a scheduled one included.
  stop() {
    this.stopped = true;
    clearTimeout(this.retry);
    this.ws?.close();
  }

  // resync drops the socket: the reconnect's state frame re-reads the room.
  resync() { this.ws?.close(1000, "membership changed"); }

  counters(): Counters { return { last: this.tracker.last, gaps: this.tracker.gaps, duplicates: this.tracker.duplicates }; }

  private frame(f: Frame) {
    switch (f.type) {
      case "state":
        if (f.snapshot) this.h.onState(f.snapshot, f.throughSeq ?? 0);
        break;
      case "sync": // before any event, and before a live gap's range: the broker's baseline
        if (typeof f.fromSeq !== "number") break;
        if (!this.tracker.baseline(f.fromSeq)) this.ws?.close(); // skipped events: resume
        this.h.onCounters?.(this.counters());
        break;
      case "event": {
        if (!f.event) break;
        const verdict = this.tracker.observe(f.event.seq);
        if (verdict === "next") this.h.onEvent(f.event);
        else if (verdict === "gap") this.ws?.close(); // resume from the last contiguous seq
        this.h.onCounters?.(this.counters());
        break;
      }
      case "ack":
        this.h.onAck?.(f);
        break;
    }
  }
}
