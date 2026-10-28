import { readFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ChatView } from "../src/chat";
import { Clips, overflows } from "../src/clip";
import type { RoomEvent } from "../src/conn";

const css = readFileSync(join(__dirname, "../src/app.css"), "utf8");

/** The declarations of the first rule whose selector list is exactly `sel`. */
function rule(sel: string): string {
  const esc = sel.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const m = css.match(new RegExp(`(?:^|\\n)${esc}\\s*{([^}]*)}`));
  expect(m, `rule ${sel}`).not.toBeNull();
  return m![1];
}

const ev = (seq: number, type: string, payload: unknown, kind = "agent"): RoomEvent => ({
  v: 1, id: `id${seq}`, seq, roomId: "3kq7x2ma", runId: "ikely2yk", actor: { kind, id: `${kind}:ikely2yk`, role: "implementer" },
  type, origin: "harness", ts: "2026-10-04T14:00:00Z", redactions: [], payload,
});
const msg = (seq: number, text: string, kind = "agent") => ev(seq, "message", { kind: "chat", text, delivery: "none" }, kind);
const long = Array.from({ length: 40 }, (_, i) => `line ${i}`).join("\n\n");

// jsdom has no layout: a body "overflows" here when its text is long.
const byLength = (body: HTMLElement) => (body.textContent ?? "").length > 100;

function mount() {
  const el = document.createElement("div");
  return { el, view: new ChatView(el, undefined, undefined, undefined, byLength) };
}
const toggle = (el: HTMLElement, seq: number) =>
  el.querySelector<HTMLButtonElement>(`[data-seq="${seq}"] .bubble > button.clip-toggle`)!;

describe("a long message folds", () => {
  it("clips a message whose body overflows, behind a Show more button that controls it", () => {
    const { el, view } = mount();
    view.apply(msg(1, long));
    const bubble = el.querySelector<HTMLElement>(".bubble")!;
    const b = toggle(el, 1);
    expect(bubble.dataset.clip).toBe("closed");
    expect(b.hidden).toBe(false);
    expect(b.type).toBe("button");
    expect(b.textContent).toBe("Show more");
    expect(b.getAttribute("aria-expanded")).toBe("false");
    const body = bubble.querySelector(".md")!;
    expect(body.id).not.toBe("");
    expect(b.getAttribute("aria-controls")).toBe(body.id);
  });

  it("expands on click and folds again on Show less", () => {
    const { el, view } = mount();
    view.apply(msg(1, long));
    const b = toggle(el, 1);
    b.click();
    expect(el.querySelector<HTMLElement>(".bubble")!.dataset.clip).toBe("open");
    expect(b.textContent).toBe("Show less");
    expect(b.getAttribute("aria-expanded")).toBe("true");
    b.click();
    expect(el.querySelector<HTMLElement>(".bubble")!.dataset.clip).toBe("closed");
    expect(b.textContent).toBe("Show more");
    expect(b.getAttribute("aria-expanded")).toBe("false");
  });

  it("never folds a short message: it gets no button", () => {
    const { el, view } = mount();
    view.apply(msg(1, "short and sweet"));
    expect(el.querySelector<HTMLElement>(".bubble")!.dataset.clip).toBe("short");
    expect(toggle(el, 1)).toBeNull();
  });

  it("keeps a message the reader opened open through new events and a resend of its seq", () => {
    const { el, view } = mount();
    view.apply(msg(1, long));
    toggle(el, 1).click();
    view.apply(ev(2, "tool_call", { callId: "c1", tool: "terminal", args: { command: "ls" } }));
    view.apply(msg(3, long, "human"));
    view.apply(msg(1, long)); // a resume resends seq 1: a fresh node
    expect(el.querySelector<HTMLElement>('[data-seq="1"] .bubble')!.dataset.clip).toBe("open");
    expect(toggle(el, 1).textContent).toBe("Show less");
    expect(el.querySelector<HTMLElement>('[data-seq="3"] .bubble')!.dataset.clip).toBe("closed");
  });

  it("never folds a tool row, whose output has its own toggle", () => {
    const { el, view } = mount();
    view.apply(ev(1, "tool_call", { callId: "c1", tool: "terminal", args: { command: long } }));
    view.apply(ev(2, "tool_result", { callId: "c1", status: "ok", output: long }));
    expect(el.querySelector(".clip-toggle")).toBeNull();
    expect(el.querySelector("[data-clip]")).toBeNull();
  });

  it("folds only when the body runs over twelve lines by more than two", () => {
    const body = document.createElement("div");
    body.style.lineHeight = "20px"; // twelve lines: 240 px; two more: 280 px
    const height = (h: number) => Object.defineProperty(body, "scrollHeight", { configurable: true, value: h });
    height(600);
    expect(overflows(body)).toBe(true);
    height(281);
    expect(overflows(body)).toBe(true);
    height(270); // a twelve-line message with its margins: a few pixels over, left whole
    expect(overflows(body)).toBe(false);
    height(0); // not laid out yet
    expect(overflows(body)).toBe(false);
  });

  it("opens a closed message when the focus lands inside it, never on its own button", () => {
    const { el, view } = mount();
    document.body.append(el);
    view.apply(msg(1, `${long}\n\n[a link](https://example.com/far-down)`));
    const bubble = el.querySelector<HTMLElement>(".bubble")!;
    toggle(el, 1).dispatchEvent(new FocusEvent("focusin", { bubbles: true }));
    expect(bubble.dataset.clip).toBe("closed");
    bubble.querySelector("a")!.dispatchEvent(new FocusEvent("focusin", { bubbles: true }));
    expect(bubble.dataset.clip).toBe("open");
    expect(toggle(el, 1).getAttribute("aria-expanded")).toBe("true");
    view.apply(msg(1, `${long}\n\n[a link](https://example.com/far-down)`)); // it stays open, as if clicked
    expect(el.querySelector<HTMLElement>(".bubble")!.dataset.clip).toBe("open");
    el.remove();
  });

  it("forgets a message the cap evicts", () => {
    const forget = vi.spyOn(Clips.prototype, "forget");
    const el = document.createElement("div");
    const view = new ChatView(el, 2, undefined, undefined, byLength);
    for (const s of [1, 2, 3]) view.apply(msg(s, long));
    expect(forget).toHaveBeenCalledWith(1);
    forget.mockRestore();
  });
});

