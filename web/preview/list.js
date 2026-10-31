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

// src/autorefresh.ts
var REFRESH_STORAGE_KEY = "maku:list-refresh";
var DEFAULT_SECONDS = 10;
var HIDDEN_SECONDS = 60;
var CHOICES = [[0, "Off"], [5, "5 s"], [10, "10 s"], [30, "30 s"], [60, "1 min"]];
var realClock = {
  now: () => Date.now(),
  set: (f, ms) => setTimeout(f, ms),
  clear: (h) => clearTimeout(h)
};
function readInterval(store = localStorage) {
  try {
    const n = Number(store.getItem(REFRESH_STORAGE_KEY) ?? NaN);
    return CHOICES.some(([s]) => s === n) ? n : DEFAULT_SECONDS;
  } catch {
    return DEFAULT_SECONDS;
  }
}
function storeInterval(seconds, store = localStorage) {
  try {
    store.setItem(REFRESH_STORAGE_KEY, String(seconds));
  } catch {
  }
}
function mountAutoRefresh(host, refresh, o = {}) {
  const clock = o.clock ?? realClock;
  const doc = o.doc ?? document;
  const store = o.store ?? localStorage;
  const time = o.time ?? ((ms) => new Date(ms).toLocaleTimeString());
  const box = document.createElement("span");
  box.className = "refresh";
  const select = document.createElement("select");
  select.name = "refresh";
  select.setAttribute("aria-label", "Auto-refresh interval");
  for (const [s, label] of CHOICES) {
    const opt = document.createElement("option");
    opt.value = String(s);
    opt.textContent = label;
    select.append(opt);
  }
  const pause = document.createElement("button");
  pause.type = "button";
  pause.className = "refresh-pause";
  const status = document.createElement("span");
  status.className = "refresh-status";
  status.setAttribute("role", "status");
  box.append(status, select, pause);
  host.append(box);
  let seconds = readInterval(store);
  select.value = String(seconds);
  let paused = false, running = false, stopped = false, failed = false;
  let last = clock.now();
  let updated = 0;
  let timer;
  const show = () => {
    pause.hidden = seconds === 0;
    pause.textContent = paused ? "\u25B6 Resume" : "\u23F8 Pause";
    pause.setAttribute("aria-pressed", String(paused));
    const at = updated ? time(updated) : "";
    const lead = paused ? "Paused" : failed ? "Refresh failed" : "";
    status.textContent = lead && at ? `${lead} \xB7 updated ${at}` : lead || (at ? `Updated ${at}` : "");
    if (failed) status.dataset.failed = "true";
    else delete status.dataset.failed;
  };
  const schedule = () => {
    if (timer !== void 0) clock.clear(timer);
    timer = void 0;
    if (stopped || paused || running || seconds === 0) return;
    const every = doc.visibilityState === "hidden" ? Math.max(seconds, HIDDEN_SECONDS) : seconds;
    timer = clock.set(() => void tick(), Math.max(0, last + every * 1e3 - clock.now()));
  };
  const tick = async () => {
    timer = void 0;
    running = true;
    const ok = await refresh().catch(() => false);
    running = false;
    last = clock.now();
    failed = !ok;
    if (ok) updated = last;
    show();
    schedule();
  };
  select.addEventListener("change", () => {
    seconds = Number(select.value);
    storeInterval(seconds, store);
    show();
    schedule();
  });
  pause.onclick = () => {
    paused = !paused;
    show();
    schedule();
  };
  const onVisibility = () => schedule();
  doc.addEventListener("visibilitychange", onVisibility);
  show();
  schedule();
  return {
    stop() {
      stopped = true;
      if (timer !== void 0) clock.clear(timer);
      timer = void 0;
      doc.removeEventListener("visibilitychange", onVisibility);
    }
  };
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
function statusCell(st, phase) {
  const td = cell("td");
  td.className = "status";
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
  const icon2 = document.createElement("span");
  icon2.className = "row-status-icon";
  icon2.setAttribute("aria-hidden", "true");
  icon2.textContent = st.icon;
  const words = document.createElement("span");
  words.className = "row-status-words";
  words.textContent = st.text;
  line.append(icon2, " ", words);
  td.title = phase;
  td.append(line);
  return td;
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
  const status = statusCell(row.status, c.phase);
  const last = cell("td", c.last || "\u2014");
  const t = lastMs(row);
  if (t) last.title = new Date(t).toLocaleString();
  const code = document.createElement("code");
  code.textContent = c.id;
  const id = cell("td", code);
  id.className = "room-id";
  const tr = document.createElement("tr");
  tr.className = "room-row";
  tr.append(needs, cell("td", a), repo2, status, cell("td", c.pr), last, id);
  return tr;
}
async function listRooms(app, get2 = fetch, filters = {}, theme2, refresh = {}) {
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
    retry.onclick = () => void listRooms(app, get2, filters, theme2, refresh);
    app.append(retry);
  });
  if (!first) return;
  const header = document.createElement("header");
  header.className = "page-header";
  mountBrand(header);
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
  tools.append(mine.label, needsMe.label, repo2, notice);
  const table = document.createElement("table");
  table.className = "rooms";
  table.setAttribute("aria-label", "Rooms");
  const head = document.createElement("tr");
  head.append(...["Needs you", "Task", "Repository", "Status", "PR", "Last activity", "Room"].map((h) => cell("th", h)));
  const thead = document.createElement("thead");
  thead.append(head);
  const tbody = document.createElement("tbody");
  table.append(thead, tbody);
  let shown = "";
  const fill = (rows2) => {
    const needing = rows2.filter((r) => (r.needsYou ?? 0) > 0).length;
    document.title = needing > 0 ? `(${needing}) Maku rooms` : "Maku rooms";
    const now = /* @__PURE__ */ new Date();
    const sorted = sortRooms(rows2);
    const filtered = mine.input.checked || needsMe.input.checked || !!repo2.value;
    const key = JSON.stringify([filtered, sorted.map((r) => [rowCells(r, now), r.status])]);
    if (key === shown) return;
    shown = key;
    const focused = document.activeElement instanceof HTMLAnchorElement && tbody.contains(document.activeElement) ? document.activeElement.getAttribute("href") : null;
    if (rows2.length > 0) {
      tbody.replaceChildren(...sorted.map((r) => roomRow(r, now)));
      if (focused) [...tbody.querySelectorAll("a")].find((l) => l.getAttribute("href") === focused)?.focus();
      return;
    }
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
    return !!rows2;
  };
  mountAutoRefresh(tools, refetch, refresh);
  tools.append(add);
  for (const e of [mine.input, needsMe.input]) e.addEventListener("click", () => void refetch());
  repo2.addEventListener("change", () => void refetch());
  app.replaceChildren(header, tools, form, table, cliSetup(get2));
}
var roomId = /^[a-z2-7]{8}$/;
function installMaku(version) {
  const base2 = /^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(version ?? "") ? `https://github.com/Smana/maku/releases/download/${version}` : "https://github.com/Smana/maku/releases/latest/download";
  return [
    `os=$(uname -s | tr A-Z a-z); arch=$(uname -m); case $arch in x86_64) arch=amd64;; aarch64) arch=arm64;; esac`,
    `base=${base2}`,
    `curl -fsSLO "$base/maku-$os-$arch" && curl -fsSLO "$base/maku.sha256"`,
    `command -v sha256sum >/dev/null && sum=sha256sum || sum="shasum -a 256"`,
    `grep " maku-$os-$arch$" maku.sha256 | $sum -c - && sudo install -m 0755 "maku-$os-$arch" /usr/local/bin/maku`
  ].join("\n");
}
function continued(cmd, flags) {
  const parts = flags.map(([f, v]) => `${f} ${v}`);
  return parts.length > 1 ? [cmd, ...parts.map((p) => `  ${p}`)].join(" \\\n") : [cmd, ...parts].join(" ");
}
function setupSteps(c) {
  const installStep = {
    title: "Install",
    code: installMaku(c.version),
    before: ["linux and macOS \xB7 amd64 and arm64 \xB7 checked against the release's sha256"]
  };
  if (c.humanMode === "local") {
    return [
      installStep,
      { title: "Point it at this broker", code: continued("maku configure", [["--url", `${c.url}`]]) },
      { title: "Start a run", code: continued("maku run", [["--repo", "<owner/name>"], ["--task", "'<what the run should do>'"]]) }
    ];
  }
  if (!c.clientID) return;
  return [
    installStep,
    { title: "Point it at this broker", code: continued(
      "maku configure",
      [["--url", `${c.url}`], ["--issuer", `${c.issuer}`], ["--client-id", c.clientID], ["--project-id", `${c.projectID}`]]
    ) },
    {
      title: "Sign in",
      code: "maku login",
      after: ["Then: ", { code: "maku status" }, " \xB7 ", { code: "maku mcp" }, " for your agent's tools"]
    }
  ];
}
var copiedFor = 1500;
function note(parts) {
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
function stepItem(step, n, live) {
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
  let revert;
  copy.onclick = () => {
    const write = navigator.clipboard?.writeText(step.code);
    write?.then(() => {
      clearTimeout(revert);
      copy.textContent = "Copied \u2713";
      live.textContent = `Copied: ${step.title}`;
      revert = setTimeout(() => {
        copy.textContent = "Copy";
        if (live.textContent === `Copied: ${step.title}`) live.textContent = "";
      }, copiedFor);
    }, () => {
    });
  };
  const block = document.createElement("div");
  block.className = "cli-code";
  block.append(copy, pre);
  li.append(title);
  if (step.before) li.append(note(step.before));
  li.append(block);
  if (step.after) li.append(note(step.after));
  return li;
}
function cliSetup(get2 = fetch) {
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
  const say = (msg) => {
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
      const r = await api("/api/roomctl", {}, get2);
      const c = await r.json();
      const steps = r.ok ? setupSteps(c) : void 0;
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
  { ...base, id: "k2p9rt4v", phase: "Active", lastSeq: 318, issue: 152, title: "docs: a getting-started page for the platform", task: "Escalated", needsYou: 1, lastEventAt: ago2(4), status: { kind: "waiting-you", icon: "\u23F3", text: "Waiting for you \u2014 approve a pending action" } },
  { ...base, id: "3kq7x2ma", phase: "Active", lastSeq: 142, issue: 147, title: "feat: retry the bridge upload on a 503", task: "Reviewing", pr: 148, lastEventAt: ago2(18), status: { kind: "finished", icon: "\u2713", text: "Finished \u2014 PR #148 opened, handed to the reviewer" } },
  { ...base, id: "d66mo4b4", phase: "Active", lastSeq: 95, issue: 144, title: "fix: the meter double-counts a resumed run", task: "Implementing", lastEventAt: ago2(52), status: { kind: "running", icon: "\u25CF", text: "Running \u2014 implementer, 12 min" } },
  { ...base, id: "yacoriey", phase: "Idle", lastSeq: 70, issue: 139, title: "chore: bump the chart's CRD copy", task: "Done", pr: 141, lastEventAt: ago2(60 * 5), status: { kind: "done", icon: "\u2713", text: "Done \u2014 merged" } },
  { ...base, id: "m4tq2zxa", phase: "Idle", lastSeq: 33, issue: 131, title: "docs: name the trace router's allowlist", task: "Rejected", lastEventAt: ago2(60 * 26), status: { kind: "failed", icon: "\u2717", text: "Failed \u2014 CI stayed red" } },
  { ...base, id: "p7wn5c3e", phase: "Idle", owner: "human:2", driver: "human:2", lastSeq: 12, lastEventAt: ago2(60 * 24 * 3), status: { kind: "waiting", icon: "\u23F3", text: "Waiting \u2014 for the next message or run" } }
];
var params = new URLSearchParams(location.search);
var cli = params.get("cli");
var maku = {
  oidc: { url: "https://rooms.priv.cloud.ogenki.io", issuer: "https://auth.cloud.ogenki.io", clientID: "293847561029384756@agents", projectID: "293847560192837465", version: "v0.9.0" },
  local: { url: "http://localhost:8080", humanMode: "local", version: "v0.9.0" },
  none: { url: "https://rooms.priv.cloud.ogenki.io", issuer: "https://auth.cloud.ogenki.io", clientID: "" }
};
var reads = 0;
var get = (async (url) => {
  if (url.startsWith("/api/roomctl")) {
    return cli && maku[cli] ? new Response(JSON.stringify(maku[cli])) : new Response(JSON.stringify({}), { status: 404 });
  }
  const run = rows[2];
  reads++;
  run.lastEventAt = ago2(0);
  run.status = { kind: "running", icon: "\u25CF", text: `Running \u2014 implementer, ${12 + reads} min` };
  const q = new URL(url, location.origin).searchParams;
  return new Response(JSON.stringify(rows.filter((r) => (!q.has("needs_me") || (r.needsYou ?? 0) > 0) && (!q.get("repo") || r.repository === q.get("repo")))));
});
var theme = initTheme();
var override = params.get("theme");
if (override === "light" || override === "dark") applyTheme(override);
listRooms(document.getElementById("app"), get, {}, theme).then(() => {
  const fold = document.querySelector("details.cli-setup");
  if (cli && fold) fold.open = true;
}).catch((e) => {
  document.body.textContent = `preview failed: ${e instanceof Error ? e.message : e}`;
});
