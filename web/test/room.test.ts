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

  // Every reader may fork (policy Fork): a chat row's "fork here" opens the panel at
  // that row's seq, a watcher's included, and its submit sends the fork act.
  it("forks from a chat row, as a watcher", () => {
    const p = page();
    p.join(snapshot({ role: "watcher" }), 0);
    p.last().recv(event(1, "message", { kind: "chat", text: "try uv?", delivery: "none" }));
    const panel = p.app.querySelector<HTMLElement>("section.fork")!;
    expect(panel.hidden).toBe(true);
    p.app.querySelector<HTMLButtonElement>(".fork-here")!.click();
    expect(panel.hidden).toBe(false);
    expect(panel.querySelector(".fork-at")?.textContent).toBe("fork at #1");
    panel.querySelector("form")!.dispatchEvent(new Event("submit", { cancelable: true }));
    expect(p.last().sent.at(-1)).toMatchObject({ type: "act", action: { kind: "fork", seq: 1 } });
  });

  // R10: an invite or a removal changes what the page may offer. No client
  // projection: the reconnect's state frame re-resolves you; the broker checks every act.
  it("re-syncs when a membership change past the mark names you", () => {
    const p = page();
    // A human's membership events are invites' role_changed (internal/humanapi/acts.go).
    p.join(snapshot(), 2, event(1, "participant", { principal: "human:b", change: "role_changed", role: "watcher" }),
      event(2, "participant", { principal: "human:b", change: "role_changed", role: "collaborator" }));
    expect(p.last().closed).toBe(false); // the state frame already holds what the replay resends
    p.last().recv(event(3, "participant", { principal: "human:c", change: "role_changed", role: "watcher" }));
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
  // A room just created is often refused (roomctrl writes its log row afterwards) and
  // the list reads a replica's informer cache, which may lag the POST (review I2): the
  // page stops only after three absent answers in a row, each a backoff apart.
  const listed = (body: unknown) => Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) });
  // The summary fetch is not a list call: it gets a 503 and is not counted.
  const noSummary = Promise.resolve({ ok: false, status: 503, json: () => Promise.resolve({}) });
  const lists = (...script: string[][]) => {
    const calls: number[] = [];
    const get = ((url: string) => {
      if (url.includes("/summary")) return noSummary;
      calls.push(calls.length);
      return listed(script[Math.min(calls.length - 1, script.length - 1)].map((id) => ({ id })));
    }) as unknown as typeof fetch;
    return { get, calls };
  };
  const flush = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };
  const gone = "No such room, or you cannot read it. See the rooms you can read.";

  it("stops on a room the list misses three refusals in a row, with one answer for both cases", async () => {
    const l = lists([], ["3kq7x2ma"], [], [], []);
    const p = page(l.get);
    for (let n = 1; n <= 4; n++) { // absent, then listed (which starts the count over), then absent twice
      p.last().drop(1006);
      await flush();
      vi.advanceTimersByTime(30_000); // past any backoff: the re-dial continues
      expect(p.sockets).toHaveLength(n + 1);
      expect(p.app.querySelector(".title")).not.toBeNull();
    }
    p.last().drop(1006); // the third absent answer in a row
    await flush();
    expect(p.app.textContent).toBe(gone);
    expect(p.app.querySelector("a")?.getAttribute("href")).toBe("/");
    vi.advanceTimersByTime(60_000);
    expect(p.sockets).toHaveLength(5);
    expect(l.calls).toHaveLength(5);
  });

  // The last answer lands after the retry dialled: stop closes that socket too, and
  // its refusal asks the list nothing more.
  it("stops a re-dial already under way", async () => {
    let answer!: (r: unknown) => void;
    let asked = 0;
    const p = page(((url: string) => {
      if (url.includes("/summary")) return noSummary;
      asked++;
      return asked < 3 ? listed([]) : new Promise((r) => { answer = r; });
    }) as unknown as typeof fetch);
    for (let n = 0; n < 2; n++) {
      p.last().drop(1006);
      await flush();
      vi.advanceTimersByTime(30_000);
    }
    p.last().drop(1006);
    vi.advanceTimersByTime(30_000);
    expect(p.sockets).toHaveLength(4);
    answer(await listed([]));
    await flush();
    expect(p.sockets[3].closed).toBe(true);
    expect(asked).toBe(3);
    expect(p.app.textContent).toBe(gone);
    vi.advanceTimersByTime(60_000);
    expect(p.sockets).toHaveLength(4);
  });

  // Review M3: an ack clears what an act said, never a notice the controls set
  // meanwhile, such as a lost token's.
  it("keeps the lost token's notice past the ack of the give that lost it", () => {
    const p = page();
    p.join(snapshot({ principal: "human:a", role: "owner" }), 0); // human:a drives
    const notice = () => p.app.querySelector("footer .notice")!.textContent;
    p.app.querySelector<HTMLSelectElement>('select[name="delivery"]')!.value = "steering";
    p.app.querySelector<HTMLInputElement>('[name="giveTo"]')!.value = "human:c";
    p.app.querySelector<HTMLButtonElement>('[data-act="driver_give"]')!.click();
    const give = p.last().sent.at(-1) as { clientSeq: number };
    p.last().recv(event(1, "driver", { from: "human:a", to: "human:c", epoch: 2, reason: "given" }));
    const choose = "You no longer hold the driver token: choose where this message goes.";
    expect(notice()).toBe(choose);
    p.last().recv({ type: "ack", clientSeq: give.clientSeq, seq: 1 });
    expect(notice()).toBe(choose);
    // A refusal is an act's own: the next accepted ack clears it.
    p.last().recv({ type: "ack", clientSeq: give.clientSeq + 1, rejected: "rate_limited" });
    expect(notice()).toMatch(/^Too many actions/);
    p.last().recv({ type: "ack", clientSeq: give.clientSeq + 2, seq: 2 });
    expect(notice()).toBe("");
  });

  // Re-review N2: a connection the broker accepted proves the room readable, so the
  // three-in-a-row count starts over.
  it("restarts the count of absent answers after a state frame", async () => {
    const l = lists([]);
    const p = page(l.get);
    const refuse = async () => {
      p.last().drop(1006);
      await flush();
      vi.advanceTimersByTime(30_000);
    };
    await refuse();
    await refuse(); // two absent answers
    p.join(snapshot(), 0); // the third dial is accepted
    p.last().drop(1006); // a drop after the open asks the list nothing
    vi.advanceTimersByTime(30_000);
    await refuse();
    await refuse();
    expect(p.app.querySelector(".title")).not.toBeNull();
    expect(p.sockets).toHaveLength(6);
    p.last().drop(1006); // the third in a row since the open
    await flush();
    expect(p.app.textContent).toBe(gone);
    expect(l.calls).toHaveLength(5);
  });

  // Re-review N1: a send that went out supersedes every notice, the lost token's too;
  // only an ack is limited to clearing what an act said.
  it("clears the lost token's notice once the human picks a delivery and sends", () => {
    const p = page();
    p.join(snapshot({ principal: "human:a", role: "owner" }), 0); // human:a drives
    const notice = () => p.app.querySelector("footer .notice")!.textContent;
    const delivery = p.app.querySelector<HTMLSelectElement>('select[name="delivery"]')!;
    const text = p.app.querySelector<HTMLTextAreaElement>('textarea[name="text"]')!;
    const send = p.app.querySelector<HTMLButtonElement>('[data-act="message"]')!;
    delivery.value = "steering";
    p.last().recv(event(1, "driver", { from: "human:a", to: "human:c", epoch: 2, reason: "taken" }));
    const choose = "You no longer hold the driver token: choose where this message goes.";
    expect(notice()).toBe(choose);
    const frames = p.last().sent.length;
    text.value = "for the room, then";
    send.click(); // no delivery chosen: refused, and said again
    expect(p.last().sent).toHaveLength(frames);
    expect(notice()).toBe(choose);
    delivery.value = "none";
    send.click();
    const msg = p.last().sent.at(-1) as { clientSeq: number; action: unknown };
    expect(msg.action).toEqual({ kind: "message", text: "for the room, then", delivery: "none" });
    expect(notice()).toBe("");
    p.last().recv({ type: "ack", clientSeq: msg.clientSeq, seq: 2 });
    expect(notice()).toBe("");
  });

  it("announces its notices politely (R10)", () => {
    const notice = page().app.querySelector("footer .notice")!;
    expect([notice.getAttribute("role"), notice.getAttribute("aria-live")]).toEqual(["status", "polite"]);
  });

  // The summary leads the page; the raw stream is collapsed and shares the one socket.
  it("leads with the summary, refetched once a second on events, over the one socket", async () => {
    const sum = { apiVersion: "summary/v1", room: "3kq7x2ma", url: "u", status: { phase: "Open", run: null, budget: null, pr: null, issue: null, lastVerdict: null },
      needsYou: [], actions: [], notes: { untrusted: true, items: [] }, cursor: "seq:0" };
    const get = vi.fn(async () => new Response(JSON.stringify(sum), { status: 200 })) as unknown as typeof fetch;
    const p = page(get);
    await vi.advanceTimersByTimeAsync(0);
    expect(get).toHaveBeenCalledTimes(1);
    expect(p.app.querySelector('.summary [data-block="status"]')).not.toBeNull();
    expect(p.app.querySelector<HTMLDetailsElement>("details.raw-events")!.open).toBe(false);
    p.join(snapshot(), 0);
    p.last().recv(event(1, "message", { kind: "chat", text: "a", delivery: "none" }));
    p.last().recv(event(2, "message", { kind: "chat", text: "b", delivery: "none" }));
    await vi.advanceTimersByTimeAsync(1000);
    expect(get).toHaveBeenCalledTimes(2);
    expect(p.sockets).toHaveLength(1);
  });

  it("shows the summary's error and keeps the stream when the fetch fails", async () => {
    const get = (async () => new Response("", { status: 503 })) as unknown as typeof fetch;
    const p = page(get);
    await vi.advanceTimersByTimeAsync(0);
    expect(p.app.querySelector(".summary")!.textContent).toMatch(/cannot be verified/);
    p.join(snapshot(), 0);
    p.last().recv(event(1, "message", { kind: "chat", text: "still here", delivery: "none" }));
    expect(p.app.querySelector(".view-chat")!.textContent).toContain("still here");
  });

  // R23: only the raw log is collapsed; chat and composer stay in view.
  it("keeps the chat and the composer visible with the raw events closed", () => {
    const p = page();
    p.join(snapshot(), 0);
    expect(p.app.querySelector<HTMLDetailsElement>("details.raw-events")!.open).toBe(false);
    expect(p.app.querySelector("details.raw-events .view-chat")).toBeNull();
    expect(p.app.querySelector("details.raw-events")!.contains(p.app.querySelector('textarea[name="text"]'))).toBe(false);
    expect(p.app.querySelector<HTMLElement>("section.controls")!.hidden).toBe(false);
    expect(p.app.querySelector<HTMLElement>(".composer")!.hidden).toBe(false);
  });

  // R24: the broker's link is <room>#<approvalId>; it lands on that approval's card.
  describe("the broker's approval link", () => {
    const requested = (seq: number, id: string) => event(seq, "approval_requested", { approvalId: id, callId: "c1", class: "forge.pr",
      action: {}, expiresAt: "2099-01-01T00:00:00Z" });
    afterEach(() => { location.hash = ""; });

    it("focuses the Approve button of the approval the hash names", () => {
      location.hash = "#01M4A";
      const p = page();
      document.body.append(p.app);
      p.join(snapshot({ approver: true }), 0, requested(1, "01M4A"));
      expect(document.activeElement).toBe(p.app.querySelector("#approval-01M4A [data-act=approve]"));
      p.app.remove();
    });

    it("follows a later hashchange", () => {
      const p = page();
      document.body.append(p.app);
      p.join(snapshot({ approver: true }), 0, requested(1, "AAA"), requested(2, "BBB"));
      location.hash = "#BBB";
      window.dispatchEvent(new Event("hashchange"));
      expect(document.activeElement).toBe(p.app.querySelector("#approval-BBB [data-act=approve]"));
      p.app.remove();
    });

    it("does nothing, and does not throw, for a non-approver", () => {
      location.hash = "#01M4A";
      const p = page();
      document.body.append(p.app);
      expect(() => p.join(snapshot({ role: "watcher" }), 0, requested(1, "01M4A"))).not.toThrow();
      expect(p.app.querySelector("#approval-01M4A")).toBeNull();
      p.app.remove();
    });
  });
});
