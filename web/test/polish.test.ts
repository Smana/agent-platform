// SPDX-License-Identifier: Apache-2.0
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

// The interface-polish pass (2026-10-28): press, focus and hover states, hit areas, concentric
// radii and steady numbers, asserted on the stylesheet the way brand-style.test.ts does.
const css = readFileSync(join(__dirname, "../src/app.css"), "utf8");

/** The bodies of every `@media <query> {…}` block, braces balanced. */
function media(query: string): string {
  const out: string[] = [];
  const head = `@media ${query}`;
  for (let i = css.indexOf(head); i >= 0; i = css.indexOf(head, i + 1)) {
    const open = css.indexOf("{", i);
    let depth = 1, j = open + 1;
    for (; j < css.length && depth; j++) depth += css[j] === "{" ? 1 : css[j] === "}" ? -1 : 0;
    out.push(css.slice(open + 1, j - 1));
  }
  return out.join("\n");
}
/** The stylesheet outside every @media block. */
const outside = (() => {
  let s = css;
  for (;;) {
    const i = s.indexOf("@media");
    if (i < 0) return s;
    const open = s.indexOf("{", i);
    let depth = 1, j = open + 1;
    for (; j < s.length && depth; j++) depth += s[j] === "{" ? 1 : s[j] === "}" ? -1 : 0;
    s = s.slice(0, i) + s.slice(j);
  }
})();
/** The declarations of the first rule in `src` whose selector list is exactly `sel`. */
function decl(src: string, sel: string): string {
  const esc = sel.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const m = src.match(new RegExp(`(?:^|\\n)\\s*${esc}\\s*{([^}]*)}`));
  expect(m, `rule ${sel}`).not.toBeNull();
  return m![1];
}
const phone = media("(max-width: 640px)");
const motionOK = media("(prefers-reduced-motion: no-preference)");

describe("performance", () => {
  it("never transitions all", () => {
    expect(css).not.toMatch(/transition(-property)?:\s*all\b/);
    expect(css).not.toMatch(/will-change:\s*all/);
  });
});

describe("press feedback", () => {
  const press = "button:active:not(:disabled, .now-link, .clip-toggle, .t-more)";
  it("scales a pressed button to .96, only where motion is welcome", () => {
    expect(decl(motionOK, press)).toMatch(/scale:\s*\.96/);
    expect(outside).not.toMatch(/button:active[^{]*{[^}]*scale/);
  });
  it("leaves the buttons drawn as text lines unscaled: they underline instead", () => {
    expect(decl(outside, ".page-header .now-link:hover")).toMatch(/text-decoration:\s*underline/);
    expect(decl(outside, ".clip-toggle:hover")).toMatch(/text-decoration:\s*underline/);
    expect(decl(outside, ".t-more:hover")).toMatch(/text-decoration:\s*underline/);
  });
});

describe("focus", () => {
  it("rings every focused link in the ring token, like the buttons", () => {
    const a = decl(outside, "a:focus-visible");
    expect(a).toMatch(/outline:\s*2px solid var\(--ring\)/);
    expect(a).toMatch(/outline-offset:\s*2px/);
  });
});

describe("hover", () => {
  it("lifts an unchecked delivery segment toward the foreground", () => {
    expect(decl(outside, ".seg input:not(:checked, :disabled):hover + span")).toMatch(/color:\s*var\(--foreground\)/);
  });
  it("darkens a muted fold toggle on hover, with a colour-only transition", () => {
    const sel = ".session > summary, .cli-setup > summary, .notes details > summary, .fold > summary, .step-calls > summary, .controls .advanced > summary, .approval-json > summary";
    expect(decl(outside, sel)).toMatch(/transition-property:\s*color;/);
    expect(decl(outside, sel.split(", ").map((s) => `${s}:hover`).join(", "))).toMatch(/color:\s*var\(--foreground\)/);
  });
  it("fades the fork toolbar in rather than popping it", () => {
    expect(decl(outside, ".row-tools")).toMatch(/transition-property:\s*opacity/);
  });
});

describe("hit areas", () => {
  it("sets a 40 px target, 44 px on a phone", () => {
    expect(decl(outside, ":root")).toMatch(/--hit:\s*40px/);
    expect(decl(phone, ":root")).toMatch(/--hit:\s*44px/);
  });
  it("extends the small standalone controls with a centred pseudo-element of --hit", () => {
    const small = ".theme-toggle, .fork-here, .clip-toggle, .t-more, .cli-setup > summary, .notes details > summary";
    expect(decl(outside, small)).toMatch(/position:\s*relative/);
    const after = decl(outside, small.split(", ").map((s) => `${s}::after`).join(", "));
    expect(after).toMatch(/content:\s*""/);
    expect(after).toMatch(/position:\s*absolute/);
    expect(after).toMatch(/inset:\s*min\(0px,\s*\(100% - var\(--hit\)\) \/ 2\)/);
  });
  it("gives the dock's delivery segments a taller radio than their pill", () => {
    expect(decl(outside, ".seg input")).toMatch(/inset:\s*-6px 0/);
    expect(decl(phone, ".seg input")).toMatch(/inset:\s*-8px 0/);
  });
  it("makes the session toggle a 40 px target inside the dock's gaps", () => {
    expect(decl(outside, ".session > summary")).toMatch(/padding-block:\s*6px/);
    expect(decl(outside, ".session > summary::after")).toMatch(/inset:\s*-4px -8px/);
  });
  it("grows the chip links and the Needs-you pill to their half of the row gap", () => {
    expect(decl(outside, ".page-header .chip a::after")).toMatch(/inset:\s*-5px -2px/);
    expect(decl(outside, ".page-header .needs-pill::after")).toMatch(/inset:\s*-3px 0/);
    expect(decl(phone, ".page-header .needs-pill::after")).toMatch(/inset:\s*-8px 0/);
    expect(decl(phone, ".page-header .strip-chips")).toMatch(/padding-block:\s*8px;[^}]*margin-block:\s*-8px/);
  });
  it("grows the Now line on a phone, where its clamp would clip a pseudo-element", () => {
    expect(decl(phone, ".page-header .now-link")).toMatch(/padding-block:\s*12px;[^}]*margin-block:\s*-12px/);
  });
  it("sizes the room list's filters to the target", () => {
    expect(decl(outside, ".list-tools label, .list-tools select, .list-tools button")).toMatch(/min-height:\s*var\(--hit\)/);
  });
  it("gives every form control on a phone a 44 px target", () => {
    const sel = ".controls button, .controls select, .controls input:not([type=\"checkbox\"]), .needs button, .approval-decide input, .fork button, .fork input, .new-room button, .new-room input";
    expect(decl(phone, sel)).toMatch(/min-height:\s*var\(--hit\)/);
    expect(decl(phone, sel)).toMatch(/box-sizing:\s*border-box/);
    expect(decl(outside, ".composer button, .needs button")).toMatch(/min-height:\s*var\(--hit\)/);
  });
});

