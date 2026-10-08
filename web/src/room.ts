// SPDX-License-Identifier: Apache-2.0

// The room page: the header, the chat and raw views, the controls and the
// footer, all fed by one RoomConnection.

import { api } from "./api";
import { RoomConnection, type Options, type Snapshot } from "./conn";
import { hasControls, mountControls, rejection } from "./controls";
import { mountFork } from "./fork";
import { fetchSummary, focusApproval, renderSummary } from "./summary";
import { PendingActs } from "./pending";
import { RoomState } from "./room-state";
import { mountViews } from "./roomview";
import { mountThemeButton, type ThemeController } from "./theme";

// mountRoom renders room id into app. get and socket are parameters so tests
// drive the page with no broker.
export function mountRoom(app: HTMLElement, id: string, theme: ThemeController, o: Options & { get?: typeof fetch } = {}) {
  const header = document.createElement("header");
  const title = document.createElement("span");
  title.className = "title";
  header.append(title);
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
  const main = document.createElement("main");
  const section = document.createElement("section");
  section.className = "controls";
  const forkPanel = document.createElement("section");
  const footer = document.createElement("footer");
  const counters = document.createElement("span");
  const notice = document.createElement("span");
  notice.className = "notice";
  notice.setAttribute("role", "status");
  notice.setAttribute("aria-live", "polite");
  const status = document.createElement("span");
  status.className = "status";
  footer.append(counters, notice, status);
  app.replaceChildren(header, lead, main, section, forkPanel, footer);
  const views = mountViews(main, (seq) => fork.open(seq));
  mountThemeButton(header, theme);
  const state = new RoomState();
  let you: Snapshot["you"] | undefined;
  let controls: ReturnType<typeof mountControls> | undefined;
  const say = (text: string) => { notice.textContent = text; };
  // What an act said (a refusal, not connected, a lost act): an ack clears only that,
  // never a notice the controls set meanwhile, such as a lost token's. A send that went
  // out supersedes every notice.
  let actSaid = "";
  const sayAct = (text: string) => { say((actSaid = text)); };
  const clearAct = () => { if (notice.textContent === actSaid) sayAct(""); };
  // Refusals in a row the room list answered without this room (R11).
  let absent = 0;
  let snap: Snapshot | undefined;
  let mark = 0; // the latest state frame's throughSeq
  const renderHeader = () => {
    if (snap) title.textContent = `${snap.roomId} · ${state.phase} · ${snap.dataClass} · driver ${state.driver} · you: ${snap.you.role}${snap.you.approver ? " (approver)" : ""}`;
  };
  // The summary loads on mount, then at most once a second while events arrive. A failed
  // fetch shows its text in the blocks' place and leaves the stream alone.
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
        renderSummary(blocks, sum);
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
  // each controls refresh until it lands. A non-approver has no card: nothing happens.
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
      state.reset(s, throughSeq);
      renderHeader();
      if (!hasControls(s.you)) {
        controls = you = undefined;
        section.replaceChildren();
        return;
      }
      if (you && controls) Object.assign(you, s.you);
      else controls = mountControls(section, sender, state, (you = { ...s.you }), say);
      controls.refresh();
      land();
    },
    onEvent: (e) => {
      views.apply(e);
      scheduleSummary();
      state.apply(e);
      renderHeader();
      controls?.refresh();
      land();
      // A membership change naming you: the page projects none (R10). The reconnect's
      // state frame re-resolves you, and the broker re-checks every act anyway.
      if (e.type === "participant" && e.seq > mark && e.payload?.principal === snap?.you.principal) conn.resync();
    },
    onAck: (f) => {
      pending.ack(f);
      if (f.rejected) sayAct(rejection(f.rejected)); else clearAct();
      controls?.onAck(f);
      fork.onAck(f);
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
        return;
      }
      if (++absent < 3) return;
      conn.stop();
      stopped = true;
      removeEventListener("hashchange", onHash);
      clearTimeout(refreshTimer);
      const back = document.createElement("a");
      back.href = "/";
      back.textContent = "See the rooms you can read";
      const gone = document.createElement("p");
      gone.className = "gone";
      gone.append("No such room, or you cannot read it. ", back, ".");
      app.replaceChildren(gone);
    }).catch(() => {}),
    onCounters: (c) => {
      counters.textContent = `seq ${c.last} · gaps ${c.gaps} · dups ${c.duplicates}`;
    },
    onStatus: (s) => {
      footer.dataset.status = s;
      status.textContent = s;
      pending.status(s);
    },
  }, o);
  const pending = new PendingActs(sayAct);
  const sender = {
    send: (f: Record<string, unknown>) => {
      const sent = pending.send(conn, f);
      sayAct(sent ? "" : "Not connected: the action was not sent. Retry once the room is live.");
      return sent;
    },
  };
  const fork = mountFork(forkPanel, sender, state);
  loadSummary();
  conn.connect();
}
