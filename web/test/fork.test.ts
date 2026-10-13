import { afterEach, describe, expect, it, vi } from "vitest";
import { nav } from "../src/api";
import { ChatView } from "../src/chat";
import type { RoomEvent } from "../src/conn";
import { mountFork } from "../src/fork";
import { RoomState } from "../src/room-state";
import { cliSetup } from "../src/view";

function setup() {
  const sent: Record<string, any>[] = [];
  const conn = { send: (f: Record<string, unknown>) => { sent.push(f); return true; } };
  const root = document.createElement("section");
  const fork = mountFork(root, conn, new RoomState("human:a", 4));
  const field = <T extends HTMLElement>(name: string) => root.querySelector<T>(`[name="${name}"]`)!;
  const submit = () => root.querySelector("form")!.dispatchEvent(new Event("submit", { cancelable: true }));
  return { sent, root, fork, field, submit };
}

afterEach(() => vi.restoreAllMocks());

describe("mountFork", () => {
  it("is hidden until a row asks to fork at its seq", () => {
    const f = setup();
    expect(f.root.hidden).toBe(true);
    f.fork.open(12);
    expect(f.root.hidden).toBe(false);
    expect(f.root.textContent).toContain("#12");
  });
  it("sends the seq, the note, and a run's role and egress profiles", () => {
    const f = setup();
    f.fork.open(12);
    f.field<HTMLInputElement>("note").value = "  try uv  ";
    f.field<HTMLSelectElement>("forkRole").value = "implementer";
    f.field<HTMLInputElement>("forkEgress").value = "pypi, npm,";
    f.field<HTMLInputElement>("forkPrUrl").value = "https://github.com/x/y/pull/1"; // reviewer only: dropped
    f.submit();
    expect(f.sent).toHaveLength(1);
    expect(f.sent[0]).toMatchObject({ type: "act", driverEpoch: 4,
      action: { kind: "fork", seq: 12, note: "try uv", role: "implementer", egressProfiles: ["pypi", "npm"] } });
    expect(f.sent[0].action.prUrl).toBeUndefined();
  });
  it("without a role asks for a room only, and opens it", () => {
    const go = vi.spyOn(nav, "go").mockImplementation(() => {});
    const f = setup();
    f.fork.open(3);
    f.submit();
    expect(f.sent[0].action).toEqual({ kind: "fork", seq: 3 });
    f.fork.onAck({ type: "ack", clientSeq: f.sent[0].clientSeq, seq: 4, result: { roomId: "abcdefgh" } });
    expect(go).toHaveBeenCalledWith("/r/abcdefgh");
  });
  it("shows a run's claim and links the fork instead of leaving the page", () => {
    const go = vi.spyOn(nav, "go").mockImplementation(() => {});
    const f = setup();
    f.fork.open(3);
    f.field<HTMLSelectElement>("forkRole").value = "implementer";
    f.submit();
    f.fork.onAck({ type: "ack", clientSeq: f.sent[0].clientSeq, seq: 4,
      result: { roomId: "abcdefgh", run: { kind: "AgentRun", spec: { roomRef: "abcdefgh" } } } });
    expect(go).not.toHaveBeenCalled();
    expect(f.root.querySelector<HTMLAnchorElement>("a")!.getAttribute("href")).toBe("/r/abcdefgh");
    expect(f.root.querySelector("pre")!.textContent).toContain('"roomRef": "abcdefgh"');
  });
  it("says why the fork's run failed, beside the fork it made", () => {
    const f = setup();
    f.fork.open(3);
    f.field<HTMLSelectElement>("forkRole").value = "tester";
    f.submit();
    f.fork.onAck({ type: "ack", clientSeq: f.sent[0].clientSeq, seq: 4, result: { roomId: "abcdefgh", run: null, runError: "over_budget" } });
    expect(f.root.textContent).toContain("over your budget");
    expect(f.root.querySelector<HTMLAnchorElement>("a")!.getAttribute("href")).toBe("/r/abcdefgh");
  });
  it("ignores other acts' acks, and a rejected fork leaves the form as it was", () => {
    const go = vi.spyOn(nav, "go").mockImplementation(() => {});
    const f = setup();
    f.fork.open(3);
    f.field<HTMLInputElement>("note").value = "keep me";
    f.submit();
    f.fork.onAck({ type: "ack", clientSeq: 999, seq: 4, result: { roomId: "abcdefgh" } });
    f.fork.onAck({ type: "ack", clientSeq: f.sent[0].clientSeq, rejected: "bad_action" });
    expect(go).not.toHaveBeenCalled();
    expect(f.root.hidden).toBe(false);
    expect(f.field<HTMLInputElement>("note").value).toBe("keep me");
  });
  it("never trusts a room id it did not expect", () => {
    const go = vi.spyOn(nav, "go").mockImplementation(() => {});
    const f = setup();
    f.fork.open(3);
    f.submit();
    f.fork.onAck({ type: "ack", clientSeq: f.sent[0].clientSeq, seq: 4, result: { roomId: "../oauth2/sign_out" } });
    expect(go).not.toHaveBeenCalled();
  });
});

