// SPDX-License-Identifier: Apache-2.0

// The live demo: the room page replaying a recorded run at its recorded pace, sped up.
// Every path is relative, so the site serves from a domain root or under GitHub Pages'
// /<repo>/ prefix. No broker, no network beyond the site's own files: every act is inert.

import type { RoomEvent, Snapshot } from "../src/conn";
import type { Summary } from "../src/summary";
import { initTheme } from "../src/theme";
import { summaryAt } from "./demo-summary";
import { mountPreviewPage } from "./page";
import { Replay } from "./replay";
import { mountReplayBar } from "./replay-bar";

const get = async <T>(path: string): Promise<T> => (await fetch(path)).json() as Promise<T>;

// rebased stamps an event with the moment it is shown, so "5 s ago" reads true. Step
// durations then measure playback time, not the recording's; the order is unchanged.
// An approval's expiry moves by the same offset, so it stays in the future.
const rebased = (e: RoomEvent): RoomEvent => {
  const now = Date.now();
  const payload = typeof e.payload?.expiresAt === "string"
    ? { ...e.payload, expiresAt: new Date(Date.parse(e.payload.expiresAt) + now - Date.parse(e.ts)).toISOString() } : e.payload;
  return { ...e, ts: new Date(now).toISOString(), payload };
};

async function run() {
  const [summary, snap, text] = await Promise.all([
    get<Summary>("fixtures/demo-summary.json"),
    get<Snapshot>("fixtures/demo-snapshot.json"),
    fetch("fixtures/demo-session.jsonl").then((r) => r.text()),
  ]);
  const events = text.trim().split("\n").map((l) => JSON.parse(l) as RoomEvent);
  const theme = initTheme();
  if (new URLSearchParams(location.search).get("theme") === "dark") document.documentElement.classList.add("dark");

  const shown: RoomEvent[] = [];
  const page = mountPreviewPage(document.getElementById("app")!, { snap, summary: summaryAt(summary, shown, events.length, false),
    // The snapshot stands before the first event, so every event replays onto it.
    count: 0, nothingSent: "Demo: nothing is sent.", sessionOpen: false, sessionLabel: "Session · driver: the factory", theme });

  const banner = document.createElement("div");
  banner.className = "banner demo-banner";
  const about = Object.assign(document.createElement("a"), { href: "https://github.com/Smana/agent-platform#readme", textContent: "What is this?" });
  banner.append("Demo: a recorded run, replayed. Nothing here reaches a cluster. ", about);
  page.header.after(banner);

  const replay = new Replay(events, (e) => {
    const r = rebased(e);
    shown.push(r);
    page.apply(r);
    page.setSummary(summaryAt(summary, shown, events.length, shown.length === events.length));
    page.settle();
  }, {
    onChange: () => {
      bar.refresh();
      page.setStatus(`demo · ${replay.applied} / ${events.length} events`);
    },
  });
  // The bar is the dock's first row, so it stays on screen while the run scrolls.
  const slot = document.createElement("div");
  const bar = mountReplayBar(slot, replay, events.length);
  page.footer.prepend(...slot.childNodes);
  page.setStatus(`demo · 0 / ${events.length} events`);
  page.settle();
  replay.play();
}

run().catch((e) => { document.body.textContent = `demo failed: ${e instanceof Error ? e.message : e}`; });
