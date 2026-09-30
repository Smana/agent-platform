// SPDX-License-Identifier: Apache-2.0

import { RoomConnection, type Snapshot } from "./conn";
import { renderEvent } from "./render";

const app = document.getElementById("app")!;

// One row of GET /api/rooms (docs/api.md).
interface RoomRow { id: string; phase: string; owner: string; driver: string; dataClass: string; lastSeq: number }

async function list() {
  const r = await fetch("/api/rooms");
  if (!r.ok) {
    app.textContent = `rooms unavailable: ${r.status}`;
    return;
  }
  const rooms = (await r.json()) as RoomRow[];
  const ul = document.createElement("ul");
  for (const row of rooms) {
    const li = document.createElement("li");
    const a = document.createElement("a");
    a.href = `/r/${encodeURIComponent(row.id)}`;
    a.textContent = `${row.id} · ${row.phase || "Open"} · ${row.lastSeq} events · owner ${row.owner}`;
    li.append(a);
    ul.append(li);
  }
  app.replaceChildren(ul);
}

function room(id: string) {
  const header = document.createElement("header");
  const log = document.createElement("main");
  const footer = document.createElement("footer");
  const counters = document.createElement("span");
  const status = document.createElement("span");
  status.className = "status";
  footer.append(counters, status);
  app.replaceChildren(header, log, footer);
  // A resume the broker clamped can resend a seq: its row is replaced, never doubled.
  const rows = new Map<number, HTMLElement>();
  const conn = new RoomConnection(id, {
    onState: (s: Snapshot) => {
      header.textContent = `${s.roomId} · ${s.phase} · ${s.dataClass} · driver ${s.driver} · you: ${s.you.role}${s.you.approver ? " (approver)" : ""}`;
    },
    onEvent: (e) => {
      const row = renderEvent(e);
      const old = rows.get(e.seq);
      if (old) old.replaceWith(row); else log.append(row);
      rows.set(e.seq, row);
      const c = conn.counters();
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
if (m) room(m[1]); else void list();
