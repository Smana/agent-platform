// SPDX-License-Identifier: Apache-2.0

// The Maku lockup every page header shares: the supplied Orbit SVG mark plus the real
// lowercase wordmark. The two marks are both in the DOM and the `.dark` class (theme.ts)
// picks one in CSS, so an explicit theme choice and the OS theme select the same mark.

const MARK = 32; // the rendered size (the SVGs are 64×64); the phone breakpoint shrinks them in CSS only

function mark(cls: string, src: string): HTMLImageElement {
  const img = document.createElement("img");
  img.className = cls;
  img.src = src;
  img.alt = "";
  img.setAttribute("aria-hidden", "true");
  img.width = MARK;
  img.height = MARK;
  return img;
}

// mountBrand appends the home link to host and returns it. assetBase is where the
// built assets are served: /assets in production and preview, a relative path on the demo.
// homeHref is where the link goes: "./" on the demo, so GitHub Pages stays on the demo.
export function mountBrand(host: HTMLElement, assetBase = "/assets", homeHref = "/"): HTMLAnchorElement {
  const a = document.createElement("a");
  a.className = "brand";
  a.href = homeHref;
  a.setAttribute("aria-label", "Maku home");
  const word = document.createElement("span");
  word.className = "brand-wordmark";
  word.textContent = "maku";
  a.append(mark("brand-mark-light", `${assetBase}/svg/maku-mark-light.svg`), mark("brand-mark-dark", `${assetBase}/svg/maku-mark-dark.svg`), word);
  host.append(a);
  return a;
}
