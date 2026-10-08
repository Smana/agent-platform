// SPDX-License-Identifier: Apache-2.0

// The offline preview: the recorded session (test/fixtures/live-session.jsonl)
// rendered exactly as the room page renders a live one, with no broker. Serve
// web/ statically (`npm run preview`) and open /preview/preview.html; ?live
// replays the run at reading pace so the running chips and pairings show.

import type { RoomEvent } from "../src/conn";
import { mountViews, type Views } from "../src/roomview";
import { initTheme, mountThemeButton } from "../src/theme";

const app = document.getElementById("app")!;
const theme = initTheme();

async function load(): Promise<RoomEvent[]> {
  const r = await fetch("/test/fixtures/live-session.jsonl");
  const text = await r.text();
  return text.trim().split("\n").map((l) => JSON.parse(l) as RoomEvent);
}

function skeleton(): { views: Views; counters: HTMLElement; status: HTMLElement; foot: HTMLElement } {
  const header = document.createElement("header");
  const title = document.createElement("span");
  title.className = "title";
  title.textContent = "3kq7x2ma · Open · internal · preview of test/fixtures/live-session.jsonl";
  header.append(title);
  mountThemeButton(header, theme);
  const main = document.createElement("main");
  const foot = document.createElement("footer");
  const counters = document.createElement("span");
  const status = document.createElement("span");
  status.className = "status";
  foot.append(counters, status);
  app.replaceChildren(header, main, foot);
  return { views: mountViews(main, header), counters, status, foot };
}

const delay = (ms: number) => new Promise((r) => setTimeout(r, ms));

async function run() {
  const events = await load();
  const live = new URLSearchParams(location.search).has("live");
  const { views, counters, status, foot } = skeleton();

  if (!live) {
    for (const e of events) views.apply(e);
    counters.textContent = `seq ${events.length} · gaps 0 · dups 0`;
    status.textContent = "offline fixture";
    const again = document.createElement("button");
    again.textContent = "replay live";
    again.style.marginLeft = "12px";
    again.onclick = () => { location.search = "?live"; };
    foot.append(again);
    return;
  }

  status.textContent = "replaying…";
  foot.dataset.status = "live";
  for (const [i, e] of events.entries()) {
    views.apply(e);
    counters.textContent = `seq ${e.seq} · gaps 0 · dups 0`;
    status.textContent = `replaying… ${i + 1}/${events.length}`;
    await delay(450);
  }
  status.textContent = "replayed · offline fixture";
}

run().catch((e) => { app.textContent = `preview failed: ${e instanceof Error ? e.message : e}`; });
