import { describe, expect, it } from "vitest";
import { SeqTracker } from "../src/seq";

describe("SeqTracker (the SC-2 client check)", () => {
  it("counts gaps and duplicates", () => {
    const t = new SeqTracker(4);
    expect(t.observe(5)).toBe("next");
    expect(t.observe(5)).toBe("duplicate");
    expect(t.observe(7)).toBe("gap");
    expect(t.last).toBe(5); // a gap does not advance: the client resyncs from 5
    expect([t.gaps, t.duplicates]).toEqual([1, 1]);
  });
  it("takes its baseline from the first sync", () => {
    const t = new SeqTracker();
    t.baseline(8);
    expect(t.observe(8)).toBe("next");
  });
  it("takes a clamped resume's baseline too: the broker's mark wins", () => {
    const t = new SeqTracker(50); // resumed with afterSeq 50, the room's mark is 40
    t.baseline(41);
    expect(t.observe(41)).toBe("next");
    expect([t.gaps, t.duplicates]).toEqual([0, 0]);
  });
  // The broker only clamps a resume down, and a live gap's range starts at last + 1:
  // a baseline past that skipped events, and is a gap (review I1).
  it("counts a baseline past what it holds as a gap, and keeps its last", () => {
    const t = new SeqTracker(30); // resumed with afterSeq 30, the room's mark is 40
    expect(t.baseline(41)).toBe(false);
    expect([t.last, t.gaps]).toEqual([30, 1]);
    expect(t.baseline(31)).toBe(true);
    expect(t.observe(31)).toBe("next");
  });
});
