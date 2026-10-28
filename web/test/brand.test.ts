import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { mountBrand } from "../src/brand";

describe("mountBrand", () => {
  it("renders the SVG mark and real lowercase wordmark", () => {
    const host = document.createElement("header");
    const a = mountBrand(host);
    expect(host.querySelectorAll("a.brand")).toHaveLength(1);
    expect(a.getAttribute("aria-label")).toBe("Maku home");
    expect(a.getAttribute("href")).toBe("/");
    expect(a.querySelector(".brand-wordmark")?.textContent).toBe("maku");
    expect(a.querySelector(".brand-mark-light")?.getAttribute("src")).toBe("/assets/svg/maku-mark-light.svg");
    expect(a.querySelector(".brand-mark-dark")?.getAttribute("src")).toBe("/assets/svg/maku-mark-dark.svg");
    const imgs = [...a.querySelectorAll("img")];
    expect(imgs).toHaveLength(2);
    for (const img of imgs) {
      expect(img.getAttribute("alt")).toBe("");
      expect(img.getAttribute("aria-hidden")).toBe("true");
      expect(img.getAttribute("width")).toBe("32");
      expect(img.getAttribute("height")).toBe("32");
    }
  });

  it("supports a relative demo asset base without duplicating the component", () => {
    const host = document.createElement("header");
    const a = mountBrand(host, "assets");
    expect(a.querySelector(".brand-mark-light")?.getAttribute("src")).toBe("assets/svg/maku-mark-light.svg");
    expect(a.querySelector(".brand-mark-dark")?.getAttribute("src")).toBe("assets/svg/maku-mark-dark.svg");
    expect(a.innerHTML).not.toMatch(/\.png|lockup/);
    expect(a.getAttribute("href")).toBe("/");
    expect(mountBrand(host, "assets", "./").getAttribute("href")).toBe("./");
  });
});

describe("preview page", () => {
  it("mounts exactly one brand, with the demo's relative asset base", async () => {
    const { mountPreviewPage } = await import("../preview/page");
    const { initTheme } = await import("../src/theme");
    const read = (f: string) => JSON.parse(readFileSync(join(__dirname, "fixtures", f), "utf8"));
    const app = document.createElement("div");
    mountPreviewPage(app, { snap: read("snapshot.json"), summary: read("summary.json"), count: 0, nothingSent: "x", sessionOpen: false,
      assetBase: "assets", homeHref: "./", theme: initTheme() });
    expect(app.querySelectorAll(".brand")).toHaveLength(1);
    expect(app.querySelector(".brand-mark-light")?.getAttribute("src")).toBe("assets/svg/maku-mark-light.svg");
    expect(app.querySelector(".brand")?.getAttribute("href")).toBe("./");
    const dflt = document.createElement("div");
    mountPreviewPage(dflt, { snap: read("snapshot.json"), summary: read("summary.json"), count: 0, nothingSent: "x", sessionOpen: false, theme: initTheme() });
    expect(dflt.querySelector(".brand")?.getAttribute("href")).toBe("/");
    expect(dflt.querySelector(".brand-mark-dark")?.getAttribute("src")).toBe("/assets/svg/maku-mark-dark.svg");
  });
});
