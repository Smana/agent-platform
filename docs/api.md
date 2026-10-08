# API

The broker serves four listeners. Phase 1 builds only **`:8443`**, for room bridges and system
callers, and **`:9090`**, for metrics and probes. Humans get `:8080` in phase 2, agents' room tools
get `:8090` in phase 3. Every body is JSON.

| Port | Who calls it | Authentication | Phase / PR |
|---|---|---|---|
| `:8443` (TLS) | `room-bridge` in each run pod; system callers such as SP3's factory | Offline JWT: run tokens (audience `room-broker`) or system tokens (audience `rooms-system`) | AP-1 |
| `:9090` | kubelet, `vmagent` | None: probes and metrics only | AP-1 (task 1.12) |
| `:8080` | Humans, through oauth2-proxy | ZITADEL ID token and access token | 2 / AP-2 |
| `:8090` | Agents' `room_*` tools, through the `agent-router` Gateway only | Injected key plus the gateway's verified `x-ar-agent` | 3 / AP-3 |
| `:8085` (bridge) | kubelet | None | AP-1 (task 1.11) |

`room-broker serve` serves `:8443`, `:9090` (AP-1), `:8080` (AP-2) and `:8090` (AP-3).

## `:8443` — bridge and system API

Served with `ListenAndServeTLS` on the pair cert-manager writes to `/etc/room-broker/tls/`. The
broker re-reads the files when they change, so a renewal needs no restart, and a plain-HTTP request
fails (GP-18). See [security](security.md#tls-on-8443) for the certificate and its CA.

Errors are JSON, `{"error": "<reason>"}`, with the status codes below. The reasons are stable
strings, the `wire.Reason*` constants: branch on them, not on the status text.

`hello`, the events endpoint and both system endpoints limit each principal to 10 requests a second (burst
20) and 10 in flight, §4's per-principal numbers. Over either, they answer `429 rate_limited` with
`Retry-After: 1`, and the caller retries.

### Authentication

| Caller | Presents | The broker checks |
|---|---|---|
| A run's bridge | `Authorization: Bearer <token>`: a projected ServiceAccount token, audience `room-broker`, 600 s, re-read from its file before every request | Signature against the JWKS of an issuer in `runIssuers`; audience; expiry; `sub` matches the issuer's `subPattern`, which yields the run id. Then the run must be live in the `AgentRun` watch (not terminal, not revoked) and have a `roomRef`. The room is the run's `roomRef`, never a request parameter |
| A system caller | `Authorization: Bearer <token>`, audience `rooms-system` (ruling P3) | Signature against `systemIssuer`; audience; expiry; `sub` must be a key of `systemPrincipals`, whose value becomes the principal, e.g. `system:factory` |

No call reaches Kubernetes' TokenReview: validation is offline and issuer-agnostic (C2 r5). A run
that ends, is revoked or is deleted has its streams cut on the watch event.

### Endpoints

| Method and path | Caller | Does | Phase / PR |
|---|---|---|---|
| `POST /v1/bridge/hello` | Bridge | Claims the room's bridge lease, returns where the log is | AP-1 |
| `POST /v1/bridge/events` | Bridge | Appends a batch of harness items | AP-1 |
| `GET /v1/bridge/stream` | Bridge | One SSE stream down: pings; `deliver` and `interrupt` from phase 4; `decision` from phase 5 | AP-1 (pings); 4 / AP-4; 5 / AP-5 |
| `POST /v1/bridge/approvals` | Bridge | Asks for a human decision on a pending action | 5 / AP-5 |
| `GET /v1/rooms/{id}/events` | System | Reads a room's log | AP-1 |
| `POST /v1/rooms/{id}/messages` | System | Appends `message{kind: task_state}` | AP-1 |
| `POST /v1/rooms/{id}/task` | System | Appends the task's facts, `state_changed{kind: task}` | Local-first UX |
| `POST /v1/rooms/{id}/queue` | System | Queues a chat message for the room's next run | SP3 (R9) |
| `GET /v1/rooms/{id}/queue` | System | Lists the messages still queued | SP3 (R9) |
| `POST /v1/rooms/{id}/queue/consume` | System | Marks queued messages consumed by a run | SP3 (R9) |

### `POST /v1/bridge/hello`

No body. Response `200`:

```json
{"roomId": "3kq7x2ma", "afterHarnessSeq": 36, "afterStatusSeq": 4, "approvals": {"profile": ""}}
```

`afterHarnessSeq` and `afterStatusSeq` are the highest keys already stored for this run's two
streams; the bridge resumes after them. `approvals` carries the room's approval profile from phase 5
and is empty before.

| Status | `error` | When |
|---|---|---|
| `401` | `unauthenticated` | No token, a bad signature, a wrong audience or issuer, an expired token, a `sub` that names no run |
| `403` | `run_not_live` | The run is terminal, revoked, deleted, or not yet in the watch |
| `403` | `run_has_no_room` | The run has no `roomRef` |
| `429` | `rate_limited` | Over the run's limits, shared with its batches |
| `409` | `room_busy` | Another run holds the room's lease, and its run is live (rulings P17, SBB). The broker also appends `state_changed{kind: limit, reason: concurrent_run}` |
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
| `400` | `bad_batch` | Not JSON, an unknown field, or data after the batch | Never drops it: halves the batch until the refused item is alone, then keeps its slot with a `state_changed{harness_event, harnessKind: refused}` stub naming the broker's reason (Ruling AM). If even the stub is refused, it retries with backoff and logs at Error |
| `400` | `bad_item` | Unknown type or stream, `seq` ≤ 0, a type or `state_changed` kind a bridge may not push ([allowlist](event-envelope.md#state_changed-kinds)), or a key spelled two ways | As for `bad_batch` |
| `400` | `bad_payload` | The payload is not a JSON object | As for `bad_batch` |
| `413` | `batch_too_large` | Over 2 MiB or over 500 items | Halves the batch; a lone item still too large keeps its slot with a size stub |
| `401` | `unauthenticated` | As for `hello` | Re-reads its token and retries |
| `403` | `run_not_live`, `run_has_no_room` | As for `hello` | Retries on the next tick |
| `409` | `lease_lost` | **Ruling Y:** this run no longer holds the room's lease, seen by the lease renewal or by the append's fence. Nothing is appended | Must not drop the batch: the events are not in the log. Keeps it and says hello again |
| `410` | `sealed` | The room is sealed | Stops mirroring |
| `429` | `rate_limited` | Over the run's request rate or requests in flight | Must not drop the batch: retries after `Retry-After` |
| `503` | `log_unavailable`, `timed_out` | The database refused the append, or the request's 30 s ran out before the batch was redacted | Retries; keeps buffering |

A payload Postgres refuses outright (SQLSTATE class 22) is not an error: it is stored as a
`{"refused": true, "type": …, "reason": "invalid_value"}` stub so the cursor moves on.

### `GET /v1/bridge/stream`

A Server-Sent Events stream the bridge opens and holds (C4 r5: sandbox-initiated, no WebSocket).
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
| Refused for good: steering with a 4xx other than 404, 408, 409, 425, 429; an interrupt with any 4xx | Acknowledges `undeliverable` with the harness's `code`, and goes on: one refusal must not block every later `ref`. An interrupt of an idle conversation is moot, and a late one would land on a turn the driver never meant (ruling SAK) |
| Failed for now (no answer, 5xx, or steering refused with one of those 4xx) | Ends the stream; the replay hands the same `ref` over again before any later one |

**Delivery is at least once** (design, Risks: bridge crash re-delivery). A bridge injects each `ref`
once per process, but one that restarts, or shuts down while a `deliver` is in flight, after
injecting a `ref` and before its acknowledgement reaches the log, injects it again: the transcript
shows a second user message. A replayed `interrupt` lands on whatever turn is current then, which can
be a later turn than the one the driver meant.

Acknowledgements are the bridge's own claims (design T4, T6): the broker cannot see inside the
sandbox. A `ref` counts only if it is a delivery of that run, so a forged one past every delivery
skips nothing; a compromised sandbox can still hide steering from its own run, as it could by ignoring
the harness. The log may then show `delivered` for a message never injected.

| SSE `event` | `data` | Meaning | Phase / PR |
|---|---|---|---|
| `deliver` | `{"ref": 1846, "text": "…"}` | A steering message; the bridge injects it into the harness and acknowledges it as `state_changed{delivered, ref}` | 4 / AP-4 |
| `interrupt` | `{"ref": 1847}` | The driver interrupted the run; acknowledged as `state_changed{interrupted, ref}` | 4 / AP-4 |
| `decision` | `{"approvalId": "…", "allow": true, "reason": "…", "ref": 1851}` | An approval was decided; acknowledged as `state_changed{decision_applied, ref}` | 5 / AP-5 |

Errors before the stream opens are those of `hello`.

### `POST /v1/bridge/approvals` (phase 5 / AP-5)

Request, at most 64 KiB: `{"eventId": "e42", "callId": "call_97", "class": "forge.pr", "action": {…}}`.
`eventId` is the harness ActionEvent's id. `class` is one of `forge.push`, `forge.pr`, `forge.other`,
`mcp.write`, `shell.high`. The action is redacted and stored as the approval card shows it (T3).
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
bridge. An expiry is decided `allow: false` with `reason: "expired"`, within 30 s of `expiresAt`.

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

For system callers; appends the reserved kind SP3 owns (C4). Request, at most 32 KiB:

```json
{"kind": "task_state", "text": "Reviewing", "clientSeq": 1}
```

`kind` must be `task_state`, `clientSeq` greater than 0, `text` at most 16 KiB; the text is redacted.
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

For system callers (SP3 ruling R9): the factory turns a maintainer's GitHub review into a queued
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

The bridge serves `GET /healthz` on `:8085`, for kubelet only, and no metrics (Ruling AP). It reports unhealthy only when the
harness answered once and has been unreachable for more than 60 s: as a native sidecar its startup
probe gates the harness container, so it must never wait for the harness (ruling P6). It never
checks the broker, so a broker outage cannot mark sandboxes unready.

It also serves `GET /admission`, read by `room-bridge gate` on loopback (F15):

| Status | Body | When |
|---|---|---|
| `503` | `pending` | No hello has been decided yet: the broker is unreachable, or another run's live run has held the room for less than 3 minutes |
| `200` | `admitted` | The bridge holds the room's lease. The gate exits 0 and the harness starts |
| `409` | `room_busy` or `sealed` | The run will never hold the room. The gate exits 1, which fails the pod before the harness runs |
| `403` | `loopback only` | The request came from outside the pod |

## `:8080` — human API (phase 2 / AP-2)

Reached only through oauth2-proxy on `rooms.<private domain>`. Every request carries the human's
ZITADEL **ID token** in `Authorization` and their **JWT access token** in `X-Forwarded-Access-Token`,
both from oauth2-proxy.

| Check | Rule |
|---|---|
| ID token | Issuer is the identity provider, audience holds the `rooms-proxy` client id, not expired |
| Access token | Same `sub`; issued for `rooms-proxy`. A web session requires it to differ from the ID token (review M16) |
| Groups | `agents-admin` or `agents-member`, else `403` |
| Room access (D7) | `agents-admin` sees every room. Anyone else sees a room only if GitHub lets the login linked to their ZITADEL user read the room's `repository`; a room with none is admins-only. Answers are cached for at most 5 minutes; past that, a check ZITADEL or GitHub cannot answer fails closed. A room you may not see answers exactly like a missing one |
| `Origin` | Must match the room host: no cross-site WebSocket (T9) |
| Lifetime | A connection lasts `min(token expiry, 1 h)`, then closes for re-authentication |

`roomctl` presents a bearer token from its own native ZITADEL client (phase 6); oauth2-proxy lets it
through with `skip-jwt-bearer-tokens`. Such a token can read, post, queue and fork, but the broker
refuses `decide`, `message{steering}`, `interrupt` and every `driver_*` action from it (ruling P18).

Errors before a WebSocket upgrade are plain-text HTTP errors.

| Method and path | Does | Phase / PR |
|---|---|---|
| `GET /`, `GET /r/{id}`, `GET /assets/{file}` | The embedded UI, under a strict Content Security Policy. A room's page keeps its newest 5 000 events; older ones leave the page, never the log. A `401` from the API, or one behind a refused WebSocket upgrade, sends the page to `/oauth2/start?rd=<the page>` | 2 / AP-2 |
| `GET /api/rooms` | One row per room the caller may read (room access, then their role): id, phase, owner, driver, data class, last `seq`, `repository`, `needsMe` (an approval is pending that the caller could decide in the web UI), and the caller's own role. A room whose access cannot be verified is left out, with no error. The `X-Rooms-Access` header says why a list may be short, about the caller only: `unlinked` (not an admin, and no GitHub link on their ZITADEL user), `unverified` (a check failed past the cache, or none is configured), else `ok`. Filters, applied after the access and role checks so none reveals an unreadable room: `?repo=owner/name`; `?mine=1`, rooms whose task's issue the caller filed or labelled or whose PR they authored or review, matched on their linked GitHub login; `?needs_me=1`, rows with `needsMe` | 2 / AP-2; filters local-first UX |
| `GET /api/rooms/{id}/summary` | The room's [`summary/v1`](#get-apiroomsidsummary): status, what needs the caller, what they can do, the agents' notes | Local-first UX |
| `GET /v1/ws?room=<id>` | The live room, over WebSocket | 2 / AP-2 |
| `POST /api/rooms` | `{"dataClass": "public", "repository": "Smana/cloud-native-ref"}` → `201 {"id": "…"}`: a new room owned and driven by the caller, any agents member, on a `repository` they can read (room access). `400` for another data class, a missing or malformed repository, an unknown field or a principal the Room CRD would refuse, `404` for a repository the caller cannot read or the factory App cannot see (missing, or the App not installed), `429` past the caller's action budget (10/s, burst 20, shared with acts), `503` when the Room cannot be created, or `access_unverified` when the caller's access cannot be checked. The `SameSite=Strict` cookie and the `Origin` check stop a cross-site post (T9) | 4 / AP-4 |
| `GET /api/roomctl` | `{url, issuer, clientID, projectID}` for the room list's CLI setup view, any agents member: the values of `roomctl configure`. `clientID` is `""` while the broker has no roomctl client. roomctl asks for the project's audience scope with `projectID` (ruling AS) | 6 / AP-6 |

### `GET /api/rooms/{id}/summary`

The room's top layer, folded from its log, which the room page and `roomctl status` render. It is a
room read: the same room access and role check as `GET /v1/ws`. Query: `after=N` (default `0`)
keeps only the notes after `seq` N; pass back the number of the previous answer's `cursor`.

| Status | When |
|---|---|
| `200` | The summary below |
| `400` | `after` is not a non-negative integer |
| `404` | No such room, or one the caller may not read: the same `no such room` (D7) |
| `503` | `access_unverified`: ZITADEL or GitHub cannot confirm the caller's access past the cache; or the rooms or the log are unavailable. Retry |

| Field | Holds |
|---|---|
| `apiVersion` | `summary/v1`, the contract both renderers check |
| `room`, `url` | The room id and its page |
| `status` | `phase`: the task's, else the room's, and `Closed` or `Sealed` once the log is sealed. `run`, `budget`, `pr`, `issue`, `lastVerdict`: the factory's latest facts and the latest review verdict, each `null` when absent. Empty fields inside them are left out, such as a PR's `reviewers` before anyone reviews |
| `needsYou` | `[{kind: approval, id, what, deadline, url}]`: the pending approvals the caller could decide in the web UI. Each carries a link to its card in the room, never a command (ruling P18) |
| `actions` | What the caller may do now, by their standing: `queue` and `stop` carry a `cli` line; `steer`, for the driver, carries none, since `roomctl` never steers |
| `notes` | `{untrusted: true, items: [{at, run, text}]}`: the last 20 progress notes after `after`. `untrusted` is always `true`: they are the agents' claims, rendered as text |
| `cursor` | `seq:N`, the last `seq` folded |

A sealed room has no `needsYou` and no `actions`. A fork's summary starts at its `forked_from`: the
source's task, approvals, verdict and notes are not the fork's.

### `GET /v1/ws`

| Status before upgrade | When |
|---|---|
| `401` | Not authenticated, or the token is already past its expiry |
| `403` | A foreign `Origin` (T9); not in an agents group |
| `404` | No such room, or one the caller may not read: the same `no such room` (D7) |
| `429` | More than 10 connections for this person, or more than 20 people in this room (per replica, ruling P22) |
| `503` | The log is unavailable; `access_unverified`: ZITADEL or GitHub cannot confirm the caller's access past the cache |

One JSON object per text frame (Appendix B).

| Direction | Frame | Fields |
|---|---|---|
| client → broker | `hello` | `roomId`, `afterSeq?` (clamped to the mark: the `sync` frame sets the baseline) or `tail?` (default: the last 500 events). Must be the first frame, else the socket closes `1008 hello first` |
| client → broker | `act` | `clientSeq`, `action`, `driverEpoch?` (phase 4 onwards; until then every act is acked `rejected: not_permitted`) |
| client → broker | `ping` | Every 30 s |
| broker → client | `state` | `throughSeq`, `snapshot: {roomId, phase, driver, driverEpoch, dataClass, you, runs, queue, sealed}`. `queue` is the messages still queued for the next run, `[{ref, author, text}]` (redacted), read just after the mark: apply events past `throughSeq` over it. `sealed` is the log's seal at the mark (`phase` follows the Room and lags it). `approvals` is the pending approvals, `[{approvalId, runId, callId, class, action, expiresAt, seq}]`, read like `queue`: a card per approval, however far behind the tail its request is |
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
| `1008` | `no such room` | Stop: the caller may no longer read the room (D7, re-checked on every ping against the access cache, so within the TTL plus 30 s); a re-dial is refused `404` |
| `1013` | `access_unverified` | Reconnect after a backoff: ZITADEL or GitHub could not confirm the caller's access past the cache |

The broker pings every 30 s; a peer that does not answer within 10 s is disconnected without a
close frame. So is a peer that does not take a frame within 10 s (`write_timeout`): reconnect with
`afterSeq`. So is a client that sends no `hello` within 10 s.

### Actions (planned, phases 4–6)

| Action | Fields | Who (see [authorization](security.md#authorization)) | Phase |
|---|---|---|---|
| `message` | `text`, `delivery: none \| queued \| steering`, `to?` | Collaborator and up; `steering` driver only | 4 |
| `remove_queued`, `promote_queued` | `ref` | The author or the driver; promote: driver | 4 |
| `interrupt` | — | Driver | 4 |
| `driver_request`, `driver_give`, `driver_take` | `to?`, `reason?` | Request: collaborator; give: driver, to a collaborator or better (from the Room's members; an `agents-admin` who is not a member takes instead) or the room's system holder; take: owner or `agents-admin`, with a reason of at most 256 bytes | 4 |
| `start_run` | `role` (`implementer`, `reviewer`, `tester`, `triager`), `prUrl?` (reviewer only, a pull request of the room's repository), `egressProfiles?` (at most 8 names) | Driver, owner. A reviewer's task is its PR, else the one the latest agent handoff or review verdict names (ruling P24); any other role's is the fenced brief of the latest agent handoff and verdict, which consumes the queued messages it quotes. Recorded as `state_changed{run_requested}`, with the reviewer's `taskUrl`. Before SP3 the ack's `result` is the rendered `AgentRun` for the owner to apply (ruling P14). A replayed `clientSeq` acks the record without it; one retried after a failed record (`log_unavailable`) asks for the same run under the same idempotency key, so it never creates a second one ([integration](integration.md)) | 4 |
| `invite` | `principal`, `memberRole`, `approver` | Owner. At most 20 members; never demotes the driver-token holder below collaborator (`bad_action`): the holder hands the token over first | 4 |
| `close` | `reason?` | Owner | 4 |
| `decide` | `approvalId`, `decision: approved \| denied`, `reason?` (at most 1 KiB, redacted; the agent reads it) | Approver, owner, from the web UI. The first valid decision wins (`already_decided` for every later one, an expiry or a supersede included). With `approvals.fourEyes`, nobody who prompted the run decides (`four_eyes`). A replayed `clientSeq` acks the stored decision | 5 |
| `fork` | `seq`, `note?` (at most 1 KiB, redacted), `role?`, `prUrl?`, `egressProfiles?` (as `start_run`'s) | Watcher and up; also from `roomctl`. A sealed room forks only for its owner or an `agents-admin` (`sealed`). A new room owned and driven by the forker, with the source's data class, repository and retention and none of its members. Its approvals are the source's for the source's owners (owner, owner members, `agents-admin`); anyone else gets, class by class, the stricter of the source's and a new room's (`attended`), so an `unattended` or `allow` override never carries over to a watcher: events `1..seq` copied with their `seq`, then `state_changed{forked_from}`. With a `role`, the new room's first run, on `agent/<new room>` from the latest agent commit at or before `seq`, on the forker's token and budget. The ack's `seq` is the new room's `forked_from`; its `result` is `{roomId, run?, runError?}`: `run` is the `start_run` claim or `null`, `runError` its rejection. A failed run leaves the fork made. A prefix over 5,000 events or 32 MiB is `too_large`; a principal forks 3 times at once, then once a minute, per replica | 6 |

| `rejected` | Meaning |
|---|---|
| `not_permitted` | The caller's room role, flag or client does not allow it; or the factory refused the caller a run |
| `stale_epoch` | `driverEpoch` no longer matches: someone else holds the token now |
| `rate_limited` | Over 10 actions per second (burst 20), or over 3 forks at once then one a minute; per replica |
| `no_running_run` | Steering or interrupt with no run `Running` |
| `room_busy` | A run is already running; or a run was requested in the last 10 minutes and has not joined yet, and the broker does not know it ended (a claim not applied, or a factory run not seen yet). Two replicas answering a `start_run` in the same instant can both pass this check until SP3's factory refuses a second pending run per room |
| `reviewer_needs_pr` | A reviewer's `start_run` with no `prUrl`, and no pull request of the room's repository in the latest agent handoff or review verdict. Human chat, queued text and tool output never choose it |
| `over_budget` | The factory refused the run: over the caller's budget |
| `factory_unavailable` | The run could not be requested: no requester, or the factory failed or answered no run id. Retry |
| `bad_action` | Malformed, or about another room; a give to someone who cannot hold the token; a `clientSeq` this connection already used for an action of another type |
| `not_queued` | The queued message was already delivered or removed |
| `sealed` | The room's log is sealed; a fork of it by anyone but its owner or an `agents-admin` too. A queued message that reached the room's event limit is refused this way but stays in the log, unqueued: the transcript can show it as the room's last message |
| `conflict` | The Room changed under an `invite`: retry |
| `log_unavailable` | The log or the Room could not be read or written: retry |
| `too_large` | A fork's prefix over 5,000 events or 32 MiB: fork at an earlier `seq` |
| `already_decided` | Another approver decided first (phase 5) |
| `four_eyes` | The room requires an approver who did not prompt the turn (OD-16, phase 5) |

## `:8090` — room tools (phase 3 / AP-3)

An MCP server on `POST /mcp`, reached only through an `agent-router` `MCPRoute`, which authenticates
the run first and injects a generated key in `X-Room-Mcp-Key` (ruling P13, `ROOMS_MCP_KEY`). The
broker derives the run from `X-Ar-Agent`, the gateway's verified `sub`, matched whole against each
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

`tools/list` lists the tools of the run's role. A call needs the role and the §1 matrix's action
(`read` for `room_read`, `chat` for the three writes), then spends the run's one call per second.
The limit is per broker replica: with the two replicas of phase 2, a run whose calls the gateway
spreads across both can make two a second (review M4). Arguments are strict: an unknown field is refused, and text holds
no control character but tab, newline and carriage return. Every write is redacted before it is
appended, attributed to `agent:<runId>` with the run's role, origin `client`.

| Tool | Roles | Arguments | Result | Appends |
|---|---|---|---|---|
| `room_read` | all | `sinceSeq` ≥ 0, `limit` 1–100 (default and cap 100) | `{events, lastSeq}`: the room's `message` and `handoff` events, redacted, at most 1 MiB of payload a reply and 500 events scanned. Pass `lastSeq` as the next `sinceSeq` | nothing |
| `room_post` | all | `text`, 1–16 384 bytes | `{seq}` | `message{kind: chat}`, delivered to nobody |
| `room_progress` | all | `text`, one line of 1–280 characters; one note a minute per run per broker replica, on top of the call limit | `{seq}` | `message{kind: progress}`, delivered to nobody; a refused note over the minute answers `rate_limited: one progress note a minute` |
| `room_handoff` | implementer, tester, triager | `toRole`, `summary` (1–8 192 bytes), `commit` (lowercase hex, 7–40) | `{seq}` | `handoff{fromRole, toRole, summary, commit, branch}`; `fromRole` and `branch` from the `AgentRun` |
| `room_verdict` | reviewer, tester | `verdict: approve \| changes`, `summary`, `commit` | `{seq}` | `message{kind: review_verdict, verdict, commit, pullRequest}`: `pullRequest` is the run's `spec.task.url` when it is a pull request of `spec.repository`, else absent. The leader then posts it on the PR |

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
twice; both copies are attributed and visible (ruling P26).
