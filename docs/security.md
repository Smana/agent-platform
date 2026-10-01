# Security

The broker is the one place that authorizes room actions, so it is designed to fail safe (phase 1;
the database guarantees land with AP-1, Ruling Y): **a compromised broker still cannot rewrite
history, create a run, or reach a harness's keys**. The
database enforces append-only storage (Ruling Y), only SP3's factory creates runs (C3), and events
are pushed by the sandbox rather than pulled from it. Approvals are oversight, not a boundary: what
a run can do is fixed when it is created, by controls outside the sandbox.

## Threat model

The design's threats, with the controls this repository implements and where each lands.

| # | Threat | Controls | Here | Residual |
|---|---|---|---|---|
| T1 | Prompt injection by a human participant | Only members prompt; attribution is immutable; a run cannot exceed its profile; side-effect classes need an approver | Room roles (phase 2), approvals (phase 5), the broker-stamped `actor` (phase 1) | Wasted budget, bad code; bounded by budgets and the merge gate |
| T2 | Injection from agent to agent (brief, verdict, branch code) | No direct agent-to-agent path; peer text fenced as data; reviewers have read-only forge scope; a verdict has no power beyond SP3's gate | The fenced brief (phase 4); room tools are the only agent write path (phase 3) | An injected reviewer can approve bad code *inside the room*; CI and the merge gate remain |
| T3 | Injection into humans | Approval cards render the raw, redacted action, not the agent's prose, plus the `causedBy` author | Phase 5 | Social engineering of an approver |
| T4 | Impersonation | The broker stamps `actor` from the credential; offline JWT checks; a live `AgentRun` watch; re-validated human tokens; a run joins only its own `roomRef` | Phase 1 (runs, system), phase 2 (humans) | A token from a replaced pod of a still-live run is accepted until it expires (≤ 600 s) |
| T5 | Replay, duplicates | Unique idempotency keys; single-use `approvalId`; `driverEpoch` fencing; short-lived tokens | Phase 1 (keys), 4 (epoch), 5 (approvals) | — |
| T6 | Self-confirmation at the harness | Approvals are oversight by design; consequential actions are enforced outside the sandbox; agent events are untrusted claims | Phase 5 | A self-confirmed in-profile action is logged as reported; gateway and forge logs are ground truth |
| T7 | Authorization bypass at the broker | One enforcement point; every action re-checked against database state; `not_permitted` counted and alerted | The policy matrix and its table test (phase 2); `RoomRejectedActionsSpike` | Broker bugs |
| T8 | Transcript leakage | Redaction before append; members only; tailnet host; no transcript in PRs; append-only role; retention | Phase 1 (redaction, roles, retention); phase 2 (members); phase 3 (the PR carries a room id, not a transcript) | Secret shapes gitleaks does not know |
| T9 | Cross-site WebSocket hijacking | `Origin` check; oauth2-proxy cookie `SameSite=Strict` | Phase 2 | — |
| T10 | XSS from LLM output | Markdown rendered with HTML disabled; strict CSP; HttpOnly cookie | Phase 2 | XSS could still act *as* the user through the page |
| T11 | Denial of service | Size and rate limits; byte budgets per connection; authentication before subscription; gateway budgets bound agent loops | Phase 1 (payload and batch limits), phase 2 onwards (connections, rates) | A tailnet member can load the broker |
| T12 | Broker compromise | No harness keys (events are pushed); cannot rewrite history; its own CNP; runs only through the factory API, under a live human's token and budget | Phase 1, with Ruling Y for history | Reads every room; can request runs as a connected human |

## Identities

Runs and system callers: phase 1 / AP-1. Humans: phase 2 / AP-2.

