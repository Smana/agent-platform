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
const pairs = (t: Record<string, string>) => [
  ["body text on the page", t["--foreground"], t["--muted"], 4.5],
  ["muted text on a card", t["--muted-foreground"], t["--card"], 4.5],
  ["folded plumbing and marker text on the page", t["--muted-foreground"], t["--muted"], 4.5],
  ["placeholder and meta text on a card", t["--placeholder"], t["--card"], 4.5],
  ["placeholder and meta text on the page", t["--placeholder"], t["--muted"], 4.5],
  ["links on the page", t["--link"], t["--muted"], 4.5],
  ["links on a card", t["--link"], t["--card"], 4.5],
  ["success text on a card", t["--success-text"], t["--card"], 4.5],
  ["ok chip text on its tint", t["--success-text"], mix(t["--success"], t["--card"], 0.12), 4.5],
  ["running chip text on its tint", t["--link"], mix(t["--primary"], t["--card"], 0.10), 4.5],
  ["destructive text on a card", t["--destructive"], t["--card"], 4.5],
  ["error chip text on its tint", t["--destructive-text"], mix(t["--destructive"], t["--card"], 0.10), 4.5],
  ["deny pill text on a tool row", t["--destructive-text"], t["--card"], 4.5],
  ["warning text on its tint", t["--warning-foreground"], mix(t["--warning"], t["--card"], 0.18), 4.5],
  ["primary button text", t["--primary-foreground"], t["--primary"], 4.5],
  ["the header's text", t["--brand-navy-foreground"], t["--brand-navy"], 4.5],
  ["the strip's chip borders", mix(t["--brand-navy-foreground"], t["--brand-navy"], 0.45), t["--brand-navy"], 3],
  ["the needs-you pill", t["--brand-navy"], t["--warning"], 4.5],
  ["the budget meter's fill", t["--accent"], mix(t["--brand-navy-foreground"], t["--brand-navy"], 0.20), 3],
  ["the needs-you border", t["--warning-border"], t["--card"], 3],
  ["the deny button's text on a card", t["--destructive-text"], t["--card"], 4.5],
  ["the controls' group names", t["--muted-foreground"], t["--background"], 4.5],
  ["a destructive button on the controls", t["--destructive"], t["--background"], 4.5],
] as const;

describe.each([["light", light], ["dark", dark]])("%s theme", (_name, t) => {
  it.each(pairs(t))("%s", (_what, fg, bg, min) => {
    expect(fg, "token defined").toBeDefined();
    expect(ratio(fg, bg)).toBeGreaterThanOrEqual(min);
  });
});
