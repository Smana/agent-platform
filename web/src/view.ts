// SPDX-License-Identifier: Apache-2.0

import { api, nav, SignInRequired } from "./api";

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

// listRooms renders the room list and the new-room form, or why it cannot: a 401
// signs in again; an expired session may also reach fetch as a redirect to the IdP,
// so a failed fetch or a body that is not JSON.
export async function listRooms(app: HTMLElement, get: typeof fetch = fetch) {
  let rooms: RoomRow[];
  try {
    const r = await api("/api/rooms", {}, get);
    if (!r.ok) {
      app.textContent = `rooms unavailable: ${r.status}`;
      return;
    }
    rooms = (await r.json()) as RoomRow[];
  } catch (e) {
    app.textContent = e instanceof SignInRequired ? "signing in again…"
      : `rooms unavailable (reload to sign in again): ${e instanceof Error ? e.message : String(e)}`;
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
  app.replaceChildren(newRoomForm(get), ul);
}

const roomId = /^[a-z2-7]{8}$/;

// Why POST /api/rooms refused (docs/api.md).
const createErrors: Record<number, string> = {
  400: "That data class or repository is not accepted (repository is owner/name).",
  429: "Too many actions at once. Wait a moment and retry.",
  503: "Rooms cannot be created right now. Retry.",
};

// newRoomForm creates a room owned and driven by the caller, then opens it.
export function newRoomForm(get: typeof fetch = fetch): HTMLFormElement {
  const form = document.createElement("form");
  form.className = "new-room";
  const dataClass = document.createElement("select");
  dataClass.name = "dataClass";
  for (const v of ["internal", "public"]) {
    const o = document.createElement("option");
    o.value = o.textContent = v;
    dataClass.append(o);
  }
  const repository = document.createElement("input");
  repository.name = "repository";
  repository.placeholder = "owner/name (default: the CRD's)";
  const submit = document.createElement("button");
  submit.type = "submit";
  submit.textContent = "new room";
  const notice = document.createElement("span");
  notice.className = "notice";
  form.append(dataClass, repository, submit, notice);
  form.onsubmit = async (e) => {
    e.preventDefault();
    const body: Record<string, string> = { dataClass: dataClass.value };
    if (repository.value.trim()) body.repository = repository.value.trim();
    notice.textContent = "";
    try {
      const r = await api("/api/rooms", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) }, get);
      if (r.status !== 201) {
        notice.textContent = createErrors[r.status] ?? `The room was not created: ${r.status}`;
        return;
      }
      const { id } = (await r.json()) as { id?: unknown };
      if (typeof id !== "string" || !roomId.test(id)) {
        notice.textContent = "The broker answered an unexpected room id.";
        return;
      }
      nav.go(`/r/${id}`);
    } catch (err) {
      if (!(err instanceof SignInRequired)) notice.textContent = `The room was not created: ${err instanceof Error ? err.message : String(err)}`;
    }
  };
  return form;
}
