// SPDX-License-Identifier: Apache-2.0

// The demo's player: a recorded run's events, applied at the pace they were recorded,
// sped up. A long idle gap is capped, so a demo never stalls on a wait the visitor cannot
// see the reason for. The clock is injected: tests drive it, the page passes the window's.

import type { RoomEvent } from "../src/conn";

export interface Clock {
  setTimer(fn: () => void, ms: number): unknown;
  clearTimer(id: unknown): void;
}

export const windowClock: Clock = {
  setTimer: (fn, ms) => setTimeout(fn, ms),
  clearTimer: (id) => clearTimeout(id as ReturnType<typeof setTimeout>),
};

export interface ReplayOptions {
  speed?: number; // 8 by default: a twenty-minute run plays in under three
  maxGapMs?: number; // the longest recorded gap honoured, before speed: 10 s
  clock?: Clock;
  // dwell is a real-time pause after an event, in ms: neither capped nor sped up, so a moment the
  // visitor must see (an approval waiting) stays on screen at any speed. Skip ignores it.
  dwell?: (e: RoomEvent) => number;
  onChange?: () => void; // after every applied event and every play, pause, speed or skip
}

export class Replay {
  private next = 0; // the index of the next event to apply
  private timer: unknown;
  private speedNow: number;
  private readonly maxGapMs: number;
  private readonly clock: Clock;
  private readonly dwell: (e: RoomEvent) => number;
  private readonly onChange: () => void;

  constructor(private readonly events: RoomEvent[], private readonly apply: (e: RoomEvent) => void, o: ReplayOptions = {}) {
    this.speedNow = o.speed ?? 8;
    this.maxGapMs = o.maxGapMs ?? 10_000;
    this.clock = o.clock ?? windowClock;
    this.dwell = o.dwell ?? (() => 0);
    this.onChange = o.onChange ?? (() => {});
  }

  get playing(): boolean { return this.timer !== undefined; }
  get done(): boolean { return this.next >= this.events.length; }
  get speed(): number { return this.speedNow; }
  get applied(): number { return this.next; }

  // play applies the first event now and schedules the rest; a finished replay stays done.
  play() {
    if (this.playing || this.done) return;
    this.schedule(this.next === 0 ? 0 : this.wait(this.next));
    this.onChange();
  }

  pause() {
    if (!this.playing) return;
    this.clock.clearTimer(this.timer);
    this.timer = undefined;
    this.onChange();
  }

  // setSpeed takes effect from the next wait; a wait under way restarts at the new speed.
  setSpeed(speed: number) {
    if (!(speed > 0)) throw new RangeError(`speed must be positive, got ${speed}`);
    this.speedNow = speed;
    if (this.playing) {
      this.clock.clearTimer(this.timer);
      this.timer = undefined;
      this.schedule(this.wait(this.next));
    }
    this.onChange();
  }

  // skip applies every remaining event now, in order, and stops.
  skip() {
    if (this.playing) this.clock.clearTimer(this.timer);
    this.timer = undefined;
    while (!this.done) this.apply(this.events[this.next++]);
    this.onChange();
  }

  // wait is the pause before event i: its recorded gap, capped, then sped up, plus the previous
  // event's dwell.
  private wait(i: number): number {
    if (i === 0) return 0;
    const gap = Date.parse(this.events[i].ts) - Date.parse(this.events[i - 1].ts);
    return Math.min(Math.max(gap, 0), this.maxGapMs) / this.speedNow + this.dwell(this.events[i - 1]);
  }

  private schedule(ms: number) {
    this.timer = this.clock.setTimer(() => {
      this.timer = undefined;
      this.apply(this.events[this.next++]);
      if (!this.done) this.schedule(this.wait(this.next));
      this.onChange();
    }, ms);
  }
}
