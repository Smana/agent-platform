import { describe, expect, it } from "vitest";
import { mountReplayBar } from "../preview/replay-bar";

// fakeReplay records the bar's calls and exposes the state it reads.
function fakeReplay() {
  const calls: string[] = [];
  const r = {
    playing: false, done: false, speed: 8, applied: 0,
    play() { calls.push("play"); r.playing = true; },
    pause() { calls.push("pause"); r.playing = false; },
    setSpeed(n: number) { calls.push(`speed ${n}`); r.speed = n; },
    skip() { calls.push("skip"); r.applied = 90; r.done = true; r.playing = false; },
  };
  return { r, calls };
}

function setup() {
  const root = document.createElement("div");
  const { r, calls } = fakeReplay();
  const bar = mountReplayBar(root, r, 90);
  const q = <T extends Element>(s: string) => root.querySelector<T>(s)!;
  return { root, r, calls, bar, toggle: () => q<HTMLButtonElement>("button[data-act=toggle]"),
    skip: () => q<HTMLButtonElement>("button[data-act=skip]"), speed: () => q<HTMLSelectElement>("select"), progress: () => q(".replay-progress") };
}

describe("mountReplayBar", () => {
  it("is a named group with play, speed, skip and progress", () => {
    const t = setup();
    const group = t.root.querySelector(".replay-bar")!;
    expect([group.getAttribute("role"), group.getAttribute("aria-label")]).toEqual(["group", "Replay controls"]);
    expect(t.toggle().textContent).toBe("Play");
    expect([...t.speed().options].map((o) => [o.value, o.textContent])).toEqual([["1", "1×"], ["8", "8×"], ["32", "32×"]]);
    expect(t.speed().value).toBe("8");
    expect(t.speed().getAttribute("aria-label")).toBe("Replay speed");
    expect(t.skip().textContent).toBe("Skip to the end");
    expect(t.progress().textContent).toBe("0 / 90 events");
  });
  it("toggles play and pause, and names the button after the next act", () => {
    const t = setup();
    t.toggle().click();
    expect(t.calls).toEqual(["play"]);
    expect(t.toggle().textContent).toBe("Pause");
    t.toggle().click();
    expect(t.calls).toEqual(["play", "pause"]);
    expect(t.toggle().textContent).toBe("Play");
  });
  it("changes the speed from the select", () => {
    const t = setup();
    t.speed().value = "32";
    t.speed().dispatchEvent(new Event("change"));
    expect(t.calls).toEqual(["speed 32"]);
  });
  it("skips to the end, then disables play and skip and shows the full count", () => {
    const t = setup();
    t.skip().click();
    expect(t.calls).toEqual(["skip"]);
    expect([t.toggle().disabled, t.skip().disabled]).toEqual([true, true]);
    expect(t.progress().textContent).toBe("90 / 90 events");
  });
  it("reflects the replay's state on refresh", () => {
    const t = setup();
    t.r.applied = 12;
    t.r.playing = true;
    t.bar.refresh();
    expect([t.progress().textContent, t.toggle().textContent]).toEqual(["12 / 90 events", "Pause"]);
  });
});
