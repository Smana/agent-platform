import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

// Every text pair the page paints must meet WCAG AA (4.5:1); meaningful borders 3:1.
// color-mix(in srgb, A p%, B) is a channel-wise lerp, which is how the chips are tinted.
const css = readFileSync(join(__dirname, "../src/app.css"), "utf8");
function tokens(block: string): Record<string, string> {
  const m = css.match(new RegExp(`${block.replace(".", "\\.")}\\s*{([^}]*)}`))!;
  return Object.fromEntries([...m[1].matchAll(/(--[\w-]+):\s*(#[0-9a-f]{6})/gi)].map((x) => [x[1], x[2].toLowerCase()]));
}
const rgb = (h: string) => [1, 3, 5].map((i) => parseInt(h.slice(i, i + 2), 16));
const lum = (h: string) => rgb(h).map((c) => { c /= 255; return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4; })
  .reduce((s, c, i) => s + c * [0.2126, 0.7152, 0.0722][i], 0);
export const ratio = (a: string, b: string) => { const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p); return (x + 0.05) / (y + 0.05); };
const mix = (a: string, b: string, pa: number) => "#" + rgb(a).map((c, i) => Math.round(c * pa + rgb(b)[i] * (1 - pa)).toString(16).padStart(2, "0")).join("");

const light = tokens(":root"), dark = tokens(".dark");

type Tokens = Record<string, string>;
// Retained pairs: still held to their minimum, with the values of the Maku palette.
const pairs = (t: Tokens) => [
  ["body text on the page", t["--foreground"], t["--muted"], 4.5],
  ["muted text on a card", t["--muted-foreground"], t["--card"], 4.5],
  ["folded plumbing and marker text on the page", t["--muted-foreground"], t["--muted"], 4.5],
  ["placeholder and meta text on a card", t["--placeholder"], t["--card"], 4.5],
  ["placeholder and meta text on the page", t["--placeholder"], t["--muted"], 4.5],
  ["links on the page", t["--link"], t["--muted"], 4.5],
  ["links on a card", t["--link"], t["--card"], 4.5],
  ["success text on a card", t["--success-text"], t["--card"], 4.5],
  ["destructive text on a card", t["--destructive"], t["--card"], 4.5],
  ["deny pill text on a tool row", t["--destructive-text"], t["--card"], 4.5],
  ["warning text on its tint", t["--warning-foreground"], mix(t["--warning"], t["--card"], 0.18), 4.5],
  ["primary button text", t["--primary-foreground"], t["--primary"], 4.5],
  ["the header's text", t["--brand-header-foreground"], t["--brand-header"], 4.5],
  ["the strip's chip borders", mix(t["--brand-header-foreground"], t["--brand-header"], 0.45), t["--brand-header"], 3],
  ["the needs-you pill", t["--warning-solid-foreground"], t["--warning"], 4.5],
  ["the budget meter's fill", t["--primary"], mix(t["--brand-header-foreground"], t["--brand-header"], 0.12), 3],
  ["the needs-you border", t["--warning-border"], t["--card"], 3],
  ["the deny button's text on a card", t["--destructive-text"], t["--card"], 4.5],
  ["the controls' group names", t["--muted-foreground"], t["--background"], 4.5],
  ["a destructive button on the dock's session panel", t["--destructive-text"], t["--surface-raised"], 4.5],
  ["the dock's notice", t["--destructive-text"], t["--surface-raised"], 4.5],
  ["the room list's notice on the page", t["--destructive-text"], t["--muted"], 4.5],
  ["destructive text on the page (.status-error, .summary-error, .mk-harness_error)", t["--destructive"], t["--muted"], 4.5],
  ["the focus ring on the header", t["--ring"], t["--brand-header"], 3],
  // The dock (P4, P8): its own surface, the composer's boundary, the delivery segments.
  ["text on the dock", t["--foreground"], t["--surface-raised"], 4.5],
  ["muted text on the dock (session line, hints)", t["--muted-foreground"], t["--surface-raised"], 4.5],
  ["placeholder text in the composer", t["--placeholder"], t["--card"], 4.5],
  ["the composer's boundary on the dock", t["--input"], t["--surface-raised"], 3],
  ["an unchecked delivery segment", t["--muted-foreground"], t["--muted"], 4.5],
  ["the checked delivery segment", t["--foreground"], t["--card"], 4.5],
  ["the focus ring on the dock", t["--ring"], t["--surface-raised"], 3],
  // A step's line and a call's status (P1, P3).
  ["a step's exceptions on the page", t["--warning-foreground"], t["--muted"], 4.5],
  ["a rejected call's word on a card", t["--warning-foreground"], t["--card"], 4.5],
] as const;

// The Maku matrices: [description, foreground, background, minimum, expectedRatio].
type Row = readonly [string, string, string, number, number];
const rows = (t: Tokens, i: 0 | 1): Row[] => {
  const warnTint = mix(t["--warning"], t["--card"], 0.18);
  const humanTint = mix(t["--primary"], t["--card"], 0.10);
  const hdr12 = mix(t["--brand-header-foreground"], t["--brand-header"], 0.12);
  const hdr45 = mix(t["--brand-header-foreground"], t["--brand-header"], 0.45);
  const T = 4.5, U = 3;
  const m = (d: string, fg: string, bg: string, min: number, e: [number, number]): Row => [d, fg, bg, min, e[i]];
  return [
    m("foreground on muted/page", t["--foreground"], t["--muted"], T, [17.77, 17.77]),
    m("card foreground on card", t["--card-foreground"], t["--card"], T, [19.75, 16.48]),
    m("muted foreground on card", t["--muted-foreground"], t["--card"], T, [6.64, 6.65]),
    m("muted foreground on muted/page", t["--muted-foreground"], t["--muted"], T, [5.97, 7.17]),
    m("muted foreground on raised/dock", t["--muted-foreground"], t["--surface-raised"], T, [6.44, 6.18]),
    m("muted foreground on background/controls", t["--muted-foreground"], t["--background"], T, [6.64, 7.17]),
    m("placeholder on card", t["--placeholder"], t["--card"], T, [5.33, 6.65]),
    m("placeholder on muted/page", t["--placeholder"], t["--muted"], T, [4.79, 7.17]),
    m("link on muted/page", t["--link"], t["--muted"], T, [6.39, 7.26]),
    m("link on card", t["--link"], t["--card"], T, [7.10, 6.73]),
    m("success text on card", t["--success-text"], t["--card"], T, [5.70, 9.50]),
    m("destructive on card", t["--destructive"], t["--card"], T, [5.24, 6.46]),
    m("destructive on muted/page", t["--destructive"], t["--muted"], T, [4.72, 6.97]),
    m("destructive text on card", t["--destructive-text"], t["--card"], T, [5.32, 7.09]),
    m("destructive text on muted/page", t["--destructive-text"], t["--muted"], T, [4.79, 7.65]),
    m("destructive text on raised/dock", t["--destructive-text"], t["--surface-raised"], T, [5.16, 6.59]),
    m("warning foreground on warning tint", t["--warning-foreground"], warnTint, T, [10.73, 8.59]),
    m("warning foreground on muted/page", t["--warning-foreground"], t["--muted"], T, [11.13, 12.74]),
    m("warning foreground on card", t["--warning-foreground"], t["--card"], T, [12.37, 11.81]),
    m("primary foreground on primary", t["--primary-foreground"], t["--primary"], T, [6.39, 7.26]),
    m("header foreground on header", t["--brand-header-foreground"], t["--brand-header"], T, [19.75, 16.48]),
    m("warning solid foreground on warning pill", t["--warning-solid-foreground"], t["--warning"], T, [5.56, 8.87]),
    m("foreground on raised/dock", t["--foreground"], t["--surface-raised"], T, [19.17, 15.31]),
    m("card foreground on human tint", t["--card-foreground"], humanTint, T, [16.80, 14.27]),
    m("link on human tint", t["--link"], humanTint, T, [6.04, 5.83]),
    m("success foreground on success fill", t["--success-foreground"], t["--success"], T, [4.94, 7.99]),
    m("destructive foreground on destructive fill", t["--destructive-foreground"], t["--destructive"], T, [5.24, 6.97]),
    m("accent foreground on accent fill", t["--accent-foreground"], t["--accent"], T, [7.99, 7.99]),
    // Non-text: meaningful boundaries and focus rings.
    m("header chip border on header", hdr45, t["--brand-header"], U, [3.15, 4.18]),
    m("primary meter fill on header-foreground 12% mix", t["--primary"], hdr12, U, [5.48, 4.88]),
    m("warning border on card", t["--warning-border"], t["--card"], U, [4.05, 8.23]),
    m("ring on header", t["--ring"], t["--brand-header"], U, [7.10, 6.73]),
    m("ring on card", t["--ring"], t["--card"], U, [7.10, 6.73]),
    m("ring on muted/page", t["--ring"], t["--muted"], U, [6.39, 7.26]),
    m("ring on raised/dock", t["--ring"], t["--surface-raised"], U, [6.90, 6.25]),
    m("input boundary on card", t["--input"], t["--card"], U, [4.24, 3.91]),
    m("input boundary on raised/dock", t["--input"], t["--surface-raised"], U, [4.12, 3.63]),
  ];
};

// The exact Maku semantic token table: [token, light, dark].
const MAKU_TOKENS: [string, string, string][] = [
  ["--background", "#ffffff", "#0a0a0f"],
  ["--foreground", "#0a0a0f", "#f3f2fa"],
  ["--card-foreground", "#0a0a0f", "#f3f2fa"],
  ["--card", "#ffffff", "#14141c"],
  ["--muted", "#f3f2fa", "#0a0a0f"],
  ["--muted-foreground", "#5e5a6e", "#9b9aae"],
  ["--surface-raised", "#fcfbff", "#1c1b26"],
  ["--border", "#d8d5e3", "#3c3949"],
  ["--input", "#7e788c", "#777185"],
  ["--primary", "#6d28d9", "#a78bfa"],
  ["--link", "#6d28d9", "#a78bfa"],
  ["--ring", "#6d28d9", "#a78bfa"],
  ["--primary-foreground", "#f3f2fa", "#0a0a0f"],
  ["--accent", "#6ab825", "#6ab825"],
  ["--accent-foreground", "#0a0a0f", "#0a0a0f"],
  ["--success", "#4d8f14", "#6ab825"],
  ["--success-foreground", "#0a0a0f", "#0a0a0f"],
  ["--success-text", "#3a7410", "#7fcf3a"],
  ["--warning", "#ed9d13", "#ed9d13"],
  ["--warning-foreground", "#4a2f00", "#f5c97f"],
  ["--warning-solid-foreground", "#4a2f00", "#0a0a0f"],
  ["--warning-border", "#b36f00", "#ed9d13"],
  ["--destructive", "#d22323", "#f37171"],
  ["--destructive-foreground", "#ffffff", "#0a0a0f"],
  ["--destructive-text", "#d02323", "#f47e7e"],
  ["--brand-header", "#ffffff", "#14141c"],
  ["--brand-header-foreground", "#0a0a0f", "#f3f2fa"],
  ["--placeholder", "#6b6b6b", "#9b9aae"],
];

describe("Maku tokens", () => {
  it("pins the Maku semantic token values", () => {
    for (const [name, l, d] of MAKU_TOKENS) {
      expect(light[name], `light ${name}`).toBe(l);
      expect(dark[name], `dark ${name}`).toBe(d);
    }
    expect(css).not.toMatch(/--brand-navy/);
  });

  it("rejects inaccessible supplied pairs as normal-text tokens", () => {
    const [ink, surface, paper, core, muted] = ["#0a0a0f", "#14141c", "#f3f2fa", "#8b5cf6", "#9b9aae"];
    expect(ratio(core, paper)).toBeCloseTo(3.81, 2);
    expect(ratio(core, surface)).toBeCloseTo(4.33, 2);
    expect(ratio(paper, core)).toBeCloseTo(3.81, 2);
    expect(ratio(muted, paper)).toBeCloseTo(2.48, 2);
    for (const bad of [ratio(core, paper), ratio(core, surface), ratio(paper, core), ratio(muted, paper)]) expect(bad).toBeLessThan(4.5);
    // Core passes on Ink only, and the valid brand-dark pairs stay valid.
    expect(ratio(core, ink)).toBeCloseTo(4.66, 2);
    expect(ratio(paper, ink)).toBeCloseTo(17.77, 2);
    expect(ratio(paper, surface)).toBeCloseTo(16.48, 2);
    expect(ratio(muted, ink)).toBeCloseTo(7.17, 2);
    expect(ratio(muted, surface)).toBeCloseTo(6.65, 2);
    // No token carries a rejected pair as normal text.
    for (const t of [light, dark]) {
      expect(t["--link"]).not.toBe(core);
      expect(t["--primary"]).not.toBe(core);
      // Muted is never the secondary text on a Paper page (2.48:1).
      const mutedOnPaper = t["--muted-foreground"] === muted && t["--muted"] === paper;
      expect(mutedOnPaper).toBe(false);
    }
    expect(light["--muted-foreground"]).not.toBe(muted);
  });
});

describe.each([["light", light, 0], ["dark", dark, 1]] as const)("%s theme", (_name, t, i) => {
  it.each(pairs(t))("%s", (_what, fg, bg, min) => {
    expect(fg, "token defined").toBeDefined();
    expect(ratio(fg, bg)).toBeGreaterThanOrEqual(min);
  });
  it.each(rows(t, i))("matrix: %s", (_what, fg, bg, min, expected) => {
    expect(fg, "token defined").toBeDefined();
    expect(ratio(fg, bg)).toBeGreaterThanOrEqual(min);
    expect(ratio(fg, bg)).toBeCloseTo(expected, 2);
  });
});
