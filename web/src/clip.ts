// SPDX-License-Identifier: Apache-2.0

// A long message folds: past about a dozen lines its bubble shows clipped, under a fade
// in the bubble's own background, with "Show more"; "Show less" folds it again. A body
// folds only when it runs over by more than two lines: a few pixels of margin never earn
// a button. Whether it does is read once it is laid out, and again when its size changes,
// by a ResizeObserver that reads every body before it writes any, so a replayed backlog
// lays out once, not once a row. jsdom has neither layout nor the observer: there attach
// measures at once, with the measure the test passes. What the reader opened is kept by
// seq, not on the node: a resend or a resume renders a fresh bubble that opens as they
// left it, and one that fits again (a wider window) drops its button until it does not.

// The clip (app.css: calc(12 * 1.55em), the body's line height) and the slack past it.
const clipLines = 12;
const slackLines = 2;

// overflows is whether a body runs past the clip by more than the slack: false until laid
// out. It reads the content's height, not the box's, so it holds clipped or not.
export function overflows(body: HTMLElement): boolean {
  const line = parseFloat(getComputedStyle(body).lineHeight);
  return line > 0 && body.scrollHeight > (clipLines + slackLines) * line;
}

type ClipState = "open" | "closed" | "short";

// The seq of a body's message: its row carries it.
const seqOf = (body: HTMLElement) => Number(body.closest<HTMLElement>("[data-seq]")?.dataset.seq);

// Clips holds which messages the reader opened, and measures them all.
export class Clips {
  private opened = new Set<number>();
  private observer?: ResizeObserver;

  constructor(private over: (body: HTMLElement) => boolean = overflows) {
    if (typeof ResizeObserver === "function") {
      this.observer = new ResizeObserver((entries) => {
        const bodies = entries.map((e) => e.target as HTMLElement);
        for (const b of bodies) if (!b.isConnected) this.observer!.unobserve(b); // a replaced or evicted row
        const live = bodies.filter((b) => b.isConnected);
        const long = live.map((b) => this.over(b)); // every read first, then every write
        live.forEach((b, i) => this.decide(b, long[i], seqOf(b)));
      });
    }
  }

  // attach makes bubble's body, the message of seq, foldable; bubble already sits in its row.
  attach(bubble: HTMLElement, body: HTMLElement, seq: number): void {
    body.id = `msg-${seq}-body`;
    // A link in the hidden tail can take the focus: the message opens rather than scroll
    // it under the fade. Its own button toggles it, and is not in the body.
    bubble.addEventListener("focusin", (e) => {
      if (bubble.dataset.clip === "closed" && body.contains(e.target as Node)) this.toggle(bubble, seq);
    });
    if (this.observer) this.observer.observe(body);
    else this.decide(body, this.over(body), seq);
  }

  // forget drops a seq that left the page.
  forget(seq: number): void { this.opened.delete(seq); }

  // decide writes a measured body's state: short when it fits, else as the reader left it.
  private decide(body: HTMLElement, long: boolean, seq: number) {
    const bubble = body.parentElement;
    if (bubble) this.show(bubble, !long ? "short" : this.opened.has(seq) ? "open" : "closed", seq);
  }

  private toggle(bubble: HTMLElement, seq: number) {
    if (this.opened.delete(seq)) {
      this.show(bubble, "closed", seq);
      bubble.scrollIntoView?.({ block: "nearest" }); // the reader may be far below its top
    } else {
      this.opened.add(seq);
      this.show(bubble, "open", seq);
    }
  }

  // show writes a bubble's state onto it and its button. A short message has none: the
  // button is made the first time its bubble folds, and hides while it fits.
  private show(bubble: HTMLElement, state: ClipState, seq: number) {
    bubble.dataset.clip = state;
    let b = bubble.querySelector<HTMLButtonElement>(":scope > button.clip-toggle");
    if (!b && state === "short") return;
    b ??= this.button(bubble, seq);
    b.hidden = state === "short";
    b.textContent = state === "open" ? "Show less" : "Show more";
    b.setAttribute("aria-expanded", String(state === "open"));
  }

  private button(bubble: HTMLElement, seq: number): HTMLButtonElement {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "clip-toggle";
    b.setAttribute("aria-controls", `msg-${seq}-body`);
    b.onclick = () => this.toggle(bubble, seq);
    bubble.append(b);
    return b;
  }
}
