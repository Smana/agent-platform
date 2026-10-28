// SPDX-License-Identifier: Apache-2.0

// The offline room list preview: listRooms as main.ts mounts it, fed six fixture rooms
// instead of the broker. `npm run preview`, then open /preview/list.html (?theme=dark for
// the dark palette; ?cli=oidc, ?cli=local or ?cli=none opens the CLI setup fold on that
// broker's answer). Nothing leaves the page: the fetch below answers every call itself.

import { applyTheme, initTheme } from "../src/theme";
import { listRooms, type RoomRow } from "../src/view";

const ago = (min: number) => new Date(Date.now() - min * 60_000).toISOString();
const repo = "Smana/ai-native-ref";
const base = { owner: "system:factory", driver: "system:factory", dataClass: "internal", repository: repo };
const rows: RoomRow[] = [
  { ...base, id: "k2p9rt4v", phase: "Active", lastSeq: 318, issue: 152, title: "docs: a getting-started page for the platform", task: "Escalated", needsYou: 1, lastEventAt: ago(4) },
  { ...base, id: "3kq7x2ma", phase: "Active", lastSeq: 142, issue: 147, title: "feat: retry the bridge upload on a 503", task: "Reviewing", pr: 148, lastEventAt: ago(18) },
  { ...base, id: "d66mo4b4", phase: "Active", lastSeq: 95, issue: 144, title: "fix: the meter double-counts a resumed run", task: "Implementing", lastEventAt: ago(52) },
  { ...base, id: "yacoriey", phase: "Idle", lastSeq: 70, issue: 139, title: "chore: bump the chart's CRD copy", task: "Merged", pr: 141, lastEventAt: ago(60 * 5) },
  { ...base, id: "m4tq2zxa", phase: "Idle", lastSeq: 33, issue: 131, title: "docs: name the trace router's allowlist", task: "Closed", lastEventAt: ago(60 * 26) },
  { ...base, id: "p7wn5c3e", phase: "Idle", owner: "human:2", driver: "human:2", lastSeq: 12, lastEventAt: ago(60 * 24 * 3) },
];

const params = new URLSearchParams(location.search);
const cli = params.get("cli");
const maku: Record<string, object> = {
  oidc: { url: "https://rooms.priv.cloud.ogenki.io", issuer: "https://auth.cloud.ogenki.io", clientID: "293847561029384756@agents", projectID: "293847560192837465", version: "v0.9.0" },
  local: { url: "http://localhost:8080", humanMode: "local", version: "v0.9.0" },
  none: { url: "https://rooms.priv.cloud.ogenki.io", issuer: "https://auth.cloud.ogenki.io", clientID: "" },
};

const get = (async (url: string) => {
  if (url.startsWith("/api/roomctl")) {
    return cli && maku[cli] ? new Response(JSON.stringify(maku[cli])) : new Response(JSON.stringify({}), { status: 404 });
  }
  const q = new URL(url, location.origin).searchParams;
  return new Response(JSON.stringify(rows.filter((r) => (!q.has("needs_me") || (r.needsYou ?? 0) > 0) && (!q.get("repo") || r.repository === q.get("repo")))));
}) as unknown as typeof fetch;

const theme = initTheme();
const override = params.get("theme");
if (override === "light" || override === "dark") applyTheme(override);
listRooms(document.getElementById("app")!, get, {}, theme)
  .then(() => {
    const fold = document.querySelector<HTMLDetailsElement>("details.cli-setup");
    if (cli && fold) fold.open = true;
  })
  .catch((e) => { document.body.textContent = `preview failed: ${e instanceof Error ? e.message : e}`; });
