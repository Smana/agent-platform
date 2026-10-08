// SPDX-License-Identifier: Apache-2.0

// The platform's theme, ported from the app-wizard (ui/src/lib/theme.ts) and
// kept in lockstep with it: the same three modes, the same storage key, and the
// same contract — a `dark` class on <html> that app.css's tokens key off. The
// palettes live in app.css; this module only decides which one applies.
//
// Three modes rather than a boolean: "system" is a real, sticky choice ("keep
// following my OS"), not merely the absence of one.

export type ThemeMode = "light" | "dark" | "system";

export const THEME_STORAGE_KEY = "app-wizard:theme";

const DARK_QUERY = "(prefers-color-scheme: dark)";

function isThemeMode(value: unknown): value is ThemeMode {
  return value === "light" || value === "dark" || value === "system";
}

// The user's stored choice, or "system" when unset or corrupt. A throwing
// Storage (Safari private mode) answers "system": following the OS is a
// perfectly good answer; failing to render is not.
export function readStoredMode(store: Storage = localStorage): ThemeMode {
  try {
    const stored = store.getItem(THEME_STORAGE_KEY);
    return isThemeMode(stored) ? stored : "system";
  } catch {
    return "system";
  }
}

export function storeMode(mode: ThemeMode, store: Storage = localStorage): void {
  try {
    store.setItem(THEME_STORAGE_KEY, mode);
  } catch {
    // Non-fatal: the theme still applies for this session, it just won't persist.
  }
}

export function systemPrefersDark(): boolean {
  return typeof matchMedia === "function" && matchMedia(DARK_QUERY).matches;
}

// applyTheme toggles the `dark` class on <html>, which is what the CSS keys off.
export function applyTheme(mode: ThemeMode): void {
  document.documentElement.classList.toggle("dark", mode === "dark" || (mode === "system" && systemPrefersDark()));
}

export function nextMode(mode: ThemeMode): ThemeMode {
  if (mode === "light") return "dark";
  if (mode === "dark") return "system";
  return "light";
}

export interface ThemeController {
  readonly mode: ThemeMode;
  cycle(): ThemeMode;
}

// initTheme applies the stored mode before the page renders and, while the mode
// is "system", keeps following the OS. An explicit light/dark choice survives
// the user switching their OS theme.
export function initTheme(store: Storage = localStorage): ThemeController {
  let mode = readStoredMode(store);
  applyTheme(mode);
  if (typeof matchMedia === "function") {
    matchMedia(DARK_QUERY).addEventListener("change", () => {
      if (mode === "system") applyTheme(mode);
    });
  }
  return {
    get mode() { return mode; },
    cycle() {
      mode = nextMode(mode);
      storeMode(mode, store);
      applyTheme(mode);
      return mode;
    },
  };
}

// mountThemeButton adds the wizard's mode cycle to a header bar.
export function mountThemeButton(bar: HTMLElement, ctl: ThemeController): HTMLButtonElement {
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "theme-toggle";
  const show = () => { btn.textContent = `theme: ${ctl.mode}`; btn.title = "Light, dark, or follow the OS"; };
  btn.onclick = () => { ctl.cycle(); show(); };
  show();
  bar.append(btn);
  return btn;
}
