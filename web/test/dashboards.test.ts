import { describe, expect, it } from "vitest";
import { expandDashboards, fetchDashboards } from "../src/dashboards";

const room = { label: "Room metrics", url: "https://grafana.example.com/d/agent-room/x?var-room={room}" };
const run = { label: "Run", url: "https://grafana.example.com/d/agent-run/x?var-run={run}" };
const task = { label: "Task", url: "https://grafana.example.com/d/agent-factory/x?var-task={task}" };

describe("dashboard links", () => {
  it("are none without templates", () => {
    expect(expandDashboards(undefined, { room: "3kq7x2ma" })).toEqual([]);
    expect(expandDashboards([], { room: "3kq7x2ma" })).toEqual([]);
  });
  it("fill {room} with the encoded room id", () => {
    expect(expandDashboards([room], { room: "3kq7x2ma" })).toEqual([
      { label: "Room metrics", url: "https://grafana.example.com/d/agent-room/x?var-room=3kq7x2ma" }]);
    expect(expandDashboards([room], { room: "a b&c=#/" })[0].url)
      .toBe("https://grafana.example.com/d/agent-room/x?var-room=a%20b%26c%3D%23%2F");
  });
  it("fill every occurrence of every placeholder", () => {
    const all = { label: "All", url: "https://grafana.example.com/d/{run}/x?r={room}&t={task}&r2={room}" };
    expect(expandDashboards([all], { room: "r1", run: "u1", task: "r1" })[0].url)
      .toBe("https://grafana.example.com/d/u1/x?r=r1&t=r1&r2=r1");
  });
  it("hide a link whose placeholder has no value", () => {
    expect(expandDashboards([room, run, task], { room: "3kq7x2ma" }).map((l) => l.label)).toEqual(["Room metrics"]);
    expect(expandDashboards([room, run, task], { room: "3kq7x2ma", run: "ikely2yk" }).map((l) => l.label)).toEqual(["Room metrics", "Run"]);
    expect(expandDashboards([run], { room: "3kq7x2ma", run: "" })).toEqual([]);
  });
  it("drop a result that is not https", () => {
    const bad = [{ label: "http", url: "http://grafana.example.com/?r={room}" },
      { label: "js", url: "javascript:alert('{room}')" }, { label: "relative", url: "/d/x?r={room}" }];
    expect(expandDashboards(bad, { room: "3kq7x2ma" })).toEqual([]);
  });
  it("drop a result whose host moved or that gained a username or password", () => {
    const glued = [{ label: "glued", url: "https://grafana.example.com{room}/x" }];
    expect(expandDashboards(glued, { room: "evil.example" })).toEqual([]);
    const path = [{ label: "path", url: "https://grafana.example.com/{room}" }];
    expect(expandDashboards(path, { room: "@evil.example" })[0].url).toBe("https://grafana.example.com/%40evil.example");
    const user = [{ label: "user", url: "https://{room}@grafana.example.com/" }];
    expect(expandDashboards(user, { room: "alice" })).toEqual([]);
  });
  it("are read from GET /api/roomctl, none when absent or unreadable", async () => {
    const answer = (status: number, body: unknown) => (async (path: string) => {
      expect(path).toBe("/api/roomctl");
      return new Response(JSON.stringify(body), { status });
    }) as unknown as typeof fetch;
    expect(await fetchDashboards(answer(200, { url: "u", dashboards: [room, { label: 3 }, run] }))).toEqual([room, run]);
    expect(await fetchDashboards(answer(200, { url: "u" }))).toEqual([]);
    expect(await fetchDashboards(answer(503, {}))).toEqual([]);
    expect(await fetchDashboards((async () => { throw new Error("offline"); }) as unknown as typeof fetch)).toEqual([]);
  });
});
