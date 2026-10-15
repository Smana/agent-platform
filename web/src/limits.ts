// SPDX-License-Identifier: Apache-2.0

// The page keeps the newest maxRows rows; older ones leave the page, never the log. The
// chat, the raw view and the room's state share the cap, so none of them imports another.
export const maxRows = 5000;
