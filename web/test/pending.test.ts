import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RoomConnection, type SocketLike } from "../src/conn";
import { lostAct, lostRun, PendingActs } from "../src/pending";

class FakeSocket implements SocketLike {
  onopen: SocketLike["onopen"] = null;
  onmessage: SocketLike["onmessage"] = null;
  onclose: SocketLike["onclose"] = null;
  send() {}
  close(code = 1000, reason = "") { this.onclose?.({ code, reason }); }
  open() { this.onopen?.(); }
  recv(frame: unknown) { this.onmessage?.({ data: JSON.stringify(frame) }); }
}

// Wired as main.ts wires it: acks and status through the connection's handlers.
function setup() {
  const notices: string[] = [];
  const sockets: FakeSocket[] = [];
  const pending = new PendingActs((t) => notices.push(t));
  const conn = new RoomConnection("3kq7x2ma", {
    onEvent: () => {}, onState: () => {},
    onAck: (f) => pending.ack(f),
    onStatus: (s) => pending.status(s),
  }, { socket: () => { const s = new FakeSocket(); sockets.push(s); return s; }, random: () => 0 });
  conn.connect();
  sockets[0].open();
  const act = (clientSeq: number, kind: string) => pending.send(conn, { type: "act", clientSeq, action: { kind } });
  return { notices, sockets, act };
}

describe("PendingActs (review 4.5 I2)", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("says a hand to role's claim may be lost when the socket drops before its ack", () => {
    const c = setup();
    expect(c.act(1, "message")).toBe(true);
    expect(c.act(2, "start_run")).toBe(true);
    c.sockets[0].recv({ type: "ack", clientSeq: 1, seq: 40 });
    c.sockets[0].close(1006);
    expect(c.notices).toEqual([lostRun]);
    expect(lostRun).toMatch(/claim was lost/);
    expect(lostRun).toMatch(/room_busy for up to 10 minutes/);
    // Said once: the next reconnect attempt has nothing pending.
    vi.advanceTimersByTime(500);
    c.sockets[1].close(1006);
    expect(c.notices).toHaveLength(1);
  });

  it("says an ordinary act may not have landed", () => {
    const c = setup();
    c.act(1, "message");
    c.sockets[0].close(1001, "shutdown");
    expect(c.notices).toEqual([lostAct]);
  });

  it("stays quiet when every act was answered, or none went out", () => {
    const c = setup();
    c.act(1, "start_run");
    c.sockets[0].recv({ type: "ack", clientSeq: 1, rejected: "room_busy" });
    c.sockets[0].close(1006);
    vi.advanceTimersByTime(500);
    expect(c.act(2, "start_run")).toBe(false); // not open: never pending
    c.sockets[1].close(1006);
    expect(c.notices).toEqual([]);
  });
});
