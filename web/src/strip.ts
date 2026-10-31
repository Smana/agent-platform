// SPDX-License-Identifier: Apache-2.0

// The title strip heads the room page: the status line, the task's title (its issue's
// link), the facts (run, budget, PR, data class, who drives, a watcher's View only), what
// comes next, and what needs you. A fact that is the norm says nothing: the role shows only
// when it stops you, and the phase only when no status line says more. It renders from the state frame alone until a summary arrives,
// so a summary that cannot be fetched leaves a working strip. Every string here may
// be agent- or human-written: all of it enters the DOM as text.

import type { Snapshot } from "./conn";
import { bannerText } from "./approval-help";
import { el, shortPrincipal } from "./controls";
import { type Dashboard, expandDashboards } from "./dashboards";
import type { Latest } from "./room-state";
import { type RunStatus, statusLine } from "./run-status";
import { ago, safeLink, type Summary, when } from "./summary";

export { shortPrincipal };

// A step's link is the broker's own, but it still goes in only when it is https on github.com.
function isGitHubUrl(url?: string): boolean {
  try { const u = url ? new URL(url) : null; return !!u && u.protocol === "https:" && u.hostname === "github.com"; } catch { return false; }
}

export interface StripInput {
  roomId: string; snap?: Snapshot; phase: string; driver: string; sealedBy?: { who: string; at: string };
  sealed?: boolean; // the room is sealed, closer known (sealedBy) or not
  summary?: Summary; needs: number;
  // Pending approvals: all of them, and those the viewer may decide (needs.ts approvalCounts).
  approvals?: { total: number; mine: number };
  now?: Latest; // the latest activity (RoomState.latest), for the Now line
  // Whether an agent is present now, and whether one ever was (RoomState.running, ran).
  // Undefined when nothing tracks it: the Now line then reads the latest activity as current.
  running?: boolean; ran?: boolean;
  dashboards?: Dashboard[]; // the operator's link templates (GET /api/roomctl); none, no group
}

// fmtTokens reads a token count at a glance: 189093 → "189 k", 1500000 → "1.5 M". A count
// that would round to "1000 k" reads "1 M", as roomctl's tokens() does.
export function fmtTokens(n: number): string {
  if (n < 1000) return String(n);
  if (n < 999_500) return `${Math.round(n / 1e3)} k`;
  return `${String(Math.round(n / 1e5) / 10)} M`;
}

function since(iso: string, now: number): string {
  const t = Date.parse(iso);
  if (isNaN(t)) return "";
  const m = Math.max(0, Math.floor((now - t) / 60000));
  return m < 60 ? `${m} min` : `${Math.floor(m / 60)} h ${m % 60} min`;
}

