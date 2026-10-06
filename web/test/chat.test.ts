import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { ChatView } from "../src/chat";
import type { RoomEvent } from "../src/conn";

const ev = (seq: number, type: string, payload: unknown, actor: { kind: string; id: string; role?: string } = { kind: "agent", id: "agent:ikely2yk", role: "implementer" }): RoomEvent => ({
  v: 1, id: `id${seq}`, seq, roomId: "3kq7x2ma", runId: "ikely2yk", actor,
  type, origin: "harness", ts: `2026-10-04T14:00:${String(seq).padStart(2, "0")}Z`, redactions: [], payload,
});

// Every element the chat view may build; anything else in the output is a hole (T10).
const allowed = new Set(["ARTICLE", "HEADER", "DIV", "P", "H1", "H2", "H3", "H4", "H5", "H6", "UL", "OL", "LI",
  "BLOCKQUOTE", "STRONG", "EM", "S", "A", "SPAN", "CODE", "PRE", "HR", "BR", "TABLE", "THEAD", "TBODY", "TR",
  "TH", "TD", "DETAILS", "SUMMARY", "TIME"]);

function inert(root: HTMLElement) {
  for (const n of [root, ...root.querySelectorAll<HTMLElement>("*")]) {
    expect(allowed.has(n.tagName), n.tagName).toBe(true);
    for (const a of n.attributes) {
      expect(a.name.startsWith("on") || a.name === "style" || a.name === "src", a.name).toBe(false);
    }
  }
  for (const a of root.querySelectorAll("a")) expect(a.href).toMatch(/^https?:\/\//);
}

function view(events: RoomEvent[], cap = 5000): { el: HTMLElement; v: ChatView } {
  const el = document.createElement("div");
  const v = new ChatView(el, cap);
  for (const e of events) v.apply(e);
  return { el, v };
}

describe("ChatView messages", () => {
  it("renders a chat message as a bubble with the actor and its markdown", () => {
    const { el } = view([ev(1, "message", { kind: "chat", text: "two **words**", delivery: "none" })]);
    const msg = el.querySelector(".msg")!;
    expect(msg.className).toContain("from-agent");
    expect(msg.querySelector("header")?.textContent).toContain("implementer");
    expect(msg.querySelector(".bubble strong")?.textContent).toBe("words");
  });
  it("never renders HTML from a bubble", () => {
    const { el } = view([ev(1, "message", { kind: "chat", text: "<img src=x onerror=alert(1)>", delivery: "none" })]);
    inert(el);
    expect(el.querySelector("img")).toBeNull();
    expect(el.textContent).toContain("<img src=x onerror=alert(1)>");
  });
  // R09: every bubble says where it went, as the raw row does.
  it("marks each message with its delivery", () => {
    const { el } = view([
      ev(1, "message", { kind: "chat", text: "for the room", delivery: "none" }),
      ev(2, "message", { kind: "chat", text: "later", delivery: "queued" }),
      ev(3, "message", { kind: "chat", text: "now", delivery: "steering", to: ["agent:ikely2yk"] }),
    ]);
    expect([...el.querySelectorAll(".chip-delivery")].map((c) => c.textContent)).toEqual(["chat", "queued", "steering → agent:ikely2yk"]);
  });
  it("names the message a delivery delivered", () => {
    const { el } = view([ev(1, "state_changed", { kind: "delivered", ref: 1846, runId: "ikely2yk" }, { kind: "system", id: "system:room-broker" })]);
    expect(el.querySelector(".mark-text")?.textContent).toBe("delivered #1846");
  });
  it("keeps a review verdict's chip", () => {
    const { el } = view([ev(1, "message", { kind: "review_verdict", text: "add a test", delivery: "none", verdict: "changes" })]);
    expect(el.querySelector(".verdict-changes")?.textContent).toContain("changes");
  });
});

describe("ChatView tool pairing", () => {
  const call = (seq: number, callId: string, command: string) =>
    ev(seq, "tool_call", { callId, tool: "terminal", args: { command }, risk: "LOW", decidedBy: null });
  const result = (seq: number, callId: string, status: string, output: string) =>
    ev(seq, "tool_result", { callId, status, output, truncated: false, bytes: output.length });

  it("shows a call without its result as running", () => {
    const { el } = view([call(1, "call_1", "git push origin agent/ibbay5ud")]);
    const tool = el.querySelector("details.tool")!;
    expect(tool.className).toContain("st-running");
    expect(tool.querySelector(".chip")?.textContent).toBe("running");
    expect(tool.querySelector(".t-cmd")?.textContent).toBe("git push origin agent/ibbay5ud");
    expect(el.children).toHaveLength(1);
  });
  it("pairs a result with its call: one row, the chip flips, the output is behind the toggle", () => {
    const { el } = view([call(1, "call_1", "git status"), result(2, "call_1", "ok", " M web/src/render.ts")]);
    expect(el.children).toHaveLength(1);
    const tool = el.querySelector("details.tool")!;
    expect(tool.className).toContain("st-ok");
    expect(tool.querySelector(".chip")?.textContent).toBe("ok");
    expect(tool.querySelector(".t-out")?.textContent).toBe(" M web/src/render.ts");
    expect(tool.querySelector(".t-args")?.textContent).toContain("git status");
  });
  it("renders a rejected result in its own state", () => {
    const { el } = view([call(1, "c", "git push"), result(2, "c", "rejected", "refused")]);
    expect(el.querySelector("details.tool")!.className).toContain("st-rejected");
  });
  it("keeps an orphan result as its own row", () => {
    const { el } = view([result(5, "call_gone", "ok", "leftover")]);
    const tool = el.querySelector("details.tool")!;
    expect(tool.className).toContain("st-ok");
    expect(tool.textContent).toContain("leftover");
  });
  it("replays idempotently: the same seqs again leave one row", () => {
    const events = [call(1, "c", "ls"), result(2, "c", "ok", "x")];
    const { el } = view([...events, ...events]);
    expect(el.children).toHaveLength(1);
  });
  it("keeps two calls with different ids apart", () => {
    const { el } = view([call(1, "a", "ls"), call(2, "b", "pwd"), result(3, "a", "ok", "x")]);
    expect(el.querySelectorAll("details.tool")).toHaveLength(2);
    expect(el.querySelector(".st-running .t-cmd")?.textContent).toBe("pwd");
  });
});

describe("ChatView policy badges", () => {
  const decided = (seq: number, callId: string, decision: string) =>
    ev(seq, "state_changed", { kind: "policy_decision", class: "forge.other", callId, decision });

  it("attaches a decision to the tool row by callId", () => {
    const { el } = view([
      ev(1, "tool_call", { callId: "c", tool: "terminal", args: { command: "git push" }, risk: "UNKNOWN", decidedBy: null }),
      decided(2, "c", "deny"),
    ]);
    expect(el.children).toHaveLength(1);
    expect(el.querySelector(".pol-deny")?.textContent).toContain("deny");
  });
  it("renders a decision with no known call as a slim marker", () => {
    const { el } = view([decided(1, "call_gone", "allow")]);
    expect(el.querySelector(".mark")?.textContent).toContain("allow");
    expect(el.querySelector("details.tool")).toBeNull();
  });
  it("folds a marker into its call when the call still arrives", () => {
    const { el } = view([
      decided(2, "c", "allow"),
      ev(3, "tool_call", { callId: "c", tool: "terminal", args: { command: "ls" }, risk: "LOW", decidedBy: null }),
    ]);
    expect(el.querySelectorAll(".mark")).toHaveLength(0);
    expect(el.querySelector("details.tool .pol-allow")?.textContent).toContain("allow");
  });
});

describe("ChatView markers and system lines", () => {
  it("renders a harness status change as a marker, not JSON", () => {
    const { el } = view([ev(1, "state_changed", { kind: "harness_status", status: "waiting_for_confirmation", previous: "running" })]);
    const m = el.querySelector(".mark")!;
    expect(m.textContent).toContain("running");
    expect(m.textContent).toContain("waiting_for_confirmation");
    expect(m.textContent).not.toContain("{");
  });
  it("names why a run ended", () => {
    const { el } = view([ev(1, "state_changed", { kind: "run_phase", phase: "Revoked", reason: "deleted" }, { kind: "system", id: "system:room-broker" })]);
    expect(el.querySelector(".mark")?.textContent).toContain("deleted");
  });
  it("renders a turn as a marker", () => {
    const { el } = view([ev(1, "turn", { runId: "ikely2yk", turnId: "t3", phase: "completed" })]);
    expect(el.querySelector(".mark")?.textContent).toContain("t3");
    expect(el.querySelector(".mark")?.textContent).toContain("completed");
  });
  it("renders a participant change as a subtle system line", () => {
    const { el } = view([ev(1, "participant", { principal: "agent:ikely2yk", change: "left", role: "implementer" }, { kind: "system", id: "system:room-broker" })]);
    const s = el.querySelector(".sys")!;
    expect(s.textContent).toContain("agent:ikely2yk");
    expect(s.textContent).toContain("left");
  });
  it("keeps an unknown type visible behind a toggle", () => {
    const { el } = view([ev(1, "commit", { sha: "abc" })]);
    expect(el.querySelector("details.other pre")?.textContent).toContain("abc");
  });
  it("shows redactions on a bubble", () => {
    const e = ev(1, "message", { kind: "chat", text: "x", delivery: "none" });
    e.redactions = ["github-app-token"];
    expect(view([e]).el.querySelector(".redacted")?.textContent).toContain("github-app-token");
  });
});

describe("ChatView bounds", () => {
  it("holds at most cap rows", () => {
    const events = Array.from({ length: 6 }, (_, i) => ev(i + 1, "message", { kind: "chat", text: `m${i}`, delivery: "none" }));
    const { el } = view(events, 4);
    expect(el.children).toHaveLength(4);
    expect(el.textContent).not.toContain("m0");
    expect(el.textContent).toContain("m5");
  });
});

describe("the recorded session fixture", () => {
  const events: RoomEvent[] = readFileSync(join(process.cwd(), "test/fixtures/live-session.jsonl"), "utf8")
    .trim().split("\n").map((l) => JSON.parse(l));

  it("is a strict, gapless seq order", () => {
    events.forEach((e, i) => expect(e.seq).toBe(i + 1));
  });
  it("renders the whole session inert", () => {
    const { el } = view(events);
    inert(el);
  });
  it("reads as a chat: bubbles, five tool rows, markers and system lines", () => {
    const { el } = view(events);
    expect(el.querySelectorAll(".msg")).toHaveLength(5); // four agent, one human
    expect(el.querySelectorAll("details.tool")).toHaveLength(5); // results paired, none doubled
    expect(el.querySelectorAll(".st-rejected")).toHaveLength(1); // the refused push
    expect(el.querySelectorAll(".st-ok")).toHaveLength(3);
    expect(el.querySelectorAll(".st-running")).toHaveLength(1); // git log, still running
    expect(el.querySelectorAll(".pol-allow")).toHaveLength(3); // policy badges on their rows
    expect(el.querySelectorAll(".sys")).toHaveLength(2); // owner and agent joined
    expect(el.querySelector(".redacted")?.textContent).toContain("github-app-token");
    expect(el.querySelector(".msg.from-human .chip-delivery")?.textContent).toBe("steering → agent:ikely2yk");
  });
});
