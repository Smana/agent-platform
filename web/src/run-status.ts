// SPDX-License-Identifier: Apache-2.0

// The run status is the first thing a room says: where it stands, in an icon, a colour and words.
// It mirrors internal/summary/runstatus.go, and web/test/run-status.test.ts runs the cases the Go
// side runs (internal/summary/testdata/runstatus.json). The words are fixed phrases, numbers and a
// role the page bounds: text an agent wrote never enters the line.

export type StatusKind = "running" | "queued" | "waiting-you" | "waiting-other" | "waiting-pr" | "waiting"
  | "finished" | "done" | "failed" | "stopped";

export interface RunStatus { kind: StatusKind | ""; icon: string; words: string }

export interface StatusIn {
  state?: string; phase?: string; reason?: string;
  runRole?: string; runStartedAt?: string; runPresent?: boolean;
  pr?: number; issue?: number;
  mine?: number; others?: number; // live approvals the viewer may decide, and those only others may
  step?: { kind: string; label?: string } | null;
  now?: number; // ms since the epoch
}

const FAILURES: Record<string, string> = {
  "budget-task": "budget reached", resume_budget: "budget reached", ci_red: "CI stayed red",
  ci_cancelled: "a check was cancelled", deadline: "time limit reached", agent_error: "the agent stopped on an error",
  agent_stuck: "the agent was stuck", stuck: "no activity", stalled: "no progress",
  not_met: "the acceptance checks are unmet", judge_impossible: "cannot be done as asked", pod_lost: "the sandbox was lost",
  run_unschedulable: "no capacity for the run", secret_scan_red: "the secret scan failed",
  review_rounds_exhausted: "the review did not settle", no_verdict: "no review verdict",
};

const ROLE = /^[a-z][a-z0-9-]{0,23}$/;

// elapsed is a duration at a glance: "<1 min", "4 min", "1 h 5 min".
export function elapsed(fromMs: number, nowMs: number): string {
  const m = Math.floor(Math.max(0, nowMs - fromMs) / 60000);
  return m < 1 ? "<1 min" : m < 60 ? `${m} min` : `${Math.floor(m / 60)} h ${m % 60} min`;
}

export function statusLine(i: StatusIn): RunStatus {
  const s = (kind: StatusKind, icon: string, words: string): RunStatus => ({ kind, icon, words });
  const fail = (): RunStatus => {
    const w = (i.reason && Object.hasOwn(FAILURES, i.reason) ? FAILURES[i.reason] : "")
      || (i.phase === "Rejected" ? "rejected" : i.phase === "Reverted" ? "the merge was reverted" : "");
    return s("failed", "✗", w ? `Failed — ${w}` : "Failed");
  };
  const mine = i.mine ?? 0, others = i.others ?? 0, pr = i.pr ?? 0, issue = i.issue ?? 0;
  const reasoned = !!i.reason && Object.hasOwn(FAILURES, i.reason);
  if (i.state === "done") return s("done", "✓", i.phase === "NoOp" ? "Done — nothing to change" : pr > 0 ? "Done — merged" : "Done");
  if (i.state === "failed") return fail();
  if (i.state === "stopped" || i.phase === "Closed" || i.phase === "Sealed") return s("stopped", "■", "Stopped");
  if (mine === 1) return s("waiting-you", "⏳", "Waiting for you — approve a pending action");
  if (mine > 1) return s("waiting-you", "⏳", `Waiting for you — decide ${mine} approvals`);
  if (others > 0) return s("waiting-other", "⏳", "Waiting for another approver");
  if (i.step?.kind === "pr" && pr > 0) return s("waiting-pr", "⏳", `Waiting — review and merge PR #${pr}`);
  if (i.step?.kind === "answer" && issue > 0) return s("waiting", "⏳", `Waiting — answer the question on issue #${issue}`);
  if (i.phase === "Escalated") return reasoned ? fail() : s("waiting", "⏳", "Waiting — a maintainer decides");
  if (i.state === "waiting_for_human") return s("waiting", "⏳", "Waiting — for the next message or run");
  if (i.state === "queued") return s("queued", "○", "Queued");
  if (i.state === "running") {
    if (i.runPresent) {
      const t = i.runStartedAt ? Date.parse(i.runStartedAt) : NaN;
      const parts = [i.runRole && ROLE.test(i.runRole) ? i.runRole : "", isNaN(t) ? "" : elapsed(t, i.now ?? Date.now())].filter(Boolean);
      return s("running", "●", parts.length ? `Running — ${parts.join(", ")}` : "Running");
    }
    if (pr > 0 && i.phase === "Reviewing") return s("finished", "✓", `Finished — PR #${pr} opened, handed to the reviewer`);
    if (pr > 0 && i.phase === "AwaitingCI") return s("finished", "✓", `Finished — PR #${pr} opened, waiting for CI`);
    return s("running", "●", "Running — between runs");
  }
  return { kind: "", icon: "", words: "" };
}
