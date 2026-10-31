import { afterEach, describe, expect, it, vi } from "vitest";
import { nav } from "../src/api";
import { listRooms, newRoomForm, RoomLog, rowCells, sortRooms } from "../src/view";

const row = (text: string) => { const p = document.createElement("p"); p.textContent = text; return p; };

describe("RoomLog", () => {
  it("replaces a resent seq's row in place", () => {
    const el = document.createElement("main");
    const log = new RoomLog(el, 10);
    log.put(1, row("a"));
    log.put(2, row("b"));
    log.put(1, row("a2"));
    expect([...el.children].map((c) => c.textContent)).toEqual(["a2", "b"]);
  });
  it("inserts a resent seq it no longer holds in order, never at the end", () => {
    const el = document.createElement("main");
    const log = new RoomLog(el, 10);
    for (const s of [1, 3, 5]) log.put(s, row(String(s)));
    log.put(4, row("4"));
    log.put(2, row("2"));
    log.put(6, row("6"));
    expect([...el.children].map((c) => c.textContent)).toEqual(["1", "2", "3", "4", "5", "6"]);
  });
  it("keeps the newest rows up to its cap (review M2)", () => {
    const el = document.createElement("main");
    const log = new RoomLog(el, 3);
    for (let s = 1; s <= 5; s++) log.put(s, row(String(s)));
    expect([...el.children].map((c) => c.textContent)).toEqual(["3", "4", "5"]);
    log.put(2, row("late")); // a clamped resume resending a row already dropped
    expect([...el.children].map((c) => c.textContent)).toEqual(["3", "4", "5"]);
  });
});

describe("listRooms", () => {
  const app = () => document.createElement("div");
  it("says why when the list cannot be read, not a blank page (review M3)", async () => {
    for (const fail of [
      () => Promise.reject(new TypeError("Failed to fetch")),
      () => Promise.resolve(new Response("<html>sign in</html>", { status: 200 })),
      () => Promise.resolve(new Response("", { status: 503 })),
    ]) {
      const el = app();
      await listRooms(el, fail as typeof fetch);
      expect(el.textContent).toMatch(/^The room list is unavailable/);
    }
  });
  it("links each room", async () => {
    const el = app();
    const rows = [{ id: "3kq7x2ma", phase: "", owner: "human:1", driver: "", dataClass: "internal", lastSeq: 4 }];
    await listRooms(el, (() => Promise.resolve(new Response(JSON.stringify(rows)))) as typeof fetch);
    expect(el.querySelector("tbody a")?.getAttribute("href")).toBe("/r/3kq7x2ma");
  });
  // #50: a row says which task it is, not only a random id; the title is text, never markup.
  it("names the issue, title, task phase, PR and what needs you", async () => {
    const el = app();
    const rows = [
      { id: "yacoriey", phase: "Idle", owner: "system:factory", driver: "", dataClass: "internal", lastSeq: 70, repository: "Smana/a",
        issue: 2266, title: "<img src=x onerror=alert(1)>", task: "Reviewing", pr: 2267, needsYou: 2 },
      { id: "d66mo4b4", phase: "Idle", owner: "human:1", driver: "", dataClass: "internal", lastSeq: 95, needsYou: 0 },
    ];
    await listRooms(el, (() => Promise.resolve(new Response(JSON.stringify(rows)))) as typeof fetch);
    const [first, second] = [...el.querySelectorAll("tbody tr")].map((tr) => [...tr.querySelectorAll("td")].map((td) => td.textContent));
    expect(first).toEqual(["2", "#2266 <img src=x onerror=alert(1)>", "Smana/a", "Reviewing", "#2267", "—", "yacoriey"]);
    expect(el.querySelector("tbody img")).toBeNull();
    expect(second).toEqual(["", "Room d66mo4b4", "", "Idle", "", "—", "d66mo4b4"]);
    expect(el.querySelectorAll("th[scope=col]").length).toBe(7);
  });
  it("renders a zero lastEventAt as a dash, never 1970", () => {
    const base = { id: "a", phase: "Idle", owner: "", driver: "", dataClass: "", lastSeq: 1 };
    const now = new Date("2026-10-15T10:00:00Z");
    expect(rowCells({ ...base, lastEventAt: "0001-01-01T00:00:00Z" }, now).last).toBe("");
    expect(rowCells({ ...base, lastEventAt: "2026-10-15T09:00:00Z" }, now).last).toBe("1 h ago");
  });
});

