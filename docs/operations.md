# Operations

Operate rooms through three windows: the **`Room` CR's status** (`kubectl get room -n agent-system`),
the broker's **metrics and alerts**, and **SQL on the log**. The manifests live in cloud-native-ref.
The factory's signals are [at the end](#the-factory).

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

Scraped from `:9090/metrics` by a `VMServiceScrape`. The broker records them through the
OpenTelemetry metric API with a Prometheus exporter that adds no suffix, so the names below are
exposed byte for byte.
`rooms`, `rooms_approvals_pending` and `rooms_last_event_timestamp_seconds` come from the Room
controller, which runs on the leader only. A room keeps its last values until its next successful
reconcile, so a database outage freezes them rather than emptying them, and it leaves them once its
`Room` is gone. The append counters see every writer: the bridge API, the controller and the run
events.

| Metric | Type | Labels | Meaning |
|---|---|---|--- |
| `rooms_build_info` | gauge | `version` | Always 1; carries the running version |
| `rooms` | gauge | `phase` | Rooms per phase |
| `rooms_events_appended_total` | counter | `type`, `origin` | Durable events appended |
| `rooms_append_seconds` | histogram | — | Append latency |
| `rooms_append_errors_total` | counter | — | Appends that failed on the database: not refusals of the value (SQLSTATE class 22), the room or the lease |
| `rooms_redactions_total` | counter | `rule` | Secrets redacted |
| `rooms_last_event_timestamp_seconds` | gauge | `room` | Last durable event of each room with a `Running` run, whatever its phase: 30 silent minutes turn such a room `AwaitingHuman`, and the series must outlive that flip |
| `rooms_authn_jwks_last_refresh_timestamp_seconds` | gauge | `issuer` | Last successful JWKS fetch per issuer. Held keys stop verifying 24 h after it. Every replica refreshes each issuer hourly, with jitter, whether or not tokens arrive; a failed refresh leaves it, so it ages only while the issuer is unreachable. The human issuer has no series until its first fetch: the broker starts without it |
| `rooms_connections` | gauge | `kind` | Open connections |
| `rooms_connections_dropped_total` | counter | `reason` | Connections the broker closed: `reauth`, `slow_consumer`, `write_timeout` (a live peer took no frame within 10 s), `ping_timeout`, `shutdown`, `log_unavailable`, `protocol` (a malformed or oversize frame, or a first frame that is not `hello`, or none within 10 s), `access_revoked` and `access_unverified` (the room-access re-check on an open socket) |
| `rooms_participants` | gauge | `kind` | Live participants. `kind="run"`: live runs naming a room, counted by every replica's `AgentRun` watch, so aggregate with `max`, never `sum`. `kind="human"`: open viewer WebSockets on this replica, so `sum` across replicas |
| `rooms_fanout_lag_seconds` | histogram | — | Append to delivery on a viewer's connection; the target is p95 < 0.5 s |
| `rooms_fanout_listener_up` | gauge | — | 1 while the replica's fan-out hub holds its `LISTEN` connection. At 0 the hub polls every subscribed room each second, so viewers still get every event, up to a second late; `/readyz` ignores it on purpose |
| `rooms_rejected_actions_total` | counter | `reason` | Actions refused (`not_permitted`, `stale_epoch`, `approval_expired`, …) |
| `rooms_verdict_posts_total` | counter | `result` | Verdict comments `posted`, `not_posted` or `error` |
| `rooms_driver_changes_total` | counter | — | Driver token changes: a human's request, give or take, and the leader's lease expiries |
| `rooms_approvals_pending` | gauge | — | Undecided approvals in open rooms: the sum of the Rooms' `status.pendingApprovals` |
| `rooms_approvals_oldest_pending_seconds` | gauge | — | Age of the oldest undecided approval in an open room, set by the leader's approval sweep every 30 s; 0 on a replica that stopped leading |
| `rooms_approval_decision_seconds` | histogram | — | Request to decision |

Two more count what bridges tell their rooms. Nothing dials into a sandbox, so the bridge serves
no metrics: the broker counts these events as it appends them, and exports them on `:9090` with the
rest. Neither carries a run or room label.

