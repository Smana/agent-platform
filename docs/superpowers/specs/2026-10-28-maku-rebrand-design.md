# Maku rebrand: renaming everything from agent-platform to maku

**Status:** draft for owner review, 2026-10-28.
**Decided with the owner:**
- Everything is renamed, the CRD group included.
- Components are named `maku-*`, and the CLI is `maku`.
- The CRD group becomes `maku.ogenki.io`.
- Metrics get a `maku_` prefix and keep the rest of their name.
- The web host stays `rooms.<domain>`.
- The work runs in two phases (approach A).
- It starts after #165 (install anywhere) merges.

`docs/naming.md` records why the product is called Maku. This spec covers how every technical name follows it without breaking the deployment that runs it (ai-native-ref, pinned by cloud-native-ref on gcp-0).

## Scope

| Kind | Today | After |
|---|---|---|
| GitHub repository | `Smana/agent-platform` | `Smana/maku` |
| Go module | `github.com/Smana/agent-platform` | `github.com/Smana/maku` |
| Broker binary and image | `room-broker` | `maku-broker` |
| Bridge sidecar | `room-bridge` | `maku-bridge` |
| Human CLI | `roomctl` | `maku` |
| Factory | `agent-factory` | `maku-factory` |
| Run controller | `run-controller` | `maku-run-controller` |
| Slack App | `factory-slack-app` | `maku-slack` |
| Harness image | `agent-harness` | `maku-harness` |
| Umbrella chart | `agent-platform` | `maku` |
| Factory chart | `agent-factory` | `maku-factory` |
| CRD group (`Room`, `Task`, the quickstart's `AgentRun`) | `agents.ogenki.io` | `maku.ogenki.io` |
| Metrics | `rooms_*` (the SP2 §9 set), `agent_factory_*` | `maku_rooms_*`, `maku_factory_*` |
| Web host | `rooms.<domain>` | unchanged |

**Out of scope, deliberately:**
- **`agentruns.cloud.ogenki.io`.** Production's `AgentRun` is a Crossplane XRD in crossplane-configuration, part of the platform's `cloud.ogenki.io` API next to `App` and `EPI`. It belongs to the platform, not the product. `internal/runwatch` already takes its group from config (`Kind.Group`), so nothing in maku pins it.
- **The `agents` namespace** and the `xplane-run-` claim prefix: these are platform seams too, and already configurable.
- **The JWT audiences `room-broker` and `rooms-system`** (`internal/authn/jwt.go`). These are constants that issuers mint against, and changing them means re-issuing every token and client. They stay, and the doc names them as legacy identifiers.
- **Event types and the C4 envelope** (`room.*`, `summary/v1`). They are a wire format, and "room" is still the product's word for a room.

## Phase 1: names, no data migration

Phase 1 ships as one agent-platform release, `v0.30.0`, and two consumer PRs. Nothing stored changes.

**1. The repository and module.**
- Rename the repository on GitHub. GitHub redirects clones, links, release downloads and `uses: Smana/agent-platform/...` actions.
- Run one mechanical PR that moves the module path to `github.com/Smana/maku` across `go.mod`, every import, `-ldflags` paths, `.golangci.yaml` and the docs.
- Update every local clone with `git remote set-url origin https://github.com/Smana/maku.git`.

**2. Binaries, images and charts.**
- `cmd/<old>` becomes `cmd/<new>`, `images/<old>` becomes `images/<new>`, and the release matrix publishes the new names.
- Charts move to `charts/maku` and `charts/maku-factory`, published at `oci://ghcr.io/smana/charts/<new>`.
- **The old image names stop at v0.29.x.** No more tags are pushed under the old names. Consumers repin in the same step as they bump the version, so no dual publishing is needed.
- `maku` replaces `roomctl`. Its config directory moves from `$XDG_CONFIG_HOME/roomctl` to `$XDG_CONFIG_HOME/maku`. On first run it reads the old directory once and writes the new one, so people don't have to sign in again.
- The embedded Agent Skill is installed under the new name.
- The web UI's CLI setup block downloads `maku-<os>-<arch>`.
- The `roomctl` names in docs, prompts, MCP server names (`roomctl mcp` becomes `maku mcp`) and the factory-handoff skill follow.

**3. The signing identity.**
- Release signatures carry the workflow's repository path, so the signer becomes `https://github.com/Smana/maku/.github/workflows/release.yaml@refs/tags/v*`.
- **Before** the rename, ai-native-ref's `flux/sources/ocirepo-agent-factory.yaml`, and any other consumer that verifies, accepts both `Smana/(agent-platform|maku)`.
- After phase 2 completes, a follow-up narrows it to `maku`.
- The `cosign verify` examples in `docs/security.md` and `docs/architecture.md` change in the same PR.

**4. Metrics, emitted under both names for one release.**
- `v0.30.0` emits every series twice: the old name and the `maku_`-prefixed name (`rooms_x` becomes `maku_rooms_x`, and `agent_factory_x` becomes `maku_factory_x`). This is done by registering a second instrument set over the same values in `internal/metrics` and `internal/factory/fmetrics`, under a feature switch that defaults on.
- ai-native-ref moves its dashboards, VMRules and alerts to the new names in the same pin PR, so nothing goes blank between Flux applying the pin and the pods restarting. The old names are still there throughout.
- `v0.31.0` stops emitting the old names.
- The `*_build_info` series follow the same rule.

**5. Consumers, in order:**
1. An ai-native-ref PR that accepts both signers. It needs no agent-platform release.
2. Rename the GitHub repository.
3. Tag agent-platform `v0.30.0`.
4. An ai-native-ref PR, `v0.30.0` pins under the new names, which changes:
   - every image and chart reference;
   - the HelmRelease chart names;
   - the Deployment and container names, where they embed the binary name;
   - ServiceAccount names. These stay **unchanged**, because the broker's principal allowlists and the subjects issuers mint for are keyed on them. Renaming them is a later, separate change.
   - the dashboards and rules, switched to `maku_*`.
5. A cloud-native-ref pin PR, including its test-contract action reference.
6. Coordinate every tag with the other session that releases agent-platform: no tag while phase 1 is in flight.

**Rollback:** revert the pin in ai-native-ref, which restores the v0.29.x images. The renamed repository can stay, since redirects cover the old path.

## Phase 2: the CRD group, in a scheduled quiet window

Kubernetes can't serve one kind under two groups, so the objects are copied, not converted.

**Code (agent-platform v0.31.0):**
- Set `groupName` to `maku.ogenki.io` in `api/v1alpha1` and `api/factory/v1alpha1`.
- Regenerate the CRDs into `config/crd/maku.ogenki.io_*.yaml` and the chart's copies.
- Update the `Room` controller, the factory reconciler and every GVK constant.
- Add a new one-shot subcommand, `maku-broker migrate-group --from agents.ogenki.io --to maku.ogenki.io`. For every `Room`, then every `Task`, in every namespace, it:
  - creates the copy under the new group with the same name, namespace, labels, annotations and spec;
  - writes the status back through the status subresource;
  - removes the old object's finalizers;
  - deletes the old object.
- **Idempotency:** an object that already exists under the new group with the same UID-derived annotation (`maku.ogenki.io/migrated-from: <old uid>`) is skipped.
- **Dry run by default.** `--apply` does the work.
- **Each object succeeds or fails on its own,** with a report. Nothing is deleted until its copy is confirmed.
- **Rooms' durable state is in Postgres,** keyed by room id, which doesn't change. So a `Room` copy only re-attaches the reconciler to the same log; no event data moves.

**The window (operator runbook in `docs/`):**
1. Engage the factory kill switch and wait until no `AgentRun` is active (`kubectl get agentruns -A`), so no run is mid-flight against an old `Room`.
2. Apply the new CRDs (pin v0.31.0's chart, broker and factory). The old CRDs stay installed.
3. Run `maku-broker migrate-group` in dry-run mode, read the report, then run it with `--apply`.
4. Check:
   - every room page loads;
   - `maku status <room>` answers;
   - the factory lists its `Task`s;
   - a test task runs end to end.
5. Release the kill switch.
6. After one day, delete the old CRDs. That removes any stragglers, so the report from step 3 must be empty first.

**Rollback before step 6:** the old CRDs and any objects left under them are still there. Re-pin v0.30.x and delete the new-group copies. Rollback after step 6 means re-running the migration in reverse, which the subcommand supports with `--from` and `--to` swapped.

## Testing

- **Phase 1:**
  - `task check` on the renamed tree.
  - A test that both metric sets register and carry equal values.
  - A test that the `maku` CLI imports the config from the `roomctl` directory once.
  - A release dry run on a PR (the prerelease images get the new names).
  - The quickstart e2e on the new chart.
- **Phase 2:**
  - `migrate-group` table tests against a fake client: copy, skip already migrated objects, preserve status, report failures, dry run changes nothing, and run in reverse.
  - A kind e2e that creates rooms and tasks under the old group, upgrades, migrates, and checks that the room log is reachable and the factory resumes.
- **ai-native-ref:** its `task check` covers the dashboard and rule tests on the new metric names, and the config-equality tests between base and gcp-0.

## Risks

| Risk | Mitigation |
|---|---|
| A release signed by `Smana/maku` is refused by Flux | Consumers accept both signers before the rename (phase 1, step 1) |
| Dashboards go blank mid-switch | One release emits both metric names |
| A run is mid-flight against an old `Room` during phase 2 | Kill switch, plus waiting for zero active `AgentRun`s, before migrating |
| A partial migration | Idempotent per object, dry run first, old CRDs kept until the report is empty |
| The other release session tags in between | Explicit tag coordination; no tags during either phase |
| A forgotten old name | After each phase, a `git grep` gate (`room-broker`, `roomctl`, `agents.ogenki.io`, `rooms_`) in CI that allows only listed historical docs (plans, specs, the changelog) |

## Open questions for the owner

1. **ServiceAccounts and principals.** Keep `system:serviceaccount:agent-system:agent-factory` and similar names as they are (recommended: they are identities other configs pin), or rename them in a later phase 3?
2. **JWT audiences** `room-broker` and `rooms-system`: keep them as legacy identifiers (recommended), or plan a token re-issue?
3. **The Go module rename timing.** It is pure churn for anyone importing the module. There are no known external importers, so phase 1 is fine.
