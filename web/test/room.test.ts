import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Snapshot } from "../src/conn";
import { mountRoom } from "../src/room";
import { FakeSocket } from "./fakes";

const theme = { mode: "light" as const, cycle: () => "light" as const };

const snapshot = (you: Partial<Snapshot["you"]> = {}): Snapshot => ({ roomId: "3kq7x2ma", phase: "Open", driver: "human:a",
  driverEpoch: 1, dataClass: "internal", you: { principal: "human:b", role: "collaborator", approver: false, driver: false, webUI: true, ...you } });

const event = (seq: number, type: string, payload: unknown) => ({ type: "event", event: { v: 1, id: String(seq), seq,
  roomId: "3kq7x2ma", actor: { kind: "system", id: "system:room-broker" }, type, origin: "broker", ts: "2026-10-06T10:00:00Z",
  redactions: [], payload } });

// The room page with a fake broker: join opens the socket and answers its hello as
// the broker does, the state frame first, then the replay.
function page(get?: typeof fetch) {
  const app = document.createElement("div");
  const sockets: FakeSocket[] = [];
  mountRoom(app, "3kq7x2ma", theme, { socket: (url) => { const s = new FakeSocket(url); sockets.push(s); return s; },
    random: () => 0, get });
  const last = () => sockets[sockets.length - 1];
  const join = (s: Snapshot, throughSeq: number, ...replay: unknown[]) => {
    last().open();
    last().recv({ type: "state", throughSeq, snapshot: s });
    last().recv({ type: "sync", fromSeq: 1, throughSeq });
    for (const f of replay) last().recv(f);
  };
  const title = () => app.querySelector(".title")?.textContent;
  return { app, sockets, last, join, title };
}

describe("the room page", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("heads the page with the room's phase as the log moves it (R10)", () => {
    const p = page();
    p.join(snapshot(), 0);
    expect(p.title()).toMatch(/^3kq7x2ma · Open · internal · /);
    p.last().recv(event(1, "state_changed", { kind: "room_phase", phase: "Closed", reason: "owner" }));
    expect(p.title()).toMatch(/^3kq7x2ma · Closed · internal · /);
  });

  // R10: an invite or a removal changes what the page may offer. No client
  // projection: the reconnect's state frame re-resolves you; the broker checks every act.
  it("re-syncs when a membership change past the mark names you", () => {
    const p = page();
    p.join(snapshot(), 2, event(1, "participant", { principal: "human:b", change: "joined", role: "watcher" }),
      event(2, "participant", { principal: "human:b", change: "role_changed", role: "collaborator" }));
    expect(p.last().closed).toBe(false); // the state frame already holds what the replay resends
    p.last().recv(event(3, "participant", { principal: "human:c", change: "joined", role: "watcher" }));
    expect(p.last().closed).toBe(false); // someone else's
    p.last().recv(event(4, "participant", { principal: "human:b", change: "left" }));
    expect(p.sockets[0].closed).toBe(true);
    vi.advanceTimersByTime(500);
    expect(p.sockets).toHaveLength(2);
    p.last().open();
    expect(p.last().sent[0]).toMatchObject({ type: "hello", afterSeq: 4 });
  });

  it("announces its notices politely (R10)", () => {
    const notice = page().app.querySelector("footer .notice")!;
    expect([notice.getAttribute("role"), notice.getAttribute("aria-live")]).toEqual(["status", "polite"]);
  });
});
