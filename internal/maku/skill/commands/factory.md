---
description: "The agent factory: /factory handoff <what> | status [#issue] | inbox"
---

You were asked: `/factory $ARGUMENTS`. Use the `factory` MCP server's tools. The first word is the
subcommand; the rest is its argument. Show each tool's text as it is, and never apply a label.

- `handoff [what]`: draft one issue for what the developer gave (default: the side task just
  discussed) in the template's three sections (`## What is wrong`, `## What it should be`,
  `## Acceptance check`). Check it with `issue_draft`, fix what it reports, then call `issue_file`. When it returns `started: true`, start a background `room_wait` on the room at once (see the skill), without being asked.
- `status [room]`: call `room_summary` for the room. The room may be `#<issue>`, `PR #<n>`, a bare
  number or a room id, passed as typed; with none, use the `rooms_list` row updated most recently.
- `inbox`: call `rooms_list` with `needsMe`, then with `mine`.

With no subcommand, or one that is not listed, show these three lines and stop.