// plainText reduces an agent's markdown to one line of text for a place that cannot render it
// (the Now line): line prefixes, links, code and clear emphasis go; whitespace collapses. An
// underscore or star counts as emphasis only at a word edge, so snake_case and "2 * 3" stay.
export function plainText(src: string): string {
  return src
    .replace(/^\s{0,3}(?:#{1,6}\s+|>\s?|[-*+]\s+|\d+[.)]\s+)/gm, "")
    .replace(/!?\[([^\]]*)\]\([^)\s]*\)/g, "$1")
    .replace(/`+([^`]+)`+/g, "$1")
    .replace(/\*\*(?!\s)(.+?)(?<!\s)\*\*/g, "$1")
    .replace(/(?<!\w)__(?!\s)(.+?)(?<!\s)__(?!\w)/g, "$1")
    .replace(/(?<![\w*])\*(?![\s*])([^*]+?)(?<!\s)\*(?![\w*])/g, "$1")
    .replace(/(?<!\w)_(?![\s_])([^_]+?)(?<!\s)_(?!\w)/g, "$1")
    .replace(/\s+/g, " ")
    .trim();
}

// nowText is the Now line: what, its status, and how long ago; with no agent present, the
// last activity, which no longer runs.
function nowText(n: Latest, t: number, present: boolean): string {
  const text = plainText(n.text);
  return present ? [`Now ▸ ${text}`, n.status, ago(n.ts, t)].filter(Boolean).join(" · ")
    : [`No run · last: ${text}`, ago(n.ts, t)].filter(Boolean).join(" · ");
}

// nowLine says what the agent is doing: the latest activity, a button that takes the
// reader to its row. With no agent present it says so: "No run yet" until one ever ran (a
// human's message does not start one), then "No run" and the last activity; an owner or
// the driver is told where to start a run.
function nowLine(i: StripInput, t: number, onNow: (seq: number) => void): HTMLElement[] {
  const line = el("div", { className: "now" });
  const present = i.running ?? !!i.now;
  const ran = i.ran || !!i.summary?.status.run;
  const n = present || ran ? i.now : undefined;
  if (n) {
    const b = el("button", { type: "button", className: "now-link", textContent: nowText(n, t, present) });
    b.dataset.status = present ? n.status : "ended";
    b.onclick = () => onNow(n.seq);
    line.append(b);
  } else line.textContent = present ? "Now ▸ the run has started" : ran ? "No run" : "No run yet";
  // A room that already says what comes next (a /factory retry, a PR to merge) gets no
  // second, contrary way forward.
  const told = !!(i.summary?.status.next || i.summary?.step);
  const lead = !present && !told && i.snap && (i.snap.you.role === "owner" || i.snap.you.driver);
  return lead ? [line, el("span", { className: "now-hint", textContent: "Start one under Session › Run" })] : [line];
}

// runText is the run chip: role · id · model · effort · age. The model and effort are the run's own
// (the present run's or the last's, matched by id), each left out when the log does not say it.
function runText(st: Summary["status"], t: number): string {
  const r = st.run!;
  const m = [st.runModel, st.lastRunModel].find((x) => x?.run === r.id);
  return [r.role, r.id, m?.model, m?.effort, r.startedAt ? since(r.startedAt, t) : ""].filter(Boolean).join(" · ");
}

// statusOf is the room's run status: the summary's own state, phase and facts, and the viewer's approvals.
function statusOf(i: StripInput, t: number): RunStatus {
  const st = i.summary?.status;
  const run = st?.run;
  return statusLine({
    state: st?.state || (i.sealed || i.sealedBy ? "stopped" : undefined), phase: st?.phase || i.phase, reasonCode: st?.reasonCode,
    runRole: run?.role, runStartedAt: run?.startedAt, runPresent: !!run && (i.running ?? !run.endedAt),
    pr: st?.pr?.number, issue: st?.issue?.number,
    mine: i.approvals?.mine ?? 0, others: Math.max(0, (i.approvals?.total ?? 0) - (i.approvals?.mine ?? 0)),
    step: i.summary?.step, now: t,
  });
}

// The data class, in words: which model backends the room's runs may reach (docs/concepts.md).
const DATA_CLASS: Record<string, string> = {
  public: "Public: the runs may use external model providers",
  internal: "Internal: the runs use self-hosted models only, with no egress",
};

// withCode sets a line of text with its backticked spans as code, each with a copy button.
// Every part goes in as text: the line may be agent- or factory-written.
export function withCode(text: string): Node[] {
  return text.split(/`([^`]+)`/).flatMap<Node>((part, n) => {
    if (n % 2 === 0) return part ? [document.createTextNode(part)] : [];
    const copy = el("button", { type: "button", className: "copy-code", textContent: "Copy" });
    copy.setAttribute("aria-label", `Copy ${part}`);
    copy.onclick = () => void navigator.clipboard?.writeText(part).then(() => { copy.textContent = "Copied"; }, () => {});
    return [el("code", { textContent: part }), copy];
  });
}

const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

