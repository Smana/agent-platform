// SPDX-License-Identifier: Apache-2.0

// The room page: the header, the reconnecting banner, the needs-you panel, the notes,
// the chat and raw views, and the dock (the session's controls behind a toggle, the
// composer, the connection), all fed by one RoomConnection.

import { api } from "./api";
import { mountBrand } from "./brand";
import { focusRow } from "./chat";
import { RoomConnection, type Options, type Snapshot } from "./conn";
import { hasControls, mountComposer, mountControls, rejection, shortPrincipal } from "./controls";
import { mountFork } from "./fork";
import { mountNeeds } from "./needs";
import { changesSummary, fetchSummary, focusApproval, renderNotes, type Summary } from "./summary";
import { PendingActs } from "./pending";
import { RoomState } from "./room-state";
import { mountViews } from "./roomview";
import { mountStrip } from "./strip";
import { mountThemeButton, type ThemeController } from "./theme";

// mountRoom renders room id into app. get and socket are parameters so tests
// drive the page with no broker.
export function mountRoom(app: HTMLElement, id: string, theme: ThemeController, o: Options & { get?: typeof fetch } = {}) {
  const header = document.createElement("header");
  header.className = "page-header";
  mountBrand(header);
  const strip = mountStrip(header);
  // Not live: why, and when the page retries. Hidden while live.
  const banner = document.createElement("div");
  banner.className = "banner";
  banner.setAttribute("role", "status");
  banner.hidden = true;
  const needsPanel = document.createElement("section");
  const lead = document.createElement("section");
  lead.className = "summary";
  // The blocks and the error line are separate, so a failed refresh adds a line and
  // keeps what the page already shows. No aria-live: a rebuild would re-read every block.
  const blocks = document.createElement("div");
  const summaryError = document.createElement("p");
  summaryError.className = "summary-error";
  summaryError.setAttribute("role", "status");
  summaryError.hidden = true;
  lead.append(blocks, summaryError);
  // Until the first frame, three grey message shapes stand where the stream will be:
  // an empty page reads as broken (P9).
  const skeleton = document.createElement("div");
  skeleton.className = "skeleton";
  skeleton.setAttribute("aria-hidden", "true");
  for (let i = 0; i < 3; i++) skeleton.append(Object.assign(document.createElement("div"), { className: "sk-msg" }));
  const main = document.createElement("main");
  const section = document.createElement("section");
  section.className = "controls";
  // The session: Run, Driver, the queue and Room, behind a toggle in the dock (P7). Its
  // line says who drives and whether a run is on, so a closed toggle still answers both.
  const session = document.createElement("details");
  session.className = "session";
  session.hidden = true; // until the first frame says whether the viewer has controls
  const sessionLine = document.createElement("summary");
  session.append(sessionLine, section);
  const showSession = () => {
    session.hidden = !you || !hasControls(you);
    const holder = !state.driver ? "nobody" : state.driver === you?.principal ? "you" : shortPrincipal(state.driver);
    sessionLine.textContent = `Session · driver: ${holder} · ${state.running() ? "a run is on" : "no run"}`;
  };
  const forkPanel = document.createElement("section");
  // The dock: the session, the composer (mounted with the first state frame), then the
  // page's act notice, the theme button and the connection.
  const footer = document.createElement("footer");
  footer.className = "page-dock";
  const composerEl = document.createElement("div");
  composerEl.className = "composer";
  const meta = document.createElement("div");
  meta.className = "dock-meta";
  const notice = document.createElement("span");
  notice.className = "notice act-notice";
  notice.setAttribute("role", "status");
  notice.setAttribute("aria-live", "polite");
  const status = document.createElement("span");
  status.className = "status";
  status.textContent = "connecting…";
  meta.append(notice);
  mountThemeButton(meta, theme);
  meta.append(status);
  footer.append(session, composerEl, meta);
  app.replaceChildren(header, banner, needsPanel, lead, skeleton, main, forkPanel, footer);
  const views = mountViews(main, (seq) => fork.open(seq), () => snap?.you.principal);
  const state = new RoomState();
  let you: Snapshot["you"] | undefined;
  const sender = {
    send: (f: Record<string, unknown>) => {
      const sent = pending.send(conn, f);
      sayAct(sent ? "" : "Not connected: the action was not sent. Retry once the room is live.");
      return sent;
    },
  };
  const needsPanelView = mountNeeds(needsPanel, sender, state, () => snap?.you, views);
  let controls: ReturnType<typeof mountControls> | undefined;
  let composer: ReturnType<typeof mountComposer> | undefined;
  const say = (text: string) => { notice.textContent = text; };
  // What an act said (a refusal, not connected, a lost act): an ack clears only that,
  // never a notice the controls set meanwhile, such as a lost token's. A send that went
  // out supersedes every notice.
  let actSaid = "";
  const sayAct = (text: string) => { say((actSaid = text)); };
  const clearAct = () => { if (notice.textContent === actSaid) sayAct(""); };
  // Refusals in a row the room list answered without this room (R11); while any, the
  // page says it is checking the room rather than reconnecting.
  let absent = 0;
  let checking = false;
  const showBanner = () => {
    banner.hidden = connection === "live" && !checking;
    banner.textContent = banner.hidden ? "" : checking ? "Checking the room…" : connection.charAt(0).toUpperCase() + connection.slice(1);
  };
  let snap: Snapshot | undefined;
  let mark = 0; // the latest state frame's throughSeq
  let summary: Summary | undefined;
  let connection = "connecting…";
  let events = 0; // the counters' last seq, for the dock's "live · N events"
  const showStatus = () => { status.textContent = connection === "live" ? `live · ${events} events` : connection; };
  // The strip renders from the state frame alone until a summary arrives: a summary
  // that cannot be fetched leaves the room's id, phase, driver and you in place.
  const refreshStrip = (sum = summary) => strip.update({ roomId: id, snap, phase: state.phase, driver: state.driver,
    sealedBy: state.closed, sealed: state.sealed, summary: sum, needs: needsPanelView.count(), connection,
    now: state.latest(), running: state.running(), ran: state.ran() });
  // The pill takes the reader, and the keyboard focus, to the panel.
  strip.onNeedsClick(() => {
    needsPanel.scrollIntoView?.({ block: "start" });
    needsPanel.tabIndex = -1;
    needsPanel.focus({ preventScroll: true });
  });
  // The Now line's row: the chat's, else the raw log's (the chat folds a result into its
  // call's row, which carries the call's seq). A row gone at the cap finds nothing. The
  // row opens, and so does every fold or raw log around it; the focus moves to it.
  strip.onNowClick((seq) => {
    const row = main.querySelector<HTMLElement>(`.view-chat [data-seq="${seq}"]`) ?? main.querySelector<HTMLElement>(`[data-seq="${seq}"]`);
    if (row) focusRow(row);
  });
  // The strip's ages (the Now line's, the run's) move with the clock: one timer per
  // mounted page, rewriting them in place every second. It stops with the page, and on
  // its own once the page has left app (a re-mount or the "no such room" page replaced it).
  const ticker = setInterval(() => {
    if (!app.contains(header)) clearInterval(ticker);
    else refreshStrip();
  }, 1000);
  // The summary loads on mount, then at most once a second while events it folds arrive
  // (R27). A failed fetch shows its text on the error line and leaves the stream alone.
  let refreshTimer: ReturnType<typeof setTimeout> | undefined;
  let stopped = false;
  // Responses can overlap: only one newer than the last rendered one counts, and an
  // unchanged summary is not rebuilt (it would drop the focus on a link in it). A render
  // that throws keeps the blocks, shows its error, and counts as not shown, so the next
  // refresh tries again.
  let requested = 0;
  let rendered = 0;
  let shown = "";
  const loadSummary = () => {
    refreshTimer = undefined;
    const n = ++requested;
    fetchSummary(id, o.get).then((sum) => {
      if (stopped || n < rendered) return;
      rendered = n;
      const key = JSON.stringify(sum);
      if (key !== shown) {
        summary = sum; // first: a controls group mounted by a render below, or the next tick, reads it
        needsPanelView.setSummary(sum); // before the strip: its count reads it
        controls?.setSummary(sum);
        refreshStrip(sum); // a strip that throws leaves the notes as they were
        renderNotes(blocks, sum);
        shown = key;
      }
      summaryError.hidden = true;
    }).catch((e) => {
      if (stopped || n < rendered) return;
      summaryError.textContent = e instanceof Error ? e.message : String(e);
      summaryError.hidden = false;
    });
  };
  const scheduleSummary = () => { refreshTimer ??= setTimeout(loadSummary, 1000); };
  // The broker's link is <room>#<approvalId> (roomctl status, the summary's needsYou url).
  // The card appears once the replay delivers its request, so the landing is retried on
  // each panel refresh until it lands. A viewer who may not decide has a read-only card:
  // the landing shows it and focuses nothing.
  let landed = false;
  const land = () => {
    if (landed) return;
    let id = "";
    try { id = decodeURIComponent(location.hash.slice(1)); } catch { return; }
    if (id) landed = focusApproval(id.replace(/^approval-/, ""));
  };
  const onHash = () => { landed = false; land(); };
  addEventListener("hashchange", onHash);
  const conn = new RoomConnection(id, {
    // A state frame comes on every (re)connect: an invite may have changed the role.
    onState: (s: Snapshot, throughSeq: number) => {
      snap = s;
      mark = throughSeq;
      absent = 0; // the broker let us in: the room is readable
      checking = false;
      showBanner();
      state.reset(s, throughSeq);
      needsPanelView.refresh();
      refreshStrip();
      // One you for the page: the controls and the composer read it, and an invite that
      // changes the role updates it in place.
      if (you) Object.assign(you, s.you);
      else you = { ...s.you };
      if (!hasControls(s.you)) {
        controls = undefined;
        section.replaceChildren();
      } else {
        if (!controls) {
          controls = mountControls(section, sender, state, you, say);
          if (summary) controls.setSummary(summary);
        }
        controls.refresh();
      }
      if (!composer) {
        composer = mountComposer(composerEl, sender, state, you);
        composer.setLive(connection === "live");
      }
      composer.refresh();
      showSession();
      skeleton.hidden = true;
      land();
    },
    onEvent: (e) => {
      views.apply(e);
      if (changesSummary(e)) scheduleSummary();
      state.apply(e);
      needsPanelView.refresh();
      refreshStrip();
      controls?.refresh();
      composer?.refresh();
      showSession();
      skeleton.hidden = true;
      land();
      // A membership change naming you: the page projects none (R10). The reconnect's
      // state frame re-resolves you, and the broker re-checks every act anyway.
      if (e.type === "participant" && e.seq > mark && e.payload?.principal === snap?.you.principal) conn.resync();
    },
    onAck: (f) => {
      pending.ack(f);
      if (f.rejected) sayAct(rejection(f.rejected)); else clearAct();
      controls?.onAck(f);
      composer?.onAck(f);
      fork.onAck(f);
      needsPanelView.refresh();
    },
    // Refused before it opened: an expired session (a 401 from the list signs in
    // again), a broker restarting, or a room this caller cannot read. Only the list
    // tells the last apart, and it says the same of a room that does not exist: no
    // existence oracle (R11). A room just created is refused until roomctrl writes its
    // log row, and the list reads a replica's informer cache, which may lag the POST:
    // only three absent answers in a row, a backoff apart, stop the page.
    onRefused: () => void api("/api/rooms", {}, o.get).then(async (r) => {
      if (!r.ok) return;
      const rooms = (await r.json()) as { id: string }[];
      if (rooms.some((row) => row.id === id)) {
        absent = 0;
        checking = false;
        showBanner();
        return;
      }
      if (++absent < 3) {
        checking = true;
        showBanner();
        return;
      }
      conn.stop();
      stopped = true;
      removeEventListener("hashchange", onHash);
      clearTimeout(refreshTimer);
      clearInterval(ticker);
      const back = document.createElement("a");
      back.href = "/";
      back.textContent = "See the rooms you can read";
      const gone = document.createElement("p");
      gone.className = "gone";
      gone.append("No such room, or you cannot read it. ", back, ".");
      app.replaceChildren(gone);
    }).catch(() => {}),
    onCounters: (c) => {
      events = c.last;
      views.counters(`seq ${c.last} · gaps ${c.gaps} · dups ${c.duplicates}`);
      showStatus();
    },
    onStatus: (s) => {
      footer.dataset.status = s.startsWith("reconnecting") ? "reconnecting" : s;
      connection = s;
      showStatus();
      showBanner();
      composer?.setLive(s === "live");
      refreshStrip();
      pending.status(s);
    },
  }, o);
  const pending = new PendingActs(sayAct);
  const fork = mountFork(forkPanel, sender, state);
  refreshStrip();
  loadSummary();
  conn.connect();
}
