// SPDX-License-Identifier: Apache-2.0

// The room page's regions for an offline page, mounted as room.ts mounts them, with no
// broker: every act is inert. The developer preview (full.ts) and the live demo (demo.ts)
// both mount through here, so the demo can never drift from what the preview shows.

import { mountBrand } from "../src/brand";
import type { RoomEvent, Snapshot } from "../src/conn";
import { mountComposer, mountControls } from "../src/controls";
import { mountFork } from "../src/fork";
import { mountNeeds } from "../src/needs";
import { RoomState } from "../src/room-state";
import { mountViews } from "../src/roomview";
import { mountStrip } from "../src/strip";
import { renderNotes, type Summary } from "../src/summary";
import { mountThemeButton, type ThemeController } from "../src/theme";

export interface PreviewPage {
  apply(e: RoomEvent): void;
  settle(): void; // refresh needs, controls, composer, strip and notes
  setSummary(s: Summary): void; // the summary the next settle() shows
  say(text: string): void; // the dock's act notice
  setStatus(text: string): void;
  header: HTMLElement;
  footer: HTMLElement;
}

export interface PreviewOptions {
  snap: Snapshot;
  summary: Summary;
  count: number; // the seq the snapshot stands at: events at or below it are already in it
  nothingSent: string; // what an act says, since it goes nowhere
  sessionOpen: boolean;
  sessionLabel?: string; // the session toggle's text
  homeHref?: string; // the brand link target; "/" unless the demo keeps it on its own page
  assetBase?: string; // where the brand marks are served; "/assets" unless the demo serves them relative
  theme: ThemeController; // initialised by the caller, so a ?theme override can follow it
}

export function mountPreviewPage(app: HTMLElement, o: PreviewOptions): PreviewPage {
  const { snap } = o;
  let summary = o.summary;
  const header = document.createElement("header");
  header.className = "page-header";
  mountBrand(header, o.assetBase ?? "/assets", o.homeHref ?? "/");
  const strip = mountStrip(header);
  const needsPanel = document.createElement("section");
  const lead = document.createElement("section");
  lead.className = "summary";
  const blocks = document.createElement("div");
  lead.append(blocks);
  const main = document.createElement("main");
  const section = document.createElement("section");
  section.className = "controls";
  // The session toggle, as room.ts mounts it.
  const session = document.createElement("details");
  session.className = "session";
  session.open = o.sessionOpen;
  session.append(Object.assign(document.createElement("summary"), { textContent: o.sessionLabel ?? "Session · driver: you · a run is on" }), section);
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
  meta.append(notice);
  mountThemeButton(meta, o.theme);
  meta.append(status);
  footer.append(session, composerEl, meta);
  app.replaceChildren(header, needsPanel, lead, main, forkPanel, footer);

  const say = (text: string) => { notice.textContent = text; };
  const sender = { send: () => { say(o.nothingSent); return false; } };
  const views = mountViews(main, (seq) => fork.open(seq), () => snap.you.principal);
  const state = new RoomState();
  state.reset(snap, o.count);
  const needs = mountNeeds(needsPanel, sender, state, () => snap.you, views);
  const you = { ...snap.you };
  const controls = mountControls(section, sender, state, you, say);
  const composer = mountComposer(composerEl, sender, state, you);
  composer.setLive(true);
  const fork = mountFork(forkPanel, sender, state);

  let applied = 0;
  return {
    apply(e) {
      views.apply(e);
      state.apply(e);
      views.counters(`seq ${++applied} · gaps 0 · dups 0`);
    },
    settle() {
      needs.refresh();
      needs.setSummary(summary);
      controls.setSummary(summary);
      controls.refresh();
      composer.refresh();
      renderNotes(blocks, summary);
      strip.update({ roomId: summary.room, snap, phase: state.phase, driver: state.driver, sealedBy: state.closed, sealed: state.sealed,
        summary, needs: needs.count(), connection: "live", now: state.latest(), running: state.running(), ran: state.ran() });
    },
    setSummary(s) { summary = s; },
    say,
    setStatus(text) { status.textContent = text; },
    header,
    footer,
  };
}
