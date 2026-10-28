// SPDX-License-Identifier: Apache-2.0

// The title strip heads the room page: the task's title, its phase, run, budget,
// PR and issue, the room's class, who drives and who you are, why it waits, and
// what needs you. It renders from the state frame alone until a summary arrives,
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
  summary?: Summary; needs: number; connection: string;
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

// nowText is the Now line: what, its status, and how long ago; with no agent present, the
// last activity, which no longer runs.
function nowText(n: Latest, t: number, present: boolean): string {
  return present ? [`Now ▸ ${n.text}`, n.status, ago(n.ts, t)].filter(Boolean).join(" · ")
    : [`No run · last: ${n.text}`, ago(n.ts, t)].filter(Boolean).join(" · ");
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
  const lead = !present && i.snap && (i.snap.you.role === "owner" || i.snap.you.driver);
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

function chips(i: StripInput, now: number): HTMLElement[] {
  const st = i.summary?.status;
  const out: HTMLElement[] = [];
  const phase = chip("chip-phase", st?.phase || i.phase);
  if (st?.phase && i.phase) phase.title = `room: ${i.phase}`;
  if (phase.textContent) out.push(phase);
  // The facts' run outlives it: with no agent present, its age would keep counting.
  if (st?.run && i.running !== false) out.push(chip("chip-run", runText(st, now)));
  if (st?.budget) {
    const { usedTokens: used, limitTokens: limit } = st.budget;
    const pct = limit > 0 ? Math.round((used / limit) * 100) : 0;
    const bar = el("span", { className: "meter-bar" });
    bar.style.setProperty("--fill", `${Math.min(pct, 100)}%`); // CSSOM: the CSP refuses a style attribute
    bar.setAttribute("aria-hidden", "true");
    out.push(chip("chip-budget", `${fmtTokens(used)} / ${fmtTokens(limit)}${limit > 0 ? ` · ${pct} %` : ""} `, bar));
  }
  if (st?.pr) out.push(chip("chip-pr", safeLink(`PR #${st.pr.number}`, st.pr.url)));
  if (st?.issue) out.push(chip("chip-issue", safeLink(`issue #${st.issue.number}`, st.issue.url)));
  if (st?.lastVerdict) out.push(chip("chip-verdict", `last verdict: ${st.lastVerdict.by} ${st.lastVerdict.verdict}`));
  if (i.snap) out.push(chip("chip-class", i.snap.dataClass));
  if (i.driver) {
    const yours = i.snap?.you.principal === i.driver;
    const d = chip("chip-driver", `driver: ${yours ? "you" : shortPrincipal(i.driver)}`);
    if (!yours) d.title = i.driver;
    out.push(d);
  }
  if (i.snap) out.push(chip("chip-you", `you: ${i.snap.you.role}${i.snap.you.approver ? " (approver)" : ""}`));
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
  const conn = el("span", { className: "strip-conn" });
  row.append(conn);
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
      if (title.textContent !== text) {
        title.textContent = text;
        title.title = text; // a phone clamps the title to two lines: the whole of it on hover
      }
      document.title = phase ? `${text} · ${phase} · Maku` : `${text} · Maku`;
      const { now: latest, sealedBy, needs, connection, approvals: pend, ...rest } = i;
      const headKey = JSON.stringify(rest);
      if (headKey !== shown.head) {
        for (const c of [...row.querySelectorAll(":scope > .chip")]) c.remove();
        row.prepend(...chips(i, t));
        shown.head = headKey;
      } else if (st?.run) {
        const run = head.querySelector(".chip-run");
        const r = runText(st, t);
        if (run && run.textContent !== r) run.textContent = r;
      }
      if (needs > 0) {
        const p = `Needs you · ${needs}`;
        if (pill.textContent !== p) pill.textContent = p;
        if (!pill.isConnected || pill.nextElementSibling !== conn) conn.before(pill);
      } else pill.remove();
      if (conn.textContent !== connection) conn.textContent = connection;
      const status = statusOf(i, t);
      const step = i.summary?.step;
      // A line that waits for an approval takes you to its card; one that waits for a PR links it.
      const toCard = status.kind === "waiting-you" || status.kind === "waiting-other";
      const prUrl = status.kind === "waiting-pr" && isGitHubUrl(step?.url) ? step!.url! : "";
      const statusKey = JSON.stringify([status, prUrl]);
      if (statusKey !== shown.status) {
        statusRow.hidden = !status.kind;
        statusRow.dataset.kind = status.kind;
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
      const nowKey = JSON.stringify([latest, i.running, i.ran || !!st?.run, i.snap?.you.role, i.snap?.you.driver]);
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
          lines.push(el("div", { className: "waiting", textContent: `Waiting: ${[st.reason, st.next].filter(Boolean).join(" → ")}` }));
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
        tail.replaceChildren(...lines);
        shown.tail = tailKey;
      }
    },
    onNeedsClick(f: () => void): void { onNeeds = f; },
    onApprovalsClick(f: () => void): void { onApprovals = f; },
    onNowClick(f: (seq: number) => void): void { onNow = f; },
  };
}
