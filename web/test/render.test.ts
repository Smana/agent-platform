import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { renderEvent } from "../src/render";

const ev = (type: string, payload: unknown, actorId = "agent:7f3cq2xz") => ({
  v: 1, id: "x", seq: 3, roomId: "3kq7x2ma", actor: { kind: "agent", id: actorId, role: "reviewer" },
  type, origin: "harness", ts: "2026-09-27T10:00:00Z", redactions: [], payload,
});
const chat = (text: string) => renderEvent(ev("message", { kind: "chat", text, delivery: "none" }));

// Everything the renderer may build; anything else in the output is a hole.
const allowed = new Set(["ARTICLE", "HEADER", "DIV", "P", "H1", "H2", "H3", "H4", "H5", "H6", "UL", "OL", "LI",
  "BLOCKQUOTE", "STRONG", "EM", "S", "A", "SPAN", "CODE", "PRE", "HR", "BR", "TABLE", "THEAD", "TBODY", "TR",
  "TH", "TD", "DETAILS", "SUMMARY"]);

function inert(el: HTMLElement) {
  for (const n of [el, ...el.querySelectorAll("*")]) {
    expect(allowed.has(n.tagName), n.tagName).toBe(true);
    for (const a of n.attributes) {
      expect(a.name.startsWith("on") || a.name === "style" || a.name === "src", a.name).toBe(false);
    }
  }
  for (const a of el.querySelectorAll("a")) expect(a.href).toMatch(/^https?:\/\//);
}

describe("renderEvent (T10)", () => {
  it("never renders HTML from a message", () => {
    const el = chat("<img src=x onerror=alert(1)><script>alert(1)</script>**ok**");
    expect(el.querySelector("script")).toBeNull();
    expect(el.querySelector("img")).toBeNull();
    expect(el.querySelector("strong")?.textContent).toBe("ok");
    expect(el.textContent).toContain("<script>");
    inert(el);
  });
  it("shows tool output as text", () => {
    const el = renderEvent(ev("tool_result", { callId: "c", status: "ok", output: "<b>raw</b>", truncated: false, bytes: 10 }));
    expect(el.querySelector("b")).toBeNull();
    expect(el.textContent).toContain("<b>raw</b>");
  });
  it("names why a run ended", () => {
    const el = renderEvent(ev("state_changed", { kind: "run_phase", phase: "Failed", reason: "pod_lost" }));
    expect(el.textContent).toContain("pod_lost");
  });

  // SP3's sanitiser defuses markup with &lt;, !\[ and ]\: before it reaches the log. The
  // UI renders it once and never unescapes it first, so none of it re-arms.
  it("renders a hostile payload inert", () => {
    const el = chat([
      "<img src=x onerror=alert(1)>",
      "[click](javascript:alert(1)) [caps](JaVaScRiPt:alert(1)) <javascript:alert(1)> [data](data:text/html,<b>x</b>)",
      "[here](/api/rooms) [proto](//evil.example/x) [ftp](ftp://evil.example/f)",
      "&lt;img src=x onerror=alert(1)&gt;",
      "&amp;lt;b&amp;gt;",
      "!\\[pixel](https://evil.example/p.png)",
      "![real](https://evil.example/q.png)",
      "[ref]\\: https://evil.example/r",
      "",
      "see [ref] and [ok](https://example.com/a)",
    ].join("\n"));
    inert(el);
    expect(el.querySelector("img")).toBeNull();
    // An entity-encoded tag is decoded once, into text: shown, never parsed.
    expect(el.textContent).toContain("<img src=x onerror=alert(1)>");
    // A double-encoded one keeps its second layer: nothing unescapes twice.
    expect(el.textContent).toContain("&lt;b&gt;");
    // The defused image and reference stay text; linkify may make their bare URL a
    // link (inert() allows only http and https), which loads nothing until clicked.
    expect(el.textContent).toContain("![pixel](https://evil.example/p.png)");
    expect(el.textContent).toContain("[ref]: https://evil.example/r");
    const links = [...el.querySelectorAll("a")];
    // Only the web is linked: no same-origin path, no other scheme.
    for (const text of ["ref", "here", "proto", "ftp", "click", "caps", "data"]) {
      expect(links.map((a) => a.textContent)).not.toContain(text);
    }
    expect(el.textContent).toContain("[image: real]"); // an image shows its alt text, and loads nothing
    const ok = links.find((a) => a.textContent === "ok")!;
    expect([ok.href, ok.rel, ok.target]).toEqual(["https://example.com/a", "noopener noreferrer nofollow", "_blank"]);
    for (const a of links) expect([a.rel, a.target]).toEqual(["noopener noreferrer nofollow", "_blank"]);
  });
  it("renders a hostile actor as text", () => {
    const el = renderEvent(ev("message", { kind: "chat", text: "hi", delivery: "none" }, "<img src=x onerror=alert(1)>"));
    expect(el.querySelector("img")).toBeNull();
    expect(el.querySelector("header")?.textContent).toContain("<img src=x onerror=alert(1)>");
  });
  it("shows an approval request as its card, read-only, and a decision with its reason", () => {
    const el = renderEvent({ ...ev("approval_requested", { approvalId: "ap1", callId: "call_97", class: "forge.pr",
      action: { command: "<img src=x onerror=alert(1)>" }, expiresAt: "2026-10-01T10:30:00Z" }), runId: "7f3cq2xz" });
    inert(el);
    expect(el.querySelector("img")).toBeNull();
    expect(el.querySelector("button")).toBeNull();
    expect(el.querySelector(".approval-head")?.textContent).toMatch(/^approval: forge\.pr · run 7f3cq2xz · call call_97 · expires /);
    expect(el.querySelector("pre")?.textContent).toBe(JSON.stringify({ command: "<img src=x onerror=alert(1)>" }, null, 2));
    const d = renderEvent(ev("approval_decided", { approvalId: "ap1", decision: "denied", reason: "<b>not</b> this branch" }, "human:apr"));
    expect(d.querySelector(".decision-denied")?.textContent).toBe("denied · <b>not</b> this branch");
    expect(d.querySelector("b")).toBeNull();
  });
  // R09: a row says where its message went, and a delivery names the message it delivered.
  it("badges a message with its delivery, one per value", () => {
    const badge = (payload: object) =>
      renderEvent(ev("message", { kind: "chat", text: "x", ...payload })).querySelector(".chip-delivery")?.textContent;
    expect(badge({ delivery: "none" })).toBe("chat");
    expect(badge({ delivery: "queued" })).toBe("queued");
    expect(badge({ delivery: "steering", to: ["agent:7f3cq2xz"] })).toBe("steering → agent:7f3cq2xz");
    const d = renderEvent(ev("state_changed", { kind: "delivered", ref: 1846, runId: "7f3cq2xz" }));
    expect(d.querySelector(".state")?.textContent).toBe("delivered #1846");
  });
  it("keeps markdown's structure", () => {
    const el = chat("# T\n\n- a\n- `b`\n\n```\n<i>c</i>\n```\n\n> q");
    inert(el);
    expect(el.querySelector("h1")?.textContent).toBe("T");
    expect(el.querySelectorAll("li")).toHaveLength(2);
    expect(el.querySelector("li code")?.textContent).toBe("b");
    expect(el.querySelector("pre code")?.textContent).toBe("<i>c</i>\n");
    expect(el.querySelector("blockquote")?.textContent).toContain("q");
  });
});

describe("the UI source", () => {
  // Room text reaches the DOM through textContent only (T10).
  it("never builds HTML from a string", () => {
    const dir = join(process.cwd(), "src"); // vitest runs in web/
    const files = readdirSync(dir).filter((n) => n.endsWith(".ts"));
    expect(files.length).toBeGreaterThan(3);
    for (const f of files) {
      expect(readFileSync(join(dir, f), "utf8"), f).not.toMatch(/innerHTML|outerHTML|insertAdjacentHTML|document\.write|\.render\(/);
    }
  });
});
