// SPDX-License-Identifier: Apache-2.0

// The page's icons: one outline set, a 1.5 px stroke on a 16 px grid, built with
// createElementNS so the strict CSP needs no markup. Each draws in currentColor: its
// state (hover, disabled, a status) comes from CSS, never from a second asset.

const NS = "http://www.w3.org/2000/svg";

const paths = {
  check: ["M3.5 8.5l3 3 6-7"],
  x: ["M4.5 4.5l7 7M11.5 4.5l-7 7"],
  ban: ["M8 1.75a6.25 6.25 0 1 0 0 12.5a6.25 6.25 0 1 0 0-12.5z", "M3.6 3.6l8.8 8.8"],
  spinner: ["M8 1.75a6.25 6.25 0 1 0 6.25 6.25"],
  chevron: ["M6 3.5l4.5 4.5-4.5 4.5"],
  send: ["M2.5 8h10", "M8.5 4l4 4-4 4"],
} as const;

export type IconName = keyof typeof paths;

// icon is one glyph: decorative (hidden from assistive tech) unless it carries a label,
// for an icon that is the only sign of a state.
export function icon(name: IconName, label?: string): SVGSVGElement {
  const svg = document.createElementNS(NS, "svg");
  for (const [k, v] of Object.entries({
    viewBox: "0 0 16 16", width: "16", height: "16", fill: "none", stroke: "currentColor",
    "stroke-width": "1.5", "stroke-linecap": "round", "stroke-linejoin": "round", class: `icon icon-${name}`,
  })) svg.setAttribute(k, v);
  if (label) {
    svg.setAttribute("role", "img");
    svg.setAttribute("aria-label", label);
  } else {
    svg.setAttribute("aria-hidden", "true");
  }
  for (const d of paths[name]) {
    const p = document.createElementNS(NS, "path");
    p.setAttribute("d", d);
    svg.append(p);
  }
  return svg;
}
