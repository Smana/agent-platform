// SPDX-License-Identifier: Apache-2.0

// The chat sits in main; the raw event stream sits below it in a <details> closed by
// default, so the page leads with the conversation and the summary, not the noise.
// Both are fed on every event, so opening the raw log is instant and never stale.

import { ChatView } from "./chat";
import type { RoomEvent } from "./conn";
import { renderEvent } from "./render";
import { RoomLog } from "./view";

export interface Views {
  apply(ev: RoomEvent): void;
  rowFor(callId: string): HTMLElement | undefined; // a call's chat row, while the page holds it
  counters(text: string): void; // the protocol's self-check, on the raw log's summary line
}

// mountViews fills main with the chat and the collapsed raw log. fork, when set,
// puts "fork here" on chat messages and handoffs; you is the viewer's principal, once known.
export function mountViews(main: HTMLElement, fork?: (seq: number) => void, you?: () => string | undefined): Views {
  const chatEl = document.createElement("div");
  chatEl.className = "view-chat";
  const rawEl = document.createElement("div");
  rawEl.className = "view-raw";
  const details = document.createElement("details");
  details.className = "raw-events";
  const title = document.createElement("summary");
  title.textContent = "Raw events";
  details.append(title, rawEl);
  main.replaceChildren(chatEl, details);

  const chat = new ChatView(chatEl, undefined, fork, you);
  const raw = new RoomLog(rawEl);

  return {
    apply(ev: RoomEvent) {
      chat.apply(ev);
      raw.put(ev.seq, renderEvent(ev));
    },
    rowFor: (callId) => chat.rowFor(callId),
    counters(text: string) { title.textContent = `Raw events · ${text}`; },
  };
}
