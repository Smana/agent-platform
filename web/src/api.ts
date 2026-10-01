// SPDX-License-Identifier: Apache-2.0

// Where the page goes; an object so tests can replace it (jsdom does not navigate).
export const nav = { go: (url: string) => location.assign(url) };

export class SignInRequired extends Error {
  constructor() { super("signing in again"); }
}

// signIn sends the browser through oauth2-proxy and back to this page.
export function signIn() {
  nav.go(`/oauth2/start?rd=${encodeURIComponent(location.pathname + location.search)}`);
}

// api is fetch for the human API. A 401 means oauth2-proxy could not refresh the
// session: without a new sign-in every later call fails the same way, and the page
// sits dead until a reload.
export async function api(path: string, init: RequestInit = {}, get: typeof fetch = fetch): Promise<Response> {
  const r = await get(path, { credentials: "same-origin", ...init });
  if (r.status === 401) {
    signIn();
    throw new SignInRequired();
  }
  return r;
}
