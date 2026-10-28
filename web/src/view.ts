// SPDX-License-Identifier: Apache-2.0

import { mountBrand } from "./brand";
import { api, nav, SignInRequired } from "./api";
import { maxRows } from "./limits";
import { ago } from "./summary";
import { mountThemeButton, type ThemeController } from "./theme";

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

// One row of GET /api/rooms (docs/api.md). lastEventAt is omitted for a room with no event.
export interface RoomRow {
  id: string; phase: string; owner: string; driver: string; dataClass: string; lastSeq: number;
  repository?: string; issue?: number; title?: string; task?: string; pr?: number; needsYou?: number;
  lastEventAt?: string;
  // status is the room page's status line, computed by the broker (summary.StatusLine): fixed words
  // and numbers, never agent text. Absent from an older broker, or when it does not know the state.
  status?: RowStatus;
}

export interface RowStatus { kind: string; icon: string; text: string }

// lastMs is a row's last event in ms, 0 when omitted, zero or unreadable (never 1970 on screen).
function lastMs(row: RoomRow): number {
  const t = Date.parse(row.lastEventAt ?? "");
  return isNaN(t) || t < Date.UTC(2000, 0, 1) ? 0 : t;
}

// sortRooms puts the rooms that need a human first, then the most recent activity, then the id.
export function sortRooms(rows: RoomRow[]): RoomRow[] {
  return [...rows].sort((a, b) =>
    (b.needsYou ?? 0) - (a.needsYou ?? 0) || lastMs(b) - lastMs(a) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
}

// rowCells is a room's table cells as text: the task it works on when it has one, else the room alone (#50).
export function rowCells(row: RoomRow, now: Date): { needs: string; task: string; repo: string; phase: string; pr: string; last: string; id: string } {
  const t = lastMs(row);
  return {
    needs: row.needsYou ? String(row.needsYou) : "",
    task: row.issue ? [`#${row.issue}`, row.title ?? ""].filter(Boolean).join(" ") : `Room ${row.id}`,
    repo: row.repository ?? "",
    phase: row.task || row.phase || "Open",
    pr: row.pr ? `#${row.pr}` : "",
    last: t ? ago(new Date(t).toISOString(), now.getTime()) : "",
    id: row.id,
  };
}

export interface RoomFilters { mine?: boolean; needsMe?: boolean; repo?: string }

function roomsURL(f: RoomFilters): string {
  const q = new URLSearchParams();
  if (f.mine) q.set("mine", "1");
  if (f.needsMe) q.set("needs_me", "1");
  if (f.repo) q.set("repo", f.repo);
  const s = q.toString();
  return s ? `/api/rooms?${s}` : "/api/rooms";
}

function cell(tag: "td" | "th", ...kids: (Node | string)[]): HTMLElement {
  const c = document.createElement(tag);
  if (tag === "th") c.scope = "col";
  c.append(...kids);
  return c;
}

// statusCell is the room page's status line, compact: the kind's accent, the icon and the words, as
// plain text with no live region (a list of them would chatter). The raw phase is the tooltip. A row
// without a status shows the phase as the room page's phase chip, as before.
function statusCell(st: RowStatus | undefined, phase: string): HTMLElement {
  const td = cell("td");
  td.className = "status";
  td.title = phase;
  if (!st?.kind) {
    const chip = document.createElement("span");
    chip.className = "chip chip-phase";
    chip.textContent = phase;
    td.append(chip);
    return td;
  }
  const line = document.createElement("span");
  line.className = "row-status";
  line.dataset.kind = st.kind;
  const icon = document.createElement("span");
  icon.className = "row-status-icon";
  icon.setAttribute("aria-hidden", "true"); // the words say it all
  icon.textContent = st.icon;
  const words = document.createElement("span");
  words.className = "row-status-words";
  words.textContent = st.text;
  line.append(icon, " ", words);
  td.append(line);
  return td;
}

function roomRow(row: RoomRow, now: Date): HTMLTableRowElement {
  const c = rowCells(row, now);
  const needs = cell("td");
  if (c.needs) {
    const pill = document.createElement("span");
    pill.className = "needs-pill";
    pill.textContent = c.needs;
    pill.title = `${c.needs} need you`;
    needs.append(pill);
  }
  // The task link is stretched over its row in CSS (.task-link::after), so the whole row
  // opens the room with no click handler: one link for the keyboard and assistive tech.
  const a = document.createElement("a");
  a.className = "task-link";
  a.href = `/r/${encodeURIComponent(row.id)}`;
  a.textContent = c.task;
  const repo = cell("td", c.repo);
  repo.className = "repo";
  const status = statusCell(row.status, c.phase);
  const last = cell("td", c.last || "—");
  const t = lastMs(row);
  if (t) last.title = new Date(t).toLocaleString();
  const code = document.createElement("code");
  code.textContent = c.id;
  const id = cell("td", code);
  id.className = "room-id";
  const tr = document.createElement("tr");
  tr.className = "room-row";
  tr.append(needs, cell("td", a), repo, status, cell("td", c.pr), last, id);
  return tr;
}

// listRooms renders the room list as a table, the filters and the new-room form, or why it
// cannot: a 401 signs in again; an expired session may also reach fetch as a redirect to
// the IdP, so a failed fetch or a body that is not JSON.
export async function listRooms(app: HTMLElement, get: typeof fetch = fetch, filters: RoomFilters = {}, theme?: ThemeController) {
  const fetchRooms = async (f: RoomFilters, say: (msg: string) => void = (m) => { app.textContent = m; }): Promise<RoomRow[] | undefined> => {
    try {
      const r = await api(roomsURL(f), {}, get);
      if (!r.ok) {
        say(`The room list is unavailable (${r.status}). Retry.`);
        return;
      }
      return (await r.json()) as RoomRow[];
    } catch (e) {
      say(e instanceof SignInRequired ? "signing in again…"
        : `The room list is unavailable (reload to sign in again): ${e instanceof Error ? e.message : String(e)}`);
    }
  };
  const first = await fetchRooms(filters, (m) => {
    const p = document.createElement("p");
    p.textContent = m;
    app.replaceChildren(p);
    if (m.startsWith("signing")) return;
    const retry = document.createElement("button");
    retry.type = "button";
    retry.textContent = "Retry";
    retry.onclick = () => void listRooms(app, get, filters, theme);
    app.append(retry);
  });
  if (!first) return;

  const header = document.createElement("header");
  header.className = "page-header";
  mountBrand(header);
  document.title = "Maku rooms";
  const h1 = document.createElement("h1");
  h1.className = "page-title";
  h1.textContent = "Rooms";
  header.append(h1);
  if (theme) mountThemeButton(header, theme);

  const check = (name: string, label: string, on?: boolean) => {
    const input = document.createElement("input");
    input.type = "checkbox";
    input.name = name;
    input.checked = !!on;
    const l = document.createElement("label");
    l.append(input, ` ${label}`);
    return { input, label: l };
  };
  const mine = check("mine", "mine", filters.mine);
  const needsMe = check("needsMe", "needs me", filters.needsMe);
  const repo = document.createElement("select");
  repo.name = "repo";
  repo.setAttribute("aria-label", "repository");
  const all = document.createElement("option");
  all.value = "";
  all.textContent = "all repositories";
  repo.append(all);
  for (const name of [...new Set(first.map((r) => r.repository).filter((r): r is string => !!r))].sort()) {
    const o = document.createElement("option");
    o.value = o.textContent = name;
    repo.append(o);
  }
  repo.value = filters.repo ?? "";
  const form = newRoomForm(get);
  form.hidden = true;
  const add = document.createElement("button");
  add.type = "button";
  add.className = "primary";
  add.dataset.act = "new-room";
  add.textContent = "New room";
  add.onclick = () => { form.hidden = !form.hidden; };
  const tools = document.createElement("div");
  tools.className = "list-tools";
  const notice = document.createElement("span");
  notice.className = "notice";
  notice.setAttribute("role", "status");
  // The filters on the left, New room, the page's one primary act, at the row's end.
  tools.append(mine.label, needsMe.label, repo, notice, add);

  const table = document.createElement("table");
  table.className = "rooms";
  table.setAttribute("aria-label", "Rooms");
  const head = document.createElement("tr");
  head.append(...["Needs you", "Task", "Repository", "Status", "PR", "Last activity", "Room"].map((h) => cell("th", h)));
  const thead = document.createElement("thead");
  thead.append(head);
  const tbody = document.createElement("tbody");
  table.append(thead, tbody);
  const fill = (rows: RoomRow[]) => {
    const now = new Date();
    if (rows.length > 0) return tbody.replaceChildren(...sortRooms(rows).map((r) => roomRow(r, now)));
    // An empty table says why: a filter that leaves nothing, or no room at all.
    const filtered = mine.input.checked || needsMe.input.checked || !!repo.value;
    const td = cell("td", filtered ? "No rooms match" : "No rooms you can read yet");
    td.setAttribute("colspan", String(head.childElementCount));
    const tr = document.createElement("tr");
    tr.append(td);
    tbody.replaceChildren(tr);
  };
  fill(first);

  let seq = 0; // a slow answer to an older filter never overwrites a newer one
  const refetch = async () => {
    const mark = ++seq;
    notice.textContent = "";
    const rows = await fetchRooms({ mine: mine.input.checked, needsMe: needsMe.input.checked, repo: repo.value || undefined },
      (m) => { if (mark === seq) notice.textContent = m; });
    if (rows && mark === seq) fill(rows);
  };
  // click, not change: a checkbox's change event needs a connected element; click fires either way.
  for (const e of [mine.input, needsMe.input]) e.addEventListener("click", () => void refetch());
  repo.addEventListener("change", () => void refetch());

  app.replaceChildren(header, tools, form, table, cliSetup(get));
}

export const roomId = /^[a-z2-7]{8}$/;

// installMaku downloads the maku release for this machine, checks it against the release's
// checksums and installs it: the broker's own release, or the latest when the broker is not a
// release build. The version lands in a pasted shell block: only a whole semver passes. macOS
// has shasum, not always sha256sum.
function installMaku(version?: string): string {
  const base = /^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(version ?? "")
    ? `https://github.com/Smana/maku/releases/download/${version}`
    : "https://github.com/Smana/maku/releases/latest/download";
  return [
    `os=$(uname -s | tr A-Z a-z); arch=$(uname -m); case $arch in x86_64) arch=amd64;; aarch64) arch=arm64;; esac`,
    `base=${base}`,
    `curl -fsSLO "$base/maku-$os-$arch" && curl -fsSLO "$base/maku.sha256"`,
    `command -v sha256sum >/dev/null && sum=sha256sum || sum="shasum -a 256"`,
    `grep " maku-$os-$arch$" maku.sha256 | $sum -c - && sudo install -m 0755 "maku-$os-$arch" /usr/local/bin/maku`,
  ].join("\n");
}

// continued is a command with each flag on its own line, continued with a backslash, so a
// long one reads at any width and still pastes and runs as is. One flag stays on its line.
function continued(cmd: string, flags: [string, string][]): string {
  const parts = flags.map(([f, v]) => `${f} ${v}`);
  return parts.length > 1 ? [cmd, ...parts.map((p) => `  ${p}`)].join(" \\\n") : [cmd, ...parts].join(" ");
}

// A setup step: its title, its command, and a muted note before or after it. A note is
// text and inline code, never markup.
interface SetupStep { title: string; code: string; before?: (string | { code: string })[]; after?: (string | { code: string })[] }

// setupSteps is what the fold shows for GET /api/roomctl's answer: a local broker's own
// steps, an OIDC broker's three, or, with no maku client, nothing.
function setupSteps(c: { url?: string; issuer?: string; clientID?: string; projectID?: string; version?: string; humanMode?: string }): SetupStep[] | undefined {
  const installStep: SetupStep = {
    title: "Install", code: installMaku(c.version),
    before: ["linux and macOS · amd64 and arm64 · checked against the release's sha256"],
  };
  if (c.humanMode === "local") {
    return [installStep,
      { title: "Point it at this broker", code: continued("maku configure", [["--url", `${c.url}`]]) },
      { title: "Start a run", code: continued("maku run", [["--repo", "<owner/name>"], ["--task", "'<what the run should do>'"]]) }];
  }
  if (!c.clientID) return;
  return [installStep,
    { title: "Point it at this broker", code: continued("maku configure",
      [["--url", `${c.url}`], ["--issuer", `${c.issuer}`], ["--client-id", c.clientID], ["--project-id", `${c.projectID}`]]) },
    { title: "Sign in", code: "maku login",
      after: ["Then: ", { code: "maku status" }, " · ", { code: "maku mcp" }, " for your agent's tools"] }];
}

// copiedFor is how long a Copy button says Copied before it reverts.
const copiedFor = 1500;

function note(parts: (string | { code: string })[]): HTMLElement {
  const p = document.createElement("p");
  p.className = "cli-note";
  p.append(...parts.map((x) => {
    if (typeof x === "string") return x;
    const c = document.createElement("code");
    c.textContent = x.code;
    return c;
  }));
  return p;
}

// stepItem renders one step: the command in its block, with a Copy button that writes the
// block's exact text. The label says Copied only once the clipboard took it; with no
// clipboard (an insecure origin) or a refused write it stays Copy, and the reader selects.
function stepItem(step: SetupStep, n: number, live: HTMLElement): HTMLLIElement {
  const li = document.createElement("li");
  li.className = "cli-step";
  const title = document.createElement("h3");
  title.className = "cli-step-title";
  title.id = `cli-step-${n}`;
  title.textContent = step.title;
  const pre = document.createElement("pre");
  pre.textContent = step.code;
  const copy = document.createElement("button");
  copy.type = "button";
  copy.className = "cli-copy";
  copy.textContent = "Copy";
  copy.setAttribute("aria-describedby", title.id);
  let revert: ReturnType<typeof setTimeout> | undefined;
  copy.onclick = () => {
    const write = navigator.clipboard?.writeText(step.code);
    write?.then(() => {
      clearTimeout(revert);
      copy.textContent = "Copied ✓";
      live.textContent = `Copied: ${step.title}`;
      revert = setTimeout(() => {
        copy.textContent = "Copy";
        if (live.textContent === `Copied: ${step.title}`) live.textContent = "";
      }, copiedFor);
    }, () => {});
  };
  const block = document.createElement("div");
  block.className = "cli-code";
  block.append(copy, pre); // the button floats top-right: the command's lines flow past it
  li.append(title);
  if (step.before) li.append(note(step.before));
  li.append(block);
  if (step.after) li.append(note(step.after));
  return li;
}

// cliSetup is the CLI setup view: maku's install, its configure line for this broker,
// then signing in (or, on a local broker, a first run), as numbered steps each with its
// own Copy button, read from GET /api/roomctl the first time it opens.
export function cliSetup(get: typeof fetch = fetch): HTMLDetailsElement {
  const d = document.createElement("details");
  d.className = "cli-setup";
  const summary = document.createElement("summary");
  summary.textContent = "Use maku from your terminal";
  const card = document.createElement("div");
  card.className = "cli-card";
  const live = document.createElement("span");
  live.className = "cli-live";
  live.setAttribute("role", "status");
  live.setAttribute("aria-live", "polite");
  d.append(summary, card, live);
  const say = (msg: string) => {
    const p = document.createElement("p");
    p.className = "cli-message";
    p.textContent = msg;
    card.replaceChildren(p);
  };
  let asked = false;
  d.addEventListener("toggle", async () => {
    if (!d.open || asked) return;
    asked = true;
    try {
      const r = await api("/api/roomctl", {}, get);
      const c = (await r.json()) as Parameters<typeof setupSteps>[0];
      const steps = r.ok ? setupSteps(c) : undefined;
      if (!steps) return say("maku is not set up on this broker: it has no maku client.");
      const ol = document.createElement("ol");
      ol.className = "cli-steps";
      ol.append(...steps.map((s, i) => stepItem(s, i + 1, live)));
      card.replaceChildren(ol);
    } catch (err) {
      asked = false;
      if (!(err instanceof SignInRequired)) say(`CLI setup unavailable: ${err instanceof Error ? err.message : String(err)}`);
    }
  });
  return d;
}

// Why POST /api/rooms refused (docs/api.md).
const createErrors: Record<number, string> = {
  400: "That data class or repository is not accepted (repository is owner/name).",
  404: "You cannot read that repository on GitHub, or it does not exist.",
  409: "A room with that id already exists. Retry.",
  429: "Too many actions at once. Wait a moment and retry.",
  500: "The broker could not create the room: see its logs.",
  503: "Rooms cannot be created right now, or your GitHub access cannot be checked. Retry.",
};

// newRoomForm creates a room owned and driven by the caller, then opens it.
export function newRoomForm(get: typeof fetch = fetch): HTMLFormElement {
  const form = document.createElement("form");
  form.className = "new-room";
  const dataClass = document.createElement("select");
  dataClass.name = "dataClass";
  dataClass.setAttribute("aria-label", "data class");
  dataClass.required = true;
  // No default, least of all public: the class outlives every choice made after it (R11).
  const choose = document.createElement("option");
  choose.value = "";
  choose.textContent = "choose a data class";
  choose.disabled = choose.defaultSelected = true;
  dataClass.append(choose);
  for (const v of ["internal", "public"]) {
    const o = document.createElement("option");
    o.value = o.textContent = v;
    dataClass.append(o);
  }
  const repository = document.createElement("input");
  repository.name = "repository";
  repository.placeholder = "owner/name";
  repository.setAttribute("aria-label", "repository, owner/name");
  // D7: who can read the repository on GitHub can read the room, so a room names one.
  repository.required = true;
  const submit = document.createElement("button");
  submit.type = "submit";
  submit.textContent = "new room";
  const notice = document.createElement("span");
  notice.className = "notice";
  const hint = document.createElement("small");
  hint.className = "hint";
  hint.id = "data-class-hint";
  hint.textContent = "The data class cannot change once the room exists. It sets the model route, the tools and the egress " +
    "its runs get; it does not change who can read the room. The repository does: anyone who can read it on GitHub can.";
  dataClass.setAttribute("aria-describedby", hint.id);
  repository.setAttribute("aria-describedby", hint.id);
  form.append(dataClass, repository, submit, notice, hint);
  form.onsubmit = async (e) => {
    e.preventDefault();
    if (!dataClass.value) { // required stops a click; this stops any other submit
      notice.textContent = "Choose a data class first.";
      return;
    }
    if (!repository.value.trim()) {
      notice.textContent = "Name the room's repository first (owner/name).";
      return;
    }
    const body = { dataClass: dataClass.value, repository: repository.value.trim() };
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
