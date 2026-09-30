// SPDX-License-Identifier: Apache-2.0

// The page keeps the newest maxRows rows; older ones leave the page, never the log.
export const maxRows = 5000;

// A room's rows by seq, in seq order on the page. A resume the broker clamped can
// resend a seq: its row is replaced in place, never doubled, and one no longer held
// goes back in order.
export class RoomLog {
  private rows = new Map<number, HTMLElement>();
  private seqs: number[] = []; // the held seqs, ascending
  constructor(private el: HTMLElement, private cap = maxRows) {}

  put(seq: number, row: HTMLElement) {
    const old = this.rows.get(seq);
    if (old) {
      old.replaceWith(row);
      this.rows.set(seq, row);
      return;
    }
    let i = this.seqs.length;
    while (i > 0 && this.seqs[i - 1] > seq) i--; // from the end: live events land there
    const next = i < this.seqs.length ? this.rows.get(this.seqs[i]) : undefined;
    if (next) next.before(row); else this.el.append(row);
    this.seqs.splice(i, 0, seq);
    this.rows.set(seq, row);
    while (this.seqs.length > this.cap) {
      const s = this.seqs.shift()!;
      this.rows.get(s)?.remove();
      this.rows.delete(s);
    }
  }
}

// One row of GET /api/rooms (docs/api.md).
interface RoomRow { id: string; phase: string; owner: string; driver: string; dataClass: string; lastSeq: number }

// listRooms renders the room list, or why it cannot: an expired session reaches
// fetch as a redirect to the IdP, so a failed fetch or a body that is not JSON.
export async function listRooms(app: HTMLElement, get: typeof fetch = fetch) {
  let rooms: RoomRow[];
  try {
    const r = await get("/api/rooms");
    if (!r.ok) {
      app.textContent = `rooms unavailable: ${r.status}`;
      return;
    }
    rooms = (await r.json()) as RoomRow[];
  } catch (e) {
    app.textContent = `rooms unavailable (reload to sign in again): ${e instanceof Error ? e.message : String(e)}`;
    return;
  }
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