| Principal | Credential | Validated by the broker | Canonical id |
|---|---|---|---|
| Agent run | A projected ServiceAccount token of `xplane-run-<runId>` in `agents`, audience `room-broker`, 600 s, mounted **only** in the bridge container | Offline against the JWKS of an issuer in `runIssuers`; `sub` must match the issuer's `subPattern`; then the `AgentRun` must be live and name a room | `agent:<runId>` |
| System caller | A ServiceAccount token with audience `rooms-system` (ruling P3: SP1's Kyverno policies reserve every `room-broker*` audience for `agents`) | Offline against `systemIssuer`; `sub` must be a key of the `systemPrincipals` allowlist | e.g. `system:factory` |
| Human | A ZITADEL ID token and a JWT access token, both from oauth2-proxy (phase 2) | Offline: issuer, expiry, groups; `aud` holds the project id and a rooms client, and the token names that client: the ID token in `azp` (Ruling AS), the access token in `client_id`, since ZITADEL access tokens carry no `azp`. The two must share the `sub` and the client | `human:<sub>` |

Why offline: every consumer validates issuer-agnostically (C2 r5). A runtime whose identities are
not ServiceAccounts needs one more `runIssuers` entry, not a new code path. Liveness comes from the
`AgentRun` itself (`status.phase` and the `agents.ogenki.io/revoked` annotation): when a run turns
terminal, revoked or deleted, every replica drops its connections on the watch event.

The allowlist of system callers ships **empty**. SP3's entry,
`system:serviceaccount:agent-system:agent-factory: system:factory`, stays commented until SP3 ships.

**Humans.** The `rooms-proxy` ZITADEL client issues JWT access tokens, unlike the platform's other
clients, so that the broker and the factory can validate them offline. oauth2-proxy keeps them in an
HttpOnly, `SameSite=Strict` cookie, away from a page that renders LLM output. Two ZITADEL project
roles, flattened into `groups`, gate access: `agents-admin` (owner and approver everywhere) and
`agents-member` (watches everywhere, may create rooms). Anyone else is refused at oauth2-proxy and
again by the broker. Revoking a group takes effect within the hour, when connections re-authenticate.
When the broker asks SP3 for a run on a human's behalf (phase 4), it forwards that human's access
token, never an asserted `sub`, so the factory proves the principal itself (C4).

## Authorization

The design's §1 matrix, enforced by the broker from phase 2 (`internal/policy`, tested row by
row). Room roles are cumulative; the approver flag is independent; the driver is one token.

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

- **Never an agent approves** (S10): one injected transcript would otherwise approve another's
  action.
- **Never from `roomctl`**: a token issued to the `roomctl` client cannot decide, steer,
  interrupt or move the driver token (ruling P18), because a local agent could run it.
- **Four-eyes** (OD-16, off by default, per room): the humans in the triggering turn's `causedBy`
  chain cannot decide it.
- **An invite is two writes**: the Room CR, then the `participant` event. If the append fails after
  the update, the membership briefly has no record; retrying the act with the same `clientSeq`
  heals it (the update is then a no-op, and the append writes or replays the record). The CR
  change is also in the Kubernetes audit log. A sealed room refuses the invite before the update.
- **The broker never creates `AgentRun`s**: its RBAC is `get`, `list`, `watch`, `delete` on
  `agentruns` in `agents` (C3); CRUD on `rooms` in `agent-system`; leases; no cluster-admin.

## TLS on :8443

The bridge-to-broker hop serves TLS **on both clouds** (GP-18). gcp-0's Cilium has no WireGuard to
encrypt pod traffic, and one configuration for both clouds is simpler than two. The broker and bridge
sides are AP-1; the manifests are CC-S2 and S1 (planned).

