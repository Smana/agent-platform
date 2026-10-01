// SPDX-License-Identifier: Apache-2.0

import type { RoomEvent } from "./conn";

export interface Queued { ref: number; author: string; text: string }

// What the controls need: the state frame's projection of the log (driver, queue,
// seal), then the log's events past its mark (§1).
export class RoomState {
  private queued = new Map<number, Queued>();
  private through = 0;
  sealed = false;
  constructor(public driver = "", public driverEpoch = 0) {}

  // reset takes a state frame. Its queue is the store's at the mark: a page's tail
  // of 500 events rarely reaches back to what was queued for the next run.
  reset(s: { driver: string; driverEpoch: number; queue?: Queued[]; sealed?: boolean }, throughSeq: number) {
    this.driver = s.driver;
    this.driverEpoch = s.driverEpoch;
    this.sealed = !!s.sealed;
    this.queued = new Map((s.queue ?? []).map((q) => [q.ref, { ref: q.ref, author: q.author, text: q.text }]));
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
      case "message":
        // At or below the mark, the snapshot's queue decides: a message refused at
        // the room's event limit stays in the log, unqueued.
        if (p.delivery === "queued" && ev.seq > this.through) {
          this.queued.set(ev.seq, { ref: ev.seq, author: ev.actor.id, text: String(p.text ?? "") });
        }
        if (p.delivery === "steering" && ev.causedBy) this.queued.delete(ev.causedBy); // promoted
        break;
      case "state_changed":
        if (p.kind === "queued_removed") this.queued.delete(p.ref);
        // Only what the brief quoted is consumed; a reviewer's request consumes nothing.
        if (p.kind === "run_requested" && Array.isArray(p.consumed)) for (const ref of p.consumed) this.queued.delete(ref);
        // The seals (internal/store sealTx): a close, or the room's event or byte limit.
        if ((p.kind === "room_phase" && p.phase === "Closed") || (p.kind === "limit" && p.events !== undefined)) this.sealed = true;
        break;
    }
  }

  queue(): Queued[] { return [...this.queued.values()].sort((a, b) => a.ref - b.ref); }
}