describe("steady numbers", () => {
  it("sets the strip's counters, the list's cells and the fold counts in tabular numerals", () => {
    for (const sel of [".strip-chips", ".strip-now", ".rooms th, .rooms td"]) {
      expect(decl(outside, sel), sel).toMatch(/font-variant-numeric:\s*tabular-nums/);
    }
    expect(decl(outside, ".notes details > summary, .fold > summary, .step-calls > summary, .t-sec")).toMatch(/font-variant-numeric:\s*tabular-nums/);
  });
});

describe("text wrapping", () => {
  it("keeps short UI lines free of orphans", () => {
    expect(decl(outside, ".waiting, .closed, .mark-text, .sys, .hint, .approval-meta")).toMatch(/text-wrap:\s*pretty/);
  });
});

describe("surfaces", () => {
  it("rounds the approval card concentrically: its 6 px controls plus 8 px of padding", () => {
    const a = decl(outside, ".approval");
    expect(a).toMatch(/border-radius:\s*14px/);
    expect(a).toMatch(/padding:\s*8px 12px/);
  });
  it("squares an open call's summary at the body's edge", () => {
    expect(decl(outside, ".tool[open] > summary")).toMatch(/border-radius:\s*8px 8px 0 0/);
  });
});

describe("icons", () => {
  it("weights Send's arrow to its bold label and trims the icon side's padding", () => {
    expect(decl(outside, "button.send .icon")).toMatch(/stroke-width:\s*2\.5/);
    expect(decl(outside, "button.send")).toMatch(/padding:\s*0 14px 0 16px/);
  });
});

describe("fork here on touch screens", () => {
  const touch = media("(hover: none), (pointer: coarse)");
  it("is never an invisible tap target: hidden means inert", () => {
    const hidden = decl(outside, ".row-tools");
    expect(hidden).toMatch(/opacity:\s*0/);
    expect(hidden).toMatch(/visibility:\s*hidden/);
    expect(hidden).toMatch(/pointer-events:\s*none/);
    expect(decl(outside, ".row:hover .row-tools, .row:focus-within .row-tools")).toMatch(/visibility:\s*visible/);
  });
  it("is shown on touch, in the flow under the row, clear of the timestamp", () => {
    const bar = decl(touch, ".row-tools");
    expect(bar).toMatch(/position:\s*static/);
    expect(bar).toMatch(/opacity:\s*1/);
    expect(bar).toMatch(/visibility:\s*visible/);
    expect(bar).toMatch(/pointer-events:\s*auto/);
  });
  it("is drawn quiet on touch: no border, background or shadow, muted 13 px text", () => {
    const b = decl(touch, ".row-tools .fork-here");
    expect(b).toMatch(/border:\s*(0|none)/);
    expect(b).toMatch(/background:\s*none|background:\s*transparent/);
    expect(b).toMatch(/box-shadow:\s*none/);
    expect(b).toMatch(/color:\s*var\(--muted-foreground\)/);
    expect(b).toMatch(/font-size:\s*13px/);
    expect(b).not.toMatch(/min-height:\s*[1-9]/);
  });
  it("gets its 44 px hit area from ::after, not from visible padding", () => {
    const a = decl(touch, ".row-tools .fork-here::after");
    expect(a).toMatch(/inset:\s*-12px\s+-8px/);
  });
});
