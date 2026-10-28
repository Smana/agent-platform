// SPDX-License-Identifier: Apache-2.0

// What an approval card says around the agent's command: what it wants, why it stopped,
// what each button does, the deadline and whether this viewer may decide. All of it is
// fixed prose chosen by the broker's bounded class code (§6); nothing the agent wrote
// is ever the reason. An unknown code reads as the generic rule.

export interface Rule { intent: string; why: string; approve: string; deny: string }

const DENIED = "the agent is told no and none of it runs; it may try another way.";

export const RULES: Record<string, Rule> = {
  "forge.push": {
    intent: "Push commits to the forge.",
    why: "The room's policy asks a person before any push to the forge.",
    approve: "the push goes out and the run continues.",
    deny: `nothing is pushed; ${DENIED}`,
  },
  "forge.pr": {
    intent: "Open or change a pull request.",
    why: "The room's policy asks a person before a pull request is opened or changed.",
    approve: "the pull request action goes ahead.",
    deny: `no pull request is touched; ${DENIED}`,
  },
  "forge.other": {
    intent: "Run a command that may write to the forge.",
    why: "The command is outside the shapes the broker can read as safe (an allowlist): a forge write, an alias or an expansion it cannot see through.",
    approve: "the command runs as written, with the run's forge access.",
    deny: `the command does not run; ${DENIED}`,
  },
  "mcp.write": {
    intent: "Call an MCP tool that can write.",
    why: "The tool is not on the run's read-only list, so it may change something outside the room.",
    approve: "the tool call goes ahead.",
    deny: `the tool is not called; ${DENIED}`,
  },
  "shell.high": {
    intent: "Run a command its own harness flagged as high risk.",
    why: "The harness labelled the command high risk and no other rule matched it.",
    approve: "the command runs as written.",
    deny: `the command does not run; ${DENIED}`,
  },
  "egress.new": {
    intent: "Reach a network destination outside the run's egress profiles.",
    why: "The destination is not one of the run's allowed egress profiles.",
    approve: "the connection is allowed for this call.",
    deny: `no connection is made; ${DENIED}`,
  },
};

const GENERIC: Rule = {
  intent: "Take an action the room's policy gates.",
  why: "The room's policy asks a person before this kind of action.",
  approve: "the action goes ahead.",
  deny: `the action does not run; ${DENIED}`,
};

export const ruleFor = (cls: string): Rule => (Object.hasOwn(RULES, cls) ? RULES[cls] : GENERIC);

const pad = (n: number) => String(n).padStart(2, "0");
const clock = (d: Date) => `${pad(d.getHours())}:${pad(d.getMinutes())}`;

function inMinutes(ms: number): string {
  const m = Math.floor(ms / 60000);
  if (m < 1) return "under a minute";
  return m < 60 ? `${m} min` : `${Math.floor(m / 60)} h ${m % 60} min`;
}

export const EXPIRY = "the request expires and the agent is told no";

// deadlineText: "decide by HH:MM (in N min); if nobody decides: …". Another day's deadline
// shows its date. A missing or unreadable expiry says there is none.
export function deadlineText(expiresAt: string, now: number = Date.now()): string {
  const due = new Date(expiresAt);
  if (!expiresAt || isNaN(due.getTime()) || due.getFullYear() < 2000) return "No deadline.";
  const left = due.getTime() - now;
  if (left <= 0) return `The deadline has passed (${clock(due)}); ${EXPIRY}.`;
  const day = due.toDateString() === new Date(now).toDateString() ? "" : `${due.getFullYear()}-${pad(due.getMonth() + 1)}-${pad(due.getDate())} `;
  return `Decide by ${day}${clock(due)} (in ${inMinutes(left)}); if nobody decides: ${EXPIRY}.`;
}

export interface Standing {
  principal: string; role: string; approver: boolean;
}

// decideStatus says whether the viewer may decide this approval, and why not. The broker
// still enforces it (four-eyes, who decided first); this only explains. prompters absent
// (an older broker) never hides the buttons.
export function decideStatus(o: { you?: Standing; sealed?: boolean; fourEyes?: boolean; prompters?: string[] }): { can: boolean; text: string } {
  const y = o.you;
  if (o.sealed) return { can: false, text: "This room is sealed: nothing here can be decided." };
  if (!y || !(y.approver || y.role === "owner")) return { can: false, text: "Waiting for an approver: only approvers decide, and you are not one in this room." };
  if (o.fourEyes && o.prompters?.includes(y.principal)) {
    return { can: false, text: "Waiting for another approver: you prompted this turn (four-eyes)." };
  }
  return { can: true, text: "You can decide." };
}

// bannerText is the strip's line for pending approvals; "" when none. mine are those the
// viewer may decide.
export function bannerText(total: number, mine: number): string {
  if (total <= 0) return "";
  if (mine > 0) return `⏳ ${mine} approval${mine === 1 ? " waits" : "s wait"} for you`;
  return `⏳ ${total} approval${total === 1 ? " waits" : "s wait"} for another approver`;
}
