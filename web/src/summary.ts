// SPDX-License-Identifier: Apache-2.0

// The room page's lead blocks, rendered from the broker's summary/v1 JSON
// (GET /api/rooms/{id}/summary): status, needs you, what you can do, the agents'
// notes. Every string here may be agent-written, so all of it goes in as text.

import { api } from "./api";
import { approvalAnchor, el } from "./controls";

export interface Summary {
  apiVersion: "summary/v1";
  room: string;
  url: string;
  status: {
    phase: string; // a task phase, or Closed/Sealed
    run: { id: string; role: string; trigger: string; startedAt: string } | null;
    budget: { usedTokens: number; limitTokens: number } | null;
    pr: { number: number; url: string; author: string; reviewers: string[] } | null;
    issue: { number: number; url: string; author: string; labelledBy: string } | null;
    lastVerdict: { by: string; verdict: string; at: string } | null;
  };
  needsYou: { kind: "approval"; id: string; what: string; deadline: string; url: string }[];
  actions: { kind: string; what: string; cli?: string }[];
  notes: { untrusted: true; items: { at: string; run: string; text: string }[] };
  cursor: string;
}

const failures: Record<number, string> = {
  404: "No such room, or you cannot read it.",
  503: "The summary is unavailable: your access cannot be verified, or the room log is unreadable. Retry.",
};

export async function fetchSummary(id: string, get: typeof fetch = fetch): Promise<Summary> {
  const r = await api(`/api/rooms/${encodeURIComponent(id)}/summary`, {}, get);
  if (!r.ok) throw new Error(failures[r.status] ?? `The summary is unavailable: ${r.status}`);
  const s = (await r.json()) as Summary;
  if (s?.apiVersion !== "summary/v1") throw new Error("The summary is in a format this page does not know. Reload the page.");
  return s;
}

// Agent-written URLs become a link only when https; anything else stays text.
function safeLink(text: string, url: string): Node {
  try {
    if (new URL(url).protocol === "https:") return el("a", { href: url, textContent: text, rel: "noopener noreferrer", target: "_blank" });
  } catch { /* not a URL: text */ }
  return document.createTextNode(`${text} (${url})`);
}

function when(iso: string): string {
  const d = new Date(iso);
  return isNaN(d.getTime()) ? iso : d.toLocaleString();
}

function block(name: string, title: string, ...body: Node[]): HTMLElement {
  const s = el("section", { className: `summary-block summary-${name}` });
  s.dataset.block = name;
  s.append(el("h2", { textContent: title }), ...body);
  return s;
}

function line(...parts: (string | Node)[]): HTMLElement {
  const p = el("p");
  p.append(...parts);
  return p;
}

function statusBlock(st: Summary["status"]): HTMLElement {
  const rows: HTMLElement[] = [line(`Phase: ${st.phase}`)];
  rows.push(line(st.run ? `Run: ${st.run.id} · ${st.run.role} · ${st.run.trigger} · since ${when(st.run.startedAt)}` : "Run: none"));
  if (st.budget) rows.push(line(`Budget: ${st.budget.usedTokens} / ${st.budget.limitTokens} tokens`));
  if (st.pr) rows.push(line("PR: ", safeLink(`#${st.pr.number}`, st.pr.url), ` by ${st.pr.author}`,
    st.pr.reviewers.length ? ` · reviewers ${st.pr.reviewers.join(", ")}` : ""));
  if (st.issue) rows.push(line("Issue: ", safeLink(`#${st.issue.number}`, st.issue.url), ` by ${st.issue.author}`));
  if (st.lastVerdict) rows.push(line(`Last verdict: ${st.lastVerdict.verdict} by ${st.lastVerdict.by} · ${when(st.lastVerdict.at)}`));
  return block("status", "Status", ...rows);
}

// focusApproval reveals the existing approval card for id and focuses its Approve
// button. False when there is no card (a non-approver has none, or it is decided).
export function focusApproval(id: string): boolean {
  const card = document.getElementById(approvalAnchor(id));
  if (!card) return false;
  card.scrollIntoView?.({ block: "center" });
  (card.querySelector<HTMLElement>('button[data-act="approve"]') ?? card.querySelector<HTMLElement>("button, input"))?.focus();
  return true;
}

function needsYouBlock(items: Summary["needsYou"]): HTMLElement {
  const ul = el("ul");
  for (const n of items) {
    const a = el("a", { href: `#${approvalAnchor(n.id)}`, textContent: "Review and decide" });
    // Reveal the existing approval card; the page keeps one decide path.
    a.onclick = (e) => { if (focusApproval(n.id)) e.preventDefault(); };
    const li = el("li");
    li.append(`Approval: ${n.what} · decide by ${when(n.deadline)} · `, a);
    ul.append(li);
  }
  return block("needs-you", "Needs you", ul);
}

function actionsBlock(actions: Summary["actions"]): HTMLElement {
  if (!actions.length) return block("actions", "What you can do", line("Nothing here beyond reading."));
  const ul = el("ul");
  for (const a of actions) {
    const li = el("li");
    li.append(a.what);
    if (a.cli) li.append(" ", el("code", { textContent: a.cli }));
    ul.append(li);
  }
  return block("actions", "What you can do", ul);
}

function notesBlock(n: Summary["notes"]): HTMLElement {
  const ul = el("ul");
  for (const i of n.items) ul.append(el("li", { textContent: `${when(i.at)} · ${i.run}: ${i.text}` }));
  return block("notes", "Notes", el("p", { className: "hint", textContent: "The agents' claims, not verified." }),
    n.items.length ? ul : line("No notes yet."));
}

export function renderSummary(root: HTMLElement, s: Summary): void {
  const blocks = [statusBlock(s.status)];
  if (s.needsYou.length) blocks.push(needsYouBlock(s.needsYou));
  blocks.push(actionsBlock(s.actions), notesBlock(s.notes));
  root.replaceChildren(...blocks);
}
