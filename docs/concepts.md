# Concepts

A **room** is one append-only, ordered log for a piece of work. Humans and agent runs are its
participants. Everything they do, and everything that happens to them, becomes an event in that log
with a gapless sequence number that only the broker assigns. The rest of this page names the parts.

## Glossary

| Term | Meaning | Where it lives | Status |
|---|---|---|---|
| **Room** | One session: its policy (owner, driver, members, approvals, retention, data class) and its log. Rooms are runtime objects, created by SP3's factory, the broker or the owner, never committed to Git | `Room` CR (`agents.ogenki.io/v1alpha1`) in `agent-system`, named with a C2 id; its log in Postgres | AP-1 (the CRD, task 1.5, [reference below](#the-room-crd)) |
| **Run** | One agent, one role, one task, one branch, with a deadline. It joins a room by naming it in `spec.roomRef`. One run is `Running` per room at a time | `AgentRun` claim (`cloud.ogenki.io`) in `agents`, owned by SP1 | Built in SP1 |
| **Harness** | The agent loop inside the sandbox: the OpenHands agent-server on `127.0.0.1:8000` | The run's pod | Built in SP1 |
| **Bridge** | `room-bridge`, a native sidecar in every run pod that has a `roomRef`. It polls the harness and pushes its events to the broker; it carries steering, interrupts and decisions back | The run's pod, namespace `agents` | AP-1 |
| **Broker** | `room-broker`, the stateless service that owns rooms: it authenticates writers, redacts, sequences and stores events, and serves them | `agent-system` | AP-1 |
| **Participant** | Anyone in a room: a human, a run, or a system principal. Joining and leaving are `participant` events | The log | AP-1 for runs; planned, phase 2 for humans |
| **Principal** | A canonical identity: `agent:<runId>`, `human:<zitadel sub>` or `system:<component>` (C2). The broker stamps it on every event from the authenticated credential | Every event's `actor.id` | AP-1 |
| **C2 id** | 8 characters of lowercase unpadded base32, `[a-z2-7]{8}`. Room, run and task ids share the format, so `agent/<id>` branches match the merge gate | Names of rooms and runs | AP-1 |
| **Run role** | What a run may do: `implementer`, `reviewer`, `tester` or `triager` (C2). A policy input, not an identity | `AgentRun.spec.role`; an agent event's `actor.role` | Built in SP1 |
| **Room role** | What a human may do in a room: `watcher` < `collaborator` < `owner`, cumulative. **Approver** is a separate flag | `Room.spec.members` | Planned, phase 2 / AP-2 |
| **Driver** | The one principal allowed to steer or interrupt the running run. A token that moves with *give*, *request* and *take*; each change bumps `driverEpoch`, which fences races | `rooms.driver`, `driver_epoch`; `driver` events | Planned, phase 4 / AP-4 |
| **Message delivery** | `none` (chat, delivered to nobody), `queued` (FIFO, goes into the next run's brief) or `steering` (injected into the running run now, driver only) | `message` payload | `none` AP-1; the others planned, phase 4 |
| **Handoff** | A run hands *work* to the next role, with the commit it stopped at. The next run starts from that commit. Not a change of driver | `handoff` event | AP-3 |
| **Verdict** | A reviewer's or tester's `approve` or `changes`, recorded as `message{kind: review_verdict}`. The broker also posts it on the pull request as one comment | The log; the PR | AP-3 |
| **Brief** | The fenced, bounded text the next run starts from: the last handoff and verdict, plus the queued messages, quoted as untrusted data | Built by the broker | Planned, phase 4 / AP-4 |
| **Approval** | A human decision on a pending harness action. Oversight, not a security boundary: a run's capabilities are fixed at creation. The first valid decision wins | `approval_requested`, `approval_decided` | Planned, phase 5 / AP-5 |
| **Fork** | A new room copied from a prefix of another, events `1..N` with their `seq`, owned and driven by whoever forked it | New `Room`; `state_changed{forked_from}` | Planned, phase 6 / AP-6 |
| **Envelope** | The frozen C4 v1 shape of every log entry: `v, id, seq, roomId, runId, actor, type, causedBy, origin, ts, redactions, payload` | [Event envelope](event-envelope.md) | AP-1 |
| **seq** | The per-room sequence number: gapless from 1, assigned by the broker under the room's row lock | `events.seq`, `rooms.last_seq` | AP-1 |
| **Origin** | Who produced an event: `harness` (mirrored from a run), `client` (a human, a system caller, a room tool) or `broker` | Every event | AP-1 |
| **Idempotency key** | `(roomId, originClient, originSeq)`: a writer's own sequence number. A replayed key returns the stored event and appends nothing | `events` unique index | AP-1 |
| **Seal** | The last event of a room. After it, nothing can append. A room seals when it closes or fills up (100 000 events or 256 MiB), and sealing starts its retention clock | `rooms.sealed`, `closed_at` | AP-1 |
| **Retention** | How long a closed room's log is kept: `spec.retention`, `<n>d`, default `90d` (OD-17). A daily job deletes expired sealed rooms | `rooms.retention` | AP-1 (the column and `room-broker retention`); the CronJob S1 (planned) |
| **Bridge lease** | Which run's bridge holds the room. A second live run's bridge is refused `409 room_busy`. With Ruling Y, the lease also fences writes: a displaced bridge cannot append | `rooms.bridge_run`, `bridge_seen_at` | AP-1, fencing from Ruling Y |
| **Data class** | `public` or `internal` (C3): which model backends a run may reach. A room's runs inherit its class; an internal room's verdict comment carries no summary. Fixed at creation (ruling TD) | `Room.spec.dataClass` | AP-1 (the field); the verdict rule AP-3 |
| **Tier** | A logical model name on the agent gateway: `tier-light`, `tier-standard`, `tier-frontier` (C5), chosen once per task by SP3's classifier (C7). Rooms do not carry or change it: escalating a task means a new run, never a switch inside one | `AgentRun.spec.model` | Owned by SP4 |
| **Redaction** | Replacing a detected secret with `[REDACTED:<rule>]` before the event is stored, and listing the rule in `redactions` | The broker | AP-1 |
| **End reason** | Why a run ended: `agent_finished`, `agent_error`, `agent_stuck`, `deadline`, `pod_lost`, `revoked`, `deleted`, `budget-run`, or a `BudgetExhausted` run's `agents.ogenki.io/revoked` annotation (ruling P15). That annotation is free text, so it is redacted, then cut to 64 bytes. The `AgentRun` only ever says `Failed`; the room says why | `state_changed{run_phase}` | AP-1 |

## The Room CRD

`Room` (`agents.ogenki.io/v1alpha1`), namespaced, with a `status` subresource. AP-1, task 1.5
(`api/v1alpha1`, generated into `config/crd/agents.ogenki.io_rooms.yaml`). `kubectl get rooms`
prints `Phase`, `Driver`, `Seq`, `Pending` and `Class`.

**Name.** A CEL rule requires a C2 id: `self.metadata.name.matches('^[a-z2-7]{8}$')`, message
"a Room is named with a C2 id: 8 characters of [a-z2-7]".

| `spec` field | Type | Required | Default | Validation | Meaning |
|---|---|---|---|---|---|
| `owner` | string | yes | — | `^(human:[A-Za-z0-9@._-]+\|system:[a-z0-9-]+)$`, at most 261 characters | Who owns the room. 261 = OIDC's 255-character `sub` plus `human:` |
| `driver` | string | yes | — | Same as `owner` | The initial driver-token holder; afterwards the log decides |
| `members` | list, map keyed by `principal` | no | — | At most 20 | Humans only; runs join through their own `spec.roomRef` |
| `members[].principal` | string | yes | — | `^human:[A-Za-z0-9@._-]+$`, at most 261 characters | |
| `members[].role` | string | yes | — | `watcher`, `collaborator`, `owner` | Cumulative room role |
| `members[].approver` | bool | no | `false` | — | The approver flag |
| `approvals` | object | no | `{}` | — | Present even when omitted, so its own defaults apply |
| `approvals.profile` | string | no | `attended` | `attended`, `unattended` | The §6 profile table (phase 5) |
| `approvals.overrides` | map | no | — | Keys: `forge.push`, `forge.pr`, `forge.other`, `mcp.write`, `shell.high`; values: `allow`, `deny`, `human` | Per-class override of the profile |
| `approvals.ttl` | string | no | `4h` | `^[1-9][0-9]{0,3}(m\|h)$` (at most 9999h) | Auto-deny delay for an unattended approval |
| `approvals.fourEyes` | bool | no | `false` | — | OD-16: the approver must not have prompted the turn |
| `retention` | string | no | `90d` | `^[1-9][0-9]{0,3}d$` | How long the sealed log is kept (OD-17) |
| `dataClass` | string | **yes** | — | `public`, `internal`; immutable (ruling TD) | Runs requested for this room inherit it (C3) |
| `repository` | string | no | `Smana/cloud-native-ref` | `^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$` | The repository its runs work on (ruling P25) |

| `status` field | Type | Meaning |
|---|---|---|
| `phase` | string | `Open`, `Active`, `Idle`, `AwaitingHuman` or `Closed` ([below](#a-rooms-phases)) |
| `lastSeq` | integer | The log's high-water mark |
| `driver` | string | The current driver-token holder |
| `driverEpoch` | integer | Bumped on every driver change (phase 4) |
| `pendingApprovals` | integer | Undecided approvals (phase 5; 0 before) |
| `observedGeneration` | integer | The `spec` generation last projected |

The broker's leader writes `status` from the log (task 1.8). A minimal room:

```yaml
apiVersion: agents.ogenki.io/v1alpha1
kind: Room
metadata: {name: 3kq7x2ma, namespace: agent-system}
spec: {owner: "system:factory", driver: "system:factory", dataClass: public}
```

## A room's phases

The broker projects `status.phase` from the log and the room's runs every 15 s (ruling P21).

```mermaid
stateDiagram-v2
  [*] --> Open: Room created, log seq 1
  Open --> Active: a run is Running
  Active --> Idle: run ended, no successor yet
  Idle --> Active: the next run starts
  Active --> AwaitingHuman: approval pending, or no event for 30 min
  AwaitingHuman --> Active: decided, expired, or events resume
  Idle --> Closed: owner or SP3 closes, or the Room is deleted
  Active --> Closed: the log reaches its limit
  Closed --> [*]: retention elapsed, log purged
```

`AwaitingHuman` from an approval needs phase 5; from a stall it applies from phase 1. Deleting a
`Room` runs the finalizer `agents.ogenki.io/room-log`, which deletes the room's runs and seals the
log. The log keeps its own retention clock after the CR is gone.

## Who does what

| Actor | Creates | Reads | Writes into the log |
|---|---|---|---|
| Owner, before SP3 | `Room` CRs with `kubectl`; runs with `task agent:run -- --room` | SQL; the system API | nothing directly |
| SP3's factory | `Room` CRs and every `AgentRun` (C3: the only creator) | `GET /v1/rooms/{id}/events`, `GET /v1/rooms/{id}/queue` | `message{kind: task_state}`, the task's snapshot first; a maintainer's review as a queued `chat` (`POST /v1/rooms/{id}/queue`, stream `review`), consumed by the run whose brief quoted it |
| A run | nothing | `room_read` (phase 3) | its harness events through the bridge; `room_post`, `room_handoff`, `room_verdict` (phase 3) |
| A human | rooms from the UI (phase 4), forks (phase 6) | the web UI (phase 2), `roomctl` (phase 6) | messages, driver actions, decisions (phases 4–6) |
| The broker | the `Room`s a human asks for, new or forked (phases 4, 6); **never** an `AgentRun` | everything | room and run lifecycle, seals, limits, verdict outcomes |

A maintainer's review reaches the next run once queued: the factory re-reads the pull request
just before that run starts, so a review submitted while the task waits joins it, and a review
already dismissed is skipped. A review dismissed after it was queued still drives that run, since
the system API cannot remove a queued message.

The factory's other human inputs (§6.3):

| Input | Effect |
|---|---|
| A maintainer's comment with `/factory retry` alone on a line outside a code fence, on an escalated task's issue or pull request | A fresh run, through `Queued` and its caps, within 5 minutes (escalated tasks are polled every 5 minutes). Edited comments, and anyone else's, are ignored without an answer |
| No maintainer activity on an `AwaitingHuman` pull request for 48 h: no review, no comment on it or on the issue, no push without an `Agent-Run` trailer | One reminder mentioning the maintainers, for each quiet spell. A spell never restarts earlier than one already reminded |
| The same for 14 days, at least 24 h after that reminder, and the latest maintainer review is not an approval | The pull request is labelled `factory/stale` and closed, and the task ends `Closed` (`stale`). An approved pull request is never closed. A refused close takes the label off again |

The full permission matrix is in [Security](security.md#authorization).
