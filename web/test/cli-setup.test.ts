import { afterEach, describe, expect, it, vi } from "vitest";
import { cliSetup } from "../src/view";

// The room list's CLI setup fold: numbered steps, each command in its own block with a
// Copy button that copies exactly what the block shows.
const oidc = { url: "https://rooms.priv.example", issuer: "https://auth.example", clientID: "3434@agents", projectID: "29184", version: "v0.7.0" };
const install = [
  `os=$(uname -s | tr A-Z a-z); arch=$(uname -m); case $arch in x86_64) arch=amd64;; aarch64) arch=arm64;; esac`,
  `base=https://github.com/Smana/maku/releases/download/v0.7.0`,
  `curl -fsSLO "$base/roomctl-$os-$arch" && curl -fsSLO "$base/roomctl.sha256"`,
  `command -v sha256sum >/dev/null && sum=sha256sum || sum="shasum -a 256"`,
  `grep " roomctl-$os-$arch$" roomctl.sha256 | $sum -c - && sudo install -m 0755 "roomctl-$os-$arch" /usr/local/bin/roomctl`,
].join("\n");

async function open(body: object) {
  const get = vi.fn(() => Promise.resolve(new Response(JSON.stringify(body))));
  const d = cliSetup(get as unknown as typeof fetch);
  d.open = true;
  d.dispatchEvent(new Event("toggle"));
  await vi.waitFor(() => expect(d.querySelector(".cli-card")?.childElementCount).toBeGreaterThan(0));
  const steps = [...d.querySelectorAll<HTMLElement>(".cli-step")];
  return {
    d,
    steps,
    titles: steps.map((s) => s.querySelector(".cli-step-title")?.textContent),
    code: steps.map((s) => s.querySelector("pre")?.textContent),
    notes: steps.map((s) => [...s.querySelectorAll(".cli-note")].map((n) => n.textContent)),
  };
}

const stubClipboard = (writeText?: (t: string) => Promise<void>) =>
  Object.defineProperty(navigator, "clipboard", { value: writeText ? { writeText } : undefined, configurable: true });

afterEach(() => { vi.useRealTimers(); stubClipboard(undefined); });

describe("the CLI setup fold", () => {
  it("says what it is for in its summary", () => {
    expect(cliSetup().querySelector("summary")?.textContent).toBe("Use roomctl from your terminal");
  });

  it("reads as three numbered steps on an OIDC broker: install, point it at the broker, sign in", async () => {
    const { d, titles, code, notes } = await open(oidc);
    expect(d.querySelector("ol.cli-steps")).not.toBeNull();
    expect(titles).toEqual(["Install", "Point it at this broker", "Sign in"]);
    expect(code).toEqual([
      install,
      "roomctl configure \\\n  --url https://rooms.priv.example \\\n  --issuer https://auth.example \\\n  --client-id 3434@agents \\\n  --project-id 29184",
      "roomctl login",
    ]);
    expect(notes[0]).toEqual(["linux and macOS · amd64 and arm64 · checked against the release's sha256"]);
    expect(notes[2]).toEqual(["Then: roomctl status · roomctl mcp for your agent's tools"]);
    expect([...d.querySelectorAll(".cli-step")[2].querySelectorAll(".cli-note code")].map((c) => c.textContent))
      .toEqual(["roomctl status", "roomctl mcp"]);
  });

  it("splits configure into one flag a line, each continued with a backslash", async () => {
    const { code } = await open(oidc);
    const lines = code[1]!.split("\n");
    expect(lines).toHaveLength(5);
    for (const l of lines.slice(0, -1)) expect(l.endsWith(" \\")).toBe(true);
    expect(lines.slice(1).every((l) => l.startsWith("  --"))).toBe(true);
  });

  it("keeps a local broker's own steps: install, configure the URL, start a run", async () => {
    const { titles, code } = await open({ url: "http://localhost:8080", humanMode: "local", clientID: "", version: "v0.7.0" });
    expect(titles).toEqual(["Install", "Point it at this broker", "Start a run"]);
    expect(code).toEqual([
      install,
      "roomctl configure --url http://localhost:8080",
      "roomctl run \\\n  --repo <owner/name> \\\n  --task '<what the run should do>'",
    ]);
  });

  it("copies exactly each step's displayed command, continuations included", async () => {
    const writeText = vi.fn((_: string) => Promise.resolve());
    stubClipboard(writeText);
    const { steps } = await open(oidc);
    for (const s of steps) s.querySelector<HTMLButtonElement>("button.cli-copy")!.click();
    expect(writeText.mock.calls.map((c) => c[0])).toEqual(steps.map((s) => s.querySelector("pre")!.textContent));
  });

  it("enters every broker value as text, never as markup", async () => {
    const { d, code } = await open({ ...oidc, url: `<img src=x onerror="alert(1)">` });
    expect(d.querySelector("img")).toBeNull();
    expect(code[1]).toContain(`--url <img src=x onerror="alert(1)"> \\`);
  });

  it("keeps today's message, in the card, when this broker has no roomctl client", async () => {
    const { d, steps } = await open({ url: "https://rooms.priv.example", issuer: "https://auth.example", clientID: "" });
    expect(steps).toHaveLength(0);
    expect(d.querySelector(".cli-card")?.textContent).toBe("roomctl is not set up on this broker: it has no roomctl client.");
    expect(d.querySelector("button")).toBeNull();
  });
});

