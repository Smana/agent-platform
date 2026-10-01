# Operations

Operate rooms through three windows: the **`Room` CR's status** (`kubectl get room -n agent-system`),
the broker's **metrics and alerts**, and **SQL on the log**. Nothing here is deployed yet: the
manifests land with cloud-native-ref's S1 (phase 1) and grow with each later phase.

## Health at a glance

```bash
kubectl get rooms -n agent-system                                   # PHASE, DRIVER, SEQ, PENDING, CLASS
kubectl get pods -n agent-system -l app.kubernetes.io/name=room-broker
kubectl get sqlinstance,atlasmigration -n agent-system              # xplane-rooms Ready, schema migrated
kubectl get cluster -n agent-system xplane-rooms-cnpg-cluster       # the CNPG cluster
flux get kustomization room-broker -n flux-system
```

| Probe (`:9090`) | Fails when |
|---|---|
| `/startupz` | The schema is not migrated yet. The first deploy waits up to 10 minutes for CNPG and Atlas |
| `/readyz` | Postgres does not answer, or the Kubernetes caches are not synced |
| `/healthz` | The process is wedged |

The bridge's `/healthz` on `:8085` fails only when the harness was reachable and has been silent for
over 60 s; it never depends on the broker.

## Metrics

Scraped from `:9090/metrics` by a `VMServiceScrape` (S1). The §9 set, plus four the alerts need and a
build-info gauge. All are defined in phase 1; the last column is the phase whose
feature a metric measures. Ruling AC: the broker records them through the OpenTelemetry metric API
with a Prometheus exporter that adds no suffix, so the names below are exposed byte for byte.
`rooms`, `rooms_approvals_pending` and `rooms_last_event_timestamp_seconds` come from the Room
controller, which runs on the leader only. A room keeps its last values until its next successful
reconcile, so a database outage freezes them rather than emptying them, and it leaves them once its
`Room` is gone. The append counters see every writer: the bridge API, the controller and the run
events.

| Metric | Type | Labels | Meaning | Phase |
|---|---|---|---|---|
| `rooms_build_info` | gauge | `version` | Always 1; carries the running version | 1 |
| `rooms` | gauge | `phase` | Rooms per phase | 1 |
| `rooms_events_appended_total` | counter | `type`, `origin` | Durable events appended | 1 |
| `rooms_append_seconds` | histogram | — | Append latency | 1 |
| `rooms_append_errors_total` | counter | — | Appends that failed on the database: not refusals of the value (SQLSTATE class 22), the room or the lease | 1 |
| `rooms_redactions_total` | counter | `rule` | Secrets redacted | 1 |
| `rooms_last_event_timestamp_seconds` | gauge | `room` | Last durable event of each room with a `Running` run, whatever its phase: 30 silent minutes turn such a room `AwaitingHuman`, and the series must outlive that flip | 1 |
| `rooms_authn_jwks_last_refresh_timestamp_seconds` | gauge | `issuer` | Last successful JWKS fetch per issuer. Held keys stop verifying 24 h after it (Ruling AF). Every replica refreshes each issuer hourly, with jitter, whether or not tokens arrive (Ruling AQ); a failed refresh leaves it, so it ages only while the issuer is unreachable. The human issuer has no series until its first fetch: the broker starts without it | 1 |
| `rooms_connections` | gauge | `kind` | Open connections | 2 |
| `rooms_connections_dropped_total` | counter | `reason` | Connections the broker closed: `reauth`, `slow_consumer`, `write_timeout` (a live peer took no frame within 10 s), `ping_timeout`, `shutdown`, `log_unavailable`, `protocol` (a malformed or oversize frame, or a first frame that is not `hello`, or none within 10 s) | 2 |
| `rooms_participants` | gauge | — | Live participants | 2 |
| `rooms_fanout_lag_seconds` | histogram | — | Append to delivery on a viewer's connection; SC-12 wants p95 < 0.5 s | 2 |
| `rooms_fanout_listener_up` | gauge | — | 1 while the replica's fan-out hub holds its `LISTEN` connection. At 0 the hub polls every subscribed room each second, so viewers still get every event, up to a second late; `/readyz` ignores it on purpose | 2 |
| `rooms_rejected_actions_total` | counter | `reason` | Actions refused (`not_permitted`, `stale_epoch`, …) | 2 |
| `rooms_verdict_posts_total` | counter | `result` | Verdict comments `posted`, `not_posted` or `error` | 3 |
| `rooms_driver_changes_total` | counter | — | Driver token changes: a human's request, give or take, and the leader's lease expiries | 4 |
| `rooms_approvals_pending` | gauge | — | Undecided approvals: the sum of the Rooms' `status.pendingApprovals`, 0 until phase 5 | 5 |
| `rooms_approvals_oldest_pending_seconds` | gauge | — | Age of the oldest undecided approval | 5 |
| `rooms_approval_decision_seconds` | histogram | — | Request to decision | 5 |

