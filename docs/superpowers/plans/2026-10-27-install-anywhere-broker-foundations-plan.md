# Install Anywhere Broker Foundations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the independently testable broker foundations for install-anywhere—local human access, TokenReview authentication, an optional room-tools listener, and owner-only embedded SQL migrations—without changing existing production defaults.

**Architecture:** Preserve the existing `config.Load` → `app.authenticators` → listener wiring and add modes at those seams. `internal/authn` owns a small verifier interface and a TokenReview verifier against a consumer-owned Kubernetes `Create` interface; `internal/app` alone constructs the client-go adapter. `internal/store` owns embedded migration execution and its revision ledger, while `room-broker migrate` reads a separate owner DSN.

**Tech Stack:** Go 1.27.3, stdlib `testing` with hand-written fakes, pgx/v5 and PostgreSQL 18 testcontainers, client-go TokenReview authentication v1, kind/containerd/gVisor and kubectl for the throwaway spike.

**Spec:** docs/superpowers/specs/2026-10-18-install-anywhere-design.md

**Companion plan:** [Maku brand integration](2026-10-27-maku-brand-integration-plan.md) is a separate docs/assets-only change; it does not alter Tasks 1–6 or authorize branding work within them.

## Global Constraints

- Existing configuration keeps its behavior: human/issuer modes default to `oidc`/`jwks`, absent `roomTools` means enabled, absent `Origin` passes, and `ROOMS_MCP_KEY` remains the only room-tools key. Every-route Origin middleware is local-mode-only; OIDC discovery/static routes stay byte-compatible and retain only the current `authn.Humans` Origin checks on authenticated routes.
- `config.Load` remains strict decode → `ApplyDefaults` → `Validate`; startup validation aggregates errors and names fields and allowed values. Do not add special `roomTools` validation beyond strict decoding and its defined value checks.
- Never log or return a raw token, subject, authorization header, DSN, or secret. TokenReview cache keys are SHA-256 digests. Errors wrap with `%w` when an underlying error is safe to retain; authentication failures wrap `authn.ErrUnauthenticated` without echoing credential data.
- Secrets come from environment or mounted files, never argv. Serve/retention keep `ROOMS_DATABASE_URL`; migrate alone reads `ROOMS_MIGRATION_DATABASE_URL` as `rooms_owner`. Do not widen database grants, edit migrations or `atlas.sum`, or add runtime Atlas-sum verification; `task migrations` remains the Atlas CI check.
- Use stdlib tests and hand fakes, `-race` on targeted Go tests, and testcontainers only in `internal/store`. Keep interfaces consumer-owned and minimal, constructors concrete, errors classified with `errors.Is`/`errors.As`, clocks injectable, and new Go files SPDX-headed. `TokenVerifier` interface fields hold only non-nil concrete values or untyped nil—never a typed-nil `*authn.Verifier`; do not add reflection or a generic authentication framework.
- No Maku rename, chart/factory changes, `roomctl run`, `hack/quickstart`, `docs/quickstart.md`, or quickstart/e2e CI work. Spike artifacts are throwaway under `/tmp/opencode/install-anywhere-spike/` and are never merged.
- Execute and review tasks sequentially. Run targeted suites per task, never concurrent full suites, and run the full `task check` once in Task 6 before one stage commit with no attribution or co-author lines.

## Review Focus

- **Foreign versus absent `Origin`, including discovery, ordinary HTTP and WebSocket upgrade:** Task 2 `TestLocalHumanOriginPolicy` and `TestLocalHumanWebSocketOrigin` prove local foreign non-empty origins are 403 while absent origins and `http://localhost:8080` pass; OIDC public discovery/static behavior stays unchanged and its authenticated routes keep the current `authn.Humans` checks.
- **Absent `systemIssuer` must not become a nil dereference or remove routes:** Task 3 `TestSystemsWithoutIssuer` and `TestSystemRouteWithoutIssuer` prove a non-nil authenticator returns 401, not 404, with or without a bearer.
- **TokenReview audience multiplicity, claims lifetime and cache expiry:** Task 3 `TestTokenReviewAudiences`, `TestTokenReviewPositiveCache` and `TestTokenReviewRawExpiryPast` pin exactly one status audience, raw/zero `Claims.ExpiresAt`, cache-only `min(exp,now+5m)`, fail-closed past `exp`, and no negative cache under an injected clock.
- **A txmode-none failure can leave an INVALID index:** Task 5 `TestMigrateRefusesInvalidIndex` pins the owned index and exact `DROP INDEX CONCURRENTLY public.<index>;` recovery instruction; `TestMigrateIgnoresUnrelatedInvalidIndex` proves third-party invalid indexes are not inspected or refused.
- **Existing config bytes and absent listener blocks:** Task 2 `TestExistingConfigDefaultsUnchanged`, Task 3 `TestExistingJWKSConfigUnchanged`, and Task 4 `TestRoomToolsDefaultAndStrictDecode` prove current YAML needs no edit, remains JWKS/OIDC, and still opens `:8090` with unchanged key gating.

---

### Task 1: Feasibility spike and go/no-go record

**Files:**
- Modify: `docs/superpowers/plans/2026-10-27-install-anywhere-broker-foundations-plan.md` (append only the dated findings and decision under this task)
- Create, never commit: `/tmp/opencode/install-anywhere-spike/{kind-config.yaml,runsc-pod.yaml,loopback-pod.yaml,findings.md,logs/}`

**Interfaces:**
- Consumes: Linux Docker, kind, kubectl, containerd, gVisor `runsc`/`containerd-shim-runsc-v1`; the spec's loopback fallback and stop rules.
- Produces: recorded `pass`, `fail`, or `unverified` outcomes for gVisor and port-forward on Linux and macOS, plus a go/no-go decision for Task 2 and for the later quickstart plan. No repository product artifact.

- [ ] **Step 1: Preflight and capture the environment**

Run `uname -a`, `docker version`, `kind version`, `kubectl version --client`, and `runsc --version` when present; save output under `logs/`. Require Linux Docker for the Linux cases. Do not treat missing macOS hardware as a failure or a pass.

