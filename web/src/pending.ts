// SPDX-License-Identifier: Apache-2.0

import type { Frame } from "./conn";
import type { Sender } from "./controls";

export const lostAct = "The connection dropped before the broker answered: check the log before retrying.";
export const lostRun = "The connection dropped before the broker answered a hand to role. If a run_requested appears in the log, " +
  "its claim was lost, and the room answers room_busy for up to 10 minutes.";

// PendingActs remembers the acts the broker has not answered. A reconnect is a new
// idempotency scope, so an unanswered act is never acked: say so, or a lost
// start_run leaves the owner with no claim and a busy room (review 4.5 I2).
export class PendingActs {
  private acts = new Map<number, string>();
  constructor(private say: (text: string) => void) {}

  send(conn: Sender, frame: Record<string, unknown>): boolean {
    const sent = conn.send(frame);
    const action = frame.action as { kind?: unknown } | undefined;
    if (sent && typeof frame.clientSeq === "number") this.acts.set(frame.clientSeq, String(action?.kind ?? ""));
    return sent;
  }

  ack(f: Frame) { if (f.clientSeq) this.acts.delete(f.clientSeq); }

  status(s: string) {
    if (s === "live" || this.acts.size === 0) return;
    const run = [...this.acts.values()].includes("start_run");
    this.acts.clear();
    this.say(run ? lostRun : lostAct);
  }
}
