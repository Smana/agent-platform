// SPDX-License-Identifier: Apache-2.0

// The offline full-page preview: the room page's regions, mounted as room.ts mounts
// them, fed by the recorded fixtures instead of a socket. `npm run preview`, then open
// /preview/full.html (?theme=dark for the dark palette). No broker, no network beyond
// the static fixtures.

import type { RoomEvent, Snapshot } from "../src/conn";
import type { Summary } from "../src/summary";
import { applyTheme, initTheme } from "../src/theme";
import { mountPreviewPage } from "./page";

const get = async <T>(path: string): Promise<T> => (await fetch(path)).json() as Promise<T>;

async function run() {
  const [summary, snap, text] = await Promise.all([
    get<Summary>("/test/fixtures/summary.json"),
    get<Snapshot>("/test/fixtures/snapshot.json"),
    fetch("/test/fixtures/live-session.jsonl").then((r) => r.text()),
  ]);
  const events = text.trim().split("\n").map((l) => JSON.parse(l) as RoomEvent);
  const theme = initTheme();
  const override = new URLSearchParams(location.search).get("theme");
  if (override === "light" || override === "dark") applyTheme(override);

  // Example dashboards (human.dashboards), filled with the fixture's room, run and task.
  const dashboards = [
    { label: "Room", url: "https://grafana.example.com/d/agent-room/agent-room?var-room={room}" },
    { label: "Run", url: "https://grafana.example.com/d/agent-run/agent-run?var-run={run}" },
    { label: "Task", url: "https://grafana.example.com/d/agent-factory/agent-factory?var-task={task}" },
  ];
  const page = mountPreviewPage(document.getElementById("app")!, { snap, summary: { ...summary, status: { ...summary.status, task: summary.room } },
    count: events.length, dashboards,
    nothingSent: "Offline preview: nothing was sent.", sessionOpen: new URLSearchParams(location.search).has("session"), theme });
  for (const e of events) page.apply(e);
  page.setStatus(`live · ${events.length} events (offline fixture)`);
  page.settle();
}

run().catch((e) => { document.body.textContent = `preview failed: ${e instanceof Error ? e.message : e}`; });
