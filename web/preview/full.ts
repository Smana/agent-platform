// SPDX-License-Identifier: Apache-2.0

// The offline full-page preview: the room page's regions, mounted as room.ts mounts
// them, fed by the recorded fixtures instead of a socket. `npm run preview`, then open
// /preview/full.html (?theme=dark for the dark palette). No broker, no network beyond
// the static fixtures.

import type { RoomEvent, Snapshot } from "../src/conn";
import { mountComposer, mountControls } from "../src/controls";
import { mountFork } from "../src/fork";
import { mountNeeds } from "../src/needs";
import { RoomState } from "../src/room-state";
import { mountViews } from "../src/roomview";
import { mountStrip } from "../src/strip";
import { renderNotes, type Summary } from "../src/summary";
import { initTheme, mountThemeButton } from "../src/theme";

const get = async <T>(path: string): Promise<T> => (await fetch(path)).json() as Promise<T>;

async function run() {
  const [summary, snap, text] = await Promise.all([
    get<Summary>("/test/fixtures/summary.json"),
    get<Snapshot>("/test/fixtures/snapshot.json"),
    fetch("/test/fixtures/live-session.jsonl").then((r) => r.text()),
  ]);
  const events = text.trim().split("\n").map((l) => JSON.parse(l) as RoomEvent);
  const theme = initTheme();
  if (new URLSearchParams(location.search).get("theme") === "dark") document.documentElement.classList.add("dark");

  const app = document.getElementById("app")!;
  const header = document.createElement("header");
  header.className = "page-header";
  const strip = mountStrip(header);
  const needsPanel = document.createElement("section");
  const lead = document.createElement("section");
  lead.className = "summary";
  const blocks = document.createElement("div");
  lead.append(blocks);
  const main = document.createElement("main");
  const section = document.createElement("section");
  section.className = "controls";
  const forkPanel = document.createElement("section");
  const footer = document.createElement("footer");
  footer.className = "page-dock";
  footer.dataset.status = "live";
  const composerEl = document.createElement("div");
  composerEl.className = "composer";
  const meta = document.createElement("div");
  meta.className = "dock-meta";
  const notice = document.createElement("span");
  notice.className = "notice act-notice";
  const status = document.createElement("span");
  status.className = "status";
  status.textContent = `live · ${events.length} events (offline fixture)`;
  meta.append(notice);
  mountThemeButton(meta, theme);
  meta.append(status);
  footer.append(composerEl, meta);
  app.replaceChildren(header, needsPanel, lead, main, section, forkPanel, footer);

  // Acts go nowhere: the preview has no broker.
  const sender = { send: () => { notice.textContent = "Offline preview: nothing was sent."; return false; } };
  const views = mountViews(main, (seq) => fork.open(seq), () => snap.you.principal);
  const state = new RoomState();
  state.reset(snap, events.length);
  const needs = mountNeeds(needsPanel, sender, state, () => snap.you, views);
  const you = { ...snap.you };
  const controls = mountControls(section, sender, state, you, (t) => { notice.textContent = t; });
  const composer = mountComposer(composerEl, sender, state, you);
  composer.setLive(true);
  const fork = mountFork(forkPanel, sender, state);

  for (const e of events) {
    views.apply(e);
    state.apply(e);
  }
  views.counters(`seq ${events.length} · gaps 0 · dups 0`);
  needs.refresh();
  needs.setSummary(summary);
  controls.setSummary(summary);
  controls.refresh();
  composer.refresh();
  renderNotes(blocks, summary);
  strip.update({ roomId: summary.room, snap, phase: state.phase, driver: state.driver, sealedBy: state.closed, sealed: state.sealed,
    summary, needs: needs.count(), connection: "live", now: state.latest(), running: state.running(), ran: state.ran() });
}

run().catch((e) => { document.body.textContent = `preview failed: ${e instanceof Error ? e.message : e}`; });
