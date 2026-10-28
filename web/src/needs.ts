// SPDX-License-Identifier: Apache-2.0

// The needs-you panel sits in the room's lead, under the title strip: one card per
// pending approval, command first, decided right there, then the summary's merges and
// holds. It is the page's one decide path (approvals are web-only); a card leaves when
// the log decides it (approval_decided), whoever decided, so a decide that lost the race
// (already_decided) leaves no dead button. Every string here may be agent-written: all
// of it enters the DOM as text.

import { decideStatus, deadlineText, ruleFor } from "./approval-help";
import { focusRow } from "./chat";
import type { Snapshot } from "./conn";
import { act, approvalAnchor, approvalCard, button, el, keyed, named, type Sender } from "./controls";
import type { RoomState } from "./room-state";
import { needsItems, type Summary } from "./summary";

// mountNeeds renders the panel into root. you reads the viewer as the latest state frame
// has it; chat finds a call's row for the card's link.
export function mountNeeds(root: HTMLElement, conn: Sender, state: RoomState, you: () => Snapshot["you"] | undefined,
  chat: { rowFor(callId: string): HTMLElement | undefined }, fourEyes: () => boolean | undefined = () => undefined):
  { refresh(): void; setSummary(s: Summary): void; count(): number; approvalCounts(): { total: number; mine: number }; focusFirst(): void } {
  root.classList.add("needs");
  root.setAttribute("aria-labelledby", "needs-title");
  const approvals = el("ul", { className: "approvals" });
  const other = el("ul", { className: "needs-other" });
  const title = el("h2", { id: "needs-title", textContent: "Needs you" });
  root.replaceChildren(title, approvals, other);
  root.hidden = true; // until something needs showing: never an empty heading
  const rows = keyed(approvals);
  // Approvers and owners decide (policy Decide); a sealed room answers every act sealed;
  // with four-eyes, whoever prompted the turn waits for another. The card says which, and
  // the broker still checks four-eyes and who decided first.
  type Approval = ReturnType<RoomState["approvals"]>[number];
  const standing = (a: Approval) => decideStatus({ you: you(), sealed: state.sealed, fourEyes: fourEyes(), prompters: a.prompters });
  let others = 0;

  const card = (a: Approval, st: { can: boolean; text: string }) => {
    const li = approvalCard(a, "li");
    li.id = approvalAnchor(a.approvalId);
    const command = li.querySelector<HTMLElement>(".approval-command")!;
    command.id = `${li.id}-command`;
    // What it wants, why it stopped, what each answer does, by when, who may: fixed prose
    // per class. The agent's own command and raw action sit folded under "details".
    const rule = ruleFor(a.class);
    const intent = el("p", { className: "approval-intent", id: `${li.id}-intent`, textContent: rule.intent });
    const why = el("p", { className: "approval-why", textContent: `Why it stopped: ${rule.why}` });
    const outcomes = el("div", { className: "approval-outcomes" });
    outcomes.append(el("p", { textContent: `If you approve: ${rule.approve}` }), el("p", { textContent: `If you deny: ${rule.deny}` }));
    const deadline = el("p", { className: "approval-deadline", textContent: deadlineText(a.expiresAt) });
    deadline.dataset.expires = a.expiresAt;
    const status = el("p", { className: "approval-status", textContent: st.text });
    status.dataset.can = String(st.can);
    const details = el("details", { className: "approval-details" });
    details.append(el("summary", { textContent: "details" }), ...Array.from(li.children));
    li.replaceChildren(intent, why, outcomes, deadline, status, details);
    // A same-page link to the call's row, shown while the chat holds that row; refresh
    // names it by the row's seq, which also tells two cards' links apart.
    const link = el("a", { className: "approval-call", href: "#" });
    link.dataset.callId = a.callId;
    link.onclick = (e) => {
      e.preventDefault();
      const row = chat.rowFor(a.callId);
      if (row) focusRow(row); // the row, and any fold around it, opened and focused
    };
    details.before(link);
    if (!st.can) return li;
    const reason = named(el("input", { name: "decisionReason", maxLength: 1024, placeholder: "reason (optional)" }), "reason for the decision (optional)");
    const send = (decision: string) => () => {
      const action: Record<string, unknown> = { kind: "decide", approvalId: a.approvalId, decision };
      if (reason.value.trim()) action.reason = reason.value.trim();
      act(conn, state, action);
    };
    const approve = button("approve", "approve", send("approved"));
    approve.classList.add("primary");
    const deny = button("deny", "deny", send("denied"));
    deny.classList.add("deny");
    // Each button names what it decides: "approve" alone repeats on every card.
    for (const e of [reason, approve, deny]) e.setAttribute("aria-describedby", command.id);
    const bar = el("div", { className: "approval-decide" });
    bar.append(reason, approve, deny);
    details.before(bar);
    return li;
  };

  const panel = {
    refresh(): void {
      const pending = state.approvals();
      rows(pending.map((a) => { const st = standing(a); return { key: a.approvalId, sig: st.text, build: () => card(a, st) }; }));
      // The deadline counts down without a rebuild, which would drop a reason being typed.
      for (const d of approvals.querySelectorAll<HTMLElement>("p.approval-deadline")) {
        const text = deadlineText(d.dataset.expires ?? "");
        if (d.textContent !== text) d.textContent = text;
      }
      // A call's row comes and goes (it leaves at the chat's cap): the link follows it
      // without a rebuild, which would drop a reason being typed.
      for (const l of approvals.querySelectorAll<HTMLAnchorElement>("a.approval-call")) {
        const row = chat.rowFor(l.dataset.callId ?? "");
        l.hidden = !row;
        const text = row ? `see the call #${row.dataset.seq ?? "?"}` : "see the call";
        if (l.textContent !== text) l.textContent = text;
      }
      // A viewer who decides nothing here and has no merge or hold reads the cards as
      // someone else's turn.
      const heading = panel.count() > 0 ? "Needs you" : "Waiting for an approver";
      if (title.textContent !== heading) title.textContent = heading;
      root.hidden = pending.length === 0 && others === 0;
    },
    setSummary(s: Summary): void {
      const items = needsItems(s);
      other.replaceChildren(...items);
      others = items.length;
      panel.refresh();
    },
    // count is what needs this viewer: the approvals it may decide, then merges and holds.
    count(): number { return panel.approvalCounts().mine + others; },
    // approvalCounts: the pending approvals, and those this viewer may decide.
    approvalCounts(): { total: number; mine: number } {
      if (state.sealed) return { total: 0, mine: 0 };
      const all = state.approvals();
      return { total: all.length, mine: all.filter((a) => standing(a).can).length };
    },
    // focusFirst takes the reader, and the keyboard focus, to the first card.
    focusFirst(): void {
      const li = approvals.querySelector<HTMLElement>("li.approval");
      if (!li) return;
      li.tabIndex = -1;
      li.scrollIntoView?.({ block: "start" });
      li.focus({ preventScroll: true });
    },
  };
  return panel;
}