// phaseTitle is the status line's tooltip: the task's raw phase, and the room's when it differs.
function phaseTitle(i: StripInput): string {
  const task = i.summary?.status.phase;
  if (!task) return i.phase ? `phase: ${i.phase}` : "";
  return i.phase && i.phase !== task ? `phase: ${task} · room: ${i.phase}` : `phase: ${task}`;
}

function chip(cls: string, ...body: (string | Node)[]): HTMLElement {
  const c = el("span", { className: `chip ${cls}` });
  c.append(...body);
  return c;
}

// dashboardsChip is the "Dashboards:" group, filled with the room's ids: the run is the
// facts' current or last one, else the history's last, else the state frame's last. A chip
// in the chip row, so a phone's strip keeps its one sideways line. Undefined when no link shows.
function dashboardsChip(i: StripInput): HTMLElement | undefined {
  const s = i.summary;
  const run = s?.status.run?.id || s?.runs?.at(-1)?.id || i.snap?.runs?.at(-1)?.id;
  const links = expandDashboards(i.dashboards, { room: i.roomId, run, task: s?.status.task });
  if (links.length === 0) return undefined;
  const body: (string | Node)[] = ["Dashboards: "];
  links.forEach((l, n) => body.push(...(n ? [" · "] : []), safeLink(l.label, l.url)));
  return chip("chip-dashboards", ...body);
}

// chips are the facts row. status is whether a status line heads the strip: then it says
// where the task stands, and the phase is its tooltip, not a chip.
function chips(i: StripInput, now: number, status: boolean): HTMLElement[] {
  const st = i.summary?.status;
  const out: HTMLElement[] = [];
  const phase = st?.phase || i.phase;
  if (phase && !status) out.push(chip("chip-phase", phase));
  // The facts' run outlives it: with no agent present, its age would keep counting.
  if (st?.run && i.running !== false) out.push(chip("chip-run", runText(st, now)));
  if (st?.budget) {
    const { usedTokens: used, limitTokens: limit } = st.budget;
    const pct = limit > 0 ? Math.round((used / limit) * 100) : 0;
    const bar = el("span", { className: "meter-bar" });
    bar.style.setProperty("--fill", `${Math.min(pct, 100)}%`); // CSSOM: the CSP refuses a style attribute
    bar.setAttribute("aria-hidden", "true");
    const b = chip("chip-budget", `Tokens ${fmtTokens(used)}${limit > 0 ? ` of ${fmtTokens(limit)}` : ""} `, bar);
    if (limit > 0) {
      b.title = `${pct} % of this task's token budget: the task stops when it is spent`;
      if (used >= limit) b.dataset.level = "over"; else if (used >= limit * 0.8) b.dataset.level = "warn";
    }
    out.push(b);
  }
  if (st?.pr) out.push(chip("chip-pr", safeLink(`PR #${st.pr.number}`, st.pr.url)));
  // The title links the issue when it names it; an issue with no title keeps its own link here.
  if (st?.issue && !st.issue.title) out.push(chip("chip-issue", safeLink(`Issue #${st.issue.number}`, st.issue.url)));
  if (st?.lastVerdict) out.push(chip("chip-verdict", `last verdict: ${st.lastVerdict.by} ${st.lastVerdict.verdict}`));
  if (i.snap) {
    const c = chip("chip-class", `Data: ${i.snap.dataClass}`);
    if (Object.hasOwn(DATA_CLASS, i.snap.dataClass)) c.title = DATA_CLASS[i.snap.dataClass];
    out.push(c);
  }
  if (i.driver) {
    const yours = i.snap?.you.principal === i.driver;
    const who = yours ? "you" : i.driver === "system:factory" ? "the factory" : shortPrincipal(i.driver);
    const d = chip("chip-driver", `Driven by ${who}`);
    d.title = `${yours ? "You" : i.driver}: the one who may steer or interrupt the running run`;
    out.push(d);
  }
  // A watcher who cannot approve can only read: the one role worth saying.
  const you = i.snap?.you;
  if (you && (you.role === "watcher" || !you.role) && !you.approver) {
    const c = chip("chip-you", "View only");
    c.title = `Your role here: ${you.role || "none"}`;
    out.push(c);
  }
  const dash = dashboardsChip(i);
  if (dash) out.push(dash);
  return out;
}