| Piece | Setting |
|---|---|
| Certificate | cert-manager `Certificate room-broker-tls` in `agent-system`, issuer `ClusterIssuer openbao` (the platform's private CA) |
| Names | `room-broker.agent-system.svc.cluster.local`, `room-broker.agent-system.svc` |
| Lifetime | `duration: 720h`, `renewBefore: 240h` |
| Broker | Mounts the Secret at `/etc/room-broker/tls/{tls.crt,tls.key}`; `ListenAndServeTLS` with a `GetCertificate` that re-reads the pair when the files change, so a renewal needs no restart. A plain-HTTP request fails |
| CA distribution | An ExternalSecret `room-broker-ca` in `agents` copies the platform CA chain, key `ca.crt` (S1 picks the store: `agents-secrets` is a namespaced SecretStore in `agent-system` and cannot serve `agents`) |
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
never payloads. Status: AP-1 (`internal/redact`).

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

Human actions (phase 4) run every payload they append through the same redactor, a take's reason
included, and a queued message's row keeps the redacted text the next run's brief quotes (review M7).
Phase 5 applies it to the action on an approval card. The harness also redacts GitHub tokens from its own step log before
printing (cloud-native-ref H-1, review M4).

## Database roles

| Role | Can | Cannot |
|---|---|---|
| `rooms_owner` | Own and migrate the schema | Create roles |
| `rooms_broker` | Append and read events; insert room rows; update only the `rooms` columns it must move (Ruling Y) | Update or delete events; unseal, re-date or re-time a room; move `last_seq` by anything but +1 |
| `rooms_retention` | Find sealed (Ruling Y) rooms closed past their retention, then delete their events and rows | Read any event column but `room_id`, or any `rooms` column but the four that decide expiry (column grants); see or delete a room that is not expired, or its events (row-level security, Ruling AX) |

Details and the tests that prove them: [room log](room-log.md#the-guarantees).

## Network

Planned, S1 onwards: these policies live in cloud-native-ref. Default deny everywhere; one allow per
flow (spec §9, as the plan builds it).

| Endpoint | Ingress | Egress |
|---|---|---|
| `room-broker` | Run pods in `agents` (label `agents.ogenki.io/run-id`) and the factory on 8443; oauth2-proxy on 8080 (phase 2); `agent-router` proxies on 8090 (phase 3); `vmagent` and kubelet on 9090 | DNS with an L7 rule; the Kubernetes API; CNPG on 5432; the run issuer's JWKS host on 443; the identity provider (phase 2); `api.github.com` on 443 (phase 3); the factory's run API (phase 4) |
| Run pod | kubelet on the bridge's 8085 | The broker on 8443, only when `roomRef` is set (the run's own CNP) |
| CNPG `xplane-rooms` | The broker and the retention job on 5432; the Atlas and CNPG operators; `vmagent` on 9187 | DNS, the Kubernetes API, peers, the backup plugin, object storage |
| Retention job | None | DNS; CNPG on 5432 |

On gcp-0 the identity-provider rule is `toEntities: [all]` without ports: a port-scoped rule to the
cluster's own gateway hairpins through the socket load balancer and is dropped (ruling P11a).

## Supply chain

| Control | How |
|---|---|
| Pinned actions | Every `uses:` is pinned to a 40-character commit SHA, with the version in a comment; Dependabot keeps them current |
| Least privilege in CI | `permissions: {}` at the top of every workflow, granted per job; `persist-credentials: false` on every checkout; timeouts on every job |
| Static analysis | CodeQL (`security-extended`, Go) on every PR, on `main` and weekly; `golangci-lint` with `gosec`, `noctx`, `errorlint`, `bodyclose`, `sqlclosecheck` and more ([AGENTS.md](../AGENTS.md)); no rule disabled in production code, while tests and `cmd/` carry scoped exclusions |
| Vulnerabilities | `govulncheck` in `task check`, so on every PR and before every release |
| Dependency drift | `go mod tidy -diff` in `task check`. Dependabot for actions, Go modules and the Docker base images: weekly, a 7-day cooldown, majors ignored while the stack is open |
| Scorecard | OpenSSF Scorecard weekly on `main`, results published and uploaded to code scanning |
| Images | Static binaries on digest-pinned distroless `static-debian12:nonroot`, multi-arch (`linux/amd64`, `linux/arm64`). Never `latest`: consumers pin by digest |
| Attestations | An SPDX SBOM and SLSA provenance (`mode=max`) attached to every pushed image |
| Signing | A keyless cosign signature on the digest, never the tag, from the workflow's OIDC identity |
| Releases | The release job re-runs `task check` (a tag can name a commit that never passed CI) and uses no build cache |
| Repository | `main` is protected by a ruleset: pull requests only, squash merges, required checks `check` and `analyze`, linear history, no force-push or deletion; admins may bypass, through a pull request only. Private vulnerability reporting and Dependabot security updates are on |

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

No `v*` tag exists yet, so today this fails for every image: the first release, and a ruleset that
restricts who may push `v*` tags, are Phase 7 items. Until that ruleset exists, the tag anchor proves
which workflow built an image, not who was allowed to tag it.

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
