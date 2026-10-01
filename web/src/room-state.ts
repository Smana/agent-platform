// SPDX-License-Identifier: Apache-2.0

import type { RoomEvent } from "./conn";

export interface Queued { ref: number; author: string; text: string }

// What the controls need, derived from the log alone: the log is the truth (§1).
export class RoomState {
  private queued = new Map<number, Queued>();
  constructor(public driver = "", public driverEpoch = 0) {}

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
        if (p.delivery === "queued") this.queued.set(ev.seq, { ref: ev.seq, author: ev.actor.id, text: String(p.text ?? "") });
        if (p.delivery === "steering" && ev.causedBy) this.queued.delete(ev.causedBy); // promoted
        break;
      case "state_changed":
        if (p.kind === "queued_removed") this.queued.delete(p.ref);
        // Only what the brief quoted is consumed; a reviewer's request consumes nothing.
        if (p.kind === "run_requested" && Array.isArray(p.consumed)) for (const ref of p.consumed) this.queued.delete(ref);
        break;
    }
  }

  queue(): Queued[] { return [...this.queued.values()].sort((a, b) => a.ref - b.ref); }
}