Two more count what bridges tell their rooms. Nothing dials into a sandbox (C4), so the bridge serves
no metrics: the broker counts these events as it appends them, and exports them on `:9090` with the
rest (Ruling AP). Neither carries a run or room label.

| Metric | Type | Labels | Meaning | Phase |
|---|---|---|---|---|
| `rooms_bridge_harness_stalls_total` | counter | `reason` | Stalls on the harness log (`event_too_large`, `cursor_lost`, `next_page_unreadable`): a bridge's `state_changed{harness_error}` with that code, told once per stall; the cursor holds. The broker cannot tell a harness's own `ConversationErrorEvent` from a stall when its code is one of these three, so that one is counted too; any other code is not | 1 |
| `rooms_bridge_items_stubbed_total` | counter | `reason` | Harness items kept as a stub in their slot, by why, from exactly this set: `refused` (the broker answered `400` to a lone item; the bridge's `harness_event{harnessKind: refused}`), `invalid_value` (the store rejected a value, SQLSTATE class 22, such as `\u0000` in tool output), `key_collision` (redaction refused keys that are one once case-folded or NUL-stripped), `oversize` (the bridge's or the store's size stub). A harness event whose own kind is one of these names is counted the same way | 1 |

## Alerts

One `VMRule`, `agent-rooms`, inside the `agent-platform` umbrella: silent while the umbrella is
suspended. Every rule carries `runbook_url` and `dashboard` annotations, which a cloud-native-ref
test enforces.

| Alert | Fires when | For | First step | Phase |
|---|---|---|---|---|
| `RoomBrokerDown` | No broker replica is up | 5 min | `kubectl get pods -n agent-system -l app.kubernetes.io/name=room-broker`. Runs keep working and their bridges buffer; nothing reaches the log | 1 |
| `RoomLogAppendErrors` | Any append failed on the database in 10 min | 10 min | `kubectl get cluster -n agent-system xplane-rooms-cnpg-cluster` | 1 |
| `RoomRedactionsSpike` | More than 20 secrets redacted in 15 min | — | An agent is handling credentials: find the rooms with [the redactions query](#reading-the-log-with-sql) | 1 |
| `RoomLogDiskFilling` | The log's volume is over 80 % | 15 min | Shorten `spec.retention` on busy rooms, or grow `storageSize` on `SQLInstance xplane-rooms` | 1 |
| `RoomStalled` | A room with a `Running` run has had no event for 30 min | — | Its run is `Running` but silent: read the bridge's logs (below) | 1 |
| `RoomRejectedActionsSpike` | More than 30 actions refused in 10 min | — | Someone is probing, or the UI disagrees with the policy | 2 |
| `RoomVerdictsNotReachingGitHub` | Posting errors, and no successful post in 30 min | 30 min | The factory App's key at `agents/factory-app`; egress to `api.github.com` | 3 |
| `RoomApprovalPendingTooLong` | An approval has waited over 15 min; reaches Slack through Alertmanager | — | Decide it; an unattended room auto-denies at `approvals.ttl` | 5 |

Bridge logs:

```bash
kubectl logs -n agents -l agents.ogenki.io/run-id=<runId> -c room-bridge
```

## Reading the log with SQL

Until the web UI (phase 2), the owner reads rooms with SQL and the system API. Read only: the log
is the audit trail, and a hand-made write defeats it. Run queries as `rooms_broker` where you can,
so the database refuses a mistaken write.

```bash
PSQL="kubectl exec -n agent-system xplane-rooms-cnpg-cluster-1 -c postgres -- psql -d rooms -tA -c"
ROOM=3kq7x2ma
```

| Question | Query |
|---|---|
| The transcript | `$PSQL "SELECT seq, type, actor_id, left(payload::text, 120) FROM events WHERE room_id = '$ROOM' ORDER BY seq"` |
| Is it gapless (SC-1)? | `$PSQL "SELECT max(seq) = count(*) FROM events WHERE room_id = '$ROOM'"` → `t` |
| Why did each run end? | `$PSQL "SELECT run_id, payload->>'phase', payload->>'reason' FROM events WHERE room_id = '$ROOM' AND payload->>'kind' = 'run_phase' AND payload->>'phase' <> 'Running' ORDER BY seq"` |
| Latest redactions | `$PSQL "SELECT room_id, seq, redactions FROM events WHERE redactions <> '{}' ORDER BY ts DESC LIMIT 20"` |
| Who holds the bridge lease? | `$PSQL "SELECT bridge_run, bridge_seen_at FROM rooms WHERE room_id = '$ROOM'"` |
| Concurrent-run refusals | `$PSQL "SELECT seq, payload FROM events WHERE room_id = '$ROOM' AND payload->>'kind' = 'limit'"` |
| Sealed and when | `$PSQL "SELECT sealed, closed_at, retention, last_seq, bytes FROM rooms WHERE room_id = '$ROOM'"` |
| Append-only, proven (SC-10) | `$PSQL "SET ROLE rooms_broker; UPDATE events SET payload = '{}' WHERE room_id = '$ROOM'"` → `permission denied for table events` |

## The system API from a terminal

The system API needs a token from an allowlisted ServiceAccount (see [API](api.md#authentication)),
over TLS with the platform CA. From a pod in `agent-system` whose ServiceAccount is in
`systemPrincipals`, and that the broker's CNP admits:

```bash
TOKEN=$(kubectl create token <allowlisted-sa> -n agent-system --audience rooms-system --duration 10m)
curl -s --cacert ca.crt -H "Authorization: Bearer $TOKEN" \
  "https://room-broker.agent-system.svc:8443/v1/rooms/$ROOM/events?afterSeq=0&limit=100"
```

`ca.crt` is the platform CA (`openbao-ca`, the same one `room-broker-ca` copies). The allowlist
ships empty; adding an entry is a config change and a broker restart. Remove a probe's entry, pod
and ServiceAccount when you are done.

## Retention

The CronJob `room-broker-retention` runs daily at 03:17. Run it by hand to check it:

```bash
kubectl create job -n agent-system --from=cronjob/room-broker-retention retention-check
kubectl wait -n agent-system job/retention-check --for=condition=Complete --timeout=5m
kubectl logs -n agent-system job/retention-check          # one "purged expired rooms" line per run, with its rooms and events counts
kubectl delete job -n agent-system retention-check
```

Change a room's retention through `Room.spec.retention` before the room is created: the log copies it
into the room's row once, and Ruling Y makes that value immutable.

## Backups and rebuilds

| Item | Setting |
|---|---|
| Backups | CNPG daily at 02:00 to the cluster's CNPG backup bucket, through the Barman Cloud plugin; WAL archived continuously |
| First deploy | No recovery source: nothing exists to recover (ruling P8) |
| After a day of real rooms | Promote a seed with cloud-native-ref's `cnpg-promote-seed.sh` and set `objectStoreRecovery.path: rooms-<date>` on the claim (phase 2) |
| Before every teardown | Promote a fresh seed; events after the last promoted seed are lost on a rebuild |

The log's volume is 20 Gi on one instance. A spot interruption stalls rooms but loses nothing that
the harnesses still hold.

## Common failures

| Symptom | Cause | Fix |
|---|---|---|
| Broker `CrashLoopBackOff` right after a config change | The config file failed strict parsing (unknown key, a `subPattern` without one capture group) | Read the first log line, fix `room-broker-config` |
| Broker never passes `/startupz` | The Atlas migration has not run: CNPG has not created the login roles yet, or the `atlasSchema.ref` branch is gone | `kubectl get atlasmigration -n agent-system`; the operator retries once the roles exist. Point `ref` at `main` once the AP branch has merged |
| Bridge `hello` gets `503 no_room` | The `Room` has not been reconciled, so its row does not exist | `kubectl get room -n agent-system <id>`; the bridge retries |
| Bridge gets `503 log_unavailable` | The database is down or refused the call | `kubectl get cluster -n agent-system xplane-rooms-cnpg-cluster` |
| Bridge gets `401 unauthenticated` | Wrong audience, an issuer not in `runIssuers`, or the broker cannot fetch the JWKS | Check the run's `room-token` audience (`room-broker`), the issuer, and the broker's egress to the JWKS host |
| Bridge gets `403 run_not_live` | The run is terminal, revoked or deleted, or the watch has not seen it yet | Expected at the end of a run; otherwise check `kubectl get agentrun -n agents` |
| Bridge gets `409 room_busy`; the log has `limit{concurrent_run}` | A second run joined a room whose first run is still live | Delete the extra run. A dead holder frees the room within 2 minutes |
| Bridge gets `410 sealed` | The room is closed or full | Start a new room; a full room means a run far beyond normal size |
| A room run's pod waits in `ContainerCreating` | The `room-broker-ca` Secret is not in `agents`: the bridge mounts it, and the mount is not optional | `kubectl describe pod -n agents <pod>` names the missing Secret; `kubectl get externalsecret -n agents room-broker-ca` |
| Bridge refuses to start: URL not `https://`, or no CA in the file | The composition predates GP-18 | Pin a composition with CC-S2 |
| Bridge logs `x509: certificate signed by unknown authority` | The CA it trusts is not the one that signed the broker's certificate | Compare `room-broker-ca` with the `openbao` issuer's CA |
| JWKS or identity-provider calls time out on gcp-0 only | A port-scoped egress rule to the cluster's own gateway is dropped by the socket-LB hairpin | Keep the `toEntities: [all]` rule without ports (ruling P11a) |
| A run ends `pod_lost` | The pod died before the harness finished and before its deadline | Expected for evictions; `deadline` means it hit `maxMinutes` |
| Duplicate harness events after a bridge restart | The harness skipped an unreadable event file (ruling P35) | A known limit, confined to that run; nothing to repair |
| `RoomStalled` | The run is silent: a stuck harness, or a bridge that lost the broker | Bridge logs; the harness's step log in VictoriaLogs |
| A migration that builds an index `CONCURRENTLY` failed (`20260929120000_verdicts`, `20261001130000_delivery_indexes`, `20261001140000_brief_index`), or `\d events` in `psql` shows the index `INVALID` | `CREATE INDEX CONCURRENTLY` runs outside a transaction: a failure leaves an `INVALID` index behind, and the re-run refuses with "already exists" | Drop it by hand before the migration is retried: as `rooms_owner`, `DROP INDEX CONCURRENTLY <index>;` (`events_agent_verdicts`, `events_deliveries`, `events_acks` or `events_brief`), then let the Atlas operator re-run it |
| Verdicts stay in the room, `rooms_verdict_posts_total{result="error"}` rises | GitHub is failing or refusing the App's key (ruling SZ): each verdict backs off from 15 s to 15 min, and a tick stops after two failures in a row | The broker's `verdict not posted yet` logs; the key at `agents/factory-app`. A verdict older than 24 h is recorded as `verdict_not_posted{reason: expired}` |

## Upgrades

| Change | How |
|---|---|
| Broker image | Pin the new digest in the `App` claim **and** the retention CronJob together; re-vendor `crd-rooms.yaml` from the same ref; move `atlasSchema.ref` if the release adds migrations |
| New migration | Ships with the image that needs it: move `atlasSchema.ref` in the same commit as the digest. `/startupz` checks only that the first migration ran, so it does not hold a broker back from a later one |
| Bridge image | A new `_BRIDGE_IMAGE` digest in crossplane-configuration, then that package's pin in cloud-native-ref. Runs pick it up at creation; a running run keeps its bridge |
| Config | Edit `room-broker-config`, then `kubectl rollout restart deployment/room-broker -n agent-system`: the broker reads it once |
| CRD | Vendored with every pin. From AP-1, `task check` fails when the committed CRD differs from what the Go types generate |
