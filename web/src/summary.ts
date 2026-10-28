// SPDX-License-Identifier: Apache-2.0

// The room page's summary parts, rendered from the broker's summary/v1 JSON
// (GET /api/rooms/{id}/summary): the merges and holds the needs-you panel lists
// (needs.ts) and the agents' notes; the status heads the page in the title strip (strip.ts). Every string here may be agent-written, so all of
// it goes in as text. A field the broker omits when empty (omitempty, omitzero) is
// optional here, and its part is skipped.

import { api } from "./api";
import type { RoomEvent } from "./conn";
import { approvalAnchor, el } from "./controls";

export interface RunModel { run: string; model?: string; effort?: string }

export interface Summary {
  apiVersion: "summary/v1";
  room: string;
  url: string;
  status: {
    phase: string; // a task phase, or Closed/Sealed
    state?: string; // the coarse state (queued, running, waiting_for_human, done, failed, stopped)
    task?: string; // the room's Task, named as the room; absent without one
    run: { id: string; role: string; trigger?: string; startedAt?: string; endedAt?: string } | null;
    budget: { usedTokens: number; limitTokens: number } | null;
    pr: { number: number; url: string; author?: string; reviewers?: string[] } | null;
    issue: { number: number; url: string; title?: string; author?: string; labelledBy?: string } | null;
    lastVerdict: { by: string; verdict: string; at: string } | null;
    reason?: string; // why the task waits (#59)
    reasonCode?: string; // the factory's raw reason code, the status line's key
    next?: string; // what happens or who acts next
    // What a run runs on: model is its claim's spec.model, effort what its harness reported. The
    // present run's, and the last run's (the one before the present run, or the last when none is
    // present). Absent until the log says.
    runModel?: RunModel;
    lastRunModel?: RunModel;
  };
  // A hold (N3): a factory task waits on the viewer's hold on this room; id is its wait reason.
  runs?: { id: string; role: string }[]; // the task's run history, oldest first
  needsYou: { kind: "approval" | "hold" | "merge"; id: string; what: string; deadline?: string; url: string }[];
  // The one thing to do for the room and where: "pr" and "answer" are on GitHub, "approve" on this page.
  step?: { kind: "approve" | "pr" | "answer"; label: string; url?: string };
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

// changesSummary is whether the broker's fold (internal/summary) reads e: only then is the
// summary refetched, never on a run's chat or tool calls.
export function changesSummary(e: RoomEvent): boolean {
  const kind = e.payload?.kind;
  switch (e.type) {
    case "state_changed": return kind === "task" || kind === "room_phase" || kind === "limit";
    case "message": return kind === "progress" || kind === "review_verdict";
    case "approval_requested": case "approval_decided": return true;
  }
  return false;
}

// Agent-written URLs become a link only when https; anything else stays text.
export function safeLink(text: string, url: string): Node {
  try {
    if (new URL(url).protocol === "https:") return el("a", { href: url, textContent: text, rel: "noopener noreferrer", target: "_blank" });
  } catch { /* not a URL: text */ }
  return document.createTextNode(`${text} (${url})`);
}

export function when(iso: string): string {
  const d = new Date(iso);
  return isNaN(d.getTime()) ? iso : d.toLocaleString();
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

// focusApproval reveals the approval card for id in the needs-you panel and focuses its
// Approve button, or its first control. False when there is no card (it is decided, or
// its request has not arrived yet).
export function focusApproval(id: string): boolean {
  const card = document.getElementById(approvalAnchor(id));
  if (!card) return false;
  card.scrollIntoView?.({ block: "center" });
  (card.querySelector<HTMLElement>('button[data-act="approve"]') ?? card.querySelector<HTMLElement>("button, input"))?.focus();
  return true;
}

// needsItems is the summary's merges and holds, one list item each; the approvals come
// from the live log instead (needs.ts), so the summary's approval entries are skipped.
export function needsItems(s: Summary): HTMLLIElement[] {
  const out: HTMLLIElement[] = [];
  for (const n of s.needsYou) {
    if (n.kind === "hold") out.push(el("li", { textContent: `Hold: ${n.what}` })); // the page is the room: nothing to link to
    if (n.kind === "merge") { // the PR is on GitHub: link out, https only
      const li = el("li");
      li.append(`Merge: ${n.what} · `, safeLink("open the PR", n.url));
      out.push(li);
    }
  }
  return out;
}

// renderNotes renders the agents' notes (oldest first in the summary): the latest on one
// line, labelled as the agent's claim, and the earlier ones, newest first, behind a
// "Notes (N)" toggle. No notes, no block.
export function renderNotes(root: HTMLElement, s: Summary, now = Date.now()): void {
  const items = s.notes.items;
  if (!items.length) {
    root.replaceChildren();
    return;
  }
  const last = items[items.length - 1];
  const box = el("div", { className: "notes" });
  box.dataset.block = "notes";
  const latest = el("p", { className: "note-latest" });
  latest.append(el("span", { textContent: [last.run, ago(last.at, now), last.text].filter(Boolean).join(" · ") }),
    el("span", { className: "hint", textContent: "the agent's claim" }));
  box.append(latest);
  const rest = items.slice(0, -1).reverse();
  if (rest.length) {
    const ul = el("ul");
    for (const i of rest) ul.append(el("li", { textContent: `${when(i.at)} · ${i.run}: ${i.text}` }));
    const d = el("details");
    d.append(el("summary", { textContent: `Notes (${rest.length})` }), ul);
    box.append(d);
  }
  root.replaceChildren(box);
}