- [ ] **Step 2: Run the Linux gVisor-in-kind case**

Create a throwaway kind cluster whose node mounts the downloaded runsc binaries and whose containerd registers the `runsc` handler with `systrap`; start a pod with `runtimeClassName: runsc`. Record the exact binary/image versions and whether the pod becomes Ready and executes a command. On failure, retain events/logs, then delete the cluster. A gVisor failure is `fail` for the sandboxed quickstart path and routes that later plan to its explicit `ALLOW_UNSANDBOXED=1` behavior; it does not silently permit unsandboxed execution.

- [ ] **Step 3: Run the Linux loopback port-forward boundary case**

Start a pod with no Service that listens on `127.0.0.1:8080` only. From the host run `kubectl port-forward pod/<pod> 18080:8080`, then prove `curl --fail http://127.0.0.1:18080/` succeeds. From a second pod, prove the first pod's IP at `:8080` is unreachable. Save commands, status codes, pod IP and logs without credentials.

- [ ] **Step 4: Apply the only permitted fallback if Step 3 fails**

Repeat with the server bound to the pod IP, still no Service, and Cilium installed. Identify the source identity observed for port-forward and test a broker-namespace CiliumNetworkPolicy scoped only to `:8080`: pod-originated access must fail, port-forward must pass, and probe/metrics/bridge traffic on `:9090`/`:8443` must retain current access. If that identity cannot be expressed without weakening those boundaries, record `no-go: design amendment required`; do not substitute a namespace-wide deny or unauthenticated cluster-network listener.

- [ ] **Step 5: Record macOS honestly**

If no macOS/Docker Desktop host is available to this Linux executor, write exactly: `macOS: unverified — no macOS host or Docker Desktop VM available from this Linux executor; gVisor-in-kind and kubectl port-forward to 127.0.0.1:8080 were not run; this is not a pass.` If a real macOS host is available, run the same two cases and replace that sentence with dated evidence.

- [ ] **Step 6: Record findings and enforce the gate**

Append the environment, four platform/risk outcomes, and go/no-go rationale to this task and save the same evidence in `findings.md`. Port-forward `pass` permits Task 2; its fallback may proceed only if every boundary assertion passes. A required design amendment stops Task 2 and all dependent quickstart work, while Tasks 3, 4 and 5 may continue only where still valid. gVisor `unverified`/`fail` does not block independent TokenReview, roomTools or migrate foundations. Delete every cluster and keep `/tmp/opencode` artifacts unmerged.

**Review gate:** findings distinguish evidence from unverified results, and the selected path does not weaken loopback, sandbox, bridge, metrics or probe boundaries.

#### Findings — 2026-10-27 (Linux executor; full evidence in `.superpowers/sdd/2026-10-27-install-anywhere-broker-foundations-plan/task-1-report.md` and `/tmp/opencode/install-anywhere-spike/findings.md`, artifacts unmerged)

**Environment.** Linux `ogenki` 7.2.11-arch1-1 x86_64; Docker 29.9.2 (engine + containerd 2.4.2, runc 1.5.2, cgroup v2); kind v0.32.0 creating `kindest/node:v1.37.0` (server v1.37.0, node containerd 2.3.4); kubectl v1.29.4; gVisor `runsc release-20260928.0` (spec 1.2.1) with `containerd-shim-runsc-v1` and `gvisor-bin/`, sha512-verified from `storage.googleapis.com/gvisor/releases/release/20260928.0/x86_64/`; workloads `busybox:1.37.0`. `runsc` was not installed on the host before the spike.

| Case | Outcome | Evidence |
|---|---|---|
| gVisor in kind, Linux: node mounts `runsc`/shim/`gvisor-bin`, containerd registers handler `runsc` with `platform = "systrap"` via `ConfigPath`, pod with `runtimeClassName: runsc` | **pass** | Pod Ready in seconds; `uname -a` shows `4.19.0-gvisor`, dmesg `Starting gVisor...`, `crictl inspectp` reports `runtimeType: io.containerd.runsc.v1` / `runtimeHandler: runsc`; the node's `config.toml` is `version = 2`, so the patch key is `io.containerd.grpc.v1.cri` |
| `kubectl port-forward` from the host to a pod listening on `127.0.0.1:8080` only, no Service, broker pod's runtime (no `runtimeClassName`) | **pass** | `Forwarding from 127.0.0.1:18081 -> 8080`; `curl --fail http://127.0.0.1:18081/` → `http_code=200` and the pod's body; `kubectl get svc -A` shows only `kubernetes`/`kube-dns`; from a second pod the same pod's `:8080` is `Connection refused` (wget rc=1, `nc` rc=1) while its `:8081` control answers, so the refusal is the binding, not the CNI |
| The same pod shape under `runtimeClassName: runsc` | **fail** (constraint, not the loopback claim) | Port-forward fails on **every** port: `curl: (52) Empty reply from server` for `8080` and for the `0.0.0.0:8081` control. containerd: `failed to connect to localhost:8080 inside namespace …: connection refused`. The netns the CRI port-forward dials into has no listeners for the runsc pod, and `127.0.0.1:8080` + `0.0.0.0:8081` for the runc pod: gVisor's netstack owns the sockets. So `port-forward` cannot reach a runsc pod at all with kind's containerd 2.3.4 + runsc default netstack mode |
| macOS, both cases | **unverified** | macOS: unverified — no macOS host or Docker Desktop VM available from this Linux executor; gVisor-in-kind and kubectl port-forward to 127.0.0.1:8080 were not run; this is not a pass. |
| Step 4 fallback (pod-IP bind + broker-namespace CiliumNetworkPolicy on `:8080`) | **not run, not needed** | Step 3 passed, so no Service, no namespace-wide deny and no cluster-network listener were introduced; Cilium stayed out of the spike |