| Metric | Type | Labels | Meaning |
|---|---|---|--- |
| `rooms_bridge_harness_stalls_total` | counter | `reason` | Stalls on the harness log (`event_too_large`, `cursor_lost`, `next_page_unreadable`): a bridge's `state_changed{harness_error}` with that code, told once per stall; the cursor holds. The broker cannot tell a harness's own `ConversationErrorEvent` from a stall when its code is one of these three, so that one is counted too; any other code is not |
| `rooms_bridge_items_stubbed_total` | counter | `reason` | Harness items kept as a stub in their slot, by why, from exactly this set: `refused` (the broker answered `400` to a lone item; the bridge's `harness_event{harnessKind: refused}`), `invalid_value` (the store rejected a value, SQLSTATE class 22, such as `\u0000` in tool output), `key_collision` (redaction refused keys that are one once case-folded or NUL-stripped), `oversize` (the bridge's or the store's size stub). A harness event whose own kind is one of these names is counted the same way |

## Alerts

One `VMRule`, `agent-rooms`, inside the `agent-platform` umbrella: silent while the umbrella is
suspended. Every rule carries `runbook_url` and `dashboard` annotations, which a cloud-native-ref
test enforces.

| Alert | Fires when | For | First step |
|---|---|---|--- |
| `RoomBrokerDown` | No broker replica is up | 5 min | `kubectl get pods -n agent-system -l app.kubernetes.io/name=room-broker`. Runs keep working and their bridges buffer; nothing reaches the log |
| `RoomLogAppendErrors` | Any append failed on the database in 10 min | 10 min | `kubectl get cluster -n agent-system xplane-rooms-cnpg-cluster` |
| `RoomRedactionsSpike` | More than 20 secrets redacted in 15 min | — | An agent is handling credentials: find the rooms with [the redactions query](#reading-the-log-with-sql) |
| `RoomLogDiskFilling` | The log's volume is over 80 % | 15 min | Shorten `spec.retention` on busy rooms, or grow `storageSize` on `SQLInstance xplane-rooms` |
| `RoomStalled` | A room with a `Running` run has had no event for 30 min | — | Its run is `Running` but silent: read the bridge's logs (below) |
| `RoomRejectedActionsSpike` | More than 30 actions refused in 10 min | — | Someone is probing, or the UI disagrees with the policy |
| `RoomVerdictsNotReachingGitHub` | Posting errors, and no successful post in 30 min | 30 min | The factory App's key at `agents/factory-app`; egress to `api.github.com` |
| `RoomApprovalPendingTooLong` | An approval has waited over 15 min; reaches Slack through Alertmanager | — | Decide it; undecided, it is denied at 30 min (attended) or `approvals.ttl` (unattended) |

Bridge logs:

```bash
kubectl logs -n agents -l agents.ogenki.io/run-id=<runId> -c room-bridge
```

## Reading the log with SQL

The web UI and `roomctl` read rooms; SQL is for what they do not show. Read only: the log
is the audit trail, and a hand-made write defeats it. Run queries as `rooms_broker` where you can,
so the database refuses a mistaken write.

```bash
PSQL="kubectl exec -n agent-system xplane-rooms-cnpg-cluster-1 -c postgres -- psql -d rooms -tA -c"
ROOM=3kq7x2ma
```

| Question | Query |
|---|---|
| The transcript | `$PSQL "SELECT seq, type, actor_id, left(payload::text, 120) FROM events WHERE room_id = '$ROOM' ORDER BY seq"` |
| Is it gapless? | `$PSQL "SELECT max(seq) = count(*) FROM events WHERE room_id = '$ROOM'"` → `t` |
| Why did each run end? | `$PSQL "SELECT run_id, payload->>'phase', payload->>'reason' FROM events WHERE room_id = '$ROOM' AND payload->>'kind' = 'run_phase' AND payload->>'phase' <> 'Running' ORDER BY seq"` |
| Latest redactions | `$PSQL "SELECT room_id, seq, redactions FROM events WHERE redactions <> '{}' ORDER BY ts DESC LIMIT 20"` |
| Who holds the bridge lease? | `$PSQL "SELECT bridge_run FROM rooms WHERE room_id = '$ROOM'"` (`bridge_seen_at` is no longer written; a later migration drops it) |
| Concurrent-run refusals | `$PSQL "SELECT seq, payload FROM events WHERE room_id = '$ROOM' AND payload->>'kind' = 'limit'"` |
| Sealed and when | `$PSQL "SELECT sealed, closed_at, retention, last_seq, bytes FROM rooms WHERE room_id = '$ROOM'"` |
| Append-only, proven | `$PSQL "SET ROLE rooms_broker; UPDATE events SET payload = '{}' WHERE room_id = '$ROOM'"` → `permission denied for table events` |

## The system API from a terminal

The system API needs a token from an allowlisted ServiceAccount (see [API](api.md#authentication)),
over TLS with the platform CA. From a pod in `agent-system` whose ServiceAccount is in
`systemPrincipals`, and that the broker's CNP admits:

```bash
TOKEN=$(kubectl create token <allowlisted-sa> -n agent-system --audience rooms-system --duration 10m)
curl -s --cacert ca.crt -H "Authorization: Bearer $TOKEN" \
  "https://room-broker.agent-system.svc:8443/v1/rooms/$ROOM/events?afterSeq=0&limit=100"
```

`ca.crt` is the platform CA (`openbao-ca`, the same one `room-broker-ca` copies). Adding an entry
to `systemPrincipals` is a config change and a broker restart. Remove a probe's entry, pod
and ServiceAccount when you are done.

## Retention

The CronJob `room-broker-retention` runs daily at 03:17. Run it by hand to check it:

```bash
kubectl create job -n agent-system --from=cronjob/room-broker-retention retention-check
kubectl wait -n agent-system job/retention-check --for=condition=Complete --timeout=5m
kubectl logs -n agent-system job/retention-check          # one "purged expired rooms" line per run, with its rooms and events counts
kubectl delete job -n agent-system retention-check
```

Each room's purge runs under its own `statement_timeout` of 10 minutes (`SET LOCAL`, the purge
transaction only), not the broker's 15 s. Keep the CronJob's `activeDeadlineSeconds`, if it sets
one, above 10 minutes, or the job is killed before a large room's purge can finish.

Change a room's retention through `Room.spec.retention` before the room is created: the log copies it
into the room's row once, and the database keeps that value immutable.

## Backups and rebuilds

| Item | Setting |
|---|---|
| Backups | CNPG daily at 02:00 to the cluster's CNPG backup bucket, through the Barman Cloud plugin; WAL archived continuously |
| First deploy | No recovery source: nothing exists to recover |
| After a day of real rooms | Promote a seed with cloud-native-ref's `cnpg-promote-seed.sh` and set `objectStoreRecovery.path: rooms-<date>` on the claim |
| Before every teardown | Promote a fresh seed; events after the last promoted seed are lost on a rebuild |

The log's volume is 20 Gi on one instance. A spot interruption stalls rooms but loses nothing that
the harnesses still hold.

## Common failures

| Symptom | Cause | Fix |
|---|---|---|
| Broker `CrashLoopBackOff` right after a config change | The config file failed strict parsing (unknown key, a `subPattern` without one capture group) | Read the first log line, fix `room-broker-config` |
| Broker never passes `/startupz` | The Atlas migration has not run: CNPG has not created the login roles yet, or `atlasSchema.ref` names a branch that is gone | `kubectl get atlasmigration -n agent-system`; the operator retries once the roles exist. Point `ref` at the broker image's release tag |
| Bridge `hello` gets `503 no_room` | The `Room` has not been reconciled, so its row does not exist | `kubectl get room -n agent-system <id>`; the bridge retries |
| Bridge gets `503 log_unavailable` | The database is down or refused the call | `kubectl get cluster -n agent-system xplane-rooms-cnpg-cluster` |
| Bridge gets `401 unauthenticated` | Wrong audience, an issuer not in `runIssuers`, or the broker cannot fetch the JWKS | Check the run's `room-token` audience (`room-broker`), the issuer, and the broker's egress to the JWKS host |
| Bridge gets `403 run_not_live` | The run is terminal, revoked or deleted, or the watch has not seen it yet | Expected at the end of a run; otherwise check `kubectl get agentrun -n agents` |
| Bridge gets `409 room_busy`; the log has `limit{concurrent_run}` | A second run joined a room whose first run is still live | Nothing to do: its harness waits behind `room-bridge gate`. If the room is still busy after 3 minutes, the pod fails before the harness runs and the run ends `room_busy`. The holder frees the room once its run ends |
| A room run's pod is `Init:Error`; `room-bridge gate` logs `the room refused this run` | The room stayed busy for 3 minutes, or is sealed | Expected. Start the run again once the holder has ended |
| Bridge gets `410 sealed` | The room is closed or full | Start a new room; a full room means a run far beyond normal size |
| A room run's pod waits in `ContainerCreating` | The `room-broker-ca` Secret is not in `agents`: the bridge mounts it, and the mount is not optional | `kubectl describe pod -n agents <pod>` names the missing Secret; `kubectl get externalsecret -n agents room-broker-ca` |
| Bridge refuses to start: URL not `https://`, or no CA in the file | The composition is older than the bridge's TLS requirement | Pin a composition package that mounts `room-broker-ca` and sets `BROKER_CA_FILE` |
| Bridge logs `x509: certificate signed by unknown authority` | The CA it trusts is not the one that signed the broker's certificate | Compare `room-broker-ca` with the `openbao` issuer's CA |
| JWKS or identity-provider calls time out on gcp-0 only | A port-scoped egress rule to the cluster's own gateway is dropped by the socket-LB hairpin | Keep the `toEntities: [all]` rule without ports |
| A run ends `agent_error`; the harness log has `agent-run error: LLMServiceUnavailableError` | The model path failed three times in about 30 s: a provider or gateway outage, not one blip | The harness's `agent-run model retry` lines show the attempts. Retry policy: [harness README](../images/agent-harness/README.md#model-errors). A `429` is never retried: check the run's budget first |
| A run ends `pod_lost` | The pod died before the harness finished and before its deadline | Expected for evictions; `deadline` means it hit `maxMinutes` |
| Duplicate harness events after a bridge restart | The harness skipped an unreadable event file | A known limit, confined to that run; nothing to repair |
| `RoomStalled` | The run is silent: a stuck harness, or a bridge that lost the broker | Bridge logs; the harness's step log in VictoriaLogs |
| A migration that builds an index `CONCURRENTLY` failed (`20260929120000_verdicts`, `20261001130000_delivery_indexes`, `20261001140000_brief_index`, `20261005130000_approval_indexes`, `20261008120000_task_facts_index`), or `\d events` in `psql` shows the index `INVALID` | `CREATE INDEX CONCURRENTLY` runs outside a transaction: a failure leaves an `INVALID` index behind, and the re-run refuses with "already exists" | Drop it by hand before the migration is retried: as `rooms_owner`, `DROP INDEX CONCURRENTLY <index>;` (`events_agent_verdicts`, `events_deliveries`, `events_acks`, `events_brief`, `approvals_pending`, `events_decisions`, `events_decision_acks`, `events_tool_results` or `events_task_facts`), then let the Atlas operator re-run it |
| Verdicts stay in the room, `rooms_verdict_posts_total{result="error"}` rises | GitHub is failing or refusing the App's key: each verdict backs off from 15 s to 15 min, and a tick stops after two failures in a row | The broker's `verdict not posted yet` logs; the key at `agents/factory-app`. A verdict older than 24 h is recorded as `verdict_not_posted{reason: expired}` |

## Upgrades

| Change | How |
|---|---|
| Broker image | Pin the new digest in the `App` claim **and** the retention CronJob together; re-vendor `crd-rooms.yaml` from the same ref; move `atlasSchema.ref` to the same release tag |
| New migration | Ships with the image that needs it: move `atlasSchema.ref` in the same commit as the digest. `/startupz` checks only that the first migration ran, so it does not hold a broker back from a later one |
| Bridge image | A new `_BRIDGE_IMAGE` digest in crossplane-configuration, then that package's pin in cloud-native-ref. Runs pick it up at creation; a running run keeps its bridge |
| Config | Edit `room-broker-config`, then `kubectl rollout restart deployment/room-broker -n agent-system`: the broker reads it once |
| Factory run-request API port | The chart serves `POST /v1/runs` on `:8082` (Service and container port `api`), plaintext inside the mesh; the RunLore intake stays on `:8080`, and the broker's `:8443` is the only TLS listener. Releases before it served the API on `:8443`: move the factory config's `api.listen` and the broker's `factoryURL` (`http://agent-factory.agent-system.svc:8082`) to `:8082` in the same change as the chart, or the broker's `start_run` calls fail until they do |
| Factory | Pin the chart's version and digest in `ocirepo-agent-factory.yaml` and the image in the factory's values, from the same release. The `HelmRelease` sets `crds: CreateReplace`, since Helm alone never upgrades the `Task` CRD in the chart's `crds/` |
| CRD | Vendored with every pin. `task check` fails when the committed CRD differs from what the Go types generate |

## The factory

What the factory does, its labels, budgets, merge gate and kill switch, is in
[the factory](factory.md). This section is what an operator watches. The factory serves its
metrics on `:9090` (the chart's `metrics` port), next to its probes.

### Factory metrics

Defined in `internal/factory/fmetrics` (and `agent_factory_build_info` in `internal/metrics`). The
leader alone reports `agent_factory_tasks`, `agent_factory_kill_switch_engaged` and
`agent_factory_task_phase_seconds`, read from its cache at scrape time; a failed read leaves them
absent rather than 0.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `agent_factory_build_info` | gauge | `version` | Always 1; carries the running version |
| `agent_factory_tasks` | gauge | `phase`, `source`, `predicted_class`, `tier`, `repository` | Tasks by state |
| `agent_factory_task_phase_seconds` | gauge | `task`, `phase` | Seconds an open task has spent in its phase |
| `agent_factory_kill_switch_engaged` | gauge | — | 1 while the stop object exists; `factory/stop` on the control issue engages it by creating that object |
| `agent_factory_time_to_pr_seconds` | histogram | `source`, `tier`, `template`, `repository` | From intake to the pull request opening |
| `agent_factory_pr_outcomes_total` | counter | `class`, `outcome`, `repository` | `auto_merged`, `human_merged`, `closed`, `reverted` |
| `agent_factory_task_tokens` | histogram | `tier`, `template`, `predicted_class`, `repository` | Tokens per finished task |
| `agent_factory_budget_remaining_tokens` | gauge | `principal` | Tokens the principal has left today |
| `agent_factory_budget_cap_tokens` | gauge | `principal` | The principal's daily token cap |
| `agent_factory_human_interventions_total` | counter | `kind`, `repository` | `stop`, `retry`, `request_changes`, `steer`, `takeover`, `approve` |
| `agent_factory_merge_decisions_total` | counter | `class`, `verdict`, `reason` | The merge gate's decisions: `arm`, `shadow`, `human`, `escalate`, `ci_red`, `ci_cancelled`; a wait is none |
| `agent_factory_class_mismatch_total` | counter | `predicted`, `matched` | The predicted class differs from the one policy-bot matched |
| `agent_factory_tier_fit_total` | counter | `classifier`, `tier`, `fit`, `control` | A finished task's tier fit, per classifier |
| `agent_factory_intake_errors_total` | counter | `source`, `repository` | Failed intake polls or requests: `issue`, `runlore`, `schedule`, `api` |
| `agent_factory_label_events_truncated_total` | counter | `label` | Label polls that met the forge's event cap and left the label for the next poll |
| `agent_factory_run_revocations_total` | counter | `reason`, `limit` | Runs the factory revoked; `limit` when the run meter did it (below) |
| `agent_factory_run_overshoot_tokens_total` | counter | — | Tokens runs spent past their cap before the meter revoked them: a tick's lag, or a blind interval's spend after an outage |
| `agent_factory_run_cost_usd` | histogram | `tier` | A priced run's USD cost, reported two minutes after it ended (below) |
| `agent_factory_run_spend_usd` | gauge | `run`, `model` | A priced run's USD spend so far, until its cost is reported |
| `agent_factory_model_cost_usd_total` | counter | `model` | USD the runs spent, by served model, added as each run's cost is reported |
| `agent_factory_run_budget_usd` | gauge | `tier` | Each tier's USD run cap; absent for a tier capped in tokens |
| `agent_factory_run_budget_fallbacks_total` | counter | `scope`, `why` | Runs and tasks of a USD-capped tier held to their token cap (below) |
| `agent_factory_meter_unpriced_model_runs_total` | counter | `model` | Runs a model served with no price (below) |
| `agent_factory_meter_usage_read_errors_total` | counter | — | Meter ticks whose usage read failed: no cap was checked on them |
| `agent_factory_meter_last_success_timestamp_seconds` | gauge | — | When the meter last read usage and settled it in the day's ledger; set by the leader only, so alert on its max across pods. A tick whose ledger write fails sets nothing: it still revokes runs at their own caps, but the daily budgets wait and the increase is retried whole on the next tick |
| `agent_factory_resumes_total` | counter | `reason` | Runs resumed after their pod was `Disrupted` or `PodLost` |
| `agent_factory_rooms_closed_at_cap_total` | counter | `reason` | Rooms closed at the cap (below): `human_driver`, `facts_due` |
| `agent_factory_github_up` | gauge | — | 1 while any GitHub call succeeded in the last 3 minutes, per replica |
| `agent_factory_github_rate_remaining` | gauge | — | The factory App's remaining REST rate limit |
| `agent_factory_trace_export_abandoned_total` | counter | — | Task spans given up after the collector refused them for a day |
| `agent_factory_repository_owned` | gauge | `repository` | 1 while the repository's `.github/agent-factory.yaml` names this factory (`factory` in the config), per replica; 0 means no intake, schedules, API runs or merges there |
| `agent_factory_admission_refusals_total` | counter | `repository`, `reason` | Run requests refused on their repository: `repository_differs_from_the_room`, `repository_differs_from_the_branch`, `repository_permission`, `repository_not_owned` |

A label value outside its known set is recorded as `other`; `repository` takes only the configured repositories' names.

### Factory alerts

| Alert | Fires when | For | First step |
|---|---|---|---|
| `FactoryGitHubUnreachable` | `agent_factory_github_up == 0` | 10 min | GitHub's status and the App key (`agent-factory-github`): [below](#factory-failures) |
| `FactoryRepositoryNotOwned` | `max by (repository) (agent_factory_repository_owned) == 0` | 20 min | The repository's `.github/agent-factory.yaml` on its default branch: absent, unparsable, or naming another factory. A read that fails turns the gauge 0 after 15 min |

No series, no alert: on a release without the gauge the expression is empty, so the rule is safe to ship ahead of it.

### Factory failures

| Symptom | Cause | Fix |
|---|---|---|
| `agent_factory_github_up` is 0; factory loops log GitHub errors and no task advances, while the replicas stay `Ready` | GitHub is down, or the App key is refused. Readiness ignores GitHub on purpose: unready replicas would serve nothing more, and the PDB's `unhealthyPodEvictionPolicy: AlwaysAllow` (Kubernetes 1.27+) never blocks a drain either way. The gauge is 1 while any GitHub call succeeded in the last 3 minutes, on each replica, and `FactoryGitHubUnreachable` (`agent_factory_github_up == 0` for 10m) pages on it | Check GitHub's status and the App key (`agent-factory-github`); the loops recover by themselves when GitHub returns |
| Factory `CrashLoopBackOff`; the log says `merger App key ... is missing` | The Secrets are hard prerequisites whenever any merge class is `live` or `shadow` (deciding it reads checks through the merger): the optional mount lets the pod start, and the binary then refuses, so a missing key is a crash loop with a clear log line, no longer `ContainerCreating` | Create the `agent-factory-merger` Secret (keys `app_id`, `private_key`), or turn every class off: with none live or in shadow the key is optional and the merger fails closed. A task at the merge gate then waits for a maintainer (`merge_gate_off`) |
| Factory `POST /intake/runlore` answers `503 intake_unconfigured`; `agent_factory_intake_errors_total{source="runlore"}` rises and the log warns once a minute | The `agent-factory-intake` Secret is absent or empty (the mount is optional): the intake refuses everything instead of running open. It is a hard prerequisite whenever RunLore is meant to feed the factory | Create the Secret (key `token`); the token is re-read per request, no restart |

### Budget signals

The budget model, prices and caps are in [the factory's budgets](factory.md#budgets).

| Signal | Meaning |
|---|---|
| A tier sets `runUSD` or `taskUSD` and its model has no price | The rollout fails. An unpriced run would cost $0, and $0 never reaches a cap |
| `agent_factory_run_budget_fallbacks_total{scope="run", why="unpriced"}` | A model that served the run has no price (or, without `gen_ai_request_model` in `meter.query`, its claim's model has none). The run is capped at `runTokens`. The log says `run capped in tokens` |
| `agent_factory_meter_unpriced_model_runs_total{model}` | Runs served by `model`, which has no `meter.prices` row: a backend pins a model the price table lacks. Counted once per run and model, whatever the tier caps in. Alert on any increase |
| `agent_factory_run_budget_fallbacks_total{scope="run", why="untyped"}` | `meter.query` has no `gen_ai_token_type` grouping, so no run can be priced and every run is capped at `runTokens` |
| `agent_factory_run_budget_fallbacks_total{scope="task", why}` | A task with a USD cap holds a run the meter could not price, so the task is held to `taskTokens`. Counted once per task. The log says `task capped in tokens` |
| `agent_factory_run_revocations_total{reason, limit}` | The limit that fired: `run-usd`, `run-tokens`, `task-usd`, `task-tokens`, `gateway-ceiling`, `fleet` or `principal-day`. The `revoked` annotation keeps the reason (`budget-run`, …), because the composition maps each value to a phase |
| `agent_factory_run_cost_usd{tier}` | A priced run's cost, reported two minutes after the meter first sees it ended, or when its claim is deleted first. `agent_factory_run_budget_usd{tier}` is the run cap |
| `agent_factory_run_spend_usd{run, model}` | A priced run's spend so far, by the model that served it, from its first reading until its cost is reported. Only runs not yet reported carry the series, so its cardinality is the live runs times their models. One run's final price: `max_over_time(agent_factory_run_spend_usd{run="<id>"}[1d])` |
| `agent_factory_model_cost_usd_total{model}` | Spend by served model, added when each run's cost is reported: `increase(...[1d])` is a day's bill per model |

A run's last calls reach the meter a scrape interval plus VictoriaMetrics' 30 s latency offset
after they happen, so the first tick that sees a run ended can read it short. On
`xplane-run-nh5tztlh` that tick read 370,526 of 507,350 tokens. The meter keeps annotating an
ended run's usage, and reports its cost only once the window has passed.

A run's USD total is kept in the meter's memory only, because the factory's patch limit admits no
annotation besides the existing ones. A leader failover after a gateway counter reset forgets what a
run spent before that reset; the Task status keeps the costs already recorded
(`runs[].costMicroUSD`, `usage.costMicroUSD`).

### Finished rooms close

A finished task's room closes once its outbox, settle, facts and revert window are done, and while
no person drives it. The last two waits are capped a day after the task ended: past that the room
closes anyway, a person still driving it included, and `agent_factory_rooms_closed_at_cap_total`
counts it.

### Escalated tasks and the kill switch

How long an escalated task waits (`caps.escalatedHours`) and how to engage or lift the stop are in
[the factory](factory.md#escalation-reminders-and-stale-tasks) and
[its kill switch](factory.md#the-kill-switch). An operator's stop is a fresh object:
`kubectl create configmap agent-factory-stop -n agent-system`.
