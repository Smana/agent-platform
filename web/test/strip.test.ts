import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { fmtTokens, mountStrip, shortPrincipal, type StripInput } from "../src/strip";
import type { Summary } from "../src/summary";

const summary = (over: Partial<Summary["status"]> = {}): Summary => ({
  apiVersion: "summary/v1", room: "3kq7x2ma", url: "https://rooms.example/r/3kq7x2ma",
  status: { phase: "Implementing", run: { id: "ikely2yk", role: "implementer", startedAt: "2026-10-04T14:00:02Z" },
    budget: { usedTokens: 189093, limitTokens: 1500000 }, pr: { number: 77, url: "https://github.com/o/r/pull/77" },
    issue: { number: 74, url: "https://github.com/o/r/issues/74", title: "Chat view: pair tool calls" }, lastVerdict: null,
    reason: "the policy needs a maintainer's approval", next: "an approver decides", ...over },
  needsYou: [], actions: [], notes: { untrusted: true, items: [] }, cursor: "seq:27",
});
const snap = { roomId: "3kq7x2ma", phase: "Active", driver: "human:291847362183", driverEpoch: 3, dataClass: "internal",
  you: { principal: "human:291847362183", role: "owner", approver: true, driver: true, webUI: true } };
const mount = () => { const h = document.createElement("header"); return { h, strip: mountStrip(h) }; };
const input = (over: Partial<StripInput> = {}): StripInput =>
  ({ roomId: "3kq7x2ma", snap, phase: "Active", driver: snap.driver, summary: summary(), needs: 0, connection: "live", ...over });

