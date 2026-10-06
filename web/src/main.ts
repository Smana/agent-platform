// SPDX-License-Identifier: Apache-2.0

import { mountRoom } from "./room";
import { initTheme } from "./theme";
import { listRooms } from "./view";

const app = document.getElementById("app")!;

// The platform's theme, before anything paints: light by default, dark when the
// user chose it or follows a dark OS (theme.ts, the app-wizard's contract).
const theme = initTheme();

const m = location.pathname.match(/^\/r\/([a-z2-7]{8})$/);
if (m) mountRoom(app, m[1], theme); else void listRooms(app);
