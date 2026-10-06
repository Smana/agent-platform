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

  // R11: a refused socket asks the room list. A room the list does not hold, whether
  // it does not exist or the caller cannot read it, gets one answer and no re-dial.
  it("stops on a room the list does not hold, with one answer for both cases", async () => {
    const list = (ids: string[]) => (() => Promise.resolve({ ok: true, status: 200,
      json: () => Promise.resolve(ids.map((id) => ({ id }))) })) as unknown as typeof fetch;
    const flush = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };

    const gone = page(list(["aaaaaaaa"]));
    gone.last().drop(1006);
    await flush();
    expect(gone.app.textContent).toBe("No such room, or you cannot read it. See the rooms you can read.");
    expect(gone.app.querySelector("a")?.getAttribute("href")).toBe("/");
    vi.advanceTimersByTime(60_000);
    expect(gone.sockets).toHaveLength(1);

    // The list answers after the retry dialled: stop closes that socket too, and its
    // refusal asks the list nothing more.
    let answer!: (r: unknown) => void;
    let asked = 0;
    const slow = page((() => { asked++; return new Promise((r) => { answer = r; }); }) as unknown as typeof fetch);
    slow.last().drop(1006);
    vi.advanceTimersByTime(500);
    expect(slow.sockets).toHaveLength(2);
    answer({ ok: true, status: 200, json: () => Promise.resolve([]) });
    await flush();
    expect(slow.sockets[1].closed).toBe(true);
    expect(asked).toBe(1);
    vi.advanceTimersByTime(60_000);
    expect(slow.sockets).toHaveLength(2);

    const listed = page(list(["3kq7x2ma"])); // a broker restarting: retry
    listed.last().drop(1006);
    await flush();
    vi.advanceTimersByTime(500);
    expect(listed.sockets).toHaveLength(2);
    expect(listed.app.querySelector(".title")).not.toBeNull();
  });

  it("announces its notices politely (R10)", () => {
    const notice = page().app.querySelector("footer .notice")!;
    expect([notice.getAttribute("role"), notice.getAttribute("aria-live")]).toEqual(["status", "polite"]);
  });
});
