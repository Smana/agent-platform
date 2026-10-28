// src/theme.ts
var THEME_STORAGE_KEY = "app-wizard:theme";
var DARK_QUERY = "(prefers-color-scheme: dark)";
function isThemeMode(value) {
  return value === "light" || value === "dark" || value === "system";
}
function readStoredMode(store = localStorage) {
  try {
    const stored = store.getItem(THEME_STORAGE_KEY);
    return isThemeMode(stored) ? stored : "system";
  } catch {
    return "system";
  }
}
function storeMode(mode, store = localStorage) {
  try {
    store.setItem(THEME_STORAGE_KEY, mode);
  } catch {
  }
}
function systemPrefersDark() {
  return typeof matchMedia === "function" && matchMedia(DARK_QUERY).matches;
}
function applyTheme(mode) {
  document.documentElement.classList.toggle("dark", mode === "dark" || mode === "system" && systemPrefersDark());
}
function nextMode(mode) {
  if (mode === "light") return "dark";
  if (mode === "dark") return "system";
  return "light";
}
function initTheme(store = localStorage) {
  let mode = readStoredMode(store);
  applyTheme(mode);
  if (typeof matchMedia === "function") {
    matchMedia(DARK_QUERY).addEventListener("change", () => {
      if (mode === "system") applyTheme(mode);
    });
  }
  return {
    get mode() {
      return mode;
    },
    cycle() {
      mode = nextMode(mode);
      storeMode(mode, store);
      applyTheme(mode);
      return mode;
    }
  };
}
function mountThemeButton(bar, ctl) {
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "theme-toggle";
  const show = () => {
    btn.textContent = `theme: ${ctl.mode}`;
    btn.title = "Light, dark, or follow the OS";
  };
  btn.onclick = () => {
    ctl.cycle();
    show();
  };
  show();
  bar.append(btn);
  return btn;
}

// src/brand.ts
var MARK = 32;
function mark(cls, src) {
  const img = document.createElement("img");
  img.className = cls;
  img.src = src;
  img.alt = "";
  img.setAttribute("aria-hidden", "true");
  img.width = MARK;
  img.height = MARK;
  return img;
}
function mountBrand(host, assetBase = "/assets", homeHref = "/") {
  const a = document.createElement("a");
  a.className = "brand";
  a.href = homeHref;
  a.setAttribute("aria-label", "Maku home");
  const word = document.createElement("span");
  word.className = "brand-wordmark";
  word.textContent = "maku";
  a.append(mark("brand-mark-light", `${assetBase}/svg/maku-mark-light.svg`), mark("brand-mark-dark", `${assetBase}/svg/maku-mark-dark.svg`), word);
  host.append(a);
  return a;
}

// src/api.ts
var nav = { go: (url) => location.assign(url) };
var SignInRequired = class extends Error {
  constructor() {
    super("signing in again");
  }
};
function signIn() {
  nav.go(`/oauth2/start?rd=${encodeURIComponent(location.pathname + location.search)}`);
}
async function api(path, init = {}, get2 = fetch) {
  const r = await get2(path, { credentials: "same-origin", ...init });
  if (r.status === 401) {
    signIn();
    throw new SignInRequired();
  }
  return r;
}

// src/summary.ts
function ago(iso, now) {
  const t = Date.parse(iso);
  if (isNaN(t)) return "";
  const s = Math.max(0, Math.floor((now - t) / 1e3));
  if (s < 60) return `${s} s ago`;
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  return `${Math.floor(s / 3600)} h ago`;
}

