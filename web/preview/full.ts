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
  // ?escalated: the factory's escalated room, as a maintainer meets it: the agent stopped, a
  // /factory retry is what comes next, the factory drives a public room.
  const params = new URLSearchParams(location.search);
  const status = params.has("escalated")
    ? { ...summary.status, phase: "Escalated", state: "", run: null, pr: null, budget: { usedTokens: 196_000, limitTokens: 3_000_000 },
      reason: "the agent stopped on an error", next: "a maintainer comments `/factory retry` on the issue to run it again, or pushes to the branch" }
    : summary.status;
  if (params.has("escalated")) Object.assign(snap, { driver: "system:factory", dataClass: "public" });
  const page = mountPreviewPage(document.getElementById("app")!, { snap, summary: { ...summary, status: { ...status, task: summary.room } },
    count: events.length, dashboards,
    nothingSent: "Offline preview: nothing was sent.", sessionOpen: new URLSearchParams(location.search).has("session"), theme });
  for (const e of events) page.apply(e);
  page.setStatus(`live · ${events.length} events (offline fixture)`);
  page.settle();
  // ?stress: a long Now line and a 64-hex digest in a message, the two phone bugs this fixture shows.
  if (new URLSearchParams(location.search).has("stress")) {
    const now = document.querySelector(".now-link");
    if (now) now.textContent = "Bash: rc=${PIPESTATUS[0]} ; if [ \"$rc\" -ne 0 ]; then echo failed with a long command line that keeps going and going past two lines of a phone strip so the clamp has to cut it, and then a third line keeps going on";
    const md = document.querySelector(".bubble .md");
    if (md) md.append(document.createTextNode(" sha256 401a9043056621bfce8b6261112f74236f83c0d5e7a9b1c2d3e4f5061728394a5b6c7d8"));
  }
}

run().catch((e) => { document.body.textContent = `preview failed: ${e instanceof Error ? e.message : e}`; });
