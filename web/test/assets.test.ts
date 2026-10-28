import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { describe, expect, it } from "vitest";

// One source tree, web/src/assets, feeds production (dist/assets), the preview and the demo.
const web = join(__dirname, "..");
const repo = join(web, "..");
const src = join(web, "src", "assets");
const dist = join(repo, "internal", "humanapi", "ui", "dist");

const brand = [
  "svg/maku-mark-light.svg",
  "svg/maku-mark-dark.svg",
  "favicon/favicon.svg",
  "favicon/favicon-16.png",
  "favicon/favicon-32.png",
  "favicon/apple-touch-icon.png",
];
const faces = [400, 500, 700].map((w) => `fonts/space-grotesk-latin-${w}-normal.woff2`);
const fonts = [...faces, "fonts/LICENSE"];

function files(dir: string): string[] {
  return readdirSync(dir, { recursive: true, encoding: "utf8" })
    .filter((f) => statSync(join(dir, f)).isFile())
    .map((f) => relative(dir, join(dir, f)).split("\\").join("/"))
    .sort();
}

const read = (...p: string[]) => readFileSync(join(...p));

describe("static assets", () => {
  it("ships only the ten required source assets byte-for-byte", () => {
    expect(files(src)).toEqual([...brand, ...fonts].sort());
    for (const f of brand) expect(read(src, f).equals(read(repo, "docs", "assets", "brand", f)), f).toBe(true);
    for (const f of faces) expect(read(src, f).subarray(0, 4).toString("latin1"), f).toBe("wOF2");
    expect(read(src, "fonts/LICENSE").toString()).toContain("SIL Open Font License, Version 1.1");
  });

  it("builds into dist/assets: index.html alone at the root, every asset byte-identical", () => {
    expect(readdirSync(dist).sort()).toEqual(["assets", "index.html"]);
    expect(files(join(dist, "assets"))).toEqual(["app.css", "app.js", ...brand, ...fonts].sort());
    for (const f of [...brand, ...fonts]) expect(read(dist, "assets", f).equals(read(src, f)), f).toBe(true);
    expect(read(dist, "assets", "app.css").equals(read(web, "src", "app.css"))).toBe(true);
    expect(read(dist, "index.html").equals(read(web, "src", "index.html"))).toBe(true);
  });

  const entries: { file: string; base: string; title: string; css: string }[] = [
    { file: "src/index.html", base: "/assets/", title: "Maku rooms", css: "/assets/app.css" },
    { file: "preview/preview.html", base: "/assets/", title: "Maku: room log preview (offline fixture)", css: "/src/app.css" },
    { file: "preview/full.html", base: "/assets/", title: "Maku: room page preview (offline fixture)", css: "/src/app.css" },
    { file: "preview/list.html", base: "/assets/", title: "Maku: room list preview (offline fixture)", css: "/src/app.css" },
    { file: "preview/demo.html", base: "assets/", title: "Maku: live demo", css: "app.css" },
  ];

  it("references local favicon and font paths in every HTML entry", () => {
    for (const { file, base, title, css } of entries) {
      const html = read(web, file).toString();
      expect(html, file).toContain(`<title>${title}</title>`);
      expect(html, file).toContain(`<link rel="stylesheet" href="${css}">`);
      for (const f of ["favicon/favicon.svg", "favicon/favicon-32.png", "favicon/favicon-16.png", "favicon/apple-touch-icon.png", ...faces]) {
        expect(html, `${file}: ${f}`).toContain(`href="${base}${f}"`);
      }
      const hrefs = [...html.matchAll(/(?:href|src)="([^"]+)"/g)].map((m) => m[1]);
      for (const h of hrefs.filter((h) => h.includes("assets/"))) expect(h.startsWith(base), `${file}: ${h}`).toBe(true);
    }
  });

  it("has no external font or stylesheet request", () => {
    const css = read(web, "src", "app.css").toString();
    expect(css).not.toMatch(/@import|https?:\/\//);
    const urls = [...css.matchAll(/url\("([^"]+)"\)/g)].map((m) => m[1]);
    expect(urls).toEqual(faces.map((f) => `/assets/${f}`));
    for (const { file } of entries) {
      const html = read(web, file).toString();
      expect(html, file).not.toMatch(/(?:href|src)="(?:https?:)?\/\//);
      expect(html, file).not.toMatch(/fonts\.(?:googleapis|gstatic)/);
    }
  });
});
