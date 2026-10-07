import { afterEach, describe, expect, it, vi } from "vitest";
import { applyTheme, initTheme, mountThemeButton, nextMode, readStoredMode, THEME_STORAGE_KEY } from "../src/theme";

function store(init: Record<string, string> = {}) {
  const data = new Map(Object.entries(init));
  return {
    getItem: (k: string) => data.get(k) ?? null,
    setItem: (k: string, v: string) => void data.set(k, v),
    data,
  } as unknown as Storage & { data: Map<string, string> };
}

function stubMedia(dark: boolean) {
  vi.stubGlobal("matchMedia", (q: string) => ({
    matches: dark && q === "(prefers-color-scheme: dark)",
    addEventListener: () => {},
    removeEventListener: () => {},
  }));
}

afterEach(() => {
  vi.unstubAllGlobals();
  document.documentElement.classList.remove("dark");
});

describe("theme modes", () => {
  it("cycles light → dark → system → light", () => {
    expect(nextMode("light")).toBe("dark");
    expect(nextMode("dark")).toBe("system");
    expect(nextMode("system")).toBe("light");
  });
  it("defaults to system when storage is empty or corrupt", () => {
    expect(readStoredMode(store())).toBe("system");
    expect(readStoredMode(store({ [THEME_STORAGE_KEY]: "mosaic" }))).toBe("system");
    expect(readStoredMode(store({ [THEME_STORAGE_KEY]: "dark" }))).toBe("dark");
  });
  it("survives a throwing storage", () => {
    const broken = { getItem: () => { throw new Error("private mode"); }, setItem: () => { throw new Error("private mode"); } } as unknown as Storage;
    expect(readStoredMode(broken)).toBe("system");
    expect(() => initTheme(broken).cycle()).not.toThrow();
  });
});

describe("applyTheme", () => {
  it("toggles the dark class on <html>", () => {
    applyTheme("dark");
    expect(document.documentElement.classList.contains("dark")).toBe(true);
    applyTheme("light");
    expect(document.documentElement.classList.contains("dark")).toBe(false);
  });
  it("follows the OS in system mode", () => {
    stubMedia(true);
    applyTheme("system");
    expect(document.documentElement.classList.contains("dark")).toBe(true);
    stubMedia(false);
    applyTheme("system");
    expect(document.documentElement.classList.contains("dark")).toBe(false);
  });
  it("treats a missing matchMedia as light", () => {
    applyTheme("system"); // jsdom has no matchMedia
    expect(document.documentElement.classList.contains("dark")).toBe(false);
  });
});

describe("initTheme and the toggle", () => {
  it("applies the stored mode and persists the cycle", () => {
    const s = store({ [THEME_STORAGE_KEY]: "dark" });
    const ctl = initTheme(s);
    expect(document.documentElement.classList.contains("dark")).toBe(true);
    expect(ctl.mode).toBe("dark");
    ctl.cycle(); // dark → system; jsdom has no matchMedia, so light
    expect(s.data.get(THEME_STORAGE_KEY)).toBe("system");
    expect(document.documentElement.classList.contains("dark")).toBe(false);
  });
  it("the button shows and cycles the mode", () => {
    const bar = document.createElement("header");
    const btn = mountThemeButton(bar, initTheme(store({ [THEME_STORAGE_KEY]: "light" })));
    expect(btn.textContent).toBe("theme: light");
    btn.click();
    expect(btn.textContent).toBe("theme: dark");
    expect(document.documentElement.classList.contains("dark")).toBe(true);
  });
});
