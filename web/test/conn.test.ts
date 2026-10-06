import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RoomConnection, type RoomEvent } from "../src/conn";
import { FakeSocket } from "./fakes";

const event = (seq: number) => ({ type: "event", event: { seq, type: "message", payload: {} } });

function setup(random = () => 0) {
  const sockets: FakeSocket[] = [];
  const seen: number[] = [];
  const status: string[] = [];
  const counted: string[] = [];
  const conn = new RoomConnection("3kq7x2ma", {
    onEvent: (e: RoomEvent) => seen.push(e.seq),
    onState: () => {},
    onStatus: (s) => status.push(s),
    onCounters: (n) => counted.push(`${n.last}/${n.gaps}/${n.duplicates}`),
  }, { socket: (url) => { const s = new FakeSocket(url); sockets.push(s); return s; }, random });
  conn.connect();
  const last = () => sockets[sockets.length - 1];
  return { conn, sockets, seen, status, counted, last };
}

// Closes a socket n times in a row without letting it live, and returns the waits.
// opened: each socket opens first, as one the broker drops at once (slow_consumer).
function waits(c: ReturnType<typeof setup>, n: number, code = 1006, opened = false): number[] {
  const out: number[] = [];
  for (let i = 0; i < n; i++) {
    const before = c.sockets.length;
    if (opened) c.last().open();
    c.last().drop(code);
    let waited = 0;
    while (c.sockets.length === before) { vi.advanceTimersByTime(100); waited += 100; }
    out.push(waited);
  }
  return out;
}

