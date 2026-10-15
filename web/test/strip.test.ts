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
  it.each([[0, "0"], [999, "999"], [189093, "189 k"], [1500000, "1.5 M"], [4000000, "4 M"]])("fmtTokens(%d) = %s", (n, s) => expect(fmtTokens(n)).toBe(s));
  it("shortens a principal and keeps a short one", () => {
    expect(shortPrincipal("human:291847362183")).toBe("human:2918…");
    expect(shortPrincipal("system:factory")).toBe("system:factory");
  });
});
