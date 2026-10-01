import { describe, expect, it } from "vitest";
import { RoomState } from "../src/room-state";

const ev = (seq: number, type: string, payload: unknown, causedBy?: number) =>
  ({ v: 1, id: String(seq), seq, roomId: "3kq7x2ma", actor: { kind: "human", id: "human:a" }, type, origin: "client",
     ts: "2026-09-27T10:00:00Z", redactions: [], payload, causedBy });

describe("RoomState", () => {
  it("derives the queue from the log", () => {
    const s = new RoomState();
    s.apply(ev(1, "message", { kind: "chat", text: "one", delivery: "queued" }));
    s.apply(ev(2, "message", { kind: "chat", text: "two", delivery: "queued" }));
    s.apply(ev(3, "message", { kind: "chat", text: "three", delivery: "queued" }));
    s.apply(ev(4, "state_changed", { kind: "queued_removed", ref: 1 }));
    s.apply(ev(5, "message", { kind: "chat", text: "two", delivery: "steering", to: ["agent:x"] }, 2));
    expect(s.queue().map(q => q.ref)).toEqual([3]);
    // run_requested names the refs its brief consumed (Task 4.4), not a count.
    s.apply(ev(6, "state_changed", { kind: "run_requested", role: "implementer", runId: "a2b3c4d5", consumed: [3] }));
    expect(s.queue()).toEqual([]);
  });
  it("keeps what a brief did not quote, and a reviewer's request consumes nothing", () => {
    const s = new RoomState();
    for (const seq of [1, 2, 3]) s.apply(ev(seq, "message", { kind: "chat", text: `m${seq}`, delivery: "queued" }));
    s.apply(ev(4, "state_changed", { kind: "run_requested", role: "reviewer", runId: "a2b3c4d5", consumed: [] }));
    s.apply(ev(5, "state_changed", { kind: "run_requested", role: "implementer", runId: "b2b3c4d5", consumed: [1, 2] }));
    expect(s.queue()).toEqual([{ ref: 3, author: "human:a", text: "m3" }]);
  });
  it("follows the driver token", () => {
    const s = new RoomState("system:factory", 7);
    s.apply(ev(1, "driver", { from: "system:factory", to: "human:a", epoch: 8, reason: "requested" }));
    expect([s.driver, s.driverEpoch]).toEqual(["human:a", 8]);
  });
  // The replay after a state frame resends driver events the snapshot already holds.
  it("never moves the token back to an older epoch", () => {
    const s = new RoomState("human:b", 9);
    s.apply(ev(1, "driver", { from: "system:factory", to: "human:a", epoch: 8, reason: "requested" }));
    expect([s.driver, s.driverEpoch]).toEqual(["human:b", 9]);
  });
});