describe("RoomConnection", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("says hello with a tail first, then resumes from the last seq it saw", () => {
    const c = setup();
    expect(c.last().url).toBe("ws://localhost:3000/v1/ws?room=3kq7x2ma");
    c.last().open();
    expect(c.last().sent[0]).toEqual({ type: "hello", roomId: "3kq7x2ma", tail: 500 });
    c.last().recv({ type: "sync", fromSeq: 1, throughSeq: 3 });
    [1, 2, 3].forEach((s) => c.last().recv(event(s)));
    expect(c.seen).toEqual([1, 2, 3]);
    c.last().drop(1008, "slow_consumer: resume from afterSeq");
    expect(c.status.at(-1)).toContain("slow_consumer");
    vi.advanceTimersByTime(499);
    expect(c.sockets).toHaveLength(1);
    vi.advanceTimersByTime(1);
    c.last().open();
    expect(c.last().sent[0]).toEqual({ type: "hello", roomId: "3kq7x2ma", afterSeq: 3 });
  });

  // A write_timeout or a refused upgrade reaches the browser as a bare 1006.
  it("backs off, doubling to a 30 s bound, while the connection keeps failing", () => {
    const c = setup();
    expect(waits(c, 9)).toEqual([500, 1000, 2000, 4000, 8000, 16000, 30000, 30000, 30000]);
    expect(c.status.at(-1)).toContain("connection lost");
  });

  it("keeps the bound with the most jitter", () => {
    const c = setup(() => 0.999);
    expect(Math.max(...waits(c, 9))).toBe(30000);
  });

  // An open is no proof of health: a viewer the broker drops at once must not re-dial
  // every 500 ms.
  it("keeps backing off when each connection opens and is dropped at once", () => {
    const c = setup();
    expect(waits(c, 4, 1008, true)).toEqual([500, 1000, 2000, 4000]);
  });

  it("starts over after a connection that lived, as a lifetime close does", () => {
    const c = setup();
    waits(c, 4);
    c.last().open();
    vi.advanceTimersByTime(60 * 60 * 1000);
    expect(waits(c, 1, 4001)).toEqual([500]);
    expect(c.status.at(-1)).toContain("reauth");
  });

  it("follows a resume the broker clamped to its mark", () => {
    const c = setup();
    c.last().open();
    c.last().recv({ type: "sync", fromSeq: 48, throughSeq: 50 });
    [48, 49, 50].forEach((s) => c.last().recv(event(s)));
    c.last().drop(1001, "shutdown");
    vi.advanceTimersByTime(500);
    c.last().open();
    expect(c.last().sent[0]).toMatchObject({ afterSeq: 50 });
    c.last().recv({ type: "sync", fromSeq: 41, throughSeq: 40 }); // the room's mark is 40
    c.last().recv(event(41));
    expect(c.seen.at(-1)).toBe(41);
    expect(c.conn.counters()).toEqual({ last: 41, gaps: 0, duplicates: 0 });
  });

  it("closes on a gap and resumes from the last contiguous seq", () => {
    const c = setup();
    c.last().open();
    c.last().recv({ type: "sync", fromSeq: 1, throughSeq: 1 });
    c.last().recv(event(1));
    c.last().recv(event(3));
    expect(c.seen).toEqual([1]); // 3 never reaches the page ahead of 2 (review I2)
    expect(c.conn.counters()).toEqual({ last: 1, gaps: 1, duplicates: 0 });
    vi.advanceTimersByTime(500);
    expect(c.sockets).toHaveLength(2);
    c.last().open();
    expect(c.last().sent[0]).toMatchObject({ afterSeq: 1 });
  });

  it("keeps a duplicate off the page, and the footer's counters current", () => {
    const c = setup();
    c.last().open();
    c.last().recv({ type: "sync", fromSeq: 1, throughSeq: 2 });
    [1, 2, 2].forEach((s) => c.last().recv(event(s)));
    expect(c.seen).toEqual([1, 2]);
    expect(c.counted.at(-1)).toBe("2/0/1"); // refreshed on the duplicate itself (review M1)
  });

  // A resume answered past the seq sent skipped events: resume again (review I1).
  it("closes on a sync past what it holds, and resumes from it", () => {
    const c = setup();
    c.last().open();
    c.last().recv({ type: "sync", fromSeq: 1, throughSeq: 30 });
    for (let s = 1; s <= 30; s++) c.last().recv(event(s));
    c.last().drop(1001, "shutdown");
    vi.advanceTimersByTime(500);
    c.last().open();
    c.last().recv({ type: "sync", fromSeq: 41, throughSeq: 40 });
    expect(c.conn.counters()).toEqual({ last: 30, gaps: 1, duplicates: 0 });
    expect(c.counted.at(-1)).toBe("30/1/0");
    vi.advanceTimersByTime(1000);
    expect(c.sockets).toHaveLength(3); // the sync itself closed the socket
    c.last().open();
    expect(c.last().sent[0]).toMatchObject({ afterSeq: 30 });
  });

  it("pings every 30 s while open, and stops when closed", () => {
    const c = setup();
    const first = c.last();
    first.open();
    vi.advanceTimersByTime(30_000);
    expect(first.sent.at(-1)).toEqual({ type: "ping" });
    first.drop(1006);
    const n = first.sent.length;
    vi.advanceTimersByTime(90_000);
    expect(first.sent).toHaveLength(n);
  });

  it("ignores a frame that is not JSON", () => {
    const c = setup();
    c.last().open();
    expect(() => c.last().onmessage?.({ data: "{" })).not.toThrow();
  });

  it("sends only on an open socket, and says so", () => {
    const c = setup();
    expect(c.conn.send({ type: "act" })).toBe(false);
    c.last().open();
    expect(c.conn.send({ type: "act", clientSeq: 1 })).toBe(true);
    expect(c.last().sent.at(-1)).toEqual({ type: "act", clientSeq: 1 });
    c.last().drop(1006);
    expect(c.conn.send({ type: "act" })).toBe(false);
  });

  // A refused upgrade (an expired session among others) closes before it opens.
  it("reports a socket that closed before it opened", () => {
    const refused: number[] = [];
    const sockets: FakeSocket[] = [];
    const conn = new RoomConnection("3kq7x2ma", { onEvent: () => {}, onState: () => {}, onStatus: () => {},
      onRefused: () => refused.push(sockets.length) }, { socket: (url) => { const s = new FakeSocket(url); sockets.push(s); return s; }, random: () => 0 });
    conn.connect();
    sockets[0].drop(1006);
    expect(refused).toEqual([1]);
    vi.advanceTimersByTime(500);
    sockets[1].open();
    sockets[1].drop(1006);
    expect(refused).toEqual([1]);
  });
});
