// SPDX-License-Identifier: Apache-2.0

// The chat/raw switch: two containers under main, one hidden, both fed on every
// event so the toggle is instant and the debug view is never stale. The choice
// survives a reload in storage.

import { ChatView } from "./chat";
import type { RoomEvent } from "./conn";
import { renderEvent } from "./render";
import { RoomLog } from "./view";

export type Mode = "chat" | "raw";

const key = "room-view";

export interface Views {
  apply(ev: RoomEvent): void;
}

// mountViews fills main with the chat and raw containers and adds the toggle to
// bar. store is a parameter so tests drive it; a throwing Storage (private
// mode) only loses persistence.
export function mountViews(main: HTMLElement, bar: HTMLElement, store: Storage = localStorage): Views {
  const chatEl = document.createElement("div");
  chatEl.className = "view-chat";
  const rawEl = document.createElement("div");
  rawEl.className = "view-raw";
  main.replaceChildren(chatEl, rawEl);

  const chat = new ChatView(chatEl);
  const raw = new RoomLog(rawEl);

  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "view-toggle";
  bar.append(btn);

  let mode: Mode = "chat";
  try {
    if (store.getItem(key) === "raw") mode = "raw";
  } catch { /* no storage: the default holds */ }

  const show = () => {
    chatEl.hidden = mode !== "chat";
    rawEl.hidden = mode !== "raw";
    btn.textContent = mode === "chat" ? "⇄ raw" : "⇄ chat";
    btn.title = mode === "chat" ? "Show the raw event stream" : "Show the chat view";
  };
  btn.onclick = () => {
    mode = mode === "chat" ? "raw" : "chat";
    try { store.setItem(key, mode); } catch { /* no storage: the switch still works */ }
    show();
  };
  show();

  return {
    apply(ev: RoomEvent) {
      chat.apply(ev);
      raw.put(ev.seq, renderEvent(ev));
    },
  };
}
