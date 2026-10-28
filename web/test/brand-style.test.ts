import { readFileSync, existsSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

const css = readFileSync(join(__dirname, "../src/app.css"), "utf8");
const html = readFileSync(join(__dirname, "../src/index.html"), "utf8");
const fonts = join(__dirname, "../src/assets/fonts");

/** The declarations of the first rule whose selector list is exactly `sel`. */
function rule(sel: string): string {
  const esc = sel.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const m = css.match(new RegExp(`(?:^|\\n)${esc}\\s*{([^}]*)}`));
  expect(m, `rule ${sel}`).not.toBeNull();
  return m![1];
}

describe("Maku typography", () => {
  it("loads only local Space Grotesk 400, 500 and 700 with swap", () => {
    const faces = [...css.matchAll(/@font-face\s*{([^}]*)}/g)].map((m) => m[1]);
    expect(faces).toHaveLength(3);
    for (const w of [400, 500, 700]) {
      const f = faces.find((x) => new RegExp(`font-weight:\\s*${w}\\b`).test(x))!;
      expect(f, `weight ${w}`).toBeDefined();
      expect(f).toMatch(/font-family:\s*"Space Grotesk"/);
      expect(f).toMatch(/font-style:\s*normal/);
      expect(f).toMatch(/font-display:\s*swap/);
      expect(f).toContain(`url("/assets/fonts/space-grotesk-latin-${w}-normal.woff2") format("woff2")`);
      expect(existsSync(join(fonts, `space-grotesk-latin-${w}-normal.woff2`))).toBe(true);
    }
    expect(rule("body")).toContain(
      'font: 500 16px/1.55 "Space Grotesk", ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
    );
    for (const src of [css, html]) {
      expect(src).not.toMatch(/@import/);
      expect(src).not.toMatch(/fonts\.(googleapis|gstatic)\.com/);
      expect(src).not.toMatch(/url\(\s*["']?https?:/);
    }
  });

  it("sets message prose at 400, the chrome at 500 and bold at 700", () => {
    expect(rule(".bubble .md, .handoff > .md, .notes")).toMatch(/font-weight:\s*400/);
    expect(rule("body")).toMatch(/font:\s*500 /);
    expect(rule("strong, b")).toMatch(/font-weight:\s*700/);
    expect(rule(".md :is(h1, h2, h3, h4, h5, h6)")).toMatch(/font-weight:\s*700/);
  });

  it("uses the readable Maku type scale", () => {
    expect(rule("body")).toMatch(/16px\/1\.55/);
    expect(rule(".strip-title")).toMatch(/font-size:\s*20px/);
    expect(rule(".strip-title")).toMatch(/line-height:\s*1\.3/);
    expect(rule(".strip-title")).toMatch(/font-weight:\s*700/);
    expect(rule(".page-header h1")).toMatch(/font-size:\s*20px/);
    expect(rule(".msg-head")).toMatch(/font-size:\s*13px/);
    expect(rule(".chip, .pol")).toMatch(/font-size:\s*13px/);
    expect(rule(".t-cmd, .t-command, .approval-command, pre, code")).toMatch(/font:\s*14px\/1\.5\s+ui-monospace/);
    expect(css).not.toMatch(/font-size:\s*12px/);
    expect(css).not.toMatch(/font:\s*12px/);
    expect(css).not.toMatch(/font-size:\s*1[0-1]px/);
  });

  it("keeps code, IDs, and timestamps monospace without monospacing prose", () => {
    const mono = rule("time, .t-time, .step-dur, .strip-conn, .page-dock .status, .replay-progress, .t-prompt");
    expect(mono).toMatch(/font-family:\s*ui-monospace/);
    expect(mono).toMatch(/font-size:\s*13px/);
    expect(mono).toMatch(/tabular-nums/);
    // The code group names every command, tool I/O and code surface.
    expect(css).toContain(".t-cmd, .t-command, .approval-command, pre, code {");
    // Prose and surfaces never take the monospace stack.
    for (const sel of ["body", ".bubble", ".msg", ".tool", ".chip, .pol", ".approval"]) {
      expect(rule(sel), `${sel} stays sans`).not.toMatch(/monospace/);
    }
  });
});

describe("Maku brand header", () => {
  it("switches the mark with .dark, never a media query", () => {
    expect(rule(".brand-mark-dark")).toMatch(/display:\s*none/);
    expect(rule(".dark .brand-mark-dark")).toMatch(/display:\s*block/);
    expect(rule(".dark .brand-mark-light")).toMatch(/display:\s*none/);
    expect(css).not.toMatch(/prefers-color-scheme[^{]*{[^}]*brand-mark/);
  });

  it("gives the mark clearspace of at least half its width", () => {
    const b = rule(".brand");
    expect(b).toMatch(/display:\s*flex/);
    expect(b).toMatch(/gap:\s*16px/);
    expect(b).toMatch(/text-decoration:\s*none/);
    expect(rule(".brand img")).toMatch(/width:\s*32px/);
    expect(rule(".brand img")).toMatch(/height:\s*32px/);
    // `.brand img` (0,1,1) would beat the hide rule (0,1,0): it must not set display.
    expect(rule(".brand img")).not.toMatch(/display/);
    expect(rule(".page-header")).toMatch(/padding:\s*16px/);
  });

  it("lays the header out as brand, content and actions, with a phone adjustment", () => {
    expect(rule(".page-header")).toMatch(/grid-template-columns:\s*auto minmax\(0,\s*1fr\) auto/);
    const phone = css.match(/@media \(max-width: 640px\)\s*{([\s\S]*?)\n}\n/g)?.join("\n") ?? "";
    expect(phone).toMatch(/\.brand\s*{[^}]*gap:\s*14px/);
    expect(phone).toMatch(/\.brand img\s*{[^}]*width:\s*28px/);
    expect(phone).toMatch(/\.brand-wordmark\s*{[^}]*font-size:\s*22px/);
    expect(phone).toMatch(/\.page-header\s*{[^}]*padding:\s*14px/);
    // The strip wraps under the brand: half the 28 px mark of clearspace below it too.
    expect(phone).toMatch(/\.page-header\s*{[^}]*row-gap:\s*14px/);
    expect(phone).toMatch(/\.page-header \.strip\s*{[^}]*grid-column:\s*1 \/ -1/);
  });

  it("sets the wordmark as lowercase 700 at -0.03em and keeps a visible focus", () => {
    const w = rule(".brand-wordmark");
    expect(w).toMatch(/font:\s*700 24px\/1/);
    expect(w).toMatch(/letter-spacing:\s*-0\.03em/);
    expect(w).not.toMatch(/text-transform/);
    expect(css).toMatch(/\.brand:focus-visible\s*{[^}]*outline/);
  });

  it("states only the font weights that exist", () => {
    expect(css).not.toMatch(/font-weight:\s*600/);
  });

  it("links favicons and preloads the three fonts, same origin", () => {
    expect(html).toContain("<title>Maku rooms</title>");
    expect(html).toContain('href="/assets/favicon/favicon.svg"');
    expect(html).toContain('href="/assets/favicon/favicon-16.png"');
    expect(html).toContain('href="/assets/favicon/favicon-32.png"');
    expect(html).toContain('rel="apple-touch-icon" href="/assets/favicon/apple-touch-icon.png"');
    for (const w of [400, 500, 700]) {
      expect(html).toMatch(new RegExp(`rel="preload"[^>]*href="/assets/fonts/space-grotesk-latin-${w}-normal\\.woff2"[^>]*as="font"[^>]*type="font/woff2"[^>]*crossorigin`));
    }
  });

  it("titles the preview pages and links their icons", () => {
    const read = (f: string) => readFileSync(join(__dirname, "../preview", f), "utf8");
    for (const f of ["preview.html", "full.html"]) {
      const h = read(f);
      expect(h).toMatch(/<title>Maku: /);
      expect(h).toContain('href="/assets/favicon/favicon.svg"');
      expect(h).toContain('href="/assets/fonts/space-grotesk-latin-700-normal.woff2"');
    }
    const d = read("demo.html");
    expect(d).toContain("<title>Maku: live demo</title>");
    expect(d).toContain('href="assets/favicon/favicon.svg"');
    expect(d).toContain('href="assets/fonts/space-grotesk-latin-500-normal.woff2"');
    expect(d).not.toMatch(/href="\//);
  });
});

describe("Markdown in a message", () => {
  it("keeps headings in the conversation's scale, never the browser's 2em", () => {
    const all = rule(".md :is(h1, h2, h3, h4, h5, h6)");
    expect(all).toMatch(/font-size:\s*16px/);
    expect(all).toMatch(/font-weight:\s*700/);
    expect(rule(".md h1")).toMatch(/font-size:\s*18px/);
  });
  it("sets inline code apart from prose", () => {
    expect(rule(".md :not(pre) > code")).toMatch(/background:/);
  });
});
