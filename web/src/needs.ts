// SPDX-License-Identifier: Apache-2.0

// The needs-you panel sits in the room's lead, under the title strip: one card per
// pending approval, command first, decided right there, then the summary's merges and
// holds. It is the page's one decide path (approvals are web-only); a card leaves when
// the log decides it (approval_decided), whoever decided, so a decide that lost the race
// (already_decided) leaves no dead button. Every string here may be agent-written: all
// of it enters the DOM as text.

import { reveal } from "./chat";
import type { Snapshot } from "./conn";
import { act, approvalAnchor, approvalCard, button, el, keyed, named, type Sender } from "./controls";
import type { RoomState } from "./room-state";
import { needsItems, type Summary } from "./summary";

// mountNeeds renders the panel into root. you reads the viewer as the latest state frame
// has it; chat finds a call's row for the card's link.
export function mountNeeds(root: HTMLElement, conn: Sender, state: RoomState, you: () => Snapshot["you"] | undefined,
  chat: { rowFor(callId: string): HTMLElement | undefined }): { refresh(): void; setSummary(s: Summary): void; count(): number } {
  root.classList.add("needs");
  root.setAttribute("aria-labelledby", "needs-title");
  const approvals = el("ul", { className: "approvals" });
  const other = el("ul", { className: "needs-other" });
  root.replaceChildren(el("h2", { id: "needs-title", textContent: "Needs you" }), approvals, other);
  const rows = keyed(approvals);
  // Approvers and owners decide (policy Decide); a sealed room answers every act sealed.
  // The broker still checks four-eyes and who decided first.
  const canDecide = () => { const y = you(); return !!y && !state.sealed && (y.approver || y.role === "owner"); };
  let others = 0;

  const card = (a: ReturnType<RoomState["approvals"]>[number], decide: boolean) => {
    const li = approvalCard(a, "li");
    li.id = approvalAnchor(a.approvalId);
    const command = li.querySelector<HTMLElement>(".approval-command")!;
    command.id = `${li.id}-command`;
    // A same-page link to the call's row, shown while the chat holds that row.
    const link = el("a", { className: "approval-call", href: "#", textContent: "see the call" });
    link.dataset.callId = a.callId;
    link.onclick = (e) => {
      e.preventDefault();
      const row = chat.rowFor(a.callId);
      if (!row) return;
      reveal(row); // the row, and any fold around it
      row.scrollIntoView?.({ block: "center" });
    };
    li.querySelector(".approval-meta")!.after(link);
    if (!decide) return li;
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
    li.append(bar);
    return li;
  };

  const panel = {
    refresh(): void {
      const decide = canDecide();
      const pending = state.approvals();
      rows(pending.map((a) => ({ key: a.approvalId, sig: String(decide), build: () => card(a, decide) })));
      // A call's row comes and goes (it leaves at the chat's cap): the link follows it
      // without a rebuild, which would drop a reason being typed.
      for (const l of approvals.querySelectorAll<HTMLAnchorElement>("a.approval-call")) l.hidden = !chat.rowFor(l.dataset.callId ?? "");
      root.hidden = pending.length === 0 && others === 0;
    },
    setSummary(s: Summary): void {
      const items = needsItems(s);
      other.replaceChildren(...items);
      others = items.length;
      panel.refresh();
    },
    // count is what needs this viewer: the approvals it may decide, then merges and holds.
    count(): number { return (canDecide() ? state.approvals().length : 0) + others; },
  };
  return panel;
}