// src/view.ts
function lastMs(row) {
  const t = Date.parse(row.lastEventAt ?? "");
  return isNaN(t) || t < Date.UTC(2e3, 0, 1) ? 0 : t;
}
function sortRooms(rows2) {
  return [...rows2].sort((a, b) => (b.needsYou ?? 0) - (a.needsYou ?? 0) || lastMs(b) - lastMs(a) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
}
function rowCells(row, now) {
  const t = lastMs(row);
  return {
    needs: row.needsYou ? String(row.needsYou) : "",
    task: row.issue ? [`#${row.issue}`, row.title ?? ""].filter(Boolean).join(" ") : `Room ${row.id}`,
    repo: row.repository ?? "",
    phase: row.task || row.phase || "Open",
    pr: row.pr ? `#${row.pr}` : "",
    last: t ? ago(new Date(t).toISOString(), now.getTime()) : "",
    id: row.id
  };
}
function roomsURL(f) {
  const q = new URLSearchParams();
  if (f.mine) q.set("mine", "1");
  if (f.needsMe) q.set("needs_me", "1");
  if (f.repo) q.set("repo", f.repo);
  const s = q.toString();
  return s ? `/api/rooms?${s}` : "/api/rooms";
}
function cell(tag, ...kids) {
  const c = document.createElement(tag);
  if (tag === "th") c.scope = "col";
  c.append(...kids);
  return c;
}
function roomRow(row, now) {
  const c = rowCells(row, now);
  const needs = cell("td");
  if (c.needs) {
    const pill = document.createElement("span");
    pill.className = "needs-pill";
    pill.textContent = c.needs;
    pill.title = `${c.needs} need you`;
    needs.append(pill);
  }
  const a = document.createElement("a");
  a.className = "task-link";
  a.href = `/r/${encodeURIComponent(row.id)}`;
  a.textContent = c.task;
  const repo2 = cell("td", c.repo);
  repo2.className = "repo";
  const phase = document.createElement("span");
  phase.className = "chip chip-phase";
  phase.textContent = c.phase;
  const last = cell("td", c.last || "\u2014");
  const t = lastMs(row);
  if (t) last.title = new Date(t).toLocaleString();
  const code = document.createElement("code");
  code.textContent = c.id;
  const id = cell("td", code);
  id.className = "room-id";
  const tr = document.createElement("tr");
  tr.className = "room-row";
  tr.append(needs, cell("td", a), repo2, cell("td", phase), cell("td", c.pr), last, id);
  return tr;
}
async function listRooms(app, get2 = fetch, filters = {}, theme2) {
  const fetchRooms = async (f, say = (m) => {
    app.textContent = m;
  }) => {
    try {
      const r = await api(roomsURL(f), {}, get2);
      if (!r.ok) {
        say(`The room list is unavailable (${r.status}). Retry.`);
        return;
      }
      return await r.json();
    } catch (e) {
      say(e instanceof SignInRequired ? "signing in again\u2026" : `The room list is unavailable (reload to sign in again): ${e instanceof Error ? e.message : String(e)}`);
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
    retry.onclick = () => void listRooms(app, get2, filters, theme2);
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
  if (theme2) mountThemeButton(header, theme2);
  const check = (name, label, on) => {
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
  const repo2 = document.createElement("select");
  repo2.name = "repo";
  repo2.setAttribute("aria-label", "repository");
  const all = document.createElement("option");
  all.value = "";
  all.textContent = "all repositories";
  repo2.append(all);
  for (const name of [...new Set(first.map((r) => r.repository).filter((r) => !!r))].sort()) {
    const o = document.createElement("option");
    o.value = o.textContent = name;
    repo2.append(o);
  }
  repo2.value = filters.repo ?? "";
  const form = newRoomForm(get2);
  form.hidden = true;
  const add = document.createElement("button");
  add.type = "button";
  add.className = "primary";
  add.dataset.act = "new-room";
  add.textContent = "New room";
  add.onclick = () => {
    form.hidden = !form.hidden;
  };
  const tools = document.createElement("div");
  tools.className = "list-tools";
  const notice = document.createElement("span");
  notice.className = "notice";
  notice.setAttribute("role", "status");
  tools.append(mine.label, needsMe.label, repo2, notice, add);
  const table = document.createElement("table");
  table.className = "rooms";
  table.setAttribute("aria-label", "Rooms");
  const head = document.createElement("tr");
  head.append(...["Needs you", "Task", "Repository", "Phase", "PR", "Last activity", "Room"].map((h) => cell("th", h)));
  const thead = document.createElement("thead");
  thead.append(head);
  const tbody = document.createElement("tbody");
  table.append(thead, tbody);
  const fill = (rows2) => {
    const now = /* @__PURE__ */ new Date();
    if (rows2.length > 0) return tbody.replaceChildren(...sortRooms(rows2).map((r) => roomRow(r, now)));
    const filtered = mine.input.checked || needsMe.input.checked || !!repo2.value;
    const td = cell("td", filtered ? "No rooms match" : "No rooms you can read yet");
    td.setAttribute("colspan", String(head.childElementCount));
    const tr = document.createElement("tr");
    tr.append(td);
    tbody.replaceChildren(tr);
  };
  fill(first);
  let seq = 0;
  const refetch = async () => {
    const mark2 = ++seq;
    notice.textContent = "";
    const rows2 = await fetchRooms(
      { mine: mine.input.checked, needsMe: needsMe.input.checked, repo: repo2.value || void 0 },
      (m) => {
        if (mark2 === seq) notice.textContent = m;
      }
    );
    if (rows2 && mark2 === seq) fill(rows2);
  };
  for (const e of [mine.input, needsMe.input]) e.addEventListener("click", () => void refetch());
  repo2.addEventListener("change", () => void refetch());
  app.replaceChildren(header, tools, form, table, cliSetup(get2));
}
var roomId = /^[a-z2-7]{8}$/;
function installRoomctl(version) {
  const base2 = /^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(version ?? "") ? `https://github.com/Smana/agent-platform/releases/download/${version}` : "https://github.com/Smana/agent-platform/releases/latest/download";
  return [
    `os=$(uname -s | tr A-Z a-z); arch=$(uname -m); case $arch in x86_64) arch=amd64;; aarch64) arch=arm64;; esac`,
    `base=${base2}`,
    `curl -fsSLO "$base/roomctl-$os-$arch" && curl -fsSLO "$base/roomctl.sha256"`,
    `command -v sha256sum >/dev/null && sum=sha256sum || sum="shasum -a 256"`,
    `grep " roomctl-$os-$arch$" roomctl.sha256 | $sum -c - && sudo install -m 0755 "roomctl-$os-$arch" /usr/local/bin/roomctl`
  ].join("\n");
}
function cliSetup(get2 = fetch) {
  const d = document.createElement("details");
  d.className = "cli-setup";
  const summary = document.createElement("summary");
  summary.textContent = "CLI setup";
  const out = document.createElement("pre");
  d.append(summary, out);
  let asked = false;
  d.addEventListener("toggle", async () => {
    if (!d.open || asked) return;
    asked = true;
    try {
      const r = await api("/api/roomctl", {}, get2);
      const c = await r.json();
      out.textContent = r.ok && c.clientID ? `${installRoomctl(c.version)}
roomctl configure --url ${c.url} --issuer ${c.issuer} --client-id ${c.clientID} --project-id ${c.projectID}
roomctl login` : "roomctl is not set up on this broker: it has no roomctl client.";
    } catch (err) {
      asked = false;
      if (!(err instanceof SignInRequired)) out.textContent = `CLI setup unavailable: ${err instanceof Error ? err.message : String(err)}`;
    }
  });
  return d;
}
var createErrors = {
  400: "That data class or repository is not accepted (repository is owner/name).",
  404: "You cannot read that repository on GitHub, or it does not exist.",
  409: "A room with that id already exists. Retry.",
  429: "Too many actions at once. Wait a moment and retry.",
  500: "The broker could not create the room: see its logs.",
  503: "Rooms cannot be created right now, or your GitHub access cannot be checked. Retry."
};
function newRoomForm(get2 = fetch) {
  const form = document.createElement("form");
  form.className = "new-room";
  const dataClass = document.createElement("select");
  dataClass.name = "dataClass";
  dataClass.setAttribute("aria-label", "data class");
  dataClass.required = true;
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
  repository.required = true;
  const submit = document.createElement("button");
  submit.type = "submit";
  submit.textContent = "new room";
  const notice = document.createElement("span");
  notice.className = "notice";
  const hint = document.createElement("small");
  hint.className = "hint";
  hint.id = "data-class-hint";
  hint.textContent = "The data class cannot change once the room exists. It sets the model route, the tools and the egress its runs get; it does not change who can read the room. The repository does: anyone who can read it on GitHub can.";
  dataClass.setAttribute("aria-describedby", hint.id);
  repository.setAttribute("aria-describedby", hint.id);
  form.append(dataClass, repository, submit, notice, hint);
  form.onsubmit = async (e) => {
    e.preventDefault();
    if (!dataClass.value) {
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
      const r = await api("/api/rooms", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) }, get2);
      if (r.status !== 201) {
        notice.textContent = createErrors[r.status] ?? `The room was not created: ${r.status}`;
        return;
      }
      const { id } = await r.json();
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

// preview/list.ts
var ago2 = (min) => new Date(Date.now() - min * 6e4).toISOString();
var repo = "Smana/ai-native-ref";
var base = { owner: "system:factory", driver: "system:factory", dataClass: "internal", repository: repo };
var rows = [
  { ...base, id: "k2p9rt4v", phase: "Active", lastSeq: 318, issue: 152, title: "docs: a getting-started page for the platform", task: "Escalated", needsYou: 1, lastEventAt: ago2(4) },
  { ...base, id: "3kq7x2ma", phase: "Active", lastSeq: 142, issue: 147, title: "feat: retry the bridge upload on a 503", task: "Reviewing", pr: 148, lastEventAt: ago2(18) },
  { ...base, id: "d66mo4b4", phase: "Active", lastSeq: 95, issue: 144, title: "fix: the meter double-counts a resumed run", task: "Implementing", lastEventAt: ago2(52) },
  { ...base, id: "yacoriey", phase: "Idle", lastSeq: 70, issue: 139, title: "chore: bump the chart's CRD copy", task: "Merged", pr: 141, lastEventAt: ago2(60 * 5) },
  { ...base, id: "m4tq2zxa", phase: "Idle", lastSeq: 33, issue: 131, title: "docs: name the trace router's allowlist", task: "Closed", lastEventAt: ago2(60 * 26) },
  { ...base, id: "p7wn5c3e", phase: "Idle", owner: "human:2", driver: "human:2", lastSeq: 12, lastEventAt: ago2(60 * 24 * 3) }
];
var get = (async (url) => {
  if (url.startsWith("/api/roomctl")) return new Response(JSON.stringify({}), { status: 404 });
  const q = new URL(url, location.origin).searchParams;
  return new Response(JSON.stringify(rows.filter((r) => (!q.has("needs_me") || (r.needsYou ?? 0) > 0) && (!q.get("repo") || r.repository === q.get("repo")))));
});
var theme = initTheme();
var override = new URLSearchParams(location.search).get("theme");
if (override === "light" || override === "dark") applyTheme(override);
listRooms(document.getElementById("app"), get, {}, theme).catch((e) => {
  document.body.textContent = `preview failed: ${e instanceof Error ? e.message : e}`;
});
