// SPDX-License-Identifier: Apache-2.0

// The title strip heads the room page: the task's title, its phase, run, budget,
// PR and issue, the room's class, who drives and who you are, why it waits, and
// what needs you. It renders from the state frame alone until a summary arrives,
// so a summary that cannot be fetched leaves a working strip. Every string here may
// be agent- or human-written: all of it enters the DOM as text.

import type { Snapshot } from "./conn";
import { el } from "./controls";
import { safeLink, type Summary, when } from "./summary";

export interface StripInput {
  roomId: string; snap?: Snapshot; phase: string; driver: string; sealedBy?: { who: string; at: string };
  summary?: Summary; needs: number; connection: string;
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
  if (st?.run) {
    const s = st.run.startedAt ? since(st.run.startedAt, now) : "";
    out.push(chip("chip-run", [st.run.role, st.run.id, s].filter(Boolean).join(" · ")));
  }
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
  header.append(root);
  let onNeeds = () => {};
  // An update that changes nothing keeps the DOM, so a focused PR link keeps its focus.
  let shown = "";
  return {
    update(i: StripInput): void {
      const t = now();
      const st = i.summary?.status;
      const title = st?.issue?.title ? `#${st.issue.number} ${st.issue.title}` : `Room ${i.roomId}`;
      const phase = st?.phase || i.phase;
      const key = JSON.stringify([i, since(st?.run?.startedAt ?? "", t)]);
      if (key === shown) return;
      const rows: HTMLElement[] = [el("h1", { className: "strip-title", textContent: title })];
      const row = el("div", { className: "strip-chips" });
      row.append(...chips(i, t));
      if (i.needs > 0) {
        const pill = el("button", { type: "button", className: "needs-pill", textContent: `Needs you · ${i.needs}` });
        pill.onclick = () => onNeeds();
        row.append(pill);
      }
      row.append(el("span", { className: "strip-conn", textContent: i.connection }));
      rows.push(row);
      if (i.sealedBy) {
        const who = i.sealedBy.who === i.snap?.you.principal ? "you" : shortPrincipal(i.sealedBy.who);
        rows.push(el("div", { className: "closed", textContent: `Closed ${when(i.sealedBy.at)} by ${who}` }));
      } else if (st?.reason || st?.next) {
        rows.push(el("div", { className: "waiting", textContent: `Waiting: ${[st.reason, st.next].filter(Boolean).join(" → ")}` }));
      }
      root.replaceChildren(...rows);
      document.title = phase ? `${title} · ${phase}` : title;
      shown = key;
    },
    onNeedsClick(f: () => void): void { onNeeds = f; },
  };
}
