import { describe, expect, it } from "vitest";
import { icon } from "../src/icons";

describe("icon", () => {
  it("draws an outline glyph in currentColor on a 16 px grid", () => {
    const svg = icon("check");
    expect(svg.namespaceURI).toBe("http://www.w3.org/2000/svg");
    expect(svg.getAttribute("viewBox")).toBe("0 0 16 16");
    expect([svg.getAttribute("fill"), svg.getAttribute("stroke"), svg.getAttribute("stroke-width")]).toEqual(["none", "currentColor", "1.5"]);
    expect(svg.getAttribute("class")).toBe("icon icon-check");
    expect(svg.querySelectorAll("path").length).toBeGreaterThan(0);
  });
  it("hides a decorative glyph from assistive tech", () => {
    expect(icon("send").getAttribute("aria-hidden")).toBe("true");
    expect(icon("send").getAttribute("role")).toBeNull();
  });
  it("names a glyph that is the only sign of a state", () => {
    const svg = icon("spinner", "running");
    expect([svg.getAttribute("role"), svg.getAttribute("aria-label"), svg.getAttribute("aria-hidden")]).toEqual(["img", "running", null]);
  });
  it("carries no attribute the CSP or the inert check refuses", () => {
    for (const n of [icon("ban"), ...icon("ban").querySelectorAll("*")]) {
      for (const a of n.attributes) expect(a.name === "style" || a.name.startsWith("on"), a.name).toBe(false);
    }
  });
});