describe("the title strip", () => {
  it("prints the Now line with a relative time and links to the row", () => {
    const { h, strip } = mount();
    let clicked = 0;
    strip.onNowClick((seq) => { clicked = seq; });
    strip.update({ roomId: "3kq7x2ma", snap, phase: "Active", driver: snap.driver, needs: 0, connection: "live",
      now: { seq: 27, text: "terminal git log --oneline -3", status: "running", ts: new Date(Date.now() - 12_000).toISOString() } });
    expect(h.querySelector(".now")?.textContent).toBe("Now ▸ terminal git log --oneline -3 · running · 12 s ago");
    (h.querySelector(".now button") as HTMLButtonElement).click();
    expect(clicked).toBe(27);
  });
  it("says there is no run when nothing is running", () => {
    const { h, strip } = mount();
    strip.update({ roomId: "3kq7x2ma", snap, phase: "Open", driver: snap.driver, needs: 0, connection: "live" });
    expect(h.querySelector(".now")?.textContent).toBe("No run yet");
  });
  it("hints at Run only to an owner or the driver", () => {
    const { h, strip } = mount();
    strip.update(input({ summary: undefined }));
    expect(h.querySelector(".now-hint")?.textContent).toBe("Start one under Run");
    strip.update(input({ summary: undefined, snap: { ...snap, you: { ...snap.you, role: "collaborator", driver: false } } }));
    expect(h.querySelector(".now-hint")).toBeNull();
  });
  it("re-renders the Now line's age on the same input as the clock moves", () => {
    let t = Date.parse("2026-10-04T14:00:00Z");
    const h = document.createElement("header");
    const strip = mountStrip(h, () => t);
    const i = input({ now: { seq: 3, text: "hi", status: "message", ts: "2026-10-04T13:59:58Z" } });
    strip.update(i);
    expect(h.querySelector(".now")?.textContent).toBe("Now ▸ hi · message · 2 s ago");
    t += 4 * 60_000;
    strip.update(i);
    expect(h.querySelector(".now")?.textContent).toBe("Now ▸ hi · message · 4 min ago");
    t += 2 * 3600_000;
    strip.update(i);
    expect(h.querySelector(".now")?.textContent).toBe("Now ▸ hi · message · 2 h ago");
  });
  // The 1 s refresh must not rebuild the strip: a focused PR link or Now button keeps its node.
  it("moves both ages in place, keeping every node", () => {
    let t = Date.parse("2026-10-04T14:04:30Z");
    const h = document.createElement("header");
    const strip = mountStrip(h, () => t);
    const i = input({ now: { seq: 3, text: "hi", status: "running", ts: "2026-10-04T14:04:28Z" } });
    strip.update(i);
    const [h1, link, btn] = [h.querySelector("h1"), h.querySelector("a"), h.querySelector(".now-link")];
    t += 60_000;
    strip.update(i);
    expect(h.querySelector(".chip-run")?.textContent).toBe("implementer · ikely2yk · 5 min");
    expect(h.querySelector(".now")?.textContent).toBe("Now ▸ hi · running · 1 min ago");
    expect(h.querySelector("h1")).toBe(h1);
    expect(h.querySelector("a")).toBe(link);
    expect(h.querySelector(".now-link")).toBe(btn);
  });
  it("puts untrusted Now text in as text", () => {
    const { h, strip } = mount();
    strip.update(input({ now: { seq: 1, text: "<img src=x onerror=alert(1)>", status: "message", ts: new Date().toISOString() } }));
    expect(h.querySelector("img")).toBeNull();
    expect(h.querySelector(".now")?.textContent).toContain("<img src=x onerror=alert(1)>");
  });
  it("heads the page with the issue title and sets the tab title", () => {
    const { h, strip } = mount();
    strip.update({ roomId: "3kq7x2ma", snap, phase: "Active", driver: snap.driver, summary: summary(), needs: 0, connection: "live" });
    expect(h.querySelector("h1")?.textContent).toBe("#74 Chat view: pair tool calls");
    expect(document.title).toBe("#74 Chat view: pair tool calls · Implementing");
  });
  it("falls back to the room id before the summary arrives", () => {
    const { h, strip } = mount();
    strip.update({ roomId: "3kq7x2ma", snap, phase: "Open", driver: snap.driver, needs: 0, connection: "connecting…" });
    expect(h.querySelector("h1")?.textContent).toBe("Room 3kq7x2ma");
    expect(h.querySelector(".strip-conn")?.textContent).toBe("connecting…");
  });
  it("shows the task phase as a chip, the budget as a meter, and you as 'you'", () => {
    const { h, strip } = mount();
    strip.update({ roomId: "3kq7x2ma", snap, phase: "Active", driver: snap.driver, summary: summary(), needs: 0, connection: "live" });
    const chips = [...h.querySelectorAll(".chip")].map((c) => c.textContent);
    expect(chips).toContain("Implementing");
    expect(chips.some((c) => c?.includes("189 k / 1.5 M · 13 %"))).toBe(true);
    // CSP: no style= in markup; the fill is set through the CSSOM, which the CSP permits
    // (jsdom, like a browser, reflects it into the attribute, so the attribute is no evidence).
    expect((h.querySelector(".meter-bar") as HTMLElement).style.getPropertyValue("--fill")).toBe("13%");
    expect(h.querySelector(".chip-driver")?.textContent).toBe("driver: you");
  });
  it("says why the task waits and what comes next, on one line", () => {
    const { h, strip } = mount();
    strip.update({ roomId: "3kq7x2ma", snap, phase: "Active", driver: snap.driver, summary: summary(), needs: 0, connection: "live" });
    expect(h.querySelector(".waiting")?.textContent).toBe("Waiting: the policy needs a maintainer's approval → an approver decides");
    strip.update({ roomId: "3kq7x2ma", snap, phase: "Active", driver: snap.driver, summary: summary({ reason: undefined, next: undefined }), needs: 0, connection: "live" });
    expect(h.querySelector(".waiting")).toBeNull();
  });
  it("shows a needs-you pill only when something does, and a closed line when sealed", () => {
    const { h, strip } = mount();
    strip.update({ roomId: "3kq7x2ma", snap, phase: "Active", driver: snap.driver, summary: summary(), needs: 2, connection: "live" });
    expect(h.querySelector(".needs-pill")?.textContent).toBe("Needs you · 2");
    strip.update({ roomId: "3kq7x2ma", snap, phase: "Closed", driver: snap.driver, summary: summary(), needs: 0, connection: "live",
      sealedBy: { who: "human:291847362183", at: "2026-10-04T14:05:00Z" } });
    expect(h.querySelector(".needs-pill")).toBeNull();
    expect(h.querySelector(".closed")?.textContent).toMatch(/^Closed .* by you$/);
  });

  // Moved from the summary's status block: the parts the strip took over.
  it("calls the pill's handler on click", () => {
    const { h, strip } = mount();
    let clicked = 0;
    strip.onNeedsClick(() => clicked++);
    strip.update(input({ needs: 1 }));
    h.querySelector<HTMLButtonElement>(".needs-pill")!.click();
    expect(clicked).toBe(1);
  });
  it("shows the room phase when there are no task facts, the other phase as a title", () => {
    const { h, strip } = mount();
    strip.update(input({ summary: undefined }));
    expect(h.querySelector(".chip-phase")?.textContent).toBe("Active");
    strip.update(input());
    expect(h.querySelector(".chip-phase")?.getAttribute("title")).toBe("room: Active");
  });
  it("names the run, its role and how long it has run", () => {
    const h = document.createElement("header");
    const strip = mountStrip(h, () => Date.parse("2026-10-04T14:04:30Z"));
    strip.update(input());
    expect(h.querySelector(".chip-run")?.textContent).toBe("implementer · ikely2yk · 4 min");
  });
  it("names another driver short, the full id as a title, and your role", () => {
    const { h, strip } = mount();
    strip.update(input({ driver: "human:999999999999" }));
    const d = h.querySelector(".chip-driver")!;
    expect(d.textContent).toBe("driver: human:9999…");
    expect(d.getAttribute("title")).toBe("human:999999999999");
    const chips = [...h.querySelectorAll(".chip")].map((c) => c.textContent);
    expect(chips).toEqual(expect.arrayContaining(["internal", "you: owner (approver)"]));
  });
  it("renders untrusted text as text, never as markup", () => {
    const { h, strip } = mount();
    const evil = "<img src=x onerror=alert(1)>";
    strip.update(input({ summary: summary({ reason: evil, next: evil, issue: { number: 1, url: "https://x.example/1", title: evil },
      run: { id: evil, role: evil } }) }));
    expect(h.querySelector("img")).toBeNull();
    expect(h.querySelector("h1")?.textContent).toBe(`#1 ${evil}`);
  });
  it("links only https URLs", () => {
    const { h, strip } = mount();
    strip.update(input({ summary: summary({ pr: { number: 7, url: "javascript:alert(1)" }, issue: { number: 3, url: "http://insecure.example/3" } }) }));
    const hrefs = [...h.querySelectorAll("a")].map((a) => a.getAttribute("href"));
    expect(hrefs.some((x) => x?.startsWith("javascript:") || x?.startsWith("http:"))).toBe(false);
    expect(h.textContent).toContain("javascript:alert(1)");
    strip.update(input());
    expect([...h.querySelectorAll("a")].map((a) => a.getAttribute("href")))
      .toEqual(["https://github.com/o/r/pull/77", "https://github.com/o/r/issues/74"]);
  });
  it("skips the parts the broker omits", () => {
    const { h, strip } = mount();
    strip.update(input({ summary: summary({ run: { id: "run-1", role: "implementer" }, budget: null, pr: null,
      issue: { number: 3, url: "https://github.com/o/r/issues/3" }, reason: undefined, next: undefined }) }));
    expect(h.querySelector(".chip-run")?.textContent).toBe("implementer · run-1");
    expect(h.querySelector(".chip-budget")).toBeNull();
    expect(h.querySelector("h1")?.textContent).toBe("Room 3kq7x2ma");
    expect(h.textContent).not.toMatch(/undefined|null|NaN/);
  });
  it("renders before the state frame, from the room id alone", () => {
    const { h, strip } = mount();
    strip.update({ roomId: "3kq7x2ma", phase: "", driver: "", needs: 0, connection: "connecting…" });
    expect(h.querySelector("h1")?.textContent).toBe("Room 3kq7x2ma");
    expect(h.textContent).not.toMatch(/undefined|driver:/);
  });

  // Q2: after a run ends the page says so, and the Now line names the last activity.
  describe("with no agent present", () => {
    const hi = { seq: 3, text: "hi", status: "ok" as const, ts: "2026-10-04T13:59:58Z" };
    const at = () => Date.parse("2026-10-04T14:00:00Z");
    it.each([
      ["a room that never ran, a human's message aside", { ran: false, summary: summary({ run: null }), now: { ...hi, status: "message" as const } }, "No run yet"],
      ["a run that ended, with its last activity", { ran: true, now: hi }, "No run · last: hi · 2 s ago"],
      ["a run that ended with no activity seen", { ran: true, now: undefined }, "No run"],
    ])("%s", (_name, over, want) => {
      const h = document.createElement("header");
      const strip = mountStrip(h, at);
      strip.update(input({ running: false, ...over }));
      expect(h.querySelector(".now")?.textContent).toBe(want);
      expect(h.querySelector(".now-hint")?.textContent).toBe("Start one under Run");
      expect(h.querySelector(".chip-run")).toBeNull(); // the facts' run is over: no ageing chip
    });
    it("links the last activity to its row", () => {
      const h = document.createElement("header");
      const strip = mountStrip(h, at);
      let clicked = 0;
      strip.onNowClick((seq) => { clicked = seq; });
      strip.update(input({ running: false, ran: true, now: hi }));
      h.querySelector<HTMLButtonElement>(".now-link")!.click();
      expect(clicked).toBe(3);
    });
    it("names the run again once an agent joins", () => {
      const h = document.createElement("header");
      const strip = mountStrip(h, () => Date.parse("2026-10-04T14:04:30Z"));
      strip.update(input({ running: false, ran: true, now: hi }));
      strip.update(input({ running: true, ran: true, now: hi }));
      expect(h.querySelector(".now")?.textContent).toBe("Now ▸ hi · ok · 4 min ago");
      expect(h.querySelector(".chip-run")?.textContent).toBe("implementer · ikely2yk · 4 min");
    });
  });
  it("shows the last verdict, as roomctl status does", () => {
    const { h, strip } = mount();
    strip.update(input({ summary: summary({ lastVerdict: { by: "reviewer", verdict: "changes_requested", at: "2026-10-04T13:00:00Z" } }) }));
    expect(h.querySelector(".chip-verdict")?.textContent).toBe("last verdict: reviewer changes_requested");
    strip.update(input());
    expect(h.querySelector(".chip-verdict")).toBeNull();
  });
  it("reads Waiting or Closed before the Now line, as the spec draws it", () => {
    const { h, strip } = mount();
    strip.update(input({ now: { seq: 1, text: "hi", status: "message", ts: new Date().toISOString() } }));
    const order = [...h.querySelector(".strip")!.children].map((c) => c.className);
    expect(order).toEqual(["strip-head", "strip-tail", "strip-now"]);
  });
  it("keeps the title, the chips and a focused pill in place when only the count or the connection moves", () => {
    const { h, strip } = mount();
    document.body.append(h);
    try {
      strip.update(input({ needs: 1 }));
      const [h1, pr, pill] = [h.querySelector("h1"), h.querySelector(".chip-pr"), h.querySelector<HTMLButtonElement>(".needs-pill")!];
      pill.focus();
      strip.update(input({ needs: 2, connection: "reconnecting in 2 s: connection lost" }));
      expect(h.querySelector("h1")).toBe(h1);
      expect(h.querySelector(".chip-pr")).toBe(pr);
      expect(h.querySelector(".needs-pill")).toBe(pill);
      expect(pill.textContent).toBe("Needs you · 2");
      expect(document.activeElement).toBe(pill);
      expect(h.querySelector(".strip-conn")?.textContent).toBe("reconnecting in 2 s: connection lost");
      strip.update(input({ needs: 0 }));
      expect(h.querySelector(".needs-pill")).toBeNull();
      strip.update(input({ needs: 3 }));
      expect(h.querySelector(".needs-pill")?.textContent).toBe("Needs you · 3");
      expect(h.querySelector(".strip-chips")!.lastElementChild?.className).toBe("strip-conn"); // the pill sits before it
    } finally {
      h.remove();
    }
  });
  it.each([["Closed", "Closed"], ["Sealed", "Sealed"]])("says %s when sealed with no known closer", (phase, want) => {
    const { h, strip } = mount();
    strip.update(input({ sealed: true, summary: summary({ phase, reason: undefined, next: undefined }) }));
    expect(h.querySelector(".closed")?.textContent).toBe(want);
  });
  it("says Closed from the room phase alone when no summary says more", () => {
    const { h, strip } = mount();
    strip.update(input({ sealed: true, phase: "Closed", summary: undefined }));
    expect(h.querySelector(".closed")?.textContent).toBe("Closed");
    strip.update(input({ sealed: true, phase: "Active", summary: undefined }));
    expect(h.querySelector(".closed")?.textContent).toBe("Sealed");
  });
});

