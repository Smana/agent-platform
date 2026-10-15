// SPDX-License-Identifier: Apache-2.0

// The title strip heads the room page: the task's title, its phase, run, budget,
// PR and issue, the room's class, who drives and who you are, why it waits, and
// what needs you. It renders from the state frame alone until a summary arrives,
// so a summary that cannot be fetched leaves a working strip. Every string here may
// be agent- or human-written: all of it enters the DOM as text.

import type { Snapshot } from "./conn";
import { el } from "./controls";
import type { Latest } from "./room-state";
import { safeLink, type Summary, when } from "./summary";

export interface StripInput {
  roomId: string; snap?: Snapshot; phase: string; driver: string; sealedBy?: { who: string; at: string };
  summary?: Summary; needs: number; connection: string;
  now?: Latest; // the latest activity (RoomState.latest), for the Now line
}

// fmtTokens reads a token count at a glance: 189093 → "189 k", 1500000 → "1.5 M".
export function fmtTokens(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1e6) return `${Math.round(n / 1e3)} k`;
  return `${String(Math.round(n / 1e5) / 10)} M`;
}

// shortPrincipal keeps the kind and the first four characters of a long id.
export function shortPrincipal(p: string): string {
  const i = p.indexOf(":");
  const id = p.slice(i + 1);
  return id.length > 8 ? `${p.slice(0, i + 1)}${id.slice(0, 4)}…` : p;
}

function since(iso: string, now: number): string {
  const t = Date.parse(iso);
  if (isNaN(t)) return "";
  const m = Math.max(0, Math.floor((now - t) / 60000));
  return m < 60 ? `${m} min` : `${Math.floor(m / 60)} h ${m % 60} min`;
}

// ago reads a timestamp relative to now: "12 s ago", "4 min ago", "2 h ago".
export function ago(iso: string, now: number): string {
  const t = Date.parse(iso);
  if (isNaN(t)) return "";
  const s = Math.max(0, Math.floor((now - t) / 1000));
  if (s < 60) return `${s} s ago`;
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  return `${Math.floor(s / 3600)} h ago`;
}

// nowText is the Now line: what, its status, and how long ago.
function nowText(n: Latest, t: number): string {
  return [`Now ▸ ${n.text}`, n.status, ago(n.ts, t)].filter(Boolean).join(" · ");
}

// nowLine says what the agent is doing: the latest activity, a button that takes the
// reader to its row. With none, an owner or the driver is told where to start a run.
function nowLine(i: StripInput, t: number, onNow: (seq: number) => void): HTMLElement[] {
  const line = el("div", { className: "now" });
  if (!i.now) {
    line.textContent = "No run yet";
    const lead = i.snap && (i.snap.you.role === "owner" || i.snap.you.driver);
    return lead ? [line, el("span", { className: "now-hint", textContent: "Start one under Run" })] : [line];
  }
  const n = i.now;
  const b = el("button", { type: "button", className: "now-link", textContent: nowText(n, t) });
  b.dataset.status = n.status;
  b.onclick = () => onNow(n.seq);
  line.append(b);
  return [line];
}

function runText(r: NonNullable<Summary["status"]["run"]>, t: number): string {
  return [r.role, r.id, r.startedAt ? since(r.startedAt, t) : ""].filter(Boolean).join(" · ");
}

function chip(cls: string, ...body: (string | Node)[]): HTMLElement {
  const c = el("span", { className: `chip ${cls}` });
  c.append(...body);
  return c;
}

function chips(i: StripInput, now: number): HTMLElement[] {
  const st = i.summary?.status;
  const out: HTMLElement[] = [];
  const phase = chip("chip-phase", st?.phase || i.phase);
  if (st?.phase && i.phase) phase.title = `room: ${i.phase}`;
  if (phase.textContent) out.push(phase);
  if (st?.run) out.push(chip("chip-run", runText(st.run, now)));
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
  if (i.snap) out.push(chip("chip-class", i.snap.dataClass));
  if (i.driver) {
    const yours = i.snap?.you.principal === i.driver;
    const d = chip("chip-driver", `driver: ${yours ? "you" : shortPrincipal(i.driver)}`);
    if (!yours) d.title = i.driver;
    out.push(d);
  }
  if (i.snap) out.push(chip("chip-you", `you: ${i.snap.you.role}${i.snap.you.approver ? " (approver)" : ""}`));
  return out;
}

// mountStrip renders into a container it appends to header, so the header's other
// children (the theme button) stay. now is the clock the run's age is read from.
export function mountStrip(header: HTMLElement, now: () => number = Date.now) {
  const root = el("div", { className: "strip" });
  // Three parts, each rebuilt only when its own input changes: the title and chips, the
  // Now line, the waiting or closed line. The ages (the run's, the Now line's) move with
  // the clock and are rewritten in place, so a focused link or button keeps its focus.
  const head = el("div", { className: "strip-head" });
  const nowRow = el("div", { className: "strip-now" });
  const tail = el("div", { className: "strip-tail" });
  root.append(head, nowRow, tail);
  header.append(root);
  let onNeeds = () => {};
  let onNow = (_seq: number) => {};
  const shown = { head: "", now: "", tail: "" };
  return {
    update(i: StripInput): void {
      const t = now();
      const st = i.summary?.status;
      const title = st?.issue?.title ? `#${st.issue.number} ${st.issue.title}` : `Room ${i.roomId}`;
      const phase = st?.phase || i.phase;
      const { now: latest, sealedBy, ...rest } = i;
      const headKey = JSON.stringify(rest);
      if (headKey !== shown.head) {
        const row = el("div", { className: "strip-chips" });
        row.append(...chips(i, t));
        if (i.needs > 0) {
          const pill = el("button", { type: "button", className: "needs-pill", textContent: `Needs you · ${i.needs}` });
          pill.onclick = () => onNeeds();
          row.append(pill);
        }
        row.append(el("span", { className: "strip-conn", textContent: i.connection }));
        head.replaceChildren(el("h1", { className: "strip-title", textContent: title }), row);
        document.title = phase ? `${title} · ${phase}` : title;
        shown.head = headKey;
      } else if (st?.run) {
        const run = head.querySelector(".chip-run");
        const text = runText(st.run, t);
        if (run && run.textContent !== text) run.textContent = text;
      }
      const nowKey = JSON.stringify([latest, i.snap?.you.role, i.snap?.you.driver]);
      if (nowKey !== shown.now) {
        nowRow.replaceChildren(...nowLine(i, t, (seq) => onNow(seq)));
        shown.now = nowKey;
      } else if (latest) {
        const b = nowRow.querySelector(".now-link");
        const text = nowText(latest, t);
        if (b && b.textContent !== text) b.textContent = text;
      }
      const tailKey = JSON.stringify([sealedBy, i.snap?.you.principal, st?.reason, st?.next]);
      if (tailKey !== shown.tail) {
        const lines: HTMLElement[] = [];
        if (sealedBy) {
          const who = sealedBy.who === i.snap?.you.principal ? "you" : shortPrincipal(sealedBy.who);
          lines.push(el("div", { className: "closed", textContent: `Closed ${when(sealedBy.at)} by ${who}` }));
        } else if (st?.reason || st?.next) {
          lines.push(el("div", { className: "waiting", textContent: `Waiting: ${[st.reason, st.next].filter(Boolean).join(" → ")}` }));
        }
        tail.replaceChildren(...lines);
        shown.tail = tailKey;
      }
    },
    onNeedsClick(f: () => void): void { onNeeds = f; },
    onNowClick(f: (seq: number) => void): void { onNow = f; },
  };
}
