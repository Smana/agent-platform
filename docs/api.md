# API

The broker serves four listeners. Phase 1 builds only **`:8443`**, for room bridges and system
callers, and **`:9090`**, for metrics and probes. Humans get `:8080` in phase 2, agents' room tools
get `:8090` in phase 3. Every body is JSON.

| Port | Who calls it | Authentication | Phase / PR |
|---|---|---|---|
| `:8443` (TLS) | `room-bridge` in each run pod; system callers such as SP3's factory | Offline JWT: run tokens (audience `room-broker`) or system tokens (audience `rooms-system`) | AP-1 (planned, tasks 1.6, 1.9) |
| `:9090` | kubelet, `vmagent` | None: probes and metrics only | AP-1 (task 1.12) |
| `:8080` | Humans, through oauth2-proxy | ZITADEL ID token and access token | 2 / AP-2 |
| `:8090` | Agents' `room_*` tools, through the `agent-router` Gateway only | Injected key plus the gateway's verified `x-ar-agent` | 3 / AP-3 |
| `:8085` (bridge) | kubelet | None | AP-1 (task 1.11) |

`:8443`'s handlers and the store methods they call are written on the AP-1 branch (task 1.9), and
`room-broker serve` serves them (task 1.12). The rest is **planned**.

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
| `POST /v1/bridge/hello` | Bridge | Claims the room's bridge lease, returns where the log is | AP-1 (planned, task 1.9) |
| `POST /v1/bridge/events` | Bridge | Appends a batch of harness items | AP-1 (planned, task 1.9) |
| `GET /v1/bridge/stream` | Bridge | One SSE stream down: pings; `deliver` and `interrupt` from phase 4; `decision` from phase 5 | AP-1 (planned, task 1.9: pings); 4 / AP-4; 5 / AP-5 |
| `POST /v1/bridge/approvals` | Bridge | Asks for a human decision on a pending action | 5 / AP-5 |
| `GET /v1/rooms/{id}/events` | System | Reads a room's log | AP-1 (planned, task 1.9) |
| `POST /v1/rooms/{id}/messages` | System | Appends `message{kind: task_state}` | AP-1 (planned, task 1.9) |

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
| `409` | `room_busy` | Another run holds the room's lease: it is live and was seen within 2 minutes (ruling P17). The broker also appends `state_changed{kind: limit, reason: concurrent_run}` |
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
cannot keep either value: that item alone is stored as a `{"refused": true, "type": …}` stub and the
rest of the batch is appended.

Response `200`: `{"afterHarnessSeq": 36, "afterStatusSeq": 5}`, the highest key of the batch on each
stream, or `0` for a stream the batch did not carry. A replayed key is acknowledged without
appending again.

