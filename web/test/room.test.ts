import { readFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Snapshot } from "../src/conn";
import type { Dashboard } from "../src/dashboards";
import { mountRoom } from "../src/room";
import { FakeSocket } from "./fakes";

const theme = { mode: "light" as const, cycle: () => "light" as const };

const snapshot = (you: Partial<Snapshot["you"]> = {}): Snapshot => ({ roomId: "3kq7x2ma", phase: "Open", driver: "human:a",
  driverEpoch: 1, dataClass: "internal", you: { principal: "human:b", role: "collaborator", approver: false, driver: false, webUI: true, ...you } });

// A state frame whose run is live: its agent is present (RoomState.running).
const live = (you: Partial<Snapshot["you"]> = {}): Snapshot => ({ ...snapshot(you), runs: [{ id: "ikely2yk", role: "implementer", phase: "Running" }] });

const event = (seq: number, type: string, payload: unknown) => ({ type: "event", event: { v: 1, id: String(seq), seq,
  roomId: "3kq7x2ma", actor: { kind: "system", id: "system:room-broker" }, type, origin: "broker", ts: "2026-10-06T10:00:00Z",
  redactions: [], payload } });

// The room page with a fake broker: join opens the socket and answers its hello as
// the broker does, the state frame first, then the replay.
function page(get?: typeof fetch, dashboards?: () => Promise<Dashboard[]>) {
  const app = document.createElement("div");
  const sockets: FakeSocket[] = [];
  mountRoom(app, "3kq7x2ma", theme, { socket: (url) => { const s = new FakeSocket(url); sockets.push(s); return s; },
    random: () => 0, get, dashboards });
  const last = () => sockets[sockets.length - 1];
  const join = (s: Snapshot, throughSeq: number, ...replay: unknown[]) => {
    last().open();
    last().recv({ type: "state", throughSeq, snapshot: s });
    last().recv({ type: "sync", fromSeq: 1, throughSeq });
    for (const f of replay) last().recv(f);
  };
  const phase = () => app.querySelector(".chip-phase")?.textContent;
  return { app, sockets, last, join, phase };
}

