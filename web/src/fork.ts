// SPDX-License-Identifier: Apache-2.0

// The fork panel (§5). A chat row's "fork here" opens it at that row's seq: a
// fork is a new room the viewer owns and drives, from the log up to there, with
// an optional first run on its own branch. Every reader may fork (policy Fork),
// so the panel is mounted for every viewer, watchers included.

import { nav } from "./api";
import type { Frame } from "./conn";
import { act, claimText, el, named, rejection, select, type Sender } from "./controls";
import type { RoomState } from "./room-state";
import { roomId } from "./view";

export function mountFork(root: HTMLElement, conn: Sender, state: RoomState) {
  root.classList.add("fork");
  root.hidden = true;
  let at = 0;
  let sent = 0; // the clientSeq of the fork awaiting its ack
  const title = el("span", { className: "fork-at" });
  const note = named(el("input", { name: "note", maxLength: 1024, placeholder: "why fork (optional)" }), "why fork");
  const role = select("forkRole", "role of the fork's first run", [["", "no run"], ["implementer", "implementer"], ["reviewer", "reviewer"], ["tester", "tester"], ["triager", "triager"]]);
  const prUrl = named(el("input", { name: "forkPrUrl", type: "url", placeholder: "PR URL (reviewer only)" }), "pull request URL, reviewer only");
  const egress = named(el("input", { name: "forkEgress", placeholder: "egress profiles: pypi, npm" }), "egress profiles");
  const cancel = el("button", { type: "button", textContent: "cancel" });
  cancel.onclick = () => { root.hidden = true; };
  const form = el("form");
  form.append(title, note, role, prUrl, egress, el("button", { type: "submit", textContent: "fork" }), cancel);
  const out = el("div", { className: "fork-result" });
  root.replaceChildren(form, out);

  form.onsubmit = (e) => {
    e.preventDefault();
    const action: Record<string, unknown> = { kind: "fork", seq: at };
    if (note.value.trim()) action.note = note.value.trim();
    if (role.value) {
      action.role = role.value;
      if (role.value === "reviewer" && prUrl.value.trim()) action.prUrl = prUrl.value.trim(); // reviewer only (docs/api.md)
      const profiles = egress.value.split(",").map((s) => s.trim()).filter(Boolean);
      if (profiles.length) action.egressProfiles = profiles;
    }
    sent = act(conn, state, action);
  };

  return {
    open(seq: number) {
      at = seq;
      title.textContent = `fork at #${seq}`;
      out.replaceChildren();
      root.hidden = false;
      note.focus();
    },
    // onAck takes every ack and answers only its own fork's. A fork with nothing
    // more to show opens the new room; one with a claim to apply or a run that
    // failed stays here, with a link to it.
    onAck(f: Frame) {
      if (!sent || f.clientSeq !== sent) return;
      sent = 0;
      if (f.rejected) return; // the page's notice says why
      const r = (f.result ?? {}) as { roomId?: unknown; run?: unknown; runError?: unknown };
      if (typeof r.roomId !== "string" || !roomId.test(r.roomId)) {
        out.replaceChildren(el("p", { textContent: "The broker answered an unexpected room id." }));
        return;
      }
      if (!r.run && !r.runError) {
        nav.go(`/r/${r.roomId}`);
        return;
      }
      out.replaceChildren(el("a", { href: `/r/${r.roomId}`, textContent: `open the fork ${r.roomId}` }));
      if (typeof r.runError === "string" && r.runError) {
        out.append(el("p", { textContent: `The fork's run was not started. ${rejection(r.runError)}` }));
      }
      if (r.run) out.append(el("pre", { textContent: claimText(r.run) }));
    },
  };
}
