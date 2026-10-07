import { afterEach, describe, expect, it, vi } from "vitest";
import { api, nav, SignInRequired } from "../src/api";
import { listRooms, newRoomForm } from "../src/view";

const answer = (status: number, body = "") => (() => Promise.resolve(new Response(body, { status }))) as typeof fetch;

describe("api", () => {
  afterEach(() => vi.restoreAllMocks());

  // An expired session that oauth2-proxy could not refresh answers 401: without a
  // sign-in the page would sit dead until a reload.
  it("sends a 401 to sign in again, back to the current page", async () => {
    const go = vi.spyOn(nav, "go").mockImplementation(() => {});
    history.replaceState(null, "", "/r/3kq7x2ma?x=1");
    await expect(api("/api/rooms", {}, answer(401))).rejects.toBeInstanceOf(SignInRequired);
    expect(go).toHaveBeenCalledWith("/oauth2/start?rd=%2Fr%2F3kq7x2ma%3Fx%3D1");
  });

  it("passes any other status through", async () => {
    const go = vi.spyOn(nav, "go").mockImplementation(() => {});
    expect((await api("/api/rooms", {}, answer(503))).status).toBe(503);
    expect(go).not.toHaveBeenCalled();
  });

  it("signs in from the room list too", async () => {
    const go = vi.spyOn(nav, "go").mockImplementation(() => {});
    history.replaceState(null, "", "/");
    const el = document.createElement("div");
    await listRooms(el, answer(401));
    expect(go).toHaveBeenCalledWith("/oauth2/start?rd=%2F");
    expect(el.textContent).toMatch(/signing in/);
  });
});

describe("newRoomForm", () => {
  afterEach(() => vi.restoreAllMocks());

  function submit(get: typeof fetch, dataClass = "public", repository = "") {
    const form = newRoomForm(get);
    (form.elements.namedItem("dataClass") as HTMLSelectElement).value = dataClass;
    (form.elements.namedItem("repository") as HTMLInputElement).value = repository;
    form.dispatchEvent(new Event("submit", { cancelable: true }));
    return form;
  }

  it("creates a room and opens it", async () => {
    const go = vi.spyOn(nav, "go").mockImplementation(() => {});
    const calls: [string, RequestInit][] = [];
    const get = ((url: string, init: RequestInit) => {
      calls.push([url, init]);
      return Promise.resolve(new Response(JSON.stringify({ id: "3kq7x2ma" }), { status: 201 }));
    }) as unknown as typeof fetch;
    submit(get, "internal", "Smana/cloud-native-ref");
    await vi.waitFor(() => expect(go).toHaveBeenCalledWith("/r/3kq7x2ma"));
    expect(calls[0][0]).toBe("/api/rooms");
    expect(calls[0][1].method).toBe("POST");
    expect(JSON.parse(String(calls[0][1].body))).toEqual({ dataClass: "internal", repository: "Smana/cloud-native-ref" });
  });

  it("leaves an empty repository to the CRD's default", async () => {
    vi.spyOn(nav, "go").mockImplementation(() => {});
    let body = "";
    const get = ((_: string, init: RequestInit) => {
      body = String(init.body);
      return Promise.resolve(new Response(JSON.stringify({ id: "3kq7x2ma" }), { status: 201 }));
    }) as unknown as typeof fetch;
    submit(get);
    await vi.waitFor(() => expect(body).toBe(JSON.stringify({ dataClass: "public" })));
  });

  it("says why a room was not created", async () => {
    const go = vi.spyOn(nav, "go").mockImplementation(() => {});
    for (const [status, copy] of [[400, /not accepted/], [429, /too many/i], [503, /cannot be created/], [500, /500/]] as const) {
      const form = submit(answer(status));
      await vi.waitFor(() => expect(form.querySelector(".notice")?.textContent).toMatch(copy));
    }
    // An answer that is not a room id never becomes a navigation.
    const form = submit(answer(201, JSON.stringify({ id: "../oauth2/sign_out" })));
    await vi.waitFor(() => expect(form.querySelector(".notice")?.textContent).toMatch(/unexpected/));
    expect(go).not.toHaveBeenCalled();
  });
});