describe("the room table", () => {
  const app = () => document.createElement("div");
  const rows = [
    { id: "d66mo4b4", phase: "Idle", owner: "human:5", driver: "human:5", dataClass: "public", lastSeq: 95, repository: "o/a", needsYou: 0, lastEventAt: "2026-10-15T09:00:00Z" },
    { id: "3kq7x2ma", phase: "Active", owner: "human:2", driver: "human:2", dataClass: "internal", lastSeq: 142, repository: "o/b", issue: 74, title: "Chat view", task: "Implementing", pr: 77, needsYou: 2, lastEventAt: "2026-10-15T08:00:00Z" },
    { id: "k2p9rt4v", phase: "Active", owner: "human:2", driver: "system:factory", dataClass: "public", lastSeq: 318, repository: "o/a", issue: 81, title: "Bump mako", task: "Reviewing", pr: 83, needsYou: 0, lastEventAt: "2026-10-15T09:30:00Z" },
  ];
  it("sorts needs-you first, then the most recent activity", () => {
    expect(sortRooms(rows).map((r) => r.id)).toEqual(["3kq7x2ma", "k2p9rt4v", "d66mo4b4"]);
  });
  it("mounts one Maku brand in the list header and titles the tab", async () => {
    const root = app();
    await listRooms(root, (async () => new Response("[]", { status: 200 })) as unknown as typeof fetch);
    expect(root.querySelectorAll(".brand")).toHaveLength(1);
    expect(root.querySelector("header.page-header")?.firstElementChild?.classList.contains("brand")).toBe(true);
    expect(root.querySelector("h1")?.textContent).toBe("Rooms");
    expect(document.title).toBe("Maku rooms");
  });
  it("renders one row per room with the task first and a needs pill", async () => {
    const get = vi.fn(async (url: string) => new Response(JSON.stringify(url.includes("needs_me=1") ? [rows[1]] : rows), { status: 200 }));
    const root = app();
    await listRooms(root, get as unknown as typeof fetch);
    const cells = [...root.querySelectorAll("tbody tr")].map((tr) => [...tr.querySelectorAll("td")].map((td) => td.textContent));
    expect(cells[0][0]).toBe("2");
    expect(cells[0][1]).toBe("#74 Chat view");
    expect(cells[0][2]).toBe("o/b");
    expect(cells[2][1]).toBe("Room d66mo4b4");
    expect(root.querySelector("thead")?.textContent).toContain("Last activity");
    (root.querySelector('input[name="needsMe"]') as HTMLInputElement).click();
    await vi.waitFor(() => expect(root.querySelectorAll("tbody tr").length).toBe(1));
    expect(get).toHaveBeenLastCalledWith(expect.stringContaining("needs_me=1"), expect.anything());
  });
  it("says no room matches a filter that leaves none, in one row across the table", async () => {
    const get = vi.fn(async (url: string) => new Response(JSON.stringify(url.includes("mine=1") ? [] : rows), { status: 200 }));
    const root = app();
    await listRooms(root, get as unknown as typeof fetch);
    (root.querySelector('input[name="mine"]') as HTMLInputElement).click();
    await vi.waitFor(() => expect(root.querySelector("tbody")?.textContent).toBe("No rooms match"));
    expect(root.querySelector("tbody td")?.getAttribute("colspan")).toBe("7");
    const empty = app();
    await listRooms(empty, (async () => new Response("[]", { status: 200 })) as unknown as typeof fetch);
    expect(empty.querySelector("tbody")?.textContent).toBe("No rooms you can read yet");
  });
  it("filters by repository with the API's repo param", async () => {
    const get = vi.fn(async () => new Response(JSON.stringify(rows), { status: 200 }));
    const root = app();
    await listRooms(root, get as unknown as typeof fetch);
    const sel = root.querySelector('select[name="repo"]') as HTMLSelectElement;
    expect([...sel.options].map((o) => o.value)).toEqual(["", "o/a", "o/b"]);
    sel.value = "o/a";
    sel.dispatchEvent(new Event("change"));
    await vi.waitFor(() => expect(get).toHaveBeenLastCalledWith("/api/rooms?repo=o%2Fa", expect.anything()));
  });
  it("opens the new-room form above the table, which has a name", async () => {
    const root = app();
    await listRooms(root, (async () => new Response("[]", { status: 200 })) as unknown as typeof fetch);
    const form = root.querySelector("form.new-room")!;
    const table = root.querySelector("table.rooms")!;
    expect(form.compareDocumentPosition(table) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(table.getAttribute("aria-label")).toBe("Rooms");
  });
  it("never lets an older filter answer overwrite a newer one", async () => {
    const waiting: ((r: Response) => void)[] = [];
    const get = vi.fn((url: string) => url === "/api/rooms"
      ? Promise.resolve(new Response(JSON.stringify(rows)))
      : new Promise<Response>((res) => waiting.push(res)));
    const root = app();
    await listRooms(root, get as unknown as typeof fetch);
    (root.querySelector('input[name="mine"]') as HTMLInputElement).click(); // older
    (root.querySelector('input[name="needsMe"]') as HTMLInputElement).click(); // newer
    await vi.waitFor(() => expect(waiting.length).toBe(2));
    waiting[1](new Response(JSON.stringify([rows[1]])));
    await vi.waitFor(() => expect(root.querySelectorAll("tbody tr").length).toBe(1));
    waiting[0](new Response(JSON.stringify(rows)));
    await new Promise((r) => setTimeout(r, 20));
    expect(root.querySelectorAll("tbody tr").length).toBe(1);
  });
  it("offers Retry when the first fetch fails", async () => {
    let n = 0;
    const get = vi.fn(async () => (n++ ? new Response("[]") : new Response("", { status: 503 })));
    const root = app();
    await listRooms(root, get as unknown as typeof fetch);
    (root.querySelector("button") as HTMLButtonElement).click();
    await vi.waitFor(() => expect(root.querySelector("table.rooms")).not.toBeNull());
  });
  it("keeps the new-room form behind a button and the CLI setup below the table", async () => {
    const root = app();
    await listRooms(root, (async () => new Response("[]", { status: 200 })) as unknown as typeof fetch);
    expect((root.querySelector("form.new-room") as HTMLElement).hidden).toBe(true);
    (root.querySelector('button[data-act="new-room"]') as HTMLButtonElement).click();
    expect((root.querySelector("form.new-room") as HTMLElement).hidden).toBe(false);
    expect(root.lastElementChild?.className).toBe("cli-setup");
  });
});

describe("the room list's layout", () => {
  const rows = [
    { id: "3kq7x2ma", phase: "Active", owner: "", driver: "", dataClass: "internal", lastSeq: 1, repository: "o/b", issue: 74, title: "Chat view", task: "Reviewing", pr: 77, needsYou: 2 },
  ];
  const mount = async () => {
    const root = document.createElement("div");
    await listRooms(root, (async () => new Response(JSON.stringify(rows), { status: 200 })) as unknown as typeof fetch);
    return root;
  };
  it("sets the page title apart from the brand, as its own element", async () => {
    const h1 = (await mount()).querySelector("header.page-header h1")!;
    expect(h1.className).toBe("page-title");
    expect(h1.previousElementSibling?.classList.contains("brand")).toBe(true);
  });
  it("makes the whole row a link to the room through the task anchor, not a row handler", async () => {
    const tr = (await mount()).querySelector("tbody tr") as HTMLTableRowElement;
    expect(tr.className).toBe("room-row");
    expect(tr.onclick).toBeNull();
    const links = tr.querySelectorAll("a");
    expect(links).toHaveLength(1);
    expect(links[0].className).toBe("task-link");
    expect(links[0].getAttribute("href")).toBe("/r/3kq7x2ma");
    expect(links[0].textContent).toBe("#74 Chat view");
  });
  it("says the room page's status line, its icon and words, with the raw phase in the tooltip", async () => {
    const root = document.createElement("div");
    const said = [{ ...rows[0], status: { kind: "waiting-you", icon: "⏳", text: "Waiting for you — approve a pending action" } }];
    await listRooms(root, (async () => new Response(JSON.stringify(said), { status: 200 })) as unknown as typeof fetch);
    expect([...root.querySelectorAll("th")].map((th) => th.textContent)[3]).toBe("Status");
    const td = root.querySelectorAll("tbody tr td")[3] as HTMLElement;
    expect(td.title).toBe("Reviewing");
    const line = td.querySelector<HTMLElement>(".row-status")!;
    expect(line.dataset.kind).toBe("waiting-you");
    expect(line.querySelector(".row-status-icon")?.textContent).toBe("⏳");
    expect(line.querySelector(".row-status-icon")?.getAttribute("aria-hidden")).toBe("true");
    expect(line.querySelector(".row-status-words")?.textContent).toBe("Waiting for you — approve a pending action");
    expect(td.textContent).toBe("⏳ Waiting for you — approve a pending action");
    expect(root.querySelector("tbody [role], tbody [aria-live]")).toBeNull(); // no live region per row
    expect(td.querySelector(".chip-phase")).toBeNull();
  });
  it("falls back to the phase chip when the broker sends no status", async () => {
    const td = (await mount()).querySelectorAll("tbody tr td")[3] as HTMLElement;
    expect(td.title).toBe(""); // the chip already says the phase
    const chip = td.querySelector("span.chip.chip-phase")!;
    expect(chip.textContent).toBe("Reviewing");
    expect(td.textContent).toBe("Reviewing");
    expect(chip.children).toHaveLength(0);
  });
  it("marks the repository and the room id as secondary", async () => {
    const tds = (await mount()).querySelectorAll("tbody tr td");
    expect(tds[2].className).toBe("repo");
    expect(tds[6].className).toBe("room-id");
  });
  it("puts New room, the primary action, after the filters", async () => {
    const tools = (await mount()).querySelector(".list-tools")!;
    const add = tools.lastElementChild as HTMLButtonElement;
    expect(add.dataset.act).toBe("new-room");
    expect(add.classList.contains("primary")).toBe(true);
  });
});

describe("newRoomForm", () => {
  afterEach(() => vi.restoreAllMocks());

  // R11: the data class is fixed for the room's life and governs its runs, so the
  // human chooses it: no default, least of all public.
  it("sends nothing until a data class is chosen", async () => {
    vi.spyOn(nav, "go").mockImplementation(() => {});
    const posts: string[] = [];
    const get = ((_: string, init: RequestInit) => {
      posts.push(String(init.body));
      return Promise.resolve(new Response(JSON.stringify({ id: "3kq7x2ma" }), { status: 201 }));
    }) as unknown as typeof fetch;
    const form = newRoomForm(get);
    const dataClass = form.elements.namedItem("dataClass") as HTMLSelectElement;
    expect(dataClass.value).toBe("");
    expect(dataClass.required).toBe(true);
    expect([dataClass.selectedOptions[0].textContent, dataClass.selectedOptions[0].disabled]).toEqual(["choose a data class", true]);
    const hint = form.querySelector(".hint")!.textContent;
    for (const says of [/cannot change/, /model route, the tools and the egress/, /does not change who can read the room/]) expect(hint).toMatch(says);
    form.dispatchEvent(new Event("submit", { cancelable: true }));
    await Promise.resolve();
    expect(posts).toEqual([]);
    expect(form.querySelector(".notice")?.textContent).toBe("Choose a data class first.");
    dataClass.value = "internal";
    (form.elements.namedItem("repository") as HTMLInputElement).value = "Smana/a";
    form.dispatchEvent(new Event("submit", { cancelable: true }));
    await vi.waitFor(() => expect(posts).toEqual([JSON.stringify({ dataClass: "internal", repository: "Smana/a" })]));
  });
});

describe("the room list's auto-refresh", () => {
  const base = { owner: "", driver: "", dataClass: "internal", lastSeq: 1, repository: "o/a" };
  const a = { ...base, id: "aaaaaaaa", phase: "Active", title: "A", issue: 1, needsYou: 1 };
  const b = { ...base, id: "bbbbbbbb", phase: "Active", title: "B", issue: 2, needsYou: 0 };
  // A clock whose timers fire by hand: fire() runs the last one set, the next refresh.
  const manual = () => {
    const fires: (() => void)[] = [];
    return {
      clock: { now: () => 0, set: (f: () => void) => fires.push(f), clear: () => {} },
      fire: () => fires.at(-1)!(),
    };
  };
  const mount = async (answers: unknown[][]) => {
    const m = manual();
    let n = 0;
    const get = vi.fn(async () => new Response(JSON.stringify(answers[Math.min(n++, answers.length - 1)])));
    const root = document.createElement("div");
    document.body.append(root); // focus needs a connected element
    await listRooms(root, get as unknown as typeof fetch, {}, undefined, { clock: m.clock, store: localStorage });
    return { root, get, fire: m.fire };
  };
  afterEach(() => { document.body.replaceChildren(); localStorage.clear(); });

  it("mounts the refresh control between the filters and New room", async () => {
    const { root } = await mount([[a]]);
    const tools = root.querySelector(".list-tools")!;
    const ctl = tools.querySelector(".refresh")!;
    expect(ctl.nextElementSibling?.getAttribute("data-act")).toBe("new-room");
    expect((ctl.querySelector("select") as HTMLSelectElement).value).toBe("10");
  });
  it("refreshes with the filters the page shows", async () => {
    const { root, get, fire } = await mount([[a]]);
    (root.querySelector('input[name="mine"]') as HTMLInputElement).click();
    await vi.waitFor(() => expect(get).toHaveBeenCalledTimes(2));
    fire();
    await vi.waitFor(() => expect(get).toHaveBeenCalledTimes(3));
    expect(get).toHaveBeenLastCalledWith(expect.stringContaining("mine=1"), expect.anything());
  });
  it("leaves the rows alone when a refresh reads the same list", async () => {
    const { root, get, fire } = await mount([[a, b], [a, b]]);
    const before = root.querySelector("tbody tr");
    fire();
    await vi.waitFor(() => expect(get).toHaveBeenCalledTimes(2));
    await new Promise((r) => setTimeout(r, 0));
    expect(root.querySelector("tbody tr")).toBe(before);
  });
  it("keeps the focused room focused when a refresh changes the list", async () => {
    const { root, fire } = await mount([[a, b], [{ ...a, title: "A2" }, b]]);
    const link = root.querySelector('a[href="/r/bbbbbbbb"]') as HTMLAnchorElement;
    link.focus();
    fire();
    await vi.waitFor(() => expect(root.querySelector('a[href="/r/aaaaaaaa"]')?.textContent).toBe("#1 A2"));
    expect(document.activeElement).not.toBe(link); // rebuilt
    expect(document.activeElement?.getAttribute("href")).toBe("/r/bbbbbbbb");
  });
  it("counts the rooms that need you in the tab title, and drops the count at none", async () => {
    const { get, fire } = await mount([[a, { ...b, needsYou: 3 }], [b]]);
    expect(document.title).toBe("(2) Maku rooms");
    fire();
    await vi.waitFor(() => expect(get).toHaveBeenCalledTimes(2));
    await vi.waitFor(() => expect(document.title).toBe("Maku rooms"));
  });
});
