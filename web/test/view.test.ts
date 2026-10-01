import { describe, expect, it } from "vitest";
import { listRooms, RoomLog } from "../src/view";

const row = (text: string) => { const p = document.createElement("p"); p.textContent = text; return p; };

describe("RoomLog", () => {
  it("replaces a resent seq's row in place", () => {
    const el = document.createElement("main");
    const log = new RoomLog(el, 10);
    log.put(1, row("a"));
    log.put(2, row("b"));
    log.put(1, row("a2"));
    expect([...el.children].map((c) => c.textContent)).toEqual(["a2", "b"]);
  });
  it("inserts a resent seq it no longer holds in order, never at the end", () => {
    const el = document.createElement("main");
    const log = new RoomLog(el, 10);
    for (const s of [1, 3, 5]) log.put(s, row(String(s)));
    log.put(4, row("4"));
    log.put(2, row("2"));
    log.put(6, row("6"));
    expect([...el.children].map((c) => c.textContent)).toEqual(["1", "2", "3", "4", "5", "6"]);
  });
  it("keeps the newest rows up to its cap (review M2)", () => {
    const el = document.createElement("main");
    const log = new RoomLog(el, 3);
    for (let s = 1; s <= 5; s++) log.put(s, row(String(s)));
    expect([...el.children].map((c) => c.textContent)).toEqual(["3", "4", "5"]);
    log.put(2, row("late")); // a clamped resume resending a row already dropped
    expect([...el.children].map((c) => c.textContent)).toEqual(["3", "4", "5"]);
  });
});

describe("listRooms", () => {
  const app = () => document.createElement("div");
  it("says why when the list cannot be read, not a blank page (review M3)", async () => {
    for (const fail of [
      () => Promise.reject(new TypeError("Failed to fetch")),
      () => Promise.resolve(new Response("<html>sign in</html>", { status: 200 })),
      () => Promise.resolve(new Response("", { status: 503 })),
    ]) {
      const el = app();
      await listRooms(el, fail as typeof fetch);
      expect(el.textContent).toMatch(/^rooms unavailable/);
    }
  });
  it("links each room", async () => {
    const el = app();
    const rows = [{ id: "3kq7x2ma", phase: "", owner: "human:1", driver: "", dataClass: "internal", lastSeq: 4 }];
    await listRooms(el, (() => Promise.resolve(new Response(JSON.stringify(rows)))) as typeof fetch);
    expect(el.querySelector("a")?.getAttribute("href")).toBe("/r/3kq7x2ma");
  });
});