**Decision: go.** Port-forward to a `127.0.0.1:8080` listener with no Service passes on Linux, so Task 2 proceeds unchanged and no design amendment is required. The sandboxed path also passes on Linux (`systrap`), so the later quickstart plan's gVisor route stands and needs no `ALLOW_UNSANDBOXED=1` fallback on this platform. Two constraints carry forward: (1) the broker pod must keep the chart's default runtime — `charts/agent-platform/templates/broker-deployment.yaml` sets no `runtimeClassName` — because a runsc broker is unreachable by port-forward on any port, and the Step 4 fallback would not rescue it; (2) macOS is unverified, so `up.sh` must stop with the reason there and may not answer a loopback or port-forward failure by exposing `:8080`. The local `publicURL` port and the forwarded port must agree (`curl`/browser reach `http://localhost:<forwarded port>` while the CSRF check compares against `publicURL`), which Task 2's accepted `http://localhost:18080` already allows. The throwaway cluster was deleted (`kind get clusters` → none, no `io.x-k8s.kind.cluster` containers, no `kind-install-anywhere-spike` context, no leftover port-forward); no repository artifact and no commit came from this spike.

### Task 2: Local humans, loopback/CSRF, discovery and roomctl

**Files:**
- Create: `internal/authn/local.go`, `internal/authn/local_test.go`, `internal/app/listeners.go`, `internal/app/listeners_test.go`
- Modify: `internal/config/config.go`, `internal/app/broker.go`, `internal/app/jwks.go`, `internal/app/rooms.go`, `internal/humanapi/server.go`, `internal/humanapi/rooms.go`, `internal/roomctl/config.go`, `internal/roomctl/auth.go`, `internal/roomctl/client.go`, `internal/roomctl/doctor.go`, `internal/roomctl/mcp.go`
- Test: `internal/config/config_test.go`, `internal/app/broker_test.go`, `internal/app/jwks_test.go`, `internal/app/rooms_test.go`, `internal/app/roomctl_test.go`, `internal/app/roomctl_onboarding_test.go`, `internal/humanapi/rooms_test.go`, `internal/humanapi/humans_test.go`, `internal/roomctl/roomctl_test.go`, `internal/roomctl/doctor_test.go`, `internal/roomctl/mcp_test.go`

**Interfaces:**
- Consumes: Task 1's go decision for local human access; existing `humanapi.Authenticator`, `humanServer`, `roomctl.Config`, `Discover`, `Login`, `Token`, `Client.header` and `Relay.forward` seams.
- Produces:
  - `const config.HumanModeOIDC = "oidc"; const config.HumanModeLocal = "local"`; `HumanConfig.Mode string \`json:"mode,omitempty"\``.
  - `func (h config.HumanConfig) EffectiveGroups() config.GroupsConfig`, returning `agents-admin` and `agents-member` only for local mode, otherwise the configured groups.
  - `authn.NewLocalHumans(origin, adminGroup, memberGroup string, now func() time.Time) *authn.LocalHumans`; `Authenticate(*http.Request) (authn.Principal, error)` yields `human:local`, `Sub:"local"`, `ClientID:"local"`, both groups, one-hour expiry, no access token, and ignores any bearer.
  - `humanapi.Server.HumanMode string` and `humanapi.Server.Origin string`; discovery adds `humanMode`. App sets `Origin` only in local mode, where non-empty foreign `Origin` is rejected before every route, including discovery/static and WebSocket upgrade. OIDC leaves `Origin` empty, installs no such middleware, and keeps its current public routes and `authn.Humans` checks.
  - `app.openBrokerListeners(ctx context.Context, cfg config.Config, listen func(context.Context,string,string)(net.Listener,error)) (human, roomTools, ops net.Listener, err error)`; local human address is exactly `127.0.0.1:8080`, OIDC remains `:8080`, ops remains `:9090`.
  - `roomctl.Config.HumanMode string \`json:"humanMode,omitempty"\``. Local validation permits absolute URLs without issuer/client/project IDs, but a plain-`http` URL is accepted only when its hostname is `localhost` or `net.IP.IsLoopback()` is true. Non-loopback plain HTTP is refused before any discovery request, even if the server would report `humanMode:"local"`; HTTPS discovery and validation are unchanged.

- [ ] **Step 1: Write failing config compatibility and local-validation tests**

Add `TestExistingConfigDefaultsUnchanged`: load the existing `valid` bytes unchanged and assert OIDC mode, existing fields and defaults. Add `TestLoadLocalHuman`: `human: {mode: local}` plus no OIDC fields defaults `publicURL` to `http://localhost:8080`; table-test every set OIDC field (`issuer`, `jwksURL`, both client ID files, `projectIDFile`, `origin`, groups, `access`, and enabled/sponsored `mcp`) and invalid `human.mode`, asserting field-named errors. Table-test `publicURL` values `http://localhost:18080` and `http://127.0.0.1:8080` accepted, and `https://rooms.example`, `http://0.0.0.0:8080`, and a URL with a path refused.

- [ ] **Step 2: Run the config tests and confirm the missing mode fails**

Run: `go test -race -count=1 ./internal/config -run 'Test(ExistingConfigDefaultsUnchanged|LoadLocalHuman)$'`
Expected: FAIL because `mode` is unknown and local defaults/validation do not exist.

- [ ] **Step 3: Implement human mode defaults and validation**

Add the mode constants/field and `EffectiveGroups`. Default empty mode to `oidc`; local mode defaults only `publicURL`, requires a canonical plain-HTTP origin with loopback host (`localhost` or an IP whose `IsLoopback` is true), and rejects every OIDC/MCP/access field. OIDC follows the current validation path unchanged.

- [ ] **Step 4: Run config tests to green**

Run: `go test -race -count=1 ./internal/config`
Expected: PASS, including all pre-existing validation tests.

- [ ] **Step 5: Write failing local authentication, HTTP and listener tests**

