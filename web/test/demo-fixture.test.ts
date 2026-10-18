import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { ChatView } from "../src/chat";
import type { RoomEvent } from "../src/conn";

const dir = join(process.cwd(), "test/fixtures");
const events: RoomEvent[] = readFileSync(join(dir, "demo-session.jsonl"), "utf8").trim().split("\n").map((l) => JSON.parse(l));
const types = (t: string) => events.filter((e) => e.type === t);

describe("the demo run", () => {
  it("is a gapless seq order from 1, 80 to 120 events, one room", () => {
    expect(events.length).toBeGreaterThanOrEqual(80);
    expect(events.length).toBeLessThanOrEqual(120);
    events.forEach((e, i) => expect(e.seq).toBe(i + 1));
    expect(new Set(events.map((e) => e.roomId)).size).toBe(1);
  });
  it("carries every envelope field, with timestamps that never go back", () => {
    for (const e of events) {
      expect([e.v, typeof e.id, typeof e.actor?.id, typeof e.type, typeof e.ts, Array.isArray(e.redactions)]).toEqual([1, "string", "string", "string", "string", true]);
      expect(Number.isNaN(Date.parse(e.ts))).toBe(false);
    }
    for (let i = 1; i < events.length; i++) expect(Date.parse(events[i].ts)).toBeGreaterThanOrEqual(Date.parse(events[i - 1].ts));
  });
  it("spans 12 to 20 minutes, with at least one idle gap over 10 s", () => {
    const span = (Date.parse(events.at(-1)!.ts) - Date.parse(events[0].ts)) / 60_000;
    expect(span).toBeGreaterThanOrEqual(12);
    expect(span).toBeLessThanOrEqual(20);
    expect(events.some((e, i) => i > 0 && Date.parse(e.ts) - Date.parse(events[i - 1].ts) > 10_000)).toBe(true);
  });
  it("tells the story: an error, a denial, a steer, an approval, a handoff and a verdict", () => {
    expect(types("tool_result").some((e) => e.payload.status === "error")).toBe(true);
    expect(types("state_changed").some((e) => e.payload.kind === "policy_decision" && e.payload.decision === "deny")).toBe(true);
    expect(types("message").some((e) => e.actor.kind === "human" && e.payload.delivery === "steering")).toBe(true);
    expect(types("approval_requested")).toHaveLength(1);
    expect(types("approval_decided")).toHaveLength(1);
    expect(types("handoff")).toHaveLength(1);
    expect(types("message").some((e) => e.payload.kind === "review_verdict")).toBe(true);
    expect(types("tool_call").some((e) => e.payload.tool !== "terminal")).toBe(true);
  });
  it("names no real person, repository or secret", () => {
    const text = JSON.stringify(events);
    expect(text).not.toMatch(/ogenki|smana|github_pat_|ghp_|ghs_|sk-[A-Za-z0-9]{8}/i);
    for (const e of events) if (e.actor.kind === "human") expect(e.actor.id).toMatch(/^human:\d{12}$/);
  });
  it("renders in the chat view with every call paired and no unknown row", () => {
    const el = document.createElement("div");
    const v = new ChatView(el);
    for (const e of events) v.apply(e);
    expect(el.querySelectorAll("details.tool")).toHaveLength(types("tool_call").length);
    expect(el.querySelectorAll("details.tool.st-running")).toHaveLength(0); // the run ended: every call has its result
    expect(el.querySelectorAll(".step").length).toBeGreaterThanOrEqual(4);
  });
  it("comes with a snapshot and a summary for the same room", () => {
    const snap = JSON.parse(readFileSync(join(dir, "demo-snapshot.json"), "utf8"));
    const summary = JSON.parse(readFileSync(join(dir, "demo-summary.json"), "utf8"));
    expect(snap.roomId).toBe(events[0].roomId);
    expect(summary.room).toBe(events[0].roomId);
    expect(snap.you.approver).toBe(true);
  });
});
