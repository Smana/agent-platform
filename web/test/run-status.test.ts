import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { statusLine, type StatusIn } from "../src/run-status";

// The cases the Go side runs too (internal/summary/runstatus_test.go): one wording, two renderers.
const cases = JSON.parse(readFileSync(join(__dirname, "../../internal/summary/runstatus_cases.json"), "utf8")) as
  { name: string; in: StatusIn & { now?: string }; want: ReturnType<typeof statusLine> }[];

describe("the run status", () => {
  for (const c of cases) {
    it(c.name, () => {
      const i = { ...c.in, now: c.in.now ? Date.parse(c.in.now) : undefined };
      expect(statusLine(i)).toEqual(c.want);
    });
  }
  it("never carries a word an agent wrote", () => {
    const s = statusLine({ state: "failed", phase: "Rejected", reasonCode: "__proto__" });
    expect(s.words).toBe("Failed — rejected");
  });
});
