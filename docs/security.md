# Security

The broker is the one place that authorizes room actions, so it is designed to fail safe: **a
compromised broker still cannot rewrite history, create a run, or reach a harness's keys**. The
database enforces append-only storage, only the factory creates runs, and events
are pushed by the sandbox rather than pulled from it. Approvals are oversight, not a boundary: what
a run can do is fixed when it is created, by controls outside the sandbox.

## Threat model

The design's threats, the controls against each, and the package or mechanism that holds the control.

| # | Threat | Controls | Control | Residual |
|---|---|---|---|---|
| T1 | Prompt injection by a human participant | Only members prompt; attribution is immutable; a run cannot exceed its profile; side-effect classes need an approver | Room roles (`internal/policy`), approvals, the broker-stamped `actor` | Wasted budget, bad code; bounded by budgets and the merge gate |
| T2 | Injection from agent to agent (brief, verdict, branch code) | No direct agent-to-agent path; peer text fenced as data; reviewers have read-only forge scope; a verdict has no power beyond the factory's merge gate | The fenced brief (`internal/brief`); room tools are the only agent write path (`internal/mcp`) | An injected reviewer can approve bad code *inside the room*; CI and the merge gate remain |
| T3 | Injection into humans | Approval cards render the raw, redacted action, not the agent's prose, plus the `causedBy` author | The approval card (`internal/humanapi`, `web/`) | Social engineering of an approver |
| T4 | Impersonation | The broker stamps `actor` from the credential; offline JWT checks; a live `AgentRun` watch; re-validated human tokens; a human's ZITADEL user must still be active, admins included; a run joins only its own `roomRef` | `internal/authn`, `internal/runwatch`, `internal/ghidentity` (`CheckActive`) | A token from a replaced pod of a still-live run is accepted until it expires (≤ 600 s); a deactivated user's token, which verifies offline for up to 12 h, until the 5-minute access cache expires |
| T5 | Replay, duplicates | Unique idempotency keys; single-use `approvalId`; `driverEpoch` fencing; short-lived tokens | `internal/store`: the idempotency key, the epoch trigger, the `approvals` table | — |
| T6 | Self-confirmation at the harness | Approvals are oversight by design; consequential actions are enforced outside the sandbox; agent events are untrusted claims | Approvals; the gateway and the forge outside the sandbox | A self-confirmed in-profile action is logged as reported; gateway and forge logs are ground truth |
| T7 | Authorization bypass at the broker | One enforcement point; every action re-checked against database state; `not_permitted` counted and alerted | The policy matrix and its table test (`internal/policy`); `RoomRejectedActionsSpike` | Broker bugs |
| T8 | Transcript leakage | Redaction before append; members only; tailnet host; no transcript in PRs; append-only role; retention | `internal/redact`, the database roles, retention; room membership; the PR carries a room id, not a transcript | Secret shapes gitleaks does not know |
| T9 | Cross-site WebSocket hijacking | `Origin` check; oauth2-proxy cookie `SameSite=Strict` | `internal/humanapi`; oauth2-proxy | — |
| T10 | XSS from LLM output | Markdown rendered with HTML disabled; strict CSP; HttpOnly cookie | `web/`; `internal/humanapi`'s headers; oauth2-proxy | XSS could still act *as* the user through the page |
| T11 | Denial of service | Size and rate limits; byte budgets per connection; authentication before subscription; gateway budgets bound agent loops | Payload and batch limits (`internal/envelope`, `internal/bridgeapi`); connection and rate limits (`internal/humanapi`) | A tailnet member can load the broker |
| T12 | Broker compromise | No harness keys (events are pushed); cannot rewrite history; its own CNP; runs only through the factory API, under a live human's token and budget | The database's grants and triggers ([room log](room-log.md#the-guarantees)); the broker's RBAC | Reads every room; can request runs as a connected human; can decide pending approvals, since the triggers bind each approval row to its events, not each `approval_decided` event to a human's act, and a row check would stop nobody holding the broker's credential. Its room-access reader PAT reads the whole ZITADEL organization, users and their IdP links included (`ORG_OWNER_VIEWER` is org-wide, if read-only), and the factory App's metadata reads answer any login's permission on the App's repositories |
| T13 | Untrusted issue text reaching a prompt | Every text written outside the platform is defused before an agent reads it: invisible and control characters removed, images and raw HTML defused, fence look-alikes replaced, repeated until nothing changes | `internal/factory/sanitize`: a text whose markup does not settle is replaced whole by `Withheld`, failing closed | It is a filter, not a boundary: egress policy and the merge gate's paths are the controls |
| T14 | A forged `Agent-Run` trailer | Trailers are provenance hints, never authorisation; the merge gate binds commits to the agents App by push identity: every push to the branch must be the agents App's in GitHub's repository activity | The merge gate; [`images/agent-harness/README.md`](../images/agent-harness/README.md) | The binding is to the App, not the task: any implementer run can push as the bot. Commits are not signed |
| T15 | Theft or misuse of the merger key | One workload mounts it; the repository's ruleset lets only that App, and a repository admin, bypass a plain merge | The chart's `mergerSecret` (`charts/agent-factory/values.yaml`); the ruleset | Whoever holds the key can merge |
| T16 | Work started on a repository by someone who may not change it, or through another repository's room or branch | A room's runs act on its `spec.repository`, a resumed branch on its last run's; every human run needs `write` on the repository on GitHub, admins included, failing closed; a label counts only from that repository's maintainers with `write`; each repository has its own GitHub connections | `internal/factory/api` (`admitRoom`, `admitResume`, `mayWrite`), `internal/repoaccess`, `internal/factory/intake`; [factory](factory.md#who-may-start-work-on-a-repository) | A permission revoked inside the 5-minute cache |
| T17 | Two factories (a rebuilt cluster on a stale pin, a second cloud) acting on one repository | The repository's default branch names its one factory in `.github/agent-factory.yaml`, a gate path; a factory it does not name takes no intake, schedules or API runs and merges nothing; an unread fence ages out to not owned | `internal/factory/fence`; [factory](factory.md#the-ownership-fence) | Up to 15 minutes of a stale answer while GitHub is unreachable |

## Identities

| Principal | Credential | Validated by the broker | Canonical id |
|---|---|---|---|
| Agent run | A projected ServiceAccount token of `xplane-run-<runId>` in `agents`, audience `room-broker`, 600 s, mounted **only** in the bridge container | Offline against the JWKS of an issuer in `runIssuers`; `sub` must match the issuer's `subPattern`; then the `AgentRun` must be live and name a room | `agent:<runId>` |
| System caller | A ServiceAccount token with audience `rooms-system` (the platform's Kyverno policies reserve every `room-broker*` audience for `agents`) | Offline against `systemIssuer`; `sub` must be a key of the `systemPrincipals` allowlist | e.g. `system:factory` |
| Human | A ZITADEL ID token and a JWT access token, both from oauth2-proxy | Offline: issuer, expiry, groups; `aud` holds the project id and a rooms client, and the token names that client: the ID token in `azp`, the access token in `client_id`, since ZITADEL access tokens carry no `azp`. The two must share the `sub` and the client | `human:<sub>` |

Why offline: every consumer validates issuer-agnostically. A runtime whose identities are
not ServiceAccounts needs one more `runIssuers` entry, not a new code path. Liveness comes from the
`AgentRun` itself (`status.phase` and the `agents.ogenki.io/revoked` annotation): when a run turns
terminal, revoked or deleted, every replica drops its connections on the watch event.

The allowlist of system callers names the factory:
`system:serviceaccount:agent-system:agent-factory: system:factory`.

**Humans.** The `rooms-proxy` ZITADEL client issues JWT access tokens, unlike the platform's other
clients, so that the broker and the factory can validate them offline. oauth2-proxy keeps them in an
HttpOnly, `SameSite=Strict` cookie, away from a page that renders LLM output. Two ZITADEL project
roles, flattened into `groups`, gate access: `agents-admin` (owner and approver everywhere) and
`agents-member` (may create rooms, and watches a room only under room access, below). The broker
reads the two names from its config's `human.groups`, the factory's run API from its config's
`api.groups`, which defaults to the same two. Anyone else is refused
at oauth2-proxy and again by the broker. Revoking a group takes effect within the hour, when
connections re-authenticate.

**Room access.** A room is never more visible than its repository. For a member, the broker
reads the GitHub account linked to their ZITADEL user, never a token claim: a link is added only by
authenticating at GitHub, while user metadata is writable by machine users. It lists the user's IdP
links with a read-only machine user's PAT (`ORG_OWNER_VIEWER`), resolves the GitHub link's numeric
id to the current login, then asks GitHub whether that login can read the room's `repository`, both
through the factory App's metadata reads. Answers are cached for at most 5 minutes, so an unlinked
account or a revoked collaborator loses the room within that; past the cache, a check ZITADEL or
GitHub cannot answer fails closed. A member with no link sees no room. `agents-admin` bypasses it.
The same reader also reads the user's state on every human path, `agents-admin` included, since
tokens verify offline and ZITADEL's live 12 h: a user deactivated, locked or deleted is refused
(`user_inactive`) within the same 5 minutes, and an open WebSocket is cut on its next ping.
When the broker asks the factory for a run on a human's behalf, it forwards that human's access
token, never an asserted `sub`, so the factory proves the principal itself.

## Authorization

The permission matrix, enforced by the broker (`internal/policy`, tested row by row). Room roles are cumulative; the approver flag is independent; the driver is one token.

| Action | watcher | collaborator | approver | driver | owner | agent run | system |
|---|---|---|---|---|---|---|---|
| Read the log, live | ✓ | ✓ | — | ✓ | ✓ | `room_read` | ✓ |
| Chat (delivered to nobody) | — | ✓ | — | ✓ | ✓ | `room_post` | ✓ |
| Queue a message for the next run | — | ✓ | — | ✓ | ✓ | — | ✓ |
| Steer or interrupt the running run | — | — | — | ✓ | — | — | while driver |
| Start the next run | — | — | — | ✓ | ✓ | — | factory |
| Decide an approval | — | — | ✓ | with the flag | ✓ | **never** | `system:policy` |
| Driver token | — | request | — | give | take | — | give; yields to humans |
| Fork | ✓ | ✓ | — | ✓ | ✓ | — | ✓ |
| Invite, change roles, close | — | — | — | — | ✓ | — | ✓ |

- **Never an agent approves**: one injected transcript would otherwise approve another's
  action.
- **Never from `roomctl`**: a token issued to the `roomctl` client cannot decide, steer,
  interrupt or move the driver token, because a local agent could run it. `POST /mcp` takes only
  that client's tokens, so the same holds there, and the text of both is stored as machine-sent.
- **Four-eyes** (off by default, per room): the humans in the triggering turn's `causedBy`
  chain cannot decide it.
- **An invite is two writes**: the Room CR, then the `participant` event. If the append fails after
  the update, the membership briefly has no record; retrying the act with the same `clientSeq`
  heals it (the update is then a no-op, and the append writes or replays the record). The CR
  change is also in the Kubernetes audit log. A sealed room refuses the invite before the update.
- **The broker never creates `AgentRun`s**: its RBAC is `get`, `list`, `watch`, `delete` on
  `agentruns` in `agents`; CRUD on `rooms` in `agent-system`; leases; no cluster-admin.

### A sponsored principal's bounds

A sponsored principal ([factory](factory.md#sponsored-principals)) is a ZITADEL machine user the
config binds to a maintainer. Every bound below is enforced server-side, and none comes from a token
claim except the client that issued it.

| Bound | What holds it |
|---|---|
| Identity | The token's `client_id` equals the principal's `clientIDFile`, read at each use, and its audience holds the project (empty or missing: refused). The client must be a machine client: every subject of it is `system:<name>`. Never the web UI's or `roomctl`'s client |
| Sponsor | A maintainer of the repository, from the config (or the `onBehalfOf` link map), checked at load and again at each request; recorded as `spec.source.sponsor` |
| Day | `dailyUSD`, held by the run-time reservation under `system:<name>`, enforced whatever `enforcePrincipal` says. The intake's check is advisory. A principal removed from the config is no longer held, and its tasks escalate `principal_unknown` |
| Ceiling | `max_cost` is mandatory and at most `maxCostUSD`; the task is capped at it and each run at a third, by the meter |
| Open tasks | `maxOpenTasks`, a soft cap (a burst of N can open cap+N-1) |
| Repositories | Its `repositories` list, exact spelling, and only ones this factory owns |
| Reads | `/mcp` only, four tools, and a room only as one of its `watcher` members |
| Never | `/v1/runs`, `room_queue`, `room_fork`, `rooms_list`, approvals, class, tier or auto-merge. Its text is machine-sent, redacted and stored as untrusted |

## TLS on :8443

The bridge-to-broker hop serves TLS **on both clouds**. gcp-0's Cilium has no WireGuard to
encrypt pod traffic, and one configuration for both clouds is simpler than two. The manifests live in
cloud-native-ref.

| Piece | Setting |
|---|---|
| Certificate | cert-manager `Certificate room-broker-tls` in `agent-system`, issuer `ClusterIssuer openbao` (the platform's private CA) |
| Names | `room-broker.agent-system.svc.cluster.local`, `room-broker.agent-system.svc` |
| Lifetime | `duration: 720h`, `renewBefore: 240h` |
| Broker | Mounts the Secret at `/etc/room-broker/tls/{tls.crt,tls.key}`; `ListenAndServeTLS` with a `GetCertificate` that re-reads the pair when the files change, so a renewal needs no restart. A plain-HTTP request fails |
| CA distribution | An ExternalSecret `room-broker-ca` in `agents` copies the platform CA chain, key `ca.crt` (not from `agents-secrets`: that SecretStore is namespaced to `agent-system` and cannot serve `agents`) |
| Bridge | Mounts `room-broker-ca` read-only at `/etc/room-broker-ca`; loads `$BROKER_CA_FILE` (default `/etc/room-broker-ca/ca.crt`) as its only root. It refuses to start if the file is missing or `BROKER_URL` is not `https://` |
| Operators | Call `:8443` with `https://` and `--cacert` on the same CA |

`:8080` stays behind oauth2-proxy and `:8090` behind the `agent-router` Gateway, unchanged.

## Redaction

Every payload is redacted **in the broker, before it is appended**: every string of the JSON
document, at any depth, object keys included: an env dump puts secrets in keys. NUL characters
are stripped first, since `jsonb` refuses them. Two keys that are one once redacted cannot keep
both values, so that item is stored as a `key_collision` stub. It uses gitleaks' `detect` package with its
default rule set. A match becomes `[REDACTED:<rule>]`, the rule id is listed in the event's
`redactions`, and `rooms_redactions_total{rule}` counts it. Broker logs carry envelope metadata only,
never payloads (`internal/redact`).

A unit test pins the four rules the design names:

| Rule | Catches |
|---|---|
| `github-app-token` | `ghs_…` installation tokens, as octo-sts mints them |
| `github-pat` | `ghp_…` personal access tokens |
| `jwt` | ServiceAccount and ZITADEL tokens |
| `private-key` | PEM private keys |

**Limits.** Treat redaction as a safety net, not a guarantee.

| Limit | Consequence |
|---|---|
| Each string is scanned alone | A secret split across two fields, two array items or two events is not detected. Writers must not split tokens |
| Only known shapes | A secret gitleaks has no rule for is stored as written (T8's residual) |
| After the fact for the harness | The harness saw the secret; redaction protects the log and its readers, not the run |
| An oversize payload | Replaced by a stub, so its content never reaches the log at all |
| Broker-origin text from a claim | The one free text, a revocation annotation used as an end reason, is redacted like a payload, then cut to 64 bytes |
| gitleaks' `gitleaks:allow` marker | Ignored: a line carrying it is redacted like any other, because the marker is harness content too |

Human actions run every payload they append through the same redactor, a take's reason included,
and a queued message's row keeps the redacted text the next run's brief quotes. The action on an
approval card is redacted the same way. The harness also redacts GitHub tokens from its own step log
before printing.

## Database roles

| Role | Can | Cannot |
|---|---|---|
| `rooms_owner` | Own and migrate the schema | Create roles |
| `rooms_broker` | Append and read events; insert room rows; update only the `rooms` columns it must move | Update or delete events; unseal, re-date or re-time a room; move `last_seq` by anything but +1 |
| `rooms_retention` | Find sealed rooms closed past their retention, then delete their events and rows | Read any event column but `room_id`, or any `rooms` column but the four that decide expiry (column grants); see or delete a room that is not expired, or its events (row-level security) |

Details and the tests that prove them: [room log](room-log.md#the-guarantees).

## Network

These policies live in cloud-native-ref. Default deny everywhere; one allow per flow.

| Endpoint | Ingress | Egress |
|---|---|---|
| `room-broker` | Run pods in `agents` (label `agents.ogenki.io/run-id`) and the factory on 8443; oauth2-proxy on 8080; `agent-router` proxies on 8090; `vmagent` and kubelet on 9090 | DNS with an L7 rule; the Kubernetes API; CNPG on 5432; the run issuer's JWKS host on 443; the identity provider; `api.github.com` on 443; the factory's run API on 8082 |
| Run pod | kubelet on the bridge's 8085 | The broker on 8443, only when `roomRef` is set (the run's own CNP) |
| CNPG `xplane-rooms` | The broker and the retention job on 5432; the Atlas and CNPG operators; `vmagent` on 9187 | DNS, the Kubernetes API, peers, the backup plugin, object storage |
| Retention job | None | DNS; CNPG on 5432 |

On gcp-0 the identity-provider rule is `toEntities: [all]` without ports: a port-scoped rule to the
cluster's own gateway hairpins through the socket load balancer and is dropped.

## Internal principals and publishing

An internal principal is an authentication surface: a token whose client is declared in
`api.internalPrincipals` is a sponsored principal that may also carry internal data, and nothing else.
`POST /v1/runs` refuses it as it refuses any sponsored token. Its request refuses a `title`, an
`onBehalfOf`, a `ref`, public data, a repository or ceiling outside its entry, and a body past its
`minConfidence`, `dailyTasks`, `maxOpenTasks` and day. The investigate template is forced by the
principal, so alert text it sends reaches a public implementer only through a maintainer. A sponsored
principal is never allowed `internal`, and its request refuses an `incident`.

`factory/publish` is the one place internal text becomes public, and a maintainer's label starts it.
The proposal is the triager run's own handoff, never another writer's. It is held to the issue
template, made inert (`sanitize.Public`: no link, raw HTML, image or hidden character survives; URLs
and addresses are kept in code spans), and scanned by the redactor; a hit publishes nothing. The
factory files it once, with no label, and records it on the task. An origin is that record, never
issue text.

## Webhooks

An event carries ids and closed codes only, never room text: `reason`, `limit` and `state` are
clipped to fixed lists, and any other value is sent as `other`. A message is never decoded to build
an event.

A subscriber is `cluster` or `internet`, declared in the broker's config and never by a room. Cluster
subscribers are reached by their service name, which needs an egress allow on the broker's host
policy and the subscriber's own ingress allow: neither is implied. An internet
subscriber must be a public DNS name over `https`, and the dial guard checks every address after
resolution, so a name that rebinds is refused at connect. The guard refuses loopback, private,
link-local (the cloud metadata address included), CGNAT (the tailnet), unique-local, unspecified,
multicast and reserved addresses, and the NAT64 and 6to4 forms that embed an IPv4 address. No proxy
is used, which would dial on the broker's behalf past the guard, and redirects are not followed,
since the address they name was never declared.

Each delivery is signed with HMAC-SHA256 under a key of at least 32 bytes that lives in a mounted
secret, never in the config. The key file is re-read at each delivery.

## The Slack App

The factory's Slack App (`cmd/factory-slack-app`, [docs/slack.md](slack.md)) is the only component with a public route: `POST /slack/events`, matched exactly. The invariants, each held by a named test:

| # | Invariant | Test |
|---|---|---|
| 5 | A bad Slack signature or a stale timestamp is `401` before the body is parsed. The signature is `v0=` + HMAC-SHA256 over `v0:<timestamp>:<raw body>`, compared in constant time, within 5 minutes, on a body capped at 1 MiB | `TestSlackSignature` |
| 6 | A check mark from an unlinked user or a non-maintainer files nothing | `TestReactionFlow` |
| 7 | No Slack message carries room text: thread lines are templates of ids, states, closed codes and the room's URL | `TestThreadMessagesCarryNoText` |
| 8 | No tool or Slack path approves or steers; the App reads no reply in a thread | `TestSponsoredToolList`, `TestSlackNeverDecides` |

- **One workspace.** `teamID` is required; an event from any other workspace is dropped.
- **A check mark files only when** the reacting user is linked in the factory's `onBehalfOf.slack`, the message is the bot's own draft (its user is the bot's, and its metadata hash matches), and the channel is configured. A second check mark on the same draft is a duplicate and files nothing.
- **Secrets stay in the App.** The bot token and the client-credentials secret are mounted files, read at use. Neither reaches the broker or a run: the broker gets a ZITADEL access token, never Slack's, and a run never sees either.
- **The App's token** is a client-credentials grant with the project-audience scope, the same scope `roomctl` uses. The factory proves the principal itself from that token and the `on_behalf_of` it names.
- **A key problem is not a bad signature.** A webhook key the App cannot read answers `503 no_key`, counted apart from `bad_signature`. It is not a hold: the broker retries after 10s, 1m and 5m, then counts a failure, and three given-up deliveries disable the subscriber until its `generation` is bumped, so events are lost meanwhile ([recovery](slack.md#state)).
- **Redelivery.** Slack events and broker webhooks are deduplicated in memory (one replica). Two concurrent redeliveries of one id can both pass the check before either finishes; the worst case is one duplicate thread line.

## roomctl on the developer's machine

`roomctl mcp` and `roomctl checkout` run as the developer, beside a local coding agent that room
text and a branch can try to steer ([roomctl](roomctl.md#the-mcp-server)).

- **The start form.** The factory starts only from a form field that defaults to no, in `issue_file`.
  The model can call the tool but cannot answer the form. The factory still takes only a
  maintainer's label, and the per-task, per-person and daily dollar caps bound the spend.
  **Accepted risk:** a client that auto-accepts forms (some headless modes) lets the agent start
  work. That is not new: the agent can already label with the developer's `gh`, which local-first
  calls "advice, not a control".
- **Checking out an agent's branch (R15).** A sandboxed agent can commit agent or editor config,
  hooks or mise files that run when a tool opens the folder or when the developer commits.
  `room_checkout` and `roomctl checkout` therefore fetch only `agent/<room>` from an `origin`
  verified to be the room's repository, run git with an argument list and `core.hooksPath=/dev/null`
  (no hook runs during the fetch or the worktree creation), confine the new worktree beside the
  repository, refuse an existing path, and confirm the changed files that run code, in a form that
  defaults to no or with `y/N` on a terminal. Without either they refuse, naming the files.
  `roomctl checkout` asks on a terminal only. An agent whose shell runs in a pseudo-terminal can
  answer `y` itself; that agent can also run `git worktree add` directly, so the question is
  advice, not a control.
  The residual: a developer who confirms, then opens the folder or commits there, runs what the
  agent wrote. Read the listed files first.

## Supply chain

| Control | How |
|---|---|
| Pinned actions | Every `uses:` is pinned to a 40-character commit SHA, with the version in a comment; Dependabot keeps them current |
| Least privilege in CI | `permissions: {}` at the top of every workflow, granted per job; `persist-credentials: false` on every checkout; timeouts on every job |
| Static analysis | CodeQL (`security-extended`, Go) on every PR, on `main` and weekly; `golangci-lint` with `gosec`, `noctx`, `errorlint`, `bodyclose`, `sqlclosecheck` and more ([AGENTS.md](../AGENTS.md)); no rule disabled in production code, while tests and `cmd/` carry scoped exclusions |
| Vulnerabilities | `govulncheck` in `task check`, so on every PR and before every release |
| Dependency drift | `go mod tidy -diff` in `task check`. Dependabot for actions, Go modules and the Docker base images (the harness's `agent-server` base excepted): weekly, a 7-day cooldown, majors ignored while the stack is open. The harness's Python pins (`requirements.in` and its compiled lock) have no pip ecosystem entry, so version bumps are by hand; security updates still arrive as Dependabot pip PRs (repository vulnerability alerts are on), and the base image is held back by its ignore in `.github/dependabot.yml` |
| Scorecard | OpenSSF Scorecard weekly on `main`, results published and uploaded to code scanning |
| Images | The Go images are static binaries on digest-pinned distroless `static-debian12:nonroot`; the agent harness is a Python image on the OpenHands `agent-server` base. All multi-arch (`linux/amd64`, `linux/arm64`). Never `latest`: consumers pin by digest |
| Attestations | An SPDX SBOM and SLSA provenance (`mode=max`) attached to every pushed image |
| Signing | A keyless cosign signature on the digest, never the tag, from the workflow's OIDC identity |
| Releases | The release job re-runs `task check` (a tag can name a commit that never passed CI) and uses no build cache |
| Repository | `main` is protected by the repository ruleset `main`: pull requests only (squash, the only merge method the repository enables), required checks `check`, `analyze` and `harness-test`, linear history, no force-push or deletion; admins may bypass, through a pull request only. Private vulnerability reporting and Dependabot security updates are on |

Verify an image before you pin it. **The default accepts only images built by the release workflow
from a `v*` tag:**

```bash
IMAGE=ghcr.io/smana/room-broker@sha256:<digest>

cosign verify "$IMAGE" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/Smana/agent-platform/\.github/workflows/release\.yaml@refs/tags/v'

docker buildx imagetools inspect "$IMAGE" --format '{{ json .SBOM }}'        # SPDX SBOM per platform
docker buildx imagetools inspect "$IMAGE" --format '{{ json .Provenance }}'  # SLSA provenance
```

**PR pre-releases are not release-grade: they run unreviewed branch code. Use them for integration
testing only.** To verify one, accept the PR workflow instead:

```bash
cosign verify "$IMAGE" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/Smana/agent-platform/\.github/workflows/ci\.yaml@'
```

## Reporting a vulnerability

Privately, through [GitHub security advisories](https://github.com/Smana/agent-platform/security/advisories/new),
never a public issue. See [SECURITY.md](../SECURITY.md).
