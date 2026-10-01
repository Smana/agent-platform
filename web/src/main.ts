// SPDX-License-Identifier: Apache-2.0

import { api } from "./api";
import { RoomConnection, type Snapshot } from "./conn";
import { mountControls, rejection } from "./controls";
import { PendingActs } from "./pending";
import { renderEvent } from "./render";
import { RoomState } from "./room-state";
import { listRooms, RoomLog } from "./view";

const app = document.getElementById("app")!;

function room(id: string) {
  const header = document.createElement("header");
  const main = document.createElement("main");
  const section = document.createElement("section");
  section.className = "controls";
  const footer = document.createElement("footer");
  const counters = document.createElement("span");
  const notice = document.createElement("span");
  notice.className = "notice";
  const status = document.createElement("span");
  status.className = "status";
  footer.append(counters, notice, status);
  app.replaceChildren(header, main, section, footer);
  const log = new RoomLog(main);
  const state = new RoomState();
  let you: Snapshot["you"] | undefined;
  let controls: ReturnType<typeof mountControls> | undefined;
  const say = (text: string) => { notice.textContent = text; };
  let snap: Snapshot | undefined;
  const renderHeader = () => {
    if (snap) header.textContent = `${snap.roomId} · ${snap.phase} · ${snap.dataClass} · driver ${state.driver} · you: ${snap.you.role}${snap.you.approver ? " (approver)" : ""}`;
  };
  const conn = new RoomConnection(id, {
    // A state frame comes on every (re)connect: an invite may have changed the role.
    onState: (s: Snapshot, throughSeq: number) => {
      snap = s;
      state.reset(s, throughSeq);
      renderHeader();
      if (s.you.role === "watcher") {
        controls = you = undefined;
        section.replaceChildren();
        return;
      }
      if (you && controls) Object.assign(you, s.you);
      else controls = mountControls(section, sender, state, (you = { ...s.you }));
      controls.refresh();
    },
    onEvent: (e) => {
      log.put(e.seq, renderEvent(e));
      state.apply(e);
      renderHeader();
      controls?.refresh();
    },
    onAck: (f) => {
      pending.ack(f);
      say(f.rejected ? rejection(f.rejected) : "");
      controls?.onAck(f);
    },
    onRefused: () => void api("/api/rooms").catch(() => {}), // a 401 there signs in again
    onCounters: (c) => {
      counters.textContent = `seq ${c.last} · gaps ${c.gaps} · dups ${c.duplicates}`;
    },
    onStatus: (s) => {
      footer.dataset.status = s;
      status.textContent = s;
      pending.status(s);
    },
  });
  const pending = new PendingActs(say);
  const sender = {
    send: (f: Record<string, unknown>) => {
      const sent = pending.send(conn, f);
      say(sent ? "" : "Not connected: the action was not sent. Retry once the room is live.");
      return sent;
    },
  };
  conn.connect();
}

const m = location.pathname.match(/^\/r\/([a-z2-7]{8})$/);
if (m) room(m[1]); else void listRooms(app);
