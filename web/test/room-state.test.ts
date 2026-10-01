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
  it("dequeues only on a steering message's causedBy (review 4.5 M2)", () => {
    const s = new RoomState();
    s.apply(ev(2, "message", { kind: "chat", text: "two", delivery: "queued" }));
    s.apply(ev(3, "message", { kind: "chat", text: "a reply", delivery: "none" }, 2));
    expect(s.queue().map(q => q.ref)).toEqual([2]);
  });
  // The page's tail is 500 events; the state frame's queue reaches further (review 4.5 I1).
  it("starts from the state frame's queue, then follows events past its mark", () => {
    const s = new RoomState();
    s.reset({ driver: "human:a", driverEpoch: 3, queue: [{ ref: 2, author: "human:b", text: "after this run" }] }, 600);
    for (let seq = 101; seq <= 600; seq++) s.apply(ev(seq, "tool_call", { tool: "terminal" }));
    expect(s.queue()).toEqual([{ ref: 2, author: "human:b", text: "after this run" }]);
    // At or below the mark the snapshot decides: a message refused at the limit stays unqueued (M3).
    s.apply(ev(599, "message", { kind: "chat", text: "refused at the limit", delivery: "queued" }));
    s.apply(ev(601, "message", { kind: "chat", text: "new", delivery: "queued" }));
    s.apply(ev(602, "state_changed", { kind: "queued_removed", ref: 2 }));
    expect(s.queue().map(q => q.ref)).toEqual([601]);
    // A reconnect's state frame replaces what the page held.
    s.reset({ driver: "human:a", driverEpoch: 3, queue: [] }, 700);
    expect(s.queue()).toEqual([]);
  });
  it("knows a sealed room from its state frame or its seal event", () => {
    const s = new RoomState();
    s.reset({ driver: "", driverEpoch: 0, sealed: true }, 1);
    expect(s.sealed).toBe(true);
    for (const payload of [{ kind: "room_phase", phase: "Closed", reason: "done" }, { kind: "limit", events: 100000, bytes: 9 }]) {
      const t = new RoomState();
      t.apply(ev(1, "state_changed", { kind: "limit", reason: "concurrent_run", running: "a2b3c4d5" }));
      t.apply(ev(2, "state_changed", { kind: "room_phase", phase: "Open" }));
      expect(t.sealed).toBe(false);
      t.apply(ev(3, "state_changed", payload));
      expect(t.sealed).toBe(true);
    }
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