describe("the room page", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  // The templates load once, beside the summary; the strip fills them with the room's ids.
  it("shows the configured dashboards in the strip, loaded once", async () => {
    const load = vi.fn(async () => [{ label: "Room metrics", url: "https://grafana.example.com/d/r/x?var-room={room}" }]);
    const p = page((async () => new Response("", { status: 503 })) as unknown as typeof fetch, load);
    p.join(snapshot(), 0);
    await vi.advanceTimersByTimeAsync(0);
    expect(p.app.querySelector<HTMLAnchorElement>(".chip-dashboards a")?.href).toBe("https://grafana.example.com/d/r/x?var-room=3kq7x2ma");
    await vi.advanceTimersByTimeAsync(3000);
    expect(load).toHaveBeenCalledTimes(1);
  });

  const agent = (seq: number, type: string, payload: unknown, ts = "2026-10-06T10:00:00Z") => {
    const f = event(seq, type, payload);
    return { ...f, event: { ...f.event, ts, actor: { kind: "agent", id: "agent:ikely2yk", role: "implementer" }, origin: "bridge" } };
  };

  it("says what the agent does now, its age kept current every second", () => {
    vi.setSystemTime(Date.parse("2026-10-06T10:00:05Z"));
    const p = page();
    p.join(snapshot(), 0);
    expect(p.app.querySelector(".now")?.textContent).toBe("No run yet");
    p.last().recv(event(1, "participant", { principal: "agent:ikely2yk", change: "joined", role: "implementer" }));
    p.last().recv(agent(2, "tool_call", { callId: "c1", tool: "terminal", args: { command: "go test ./..." } }));
    expect(p.app.querySelector(".now")?.textContent).toBe("Now ▸ terminal go test ./... · running · 5 s ago");
    vi.advanceTimersByTime(3000);
    expect(p.app.querySelector(".now")?.textContent).toBe("Now ▸ terminal go test ./... · running · 8 s ago");
    p.last().recv(agent(3, "tool_result", { callId: "c1", status: "error", output: "FAIL" }));
    expect(p.app.querySelector(".now")?.textContent).toBe("Now ▸ terminal go test ./... · error · 8 s ago");
  });

  it("takes the Now line's click to its row and opens it", () => {
    const p = page();
    document.body.append(p.app); // the row is found in the page, as in a browser
    try {
      p.join(live(), 0);
      p.last().recv(agent(1, "tool_call", { callId: "c1", tool: "terminal", args: { command: "ls" } }));
      const row = p.app.querySelector<HTMLDetailsElement>('.view-chat details[data-seq="1"]')!;
      let scrolled = 0;
      row.scrollIntoView = () => { scrolled++; };
      expect(row.open).toBe(false);
      p.app.querySelector<HTMLButtonElement>(".now-link")!.click();
      expect(scrolled).toBe(1);
      expect(row.open).toBe(true);
      expect(document.activeElement).toBe(row); // the keyboard lands there too
      expect(row.tabIndex).toBe(-1);
    } finally { p.app.remove(); }
  });

  // U7: the Now link and the needs-you call link land on a visible row beside folded
  // plumbing, and a folded row's seq still resolves (its fold exposes the seqs it holds).
  it("takes the Now link and the call link to a visible row beside a folded group", () => {
    const p = page();
    document.body.append(p.app);
    try {
      p.join(live({ approver: true }), 0);
      p.last().recv(event(1, "turn", { turnId: "t1", phase: "started" }));
      p.last().recv(event(2, "state_changed", { kind: "harness_status", status: "running", previous: "idle" }));
      p.last().recv(agent(3, "tool_call", { callId: "c1", tool: "terminal", args: { command: "git push" } }));
      p.last().recv(event(4, "state_changed", { kind: "policy_decision", decision: "ask", callId: "c1" }));
      p.last().recv(event(5, "turn", { turnId: "t1", phase: "waiting" }));
      p.last().recv(event(6, "state_changed", { kind: "harness_status", status: "waiting_for_confirmation", previous: "running" }));
      p.last().recv(agent(7, "approval_requested", { approvalId: "ap1", callId: "c1", class: "forge.other",
        action: { command: "git push" }, expiresAt: "2026-10-06T11:00:00Z" }));
      const chat = p.app.querySelector<HTMLElement>(".view-chat")!;
      expect([...chat.querySelectorAll("details.fold")].map((f) => f.querySelector("summary")?.textContent)).toEqual(["· turn t1 · 2 changes", "· turn t1 · 2 changes"]);
      expect(chat.querySelector('details.fold [data-seq="5"]')).not.toBeNull();
      const row = chat.querySelector<HTMLDetailsElement>('details.tool[data-seq="3"]')!;
      expect(row.closest(".fold")).toBeNull();
      let scrolled = 0;
      row.scrollIntoView = () => { scrolled++; };
      p.app.querySelector<HTMLButtonElement>(".now-link")!.click();
      expect(row.open).toBe(true);
      row.open = false;
      const link = p.app.querySelector<HTMLAnchorElement>("a.approval-call")!;
      expect(link.hidden).toBe(false);
      expect(link.textContent).toBe("see the call #3");
      (document.activeElement as HTMLElement).blur();
      link.click();
      expect(row.open).toBe(true);
      expect(scrolled).toBe(2);
      expect(document.activeElement).toBe(row);
    } finally { p.app.remove(); }
  });

  it("names the viewer's own messages 'you', from the state frame", () => {
    const p = page();
    p.join(snapshot(), 0);
    const f = event(1, "message", { kind: "chat", text: "mine", delivery: "none" });
    p.last().recv({ ...f, event: { ...f.event, actor: { kind: "human", id: "human:b" } } });
    expect(p.app.querySelector(".view-chat .msg-head .who")?.textContent).toBe("you");
  });

  // Review focus 2: the call row went at the cap; its result alone heads the Now line.
  it("shows a result whose call the page dropped, and its click finds nothing without throwing", () => {
    const p = page();
    p.join(live(), 0);
    p.last().recv(agent(1, "tool_result", { callId: "gone", status: "ok", output: "done\nmore" }));
    expect(p.app.querySelector(".now")?.textContent).toMatch(/^Now ▸ done · ok · /);
    expect(() => p.app.querySelector<HTMLButtonElement>(".now-link")!.click()).not.toThrow();
  });

  // Q2: once the run's agent leaves, the page says no run is on, names the last activity,
  // stops ageing the run and offers no steering.
  it("says there is no run once the agent leaves, and offers no steering", () => {
    vi.setSystemTime(Date.parse("2026-10-06T10:00:05Z"));
    const p = page();
    p.join(snapshot({ principal: "human:a", role: "owner" }), 0); // human:a drives
    const steer = () => p.app.querySelector<HTMLInputElement>('input[name="delivery"][value="steering"]')!;
    const shown = () => !steer().closest("label")!.hidden;
    p.last().recv(event(1, "message", { kind: "chat", text: "anyone?", delivery: "none" }));
    expect(p.app.querySelector(".now")?.textContent).toBe("No run yet"); // a human's message starts no run
    expect(shown()).toBe(false);
    p.last().recv(event(2, "participant", { principal: "agent:ikely2yk", change: "joined", role: "implementer" }));
    p.last().recv(agent(3, "message", { kind: "chat", text: "on it", delivery: "none" }));
    expect(p.app.querySelector(".now")?.textContent).toBe("Now ▸ on it · message · 5 s ago");
    expect([shown(), steer().disabled]).toEqual([true, false]);
    steer().checked = true;
    p.last().recv(event(4, "participant", { principal: "agent:ikely2yk", change: "left" }));
    expect(p.app.querySelector(".now")?.textContent).toBe("No run · last: on it · 5 s ago");
    expect(p.app.querySelector(".now-hint")?.textContent).toBe("Start one under Session › Run");
    expect([shown(), steer().disabled]).toEqual([false, true]);
    expect(p.app.querySelector('input[name="delivery"]:checked')).toBeNull(); // a steer with no run: chosen again
    expect(p.app.querySelector("#delivery-notice")?.textContent).toMatch(/no run/i);
  });

  it("moves the focus to the needs-you panel from its pill", () => {
    const p = page();
    document.body.append(p.app);
    try {
      p.join(live({ approver: true }), 0);
      p.last().recv(agent(1, "approval_requested", { approvalId: "ap1", callId: "c1", class: "forge.other",
        action: { command: "git push" }, expiresAt: "2026-10-06T11:00:00Z" }));
      const panel = p.app.querySelector<HTMLElement>("section.needs")!;
      panel.scrollIntoView = () => {};
      p.app.querySelector<HTMLButtonElement>(".needs-pill")!.click();
      expect(document.activeElement).toBe(panel);
    } finally { p.app.remove(); }
  });

  // One owned timer per mounted room: a re-mount over the same app leaves the old one no tick.
  it("keeps one refresh timer per mounted page, across re-mounts", () => {
    const app = document.createElement("div");
    const mount = () => mountRoom(app, "3kq7x2ma", theme, { socket: (url) => new FakeSocket(url), random: () => 0,
      get: (async () => new Response("", { status: 503 })) as unknown as typeof fetch });
    mount();
    vi.advanceTimersByTime(1000);
    const one = vi.getTimerCount();
    for (let n = 0; n < 3; n++) mount();
    vi.advanceTimersByTime(1000);
    expect(vi.getTimerCount()).toBe(one);
    app.replaceChildren(); // the page left the app: its timer goes too
    vi.advanceTimersByTime(1000);
    expect(vi.getTimerCount()).toBe(one - 1);
  });

  it("styles only the page chrome as a header: a message head is not sticky navy (U1)", () => {
    const p = page();
    p.join(snapshot(), 0);
    p.last().recv(event(1, "message", { kind: "chat", text: "hi", delivery: "none" }));
    expect(p.app.querySelector("header.page-header")).not.toBeNull();
    expect(p.app.querySelector("header.msg-head")?.classList.contains("page-header")).toBe(false);
  });

  it("mounts exactly one Maku brand ahead of the strip", () => {
    const p = page();
    p.join(snapshot(), 0);
    expect(p.app.querySelectorAll(".brand")).toHaveLength(1);
    expect(p.app.querySelector("header.page-header")?.firstElementChild?.classList.contains("brand")).toBe(true);
  });

  it("heads the page with the room's phase as the log moves it (R10)", () => {
    const p = page();
    p.join(snapshot(), 0);
    expect(p.app.querySelector("h1")?.textContent).toBe("Room 3kq7x2ma");
    expect(p.phase()).toBe("Open");
    p.last().recv(event(1, "state_changed", { kind: "room_phase", phase: "Closed", reason: "owner" }));
    expect(p.phase()).toBe("Closed");
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
    const banner = () => p.app.querySelector<HTMLElement>(".banner")!;
    for (let n = 1; n <= 4; n++) { // absent, then listed (which starts the count over), then absent twice
      p.last().drop(1006);
      await flush();
      // Spec "No such room": the page says it is checking during the tries.
      expect(banner().textContent).toMatch(n === 2 ? /^Reconnecting/ : /^Checking the room…$/); // listed: the count starts over
      vi.advanceTimersByTime(30_000); // past any backoff: the re-dial continues
      expect(p.sockets).toHaveLength(n + 1);
      expect(p.app.querySelector("h1")).not.toBeNull();
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

  // Review M3: an ack clears what an act said, never the lost token's notice, which
  // now sits beside the select it is about.
  it("keeps the lost token's notice past the ack of the give that lost it", () => {
    const p = page();
    p.join(snapshot({ principal: "human:a", role: "owner" }), 0); // human:a drives
    const notice = () => p.app.querySelector("footer .act-notice")!.textContent;
    const beside = () => p.app.querySelector("#delivery-notice")!.textContent;
    p.app.querySelector<HTMLInputElement>('input[name="delivery"][value="steering"]')!.checked = true;
    p.app.querySelector<HTMLInputElement>('[name="giveTo"]')!.value = "human:c";
    p.app.querySelector<HTMLButtonElement>('[data-act="driver_give"]')!.click();
    const give = p.last().sent.at(-1) as { clientSeq: number };
    p.last().recv(event(1, "driver", { from: "human:a", to: "human:c", epoch: 2, reason: "given" }));
    const choose = "You no longer hold the driver token: choose where this message goes.";
    expect(beside()).toBe(choose);
    p.last().recv({ type: "ack", clientSeq: give.clientSeq, seq: 1 });
    expect(beside()).toBe(choose);
    // A refusal is an act's own: the next accepted ack clears it, and only it.
    p.last().recv({ type: "ack", clientSeq: give.clientSeq + 1, rejected: "rate_limited" });
    expect(notice()).toMatch(/^Too many actions/);
    p.last().recv({ type: "ack", clientSeq: give.clientSeq + 2, seq: 2 });
    expect(notice()).toBe("");
    expect(beside()).toBe(choose);
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
    expect(p.app.querySelector("h1")).not.toBeNull();
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
    const notice = () => p.app.querySelector("#delivery-notice")!.textContent;
    const radio = (v: string) => p.app.querySelector<HTMLInputElement>(`input[name="delivery"][value="${v}"]`)!;
    const text = p.app.querySelector<HTMLTextAreaElement>('textarea[name="text"]')!;
    const send = p.app.querySelector<HTMLButtonElement>('[data-act="message"]')!;
    radio("steering").checked = true;
    p.last().recv(event(1, "driver", { from: "human:a", to: "human:c", epoch: 2, reason: "taken" }));
    const choose = "You no longer hold the driver token: choose where this message goes.";
    expect(notice()).toBe(choose);
    const frames = p.last().sent.length;
    text.value = "for the room, then";
    send.click(); // no delivery chosen: refused, and said again
    expect(p.last().sent).toHaveLength(frames);
    expect(notice()).toBe(choose);
    radio("none").checked = true;
    send.click();
    const msg = p.last().sent.at(-1) as { clientSeq: number; action: unknown };
    expect(msg.action).toEqual({ kind: "message", text: "for the room, then", delivery: "none" });
    expect(notice()).toBe("");
    p.last().recv({ type: "ack", clientSeq: msg.clientSeq, seq: 2 });
    expect(notice()).toBe("");
  });

  it("announces its notices politely (R10)", () => {
    const p = page();
    p.join(snapshot(), 0);
    for (const notice of [p.app.querySelector("footer .act-notice")!, p.app.querySelector("#delivery-notice")!]) {
      expect([notice.getAttribute("role"), notice.getAttribute("aria-live")]).toEqual(["status", "polite"]);
    }
  });

  // The summary leads the page; the raw stream is collapsed and shares the one socket.
  // Only an event the summary folds refetches it: a busy run's chat and tool calls would
  // otherwise refetch the whole fold every second (R27).
  it("leads with the summary, refetched at most once a second on summary events, over the one socket", async () => {
    const sum = { apiVersion: "summary/v1", room: "3kq7x2ma", url: "u", status: { phase: "Open", run: null, budget: null, pr: null, issue: null, lastVerdict: null },
      needsYou: [], actions: [], notes: { untrusted: true, items: [{ at: "2026-10-06T10:00:00Z", run: "ikely2yk", text: "reading" }] }, cursor: "seq:0" };
    const get = vi.fn(async () => new Response(JSON.stringify(sum), { status: 200 })) as unknown as typeof fetch;
    const p = page(get);
    await vi.advanceTimersByTimeAsync(0);
    expect(get).toHaveBeenCalledTimes(1);
    expect(p.app.querySelector('.summary [data-block="notes"]')).not.toBeNull();
    expect(p.app.querySelector<HTMLDetailsElement>("details.raw-events")!.open).toBe(false);
    p.join(snapshot(), 0);
    p.last().recv(event(1, "message", { kind: "chat", text: "a", delivery: "none" }));
    p.last().recv(event(2, "tool_call", { callId: "c1", tool: "terminal", args: {} }));
    await vi.advanceTimersByTimeAsync(1000);
    expect(get).toHaveBeenCalledTimes(1);
    p.last().recv(event(3, "message", { kind: "progress", text: "planning", delivery: "none" }));
    p.last().recv(event(4, "state_changed", { kind: "task", phase: "Implementing" }));
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

  // Review focus 1: the strip needs no summary; the state frame alone heads the page.
  it("renders the strip from the state frame when the summary cannot be fetched", async () => {
    const p = page(() => Promise.resolve(new Response("", { status: 503 })));
    expect(p.app.querySelector<HTMLElement>("section.needs")!.hidden).toBe(true); // before join: no empty heading
    p.join(snapshot(), 0);
    await vi.runOnlyPendingTimersAsync();
    expect(p.app.querySelector<HTMLElement>("section.needs")!.hidden).toBe(true);
    expect(p.app.querySelector("h1")?.textContent).toBe("Room 3kq7x2ma");
    expect(p.phase()).toBe("Open");
    expect(p.app.querySelector(".chip-driver")?.textContent).toBe("driver: human:a");
    expect(p.app.querySelector(".chip-you")?.textContent).toBe("you: collaborator");
    expect(p.app.querySelector(".summary-error")?.textContent).toMatch(/unavailable/);
    p.last().recv(event(1, "message", { kind: "chat", text: "still flowing", delivery: "none" }));
    expect(p.app.querySelector(".view-chat")!.textContent).toContain("still flowing");
  });

  // R23: only the raw log is collapsed; chat and composer stay in view.
  it("keeps the chat and the composer visible with the raw events closed", () => {
    const p = page();
    p.join(snapshot(), 0);
    expect(p.app.querySelector<HTMLDetailsElement>("details.raw-events")!.open).toBe(false);
    expect(p.app.querySelector("details.raw-events .view-chat")).toBeNull();
    expect(p.app.querySelector("details.raw-events")!.contains(p.app.querySelector('textarea[name="text"]'))).toBe(false);
    expect(p.app.querySelector<HTMLElement>("section.controls")!.hidden).toBe(false);
    expect(p.app.querySelector<HTMLElement>("footer .composer")!.hidden).toBe(false);
  });

  it("keeps the Session toggle hidden until the first frame", () => {
    const p = page();
    expect(p.app.querySelector<HTMLElement>("details.session")!.hidden).toBe(true);
    p.join(snapshot({ principal: "human:a", role: "owner" }), 0);
    expect(p.app.querySelector<HTMLElement>("details.session")!.hidden).toBe(false);
  });

  it("puts Run and Driver behind a Session toggle in the dock, its line naming the driver and the run", () => {
    const p = page();
    p.join(snapshot({ principal: "human:a", role: "owner" }), 0); // human:a drives
    const session = p.app.querySelector<HTMLDetailsElement>("footer.page-dock > details.session")!;
    expect(session.open).toBe(false);
    expect(session.hidden).toBe(false);
    expect(session.querySelector("section.controls .group-run")).not.toBeNull();
    expect(session.querySelector("section.controls .group-driver")).not.toBeNull();
    expect(session.querySelector("summary")!.textContent).toBe("Session · driver: you · no run");
    p.last().recv(event(1, "participant", { principal: "agent:ikely2yk", change: "joined", role: "implementer" }));
    expect(session.querySelector("summary")!.textContent).toBe("Session · driver: you · a run is on");
    expect(session.nextElementSibling?.classList.contains("composer")).toBe(true);
  });

  it("hides the Session toggle from a watcher, who has no controls", () => {
    const p = page();
    p.join(snapshot({ role: "watcher" }), 0);
    expect(p.app.querySelector<HTMLElement>("details.session")!.hidden).toBe(true);
  });

  it("shows a skeleton stream until the first frame, then the stream alone", () => {
    const p = page();
    const skeleton = p.app.querySelector<HTMLElement>(".skeleton")!;
    expect(skeleton.hidden).toBe(false);
    expect(skeleton.querySelectorAll(".sk-msg")).toHaveLength(3);
    expect(skeleton.getAttribute("aria-hidden")).toBe("true");
    p.join(snapshot(), 0);
    expect(skeleton.hidden).toBe(true);
  });

  it("docks the composer at the bottom and disables Send while reconnecting or sealed", () => {
    const p = page();
    p.join(snapshot({ role: "collaborator" }), 0);
    const dock = p.app.querySelector("footer.page-dock")!;
    expect(dock.querySelector("textarea")).not.toBeNull();
    expect((dock.querySelector('button[data-act="message"]') as HTMLButtonElement).disabled).toBe(false);
    expect(p.app.querySelector<HTMLElement>(".banner")!.hidden).toBe(true);
    p.last().drop(1006);
    expect((dock.querySelector('button[data-act="message"]') as HTMLButtonElement).disabled).toBe(true);
    expect(p.app.querySelector(".banner")?.textContent).toMatch(/^Reconnecting in \d+ s: connection lost$/);
    expect(p.app.querySelector<HTMLElement>(".banner")!.hidden).toBe(false);
    expect(p.app.querySelector(".banner")!.previousElementSibling).toBe(p.app.querySelector("header.page-header"));
  });

  it("reads live with its event count in the dock, and the protocol counters on the raw log", () => {
    const p = page();
    expect(p.app.querySelector("footer .status")?.textContent).toBe("connecting…");
    p.join(snapshot(), 0, event(1, "message", { kind: "chat", text: "a", delivery: "none" }), event(2, "message", { kind: "chat", text: "b", delivery: "none" }));
    expect(p.app.querySelector("footer .status")?.textContent).toBe("live · 2 events");
    expect(p.app.querySelector("details.raw-events > summary")?.textContent).toBe("Raw events · seq 2 · gaps 0 · dups 0");
    expect(p.app.querySelector("footer .theme-toggle")).not.toBeNull(); // the theme button moved to the dock
    expect(p.app.querySelector("header .theme-toggle")).toBeNull();
  });

  // Review focus 5.
  it("keeps a steering draft and moves the notice beside the delivery group when the token is lost (review focus 5)", () => {
    const p = page();
    p.join(snapshot({ role: "collaborator", driver: true }), 0);
    const dock = p.app.querySelector("footer.page-dock")!;
    const text = dock.querySelector("textarea") as HTMLTextAreaElement;
    const steer = dock.querySelector('input[name="delivery"][value="steering"]') as HTMLInputElement;
    text.value = "turn left"; steer.checked = true;
    p.last().recv(event(1, "driver", { from: "human:b", to: "human:c", epoch: 2 }));
    expect(text.value).toBe("turn left");
    expect(dock.querySelector('input[name="delivery"]:checked')).toBeNull();
    expect(steer.getAttribute("aria-describedby")).toBe("delivery-notice delivery-hint");
    expect(dock.querySelector("#delivery-notice")?.textContent).toMatch(/choose where this message goes/);
    expect((dock.querySelector('button[data-act="message"]') as HTMLButtonElement).disabled).toBe(false);
  });

  it("reads Closed by whom in the strip and hides the composer", () => {
    const p = page();
    p.join(snapshot({ role: "owner" }), 0);
    p.last().recv({ ...event(1, "state_changed", { kind: "room_phase", phase: "Closed", reason: "owner" }), event: { ...event(1, "state_changed", {}).event,
      actor: { kind: "human", id: "human:b" }, payload: { kind: "room_phase", phase: "Closed", reason: "owner" } } });
    expect(p.app.querySelector(".closed")?.textContent).toMatch(/^Closed .* by you$/);
    expect(p.app.querySelector("footer.page-dock textarea")!.closest("[hidden]")).not.toBeNull();
    expect(p.app.querySelector<HTMLElement>(".group-run")!.hidden).toBe(true);
    expect(p.app.querySelector<HTMLElement>(".group-driver")!.hidden).toBe(true);
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

    // A watcher sees the panel read-only (spec D3): the link lands on the card, focusing nothing.
    it("focuses nothing, and does not throw, for a non-approver", () => {
      location.hash = "#01M4A";
      const p = page();
      document.body.append(p.app);
      expect(() => p.join(snapshot({ role: "watcher" }), 0, requested(1, "01M4A"))).not.toThrow();
      expect(p.app.querySelector("#approval-01M4A button")).toBeNull();
      expect(p.app.querySelector("#approval-01M4A")?.contains(document.activeElement)).toBe(false);
      p.app.remove();
    });
  });

  describe("the needs-you panel", () => {
    const requested = (seq: number, id: string) => event(seq, "approval_requested", { approvalId: id, callId: "c1", class: "forge.pr",
      action: { command: "git push" }, expiresAt: "2099-01-01T00:00:00Z" });
    const pill = (p: ReturnType<typeof page>) => p.app.querySelector(".needs-pill")?.textContent;

    it("heads the lead, between the strip and the chat", () => {
      const p = page();
      p.join(snapshot({ role: "owner" }), 0, requested(1, "01M4A"));
      const kids = [...p.app.children];
      const panel = p.app.querySelector("section.needs")!;
      // Only the reconnecting banner, hidden while live, sits between the strip and the panel.
      expect(kids.indexOf(panel)).toBe(kids.indexOf(p.app.querySelector("header")!) + 2);
      expect(kids[kids.indexOf(panel) - 1].className).toBe("banner");
      expect(kids.indexOf(panel)).toBeLessThan(kids.indexOf(p.app.querySelector("main")!));
      expect(panel.querySelector("#approval-01M4A .approval-command")?.textContent).toBe("git push");
    });

    it("raises a banner in the strip and a four-eyes note on the card of whoever prompted the turn", () => {
      const p = page();
      p.join({ ...snapshot({ principal: "human:b", approver: true }), fourEyes: true }, 0,
        event(1, "approval_requested", { approvalId: "01M4A", callId: "c1", class: "forge.pr", action: {}, expiresAt: "2099-01-01T00:00:00Z",
          prompters: ["human:b"] }));
      expect(p.app.querySelector("a.approvals-banner")?.textContent).toBe("⏳ 1 approval waits for another approver — review");
      expect(p.app.querySelector("#approval-01M4A .approval-status")?.textContent).toContain("you prompted this turn (four-eyes)");
      p.app.remove();
    });

    // The pill and the panel agree: approver OR owner decides (policy Decide).
    it("counts an approval in the pill for an owner who is no approver, and not for a collaborator", () => {
      const o = page();
      o.join(snapshot({ role: "owner", approver: false }), 0, requested(1, "01M4A"));
      expect(pill(o)).toBe("Needs you · 1");
      const c = page();
      c.join(snapshot({ role: "collaborator", approver: false }), 0, requested(1, "01M4A"));
      expect(pill(c)).toBeUndefined();
    });

    // Review focus 3: a decide that lost the race is refused already_decided; the card
    // leaves on the approval_decided event, with no dead button left behind.
    it("drops a card whose decide lost the race once the log decides it", () => {
      const p = page();
      p.join(snapshot({ approver: true }), 0, requested(1, "01M4A"));
      p.app.querySelector<HTMLButtonElement>("#approval-01M4A [data-act=approve]")!.click();
      const act = p.last().sent.at(-1) as { clientSeq: number; action: unknown };
      expect(act.action).toEqual({ kind: "decide", approvalId: "01M4A", decision: "approved" });
      p.last().recv({ type: "ack", clientSeq: act.clientSeq, rejected: "already_decided" });
      expect(p.app.querySelector("footer .act-notice")?.textContent).toBe("Another approver decided first.");
      p.last().recv(event(2, "approval_decided", { approvalId: "01M4A", decision: "denied" }));
      expect(p.app.querySelector("#approval-01M4A")).toBeNull();
      expect(p.app.querySelector("[data-act=approve]")).toBeNull();
      expect(p.app.querySelector<HTMLElement>("section.needs")!.hidden).toBe(true);
      expect(pill(p)).toBeUndefined();
    });
  });

  describe("summary refresh", () => {
    const sum = (phase: string, cursor: string) => ({ apiVersion: "summary/v1", room: "3kq7x2ma", url: "u",
      status: { phase, run: null, budget: null, pr: null, issue: null, lastVerdict: null },
      needsYou: [], actions: [], notes: { untrusted: true, items: [{ at: "2026-10-06T10:00:00Z", run: "ikely2yk", text: "reading" }] }, cursor });
    const ok = (b: unknown) => new Response(JSON.stringify(b), { status: 200 });
    const tick = async () => { await vi.advanceTimersByTimeAsync(1000); };
    const note = (p: ReturnType<typeof page>, n: number) => p.last().recv(event(n, "message", { kind: "progress", text: "x", delivery: "none" }));

    it("keeps the blocks and adds an error line when a refresh fails", async () => {
      const replies = [() => ok(sum("Running", "seq:1")), () => new Response("", { status: 503 })];
      const p = page((async () => replies.shift()!()) as unknown as typeof fetch);
      await vi.advanceTimersByTimeAsync(0);
      p.join(snapshot(), 0);
      note(p, 1);
      await tick();
      expect(p.phase()).toBe("Running");
      const err = p.app.querySelector<HTMLElement>(".summary-error")!;
      expect(err.hidden).toBe(false);
      expect(err.textContent).toMatch(/cannot be verified/);
    });

    it("drops a response older than the one already rendered", async () => {
      const late: ((r: Response) => void)[] = [];
      const p = page((() => new Promise<Response>((r) => late.push(r))) as unknown as typeof fetch);
      p.join(snapshot(), 0);
      note(p, 1);
      await tick(); // request 2 is out beside request 1
      expect(late).toHaveLength(2);
      late[1](ok(sum("Newer", "seq:2")));
      await vi.advanceTimersByTimeAsync(0);
      late[0](ok(sum("Older", "seq:1")));
      await vi.advanceTimersByTimeAsync(0);
      expect(p.phase()).toBe("Newer");
    });

    // C1: the broker omits a PR's reviewers until someone reviews (omitempty).
    it("renders the broker's own golden summary", async () => {
      const { want } = JSON.parse(readFileSync(join(__dirname, "../../internal/summary/testdata/normal.json"), "utf8"));
      const p = page((async () => ok(want)) as unknown as typeof fetch);
      await vi.advanceTimersByTimeAsync(0);
      expect(p.app.querySelector(".page-header")?.textContent).toContain("#2239");
      expect(p.app.querySelector<HTMLElement>(".summary-error")!.hidden).toBe(true);
    });

    // The getter stands in for a render that fails once: the page says so, and the next refresh
    // renders the same summary instead of taking it as already shown.
    it("retries a summary whose render failed, and says it failed", async () => {
      let reads = 0;
      const body = { ...sum("Running", "seq:1") } as Record<string, unknown>;
      const status = body.status;
      Object.defineProperty(body, "status", { enumerable: true, get: () => {
        if (++reads === 2) throw new Error("render failed");
        return status;
      } });
      const p = page((async () => ({ ok: true, status: 200, json: async () => body })) as unknown as typeof fetch);
      await vi.advanceTimersByTimeAsync(0);
      expect(p.app.querySelector('[data-block="notes"]')).toBeNull();
      expect(p.phase()).toBeUndefined();
      expect(p.app.querySelector<HTMLElement>(".summary-error")!.hidden).toBe(false);
      p.join(snapshot(), 0);
      note(p, 1);
      await tick();
      expect(p.phase()).toBe("Running");
      expect(p.app.querySelector('[data-block="notes"]')).not.toBeNull();
      expect(p.app.querySelector<HTMLElement>(".summary-error")!.hidden).toBe(true);
    });

    it("does not rebuild the blocks when the summary is unchanged", async () => {
      const p = page((async () => ok(sum("Running", "seq:1"))) as unknown as typeof fetch);
      await vi.advanceTimersByTimeAsync(0);
      const first = p.app.querySelector('[data-block="notes"]');
      p.join(snapshot(), 0);
      const h1 = p.app.querySelector("h1");
      note(p, 1);
      await tick();
      expect(p.app.querySelector('[data-block="notes"]')).toBe(first);
      expect(p.app.querySelector("h1")).toBe(h1); // nor the strip, whose PR link may hold the focus
    });
  });
});
