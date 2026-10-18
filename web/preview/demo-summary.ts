// SPDX-License-Identifier: Apache-2.0

// The demo's summary as the replay goes. The recorded summary is the run's end state;
// shown from the first event it would give the ending away (the verdict, the merge
// waiting). summaryAt folds the events shown so far into the fields the page reads,
// and hands back the recorded summary once the replay is done.

import type { RoomEvent } from "../src/conn";
import type { Summary } from "../src/summary";

export function summaryAt(final: Summary, shown: RoomEvent[], total: number, done: boolean): Summary {
  const notes = { untrusted: true as const, items: shown.filter((e) => e.type === "message" && e.payload.kind === "progress")
    .map((e) => ({ at: e.ts, run: e.runId ?? "", text: String(e.payload.text) })) };
  if (done) return { ...final, notes };

  const roles = new Map<string, string>();
  let run = null as Summary["status"]["run"];
  let handedOff = false;
  let verdict: Summary["status"]["lastVerdict"] = null;
  let pr: Summary["status"]["pr"] = null;
  for (const e of shown) {
    const p = e.payload;
    if (e.type === "state_changed" && p.kind === "run_requested") roles.set(p.runId, p.role);
    if (e.type === "state_changed" && p.kind === "run_phase" && e.runId)
      run = p.phase === "Running" ? { id: e.runId, role: roles.get(e.runId) ?? "" } : run?.id === e.runId ? null : run;
    if (e.type === "handoff") handedOff = true;
    if (e.type === "message" && p.kind === "review_verdict") verdict = { by: e.actor.id, verdict: p.verdict, at: e.ts };
    if (e.type === "tool_result" && final.status.pr && String(p.output).includes(final.status.pr.url)) pr = final.status.pr;
  }
  const budget = final.status.budget && { ...final.status.budget, usedTokens: Math.round(final.status.budget.usedTokens * shown.length / total) };
  return {
    ...final,
    status: { ...final.status, phase: verdict ? final.status.phase : handedOff ? "Reviewing" : "Implementing", run, budget, pr, lastVerdict: verdict,
      reason: verdict ? final.status.reason : undefined, next: verdict ? final.status.next : undefined },
    needsYou: verdict ? final.needsYou : [],
    notes,
  };
}
