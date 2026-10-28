# Slack self-serve link: design

**Status:** design, owner-approved scope (2026-10-28). **Scope:** `internal/slackapp`,
`internal/humanapi`, `internal/bridgeapi`, `internal/store`, `internal/factory/api`,
`internal/factory/rooms`, the broker's and the factory's config, `docs/slack.md`, `docs/factory.md`.
**Companion fix:** "no silent ✅" (B, end of this page).

## Conclusion

Today a Slack user may file only once an operator adds `"<team>/<user>": <login>` to the factory's
`onBehalfOf.slack` in git. Self-serve replaces that edit with a proof made by the user themselves:
Slack proves which Slack user asked (a signed `app_mention`, answered ephemerally), ZITADEL proves
which account confirmed (the rooms web UI's login), and the broker records the pair. The factory
then resolves the sponsor from the **account's current GitHub link**, never from anything Slack
sends, and keeps the maintainer check unchanged: a link grants nothing a maintainer did not have.

```mermaid
sequenceDiagram
  participant U as Slack user (browser too)
  participant S as Slack
  participant A as factory-slack-app
  participant B as room-broker
  participant Z as ZITADEL
  participant F as agent-factory
  U->>S: @factory link
  S->>A: app_mention (signed, team + user)
  A->>B: slack_link_code {team, user, name} (App's own token)
  B->>B: store sha256(code), team, user, expires +10 min; one live code per user
  B-->>A: url rooms.<domain>/link/slack?code=…
  A->>S: chat.postEphemeral (only that user sees it)
  U->>B: GET /link/slack?code=… (oauth2-proxy, ZITADEL login)
  B->>Z: the account's GitHub link (D7 resolver)
  B-->>U: "Link Slack user <name> to your account (GitHub: <login>)?" [Confirm]
  U->>B: POST /link/slack (same Origin, SameSite=Strict cookie)
  B->>B: consume code once → slack_links(team, user) = ZITADEL sub
  Note over U,F: later, a ✅ on a draft
  A->>B: run_create on_behalf_of slack:T/U
  B->>F: POST /v1/tasks (the App's token)
  F->>F: onBehalfOf.slack in config? use it (config wins)
  F->>B: GET /v1/links/slack?team&user (system:factory, :8443)
  F->>Z: the sub's current GitHub login
  F->>F: maintainer check, unchanged
```

## Decisions

1. **The code lives in the broker's database**, since the human API consumes it: table
   `slack_link_codes` keyed by `sha256(code)` (a database read leaks no live code), 128 random bits,
   base32. Minting replaces the user's earlier code (one live code per Slack user) and purges
   expired and used rows; at most 1000 live codes in all (`too_many_codes`). TTL 10 minutes.
2. **The link is `slack_links(team, user) → ZITADEL sub`**, not a GitHub login: the login is
   resolved at each ✅ through the account's GitHub IdP link, so a renamed or removed GitHub link
   follows. One Slack user links one account; an account holds at most 5 Slack users.
3. **Who may mint:** only the sponsored principal named in the broker's `human.slackLink.principal`,
   and only for `human.slackLink.teamID`; its two tools (`slack_link_code`, `slack_unlink`) are not
   listed to any other principal.
4. **Who may read a link:** only `system:factory`, on the broker's system API (`:8443`, TLS). The
   factory reads links only for a sponsored principal whose `onBehalfOf.slackLinks` is `true`, so
   another sponsored bot cannot borrow the Slack App's links.
5. **Config wins.** A `onBehalfOf.slack` entry is used as today and the broker is not asked; a
   link record for the same Slack user is ignored (documented in `docs/factory.md`).
6. **The page is server-rendered** (`/link/slack`): no script, a form POST, the room UI's CSP.
   Without a code it lists the account's links, each with an Unlink button.
7. **The ✅ answer names the sponsor** ("Filed #12 for @u, sponsored by @login"), so a wrong link is
   seen at its first use.

## Threat model

| Threat | Mitigation |
|---|---|
| **Code theft** (shoulder, history, logs) | Ephemeral message seen by the user only; 10 min; single use; only its hash is stored; `Referrer-Policy: no-referrer` and `Cache-Control: no-store`. Before sign-in the URL rides oauth2-proxy's redirect through ZITADEL, which is trusted already. A thief who confirms links the *victim's* Slack user to the *thief's* account: the victim's ✅ would file sponsored by the thief, which the thief must already be entitled to (maintainer check), and the Filed line names the sponsor |
| **Replay** | Consumption is one `UPDATE … WHERE used_at IS NULL AND expires_at > now RETURNING`: a second confirm finds nothing (`code_invalid`) |
| **Cross-team** | The App drops another workspace's events (as today); the broker mints only for `human.slackLink.teamID` and re-checks the team when the code is consumed |
| **CSRF** | GET never consumes. The confirm and unlink are POSTs that need the human session (oauth2-proxy's SameSite=Strict cookie), an `Origin` equal to the broker's public origin (required, not only "not foreign"), and a form body |
| **A user linking someone else** (A sends their link to maintainer M, M confirms, A files as M) | The page names the Slack user and workspace and says, before the button, that confirming lets *that* Slack user file tasks sponsored by you; the account's links are listed with Unlink; the Filed line names the sponsor. Linking never widens what M can sponsor: M is still checked as a maintainer |
| **A forged GitHub login** | Nothing Slack sends is a login. The factory resolves the sub's GitHub link itself, through ZITADEL |
| **A compromised Slack App** | It can mint codes (each still needs a human's confirm) and unlink. It cannot create a link or read one |

Refusals (bounded outcome labels `link_code`, `linked`, `unlinked`, `code_invalid`,
`not_linked_github`, `wrong_team`, `too_many_links`, `too_many_codes`): not signed in (oauth2-proxy
sends to the login), no GitHub link on the account, code expired or used, team not the App's.

## B. No silent ✅

Every return on the ✅ path is either a documented silent case or one short thread line naming the
reason and the fix. Silent: a ✅ on a message that is not the App's draft, a bot's own event,
malformed ids, another workspace, an event already seen. Answered: a draft in a channel no longer
served, a repository the channel does not list, a draft whose hash fails, a replaced draft, a
Slack read that failed, every factory refusal (mapped to a sentence with its fix: `not_linked`,
`not_a_maintainer`, budget, kill switch…); a ✅ typed as a reply files as the reaction does and gets the same lines. A
refusal is said once per (draft, user, reason): a re-added ✅ does not repeat it.
