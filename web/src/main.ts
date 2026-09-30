// SPDX-License-Identifier: Apache-2.0

import { RoomConnection, type Snapshot } from "./conn";
import { renderEvent } from "./render";
import { listRooms, RoomLog } from "./view";

const app = document.getElementById("app")!;

function room(id: string) {
  const header = document.createElement("header");
  const main = document.createElement("main");
  const footer = document.createElement("footer");
  const counters = document.createElement("span");
  const status = document.createElement("span");
  status.className = "status";
  footer.append(counters, status);
  app.replaceChildren(header, main, footer);
  const log = new RoomLog(main);
  const conn = new RoomConnection(id, {
    onState: (s: Snapshot) => {
      header.textContent = `${s.roomId} · ${s.phase} · ${s.dataClass} · driver ${s.driver} · you: ${s.you.role}${s.you.approver ? " (approver)" : ""}`;
    },
    onEvent: (e) => log.put(e.seq, renderEvent(e)),
    onCounters: (c) => {
      counters.textContent = `seq ${c.last} · gaps ${c.gaps} · dups ${c.duplicates}`;
    },
    onStatus: (s) => {
      footer.dataset.status = s;
      status.textContent = s;
    },
  });
  conn.connect();
}

const m = location.pathname.match(/^\/r\/([a-z2-7]{8})$/);
if (m) room(m[1]); else void listRooms(app);
