// SPDX-License-Identifier: Apache-2.0

// A step's summary: what the tool calls an agent ran after one of its messages add up
// to, as the one line a reader scans (`Ran 3 commands · 1 rejected · 0:15`), and the
// state that decides whether the step opens itself. A tally, not a list: chat.ts counts
// each row in as it lands and out as it leaves, so a step of thousands of calls costs
// constant time per event (a rescan per event made a 5,000-event replay take a minute).

export interface StepCall {
  tool: string;
  status: string; // "running" until the result lands, else the result's status
  start: string; // the call's ts
  end?: string; // the result's ts
}

export interface StepTally {
  calls: number;
  running: number;
  rejected: number;
  failed: number; // error, or any status the page does not know
  tools: number; // calls to a tool other than the terminal
  quiet: number; // plumbing rows: they count toward no call
  start: number; // ms; the earliest call's start
  end: number; // ms; the latest result's end
}

export const emptyTally = (): StepTally =>
  ({ calls: 0, running: 0, rejected: 0, failed: 0, tools: 0, quiet: 0, start: Infinity, end: -Infinity });

// count adds a call (sign 1) or takes one away (sign -1); undefined is a plumbing row.
// Taking a call away leaves start and end: a bound cannot be undone without a rescan,
// and the one removal on the hot path, a call's row replaced when its result lands,
// keeps its start and only moves its end later.
export function count(t: StepTally, c: StepCall | undefined, sign: 1 | -1) {
  if (!c) {
    t.quiet += sign;
    return;
  }
  t.calls += sign;
  if (c.status === "running") t.running += sign;
  else if (c.status === "rejected") t.rejected += sign;
  else if (c.status !== "ok") t.failed += sign;
  if (c.tool !== "terminal") t.tools += sign;
  if (sign === 1) {
    t.start = Math.min(t.start, Date.parse(c.start));
    if (c.end) t.end = Math.max(t.end, Date.parse(c.end));
  }
}

// running: a call has no result yet; failed: one errored or was rejected; done: all ok.
export type StepState = "running" | "failed" | "done";

export function stepState(t: StepTally): StepState {
  if (t.running > 0) return "running";
  if (t.rejected + t.failed > 0) return "failed";
  return "done";
}

export interface StepLine {
  head: string; // "Running 2 commands", "Ran 1 tool call", "3 changes" for plumbing alone
  exceptions: string; // "1 rejected · 2 failed", or ""
  duration: string; // "0:15", or "" for one call or none finished
}

export function stepLine(t: StepTally): StepLine {
  if (t.calls === 0) return { head: `${t.quiet} ${t.quiet === 1 ? "change" : "changes"}`, exceptions: "", duration: "" };
  const one = t.calls === 1;
  const noun = t.tools === 0 ? (one ? "command" : "commands") : (one ? "tool call" : "tool calls");
  const exceptions = [[t.rejected, "rejected"], [t.failed, "failed"]].filter(([k]) => (k as number) > 0)
    .map(([k, word]) => `${k} ${word}`).join(" · ");
  return {
    head: `${t.running > 0 ? "Running" : "Ran"} ${t.calls} ${noun}`,
    exceptions,
    duration: one || t.end < t.start ? "" : span(t.end - t.start),
  };
}

// span is a duration in ms as m:ss.
function span(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}