// sealedText is the closed line: who closed the room and when, else what sealed it.
function sealedText(i: StripInput): string {
  if (i.sealedBy) {
    const who = i.sealedBy.who === i.snap?.you.principal ? "you" : shortPrincipal(i.sealedBy.who);
    return `Closed ${when(i.sealedBy.at)} by ${who}`;
  }
  const phase = i.summary?.status.phase;
  return phase === "Closed" || phase === "Sealed" ? phase : i.phase === "Closed" ? "Closed" : "Sealed";
}

// mountStrip renders into a container it appends to header, so the header's other
// children (the theme button) stay. now is the clock the run's age is read from.
export function mountStrip(header: HTMLElement, now: () => number = Date.now) {
  const root = el("div", { className: "strip" });
  // Three parts, in the spec's order: the title and chips, the waiting or closed line, the
  // Now line. Each is rebuilt only when its own input changes. The ages (the run's, the Now
  // line's), the needs-you count and the connection move in place, so a focused link,
  // button or pill keeps its focus.
  const head = el("div", { className: "strip-head" });
  const nowRow = el("div", { className: "strip-now" });
  const tail = el("div", { className: "strip-tail" });
  // The status leads: icon, colour and words, the first thing the page says about the room.
  const statusRow = el("div", { className: "run-status" });
  statusRow.setAttribute("role", "status");
  statusRow.setAttribute("aria-live", "polite");
  statusRow.hidden = true;
  root.append(statusRow, head, tail, nowRow);
  header.append(root);
  const title = el("h1", { className: "strip-title" });
  const row = el("div", { className: "strip-chips" });
  const pill = el("button", { type: "button", className: "needs-pill" });
  head.append(title, row);
  let onNeeds = () => {};
  let onNow = (_seq: number) => {};
  let onApprovals = () => {};
  pill.onclick = () => onNeeds();
  const shown = { head: "", now: "", tail: "", status: "" };
  return {
    update(i: StripInput): void {
      const t = now();
      const st = i.summary?.status;
      const text = st?.issue?.title ? `#${st.issue.number} ${st.issue.title}` : `Room ${i.roomId}`;
      const phase = st?.phase || i.phase;
      // The title links its issue, when that is https: the issue chip it replaces said no more.
      const href = st?.issue?.title && /^https:\/\//.test(st.issue.url) ? st.issue.url : "";
      if (title.textContent !== text || (title.querySelector("a")?.getAttribute("href") ?? "") !== href) {
        title.replaceChildren(href ? safeLink(text, href) : document.createTextNode(text));
        title.title = text; // a phone clamps the title to two lines: the whole of it on hover
      }
      document.title = phase ? `${text} · ${phase} · Maku` : `${text} · Maku`;
      const { now: latest, sealedBy, needs, approvals: pend, ...rest } = i;
      const status = statusOf(i, t);
      const headKey = JSON.stringify([rest, !!status.kind]);
      if (headKey !== shown.head) {
        for (const c of [...row.querySelectorAll(":scope > .chip")]) c.remove();
        row.prepend(...chips(i, t, !!status.kind));
        shown.head = headKey;
      } else if (st?.run) {
        const run = head.querySelector(".chip-run");
        const r = runText(st, t);
        if (run && run.textContent !== r) run.textContent = r;
      }
      if (needs > 0) {
        const p = `Needs you · ${needs}`;
        if (pill.textContent !== p) pill.textContent = p;
        if (!pill.isConnected || pill.nextElementSibling) row.append(pill);
      } else pill.remove();
      const step = i.summary?.step;
      // A line that waits for an approval takes you to its card; one that waits for a PR links it.
      const toCard = status.kind === "waiting-you" || status.kind === "waiting-other";
      const prUrl = status.kind === "waiting-pr" && isGitHubUrl(step?.url) ? step!.url! : "";
      const statusKey = JSON.stringify([status, prUrl, phaseTitle(i)]);
      if (statusKey !== shown.status) {
        statusRow.hidden = !status.kind;
        statusRow.dataset.kind = status.kind;
        statusRow.title = phaseTitle(i);
        const icon = el("span", { className: "run-status-icon", textContent: status.icon });
        icon.setAttribute("aria-hidden", "true"); // the words say it all
        let words: HTMLElement;
        if (toCard) {
          words = el("a", { className: "run-status-words", href: "#needs-title", textContent: status.words });
          words.onclick = (e) => { e.preventDefault(); onApprovals(); };
        } else if (prUrl) {
          words = el("a", { className: "run-status-words", href: prUrl, textContent: status.words, rel: "noopener noreferrer", target: "_blank" });
        } else words = el("span", { className: "run-status-words", textContent: status.words });
        statusRow.replaceChildren(icon, " ", words); // the space keeps the copied text readable; the flex row ignores it
        shown.status = statusKey;
      }
      const present = i.running ?? !!latest;
      const nowKey = JSON.stringify([latest, i.running, i.ran || !!st?.run, i.snap?.you.role, i.snap?.you.driver, !!(st?.next || i.summary?.step)]);
      if (nowKey !== shown.now) {
        nowRow.replaceChildren(...nowLine(i, t, (seq) => onNow(seq)));
        shown.now = nowKey;
      } else if (latest) {
        const b = nowRow.querySelector(".now-link");
        const n = nowText(latest, t, present);
        if (b && b.textContent !== n) b.textContent = n;
      }
      const tailKey = JSON.stringify([sealedBy, i.sealed, i.snap?.you.principal, st?.phase, i.phase, st?.reason, st?.next, i.summary?.step, pend?.total, pend?.mine]);
      if (tailKey !== shown.tail) {
        const lines: HTMLElement[] = [];
        if (sealedBy || i.sealed) lines.push(el("div", { className: "closed", textContent: sealedText(i) }));
        else if (st?.reason || st?.next) {
          const line = el("div", { className: "waiting" });
          line.append(...withCode(cap([st.reason, st.next].filter(Boolean).join(" → "))));
          lines.push(line);
        }
        // Nothing at the top said an approval waits: this line does, and takes you to its card.
        const banner = sealedBy || i.sealed ? "" : bannerText(pend?.total ?? 0, pend?.mine ?? 0);
        if (banner) {
          const a = el("a", { className: "approvals-banner", href: "#needs-title", textContent: `${banner} — review` });
          a.onclick = (e) => { e.preventDefault(); onApprovals(); };
          lines.push(el("div", { className: "waiting approvals-line" }));
          lines[lines.length - 1].append(a);
        }
        if (step && !sealedBy && !i.sealed) {
          // A PR-held room has nothing to decide here: say where to act, and link it.
          const line = el("div", { className: "waiting", textContent: "Your move: " });
          line.append(step.kind !== "approve" && isGitHubUrl(step.url)
            ? safeLink(step.label, step.url!)
            : document.createTextNode(step.kind === "approve" ? `${step.label}, in Needs you below` : step.label));
          lines.push(line);
        }
        // What comes next, headed as such; a closed room has nothing next.
        const next = lines.length > 0 && !(sealedBy || i.sealed);
        if (next) lines.unshift(el("span", { className: "next-label", textContent: "Next" }));
        tail.classList.toggle("strip-next", next);
        tail.replaceChildren(...lines);
        shown.tail = tailKey;
      }
    },
    onNeedsClick(f: () => void): void { onNeeds = f; },
    onApprovalsClick(f: () => void): void { onApprovals = f; },
    onNowClick(f: (seq: number) => void): void { onNow = f; },
  };
}
