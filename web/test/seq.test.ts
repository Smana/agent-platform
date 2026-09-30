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
});