Add `TestLocalHumanAuthenticate`: with a fake clock fixed at `2026-10-27T12:00:00Z`, no bearer and a bogus bearer both return `human:local`, groups `agents-admin` and `agents-member`, and `Expiry.Equal(fakeNow.Add(time.Hour))`; `Origin:http://localhost:8080` passes, while `https://evil.example`, `null`, and `http://localhost:8081` return `ErrForbidden`; absent `Origin` passes. Add `TestLocalHumanOriginPolicy`: local `GET /api/rooms`, public `GET /.well-known/roomctl`, and static `GET /` admit no token/no Origin, foreign Origin is 403 on all three, and discovery emits `humanMode:"local"`. The same test proves OIDC discovery emits `oidc` and its public discovery response, including a foreign Origin, remains unchanged; existing OIDC authenticated-route tests retain the `human.origin` check. Add `TestLocalHumanWebSocketOrigin`: no-Origin upgrade reaches the authenticated local principal, foreign Origin is 403 before upgrade. Add `TestHumanListenerAddress`: local listen call is `127.0.0.1:8080`, OIDC is `:8080`.

- [ ] **Step 6: Run the new authentication/API/listener tests red**

Run: `go test -race -count=1 ./internal/authn ./internal/humanapi ./internal/app -run 'Test(LocalHuman|HumanListenerAddress)'`
Expected: FAIL because the local authenticator, mode discovery/middleware and address selection are absent.

- [ ] **Step 7: Implement local authentication and listener/API wiring**

Implement `LocalHumans` behind the existing `humanapi.Authenticator`; change app auth/human-side parameters to that interface rather than `*authn.Humans`. Build no human JWKS verifier in local mode. Pass `EffectiveGroups` to both server and actor, set local `WebClient` to `func() string { return "local" }`, and set `HumanMode` on both discovery responses. Set `Server.Origin` from `publicURL` and install the every-route Origin middleware only in local mode; leave it empty and uninstalled in OIDC, preserving current discovery/static responses and the existing `authn.Humans` checks as defense-in-depth. Open the selected human address through `openBrokerListeners`, closing already-opened listeners on a later error.

- [ ] **Step 8: Run the authentication/API/listener suites green**

Run: `go test -race -count=1 ./internal/authn ./internal/humanapi ./internal/app -run 'Test(LocalHuman|HumanListenerAddress|Authenticators|HumanServer|RoomctlSetup|UpgradeWithZITADELTokens)'`
Expected: PASS; local mode constructs no human verifier and OIDC tests retain their current token behavior.

- [ ] **Step 9: Write failing roomctl local discovery/no-token tests**

Add `TestDiscoverLocal`: an `httptest.NewServer` response `{"humanMode":"local"}` at `http://127.0.0.1:<port>` returns `Config{URL:base,HumanMode:"local"}` with empty IDs; `humanMode:"oidc"` with empty IDs still names the missing roomctl client. Add `TestRoomctlConfigureLocal`: URL-only configure on loopback saves local config without IDs; loopback plain HTTP from a non-local discovery response is refused; `http://rooms.example` is refused with zero requests against a counting HTTP fake even though that fake would answer `humanMode:"local"`; existing HTTPS discovery/flag override behavior is unchanged. Add `TestRoomctlLocalCommands`: `login` reports `local broker; no login needed` and writes no token file, `token` reports `local broker: no token required`, and REST, WebSocket and MCP requests carry no `Authorization` header. Add `TestDoctorLocal`: no token file/IdP calls, output contains `ok   login: local broker`, and `/api/rooms` succeeds without Authorization.

- [ ] **Step 10: Run roomctl tests red**

Run: `go test -race -count=1 ./internal/roomctl ./internal/app -run 'Test(DiscoverLocal|RoomctlConfigureLocal|RoomctlLocalCommands|DoctorLocal)$'`
Expected: FAIL on HTTP discovery and token-file/Authorization assumptions.

- [ ] **Step 11: Implement local roomctl behavior**

Decode/additive-validate `humanMode`. Before discovery, parse the requested URL and refuse non-loopback plain HTTP; permit plain-HTTP discovery only for `localhost` or an IP where `net.IP.IsLoopback()` is true, and require the response to report local mode. Apply the same loopback rule when validating a saved local plain-HTTP config. Leave HTTPS discovery and validation unchanged. Skip device login/token-file reads for local configs, emit the exact reports above, keep `Token` returning `"",nil` locally, and set Authorization only when the token is non-empty in `Client.header`, `Relay.forward` and doctor's room request. Keep every other command and OIDC flow unchanged.

- [ ] **Step 12: Run all affected roomctl and app tests green**

Run: `go test -race -count=1 ./internal/roomctl ./internal/app`
Expected: PASS, including existing OIDC onboarding, token refresh, doctor and relay tests.

**Review gate:** local access is loopback-only with an unauthenticated admin identity only behind the proven port-forward boundary; non-loopback plain HTTP is refused before discovery, loopback HTTP still requires local discovery, and production OIDC config plus public discovery/static behavior remain compatible.

### Task 3: TokenReview verifier and optional system issuer

**Files:**
- Create: `internal/authn/tokenreview.go`, `internal/authn/tokenreview_test.go`, `internal/app/tokenreview.go`, `internal/app/tokenreview_test.go`
- Modify: `internal/authn/runs.go`, `internal/authn/systems.go`, `internal/config/config.go`, `internal/app/jwks.go`, `internal/app/broker.go`
- Test: `internal/authn/authn_test.go`, `internal/config/config_test.go`, `internal/app/jwks_test.go`, `internal/bridgeapi/server_test.go`

