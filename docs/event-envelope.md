# The event envelope (C4 v1)

Every durable entry of a room's log is one **C4 envelope, version 1**. Writers hand the broker a
type, a payload and an idempotency key; the broker assigns `seq`, `id` and `ts` and stamps `actor`
from the authenticated credential. **A value a client sends for those fields is ignored.** SP2 owns
the shape and it is frozen: changing it is a programme-level change to contract C4, made in the
programme design first.

Status: the Go types, validation and limits are **AP-1** (`internal/envelope`). Payloads marked with
a later phase are defined now but produced only from that phase.

## Fields

| Field | Type | Set by | Rule |
|---|---|---|---|
| `v` | integer | broker | Always `1` |
| `id` | string | broker | A ULID (26 characters, Crockford base32), unique per event |
| `seq` | integer | broker | Gapless per room, from 1. Assigned under the room's row lock |
| `roomId` | string | broker | A C2 id, `^[a-z2-7]{8}$` |
| `runId` | string | broker | The run the event belongs to, a C2 id. Omitted when the event belongs to no run |
| `actor` | object | broker | `{kind, id, role?}`, from the credential. See [actors](#actors) |
| `type` | string | writer | One of the ten [types](#payloads-by-type) |
| `causedBy` | integer | writer | The `seq` this event answers, when there is one. Omitted otherwise |
| `origin` | string | broker | `harness`, `client` or `broker`. See [origins](#origins) |
| `ts` | string | broker | RFC 3339, UTC, up to microsecond precision (Go drops trailing zeros) |
| `redactions` | array of strings | broker | The redaction rule ids that fired on this payload. Always present, `[]` when none |
| `payload` | object | writer, then redacted | The type's payload, at most 64 KiB |

```json
{
  "v": 1,
  "id": "01JB6QM9S346Q3D25VT4F5V37E",
  "seq": 1842,
  "roomId": "3kq7x2ma",
  "runId": "7f3cq2xz",
  "actor": { "kind": "agent", "id": "agent:7f3cq2xz", "role": "reviewer" },
  "type": "tool_result",
  "causedBy": 1840,
  "origin": "harness",
  "ts": "2026-09-23T14:02:11.482913Z",
  "redactions": ["github-app-token"],
  "payload": { "callId": "call_42", "status": "ok", "output": "token=[REDACTED:github-app-token]", "truncated": false, "bytes": 51 }
}
```

A unit test pins exactly these twelve keys on the wire.

## Actors

| `kind` | `id` | `role` | Who |
|---|---|---|---|
| `agent` | `agent:<runId>` | The run's `spec.role` (`implementer`, `reviewer`, `tester`, `triager`) | A run, through its bridge or a room tool |
| `human` | `human:<zitadel sub>` | — | A person, through the web UI or `roomctl` (phase 2 onwards) |
| `system` | `system:<component>` | — | `system:room-broker` for the broker's own events; `system:factory` for SP3; `system:policy` for deterministic approval rules (phase 5) |

The broker derives `agent:<runId>` from the token's `sub`
(`system:serviceaccount:agents:xplane-run-<runId>`), never from a header or a request body.
Agent-originated events are **untrusted content attributed to that agent** (C4).

## Origins

| `origin` | Meaning | Examples |
|---|---|---|
| `harness` | Mirrored from a run's harness by its bridge | `message`, `tool_call`, `tool_result`, `turn`, `state_changed{harness_status}` |
| `client` | Written by a participant through an API | A system caller's `task_state`; a human's message (phase 4); a room tool's handoff or verdict (phase 3) |
| `broker` | Written by the broker itself | `room_phase`, `run_phase`, `participant`, the seal, `limit`, verdict outcomes |

## Idempotency

Every append carries an idempotency key, `(roomId, originClient, originSeq)`, held in a unique
database index rather than in the envelope. A key already stored returns the stored event, appends
nothing and consumes no `seq`, so retries, restarts and a new leader never duplicate an event.

| `originClient` | `originSeq` | Writer | Status |
|---|---|---|---|
| `agent:<runId>` | Item key of the harness event: event *i* owns `4i … 4i+3` | The bridge, harness events | AP-1 |
| `agent:<runId>:status` | A counter of status transitions | The bridge, status tracker | AP-1 |
| `system:<name>` | The caller's `clientSeq` | The system API | AP-1 |
| `broker:room` | `1` | The Room controller, `room_phase: Open` | AP-1 |
| `broker:run:<runId>` | `1` joined, `2` running, `3` ended, `4` left | The leader's run events | AP-1 |
| `broker:seal` | `1` | The seal | AP-1 |
| `broker:busy:<runId>` | `1` | The `concurrent_run` limit event | AP-1 |
| `human:<sub>` | The browser's `clientSeq` | Human actions | Planned, phase 4 |
| `agent:<runId>:tools` | Unix nanoseconds (ruling P26: MCP has no retry key) | Room tools | AP-3 |
| `agent:<runId>:approvals` | Unix nanoseconds; the approval itself is unique per `(room, run, callId)` | The bridge's approval requests | Planned, phase 5 |
| `broker:verdicts` | The verdict's `seq` | The verdict poster's outcome | AP-3 |

## Limits

| Limit | Value | What happens | Status |
|---|---|---|---|
| Payload | 64 KiB (C4) | Stored as a stub, `{"oversize": true, "bytes": N, "type": "<type>"}`, never refused: a refused harness event would block the bridge's cursor forever (ruling P20). The content stays in the pod until it ends | AP-1 |
| Tool output | 16 KiB | Truncated by the bridge; `truncated: true` and `bytes` keeps the original length | AP-1 |
| Human message, system message text | 16 KiB | `400 bad_message` | AP-1 (system API); phase 4 (humans) |
| A value Postgres refuses (SQLSTATE class 22), or keys that collide once redacted | — | Stored as `{"refused": true, "type": "<type>", "reason": "invalid_value" \| "key_collision"}` so the cursor moves on | AP-1 (task 1.9; `reason` from task 1.12) |
| NUL characters | — | Stripped from every string and key before storage (`jsonb` refuses them) | AP-1 |
| Room size | 100 000 events or 256 MiB | The room is sealed with a final `state_changed{kind: limit, events, bytes}` | AP-1 |
| Bridge batch | 2 MiB per request; the bridge sends at most 100 items | `413 batch_too_large` above 2 MiB or 500 items | AP-1 |

Streaming deltas, presence and typing never enter the log (C4). Human presence is not built at all
(ruling P27).

## Payloads by type

From the design's Appendix A, with the plan's additive fields.

| `type` | Payload | Written by | Status |
|---|---|---|---|
| `message` | `{kind: chat \| review_verdict \| task_state, text, to[], delivery: none \| queued \| steering}`. `review_verdict` adds `{verdict: approve \| changes, commit}`, and `pullRequest` (additive, phase 3, P29). SP3 defines `task_state`'s text | Harness (chat), system callers (`task_state`), room tools and humans | `chat` and `task_state` AP-1; `review_verdict` phase 3; `queued`, `steering` phase 4 |
| `turn` | `{runId, turnId, phase: started \| completed \| cancelled \| failed}` | The bridge's status tracker | AP-1 |
| `tool_call` | `{callId, tool, args, class, risk, decidedBy: policy \| human \| null}` | The bridge | AP-1; `class` and `decidedBy` from phase 5 |
| `tool_result` | `{callId, status: ok \| error \| rejected, output, truncated, bytes}` | The bridge | AP-1 |
| `approval_requested` | `{approvalId, callId, class, action, expiresAt}`. `action` is the raw call, redacted | The broker, from the bridge | Planned, phase 5 |
| `approval_decided` | `{approvalId, decision: approved \| denied \| expired, reason}` | A human, `system:policy`, or the expiry sweeper | Planned, phase 5 |
| `participant` | `{principal, change: joined \| left \| role_changed, role, approver}` | The broker | Runs AP-1; humans phase 2 |
| `driver` | `{from, to, epoch, reason: given \| requested \| taken: <the owner's reason, redacted> \| lease_expired}` | The broker | Store AP-4 (Task 4.1); written by human actions from Task 4.2 |
| `handoff` | `{fromRole, toRole, summary, commit, branch}` | `room_handoff` | AP-3 |
| `state_changed` | `{kind, …}`, one of the kinds below | Broker or bridge | Per kind |

### `state_changed` kinds

| `kind` | Fields | Written by | Status |
|---|---|---|---|
| `room_phase` | `phase: Open`, `owner`, `driver`, `dataClass`; or `phase: Closed`, `reason` (the seal) | Broker | AP-1 |
| `run_phase` | `phase`, and when it ended a `reason` (see [end reasons](concepts.md#glossary)) | Broker (leader) | AP-1 |
| `limit` | `events`, `bytes` (the seal of a full room); or `reason: concurrent_run`, `running` (P17) | Broker | AP-1 |
| `harness_status` | `status`, `previous` (the harness's `execution_status`) | Bridge | AP-1 |
| `harness_error` | `code`, `detail`: the harness's own error, or the bridge's stall on its log (`event_too_large`, `cursor_lost`, `next_page_unreadable`), told once per stall | Bridge | AP-1 |
| `harness_paused` | — | Bridge | AP-1 |
| `harness_event` | `harnessKind`: an event kind the pinned harness version did not have, recorded without its content; or `malformed`, `oversize` or `refused`, a stub keeping the slot of an item the broker could not take, with `detail` (its type), `bytes` and the broker's `reason` | Bridge | AP-1 |
| `verdict_posted`, `verdict_not_posted` | `verdictSeq`, and `url` or `reason`: `no_pull_request`, `github_refused` with `detail` `http_<status>` or `too_many_comments`, or `expired` with `detail: window_24h` | Broker (leader) | AP-3 |
| `interrupt` | `runId` | Broker, on the driver's interrupt | Planned, phase 4 |
| `delivered`, `interrupted` | `ref`, `runId`: the bridge's acknowledgement of a delivery; `interrupted` without `ref` is the harness's own `InterruptEvent` | Bridge | `interrupted` from the harness AP-1; acknowledgements AP-4 (Task 4.3) |
| `undeliverable` | `ref`, `runId`, `code`: the harness refused the delivery for good, with that HTTP status; the stream goes on past it | Bridge | AP-4 (Task 4.3) |
| `queued_removed` | `ref` | Broker | Planned, phase 4 |
| `run_requested` | `role`, `runId`, `via: manifest \| factory`, `baseRef`, `taskUrl` (a reviewer's PR), `consumed`: the refs of the queued messages its brief quoted, moved to `consumed` in the same transaction | Broker, on a human's `start_run` | AP-4 (Task 4.4) |
| `policy_decision`, `decision_applied` | `callId`, `class`, `decision`; or `ref`, `runId` | Bridge | Planned, phase 5 |
| `forked_from` | `room`, `seq`, `note` | Broker | Planned, phase 6 |
| `commit` | — | — | Listed in Appendix A; no plan task writes it yet |

A bridge may push only `message{kind: chat, delivery: none}`, `turn`, `tool_call`, `tool_result`, and
the `state_changed` kinds `harness_*`, `delivered`, `interrupted`, `undeliverable`, `policy_decision` and
`decision_applied`. Anything else is `400 bad_item`, so a compromised sandbox cannot forge a verdict,
a driver change or a decision (review M5).

## Where harness events come from

The bridge maps each OpenHands agent-server event to zero, one or two C4 items (AP-1).

| Harness event | C4 |
|---|---|
| `MessageEvent` | `message{kind: chat}`; from the user source it is addressed `to: ["agent:<runId>"]` |
| `ActionEvent` | `tool_call`, preceded by a `message` holding the agent's thought when it has one |
| `ObservationEvent` | `tool_result`, `status: ok` or `error` |
| `UserRejectObservation` | `tool_result`, `status: rejected` |
| `AgentErrorEvent` | `tool_result`, `status: error` |
| `ConversationErrorEvent` | `state_changed{harness_error}` |
| `PauseEvent` | `state_changed{harness_paused}` |
| `InterruptEvent` | `state_changed{interrupted, runId}`; the status tracker ends the turn (Ruling AL) |
| Any other kind | `state_changed{harness_event, harnessKind}` |
| System prompt, token, condensation and completion-log events; streaming deltas | Dropped |
| A change of `execution_status` | `state_changed{harness_status}`, plus `turn{started}` on entering `running`, and `turn{completed \| cancelled \| failed}` on leaving it |

## One example per type

`message`, a chat line mirrored from the harness:

```json
{"v":1,"id":"01JB6Q3S3E28JT97KB6CQ643DZ","seq":5,"roomId":"3kq7x2ma","runId":"7f3cq2xz",
 "actor":{"kind":"agent","id":"agent:7f3cq2xz","role":"implementer"},"type":"message",
 "origin":"harness","ts":"2026-09-23T14:00:03.120044Z","redactions":[],
 "payload":{"kind":"chat","text":"I will list the docs directory first.","delivery":"none"}}
```

`message`, a reviewer's verdict through `room_verdict` (phase 3):

```json
{"v":1,"id":"01JB6QVMXXQKFBF5KZNWJ47TAN","seq":1860,"roomId":"3kq7x2ma","runId":"aaaaaaaa",
 "actor":{"kind":"agent","id":"agent:aaaaaaaa","role":"reviewer"},"type":"message",
 "origin":"client","ts":"2026-09-23T15:10:44.002311Z","redactions":[],
 "payload":{"kind":"review_verdict","text":"Add a test for the empty case.","delivery":"none",
   "verdict":"changes","commit":"4be1c9d","pullRequest":"https://github.com/Smana/cloud-native-ref/pull/2114"}}
```

`turn`:

```json
{"v":1,"id":"01JB6Q9ZT24MNPZX45HY43KWJR","seq":4,"roomId":"3kq7x2ma","runId":"7f3cq2xz",
 "actor":{"kind":"agent","id":"agent:7f3cq2xz","role":"implementer"},"type":"turn",
 "origin":"harness","ts":"2026-09-23T14:00:02.884301Z","redactions":[],
 "payload":{"runId":"7f3cq2xz","turnId":"t1","phase":"started"}}
```

`tool_call`:

```json
{"v":1,"id":"01JB6QP1XPA7Z3DJ8FSSZ5AWSH","seq":6,"roomId":"3kq7x2ma","runId":"7f3cq2xz",
 "actor":{"kind":"agent","id":"agent:7f3cq2xz","role":"implementer"},"type":"tool_call",
 "origin":"harness","ts":"2026-09-23T14:00:03.120101Z","redactions":[],
 "payload":{"callId":"call_42","tool":"terminal","args":{"command":"ls docs"},"risk":"LOW","decidedBy":null}}
```

`tool_result`:

```json
{"v":1,"id":"01JB6Q8VHTPRE95B9EE0ZBGJ09","seq":7,"roomId":"3kq7x2ma","runId":"7f3cq2xz",
 "actor":{"kind":"agent","id":"agent:7f3cq2xz","role":"implementer"},"type":"tool_result",
 "causedBy":6,"origin":"harness","ts":"2026-09-23T14:00:04.41773Z","redactions":[],
 "payload":{"callId":"call_42","status":"ok","output":"README.md\narchitecture\nrunbooks","truncated":false,"bytes":31}}
```

`approval_requested` (phase 5):

```json
{"v":1,"id":"01JB6QTQM83XSSSS6YS3C4DWA7","seq":1850,"roomId":"3kq7x2ma","runId":"7f3cq2xz",
 "actor":{"kind":"agent","id":"agent:7f3cq2xz","role":"implementer"},"type":"approval_requested",
 "origin":"harness","ts":"2026-09-23T14:30:00.00012Z","redactions":[],
 "payload":{"approvalId":"01JB6QN36096Q14DR9GPQY77ZX","callId":"call_97","class":"forge.pr",
   "action":{"tool":"terminal","args":{"command":"gh pr create --fill"}},"expiresAt":"2026-09-23T15:00:00Z"}}
```

`approval_decided` (phase 5):

```json
{"v":1,"id":"01JB6QYYK596NGYA1DQ91K5GQA","seq":1851,"roomId":"3kq7x2ma","runId":"7f3cq2xz",
 "actor":{"kind":"human","id":"human:291847362183"},"type":"approval_decided",
 "causedBy":1850,"origin":"client","ts":"2026-09-23T14:31:12.55Z","redactions":[],
 "payload":{"approvalId":"01JB6QN36096Q14DR9GPQY77ZX","decision":"approved","reason":"PR scope matches the task"}}
```

`participant`:

```json
{"v":1,"id":"01JB6QPENECFSECZP11HYGCPWP","seq":2,"roomId":"3kq7x2ma","runId":"7f3cq2xz",
 "actor":{"kind":"system","id":"system:room-broker"},"type":"participant",
 "origin":"broker","ts":"2026-09-23T13:59:58.0012Z","redactions":[],
 "payload":{"principal":"agent:7f3cq2xz","change":"joined","role":"implementer"}}
```

`driver` (phase 4):

```json
{"v":1,"id":"01JB6QQ5E6EYCNDY0YP57RCYBV","seq":1845,"roomId":"3kq7x2ma",
 "actor":{"kind":"human","id":"human:291847362183"},"type":"driver",
 "origin":"client","ts":"2026-09-23T14:20:01.0003Z","redactions":[],
 "payload":{"from":"system:factory","to":"human:291847362183","epoch":8,"reason":"requested"}}
```

`handoff` (phase 3):

```json
{"v":1,"id":"01JB6QN5SXS5AA819X9YP98106","seq":1848,"roomId":"3kq7x2ma","runId":"7f3cq2xz",
 "actor":{"kind":"agent","id":"agent:7f3cq2xz","role":"implementer"},"type":"handoff",
 "origin":"client","ts":"2026-09-23T14:45:09.31Z","redactions":[],
 "payload":{"fromRole":"implementer","toRole":"reviewer","summary":"Fixed the broken link and added a check.",
   "commit":"4be1c9d","branch":"agent/3kq7x2ma"}}
```

`state_changed`, a run's end reason:

```json
{"v":1,"id":"01JB6QD7W2K4M6P8R0T2V4X6Z8","seq":1849,"roomId":"3kq7x2ma","runId":"7f3cq2xz",
 "actor":{"kind":"system","id":"system:room-broker"},"type":"state_changed",
 "origin":"broker","ts":"2026-09-23T14:46:30Z","redactions":[],
 "payload":{"kind":"run_phase","phase":"Succeeded","reason":"agent_finished"}}
```
