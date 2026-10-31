import { afterEach, describe, expect, it } from "vitest";
import { HIDDEN_SECONDS, mountAutoRefresh, readInterval, REFRESH_STORAGE_KEY, storeInterval, type Clock } from "../src/autorefresh";

// fakeClock runs timers by hand: advance(ms) moves the time and fires what falls due, in order.
function fakeClock(start = 1_000_000) {
  let t = start, id = 0;
  const due = new Map<number, { at: number; f: () => void }>();
  const clock: Clock = {
    now: () => t,
    set: (f, ms) => { due.set(++id, { at: t + ms, f }); return id; },
    clear: (h) => { due.delete(h as number); },
  };
  const advance = async (ms: number) => {
    const end = t + ms;
    for (;;) {
      const next = [...due.entries()].filter(([, d]) => d.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
      if (!next) break;
      due.delete(next[0]);
      t = next[1].at;
      next[1].f();
      await new Promise((r) => setTimeout(r, 0)); // let the refresh's promise settle
    }
    t = end;
  };
  return { clock, advance, pending: () => due.size };
}

function memStore(init: Record<string, string> = {}): Storage {
  const m = new Map(Object.entries(init));
  return {
    getItem: (k) => m.get(k) ?? null, setItem: (k, v) => { m.set(k, v); }, removeItem: (k) => { m.delete(k); },
    clear: () => m.clear(), key: (i) => [...m.keys()][i] ?? null, get length() { return m.size; },
  };
}

// fakeDoc is the page's visibility, flipped by hand.
function fakeDoc() {
  const target = new EventTarget();
  let state: DocumentVisibilityState = "visible";
  return {
    doc: Object.defineProperty(target, "visibilityState", { get: () => state }) as unknown as Document,
    hide: () => { state = "hidden"; target.dispatchEvent(new Event("visibilitychange")); },
    show: () => { state = "visible"; target.dispatchEvent(new Event("visibilitychange")); },
  };
}

describe("the stored refresh interval", () => {
  it("defaults to 10 s when unset, corrupt or not an offered choice", () => {
    expect(readInterval(memStore())).toBe(10);
    expect(readInterval(memStore({ [REFRESH_STORAGE_KEY]: "banana" }))).toBe(10);
    expect(readInterval(memStore({ [REFRESH_STORAGE_KEY]: "7" }))).toBe(10);
  });
  it("reads back an offered choice, off included", () => {
    const s = memStore();
    storeInterval(30, s);
    expect(readInterval(s)).toBe(30);
    storeInterval(0, s);
    expect(readInterval(s)).toBe(0);
  });
  it("answers the default when storage throws, and storing there is not fatal", () => {
    const broken = { getItem: () => { throw new Error("denied"); }, setItem: () => { throw new Error("denied"); } } as unknown as Storage;
    expect(readInterval(broken)).toBe(10);
    expect(() => storeInterval(30, broken)).not.toThrow();
  });
});

describe("mountAutoRefresh", () => {
  let stop = () => {};
  afterEach(() => stop());
  const setup = (o: { store?: Storage; result?: () => Promise<boolean> } = {}) => {
    const c = fakeClock();
    const v = fakeDoc();
    let calls = 0;
    const host = document.createElement("div");
    const r = mountAutoRefresh(host, () => { calls++; return o.result ? o.result() : Promise.resolve(true); },
      { clock: c.clock, doc: v.doc, store: o.store ?? memStore(), time: (ms) => `T${(ms - 1_000_000) / 1000}` });
    stop = r.stop;
    const select = host.querySelector("select") as HTMLSelectElement;
    const pause = host.querySelector("button") as HTMLButtonElement;
    const status = host.querySelector(".refresh-status") as HTMLElement;
    return { ...c, ...v, host, select, pause, status, calls: () => calls };
  };

  it("refreshes every 10 s by default", async () => {
    const s = setup();
    expect(s.select.value).toBe("10");
    await s.advance(9_999);
    expect(s.calls()).toBe(0);
    await s.advance(1);
    expect(s.calls()).toBe(1);
    await s.advance(10_000);
    expect(s.calls()).toBe(2);
  });
  it("offers off, 5 s, 10 s, 30 s and 1 min, and remembers the choice", async () => {
    const store = memStore();
    const s = setup({ store });
    expect([...s.select.options].map((o) => o.textContent)).toEqual(["Off", "5 s", "10 s", "30 s", "1 min"]);
    s.select.value = "30";
    s.select.dispatchEvent(new Event("change"));
    expect(readInterval(store)).toBe(30);
    await s.advance(29_999);
    expect(s.calls()).toBe(0);
    await s.advance(1);
    expect(s.calls()).toBe(1);
  });
  it("starts from the stored choice", async () => {
    const s = setup({ store: memStore({ [REFRESH_STORAGE_KEY]: "5" }) });
    expect(s.select.value).toBe("5");
    await s.advance(5_000);
    expect(s.calls()).toBe(1);
  });
  it("never refreshes when off, and hides the pause button there", async () => {
    const s = setup();
    s.select.value = "0";
    s.select.dispatchEvent(new Event("change"));
    expect(s.pause.hidden).toBe(true);
    await s.advance(600_000);
    expect(s.calls()).toBe(0);
    expect(s.pending()).toBe(0);
  });
  it("pauses and resumes, saying so on the button and in the status", async () => {
    const s = setup();
    s.pause.click();
    expect(s.pause.getAttribute("aria-pressed")).toBe("true");
    expect(s.pause.textContent).toMatch(/Resume/);
    expect(s.status.textContent).toMatch(/^Paused/);
    await s.advance(60_000);
    expect(s.calls()).toBe(0);
    s.pause.click(); // overdue: refreshes at once
    expect(s.pause.getAttribute("aria-pressed")).toBe("false");
    await s.advance(0);
    expect(s.calls()).toBe(1);
  });
  it("says when it last refreshed, and when a refresh failed", async () => {
    let ok = true;
    const s = setup({ result: () => Promise.resolve(ok) });
    await s.advance(10_000);
    expect(s.status.textContent).toBe("Updated T10");
    ok = false;
    await s.advance(10_000);
    expect(s.status.textContent).toBe("Refresh failed · updated T10");
    expect(s.status.dataset.failed).toBe("true");
  });
  it("never starts a refresh while one is still running", async () => {
    let finish = () => {};
    const s = setup({ result: () => new Promise((r) => { finish = () => r(true); }) });
    await s.advance(10_000);
    await s.advance(60_000);
    expect(s.calls()).toBe(1);
    finish();
    await new Promise((r) => setTimeout(r, 0)); // the refresh settles, the next is scheduled
    await s.advance(10_000);
    expect(s.calls()).toBe(2);
  });
  it(`slows to every ${HIDDEN_SECONDS} s in a background tab, and catches up when shown`, async () => {
    const s = setup();
    s.hide();
    await s.advance(59_999);
    expect(s.calls()).toBe(0);
    await s.advance(1);
    expect(s.calls()).toBe(1);
    await s.advance(15_000);
    s.show(); // 15 s since the last: overdue at 10 s
    await s.advance(0);
    expect(s.calls()).toBe(2);
  });
  it("stops: no timer left and no more refreshes", async () => {
    const s = setup();
    stop();
    await s.advance(600_000);
    expect(s.calls()).toBe(0);
    expect(s.pending()).toBe(0);
  });
  it("labels its controls for assistive tech", () => {
    const s = setup();
    expect(s.select.getAttribute("aria-label")).toBe("Auto-refresh interval");
    expect(s.status.getAttribute("role")).toBe("status");
  });
});
