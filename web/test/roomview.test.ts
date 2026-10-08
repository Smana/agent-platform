import { describe, expect, it } from "vitest";
import { mountViews } from "../src/roomview";

function store(init: Record<string, string> = {}) {
  const data = new Map(Object.entries(init));
  return {
    getItem: (k: string) => data.get(k) ?? null,
    setItem: (k: string, v: string) => void data.set(k, v),
    data,
  };
}

function mount(stored?: Record<string, string>) {
  const main = document.createElement("main");
  const bar = document.createElement("header");
  const s = store(stored);
  const views = mountViews(main, bar, s as unknown as Storage);
  return { main, bar, s, views };
}

describe("mountViews", () => {
  it("defaults to the chat view, the raw one hidden", () => {
    const { main } = mount();
    const chat = main.querySelector<HTMLElement>(".view-chat")!;
    const raw = main.querySelector<HTMLElement>(".view-raw")!;
    expect(chat.hidden).toBe(false);
    expect(raw.hidden).toBe(true);
  });
  it("switches on the button and remembers", () => {
    const { main, bar, s } = mount();
    const chat = main.querySelector<HTMLElement>(".view-chat")!;
    const raw = main.querySelector<HTMLElement>(".view-raw")!;
    const btn = bar.querySelector<HTMLButtonElement>("button.view-toggle")!;
    btn.click();
    expect(chat.hidden).toBe(true);
    expect(raw.hidden).toBe(false);
    expect(s.data.get("room-view")).toBe("raw");
    btn.click();
    expect(chat.hidden).toBe(false);
    expect(s.data.get("room-view")).toBe("chat");
  });
  it("honours a stored raw preference", () => {
    const { main } = mount({ "room-view": "raw" });
    expect(main.querySelector<HTMLElement>(".view-chat")!.hidden).toBe(true);
    expect(main.querySelector<HTMLElement>(".view-raw")!.hidden).toBe(false);
  });
  it("ignores a stored value it does not know", () => {
    const { main } = mount({ "room-view": "mosaic" });
    expect(main.querySelector<HTMLElement>(".view-chat")!.hidden).toBe(false);
  });
  it("routes events into both views", () => {
    const { main, views } = mount();
    const ev = {
      v: 1, id: "x", seq: 1, roomId: "3kq7x2ma", actor: { kind: "agent", id: "agent:ikely2yk", role: "implementer" },
      type: "message", origin: "harness", ts: "2026-10-04T14:00:01Z", redactions: [],
      payload: { kind: "chat", text: "hi", delivery: "none" },
    };
    views.apply(ev);
    expect(main.querySelector(".view-chat .msg")?.textContent).toContain("hi");
    expect(main.querySelector(".view-raw .ev-message")?.textContent).toContain("hi");
  });
});