describe("ChatView fork buttons", () => {
  const ev = (seq: number, type: string, payload: unknown): RoomEvent => ({ v: 1, id: `id${seq}`, seq, roomId: "3kq7x2ma",
    actor: { kind: "agent", id: "agent:ikely2yk" }, type, origin: "harness", ts: "2026-10-04T14:00:00Z", redactions: [], payload });
  it("offers fork here on every row, at the last seq the row shows", () => {
    const el = document.createElement("div");
    const asked: number[] = [];
    const v = new ChatView(el, 100, (seq) => asked.push(seq));
    v.apply(ev(1, "message", { kind: "chat", text: "hi", delivery: "none" }));
    v.apply(ev(2, "tool_call", { callId: "c1", tool: "terminal", args: { command: "ls" } }));
    v.apply(ev(3, "state_changed", { kind: "run_phase", phase: "Running" }));
    v.apply(ev(4, "tool_result", { callId: "c1", status: "ok", output: "x" }));
    const buttons = [...el.querySelectorAll<HTMLButtonElement>('button[data-act="fork"]')];
    expect(buttons).toHaveLength(3);
    for (const b of buttons) b.click();
    expect(asked).toEqual([1, 4, 3]);
  });
  it("has none without a fork handler", () => {
    const el = document.createElement("div");
    new ChatView(el).apply(ev(1, "message", { kind: "chat", text: "hi", delivery: "none" }));
    expect(el.querySelector("button")).toBeNull();
  });
});

describe("cliSetup", () => {
  const values = { url: "https://rooms.priv.example", issuer: "https://auth.example", clientID: "3434@agents", projectID: "29184" };
  const open = async (body: object) => {
    const get = vi.fn(() => Promise.resolve(new Response(JSON.stringify(body))));
    const d = cliSetup(get as unknown as typeof fetch);
    d.open = true;
    d.dispatchEvent(new Event("toggle"));
    await vi.waitFor(() => expect(d.querySelector("pre")?.textContent).not.toBe(""));
    return { get, text: d.querySelector("pre")!.textContent! };
  };
  it("installs the broker's roomctl release, checksum verified, then configures and logs in (#48)", async () => {
    const { get, text } = await open({ ...values, version: "v0.7.0" });
    expect(text).toBe([
      `os=$(uname -s | tr A-Z a-z); arch=$(uname -m); case $arch in x86_64) arch=amd64;; aarch64) arch=arm64;; esac`,
      `base=https://github.com/Smana/agent-platform/releases/download/v0.7.0`,
      `curl -fsSLO "$base/roomctl-$os-$arch" && curl -fsSLO "$base/roomctl.sha256"`,
      `command -v sha256sum >/dev/null && sum=sha256sum || sum="shasum -a 256"`,
      `grep " roomctl-$os-$arch$" roomctl.sha256 | $sum -c - && sudo install -m 0755 "roomctl-$os-$arch" /usr/local/bin/roomctl`,
      "roomctl configure --url https://rooms.priv.example --issuer https://auth.example --client-id 3434@agents --project-id 29184",
      "roomctl login",
    ].join("\n"));
    expect(get).toHaveBeenCalledWith("/api/roomctl", expect.anything());
  });
  it("installs the latest release when the broker is not a release build", async () => {
    for (const version of ["dev", undefined]) {
      const { text } = await open({ ...values, version });
      expect(text).toContain("base=https://github.com/Smana/agent-platform/releases/latest/download\n");
    }
  });
  it("says so when this broker has no roomctl client", async () => {
    const { text } = await open({ url: "https://rooms.priv.example", issuer: "https://auth.example", clientID: "" });
    expect(text).toContain("not set up");
  });
});