**Interfaces:**
- Consumes: existing `authn.Verifier.Verify(ctx, raw, audience)`, `RunIssuer`, `Runs`, `Systems`, `AudienceRun="room-broker"`, `AudienceSystem="rooms-system"`, and app-only client-go wiring.
- Produces:
  - `type authn.TokenVerifier interface { Verify(context.Context,string,string) (*authn.Claims,error) }`; `RunIssuer.Verifier` and `Systems.v` use it, so existing non-nil `*authn.Verifier` remains valid. Interface assignments use only non-nil concrete values or untyped nil; app never stores a typed-nil `*authn.Verifier`, and `NewSystems(nil, ...)` remains the intentional never-authenticating path.
  - `type authn.TokenReviewer interface { Create(context.Context,*authenticationv1.TokenReview,metav1.CreateOptions) (*authenticationv1.TokenReview,error) }`, exactly satisfied by client-go's `TokenReviewInterface`.
  - `authn.NewTokenReviewVerifier(reviewer authn.TokenReviewer, opts ...authn.TokenReviewOption) *authn.TokenReviewVerifier` and `authn.WithTokenReviewClock(now func() time.Time) authn.TokenReviewOption`.
  - `config.IssuerConfig.Mode string \`json:"mode,omitempty"\`` with `jwks` default/`tokenreview`; `Config.SystemIssuer *config.IssuerConfig \`json:"systemIssuer,omitempty"\``.
  - `type app.tokenReviewClient func() (authn.TokenReviewer,error)`; `func authenticators(ctx context.Context, cfg config.Config, m *metrics.Set, build newVerifier, reviews tokenReviewClient) (auth, error)` calls the provider only for a TokenReview issuer. `internal/app/tokenreview.go` obtains `restConfig` through `ctrl.GetConfig()`, constructs `kubernetes.NewForConfig(restConfig)`, and returns `clientset.AuthenticationV1().TokenReviews()`. Client-go client construction stays in app; the authn boundary necessarily imports Kubernetes `authenticationv1` and `metav1` API types, not the client-go client.

- [ ] **Step 1: Write failing mode/default and absent-system config tests**

Add `TestIssuerModesAndDefaults`: current JWKS bytes default to `jwks`; TokenReview accepts only `mode` and `subPattern`; unknown modes, TokenReview with `issuer`/`jwksURL`, and JWKS missing its current required fields fail with field names; no `audience` field is accepted. Replace the old no-system case with `TestSystemIssuerOptional`: absent/null issuer plus empty principals validates, and any non-empty `systemPrincipals` without it fails. Add `TestExistingJWKSConfigUnchanged` asserting the existing `valid` bytes and values still load.

- [ ] **Step 2: Run config mode tests red**

Run: `go test -race -count=1 ./internal/config -run 'Test(IssuerModesAndDefaults|SystemIssuerOptional|ExistingJWKSConfigUnchanged)$'`
Expected: FAIL on unknown `mode` and the old required-system validation.

- [ ] **Step 3: Implement issuer modes and optional system configuration**

Add the fields, pointer, defaults and mode-specific validation, updating existing Go fixtures to pointer values without changing YAML fixtures.

- [ ] **Step 4: Run config mode tests green**

Run: `go test -race -count=1 ./internal/config`
Expected: PASS.

- [ ] **Step 5: Write failing TokenReview status/audience tests**

Use a hand fake recording `Spec.Token` and `Spec.Audiences`. `TestTokenReviewAudiences` covers authenticated status with `status.audiences:["room-broker"]` accepted for `AudienceRun`; `["rooms-system"]`, `[]`, `["room-broker","rooms-system"]`, and `Authenticated:false` refused (`ErrWrongAudience` for a wrong/multi/nonempty audience, `ErrUnauthenticated` for every refusal). Assert each request has exactly the expected one-element audience and username `system:serviceaccount:agents:run-7f3cq2xz`; an API/status error containing the marker token or username must never appear in the returned error.

- [ ] **Step 6: Write failing cache timing and negative-cache tests**

For `TestTokenReviewPositiveCache`, start the fake clock at `2026-10-27T12:00:00Z` and use unsigned three-part JWTs solely for `exp` decoding. With `exp=t0+2m`, every returned/cached `Claims.ExpiresAt.Time` equals `t0+2m`, the entry is cached until `t0+1m59s`, and a review occurs again at `t0+2m`. With `exp=t0+1h`, returned `Claims.ExpiresAt.Time` still equals `t0+1h`, while the cache entry is reviewed again at `t0+5m` after a cache hit at `t0+4m59s`. With malformed token `not-a-jwt`, `Claims.ExpiresAt` is non-nil and `Claims.ExpiresAt.Time.IsZero()` is true on initial and cached returns, while the cache follows the same 5-minute cap. Assert subject and API call counts. Add `TestTokenReviewRawExpiryPast`: an authenticated, exact-audience review whose raw `exp=t0-1s` returns `ErrTokenExpired` (and `ErrUnauthenticated`), and a second call performs a second review, proving no positive cache. `TestTokenReviewRefusalsNotCached` changes the same token from unauthenticated or multi-audience to authenticated and proves a second API call succeeds; a cached run-audience success must not authenticate `AudienceSystem` without a new review.

- [ ] **Step 7: Run TokenReview tests red**

Run: `go test -race -count=1 ./internal/authn -run 'TestTokenReview'`
Expected: FAIL because the verifier and consumer interface do not exist.

- [ ] **Step 8: Implement the TokenReview verifier and shared verifier interface**

Implement the exact `Create` request and status checks above. Cache only authenticated, exact-audience, non-past-exp positives under `[32]byte` SHA-256 keys, storing the audience and cache-only expiry `min(raw exp, now+5m)`; an undecodable/missing `exp` uses `now+5m`, and an entry at/past its cache expiry is removed and reviewed again. If decoded raw `exp` is at/past `now`, refuse with `ErrTokenExpired` and do not cache: the unverified claim may cause a refusal, never an acceptance, while TokenReview remains the authentication proof. Never cache other refusals or raw tokens, and synchronize cache access. Set returned `Claims.ExpiresAt` from the raw decoded exp, otherwise to `jwt.NewNumericDate(time.Time{})`; assert no cache deadline leaks into claims. A zero `Time` leaves the bridge stream without an exp deadline, with runwatch liveness still ending it. Change `RunIssuer`/`Systems` to `TokenVerifier`, assign only non-nil concrete values or untyped nil, and keep the nil Systems verifier returning `ErrUnauthenticated` without reflection or panic.

- [ ] **Step 9: Run TokenReview and existing authn tests green**

Run: `go test -race -count=1 ./internal/authn`
Expected: PASS, including existing run/system/JWKS tests and no-token-in-error assertions.

- [ ] **Step 10: Write failing wiring, Systems-route and metrics-exclusion tests**

