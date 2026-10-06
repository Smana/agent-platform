// SPDX-License-Identifier: Apache-2.0

// The room page: the header, the chat and raw views, the controls and the
// footer, all fed by one RoomConnection.

import { api } from "./api";
import { RoomConnection, type Options, type Snapshot } from "./conn";
import { hasControls, mountControls, rejection } from "./controls";
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
  const main = document.createElement("main");
  const section = document.createElement("section");
  section.className = "controls";
  const footer = document.createElement("footer");
  const counters = document.createElement("span");
  const notice = document.createElement("span");
  notice.className = "notice";
  notice.setAttribute("role", "status");
  notice.setAttribute("aria-live", "polite");
  const status = document.createElement("span");
  status.className = "status";
  footer.append(counters, notice, status);
  app.replaceChildren(header, main, section, footer);
  const views = mountViews(main, header);
  mountThemeButton(header, theme);
  const state = new RoomState();
  let you: Snapshot["you"] | undefined;
  let controls: ReturnType<typeof mountControls> | undefined;
  const say = (text: string) => { notice.textContent = text; };
  // What an act said (a refusal, not connected, a lost act): an act or its ack clears
  // only that, never a notice the controls set meanwhile, such as a lost token's.
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
  const conn = new RoomConnection(id, {
    // A state frame comes on every (re)connect: an invite may have changed the role.
    onState: (s: Snapshot, throughSeq: number) => {
      snap = s;
      mark = throughSeq;
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
    },
    onEvent: (e) => {
      views.apply(e);
      state.apply(e);
      renderHeader();
      controls?.refresh();
      // A membership change naming you: the page projects none (R10). The reconnect's
      // state frame re-resolves you, and the broker re-checks every act anyway.
      if (e.type === "participant" && e.seq > mark && e.payload?.principal === snap?.you.principal) conn.resync();
    },
    onAck: (f) => {
      pending.ack(f);
      if (f.rejected) sayAct(rejection(f.rejected)); else clearAct();
      controls?.onAck(f);
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
      if (sent) clearAct(); else sayAct("Not connected: the action was not sent. Retry once the room is live.");
      return sent;
    },
  };
  conn.connect();
}
