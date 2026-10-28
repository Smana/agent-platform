# API

The broker serves four listeners. Every body is JSON.

| Port | Who calls it | Authentication |
|---|---|---|
| `:8443` (TLS) | The bridge (container `room-bridge`, an identity that keeps its pre-maku name, see [naming](naming.md#technical-names)) in each run pod; system callers such as the factory | Offline JWT: run tokens (audience `room-broker`) or system tokens (audience `rooms-system`) |
| `:9090` | kubelet, `vmagent` | None: probes and metrics only |
| `:8080` | Humans, through oauth2-proxy | ZITADEL ID token and access token |
| `:8090` | Agents' `room_*` tools, through the `agent-router` Gateway only | Injected key plus the gateway's verified `x-ar-agent` |
| `:8085` (bridge) | kubelet | None |

`maku-broker serve` serves `:8443`, `:9090`, `:8080` and `:8090`.

## `:8443` — bridge and system API

Served with `ListenAndServeTLS` on the pair cert-manager writes to `/etc/room-broker/tls/`. The
broker re-reads the files when they change, so a renewal needs no restart, and a plain-HTTP request
fails. See [security](security.md#tls-on-8443) for the certificate and its CA.

Errors are JSON, `{"error": "<reason>"}`, with the status codes below. The reasons are stable
strings, the `wire.Reason*` constants: branch on them, not on the status text.

`hello`, the events endpoint and both system endpoints limit each principal to 10 requests a second (burst
20) and 10 in flight, the per-principal numbers. Over either, they answer `429 rate_limited` with
`Retry-After: 1`, and the caller retries.

### Authentication

| Caller | Presents | The broker checks |
|---|---|---|
| A run's bridge | `Authorization: Bearer <token>`: a projected ServiceAccount token, audience `room-broker`, 600 s, re-read from its file before every request | Signature against the JWKS of an issuer in `runIssuers`; audience; expiry; `sub` matches the issuer's `subPattern`, which yields the run id. Then the run must be live in the `AgentRun` watch (not terminal, not revoked) and have a `roomRef`. The room is the run's `roomRef`, never a request parameter |
| A system caller | `Authorization: Bearer <token>`, audience `rooms-system` | Signature against `systemIssuer`; audience; expiry; `sub` must be a key of `systemPrincipals`, whose value becomes the principal, e.g. `system:factory` |

`systemIssuer` is optional: only the factory calls these routes, and a broker without one still
builds a system authenticator that never authenticates, so the system routes answer `401` rather
than disappearing. It takes `mode: jwks` (the default, verifying against `issuer` and `jwksURL`) or
`mode: tokenreview`, which asks the apiserver and needs no reachable issuer. `systemPrincipals`
requires it: without an issuer no system caller could ever be vouched for, and the config is
refused ([integration](integration.md#the-brokers-configuration)).

No call reaches Kubernetes' TokenReview unless an issuer says `mode: tokenreview`: every other token
is validated offline and issuer-agnostically. A run
that ends, is revoked or is deleted has its streams cut on the watch event.

### Endpoints

| Method and path | Caller | Does |
|---|---|---|
| `POST /v1/bridge/hello` | Bridge | Claims the room's bridge lease, returns where the log is |
| `POST /v1/bridge/events` | Bridge | Appends a batch of harness items |
| `GET /v1/bridge/stream` | Bridge | One SSE stream down: pings; `deliver`, `interrupt` and `decision` |
| `POST /v1/bridge/approvals` | Bridge | Asks for a human decision on a pending action |
| `GET /v1/rooms/{id}/events` | System | Reads a room's log |
| `POST /v1/rooms/{id}/messages` | System | Appends `message{kind: task_state}` |
| `POST /v1/rooms/{id}/task` | System | Appends the task's facts, `state_changed{kind: task}` |
| `POST /v1/rooms/{id}/queue` | System | Queues a chat message for the room's next run |
| `GET /v1/rooms/{id}/queue` | System | Lists the messages still queued |
| `POST /v1/rooms/{id}/queue/consume` | System | Marks queued messages consumed by a run |

### `POST /v1/bridge/hello`

No body. Response `200`:

```json
{"roomId": "3kq7x2ma", "afterHarnessSeq": 36, "afterStatusSeq": 4, "approvals": {"profile": ""}}
```

`afterHarnessSeq` and `afterStatusSeq` are the highest keys already stored for this run's two
streams; the bridge resumes after them. `approvals` carries the room's approval profile
and is empty when no approval profile is set.

| Status | `error` | When |
|---|---|---|
| `401` | `unauthenticated` | No token, a bad signature, a wrong audience or issuer, an expired token, a `sub` that names no run |
| `403` | `run_not_live` | The run is terminal, revoked, deleted, or not yet in the watch |
| `403` | `run_has_no_room` | The run has no `roomRef` |
| `429` | `rate_limited` | Over the run's limits, shared with its batches |
| `409` | `room_busy` | Another run holds the room's lease, and its run is live. The broker also appends `state_changed{kind: limit, reason: concurrent_run}` |
| `503` | `no_room` | The room's row does not exist yet: its `Room` has not been reconciled |
| `503` | `log_unavailable` | The database is unreachable |

### `POST /v1/bridge/events`

Request, at most 2 MiB; the bridge sends up to 100 items:

```json
{"items": [
  {"stream": "events", "seq": 36, "type": "tool_call",
   "payload": {"callId": "call_42", "tool": "terminal", "args": {"command": "ls docs"}, "risk": "LOW", "decidedBy": null}},
  {"stream": "status", "seq": 5, "type": "state_changed",
   "payload": {"kind": "harness_status", "status": "finished", "previous": "running"}}
]}
```

`stream` is `events` (harness events, idempotency scope `agent:<runId>`) or `status` (status
transitions, scope `agent:<runId>:status`); `seq` is the item's key in that scope, greater than 0.
Each payload is redacted, object keys included, then checked against the bridge allowlist: the
check reads the redacted payload, the one stored. The whole batch is checked before anything is
written, so a refused item leaves nothing behind. A payload's keys must each have one spelling:
Go readers fold case (`Delivery`, `ſtatus` and a Kelvin-sign `K` all match), jsonb readers do not, so
a key that folds onto another or onto a field of the type's envelope struct without being spelled as
it is refused. A `message` is stored as its envelope struct re-marshals it; a chat's `verdict` and
`commit` are dropped. An item whose keys are one once redacted (two tokens as keys of an env dump)
cannot keep either value: that item alone is stored as a `{"refused": true, "type": …, "reason": "key_collision"}` stub and the
rest of the batch is appended.

Response `200`: `{"afterHarnessSeq": 36, "afterStatusSeq": 5}`, the highest key of the batch on each
stream, or `0` for a stream the batch did not carry. A replayed key is acknowledged without
appending again. An empty batch, `{"items":[]}`, is the bridge's heartbeat: it renews the lease
and nothing else, and the bridge sends one when it has pushed nothing for 30 s.

| Status | `error` | When | The bridge then |
|---|---|---|---|
| `400` | `bad_batch` | Not JSON, an unknown field, or data after the batch | Never drops it: halves the batch until the refused item is alone, then keeps its slot with a `state_changed{harness_event, harnessKind: refused}` stub naming the broker's reason. If even the stub is refused, it retries with backoff and logs at Error |
| `400` | `bad_item` | Unknown type or stream, `seq` ≤ 0, a type or `state_changed` kind a bridge may not push ([allowlist](event-envelope.md#state_changed-kinds)), or a key spelled two ways | As for `bad_batch` |
| `400` | `bad_payload` | The payload is not a JSON object | As for `bad_batch` |
| `413` | `batch_too_large` | Over 2 MiB or over 500 items | Halves the batch; a lone item still too large keeps its slot with a size stub |
| `401` | `unauthenticated` | As for `hello` | Re-reads its token and retries |
| `403` | `run_not_live`, `run_has_no_room` | As for `hello` | Retries on the next tick |
| `409` | `lease_lost` | This run no longer holds the room's lease, seen by the lease renewal or by the append's fence. Nothing is appended | Must not drop the batch: the events are not in the log. Keeps it and says hello again |
| `410` | `sealed` | The room is sealed | Stops mirroring |
| `429` | `rate_limited` | Over the run's request rate or requests in flight | Must not drop the batch: retries after `Retry-After` |
| `503` | `log_unavailable`, `timed_out` | The database refused the append, or the request's 30 s ran out before the batch was redacted | Retries; keeps buffering |

A payload Postgres refuses outright (SQLSTATE class 22) is not an error: it is stored as a
`{"refused": true, "type": …, "reason": "invalid_value"}` stub so the cursor moves on.

### `GET /v1/bridge/stream`

A Server-Sent Events stream the bridge opens and holds (sandbox-initiated, no WebSocket).
Response `200`, `Content-Type: text/event-stream`. The broker writes a comment line, `: ping`, every
30 s. The stream ends at the token's expiry, and the bridge re-dials with a fresh token. A stream
carries only the events addressed to its own run.

Deliveries are derived from the log, never held in a replica's memory. On every connect the broker
subscribes to the room, reads the run's last acknowledgement (the highest `ref` of its `delivered`,
`interrupted` and `undeliverable` acks that is a delivery of that run), and sends the run's
deliveries after it before anything else, in the stream's first flush; then it follows the room live.
Both reads go through partial indexes, never the room's whole log. A stream whose acknowledgement it
cannot read is refused `503`, rather than replay every delivery from the start. An event's `data`
lines are bounded to 256 KiB together.

| Outcome of an injection | The bridge |
|---|---|
| Accepted by the harness | Acknowledges `delivered` or `interrupted` |
| Refused for good: steering with a 4xx other than 404, 408, 409, 425, 429; an interrupt with any 4xx | Acknowledges `undeliverable` with the harness's `code`, and goes on: one refusal must not block every later `ref`. An interrupt of an idle conversation is moot, and a late one would land on a turn the driver never meant |
| Failed for now (no answer, 5xx, or steering refused with one of those 4xx) | Ends the stream; the replay hands the same `ref` over again before any later one |

**Delivery is at least once** (a crashed bridge re-delivers). A bridge injects each `ref`
once per process, but one that restarts, or shuts down while a `deliver` is in flight, after
injecting a `ref` and before its acknowledgement reaches the log, injects it again: the transcript
shows a second user message. A replayed `interrupt` lands on whatever turn is current then, which can
be a later turn than the one the driver meant.

Acknowledgements are the bridge's own claims: the broker cannot see inside the
sandbox. A `ref` counts only if it is a delivery of that run, so a forged one past every delivery
skips nothing; a compromised sandbox can still hide steering from its own run, as it could by ignoring
the harness. The log may then show `delivered` for a message never injected.

| SSE `event` | `data` | Meaning |
|---|---|---|
| `deliver` | `{"ref": 1846, "text": "…"}` | A steering message; the bridge injects it into the harness and acknowledges it as `state_changed{delivered, ref}` |
| `interrupt` | `{"ref": 1847}` | The driver interrupted the run; acknowledged as `state_changed{interrupted, ref}` |
| `decision` | `{"approvalId": "…", "allow": true, "reason": "…", "ref": 1851}` | An approval was decided; acknowledged as `state_changed{decision_applied, ref}` |

Errors before the stream opens are those of `hello`.

### `POST /v1/bridge/approvals`

Request, at most 64 KiB: `{"eventId": "e42", "callId": "call_97", "class": "forge.pr", "action": {…}}`.
`eventId` is the harness ActionEvent's id. `class` is one of `forge.push`, `forge.pr`, `forge.other`,
`mcp.write`, `shell.high`. The action is redacted and stored as the approval card shows it.
Response `200`: `{"approvalId": "…", "expiresAt": "…"}`, with `expiresAt` always set: 30 minutes for
an `attended` room, the room's `approvals.ttl` (default `4h`) for an `unattended` one. The request is
idempotent per `(room, run, eventId)`, never per `callId`: a model provider may reuse a tool call id,
and the bridge rejects a step whose ack names an approval it already spent. The request is fenced on
the room's bridge lease, like the run's other appends, and recorded as `approval_requested`. It names
the run's prompters for four-eyes: whoever requested the run (and its `spec.principal`), steered it,
or wrote a queued message its brief consumed or that was promoted to it.

An approval whose `callId` already has a `tool_result` in the log after its request (the harness ran
or rejected the call), or whose run has left the room since (the broker's `participant{left}`), is
closed as `superseded`, never left for an approver: by the leader's sweep every 30 s, or at once when
an approver decides it. A superseded decision is never sent to the
bridge. An expiry is decided `allow: false` with `reason: "expired"`: at the first decision attempt past `expiresAt`, else by the sweep within 30 s of it.

| Status | `error` | When |
|---|---|---|
| `400` | `bad_approval` | Not JSON, an unknown field, no `eventId` or `callId` (or one over 256 bytes), or an unknown class |
| `400` | `bad_action` | The action is not a JSON object, its keys collide once redacted, or it is over 63 KiB redacted |
| `401`, `403` | as for `hello` | |
| `409` | `lease_lost` | Another run holds the room's bridge lease |
| `410` | `sealed` | The room is sealed |
| `429` | `rate_limited` | Over the run's limits |
| `503` | `log_unavailable`, `timed_out` | The log or the run's prompters could not be read or written |

### `GET /v1/rooms/{id}/events`

For system callers. Query: `afterSeq` (default `0`), `limit` (1 to 500, default `100`). Response `200`:

```json
{"events": [ {"v": 1, "seq": 1, "type": "state_changed", "…": "…"} ], "lastSeq": 1842}
```

`events` are full [C4 envelopes](event-envelope.md) with `seq > afterSeq`, in order; `lastSeq` is
the room's current high-water mark. Page by passing the last `seq` you received as `afterSeq`.

| Status | `error` | When |
|---|---|---|
| `400` | `bad_room` | `{id}` is not a C2 id |
| `401` | `unauthenticated` | Bad, expired or wrong-audience token |
| `403` | `not_permitted` | A valid token whose `sub` is not in `systemPrincipals` |
| `404` | `no_room` | No such room in the log |
| `429` | `rate_limited` | Over the principal's limits |
| `503` | `log_unavailable` | The database is unreachable. Never a `lastSeq` of 0 in its place |

### `POST /v1/rooms/{id}/messages`

For system callers; appends the reserved kind the factory owns. Request, at most 32 KiB:

```json
{"kind": "task_state", "text": "Reviewing", "clientSeq": 1}
```

`kind` must be `task_state`, `clientSeq` greater than 0, `text` not blank and at most 16 KiB; the text is redacted.
Response `201`: `{"seq": 1843}`.

The idempotency key is `(principal, clientSeq)` alone: a replay answers `200` with the original
`seq`, **even when its body differs**. The new body is not stored and no error says so, so a caller
never reuses a `clientSeq` for another message.

| Status | `error` | When |
|---|---|---|
| `400` | `bad_room`, `bad_message` | A bad id; any other kind, `clientSeq` ≤ 0, text over 16 KiB, or a malformed body |
| `401`, `403` | `unauthenticated`, `not_permitted` | As for reads |
| `404` | `no_room` | No such room |
| `410` | `sealed` | The room is sealed |
| `429` | `rate_limited` | Over the principal's limits |
| `503` | `log_unavailable`, `timed_out` | The database refused it, or the request ran out of time |

### `POST /v1/rooms/{id}/task`

For system callers: the factory's facts about the room's task, stored as
[`state_changed{kind: task}`](event-envelope.md#state_changed-kinds) so the
[summary](#get-apiroomsidsummary) reads the room log alone. Request, at most 32 KiB:

```json
{"clientSeq": 7, "facts": {"phase": "Implementing", "run": {"id": "cf4ato2x", "role": "implementer"},
  "budget": {"usedTokens": 189093, "limitTokens": 1500000},
  "issue": {"number": 42, "url": "https://github.com/Smana/cloud-native-ref/issues/42"}}}
```

`facts.phase` is required, token counts are not negative, and `issue.url` and `pr.url` are
github.com issue and pull request URLs. The facts are redacted. Response `201`: `{"seq": 1845}`.
Replays are keyed as for `/messages`, `(principal, clientSeq)`, but under the origin
`<principal>:task`: the two `clientSeq` spaces never collide. Refusals as for `/messages`; facts
that do not validate are `400 bad_message`.

A broker older than the route (v0.7) answers a plain-text `404`. The factory takes that as a wait,
warns once, and keeps the facts due until the broker serves the route: upgrade the broker first.

### The queue routes

For system callers: the factory turns a maintainer's GitHub review into a queued
message, and consumes it once a run's brief quotes it. Every route answers `501 no_queue` on a broker
built without a queue store, and otherwise refuses as the routes above do: `400 bad_room`,
`401`, `403`, `404 no_room`, `410 sealed`, `429`, `503 log_unavailable`.

`POST /v1/rooms/{id}/queue`, at most 32 KiB:

```json
{"text": "GitHub review by @Smana: use the relative link", "clientSeq": 901, "stream": "review"}
```

The text is redacted and stored as `message{kind: chat, delivery: queued}`, the caller its actor and
`<principal>:queue:<stream>` its origin. `stream` is `[a-z]{1,16}`, default `default`: each source
keeps its own `clientSeq` space, apart from the caller's `task_state` messages. Response `201`:
`{"seq": 1844}`. Replaying the stream's highest `clientSeq` answers `200 {"duplicate": true}`.
A lower `clientSeq` need not be a replay, since GitHub review ids follow creation order, not
submission order. It is stored as a new message, unless its exact key is already stored: then the
answer is `201` with the stored `seq`. Two identical requests racing can both answer `201` with
the same `seq`. Either way, nothing is stored twice.

`GET /v1/rooms/{id}/queue` answers `200 {"queued": [{"ref": 1844, "author": "system:factory",
"text": "…"}]}`, oldest first; `ref` is the queued message's `seq`.

`POST /v1/rooms/{id}/queue/consume` `{"refs": [1844], "runId": "7f3cq2xz"}` moves each ref still
queued to `consumed` by that run and answers `200 {"consumed": n}`, the refs this call moved. A ref
already consumed, promoted, removed or never queued is skipped, so a retry is safe.

| Status | `error` | When |
|---|---|---|
| `400` | `bad_message` | No text, text over 16 KiB, `clientSeq` ≤ 0, or a malformed body |
| `400` | `bad_stream` | A `stream` that is not `[a-z]{1,16}` |
| `400` | `bad_consume` | A `runId` that is not a C2 id, more than 100 refs, or a malformed body |
| `501` | `no_queue` | No queue store wired |

## `:9090` — probes and metrics

| Path | Answers `200` when | Used as |
|---|---|---|
| `GET /healthz` | The process is up. Body: `ok <version>` | Liveness |
| `GET /readyz` | Postgres answers a ping and the Kubernetes caches are synced | Readiness |
| `GET /startupz` | The schema is migrated (`events` exists) | Startup: the first deploy waits for CNPG and the Atlas migration |
| `GET /metrics` | Always | Prometheus metrics ([operations](operations.md#metrics)) |

The bridge serves `GET /healthz` on `:8085`, for kubelet only, and no metrics. It reports unhealthy only when the
harness answered once and has been unreachable for more than 60 s: as a native sidecar its startup
probe gates the harness container, so it must never wait for the harness. It never
checks the broker, so a broker outage cannot mark sandboxes unready.

It also serves `GET /admission`, read by `maku-bridge gate` on loopback:

| Status | Body | When |
|---|---|---|
| `503` | `pending` | No hello has been decided yet: the broker is unreachable, or another run's live run has held the room for less than 3 minutes |
| `200` | `admitted` | The bridge holds the room's lease. The gate exits 0 and the harness starts |
| `409` | `room_busy` or `sealed` | The run will never hold the room. The gate exits 1, which fails the pod before the harness runs |
| `403` | `loopback only` | The request came from outside the pod |

## `:8080` — human API

Reached only through oauth2-proxy on `rooms.<private domain>`. Every request carries the human's
ZITADEL **ID token** in `Authorization` and their **JWT access token** in `X-Forwarded-Access-Token`,
both from oauth2-proxy.

| Check | Rule |
|---|---|
| ID token | Issuer is the identity provider, audience holds the `rooms-proxy` client id, not expired |
| Access token | Same `sub`; issued for `rooms-proxy`. A web session requires it to differ from the ID token |
| Groups | `agents-admin` or `agents-member`, else `403` |
| User state | The ZITADEL user must be active, admins included: an inactive, locked, deleted or initial user is refused `401 user_inactive` on every route, and an open WebSocket closes `1008 user_inactive`. Read with the room-access reader, cached with its answers for at most 5 minutes; past that, a state ZITADEL cannot give fails closed (`503 access_unverified`). Counted in `rooms_rejected_actions_total{reason="user_inactive"}` |
| Room access | `agents-admin` sees every room. Anyone else sees a room only if GitHub lets the login linked to their ZITADEL user read the room's `repository`; a room with none is admins-only. Answers are cached for at most 5 minutes; past that, a check ZITADEL or GitHub cannot answer fails closed. A room you may not see answers exactly like a missing one |
| `Origin` | Must match the room host: no cross-site WebSocket |
| Lifetime | A connection lasts `min(token expiry, 1 h)`, then closes for re-authentication |

`maku` presents a bearer token from its own native ZITADEL client; oauth2-proxy lets it
through with `skip-jwt-bearer-tokens`. Such a token can read, post, queue and fork, but the broker
refuses `decide`, `message{steering}`, `interrupt` and every `driver_*` action from it. Its text is
[machine-sent](#machine-sent-text).

Errors before a WebSocket upgrade are plain-text HTTP errors.

| Method and path | Does |
|---|---|
| `GET /`, `GET /r/{id}`, `GET /assets/{file...}` | The embedded UI, under a strict Content Security Policy. `/assets/` serves nested files (`/assets/svg/…`, `/assets/fonts/…`); a directory, `/assets` or `/assets/` is a `404`, never a listing. A room's page keeps its newest 5 000 events; older ones leave the page, never the log. A `401` from the API, or one behind a refused WebSocket upgrade, sends the page to `/oauth2/start?rd=<the page>` |
| `GET /api/rooms` | One row per room the caller may read (room access, then their role): id, `url` (the room's page), phase, `state` (the [coarse state](#the-coarse-state)), owner, driver, data class, last `seq`, `lastEventAt` (when the log last moved; omitted when the room has no events or the time could not be read), `repository`, the task (`issue`, `title`, `task` phase, `pr`; absent when the room has none), `reason` (the task's wait code from its last facts, while its phase is `Queued`, `AwaitingCI`, `AwaitingHuman` or `Escalated`; absent otherwise or when it cannot be read), `needsYou` (how many things wait for the caller), `needsMe` (`needsYou` > 0: an approval is pending that the caller could decide in the web UI, or the factory's task is held on the room and the caller is its driver or owner — the summary's `kind: hold`, or a PR waits for the caller's merge — `kind: merge`, or an Escalated task waits for its issue's author and the caller owns the room — `kind: answer`; an approval past its deadline that the sweep has not decided yet counts for neither `needsYou` nor `need`, as the summary drops it), `need` (the first of those things, in the summary's order: the pending approval with the lowest id, then a hold, then a merge, then an answer; the same `kind`, `id`, `what`, `deadline?` and `url` as a `needsYou` item; absent when nothing waits for the caller), and the caller's own role. A room whose access cannot be verified is left out, with no error. The `X-Rooms-Access` header says why a list may be short, about the caller only: `unlinked` (not an admin, and no GitHub link on their ZITADEL user), `unverified` (a check failed past the cache, or none is configured), else `ok`. Filters, applied after the access and role checks so none reveals an unreadable room: `?repo=owner/name`; `?mine=1`, rooms whose task's issue the caller filed or labelled or whose PR they authored or review, matched on their linked GitHub login; `?needs_me=1`, rows with `needsMe` |
| `GET /api/rooms/{id}/summary` | The room's [`summary/v1`](#get-apiroomsidsummary): status, what needs the caller, what they can do, the agents' notes |
| `GET /v1/ws?room=<id>` | The live room, over WebSocket |
| `POST /mcp` | The [MCP server](#post-mcp) `maku mcp` relays, when `human.mcp.enabled`; else `404` |
| `POST /api/rooms` | `{"dataClass": "public", "repository": "Smana/cloud-native-ref"}` → `201 {"id": "…"}`: a new room owned and driven by the caller, any agents member, on a `repository` they can read (room access). `400` for another data class, a missing or malformed repository, an unknown field or a principal the Room CRD would refuse, `404` for a repository the caller cannot read or the factory App cannot see (missing, or the App not installed), `409` when a Room of the generated id exists, `429` past the caller's action budget (10/s, burst 20, shared with acts), `500` when the Room is refused for any other reason, such as the broker lacking `create` on rooms (logged at WARN), `503` when the API server is unreachable, overloaded or times out, or `access_unverified` when the caller's access cannot be checked. The `SameSite=Strict` cookie and the `Origin` check stop a cross-site post |
| `GET /.well-known/roomctl` | The same `{url, issuer, clientID, projectID, version, humanMode}` with no sign-in, for `maku configure --url`; none of it is a secret. `humanMode` is `oidc` or `local` (`local`: no IdP, no token, and the `maku` values are empty). The proxy in front must let this path through unauthenticated |
| `GET /api/roomctl` | `{url, issuer, clientID, projectID, version, humanMode}` for the room list's CLI setup view, any agents member: the values of `maku configure`, and the broker's version, whose maku release the view's install block downloads (the latest when it is not a `v*` build). `clientID` is `""` while the broker has no maku client. maku asks for the project's audience scope with `projectID`. `dashboards?` (`[{label, url}]`): the room page's link templates from [`human.dashboards`](integration.md#the-brokers-configuration), unfilled; the page fills `{room}`, `{run}` and `{task}` per room. Absent when none is configured. They are not secrets, so any member gets them; `/.well-known/roomctl` leaves them out, since `maku configure` has no use for them |

### `GET /api/rooms/{id}/summary`

The room's top layer, folded from its log, which the room page and `maku status` render. It is a
room read: the same room access and role check as `GET /v1/ws`. Query: `after=N` (default `0`)
keeps only the notes after `seq` N; pass back the number of the previous answer's `cursor`.

| Status | When |
|---|---|
| `200` | The summary below |
| `400` | `after` is not a non-negative integer |
| `404` | No such room, or one the caller may not read: the same `no such room` |
| `503` | `access_unverified`: ZITADEL or GitHub cannot confirm the caller's access past the cache; or the rooms or the log are unavailable. Retry |

| Field | Holds |
|---|---|
| `apiVersion` | `summary/v1`, the contract both renderers check |
| `room`, `url` | The room id and its page |
| `status` | `state`: the [coarse state](#the-coarse-state), from the log's task phase, else the room's. `reason` and `next` (absent unless the task waits: `Queued`, `AwaitingCI`, `AwaitingHuman`, `Escalated`; absent in a sealed room): why, and what happens or who acts next. `issue.title` is the issue's title, bounded. `phase`: the task's, else the room's, and `Closed` or `Sealed` once the log is sealed. `task`: the room's Task, which the factory names as the room, so the room id; absent in a room without task facts. `run`, `budget`, `pr`, `issue`, `lastVerdict`: the factory's latest facts and the latest review verdict, each `null` when absent. `spend` (`usedMicroUSD`, `limitMicroUSD?`): the task's dollars, in millionths of a dollar, beside `budget`'s tokens; absent until the factory reports it. Empty fields inside them are left out, such as a PR's `reviewers` before anyone reviews. `runModel` and `lastRunModel` (`{run, model?, effort?}`): what a run runs on. `model` is its `AgentRun` `spec.model`, from the broker's `run_phase{Running, model}`; `effort` is the reasoning effort its harness reported on `harness_status`, the run's claim, bounded to seven values. `runModel` is the run in the room now, `lastRunModel` the run before it, or the last run when none is in the room. Each absent until the log says. The model that served the run is the gateway's `gen_ai_request_model` |
| `runs` | The task's run history, oldest first, at most 20, from the factory's latest facts: `id`, `role`, `trigger?`, `startedAt?`, `endedAt?`, `outcome?` (`succeeded`, `failed`, `budget_exhausted`, `revoked`, `lost` or `ended`; absent while the run goes on), `verdict?` (at most 32 bytes), `microUSD?` (the run's dollars). Absent when the task has none. Kept in a sealed room, like `status.spend`: they are facts, not needs |
| `needsYou` | `kind: merge` (owners; `id` is the PR number): the task is `AwaitingHuman` on a PR; `what` says how to merge under the repository ruleset (the bypass box, or `gh pr merge N --squash --admin --repo owner/name`), `url` is the PR. `[{kind: approval, id, what, deadline, url}]`: the pending approvals the caller could decide in the web UI. Each carries a link to its card in the room, never a command. `kind: answer` (owners; `id` is the issue number): an Escalated task waits for its issue's author (`needs_author`); `author?` is the author's GitHub login, `url` the issue. `kind: hold` (no `deadline`): the factory's task is `Queued` behind a hold on the room (a person driving it, or another run in it), listed for its driver and owner; `id` is the wait (`waiting_human_driver`, `waiting_room_busy`), `what` says what releases it, `url` is the room. It goes with the factory's next facts |
| `actions` | What the caller may do now, by their standing: `queue` and `stop` carry a `cli` line (every `gh` command passes `--repo owner/name`); `steer`, for the driver and only while an agent is in the room (it joined and has not left), carries none, since `maku` never steers |
| `notes` | `{untrusted: true, items: [{at, run, text}]}`: the last 20 progress notes after `after`. `untrusted` is always `true`: they are the agents' claims, rendered as text |
| `queued` | `{untrusted: true, items: [{ref, author, text}]}`: the notes still queued for the next run, oldest first, redacted; absent when none wait or the room is sealed. The card lists them under "Queued for the next run" |
| `cursor` | `seq:N`, the last `seq` folded |

A sealed room has no `reason`, `next`, `needsYou` or `actions`. A fork's summary starts at its `forked_from`: the
source's task, approvals, verdict and notes are not the fork's.

### The coarse state

One vocabulary for the room list, `summary/v1` and `POST /mcp` (factory interfaces F7): the task's
phase decides, else the room's. A closed or sealed room stops a task it left unfinished. An idle room
waits for its owner unless a run is on its way. A task phase this broker does not know reads as its
room's. A room with no task reads its own phase: `Closed` is `stopped`, and a room not yet started (no
phase, no task) is `queued`. Each state maps onto one A2A task state, so a later agent card is a mapping.

| `state` | Task phases | Rooms without a task | A2A |
|---|---|---|---|
| `queued` | `Received`, `Triaged`, `Queued` | `Open`, or no phase yet | `SUBMITTED` |
| `running` | `Implementing`, `Reviewing`, `AwaitingCI`, `AutoMerging`, `Merged`, `Verifying` | `Active`; `Idle` with a run requested, pending or suspended | `WORKING` |
| `waiting_for_human` | `AwaitingHuman`, `Escalated` | `AwaitingHuman`; `Idle` with no run on its way | `INPUT_REQUIRED` |
| `done` | `Done`, `NoOp` | — | `COMPLETED` |
| `failed` | `Rejected`, `Reverted` | — | `FAILED`; `REJECTED` for `Rejected` |
| `stopped` | `Stopped`, `Closed`, and any unfinished phase in a closed room | `Closed` | `CANCELED` |

### `GET /v1/ws`

| Status before upgrade | When |
|---|---|
| `401` | Not authenticated, or the token is already past its expiry; `user_inactive`: the ZITADEL user is no longer active |
| `403` | A foreign `Origin`; not in an agents group |
| `404` | No such room, or one the caller may not read: the same `no such room` |
| `429` | More than 10 connections for this person, or more than 20 people in this room (per replica) |
| `503` | The log is unavailable; `access_unverified`: ZITADEL or GitHub cannot confirm the caller's access past the cache |

One JSON object per text frame.

| Direction | Frame | Fields |
|---|---|---|
| client → broker | `hello` | `roomId`, `afterSeq?` (clamped to the mark: the `sync` frame sets the baseline) or `tail?` (default: the last 500 events). Must be the first frame, else the socket closes `1008 hello first` |
| client → broker | `act` | `clientSeq`, `action`, `driverEpoch?` |
| client → broker | `ping` | Every 30 s |
| broker → client | `state` | `throughSeq`, `snapshot: {roomId, phase, driver, driverEpoch, dataClass, you, runs, queue, sealed}`. `runs` is `[{id, role, phase, unsandboxed}]`; `unsandboxed` is true for a run whose pod has no RuntimeClass. `queue` is the messages still queued for the next run, `[{ref, author, text}]` (redacted), read just after the mark: apply events past `throughSeq` over it. `sealed` is the log's seal at the mark (`phase` follows the Room and lags it). `approvals` is the pending approvals, `[{approvalId, runId, callId, class, action, expiresAt, seq}]`, read like `queue`: a card per approval, however far behind the tail its request is |
| broker → client | `sync` | `fromSeq`, `throughSeq`: the range that follows from the log |
| broker → client | `event` | One C4 envelope |
| broker → client | `ack` | `clientSeq`, then `seq` or `rejected`, and `result` for actions that return data |

Replay is lossless: the broker subscribes to the room's fan-out (the hub), reads the high-water
mark, pages the log up to it, then streams live events, dropping any at or below the mark. A gap in
live `seq` triggers a range read.

| Close code | Reason | Client should |
|---|---|---|
| `4001` | `reauth` | Reconnect: the connection reached `min(token expiry, 1 h)` |
| `1008` | `slow_consumer` | Reconnect with `afterSeq`: over 2 MiB was pending |
| `1008` | `hello first` | Send `hello` first, within 10 s |
| `1009` | — | Keep a frame under 32 KiB |
| `1007` | `failed to unmarshal JSON` | Send each frame as one JSON object |
| `1001` | `shutdown` | Reconnect: the replica is stopping |
| `1013` | `log_unavailable` | Reconnect with `afterSeq` after a backoff |
| `1008` | `no such room` | Stop: the caller may no longer read the room (room access, re-checked on every ping against the access cache, so within the TTL plus 30 s); a re-dial is refused `404` |
| `1013` | `access_unverified` | Reconnect after a backoff: ZITADEL or GitHub could not confirm the caller's access past the cache |
| `1008` | `user_inactive` | Stop: the caller's ZITADEL user is no longer active (re-checked on every ping, admins included, so within the TTL plus 30 s); a re-dial is refused `401` |

The broker pings every 30 s; a peer that does not answer within 10 s is disconnected without a
close frame. So is a peer that does not take a frame within 10 s (`write_timeout`): reconnect with
`afterSeq`. So is a client that sends no `hello` within 10 s.

### Actions

| Action | Fields | Who (see [authorization](security.md#authorization)) |
|---|---|---|
| `message` | `text`, `delivery: none \| queued \| steering`, `to?` | Collaborator and up; `steering` driver only |
| `remove_queued`, `promote_queued` | `ref` | The author or the driver; promote: driver |
| `interrupt` | — | Driver |
| `driver_request`, `driver_give`, `driver_take` | `to?`, `reason?` | Request: collaborator; give: driver, to a collaborator or better (from the Room's members; an `agents-admin` who is not a member takes instead) or the room's system holder; take: owner or `agents-admin`, with a reason of at most 256 bytes |
| `start_run` | `role` (`implementer`, `reviewer`, `tester`, `triager`), `prUrl?` (reviewer only, a pull request of the room's repository), `egressProfiles?` (at most 8 names) | Driver, owner. A reviewer's task is its PR, else the one the latest agent handoff or review verdict names; any other role's is the fenced brief of the latest agent handoff and verdict, which consumes the queued messages it quotes. Recorded as `state_changed{run_requested}`, with the reviewer's `taskUrl`. Without a factory requester (`via: manifest`) the ack's `result` is the rendered `AgentRun` for the owner to apply. A replayed `clientSeq` acks the record without it; one retried after a failed record (`log_unavailable`) asks for the same run under the same idempotency key, so it never creates a second one ([integration](integration.md)) |
| `invite` | `principal`, `memberRole`, `approver` | Owner. At most 20 members; never demotes the driver-token holder below collaborator (`bad_action`): the holder hands the token over first |
| `close` | `reason?` | Owner |
| `decide` | `approvalId`, `decision: approved \| denied`, `reason?` (at most 1 KiB, redacted; the agent reads it) | Approver, owner, from the web UI. The first valid decision wins (`already_decided` for every later one, a supersede included). The deadline is checked on the database clock in the transaction that would record the decision: one past it closes the approval as `expired` and is refused `approval_expired`, whether or not the sweep has run, as is any decision on an expired approval. With `approvals.fourEyes`, nobody who prompted the run decides (`four_eyes`). A replayed `clientSeq` acks the stored decision |
| `fork` | `seq`, `note?` (at most 1 KiB, redacted), `role?`, `prUrl?`, `egressProfiles?` (as `start_run`'s), `approvals?: attended \| unattended` | Watcher and up; also from `maku`. A sealed room forks only for its owner or an `agents-admin` (`sealed`). A new room owned and driven by the forker, with the source's data class, repository and retention and none of its members. Its approvals ask by default: for the source's owners (owner, owner members, `agents-admin`) `attended`, with the source's `ttl`, `fourEyes` and every override but `allow`, so an unattended factory room does not make a human's own fork run unasked; `approvals: unattended` keeps the source's approvals as they are (an attended source stays attended; `bad_action` for any other value). Anyone else, whatever they ask, gets, class by class, the stricter of the source's and a new room's (`attended`), so an `unattended` or `allow` override never carries over to a watcher: events `1..seq` copied with their `seq`, then `state_changed{forked_from}`. The source's queue as it stood at `seq` comes along: each message queued by then and not yet promoted, removed or consumed by then is queued again in the fork (one consumed through `POST /v1/rooms/{id}/queue/consume` leaves no event to date it, so it comes along only while still queued). With a `role`, the new room's first run, on `agent/<new room>` from the latest agent commit at or before `seq`, on the forker's token and budget. The ack's `seq` is the new room's `forked_from`; its `result` is `{roomId, run?, runError?}`: `run` is the `start_run` claim or `null`, `runError` its rejection. A failed run leaves the fork made. A prefix over 5,000 events or 32 MiB is `too_large`; a principal forks 3 times at once, then once a minute, per replica |

| `rejected` | Meaning |
|---|---|
| `not_permitted` | The caller's room role, flag or client does not allow it; or the factory refused the caller a run, such as a caller without `write` on the room's repository on GitHub ([factory](factory.md#who-may-start-work-on-a-repository)) |
| `stale_epoch` | `driverEpoch` no longer matches: someone else holds the token now |
| `rate_limited` | Over 10 actions per second (burst 20), or over 3 forks at once then one a minute; per replica |
| `no_running_run` | Steering or interrupt with no run `Running` |
| `room_busy` | A run is already running; or a run was requested in the last 10 minutes and has not joined yet, and the broker does not know it ended (a claim not applied, or a factory run not seen yet). Two replicas answering a `start_run` in the same instant can both pass this check until the factory refuses a second pending run per room |
| `reviewer_needs_pr` | A reviewer's `start_run` with no `prUrl`, and no pull request of the room's repository in the latest agent handoff or review verdict. Human chat, queued text and tool output never choose it |
| `over_budget` | The factory refused the run: over the caller's budget |
| `factory_unavailable` | The run could not be requested: no requester, or the factory failed or answered no run id. Retry |
| `bad_action` | Malformed, or about another room; egress profiles on a run or fork of an `internal` room; a give to someone who cannot hold the token; a `clientSeq` this connection already used for an action of another type |
| `not_queued` | The queued message was already delivered or removed |
| `sealed` | The room's log is sealed; a fork of it by anyone but its owner or an `agents-admin` too. A queued message that reached the room's event limit is refused this way but stays in the log, unqueued: the transcript can show it as the room's last message |
| `conflict` | The Room changed under an `invite`: retry |
| `log_unavailable` | The log or the Room could not be read or written: retry |
| `too_large` | A fork's prefix over 5,000 events or 32 MiB: fork at an earlier `seq` |
| `already_decided` | Another approver decided first |
| `approval_expired` | The decision reached the broker after the approval's `expiresAt`; the approval closed as `expired` |
| `four_eyes` | The room requires an approver who did not prompt the turn |

### Machine-sent text

Text that comes through `maku` or `POST /mcp`, a message's `text` and a fork's `note`, is
machine-sent (factory interfaces F8): a local agent can drive either on its human's token. The
broker sanitises it as the factory does an issue (hidden and control characters out, NFKC, fence
look-alikes, images and raw HTML defused), then stores it between two lines: `machine-sent via roomctl` from the CLI (a stored value
that keeps its pre-maku name, see [naming](naming.md#technical-names)), `machine-sent via mcp` from `POST /mcp`:

```text
⟦machine-sent via mcp: a client wrote this on a human's token⟧
use the v2 API
⟦end of machine-sent text⟧
```

The sanitiser maps the text's own `⟦` and `⟧` away, so no text forges either line. The limits hold
once wrapped: a message over 16 KiB is `too_large`, a note over 1 KiB `bad_action`. A message with
nothing readable left is `bad_action`. The web UI's text is stored as typed. The human who holds the
token stays the prompter for four-eyes.

### `POST /mcp`

An MCP server for a developer's local coding agent, through [`maku mcp`](cli.md#the-mcp-server):
streamable HTTP on this listener (factory interfaces F2). It is off unless the broker's config sets
`human.mcp.enabled` ([integration](integration.md)); off, `/mcp` is `404`. Every check of the table
above applies first. Then it takes tokens from the CLI's ZITADEL client (formerly `roomctl`) only, so nothing here carries the
web UI's standing: any other token is `403 mcp_needs_roomctl`, counted in
`rooms_rejected_actions_total`.

It keeps no session: each `POST` holds one JSON-RPC message and gets its answer as JSON, so either
replica answers any request. No `GET` stream and no `DELETE` (`405`), no batch, a body of at most
128 KiB (`413`). It serves `initialize` (revisions `2025-06-18` and `2025-11-25`; any other gets the
newest), `ping`, `tools/list`, `tools/call`, `tasks/get` and `tasks/result`; any other method is
`-32601`. A request whose `MCP-Protocol-Version` names another revision is `400`, `-32600` with
`data.supported`, so a client of a later, stateless revision falls back to `initialize`. A
notification is `202`. Each person makes 10 requests a second, burst 20, per replica; past it,
`-32603 rate_limited`.

| Tool | Standing | Arguments | Result |
|---|---|---|---|
| `rooms_list` | any member | `repo?`, `mine?`, `needsMe?`, `all?` | The overview table as the text (Markdown, to show as it is), and `{rooms, access}` as `structuredContent`: `GET /api/rooms`'s rows and its `X-Rooms-Access`. `all` adds to the text the rooms that ended over a week ago; `structuredContent` always has every row |
| `room_summary` | a reader of the room | `room`, `after?` (`seq:N`) | The room's card as the text (Markdown), and its [`summary/v1`](#get-apiroomsidsummary) as `structuredContent` |
| `room_tail` | a reader of the room | `room`, `cursor?` (`seq:N`), `kinds?` (at most 10 event types) | `{room, state, events: {untrusted: true, items}, cursor, more}`: at most 100 events and 256 KiB of payload past the first, at most 2,000 scanned for `kinds`; without a cursor, the last 50. Pass `cursor` back; `more` says the log held more when it was read |
| `room_changes` | a reader of the room | `room`, `cursor?` (`seq:N`) | What changed since a cursor, as a list of milestones (run started and ended, notes, handoffs, approvals asked and decided, verdicts, the PR opened or merged, holds, the budget) in the text, and `{room, url, since, items, more, cursor, untrusted: true}` as `structuredContent`. Without a cursor, the last 24 hours. A call scans at most 2,000 events: `more > 0` says there is more to read (milestones left out, or events past that bound), and `cursor` is the last event it covered: pass it back. A cursor past the room's last event is clamped to it, so the cursor returned can be lower. A fork's copied events are not its own milestones |
| `room_queue` | collaborator | `room`, `text` | `{room, queued}`: the message's `seq`. Its text is machine-sent |
| `room_fork` | as the `fork` action | `room`, `at`, `role?`, `prUrl?`, `egress?`, `note?` | `{room, url, run?, runError?}`. The note is machine-sent |
| `issue_draft` | any member | `repo`, `title`, `body` | `{repo, title, body, stripped, canStart, canStartWhy?}`: the draft with hidden and control characters stripped (`stripped` counts them). `canStart` is true when `repo` is in `human.mcp.sessionStart` and the caller writes to it on GitHub; else `canStartWhy` is `off` (not listed, no GitHub check wired, or a sponsored caller), `not_linked`, `no_write` (read only, or the GitHub App cannot see the repository) or `unverified` (the check failed). Refused unless the title reads `<area>: <what is wrong>` (at most 256 characters), the body holds `## What is wrong`, `## What it should be` and `## Acceptance check` in that order (at most 16 KiB), and the redactor finds no secret. It files nothing: `maku mcp`'s `issue_file` does |

The view tools (`rooms_list`, `room_summary`, `room_changes`) return their text as `content` and their JSON as `structuredContent`, with an `outputSchema`; the JSON is not repeated in the text. `run_create`'s and `room_tail`'s task results stay JSON text, as `tasks/result` returns them.

`prompts/list` and `prompts/get` serve the prompts `handoff` (`what`), `status` (`room`) and `inbox`: canned
messages the client offers as slash commands. A sponsored principal's list is empty. The prompts start nothing: the `handoff` prompt only drafts an issue.

No tool approves, steers or stops, and none labels or starts factory work by itself: only the developer's answer to `issue_file`'s form, which `maku` serves where `issue_draft` says `canStart`, can apply `factory/ready` with the developer's own `gh`. The two write tools are acts through
the CLI's ZITADEL client (formerly `roomctl`): the [actions](#actions)' rules, limits and rejections apply. Arguments are
strict: an unknown field is refused. Every string an agent wrote comes back under `untrusted: true`,
as the summary's notes do. A refused call is a tool result with `isError: true` whose text starts
with its reason: `invalid_arguments`, `invalid_draft`, `rejected: <the act's rejection>`, `no such
room` (a missing room and one the caller may not read alike), `access_unverified`, `rooms
unavailable`, `log unavailable`, `room log unreadable` or `unavailable`. Each call counts in
`rooms_human_mcp_calls_total{tool, outcome}`.

**`room_tail` as a task** (MCP Tasks, F5). With `2025-11-25`, `initialize` declares
`tasks.requests.tools.call` and `room_tail` lists `execution.taskSupport: optional`. A `tools/call`
with `task: {ttl?}` returns a task that waits for the room's next events: `ttl` is the wait, 10 s to
15 min (default 5 min); without a cursor, it waits for the events after the room's last `seq` now.
The broker keeps no task: the id is an unsigned cursor naming the query and its creator, not a signed
token. The broker checks that the caller is the creator, and every read re-authorizes the caller as the
room's reader, on any replica, from the log.

| Method | Answers |
|---|---|
| `tasks/get` | `completed` once an event of the task's `kinds` follows its cursor, its room is `done`, `failed` or `stopped`, or the wait is over; `failed` with `no such room` when the room cannot be read; else `working`, the room's state in `statusMessage`. `pollInterval` is 5 s |
| `tasks/result` | A terminal task's page, as `room_tail` returns it, with `_meta["io.modelcontextprotocol/related-task"]`. A `working` task is `-32600 task_working`: it does not block, since every request must end inside the proxy's 30 s. `maku mcp` polls `tasks/get` and blocks for its client |

Another person's task id, or a malformed one, is `-32602 no such task`. A room read that fails for
a while is `-32603`: poll again. There is no `tasks/list` or `tasks/cancel` (`-32601`), and no task
is `input_required`: that state belongs to [`run_create`](#sponsored-principals-on-mcp).

### Sponsored principals on `/mcp`

A sponsored principal (a ZITADEL machine user declared in the broker's `human.mcp.sponsored` and the
factory's `api.sponsored`, [factory](factory.md#sponsored-principals)) reaches `/mcp` with its own
client-credentials token, whose audience must hold the project (`human.projectIDFile`). Nothing
else serves it: every other route refuses it, and it never reaches `/v1/runs`. It sees five tools:
`run_create`, `issue_draft`, `room_summary`, `room_tail` and `room_changes`; `room_queue`, `room_fork` and
`rooms_list` are unknown tools to it. It reads a room only as one of its `watcher` members, which
the factory adds when it creates its task's room, with no GitHub check.

| Tool | Arguments | Result |
|---|---|---|
| `run_create` | `repo`, `text`, `dataClass` (`public`), `max_cost` (USD, above 0), all required; `title?` (files the issue, with the `issue_draft` rules), `on_behalf_of?` (`slack:<team>/<user>`), `ref?`, `idempotency_key?` (at most 128 characters; default a hash of the arguments, so a retried call makes one task) | `{task, room, issue, duplicate, url, sponsor?}`: the factory's answer, the room's page, and the GitHub login that sponsors a new task |
| `slack_link_code` | `team`, `user` (Slack ids), `handle?`, `name?` (display only). Listed only to `human.slackLink.principal`; `team` must be `human.slackLink.teamID` (`wrong_team`) | `{url, expiresAt}`: `<publicURL>/link/slack?code=<code>`, single use, 10 minutes; it replaces the user's earlier code. `too_many_codes` past 1000 waiting ([Slack self-serve link](slack.md#self-serve-link)) |
| `slack_unlink` | `team`, `user`. Listed only to `human.slackLink.principal` | `{unlinked}`: whether the Slack user had a self-serve link |

Unknown fields are refused: class, tier, template, auto-merge and egress are never the caller's.
The text passes the broker's redactor, then [`POST /v1/tasks`](#post-v1tasks) with the caller's
own token, which the factory verifies again. A factory refusal comes back as a tool error,
`rejected: <reason>`.

**`run_create` as a task.** With `2025-11-25`, `run_create` lists `execution.taskSupport: optional`.
A `tools/call` with `task: {ttl?}` creates the factory task and returns an MCP task whose id is
`run.` plus a base64url JSON `{v, by, room, created, ttl}`: stateless like `tail.`, bound to its
creator. `ttl` defaults to 24 h, at most 72 h. `tasks/get` maps the room's coarse state:

| Room state | Task status |
|---|---|
| `queued`, `running` | `working` |
| `waiting_for_human` | `input_required`, `statusMessage` naming the room page; `tasks/result` returns the summary and never asks the caller anything: approvals stay on the web page |
| `done` | `completed` |
| `failed` | `failed` |
| `stopped` | `cancelled` |

Triage makes the room, so a task whose room does not exist yet is `working` for a 2-minute grace
from its creation, then `failed` (`no such room`). A slow triage can flip a `failed` task back to
`working` once the room appears: poll `tasks/get` until `completed` or `cancelled`, and treat
`failed` as provisional until the grace is well past.

## `:8082` — the factory's task intake

### `POST /v1/tasks`

It serves on the factory's `api.listen`, only while `api.sponsored` or `api.internalPrincipals` is
non-empty (otherwise `404`). It takes sponsored and [internal](#an-internal-principals-request) tokens
only: a human or a system caller gets `403 sponsored_only`. (`POST /v1/runs` refuses a sponsored caller the other way round, with
`403 sponsored_only_tasks`: a sponsored principal starts tasks, never runs.) The broker's
`run_create` calls it with the caller's own token. Model and
limits: [sponsored principals](factory.md#sponsored-principals).

| | |
|---|---|
| Auth | `Authorization: Bearer`: a token from `api.humanIssuer` whose client is a sponsored principal's `clientIDFile` and whose audience holds `api.projectIDFile` |
| `Idempotency-Key` | Required, at most 255 bytes. The Task's name derives from `api:<name>:<key>`: a replay answers `200 {duplicate: true}` and creates nothing, two requests under one key make one task. A name another request owns is `409 idempotency_conflict` |
| Body | `repository`, `text` (at most 16 KiB), `dataClass` (`public`), `maxCostUSD` (at least 0.01), all required; `title?` (files the issue; `<area>: <what is wrong>` and the three template sections), `onBehalfOf?`, `ref?` (at most 256 characters, a git ref: letters, digits, `.`, `_`, `/`, `-`, no leading `-`, no `..`, no whitespace; the same pattern as `run_create`'s, since it is written into the agent's brief). At most 64 KiB; unknown fields refused |
| `201` | `{task, room, issue?, sponsor}`: the room is named as the task; `sponsor` is the GitHub login it is charged to |
| `200` | `{task, room, issue, duplicate: true}` |
| `400` | `bad_json`, `idempotency_key_required`, `bad_data_class`, `bad_ref`, `max_cost_required`, `max_cost_over_principal`, `max_cost_over_class`, `invalid_draft`, `bad_text` |
| `401`, `403` | `unauthenticated`; `not_permitted`, `sponsored_only`, `repository_not_allowed`, `not_linked`, `not_a_maintainer` |
| `503` (link) | `link_unverified`: a [self-serve link](factory.md#sponsored-principals) the broker or ZITADEL could not answer for; retry |
| `409`, `429` | `repository_not_owned`, `idempotency_conflict`; `principal_task_cap`, `principal_budget_exhausted` (advisory: the hard limit is the run-time reservation) |
| `503` | `kill_switch`, `meter_unpriced`, `tasks_unavailable`, `ledger_unavailable`, `intake_failed` |

#### An internal principal's request

An [internal principal](factory.md#internal-principals) (RunLore) sends the same route with a finding,
and gets `investigate` on internal data, never a title or a link to another user. The body is
`repository`, `text` (the finding), `dataClass: internal`, `maxCostUSD` and `incident`, all required:
`incident` is `{alert, resource, severity, cluster, verdict, confidence}` (`alert` and `resource`
required). `Idempotency-Key` is the incident's trigger key.

| | |
|---|---|
| `201` | `{task, room, issue}`: an `investigate` task and a public stub issue labelled `factory/proposed`; it names the alert, resource and room, never the finding |
| `200` | `{task, room, issue, duplicate: true}`: a task of the key is still open. Once it ended, the same key is a new generation |
| `202` | `{accepted: false, reason: below_threshold}`: the verdict is not `action_required` or `action_suggested`, or `confidence` is under `minConfidence` |
| `400` | `bad_json`, `idempotency_key_required`, `title_not_allowed`, `on_behalf_of_not_allowed`, `bad_ref`, `bad_data_class` (anything but `internal`), `incident_required`, `max_cost_required`, `max_cost_over_principal`, `max_cost_over_class` |
| `403`, `409` | `repository_not_allowed`, `repository_not_owned` |
| `429` | `principal_daily_tasks` (the principal's tasks created this UTC day, whatever their kind), `principal_task_cap`, `principal_budget_exhausted` |
| `503` | as above |

A sponsored principal's request refuses an `incident` (`bad_json`), and an internal principal's refuses
everything a sponsored one's does beyond its own keys.

### `POST /v1/tasks/{task}/verification`

An internal principal with `verification: true` says whether the fix of its own proposal held. It serves
on the same port, only while some internal principal has `verification: true` (otherwise `404`). `{task}`
is the **fix** task, whose `spec.origin` names a task the caller created. The body is
`verdict` (`resolved` or `persisting`), `alert` (`name`, `firingAtMerge`, `firingNow`), `checkedAt`
(RFC 3339) and `evidence?` (at most 16 KiB, scanned for secrets); at most 32 KiB, unknown fields refused.

| | |
|---|---|
| `201` | `{task, verdict, recordedAt}`: the verdict is `status.monitor.verification` of the fix task, which the [post-merge monitor](factory.md#the-post-merge-monitor) reads; the evidence is `status.published.evidence` of the origin task (internal: the origin room is closed by then) |
| `200` | `{…, duplicate: true}`: the same verdict was already recorded |
| `400` | `bad_json`, `bad_verdict`, `bad_evidence` |
| `401`, `403` | `unauthenticated`; `not_permitted` (not an internal principal), `verification_not_allowed`, `not_own_task` (no origin, another principal's origin, or a class with no `monitor.verification`) |
| `404`, `409` | `no_such_task`; `not_merged`, `window_closed` (past `monitor.verification.within` after the merge), `verdict_already_recorded` (another verdict) |
| `503` | `tasks_unavailable` |

`agent_factory_api_intake_total{principal, outcome}` counts the requests of an authenticated sponsored
or internal principal: `admitted`, `duplicate`, or the code of any refusal after that point (`kill_switch`,
`bad_*`, `max_cost_*`, `repository_*`, `invalid_draft`, the caps and the `503`s); any other value is
`other`. The `unauthenticated`, `not_permitted` and `sponsored_only` refusals come before the
principal is known and are **not counted**.

## Webhooks

The broker sends signed, id-only events to the subscribers declared in its `webhooks` config (see
[operations](operations.md#webhooks)). It is off until a subscriber is declared. A payload carries
ids and closed codes only: never room text, a title, a branch or a message.

| Event | Fires when | Fields beyond `type` and `room` |
|---|---|---|
| `room.task_received` | The task enters `Received` | `repo` |
| `room.task_triaged` | The task enters `Triaged` | `repo` |
| `room.run_started` | A run enters `Running` | `run` |
| `room.run_ended` | A run ends: `Succeeded`, `Failed`, `BudgetExhausted` or `Revoked` | `run` |
| `room.waiting_for_human` | The task enters `AwaitingHuman` or `Escalated`, or an approval is requested | `reason` |
| `room.pr_opened` | The first pull request of the task is known | `pr` |
| `room.verdict_posted` | GitHub accepted the verdict comment | `pr` |
| `room.merged` | The task enters `Merged`, or its facts carry reason `merged` or `merged_verified` (a human merge), once | `pr` |
| `room.reverted` | The task enters `Reverted` | `pr` |
| `room.budget_reached` | A task phase or a run end names a budget reason | `limit` |
| `room.ended` | The task's first coarse end: `done`, `failed` or `stopped` | `state` |

A task event fires on a change of phase only: the factory rewrites a phase's facts (budget, run,
pull request) without a new event. `repo` is on `task_received` and `task_triaged` only.
`verdict_posted` is sent once GitHub accepted the comment, not when it was attempted.

`origin` is on **every** event of a room whose task was started from an issue the factory published
(see [`factory/publish`](factory.md#internal-principals)): the room id of the investigate task. It is
absent on any other room's events.

`reason`, `limit` and `state` are clipped to closed lists (`webhook.Reasons`, `webhook.Limits`,
`webhook.States`); anything else is sent as `other`. The factory's room escalations `foreign_room`,
`room_log_unreadable` and `rooms_crd_outdated` are listed.

| Field | Values |
|---|---|
| `limit` | `budget-run`, `budget-task`, `budget-principal`, `budget-fleet`, `budget-person`, `budget-family` |
| `state` | `queued`, `running`, `waiting`, `done`, `failed`, `stopped` |
| `reason` | The wait reasons of [the summary](#get-apiroomsidsummary), plus `approval`, `manual`, `revoked`, `deleted`, `principal_unknown`, `budget-run`, `budget-principal`, `budget-fleet`, `budget-person`, `foreign_room`, `room_log_unreadable`, `rooms_crd_outdated` |

Example body:

```json
{"type":"room.pr_opened","room":"room-7f3a","pr":42}
```

### Delivery

A `POST` of `application/json` with three headers, as [Standard Webhooks](https://www.standardwebhooks.com/) defines them:

| Header | Value |
|---|---|
| `webhook-id` | `evt_…`: the same for every retry of one event, distinct per subscriber |
| `webhook-timestamp` | Unix seconds |
| `webhook-signature` | `v1,<base64>` per key, space-separated: two while a key is rotated |

The signed string is `webhook-id.webhook-timestamp.body`, HMAC-SHA256 under the subscriber's key,
over the raw body bytes.

A subscriber must:

1. verify the signature against the raw body, with a constant-time comparison;
2. reject a timestamp more than 5 minutes from its clock, either way;
3. deduplicate on `webhook-id`: a delivered event can arrive twice (a restart replays from the last cursor);
4. answer `2xx` quickly, and fetch the room through the MCP server or the human API if it needs more
   than the ids.

**At-least-once holds for delivered events only.** A delivery that does not get a `2xx` is retried after
10 s, 1 min and 5 min, then given up: the event is dropped and the room's cursor moves on, even when
the subscriber stays enabled (three given up in a row disable it). The exception is a signing key the
broker cannot read: the room is held, nothing is dropped, and each sweep tries again until the key is
fixed.

Events are ordered within a room, except for the replays after a broker restart, and not across
rooms. A subscriber's response body is never read beyond 64 KiB. No redirect is followed and no proxy
is used.

Where a subscriber starts: every room that exists when the subscriber is first declared starts at that
room's last event, so history is not replayed; a room created later starts at its first event.

A room's events are read from its log. A room forked from another replays the parent's copied history
as its own events, so a subscriber of the fork sees the parent's phases again, under the fork's room id.

A room's repository (the `repo` field and the `repositories` filter) comes only from the factory's
task facts, or else from the Room. If the two disagree, the room is held and counted `mismatch`.

Verifying with Go, standard library only. `keys` are the base64-decoded bytes of the key file's lines:

```go
func verify(keys [][]byte, h http.Header, body []byte, now time.Time) (id string, ok bool) {
	id, ts := h.Get("webhook-id"), h.Get("webhook-timestamp")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if id == "" || err != nil {
		return "", false
	}
	if d := now.Sub(time.Unix(sec, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return "", false
	}
	for _, k := range keys {
		m := hmac.New(sha256.New, k)
		fmt.Fprintf(m, "%s.%s.", id, ts)
		m.Write(body)
		want := m.Sum(nil)
		for _, s := range strings.Fields(h.Get("webhook-signature")) {
			v, found := strings.CutPrefix(s, "v1,")
			if !found {
				continue
			}
			if got, err := base64.StdEncoding.DecodeString(v); err == nil && hmac.Equal(got, want) {
				return id, true // dedupe on id, then act
			}
		}
	}
	return "", false
}
```

Verifying with Python. `keys` are the base64-decoded bytes of the key file's lines:

```python
import base64, hashlib, hmac, time

def verify(keys, headers, body: bytes) -> bool:
    i, ts = headers["webhook-id"], headers["webhook-timestamp"]
    try:
        if abs(time.time() - int(ts)) > 300:
            return False
    except ValueError:
        return False
    for key in keys:
        want = base64.b64encode(hmac.new(key, f"{i}.{ts}.".encode() + body, hashlib.sha256).digest()).decode()
        if any(hmac.compare_digest(s[3:], want) for s in headers["webhook-signature"].split() if s.startswith("v1,")):
            return True
    return False
```

A key line prefixed with `whsec_` is the secret the Standard Webhooks libraries expect.

## `:8090` — room tools

An MCP server on `POST /mcp`, reached only through an `agent-router` `MCPRoute`, which authenticates
the run first and injects a generated key in `X-Room-Mcp-Key` (`ROOMS_MCP_KEY`).

The listener is the config's top-level `roomTools` block, apart from `human.mcp`: absent, or with no
`enabled`, it is on, as every deployed broker runs it; an explicit `roomTools.enabled: false` binds
no `:8090` at all and starts no server, so `ROOMS_MCP_KEY` is not even read. On, `ROOMS_MCP_KEY`
alone decides whether a call is accepted, and without it every call is refused (`401`).

The broker derives the run from `X-Ar-Agent`, the gateway's verified `sub`, matched whole against each
run issuer's `subPattern` in turn, and reads the run's room and role from its `AgentRun`, never from a
header or an argument. It exposes `initialize`, `ping`, `tools/list` and `tools/call` only: no
`resources` or `prompts`, which the gateway would not authorize by role. It answers `POST` only and
never opens a stream or sends a request of its own (agent-router#2715).

| HTTP | When |
|---|---|
| `401` | `X-Room-Mcp-Key` wrong, missing or sent twice, or `ROOMS_MCP_KEY` unset: every call is refused |
| `403` | `X-Ar-Agent` missing, sent twice or naming no run, or the run is not live or in no room |
| `405` | Anything but `POST` |
| `413` | A body over 128 KiB |
| `503` | A call over 15 s |

`tools/list` lists the tools of the run's role. A call needs the role and the permission matrix's action (see [what each standing may do](web-ui.md#what-each-standing-may-do))
(`read` for `room_read`, `chat` for the three writes), then spends the run's one call per second.
The limit is per broker replica: with two replicas, a run whose calls the gateway
spreads across both can make two a second. Arguments are strict: an unknown field is refused, and text holds
no control character but tab, newline and carriage return. Every write is redacted before it is
appended, attributed to `agent:<runId>` with the run's role, origin `client`.

| Tool | Roles | Arguments | Result | Appends |
|---|---|---|---|---|
| `room_read` | all | `sinceSeq` ≥ 0, `limit` 1–100 (default and cap 100) | `{events, lastSeq}`: the room's `message` and `handoff` events, redacted, at most 1 MiB of payload a reply and 500 events scanned. Pass `lastSeq` as the next `sinceSeq` | nothing |
| `room_post` | all | `text`, 1–16 384 bytes | `{seq}` | `message{kind: chat}`, delivered to nobody |
| `room_progress` | all | `text`, one line of 1–280 characters; one note a minute per run per broker replica, on top of the call limit | `{seq}` | `message{kind: progress}`, delivered to nobody; a refused note over the minute answers `rate_limited: one progress note a minute`. The harness calls it too (#60), through the same identity proxy, so nothing is bypassed: a fixed `Harness (<role>): …` line at the first step and after a commit, push, check run or PR that exited 0, each once, 60 s apart, at most 8. The prefix is a text convention, not an attribution. The minute is shared with the agent's own notes, so the harness's are the guaranteed floor and the model's are best effort. The bounds are per process: the first note of a resumed run starts `(resumed)` |
| `room_handoff` | implementer, tester, triager | `toRole`, `summary` (1–8 192 bytes), `commit` (the full 40-hex sha, `git rev-parse HEAD`; an abbreviation is refused); `scope` (the triager only, listed to it alone: `{state: ready or question, task, touches, leave, plan, assumptions, acceptance: [{check, proves}], question}`, at most 4096 bytes as JSON; any other role's is refused. It is listed to every triager, but only a scoper run's is used: an investigate triager leaves it empty, as a malformed one refuses the handoff); `split` (the triager only, listed to it alone, and only beside a `scope` in state `ready`: `{why, children: [2 to 5 scopes in state ready, in the order they merge]}`, at most 21 KiB as JSON) | `{seq}` | `handoff{fromRole, toRole, summary, commit, branch, scope?, split?}`; `fromRole` and `branch` from the `AgentRun` |
| `room_verdict` | reviewer, tester | `verdict: approve \| changes`, `summary`, `commit` (as `room_handoff`) | `{seq}` | `message{kind: review_verdict, verdict, commit, pullRequest}`: `pullRequest` is the run's `spec.task.url` when it is a pull request of `spec.repository`, else absent. The leader then posts it on the PR |
| `room_judge` | implementer | `verdict: met \| not_yet \| impossible`, `reason` (1–1000 bytes), `evidence` (1–20 ids, one line each) | `{seq}` | `message{kind: judge_verdict, verdict, text, evidence}`. The harness calls it (O15) after each completion-judge round, once its own override is applied (a failing check is never `met`); the summary's `status.lastJudge` and `room_changes`' `judge` milestone show it, the reason as untrusted text. It is the judge's reading, not a decision. It is the harness's alone: the tool is listed and callable only on a request carrying the harness client's `X-Agent-Harness: 1` marker, which the model's own MCP client is configured without, so it is not on the model's tool list and its call is refused `not_permitted`. The marker is not a secret: a command the model runs could send it if it can reach the route with the run's credentials |

A refused call is a tool result with `isError: true`, whose text starts with its reason:

| Reason | Meaning |
|---|---|
| `not_permitted` | The tool is unknown, or not one of the run's role |
| `rate_limited` | A second call within the second |
| `invalid_arguments` | The arguments break the tool's schema; the text says how |
| `room_sealed` | The room takes no more events |
| `no_room` | The run's room has no log |
| `log_unavailable` | The log failed; try again later |

Tool calls carry no idempotency key (MCP request ids are per session), so a retried call can append
twice; both copies are attributed and visible.