Add `TestAuthenticatorsTokenReview`: a TokenReview-only, local-human, absent-system config builds non-nil runs/systems/humans, zero JWKS verifiers, and does not invoke the JWKS builder; a fake TokenReview authenticates a run with `AudienceRun`, and a separate config with a TokenReview `systemIssuer` authenticates the allowlisted factory subject with `AudienceSystem`. Add `TestAuthenticatorsMixedRunIssuers`: one JWKS and one TokenReview run issuer each authenticate their run token, while `auth.verifiers` contains only the one JWKS verifier and refresh/metrics registration names only its issuer. `TestAuthenticatorsBuildsTokenReviewClientOnlyWhenNeeded` proves a JWKS-only config does not call the provider. `TestTokenReviewOnlyHasNoJWKSMetrics` asserts a scrape contains no `rooms_authn_jwks_last_refresh_timestamp_seconds{issuer=...}` sample or placeholder issuer series. `TestSystemsWithoutIssuer` covers arbitrary/missing bearer; `TestSystemRouteWithoutIssuer` calls `GET /v1/rooms/3kq7x2ma/events` through the real bridgeapi handler with `authn.NewSystems(nil,nil)` and asserts 401 rather than 404/panic.

- [ ] **Step 11: Run wiring tests red**

Run: `go test -race -count=1 ./internal/authn ./internal/app ./internal/bridgeapi -run 'Test(AuthenticatorsTokenReview|AuthenticatorsMixedRunIssuers|AuthenticatorsBuildsTokenReviewClientOnlyWhenNeeded|TokenReviewOnlyHasNoJWKSMetrics|SystemsWithoutIssuer|SystemRouteWithoutIssuer)$'`
Expected: the new app/bridgeapi wiring tests FAIL on the old authenticator signature and unconditional system JWKS construction; the authn nil-Systems case is a regression control for Step 8.

- [ ] **Step 12: Implement app-only Kubernetes wiring and JWKS exclusions**

Construct TokenReview verifiers only for configured TokenReview run/system issuers, passing the fixed audience at `Verify`, and reuse subject patterns/allowlists unchanged. Keep only actual non-nil `*authn.Verifier` values in `auth.verifiers`, `refreshJWKS`, and `WatchJWKS`; mixed run issuers put only JWKS entries in those sets. Assign concrete values to `TokenVerifier` fields without typed-nil wrappers; absent system builds `NewSystems(nil, principals)` with untyped nil and no system verifier. Update `serveBroker` to supply the lazy client provider; do not add TokenReview metrics or refresh loops.

- [ ] **Step 13: Run all affected app and bridgeapi tests green**

Run: `go test -race -count=1 ./internal/app ./internal/bridgeapi`
Expected: PASS; existing production wiring tests and system routes retain behavior.

**Review gate:** the one-method authn boundary carries only Kubernetes API types, client-go client construction stays in app, audience rules are exact, claims and cache lifetimes stay distinct, interface values obey typed-nil discipline, and TokenReview-only or mixed mode creates JWKS work/metrics only for actual JWKS issuers.

### Task 4: Top-level roomTools listener switch

**Files:**
- Modify: `internal/config/config.go`, `internal/app/broker.go`, `internal/app/listeners.go`
- Test: `internal/config/config_test.go`, `internal/app/listeners_test.go`, `internal/app/mcp_test.go`, `internal/app/broker_test.go`

**Interfaces:**
- Consumes: Task 2 `openBrokerListeners`, existing `mcpAddr=":8090"`, `mcpKey`, `roomMCP`, `mcpServer`, and `serveHTTP` lifecycle.
- Produces: `type config.RoomToolsConfig struct { Enabled *bool \`json:"enabled"\` }`; `Config.RoomTools *config.RoomToolsConfig \`json:"roomTools,omitempty"\``; `func (c config.Config) RoomToolsEnabled() bool`, true for absent block or nil `enabled`, otherwise the pointed-to value.

- [ ] **Step 1: Write failing default/strict-decode and lifecycle tests**

Add `TestRoomToolsDefaultAndStrictDecode`: the existing `valid` bytes and `roomTools: {}` enable, `enabled:false` disables, `enabled:true` enables, and unknown fields or a string boolean fail strict decode. Extend `TestHumanListenerAddress` into `TestOpenBrokerListenersRoomTools`: with a recording listen fake, absent block calls exactly `:8080`, `:8090`, `:9090`; disabled calls exactly `:8080`, `:9090` and returns `roomTools=nil`; local+disabled calls `127.0.0.1:8080`, `:9090`. Add an ops-listen failure case proving already-opened listeners close. Retain `TestRoomMCP` assertions for valid/wrong/unset keys and its rejected-call counter.

- [ ] **Step 2: Run roomTools tests red**

Run: `go test -race -count=1 ./internal/config ./internal/app -run 'Test(RoomToolsDefaultAndStrictDecode|OpenBrokerListenersRoomTools|RoomMCP)$'`
Expected: the new `TestRoomToolsDefaultAndStrictDecode` and `TestOpenBrokerListenersRoomTools` FAIL because `roomTools` is unknown and `:8090` is unconditional; retained `TestRoomMCP` already passes and is a regression control.

- [ ] **Step 3: Implement the switch and conditional server lifecycle**

Add only the config type/helper above. In `serveBroker`, read the key, warn, and construct the room-tools handler only when enabled; open no `:8090` listener and register no room-tools goroutine when disabled. When enabled, preserve the current key, server bounds, drain, and rejection metrics. Keep a later listener-start failure closing every listener already opened.

- [ ] **Step 4: Run roomTools and listener tests green**

Run: `go test -race -count=1 ./internal/config ./internal/app -run 'Test(RoomToolsDefaultAndStrictDecode|OpenBrokerListenersRoomTools|RoomMCP|MCPKey|DrainsFitTheGrace)$'`
Expected: PASS; disabled mode makes no listen call and enabled mode has byte-compatible key behavior.

**Review gate:** absent config is production-compatible, explicit false creates no port/server/counter path, and `human.mcp` remains a separate listener/trust domain.

### Task 5: Owner-only `room-broker migrate`

