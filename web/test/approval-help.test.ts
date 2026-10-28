import { describe, expect, it } from "vitest";
import { bannerText, deadlineText, decideStatus, EXPIRY, RULES, ruleFor } from "../src/approval-help";

const at = (h: number, m: number, day = 28) => new Date(2026, 9, day, h, m, 0);

describe("rule prose", () => {
  it("covers every broker class with fixed intent, why, approve and deny prose", () => {
    expect(Object.keys(RULES).sort()).toEqual(["egress.new", "forge.other", "forge.pr", "forge.push", "mcp.write", "shell.high"]);
    for (const r of Object.values(RULES)) for (const k of ["intent", "why", "approve", "deny"] as const) expect(r[k].length).toBeGreaterThan(10);
  });
  it("reads an unknown or hostile code as the generic rule, never echoing it", () => {
    for (const c of ["", "made.up", "__proto__", "<img src=x>"]) {
      const r = ruleFor(c);
      expect(JSON.stringify(r)).not.toContain(c || "\u0000");
      expect(r.intent).toBe("Take an action the room's policy gates.");
    }
  });
});

describe("deadlineText", () => {
  const now = at(10, 0).getTime();
  it("gives the clock time, the minutes left and what happens on silence", () => {
    expect(deadlineText(at(10, 30).toISOString(), now)).toBe(`Decide by 10:30 (in 30 min); if nobody decides: ${EXPIRY}.`);
  });
  it("counts hours, and says under a minute", () => {
    expect(deadlineText(at(12, 5).toISOString(), now)).toContain("(in 2 h 5 min)");
    expect(deadlineText(new Date(now + 20_000).toISOString(), now)).toContain("(in under a minute)");
  });
  it("shows the date of another day's deadline", () => {
    expect(deadlineText(at(9, 5, 29).toISOString(), now)).toContain("Decide by 2026-10-29 09:05 (in 23 h 5 min)");
  });
  it("says when it has passed, and when there is none", () => {
    expect(deadlineText(at(9, 50).toISOString(), now)).toBe(`The deadline has passed (09:50); ${EXPIRY}.`);
    for (const x of ["", "garbage", "0001-01-01T00:00:00Z"]) expect(deadlineText(x, now)).toBe("No deadline.");
  });
});

describe("decideStatus", () => {
  const you = { principal: "human:a", role: "collaborator", approver: true };
  it("lets an approver or an owner decide", () => {
    expect(decideStatus({ you })).toEqual({ can: true, text: "You can decide." });
    expect(decideStatus({ you: { ...you, approver: false, role: "owner" } }).can).toBe(true);
  });
  it("says only approvers decide to anyone else", () => {
    const s = decideStatus({ you: { ...you, approver: false } });
    expect(s.can).toBe(false);
    expect(s.text).toContain("only approvers decide");
    expect(decideStatus({}).can).toBe(false);
  });
  it("explains four-eyes to the prompter, and only under four-eyes", () => {
    expect(decideStatus({ you, fourEyes: true, prompters: ["human:a"] })).toEqual({ can: false,
      text: "Waiting for another approver: you prompted this turn (four-eyes)." });
    expect(decideStatus({ you, fourEyes: false, prompters: ["human:a"] }).can).toBe(true);
    expect(decideStatus({ you, fourEyes: true, prompters: ["human:z"] }).can).toBe(true);
    expect(decideStatus({ you, fourEyes: true }).can).toBe(true); // an older broker names no prompters
  });
  it("reads a sealed room as read-only", () => {
    expect(decideStatus({ you, sealed: true }).can).toBe(false);
  });
});

describe("bannerText", () => {
  it("counts what waits for the viewer, else for another approver, else nothing", () => {
    expect(bannerText(1, 1)).toBe("⏳ 1 approval waits for you");
    expect(bannerText(3, 2)).toBe("⏳ 2 approvals wait for you");
    expect(bannerText(2, 0)).toBe("⏳ 2 approvals wait for another approver");
    expect(bannerText(1, 0)).toBe("⏳ 1 approval waits for another approver");
    expect(bannerText(0, 0)).toBe("");
  });
});
