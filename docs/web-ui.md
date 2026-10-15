# Web UI

The web UI is the one place to watch a room, steer a run, decide an approval and fork. It is
embedded in the broker and served behind oauth2-proxy; [roomctl](roomctl.md) reads and posts but
never steers or approves. What it shows is the room's [log](concepts.md#the-log), folded for a
person; the vocabulary is in [Concepts](concepts.md).

## Signing in

| Step | What happens |
|---|---|
| Open `https://rooms.<domain>` | Your browser needs the tailnet; the host resolves only there |
| oauth2-proxy | Sends you to ZITADEL and keeps your session in a `SameSite=Strict` cookie |
| The broker checks your groups | `agents-member` reads the rooms you are let into and acts where a room grants it; `agents-admin` is owner and approver in every room |
| An expired session | A `401` from the API sends the page back through `/oauth2/start` |

- **Neither group: `403`.** Ask an operator to add you to `agents-member`; the group names are the broker's config ([Integration](integration.md)).
- **Room visibility follows GitHub.** A member sees a room only if GitHub lets the account linked to their ZITADEL user read the room's repository. No link, no rooms: the list is empty rather than an error. An admin sees every room. A room you may not read answers exactly like one that does not exist. The fix for an empty list is the one in [roomctl's troubleshooting](roomctl.md#troubleshooting).

Sources: `docs/api.md` (the `:8080` checks), `internal/policy/policy.go` (`Groups`, `Resolve`).

## The room list

A table, one row per room you can read. Rooms that need you come first, then the most recently
active, then by id. The task cell links to the room page.

| Column | Shows |
|---|---|
| Needs you | A count of what waits for you: approvals you could decide, a task held on a room you drive or own, a PR waiting for your merge. Empty when nothing does |
| Task | The factory task's issue number and title; `Room <id>` for a room with no task |
| Repository | The room's `owner/name` |
| Phase | The task's phase, else the room's |
| PR | `#n`, when the task has one |
| Last activity | How long ago the room's log last moved (`lastEventAt`), the exact time on hover; `—` for a room with no events |
| Room | The room id |

Above the table, three filters refetch the list from the API: **mine** (tasks whose issue you
filed or labelled, or whose PR you authored or review, matched on your linked GitHub login),
**needs me** (rows with something waiting for you) and a repository picker, filled from the
repositories of the rooms the page first listed. They are the API's `mine`, `needs_me` and
`repo=owner/name`, applied after the access check so none reveals an unreadable room
([API](api.md#8080--human-api)). An empty table says why: `No rooms match` under a filter,
`No rooms you can read yet` without one.

### New room

**New room**, beside the filters, opens a form above the list that creates a room you own and
drive, then opens it.

| Field | Rule |
|---|---|
| Data class | `internal` or `public`, no default. It cannot change once the room exists: it sets the model route, the tools and the egress its runs get. It does not change who can read the room |
| Repository | `owner/name`, required. Anyone who can read it on GitHub can read the room. You must be able to read it yourself |

A `CLI setup` fold under the list prints the one block that installs roomctl and configures it
for this broker ([roomctl](roomctl.md)).

## The room page

From the top:

| Part | Shows |
|---|---|
| Title strip | The task's issue number and title (`Room <id>` without a task), then chips: the phase, the run (role, id and age, while an agent is in the room), the budget (`189 k / 1.5 M · 13 %` with a bar), the PR and issue links, the last verdict, the data class, the driver (`you` when it is you) and your role (with `approver` when you hold the flag). A `Needs you · N` button jumps to the panel below. Then a `Waiting: <why> → <next>` line while the task waits, or, in a sealed room, `Closed <when> by <who>` (or `Sealed`) instead. Last, the **Now line**: what the agent is doing (the latest call, its status, how long ago), a link that opens and focuses that row; `No run yet` before any run, `No run` once the agent left, with `Start one under Run` for an owner or the driver |
| Needs you | Shown only when something waits. One card per pending approval, decided right there (below); then the PRs waiting for your merge and the tasks held on your room. A viewer who may not decide reads the cards under `Waiting for an approver` |
| Notes | The latest note from the agents, labelled as the agent's claim; the earlier ones, newest first, behind a `Notes (N)` toggle. No notes, no block |
| Conversation | Messages as bubbles, each actor named by room role and short id (`you` for you); a tool call and its result as one collapsed row; the policy's decisions as badges on that row, only when not a plain allow or when a person decided. Runs of plumbing (state changes, turns, membership lines) fold into one `· turn <id> · N changes` line, open on click; a harness error, a seal and a driver change to or from you stay out of the fold. Each message and handoff can open a fork |
| Raw events | A fold, closed by default, with every event as the broker wrote it. It is always as current as the conversation |
| Controls | Named groups, each shown only to the standings that use it: **Run** (start a run as a role; a PR URL for a reviewer, egress profiles under `advanced`), **Driver** (who holds the token; give, interrupt, request, take with a reason), **Queued for the next run** (remove; `steer now` for the driver), and **Room**, a fold for owners (members, invite, how to stop the task with `factory/stop` on its issue, close) |
| Dock | Pinned at the foot: the composer (the text, `Deliver to` — post to the room, queue for the next run's brief, or steer the running agent — and Send), the page's notices, the theme button and the connection |

Destructive acts ask first: **interrupt**, **take**, **steer now** and **close room** turn into
`confirm: …` beside a cancel, and go back after 10 s, on cancel, or when the control is hidden.

The summary behind the strip, the panel and the notes refetches at most once a second, and only
when an event it folds arrives (a task phase, a limit, progress, a verdict, an approval), not on
every chat line or tool call. A sealed room shows who closed it, has no waiting line, no needs
and no actions, and hides the composer, the Run and Driver groups and the invite. Everything an
agent wrote is shown as text; a URL becomes a link only when it is `https`. The page keeps the
newest 5,000 events; older ones leave the page, never the log.

## What each standing may do

A standing is cumulative: watcher, then collaborator, then owner. The **approver** flag adds
deciding on top of any of them. The **driver** is whoever holds the driver token, whatever their role.
`agents-admin` is owner and approver everywhere. The broker checks every action; a hidden
button is a courtesy. Source: `internal/policy/policy.go`.

| Action | Watcher | Collaborator | Owner | Approver flag | Driver |
|---|---|---|---|---|---|
| Read the room, live | yes | yes | yes | | yes |
| Fork | yes | yes | yes | | yes |
| Chat (a message no agent receives) | | yes | yes | | yes |
| Queue a message for the next run | | yes | yes | | yes |
| Remove a queued message | | yes, your own | yes, your own | | yes, any |
| Steer the running agent (web only) | | | | | yes |
| Interrupt (web only) | | | | | yes |
| Promote a queued message to a steer (web only) | | | | | yes |
| Hand a task to a role (start a run) | | | yes | | yes |
| Request the driver token (web only) | | yes, if not the holder | yes, if not the holder | | |
| Give the driver token (web only) | | | | | yes |
| Take the driver token (web only) | | | yes | | |
| Decide an approval (web only) | | | yes | yes | |
| Invite, change roles, close | | | yes | | |

Reading the table:

- **Web only** means a token issued to roomctl is refused, because a local agent could run roomctl. Chat, queue, fork and start a run work from either client.
- **Steering is the driver's alone.** An owner who is not the driver takes the token (with a reason) before steering.
- **Without the driver token**, a message sent from the composer can be posted to the room or queued; the steer option is disabled. With no agent in the room it is not offered at all. A draft whose steer option goes away (the token moved, the run ended) stays, and the composer asks where it should go.
- **A sealed room** accepts no action. Only its owner or an admin may fork it.
- **An agent run** can read and post in its own room and never decides an approval; only the broker's policy engine can decide on a system's behalf.

## Approvals

When a run is about to do something a human must clear, a card appears in the Needs you panel,
where it is decided; the raw events show the same request, read-only.

| Part | Rule |
|---|---|
| The card | The command the call would run first (else its class), then the class, the run, when it expires, and the **raw action** as redacted text behind a toggle. Never an agent's description of it. A link takes you to the call's row while the page holds it. `approve` and `deny` with an optional reason; the card leaves once the log records a decision, whoever made it |
| Who decides | An owner, or a member with the approver flag, in the web UI. Others see the request but no buttons |
| First decision wins | The first valid decision stands; every later one, and an expiry or a superseded request, answers `already_decided` |
| Four-eyes | When the room requires it, nobody who prompted the turn may decide it: not whoever requested the run, steered it, or wrote a queued message it consumed |
| Reason | Optional, up to 1 KiB, redacted. The agent reads it |
| Expiry | 30 minutes in an `attended` room; the room's configured delay (default 4 hours) in an `unattended` one. An expired request is denied |
| Superseded | A request whose call already ran, or whose run has left, is closed without a decision |

Sources: `docs/api.md` (approvals, `decide`), `web/src/controls.ts` (`approvalCard`), `web/src/needs.ts`.

## Forks

Pick **fork here** on a message or handoff to branch the room at that point.

| Question | Answer |
|---|---|
| What is copied | Events 1 to the chosen one, with their `seq`, then a marker naming the source. The source's data class, repository and retention. The messages still queued at that point, queued again in the fork |
| What is not | The source's members: the fork has none |
| Who owns it | You: owner and driver |
| Approvals | A source owner or admin keeps the source's approval settings; anyone else gets, class by class, the stricter of the source's and a fresh room's |
| A first run | Optional: pick a role (and a PR for a reviewer, egress profiles for a non-`internal` room). It starts on a new branch from the latest agent commit at or before the chosen event, on your token and budget. If the run fails, the fork stays made and the panel says why |
| Limits | A prefix over 5,000 events or 32 MiB is refused (`too_large`): fork from an earlier message. Three forks at once, then one a minute |
| Sealed source | Only its owner or an admin |

## When something fails

A refused action shows its reason in the dock. Source: `web/src/controls.ts`.

| Reason | What it means | What to do |
|---|---|---|
| `not_permitted` | Your role, or this client, does not allow it; or the factory refused you a run | Check the table above; ask an owner to invite you with a wider role |
| `stale_epoch` | Someone else holds the driver token now | Read the new holder, try again if it still applies |
| `rate_limited` | Over 10 actions a second, or over the fork limits | Wait and retry |
| `no_running_run` | Nothing is running to steer or interrupt | Queue the message for the next run |
| `room_busy` | A run is already running, or one was just requested and has not joined | Wait for it to join or end |
| `reviewer_needs_pr` | A reviewer has no PR to review | Give the PR's URL |
| `over_budget` | The factory refused the run: you are over your budget | See [the factory](factory.md) |
| `factory_unavailable` | The run could not be requested now | Retry |
| `bad_action` | The request was malformed, or a give went to someone who cannot drive | Check the fields; give to a member who can drive, or the room's system holder |
| `not_queued` | The message was already delivered or removed | Nothing |
| `sealed` | The room is closed and takes no more actions | Fork it, if you own it or are an admin |
| `conflict` | The room changed at the same moment | Retry |
| `log_unavailable` | The room's log is unavailable | Retry |
| `already_decided` | Another approver decided first | Nothing |
| `four_eyes` | The room needs an approver who did not prompt the turn | Ask another approver |
| `too_large` | The fork would copy over 5,000 events or 32 MiB | Fork from an earlier message |

Other rooms-side outcomes: a `404` on the page means no such room or no access (the same
answer on purpose); creating a room fails with `400` for a data class or repository not
accepted, `404` when you cannot read the repository, `503` when your GitHub access cannot be
checked.
