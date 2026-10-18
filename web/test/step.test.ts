import { describe, expect, it } from "vitest";
import { count, emptyTally, stepLine, stepState, type StepCall, type StepTally } from "../src/step";

const c = (status: string, start: string, end?: string, tool = "terminal"): StepCall => ({ tool, status, start, end });
const t = (s: number) => `2026-10-18T07:54:${String(s).padStart(2, "0")}Z`;
const tally = (...calls: (StepCall | undefined)[]): StepTally => {
  const x = emptyTally();
  for (const k of calls) count(x, k, 1);
  return x;
};

describe("stepState", () => {
  it("is running while any call has no result, whatever failed before", () => {
    expect(stepState(tally(c("error", t(1), t(2)), c("running", t(3))))).toBe("running");
  });
  it("is failed once every call ended and one errored or was rejected", () => {
    expect(stepState(tally(c("ok", t(1), t(2)), c("rejected", t(3), t(4))))).toBe("failed");
    expect(stepState(tally(c("error", t(1), t(2))))).toBe("failed");
  });
  it("is done when every call is ok, or the step holds plumbing alone", () => {
    expect(stepState(tally(c("ok", t(1), t(2)), c("ok", t(3), t(4))))).toBe("done");
    expect(stepState(tally(undefined))).toBe("done");
  });
});

describe("stepLine", () => {
  it("counts commands while running, with no duration until one ends", () => {
    expect(stepLine(tally(c("running", t(1)), c("running", t(2))))).toEqual({ head: "Running 2 commands", exceptions: "", duration: "" });
  });
  it("names a single call in the singular and gives it no duration", () => {
    expect(stepLine(tally(c("ok", t(1), t(9))))).toEqual({ head: "Ran 1 command", exceptions: "", duration: "" });
  });
  it("says tool calls once any call is not the terminal", () => {
    expect(stepLine(tally(c("ok", t(1), t(2)), c("ok", t(3), t(4), "room_read"))).head).toBe("Ran 2 tool calls");
  });
  it("names rejected then failed calls, and spans first start to last end as m:ss", () => {
    const line = stepLine(tally(c("ok", t(1), t(2)), c("error", t(3), t(4)), c("rejected", t(5), t(16))));
    expect(line).toEqual({ head: "Ran 3 commands", exceptions: "1 rejected · 1 failed", duration: "0:15" });
  });
  it("rolls seconds over into minutes", () => {
    expect(stepLine(tally(c("ok", "2026-10-18T07:54:00Z", t(1)), c("ok", t(2), "2026-10-18T07:56:05Z"))).duration).toBe("2:05");
  });
  it("counts plumbing alone as changes", () => {
    expect(stepLine(tally(undefined, undefined)).head).toBe("2 changes");
    expect(stepLine(tally(undefined)).head).toBe("1 change");
  });
});

describe("count", () => {
  it("takes a replaced call out and its result in, the way a landing result does", () => {
    const x = tally(c("running", t(1)));
    count(x, c("running", t(1)), -1);
    count(x, c("rejected", t(1), t(4)), 1);
    expect(x).toMatchObject({ calls: 1, running: 0, rejected: 1 });
    expect(stepLine(x).head).toBe("Ran 1 command");
  });
});
