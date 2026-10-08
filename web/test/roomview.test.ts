import { describe, expect, it } from "vitest";
import { mountViews } from "../src/roomview";

function mount() {
  const main = document.createElement("main");
  const views = mountViews(main);
  return { main, views };
}

describe("mountViews", () => {
  it("shows the chat and keeps the raw log in a closed details", () => {
    const { main } = mount();
    expect(main.querySelector<HTMLElement>(".view-chat")!.hidden).toBe(false);
    const d = main.querySelector<HTMLDetailsElement>("details.raw-events")!;
    expect(d.open).toBe(false);
    expect(d.querySelector("summary")!.textContent).toBe("Raw events");
    expect(d.querySelector(".view-raw")).not.toBeNull();
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
