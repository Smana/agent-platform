import { describe, expect, it } from "vitest";
import type { RoomEvent } from "../src/conn";
import { Replay, type Clock } from "../preview/replay";

// fakeClock runs timers when the test advances it, in due order: no real time passes.
function fakeClock() {
  let now = 0;
  let id = 0;
  const timers = new Map<number, { at: number; fn: () => void }>();
  const clock: Clock = {
    setTimer: (fn, ms) => { timers.set(++id, { at: now + ms, fn }); return id; },
    clearTimer: (t) => { timers.delete(t as number); },
  };
  const advance = (ms: number) => {
    const end = now + ms;
    for (;;) {
      const due = [...timers.entries()].filter(([, t]) => t.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
      if (!due) break;
      timers.delete(due[0]);
      now = due[1].at;
      due[1].fn();
    }
    now = end;
  };
  return { clock, advance, pending: () => timers.size };
}

// ev is an event recorded `s` seconds into the run.
const ev = (seq: number, s: number): RoomEvent => ({ v: 1, id: `e${seq}`, seq, roomId: "r", actor: { kind: "agent", id: "agent:x" },
  type: "turn", origin: "harness", ts: new Date(Date.UTC(2026, 9, 18, 7, 0, 0) + s * 1000).toISOString(), redactions: [], payload: {} });

function setup(seconds: number[], speed = 1, maxGapMs = 10_000) {
  const { clock, advance, pending } = fakeClock();
  const seen: number[] = [];
  let changes = 0;
  const r = new Replay(seconds.map((s, i) => ev(i + 1, s)), (e) => seen.push(e.seq), { speed, maxGapMs, clock, onChange: () => changes++ });
  return { r, seen, advance, pending, changes: () => changes };
}

describe("Replay", () => {
  it("applies the first event at once, then each after its recorded gap", () => {
    const t = setup([0, 2, 5]);
    t.r.play();
    t.advance(0);
    expect(t.seen).toEqual([1]);
    t.advance(1999);
    expect(t.seen).toEqual([1]);
    t.advance(1);
    expect(t.seen).toEqual([1, 2]);
    t.advance(3000);
    expect(t.seen).toEqual([1, 2, 3]);
    expect([t.r.done, t.r.playing, t.pending()]).toEqual([true, false, 0]);
  });
  it("divides each gap by the speed", () => {
    const t = setup([0, 8], 8);
    t.r.play();
    t.advance(999);
    expect(t.seen).toEqual([1]);
    t.advance(1);
    expect(t.seen).toEqual([1, 2]);
  });
  it("caps a long idle gap before the speed applies", () => {
    const t = setup([0, 600], 2, 10_000);
    t.r.play();
    t.advance(5000);
    expect(t.seen).toEqual([1, 2]);
  });
  it("treats an out-of-order timestamp as no gap", () => {
    const t = setup([5, 1]);
    t.r.play();
    t.advance(0);
    expect(t.seen).toEqual([1, 2]);
  });
  it("pauses with nothing pending and resumes with the next gap", () => {
    const t = setup([0, 2, 4]);
    t.r.play();
    t.advance(2000);
    t.r.pause();
    expect([t.r.playing, t.pending()]).toEqual([false, 0]);
    t.advance(60_000);
    expect(t.seen).toEqual([1, 2]);
    t.r.play();
    t.advance(2000);
    expect(t.seen).toEqual([1, 2, 3]);
  });
  it("restarts a wait under way at a new speed", () => {
    const t = setup([0, 10]);
    t.r.play();
    t.advance(1000);
    t.r.setSpeed(10); // the 10 s gap is now 1 s, counted from the change
    t.advance(999);
    expect(t.seen).toEqual([1]);
    t.advance(1);
    expect(t.seen).toEqual([1, 2]);
  });
  it("refuses a speed that is not positive", () => {
    expect(() => setup([0]).r.setSpeed(0)).toThrow(RangeError);
  });
  it("skips to the end in order and stays done", () => {
    const t = setup([0, 30, 60]);
    t.r.play();
    t.advance(0);
    t.r.skip();
    expect(t.seen).toEqual([1, 2, 3]);
    expect([t.r.done, t.r.playing, t.pending()]).toEqual([true, false, 0]);
    t.r.play(); // a finished replay does not restart
    t.advance(60_000);
    expect(t.seen).toEqual([1, 2, 3]);
  });
  it("tells its listener after every event and every control", () => {
    const t = setup([0, 1]);
    t.r.play(); // 1
    t.advance(0); // 2
    t.r.pause(); // 3
    t.r.setSpeed(4); // 4
    expect(t.changes()).toBe(4);
  });
});