**Files:**
- Create: `internal/store/migrate.go`, `internal/store/migrate_test.go`
- Modify: `internal/store/testdb_test.go`, `internal/app/broker.go`, `cmd/room-broker/main.go`
- Test: `internal/app/broker_test.go` (created `internal/store/migrate_test.go` and modified `internal/store/testdb_test.go` cover the database behavior)

**Interfaces:**
- Consumes: `pgx.Connect`, the nine current `migrations/*.sql` files (five txmode-none files), existing owner/broker/retention test roles, `store.Open` timeouts and `SchemaReady` (both unchanged), and `RunBroker` dispatch.
- Produces:
  - `func store.Migrate(ctx context.Context, dsn string) error`, using `//go:embed migrations/*.sql` only.
  - An unexported test seam `migrate(ctx context.Context, dsn string, files []migration) error`; `type migration struct { name, version, sql string; checksum [sha256.Size]byte; txmodeNone bool }` lets tests tamper inputs without editing embedded files.
  - `splitTxModeNoneStatements(sql string) []string` is the production splitter. `txModeNoneIndexNames(files []migration) []string` is the minimal ownership helper: only unqualified names created by `CREATE INDEX CONCURRENTLY` in embedded txmode-none files, unique and sorted; it never scans or claims every public index.
  - `RunBroker(..., []string{"migrate"}, getenv)` reads only required `ROOMS_MIGRATION_DATABASE_URL` and wraps migration errors with `room-broker migrate:`.
  - Revisions table: `room_broker_revisions(version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`; checksum is lowercase hex SHA-256. No migration file or grant changes.

- [ ] **Step 1: Write failing migration catalog and empty-to-head tests**

Add `TestEmbeddedMigrationCatalog`: expect sorted versions `20260927120000` through `20261023120000_webhooks`, nine SQL files, no `atlas.sum`/kustomization, and txmode-none exactly on verdicts, delivery indexes, brief index, approval indexes and task-facts index. Add `TestMigrateTxModeNoneStatements` at the new production splitter with a comment containing a semicolon plus `CREATE INDEX CONCURRENTLY`, `GRANT`, `COMMENT`, and `REINDEX`, asserting every statement survives; the distinct name coexists with the current `TestTxModeNoneStatements` until Step 13. Add `TestMigrateEmptyToHeadAndRerun` using a newly created empty database owned by `rooms_owner`: first run creates `events`, all current indexes and nine revisions whose checksums match embedded bytes; `store.Open` + `SchemaReady` reports ready; second run succeeds with unchanged revision count and `applied_at` values.

- [ ] **Step 2: Run migration tests red**

Run: `go test -race -count=1 ./internal/store -run 'Test(EmbeddedMigrationCatalog|MigrateTxModeNoneStatements|MigrateEmptyToHeadAndRerun)$'`
Expected: FAIL because embedded migration execution and `splitTxModeNoneStatements` do not exist; the old helper/test still compile, and Docker/PostgreSQL 18 starts through the existing store TestMain.

- [ ] **Step 3: Implement catalog, lock, ordered application and revisions**

Connect once with pgx as the DSN specifies; never fall back to the serving DSN. Take a fixed session-scoped `pg_advisory_lock` on that connection and defer unlock/close. Create the revision table itself and apply pending files in filename order, skipping recorded versions for the initial re-run success path. Transactional files and their revision insert commit/roll back together; txmode-none files use `splitTxModeNoneStatements` to execute every statement separately outside a transaction and record the revision only after all statements succeed. Leave owner enforcement and tamper/unknown-version preflight to the failing tests in Step 5. Do not read `atlas.sum` at runtime.

- [ ] **Step 4: Run empty/head/re-run tests green**

Run: `go test -race -count=1 ./internal/store -run 'Test(EmbeddedMigrationCatalog|MigrateTxModeNoneStatements|MigrateEmptyToHeadAndRerun)$'`
Expected: PASS with nine revisions and current head.

- [ ] **Step 5: Write failing tamper, unknown-version, role and transaction tests**

Add `TestMigrateRefusesTamperedRevision`: after a successful run, call the injected migrator with one flipped SHA-256 for `20260927120000`; error names that version and `checksum`, and no later file runs. Add `TestMigrateRefusesUnknownRecordedVersion`: insert `29990101000000` with 64 zeroes and assert refusal names it before schema work. Add `TestMigrateRequiresOwner` on a fresh empty database, using its broker and retention DSNs and asserting refusal before `room_broker_revisions` exists in that same database. Add `TestMigrateTransactionalFileRollsBack`: an injected two-statement file creates `migration_probe` then executes invalid SQL; assert neither table nor revision remains and the error names its filename.

- [ ] **Step 6: Run tamper, unknown-version and owner tests red**

Run: `go test -race -count=1 ./internal/store -run 'Test(MigrateRefusesTamperedRevision|MigrateRefusesUnknownRecordedVersion|MigrateRequiresOwner|MigrateTransactionalFileRollsBack)$'`
Expected: FAIL on missing owner enforcement and tamper/unknown-version preflight; the transactional rollback case is a regression control for the success-path transaction handling.

- [ ] **Step 7: Implement only the failure behavior exposed by Step 5**

Require `current_user == "rooms_owner"` before changing anything, and add filename/version-wrapped first-failure errors plus recorded-version/embedded-checksum preflight: refuse any recorded version absent from the embedded catalog or any checksum mismatch before applying files. Preserve the transactional rollback implemented for the success path; do not catch and continue after SQL errors.

- [ ] **Step 8: Run refusal and rollback tests green**

Run: `go test -race -count=1 ./internal/store -run 'Test(MigrateRefusesTamperedRevision|MigrateRefusesUnknownRecordedVersion|MigrateRequiresOwner|MigrateTransactionalFileRollsBack)$'`
Expected: PASS with exact version/file names and no partial transactional state.

- [ ] **Step 9: Write failing invalid-index and concurrency tests**

