// SPDX-License-Identifier: Apache-2.0

// The demo's playback controls: play/pause, speed, skip to the end, and how far the
// replay has got. Each control drives the replay, then refresh() redraws the bar from
// the replay's state, so the bar never holds state of its own.

import type { Replay } from "./replay";

type Playable = Pick<Replay, "play" | "pause" | "setSpeed" | "skip" | "playing" | "done" | "speed" | "applied">;

const SPEEDS = [1, 8, 32];

export function mountReplayBar(root: HTMLElement, r: Playable, total: number): { refresh(): void } {
  const bar = document.createElement("div");
  bar.className = "replay-bar";
  bar.setAttribute("role", "group");
  bar.setAttribute("aria-label", "Replay controls");

  const toggle = document.createElement("button");
  toggle.type = "button";
  toggle.dataset.act = "toggle";
  const speed = document.createElement("select");
  speed.setAttribute("aria-label", "Replay speed");
  for (const n of SPEEDS) speed.append(Object.assign(document.createElement("option"), { value: String(n), textContent: `${n}×` }));
  const skip = document.createElement("button");
  skip.type = "button";
  skip.dataset.act = "skip";
  skip.textContent = "Skip to the end";
  const progress = document.createElement("span");
  progress.className = "replay-progress";
  progress.setAttribute("role", "status");
  progress.setAttribute("aria-live", "off");

  bar.append(toggle, speed, skip, progress);
  root.append(bar);

  const refresh = () => {
    toggle.textContent = r.playing ? "Pause" : "Play";
    toggle.disabled = r.done;
    skip.disabled = r.done;
    speed.value = String(r.speed);
    progress.textContent = `${r.applied} / ${total} events`;
  };
  toggle.addEventListener("click", () => { if (r.playing) r.pause(); else r.play(); refresh(); });
  speed.addEventListener("change", () => { r.setSpeed(Number(speed.value)); refresh(); });
  skip.addEventListener("click", () => { r.skip(); refresh(); });
  refresh();
  return { refresh };
}