// One fixture for Go and TS: the broker's own golden summaries, read where internal/summary keeps them.
describe("the broker's golden summaries in the strip", () => {
  const dir = join(__dirname, "../../internal/summary/testdata");
  const files = readdirSync(dir).filter((f) => f.endsWith(".json"));
  it.each(files)("%s renders every part it carries", (f) => {
    const { want } = JSON.parse(readFileSync(join(dir, f), "utf8")) as { want: Summary };
    const { h, strip } = mount();
    strip.update(input({ summary: want }));
    expect(h.querySelector(".chip-phase")?.textContent).toBe(want.status.phase);
    if (want.status.pr) expect(h.textContent).toContain(`#${want.status.pr.number}`);
    if (want.status.issue) expect(h.textContent).toContain(`#${want.status.issue.number}`);
    expect(h.textContent).not.toMatch(/undefined|null|NaN|Invalid Date/);
  });
});

describe("formatting", () => {
  // The same boundaries as roomctl's tokens() (internal/roomctl TestBudgetTextBoundaries).
  it.each([[0, "0"], [999, "999"], [1000, "1 k"], [189093, "189 k"], [999_499, "999 k"], [999_500, "1 M"], [1_000_000, "1 M"],
    [1_500_000, "1.5 M"], [4000000, "4 M"]])("fmtTokens(%d) = %s", (n, s) => expect(fmtTokens(n)).toBe(s));
  it("reads a zero limit without a percentage", () => {
    const { h, strip } = mount();
    strip.update(input({ summary: summary({ budget: { usedTokens: 5000, limitTokens: 0 } }) }));
    expect(h.querySelector(".chip-budget")?.textContent?.trim()).toBe("5 k / 0");
  });
  it("shortens a principal and keeps a short one", () => {
    expect(shortPrincipal("human:291847362183")).toBe("human:2918…");
    expect(shortPrincipal("system:factory")).toBe("system:factory");
  });
});
