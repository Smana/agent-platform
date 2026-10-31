// SPDX-License-Identifier: Apache-2.0

// The room list's auto-refresh: an interval the viewer picks (off, 5 s to 1 min, 10 s by
// default, remembered in this browser), a pause, and when it last refreshed. One refresh at a
// time: the next is scheduled only once the last settles, so a slow broker never stacks them.
// A background tab slows to HIDDEN_SECONDS rather than stopping, so its title's needs-you count
// still moves; shown again, an overdue refresh runs at once.

export const REFRESH_STORAGE_KEY = "maku:list-refresh";
export const DEFAULT_SECONDS = 10;
export const HIDDEN_SECONDS = 60;
const CHOICES: [number, string][] = [[0, "Off"], [5, "5 s"], [10, "10 s"], [30, "30 s"], [60, "1 min"]];

// Clock is the timers and time the control runs on; tests drive it by hand.
export interface Clock { now(): number; set(f: () => void, ms: number): unknown; clear(h: unknown): void }

const realClock: Clock = {
  now: () => Date.now(),
  set: (f, ms) => setTimeout(f, ms),
  clear: (h) => clearTimeout(h as ReturnType<typeof setTimeout>),
};

// readInterval is the stored choice in seconds, 0 for off; the default when unset, not an
// offered choice, or the storage throws (a private window).
export function readInterval(store: Storage = localStorage): number {
  try {
    const n = Number(store.getItem(REFRESH_STORAGE_KEY) ?? NaN);
    return CHOICES.some(([s]) => s === n) ? n : DEFAULT_SECONDS;
  } catch {
    return DEFAULT_SECONDS;
  }
}

export function storeInterval(seconds: number, store: Storage = localStorage): void {
  try {
    store.setItem(REFRESH_STORAGE_KEY, String(seconds));
  } catch {
    // Non-fatal: the choice holds for this page, it just won't persist.
  }
}

export interface AutoRefreshOptions {
  clock?: Clock; doc?: Document; store?: Storage;
  time?: (ms: number) => string; // the "updated" time, the locale's clock by default
}

// mountAutoRefresh appends the control to host and schedules refresh, which resolves whether it
// read the list. stop clears the timer and the visibility listener.
export function mountAutoRefresh(host: HTMLElement, refresh: () => Promise<boolean>, o: AutoRefreshOptions = {}) {
  const clock = o.clock ?? realClock;
  const doc = o.doc ?? document;
  const store = o.store ?? localStorage;
  const time = o.time ?? ((ms: number) => new Date(ms).toLocaleTimeString());

  const box = document.createElement("span");
  box.className = "refresh";
  const select = document.createElement("select");
  select.name = "refresh";
  select.setAttribute("aria-label", "Auto-refresh interval");
  for (const [s, label] of CHOICES) {
    const opt = document.createElement("option");
    opt.value = String(s);
    opt.textContent = label;
    select.append(opt);
  }
  const pause = document.createElement("button");
  pause.type = "button";
  pause.className = "refresh-pause";
  const status = document.createElement("span");
  status.className = "refresh-status";
  status.setAttribute("role", "status");
  box.append(status, select, pause);
  host.append(box);

  let seconds = readInterval(store);
  select.value = String(seconds);
  let paused = false, running = false, stopped = false, failed = false;
  let last = clock.now(); // the list on the page was read just now
  let updated = 0; // when a refresh last read the list; 0 until one has
  let timer: unknown;

  const show = () => {
    pause.hidden = seconds === 0;
    pause.textContent = paused ? "▶ Resume" : "⏸ Pause";
    pause.setAttribute("aria-pressed", String(paused));
    const at = updated ? time(updated) : "";
    const lead = paused ? "Paused" : failed ? "Refresh failed" : "";
    status.textContent = lead && at ? `${lead} · updated ${at}` : lead || (at ? `Updated ${at}` : "");
    if (failed) status.dataset.failed = "true"; else delete status.dataset.failed;
  };
  const schedule = () => {
    if (timer !== undefined) clock.clear(timer);
    timer = undefined;
    if (stopped || paused || running || seconds === 0) return;
    const every = doc.visibilityState === "hidden" ? Math.max(seconds, HIDDEN_SECONDS) : seconds;
    timer = clock.set(() => void tick(), Math.max(0, last + every * 1000 - clock.now()));
  };
  const tick = async () => {
    timer = undefined;
    running = true;
    const ok = await refresh().catch(() => false);
    running = false;
    last = clock.now();
    failed = !ok;
    if (ok) updated = last;
    show();
    schedule();
  };

  select.addEventListener("change", () => {
    seconds = Number(select.value);
    storeInterval(seconds, store);
    show();
    schedule();
  });
  pause.onclick = () => {
    paused = !paused;
    show();
    schedule();
  };
  const onVisibility = () => schedule();
  doc.addEventListener("visibilitychange", onVisibility);
  show();
  schedule();
  return {
    stop(): void {
      stopped = true;
      if (timer !== undefined) clock.clear(timer);
      timer = undefined;
      doc.removeEventListener("visibilitychange", onVisibility);
    },
  };
}