Add `TestTxModeNoneIndexOwnership`: the helper returns exactly `approvals_pending`, `events_acks`, `events_agent_verdicts`, `events_brief`, `events_decision_acks`, `events_decisions`, `events_deliveries`, `events_task_facts`, and `events_tool_results`, with no index from a transactional file. Add `TestMigrateRefusesInvalidIndex`: after full migration, delete the verdicts revision and mark `public.events_agent_verdicts` invalid through `pg_index` as superuser to simulate a half-completed concurrent build. Assert rerun refuses before applying later files and its error contains both `events_agent_verdicts` and `DROP INDEX CONCURRENTLY public.events_agent_verdicts;`. Execute that recovery as owner, rerun, and assert a valid index plus restored revision. Add `TestMigrateIgnoresUnrelatedInvalidIndex`: create `public.third_party_invalid`, mark it invalid the same way, and assert migrate succeeds and leaves it alone because it is outside the embedded ownership catalog. Add `TestMigrateConcurrentRunsApplyOnce`: two goroutines call `Migrate` on one empty database; both return nil, each file/revision exists once, and `SchemaReady` is true.

- [ ] **Step 10: Run ownership and invalid-index tests red**

Run: `go test -race -count=1 ./internal/store -run 'Test(TxModeNoneIndexOwnership|MigrateRefusesInvalidIndex|MigrateIgnoresUnrelatedInvalidIndex|MigrateConcurrentRunsApplyOnce)$'`
Expected: FAIL because `txModeNoneIndexNames` and owned-index refusal do not exist; the unrelated-index and concurrent-run cases are regression controls.

- [ ] **Step 11: Implement invalid-index preflight and confirm locking**

Before applying pending files, call `txModeNoneIndexNames(files)` and query invalid indexes only where `pg_namespace.nspname='public'` and `pg_class.relname` is in that parameterized ownership list; never query or refuse every public/third-party invalid index. Order matches and refuse with each owned index and its exact concurrent drop instruction; do not auto-drop. The existing advisory lock must serialize concurrent runs and make the second revalidate/re-run as a no-op.

- [ ] **Step 12: Run invalid-index and concurrency tests green**

Run: `go test -race -count=1 ./internal/store -run 'Test(TxModeNoneIndexOwnership|MigrateRefusesInvalidIndex|MigrateIgnoresUnrelatedInvalidIndex|MigrateConcurrentRunsApplyOnce)$'`
Expected: PASS.

- [ ] **Step 13: Replace the duplicated store-test migration runner**

Change `bootstrap` to create the owner database and call `Migrate` instead of reading/sorting/applying migrations itself. In the same edit remove the now-duplicated `statements` helper and old `TestTxModeNoneStatements` from `testdb_test.go`; the production splitter and `TestMigrateTxModeNoneStatements` are already green, so no step has duplicate test definitions. Keep role creation, template cloning, three DSNs and all grants/RLS tests unchanged. After this conversion the template and its clones carry `room_broker_revisions`, while `rooms_broker` receives no grant on it. Do not change `store.Open`, `SchemaReady`, any SQL, or `atlas.sum`.

- [ ] **Step 14: Run the complete store suite against the real migrator**

Run: `go test -race -count=1 ./internal/store`
Expected: PASS against PostgreSQL testcontainers, including SC-10 append-only grants and retention/RLS tests.

- [ ] **Step 15: Write the failing migrate command contract test**

Add `TestRunBrokerMigrateEnvironment` beside `TestRunBrokerRefuses`: missing `ROOMS_MIGRATION_DATABASE_URL` names that variable; an owner DSN that cannot connect is wrapped `room-broker migrate:`; unknown-subcommand help now lists `serve, retention, migrate`; serve/retention still read their existing environment.

- [ ] **Step 16: Run the migrate command test red**

Run: `go test -race -count=1 ./internal/app -run 'TestRunBroker(Refuses|MigrateEnvironment)$'`
Expected: FAIL with `unknown subcommand "migrate"`.

- [ ] **Step 17: Implement migrate dispatch**

Route `migrate` to `store.Migrate` using only `ROOMS_MIGRATION_DATABASE_URL`, update the unknown-subcommand list and thin main package comment, and leave serve/retention environment handling unchanged.

- [ ] **Step 18: Run the migrate command test green**

Run: `go test -race -count=1 ./internal/app -run 'TestRunBroker(Refuses|MigrateEnvironment)$'`
Expected: PASS.

**Review gate:** two DSN roles stay separate, only owner migrates, the lock/revision ledger makes replicas and reruns safe, checksums/unknown versions fail closed, invalid-index recovery is limited to embedded txmode-none ownership, helper/test moves compile at every step, and Atlas validation remains CI-only.

### Task 6: Final verification and single stage commit

**Files:**
- Modify: none beyond review fixes; commit the complete Tasks 1–5 stage diff, excluding all `/tmp/opencode` artifacts.

**Interfaces:**
- Consumes: all accepted task review gates and the repository quality gate.
- Produces: one conventional English commit and recorded verification evidence.

- [ ] **Step 1: Inspect scope and workspace hygiene**

Run `git status --short`, `git diff --check`, and `git diff --stat`. Confirm no chart/factory/quickstart/CI/Maku files, migration SQL, `atlas.sum`, generated UI, credentials, or spike artifacts changed. Confirm the only spec status edit is the owner's final written-spec/architectural approval record.

- [ ] **Step 2: Run the one final full quality gate**

Run: `task check`
Expected: exit 0, including `go test -race -count=1 ./...`, offline `atlas migrate validate`, lint, vuln, CRD/chart checks, docs, scripts and UI checks. Do not run another full suite concurrently; record the actual exit code and any environment limitation instead of claiming success without evidence.

- [ ] **Step 3: Commit the accepted stage once**

Stage only reviewed paths and commit:

```bash
git commit -m "feat(broker): add install-anywhere foundations"
```

Expected: one stage commit, no attribution/co-author trailers, with the spike evidence left out of the commit except the reviewed findings in this plan.

**Review gate:** final `task check` evidence is exit 0, the commit contains only this stage, and remaining chart/quickstart/`roomctl run` work is explicitly left to separate plans.