describe("a step's Copy button", () => {
  it("writes the exact text, says Copied for 1.5 s and announces it, then reverts", async () => {
    const writeText = vi.fn((_: string) => Promise.resolve());
    stubClipboard(writeText);
    const { d, steps } = await open(oidc);
    vi.useFakeTimers();
    const b = steps[1].querySelector<HTMLButtonElement>("button.cli-copy")!;
    const live = d.querySelector<HTMLElement>(".cli-live")!;
    expect(live.getAttribute("aria-live")).toBe("polite");
    expect(b.textContent).toBe("Copy");
    b.click();
    expect(writeText).toHaveBeenCalledWith(steps[1].querySelector("pre")!.textContent);
    await vi.advanceTimersByTimeAsync(0);
    expect(b.textContent).toBe("Copied ✓");
    expect(live.textContent).toBe("Copied: Point it at this broker");
    await vi.advanceTimersByTimeAsync(1499);
    expect(b.textContent).toBe("Copied ✓");
    await vi.advanceTimersByTimeAsync(1);
    expect(b.textContent).toBe("Copy");
    expect(live.textContent).toBe("");
  });

  it("restarts its 1.5 s on a second click rather than reverting early", async () => {
    stubClipboard(() => Promise.resolve());
    const { steps } = await open(oidc);
    vi.useFakeTimers();
    const b = steps[0].querySelector<HTMLButtonElement>("button.cli-copy")!;
    b.click();
    await vi.advanceTimersByTimeAsync(1000);
    b.click();
    await vi.advanceTimersByTimeAsync(1000);
    expect(b.textContent).toBe("Copied ✓");
    await vi.advanceTimersByTimeAsync(500);
    expect(b.textContent).toBe("Copy");
  });

  it("falls back silently with no clipboard or a refused write", async () => {
    const { steps } = await open(oidc);
    vi.useFakeTimers();
    const b = steps[2].querySelector<HTMLButtonElement>("button.cli-copy")!;
    b.click(); // no navigator.clipboard (an insecure origin)
    await vi.advanceTimersByTimeAsync(0);
    expect(b.textContent).toBe("Copy");
    stubClipboard(() => Promise.reject(new Error("denied")));
    b.click();
    await vi.advanceTimersByTimeAsync(0);
    expect(b.textContent).toBe("Copy");
  });
});