// A ResizeObserver the test fires by hand: jsdom has none.
class FakeObserver {
  static last: FakeObserver;
  targets = new Set<Element>();
  constructor(private cb: ResizeObserverCallback) { FakeObserver.last = this; }
  observe(t: Element) { this.targets.add(t); }
  unobserve(t: Element) { this.targets.delete(t); }
  disconnect() { this.targets.clear(); }
  fire() {
    this.cb([...this.targets].map((target) => ({ target }) as ResizeObserverEntry), this as unknown as ResizeObserver);
  }
}

describe("a long message, measured by the browser's layout", () => {
  afterEach(() => vi.unstubAllGlobals());
  const observed = () => {
    vi.stubGlobal("ResizeObserver", FakeObserver);
    const el = document.createElement("div");
    document.body.append(el);
    let wide = false; // the layout the test sets: a narrow window makes the long message overflow
    const view = new ChatView(el, 3, undefined, undefined, (b) => !wide && (b.textContent ?? "").length > 100);
    return { el, view, ro: () => FakeObserver.last, widen: (w: boolean) => { wide = w; } };
  };

  it("decides once laid out, and again when its size changes", () => {
    const { el, view, ro, widen } = observed();
    widen(true);
    view.apply(msg(1, long));
    ro().fire();
    expect(el.querySelector<HTMLElement>(".bubble")!.dataset.clip).toBe("short");
    widen(false);
    ro().fire();
    expect(el.querySelector<HTMLElement>(".bubble")!.dataset.clip).toBe("closed");
    expect(toggle(el, 1).hidden).toBe(false);
    el.remove();
  });

  it("reads every body before it writes any, so a backlog lays out once", () => {
    vi.stubGlobal("ResizeObserver", FakeObserver);
    const el = document.createElement("div");
    document.body.append(el);
    const seen: number[] = [];
    const view = new ChatView(el, undefined, undefined, undefined, (b) => {
      seen.push(el.querySelectorAll("[data-clip]").length);
      return (b.textContent ?? "").length > 100;
    });
    for (const s of [1, 2, 3]) view.apply(msg(s, long, "human"));
    FakeObserver.last.fire();
    expect(seen).toEqual([0, 0, 0]);
    expect(el.querySelectorAll('[data-clip="closed"]')).toHaveLength(3);
    el.remove();
  });

  it("stops observing a row the page dropped", () => {
    const { el, view, ro } = observed();
    view.apply(msg(1, long));
    const first = el.querySelector<HTMLElement>(".md")!;
    view.apply(msg(1, long)); // a resend: a fresh node replaces it
    ro().fire();
    expect(ro().targets.has(first)).toBe(false);
    expect(ro().targets.has(el.querySelector(".md")!)).toBe(true);
    el.remove();
  });

  it("re-measures an opened message: one that now fits loses its button, and opens again when it no longer does", () => {
    const { el, view, ro, widen } = observed();
    view.apply(msg(1, long));
    ro().fire();
    toggle(el, 1).click();
    widen(true);
    ro().fire();
    const bubble = el.querySelector<HTMLElement>(".bubble")!;
    expect(bubble.dataset.clip).toBe("short");
    expect(toggle(el, 1).hidden).toBe(true);
    widen(false);
    ro().fire();
    expect(bubble.dataset.clip).toBe("open");
    expect(toggle(el, 1).textContent).toBe("Show less");
    el.remove();
  });
});

describe("the fold's CSS", () => {
  it("clips a closed bubble at twelve lines, padded so a focus ring inside is not cut", () => {
    const clip = rule('.bubble[data-clip="closed"] > .md');
    expect(clip).toMatch(/max-height:\s*calc\(12 \* 1\.55em\)/);
    expect(clip).toMatch(/overflow-y:\s*hidden/);
    const pad = clip.match(/padding:\s*(\d+)px/);
    expect(pad, "padding").not.toBeNull();
    expect(clip).toMatch(new RegExp(`margin:\\s*-${pad![1]}px`));
    expect(css).not.toContain('.bubble:not([data-clip="open"])'); // a short or opened one is never clipped
  });
  it("fades a closed bubble into its own background, for agent, human and system bubbles", () => {
    expect(rule(".bubble")).toMatch(/--bubble-bg:\s*var\(--card\)/);
    expect(rule(".bubble")).toMatch(/background:\s*var\(--bubble-bg\)/);
    expect(rule(".from-human .bubble")).toMatch(/--bubble-bg:\s*color-mix\(in srgb, var\(--primary\) 10%, var\(--card\)\)/);
    expect(rule(".from-system .bubble")).toMatch(/--bubble-bg:\s*var\(--muted\)/);
    const fade = rule('.bubble[data-clip="closed"] > .md::after');
    expect(fade).toMatch(/linear-gradient\([^)]*var\(--bubble-bg\)\)/);
    expect(fade).toMatch(/pointer-events:\s*none/);
  });
});