| Status | `error` | When | The bridge then |
|---|---|---|---|
| `400` | `bad_batch` | Not JSON, an unknown field, or data after the batch | Drops the batch and logs it |
| `400` | `bad_item` | Unknown type or stream, `seq` ≤ 0, a type or `state_changed` kind a bridge may not push ([allowlist](event-envelope.md#state_changed-kinds)), or a key spelled two ways | Drops the batch and logs it |
| `400` | `bad_payload` | The payload is not a JSON object | Drops the batch and logs it |
| `413` | `batch_too_large` | Over 2 MiB or over 500 items | Must split the batch, not drop it |
| `401` | `unauthenticated` | As for `hello` | Re-reads its token and retries |
| `403` | `run_not_live`, `run_has_no_room` | As for `hello` | Retries on the next tick |
| `409` | `lease_lost` | **Ruling Y:** this run no longer holds the room's lease, seen by the lease renewal or by the append's fence. Nothing is appended | Must not drop the batch: the events are not in the log. Keeps it and says hello again |
| `410` | `sealed` | The room is sealed | Stops mirroring |
| `429` | `rate_limited` | Over the run's request rate or requests in flight | Must not drop the batch: retries after `Retry-After` |
| `503` | `log_unavailable`, `timed_out` | The database refused the append, or the request's 30 s ran out before the batch was redacted | Retries; keeps buffering |

A payload Postgres refuses outright (SQLSTATE class 22) is not an error: it is stored as a
`{"refused": true}` stub so the cursor moves on.

### `GET /v1/bridge/stream`

A Server-Sent Events stream the bridge opens and holds (C4 r5: sandbox-initiated, no WebSocket).
Response `200`, `Content-Type: text/event-stream`. The broker writes a comment line, `: ping`, every
30 s. The stream ends at the token's expiry, and the bridge re-dials with a fresh token. A stream
carries only the events addressed to its own run.

| SSE `event` | `data` | Meaning | Phase / PR |
|---|---|---|---|
| `deliver` | `{"ref": 1846, "text": "…"}` | A steering message; the bridge injects it into the harness and acknowledges it as `state_changed{delivered, ref}` | 4 / AP-4 |
| `interrupt` | `{"ref": 1847}` | The driver interrupted the run; acknowledged as `state_changed{interrupted, ref}` | 4 / AP-4 |
| `decision` | `{"approvalId": "…", "allow": true, "reason": "…", "ref": 1851}` | An approval was decided; acknowledged as `state_changed{decision_applied, ref}` | 5 / AP-5 |

Errors before the stream opens are those of `hello`.

### `POST /v1/bridge/approvals` (planned, phase 5 / AP-5)

Request, at most 64 KiB: `{"callId": "call_97", "class": "forge.pr", "action": {…}}`. `class` is one
of `forge.push`, `forge.pr`, `forge.other`, `mcp.write`, `shell.high`. The action is redacted and
stored as the approval card shows it (T3). Response `200`: `{"approvalId": "…", "expiresAt": "…"}`.
The expiry is 30 minutes for an `attended` room, the room's `approvals.ttl` (default `4h`) for an
`unattended` one. The request is idempotent per `(room, run, callId)`.

| Status | `error` | When |
|---|---|---|
| `400` | `bad_approval` | Not JSON, no `callId`, or an unknown class |
| `400` | `bad_action` | The action is not a JSON document |
| `503` | `log_unavailable` | The database refused it |

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

## `:8080` — human API (planned, phase 2 / AP-2)

Reached only through oauth2-proxy on `rooms.<private domain>`. Every request carries the human's
ZITADEL **ID token** in `Authorization` and their **JWT access token** in `X-Forwarded-Access-Token`,
both from oauth2-proxy.

| Check | Rule |
|---|---|
| ID token | Issuer is the identity provider, audience holds the `rooms-proxy` client id, not expired |
| Access token | Same `sub`; issued for `rooms-proxy`. A web session requires it to differ from the ID token (review M16) |
| Groups | `agents-admin` or `agents-member`, else `403` |
| `Origin` | Must match the room host: no cross-site WebSocket (T9) |
| Lifetime | A connection lasts `min(token expiry, 1 h)`, then closes for re-authentication |

`roomctl` presents a bearer token from its own native ZITADEL client (phase 6); oauth2-proxy lets it
through with `skip-jwt-bearer-tokens`. Such a token can read, post, queue and fork, but the broker
refuses `decide`, `message{steering}`, `interrupt` and every `driver_*` action from it (ruling P18).

Errors before a WebSocket upgrade are plain-text HTTP errors.

| Method and path | Does | Phase / PR |
|---|---|---|
| `GET /`, `GET /r/{id}`, `GET /assets/{file}` | The embedded UI, under a strict Content Security Policy | 2 / AP-2 |
| `GET /api/rooms` | One row per room the caller may read: id, phase, owner, driver, data class, last `seq`, and the caller's own role | 2 / AP-2 |
| `GET /v1/ws?room=<id>` | The live room, over WebSocket | 2 / AP-2 |
| `POST /api/rooms` | `{"dataClass": "public", "repository": "Smana/cloud-native-ref"}` → `201 {"id": "…"}`: a new room owned and driven by the caller | 4 / AP-4 |
| `GET /api/roomctl` | `{url, issuer, clientID}` for the UI's CLI setup page | 6 / AP-6 |

### `GET /v1/ws`

| Status before upgrade | When |
|---|---|
| `401` | Not authenticated, or the token is already past its expiry |
| `403` | A foreign `Origin` (T9); not in an agents group; not allowed to read this room (`not_permitted`) |
| `404` | No such room |
| `429` | More than 10 connections for this person, or more than 20 people in this room (per replica, ruling P22) |
| `503` | The log is unavailable |

One JSON object per text frame (Appendix B).

| Direction | Frame | Fields |
|---|---|---|
| client → broker | `hello` | `roomId`, `afterSeq?` (clamped to the mark: the `sync` frame sets the baseline) or `tail?` (default: the last 500 events). Must be the first frame, else the socket closes `1008 hello first` |
| client → broker | `act` | `clientSeq`, `action`, `driverEpoch?` (phase 4 onwards; until then every act is acked `rejected: not_permitted`) |
| client → broker | `ping` | Every 30 s |
| broker → client | `state` | `throughSeq`, `snapshot: {roomId, phase, driver, driverEpoch, dataClass, you, runs}` |
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
| `1001` | `shutdown` | Reconnect: the replica is stopping |
| `1013` | `log_unavailable` | Reconnect with `afterSeq` after a backoff |

The broker pings every 30 s; a peer that does not answer within 10 s is disconnected without a
close frame. So is a peer that does not take a frame within 10 s (`write_timeout`): reconnect with
`afterSeq`.

### Actions (planned, phases 4–6)

| Action | Fields | Who (see [authorization](security.md#authorization)) | Phase |
|---|---|---|---|
| `message` | `text`, `delivery: none \| queued \| steering`, `to?` | Collaborator and up; `steering` driver only | 4 |
| `remove_queued`, `promote_queued` | `ref` | The author or the driver; promote: driver | 4 |
| `interrupt` | — | Driver | 4 |
| `driver_request`, `driver_give`, `driver_take` | `to?`, `reason?` | Request: collaborator; give: driver; take: owner or `agents-admin`, with a reason | 4 |
| `start_run` | `role`, `prUrl?`, `egressProfiles?` | Driver, owner. Before SP3 it returns the rendered `AgentRun` for the owner to apply (ruling P14) | 4 |
| `invite` | `principal`, `memberRole`, `approver` | Owner | 4 |
| `close` | `reason?` | Owner | 4 |
| `decide` | `approvalId`, `decision: approved \| denied`, `reason` | Approver, owner | 5 |
| `fork` | `seq`, `note`, `role?`, `prUrl?`, `egressProfiles?` | Watcher and up; also from `roomctl` | 6 |

| `rejected` | Meaning |
|---|---|
| `not_permitted` | The caller's room role, flag or client does not allow it |
| `stale_epoch` | `driverEpoch` no longer matches: someone else holds the token now |
| `rate_limited` | Over 10 actions per second (burst 20), per replica |
| `no_running_run` | Steering or interrupt with no run `Running` |
| `room_busy` | A run is already running |
| `bad_action` | Malformed, or about another room |
| `not_queued` | The queued message was already delivered or removed |
| `already_decided` | Another approver decided first (phase 5) |
| `four_eyes` | The room requires an approver who did not prompt the turn (OD-16, phase 5) |

## `:8090` — room tools (planned, phase 3 / AP-3)

An MCP server on `POST /mcp`, reached only through an `agent-router` `MCPRoute`, which authenticates
the run first and injects a generated key in `X-Room-Mcp-Key` (ruling P13). The broker derives the
run from `X-Ar-Agent`, the gateway's verified `sub`, and reads the run's role from its `AgentRun`,
never from a header. It exposes `tools/list` and `tools/call` only: no `resources` or `prompts`,
which the gateway would not authorize by role. One call per second per run.

| Tool | Roles | Arguments | Result | Appends |
|---|---|---|---|---|
| `room_read` | all | `sinceSeq`, `limit` ≤ 100 | `{events, lastSeq}`: the room's `message` and `handoff` events, redacted | nothing |
| `room_post` | all | `text` | `{seq}` | `message{kind: chat}`, delivered to nobody |
| `room_handoff` | implementer, tester, triager | `toRole`, `summary`, `commit` | `{seq}` | `handoff` |
| `room_verdict` | reviewer, tester | `verdict: approve \| changes`, `summary`, `commit` | `{seq}` | `message{kind: review_verdict, pullRequest}`; the leader then posts it on the PR |

Tool calls carry no idempotency key (MCP request ids are per session), so a retried call can append
twice; both copies are attributed and visible (ruling P26).
