import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import type { RoomEvent } from "../src/conn";
import type { Summary } from "../src/summary";
import { summaryAt } from "../preview/demo-summary";

const dir = join(process.cwd(), "test/fixtures");
const events: RoomEvent[] = readFileSync(join(dir, "demo-session.jsonl"), "utf8").trim().split("\n").map((l) => JSON.parse(l));
const final: Summary = JSON.parse(readFileSync(join(dir, "demo-summary.json"), "utf8"));
const upTo = (pred: (e: RoomEvent) => boolean) => events.slice(0, events.findIndex(pred) + 1);
const at = (shown: RoomEvent[]) => summaryAt(final, shown, events.length, false);

describe("summaryAt", () => {
  it("gives nothing of the ending away at the start", () => {
    const s = at(events.slice(0, 1));
    expect([s.status.phase, s.status.run, s.status.pr, s.status.lastVerdict, s.needsYou, s.notes.items]).toEqual(["Implementing", null, null, null, [], []]);
  });
  it("names the running run, and the pull request once it is opened", () => {
    const running = at(upTo((e) => e.payload.kind === "run_phase"));
    expect(running.status.run).toEqual({ id: "w4hn8c2r", role: "implementer" });
    expect(running.status.pr).toBeNull();
    const opened = at(upTo((e) => e.type === "tool_result" && String(e.payload.output).includes("/pull/214")));
    expect(opened.status.pr).toEqual(final.status.pr);
  });
  it("moves to Reviewing on the handoff, and to the recorded phase on the verdict", () => {
    const reviewing = at(upTo((e) => e.type === "handoff"));
    expect(reviewing.status.phase).toBe("Reviewing");
    const decided = at(upTo((e) => e.payload.kind === "review_verdict"));
    expect([decided.status.phase, decided.status.lastVerdict?.verdict, decided.needsYou]).toEqual([final.status.phase, "approve", final.needsYou]);
  });
  it("lists the notes shown so far, stamped as shown", () => {
    const shown = upTo((e) => e.payload.kind === "progress").map((e) => ({ ...e, ts: "2030-01-01T00:00:00.000Z" }));
    expect(at(shown).notes.items).toEqual([{ at: "2030-01-01T00:00:00.000Z", run: "w4hn8c2r", text: shown.at(-1)!.payload.text }]);
  });
  it("is the recorded summary once done, with the shown notes", () => {
    const s = summaryAt(final, events, events.length, true);
    expect({ ...s, notes: final.notes }).toEqual(final);
    expect(s.notes.items).toHaveLength(final.notes.items.length);
  });
});
