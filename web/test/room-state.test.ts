import { describe, expect, it } from "vitest";
import { maxRows } from "../src/limits";
import { RoomState } from "../src/room-state";

const ev = (seq: number, type: string, payload: unknown, causedBy?: number) =>
  ({ v: 1, id: String(seq), seq, roomId: "3kq7x2ma", actor: { kind: "human", id: "human:a" }, type, origin: "client",
     ts: "2026-09-27T10:00:00Z", redactions: [], payload, causedBy });

const agentEv = (seq: number, type: string, payload: unknown) =>
  ({ ...ev(seq, type, payload), actor: { kind: "agent", id: "agent:ikely2yk", role: "implementer" } });

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
  // R10: the header shows the room's phase. Active, Idle and AwaitingHuman never reach
  // the log (roomctrl derives them), so at or below the mark only the seal moves it: a
  // replayed room_phase{Open}, in most tails, must not undo the state frame's Active.
  it("follows the room's phase", () => {
    const s = new RoomState();
    s.reset({ driver: "human:a", driverEpoch: 1, phase: "Active" }, 40);
    expect(s.phase).toBe("Active");
    s.apply(ev(1, "state_changed", { kind: "room_phase", phase: "Open" })); // the replayed tail
    expect(s.phase).toBe("Active");
    s.apply(ev(39, "state_changed", { kind: "room_phase", phase: "Closed" })); // the Room lags the seal
    expect(s.phase).toBe("Closed");
    s.reset({ driver: "human:a", driverEpoch: 1, phase: "Open" }, 40);
    s.apply(ev(41, "state_changed", { kind: "room_phase", phase: "Closed", reason: "owner" }));
    expect(s.phase).toBe("Closed");
    s.apply(ev(42, "state_changed", { kind: "run_phase", phase: "Failed" })); // a run's, not the room's
    expect(s.phase).toBe("Closed");
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
  // §6: a card per pending approval, from the state frame, then the log past its mark.
  it("keeps the pending approvals", () => {
    const req = (seq: number, id: string) => ({ ...ev(seq, "approval_requested",
      { approvalId: id, callId: "c" + seq, class: "forge.pr", action: { command: "gh pr create" }, expiresAt: "2026-10-01T10:30:00Z" }), runId: "7f3cq2xz" });
    const s = new RoomState();
    s.reset({ driver: "system:factory", driverEpoch: 0, approvals: [{ approvalId: "old", runId: "7f3cq2xz", callId: "c1",
      class: "shell.high", action: {}, expiresAt: "2026-10-01T10:00:00Z", seq: 2 }] }, 600);
    s.apply(req(2, "old")); // the replay resends what the snapshot holds
    s.apply(req(500, "decided-before-the-mark")); // at or below the mark the snapshot decides
    s.apply(req(601, "new"));
    expect(s.approvals().map((a) => a.approvalId)).toEqual(["old", "new"]);
    expect(s.approvals()[1]).toEqual({ approvalId: "new", runId: "7f3cq2xz", callId: "c601", class: "forge.pr",
      action: { command: "gh pr create" }, expiresAt: "2026-10-01T10:30:00Z", seq: 601 });
    s.apply(ev(602, "approval_decided", { approvalId: "old", decision: "expired" }));
    expect(s.approvals().map((a) => a.approvalId)).toEqual(["new"]);
    s.reset({ driver: "system:factory", driverEpoch: 0 }, 700);
    expect(s.approvals()).toEqual([]);
  });

  it("tracks the latest activity: a running call, then its result, then a message", () => {
    const s = new RoomState();
    s.apply(agentEv(5, "tool_call", { callId: "c1", tool: "terminal", args: { command: "git status --short" } }));
    expect(s.latest()).toMatchObject({ seq: 5, text: "terminal git status --short", status: "running" });
    s.apply(agentEv(6, "tool_result", { callId: "c1", status: "ok", output: "" }));
    expect(s.latest()).toMatchObject({ seq: 5, status: "ok" });
    s.apply(agentEv(7, "message", { kind: "chat", text: "Two files changed.", delivery: "none" }));
    expect(s.latest()).toMatchObject({ seq: 7, text: "Two files changed.", status: "message" });
  });
  // Review focus 2: the page dropped the call's row at the 5,000-row cap, or its tail began between them.
  it("shows a result whose call it never saw as the result alone (review focus 2)", () => {
    const s = new RoomState();
    s.apply(agentEv(9, "tool_result", { callId: "lost", status: "error", output: "boom\nat line 2" }));
    expect(s.latest()).toMatchObject({ seq: 9, text: "boom", status: "error" });
    s.apply(agentEv(10, "tool_result", { status: "ok" })); // no callId, no output: still no throw
    expect(s.latest()).toMatchObject({ seq: 10, text: "tool result", status: "ok" });
  });
  it("names a finish by its message, not by its JSON", () => {
    const s = new RoomState();
    s.apply(agentEv(3, "tool_call", { callId: "c1", tool: "finish", args: { kind: "FinishAction", message: "Review of PR #148 complete\nmore" } }));
    expect(s.latest()?.text).toBe("finish Review of PR #148 complete");
  });
  it("names a call with no command by its args, and ignores system messages", () => {
    const s = new RoomState();
    s.apply(agentEv(3, "tool_call", { callId: "c2", tool: "edit", args: { path: "a.go" } }));
    expect(s.latest()?.text).toBe('edit {"path":"a.go"}');
    s.apply(agentEv(4, "tool_call", { callId: "c3", tool: "edit", args: { content: "x".repeat(500) } }));
    expect(s.latest()?.text).toHaveLength(120);
    for (const [seq, callId] of [[5, "c2"], [6, "c3"]] as const) s.apply(agentEv(seq, "tool_result", { callId, status: "ok" }));
    s.apply(ev(7, "message", { kind: "chat", text: "a human says", delivery: "none" }));
    expect(s.latest()).toMatchObject({ seq: 7, status: "message" });
    s.apply({ ...ev(8, "message", { kind: "chat", text: "broker note", delivery: "none" }), actor: { kind: "system", id: "system:room-broker" } });
    expect(s.latest()?.seq).toBe(7);
  });
  it("holds the Now line on a running call over a later message, until its result", () => {
    const s = new RoomState();
    s.apply(agentEv(1, "tool_call", { callId: "c1", tool: "terminal", args: { command: "go test ./..." } }));
    s.apply(ev(2, "message", { kind: "chat", text: "how is it going?", delivery: "none" }));
    expect(s.latest()).toMatchObject({ seq: 1, text: "terminal go test ./...", status: "running" });
    s.apply(agentEv(3, "tool_result", { callId: "c1", status: "error", output: "FAIL" }));
    expect(s.latest()).toMatchObject({ seq: 1, status: "error" }); // the result is the latest activity
    s.apply(agentEv(4, "message", { kind: "chat", text: "Tests fail.", delivery: "none" }));
    expect(s.latest()).toMatchObject({ seq: 4, status: "message" });
  });
  // A run that ends without its call's result: the call no longer holds the line as running.
  it.each([
    ["its agent leaves", { ...ev(3, "participant", { principal: "agent:ikely2yk", change: "left", role: "implementer" }),
      actor: { kind: "system", id: "system:room-broker" } }],
    ["its run phase ends", { ...ev(3, "state_changed", { kind: "run_phase", phase: "Failed", reason: "harness_error" }),
      runId: "ikely2yk", actor: { kind: "system", id: "system:room-broker" } }],
    ["the room seals", ev(3, "state_changed", { kind: "room_phase", phase: "Closed", reason: "owner" })],
  ])("releases an unfinished call when %s", (_, end) => {
    const s = new RoomState();
    s.apply({ ...agentEv(1, "tool_call", { callId: "c1", tool: "terminal", args: { command: "make" } }), runId: "ikely2yk" });
    s.apply(ev(2, "message", { kind: "chat", text: "still there?", delivery: "none" }));
    expect(s.latest()).toMatchObject({ seq: 1, status: "running" });
    s.apply(end);
    expect(s.latest()).toMatchObject({ seq: 2, status: "message" });
  });
  it("marks an unfinished call ended when it was the latest activity", () => {
    const s = new RoomState();
    s.apply({ ...agentEv(1, "tool_call", { callId: "c1", tool: "terminal", args: { command: "make" } }), runId: "ikely2yk" });
    s.apply({ ...agentEv(2, "tool_call", { callId: "c2", tool: "terminal", args: { command: "ls" } }), runId: "other123" });
    s.apply({ ...ev(3, "state_changed", { kind: "run_phase", phase: "Succeeded" }), runId: "ikely2yk" });
    expect(s.latest()).toMatchObject({ seq: 2, status: "running" }); // another run's call still holds it
    s.apply({ ...ev(4, "participant", { principal: "agent:other123", change: "left" }) });
    expect(s.latest()).toMatchObject({ seq: 2, text: "terminal ls", status: "ended" });
  });
  it("keeps the latest when a result lands for an older call", () => {
    const s = new RoomState();
    s.apply(agentEv(1, "tool_call", { callId: "a", tool: "terminal", args: { command: "make" } }));
    s.apply(agentEv(2, "tool_call", { callId: "b", tool: "terminal", args: { command: "ls" } }));
    s.apply(agentEv(3, "tool_result", { callId: "a", status: "rejected", output: "" }));
    expect(s.latest()).toMatchObject({ seq: 2, status: "running" });
    s.apply(agentEv(4, "tool_result", { callId: "b", status: "ok", output: "x" }));
    expect(s.latest()).toMatchObject({ seq: 2, status: "ok" });
  });
  it("records who closed the room and when, from the seal's own event", () => {
    const s = new RoomState();
    expect(s.closed).toBeUndefined();
    s.apply(ev(3, "state_changed", { kind: "room_phase", phase: "Closed", reason: "owner" }));
    expect(s.closed).toEqual({ who: "human:a", at: "2026-09-27T10:00:00Z" });
    expect(s.sealed).toBe(true);
    // A limit seals with no closer: sealed, but not closed by anyone.
    const l = new RoomState();
    l.apply(ev(4, "state_changed", { kind: "limit", events: 100000 }));
    expect([l.sealed, l.closed]).toEqual([true, undefined]);
  });

  it("lists the human members from the log's participant events", () => {
    const s = new RoomState();
    s.apply(ev(1, "participant", { principal: "human:b", change: "joined", role: "collaborator", approver: false }));
    s.apply(ev(2, "participant", { principal: "agent:ikely2yk", change: "joined", role: "implementer" }));
    s.apply(ev(3, "participant", { principal: "human:a", change: "joined", role: "owner", approver: true }));
    s.apply(ev(4, "participant", { principal: "human:b", change: "role_changed", role: "watcher", approver: false }));
    expect(s.members()).toEqual([{ principal: "human:a", role: "owner", approver: true }, { principal: "human:b", role: "watcher", approver: false }]);
    s.apply(ev(5, "participant", { principal: "human:a", change: "left" }));
    expect(s.members().map((m) => m.principal)).toEqual(["human:b"]);
  });

  // Q2: the agents present now, from joined and left only.
  it("knows the agents present from their joins and leaves, not from role changes", () => {
    const s = new RoomState();
    const part = (seq: number, principal: string, change: string) => ev(seq, "participant", { principal, change, role: "implementer" });
    expect([s.running(), s.ran()]).toEqual([false, false]);
    s.apply(part(1, "agent:a", "joined"));
    s.apply(part(2, "agent:b", "joined"));
    s.apply(part(3, "agent:a", "left"));
    expect(s.running()).toBe(true); // b is still here
    s.apply(part(4, "agent:b", "role_changed"));
    s.apply(part(5, "agent:b", "left"));
    s.apply(part(6, "agent:b", "role_changed"));
    expect([s.running(), s.ran()]).toEqual([false, true]);
    s.apply(part(7, "human:h", "joined"));
    expect(s.running()).toBe(false);
  });
  it("takes the agents present from the state frame's live runs, events past its mark on top", () => {
    const s = new RoomState();
    s.reset({ driver: "", driverEpoch: 0, runs: [{ id: "a", role: "implementer", phase: "Running" }, { id: "b", role: "reviewer", phase: "Succeeded" }] }, 10);
    expect([s.running(), s.ran()]).toEqual([true, true]);
    s.apply(ev(5, "participant", { principal: "agent:a", change: "left" })); // at or below the mark: the frame decides
    expect(s.running()).toBe(true);
    s.apply(ev(11, "participant", { principal: "agent:a", change: "left" }));
    expect(s.running()).toBe(false);
    s.reset({ driver: "", driverEpoch: 0, runs: [{ id: "b", role: "reviewer", phase: "Failed" }] }, 12);
    expect([s.running(), s.ran()]).toEqual([false, true]);
  });
  it("ends at a state frame the calls opened at or below its mark: a clamped replay may never resend their results", () => {
    const s = new RoomState();
    s.apply(agentEv(1, "tool_call", { callId: "c1", tool: "terminal", args: { command: "sleep 1" } }));
    s.reset({ driver: "", driverEpoch: 0 }, 5);
    expect(s.latest()?.status).toBe("ended");
    s.apply(agentEv(6, "tool_call", { callId: "c2", tool: "terminal", args: { command: "ls" } }));
    s.reset({ driver: "", driverEpoch: 0 }, 5); // a call past the mark stays open
    expect(s.latest()).toMatchObject({ seq: 6, status: "running" });
  });
  it("forgets who closed the room at a state frame that is not sealed", () => {
    const s = new RoomState();
    s.apply(ev(1, "state_changed", { kind: "room_phase", phase: "Closed" }));
    expect(s.closed).toBeDefined();
    s.reset({ driver: "", driverEpoch: 0, sealed: true }, 1);
    expect(s.closed).toBeDefined(); // a seal is for good: the closer still holds
    s.reset({ driver: "", driverEpoch: 0, sealed: false }, 1);
    expect(s.closed).toBeUndefined();
  });
  it("lets a call that leaves at the cap no longer hold the Now line", () => {
    const s = new RoomState();
    for (let i = 0; i <= maxRows; i++) s.apply(agentEv(i + 1, "tool_call", { callId: `c${i}`, tool: "t", args: { command: String(i) } }));
    for (let i = 1; i <= maxRows; i++) s.apply(agentEv(maxRows + 1 + i, "tool_result", { callId: `c${i}`, status: "ok" }));
    expect(s.latest()).toMatchObject({ text: `t ${maxRows}`, status: "ok" }); // c0 went at the cap: it holds nothing
  });

  it("knows the runs that are not sandboxed, from the state frame and the log", () => {
    const s = new RoomState();
    s.reset({ driver: "", driverEpoch: 0, runs: [{ id: "b2b3c4d5", role: "implementer", phase: "Running", unsandboxed: true },
      { id: "c2b3c4d5", role: "tester", phase: "Running" }] }, 1);
    expect(s.unsandboxed()).toEqual(["b2b3c4d5"]);
    s.apply({ ...ev(2, "state_changed", { kind: "run_phase", phase: "Running", unsandboxed: true }), runId: "a2b3c4d5" });
    s.apply({ ...ev(3, "state_changed", { kind: "run_phase", phase: "Running" }), runId: "d2b3c4d5" });
    expect(s.unsandboxed()).toEqual(["a2b3c4d5", "b2b3c4d5"]);
  });
});
