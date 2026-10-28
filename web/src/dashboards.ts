// SPDX-License-Identifier: Apache-2.0

// Dashboard links (human.dashboards): templates the broker serves on GET /api/roomctl, filled
// here with the room's own ids. Each id is URL-encoded; a link whose placeholder has no value
// is hidden, and one that does not come out https is dropped.

import { api } from "./api";

export interface Dashboard { label: string; url: string }

// The room's ids a template may name: the room, its current or last run, its task.
export interface DashboardIds { room: string; run?: string; task?: string }

const placeholder = /\{(room|run|task)\}/g;

// fill puts each placeholder's value in, encoded; undefined when one has none.
function fill(tmpl: string, value: (k: keyof DashboardIds) => string | undefined): string | undefined {
  let missing = false;
  const url = tmpl.replace(placeholder, (_, k: keyof DashboardIds) => {
    const v = value(k);
    if (!v) missing = true;
    return encodeURIComponent(v ?? "");
  });
  return missing ? undefined : url;
}

// A filled link must stay https, on the template's own host (as filled with sample values),
// with no username or password: no id can choose where it goes.
export function expandDashboards(templates: Dashboard[] | undefined, ids: DashboardIds): Dashboard[] {
  const out: Dashboard[] = [];
  for (const t of templates ?? []) {
    const url = fill(t.url, (k) => ids[k]);
    if (url === undefined) continue;
    try {
      const u = new URL(url);
      const host = new URL(fill(t.url, () => "x")!).host;
      if (u.protocol === "https:" && u.host === host && !u.username && !u.password) out.push({ label: t.label, url });
    } catch { /* not a URL: dropped */ }
  }
  return out;
}

// fetchDashboards reads the templates once; none when the broker has none or cannot answer,
// since the links are a convenience the page works without.
export async function fetchDashboards(get: typeof fetch = fetch): Promise<Dashboard[]> {
  try {
    const r = await api("/api/roomctl", {}, get);
    if (!r.ok) return [];
    const body = (await r.json()) as { dashboards?: unknown };
    if (!Array.isArray(body.dashboards)) return [];
    return body.dashboards.filter((d): d is Dashboard =>
      typeof d?.label === "string" && typeof d?.url === "string").map((d) => ({ label: d.label, url: d.url }));
  } catch {
    return [];
  }
}
