# Install anywhere (adoption 3b) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A platform engineer with a laptop, an API key and a GitHub PAT watches a real run in a room in under ten minutes, without cloud-native-ref: `task quickstart` → kind + Cilium + gVisor + cert-manager + CNPG + the `agent-platform` chart.

**Architecture:**

- **Broker, three opt-in modes (Tasks 2–5):**
  - an optional `systemIssuer`;
  - a `tokenreview` run-issuer mode, behind a new `authn.RunVerifier` interface;
  - a `local` human mode: one `human:local` user, `:8080` bound to loopback, Origin-checked;
  - a `room-broker migrate` subcommand that applies the embedded migrations.

  Every default is today's, so the cloud-native-ref configs load and behave unchanged.
- **One umbrella chart, `charts/agent-platform` (Tasks 6–7):** broker plus run-controller, with the factory as an optional dependency. **Every run-kind setting comes from one `runs` values block:**
  - the broker's `runs:`;
  - its run issuer's `subPattern`;
  - the controller's `namespace` and `namePrefix`.

  This closes 3a's finding I2.
- **Quickstart (Tasks 1, 8–9):** a kind cluster built by an idempotent `hack/quickstart/up.sh`, a stub model server for CI, and a `quickstart-e2e` workflow. Runs start from `kubectl apply` of a `Room` and an `AgentRun`, and are watched in the web UI.

**Tech Stack:** Go 1.27, client-go `authentication/v1` TokenReview, `embed`, pgx (as `internal/store` uses), Helm 4.3.0, yq 4.54.1, kind, Cilium 1.20.3, cert-manager v1.21.2, CloudNativePG 1.30.1, gVisor `runsc` (systrap), GitHub Actions.

**Spec:** [`docs/superpowers/specs/2026-10-18-install-anywhere-design.md`](../specs/2026-10-18-install-anywhere-design.md). The spec was corrected in the same PR as this plan:
- `roomctl` is out of scope;
- MCP is skipped only in local mode without a key;
- `atlas.sum` stays CI's check;
- database roles come from CNPG managed roles;
- runs start from `kubectl apply` manifests;
- the PAT row is added to the production-gap table.

Also read the merged 3a spec, `docs/superpowers/specs/2026-10-18-portable-run-runtime-design.md`.

**Provenance:** the code below was written against `origin/main` at `d40876a` but has **not** been compiled. Fix compile details as you go. Never change behaviour without updating this plan.

## Global Constraints

- **Defaults are today's.** With no new keys set, `config.Load` accepts the live ai-native-ref broker config, and the broker behaves exactly as before: `:8080` on all interfaces, the `oidc` human mode, JWKS run issuers, a required `systemIssuer` when one is set, and the `:8090` MCP listener.
- **Local mode is port-forward only.** `:8080` binds `127.0.0.1` in local mode, with no override, and a write whose `Origin` is neither empty nor `publicURL` is 403.
- **Run-kind agreement (3a I2).** The chart renders the broker's `runs:`, its tokenreview `subPattern`, and the run-controller's `namespace` and `namePrefix` from the single `runs` values block. No template hard-codes a prefix or namespace.
- **Images:** every image value is `<version>@sha256:<digest>`, never a bare tag, as `charts/agent-factory/templates/_helpers.tpl` enforces.
- **Run-controller wiring (3a):** metrics `:9090`, probes `:9091`; leader election in its namespace; RBAC on `agentruns`, `agentruns/status`, pods, serviceaccounts, configmaps, `ciliumnetworkpolicies`, `leases` and `events` in the runs' namespace, plus `get` on the CA Secret by name in the broker's namespace.
- **Platform rules:** a default-deny CNP on every workload pod; requests and limits; liveness and readiness probes; a restricted pod security context; no credentials in env (files or `secretKeyRef` only).
- **Never co-author commits**, and no generated-with lines. Conventional commit messages, in English.
- **SPDX** on every new Go and shell file (`hack/spdx.sh`). Comments carry *why*, never *how*.
- **Gates per task:** `go build ./... && go vet ./...`; `task test` (envtest); `task lint`; `hack/docs.sh`; and `task chart:check` once Task 6 exists. `internal/store` needs Docker. Without a daemon, run the other packages and say so.
- **Pinned versions:** kind and gVisor are pinned to exact releases at least two weeks old at execution time, recorded in `mise.toml` (kind) or `hack/quickstart/versions.env` (everything the script installs).

---

### Task 1: Spike — gVisor inside kind

**Output:** a finding recorded in `hack/quickstart/README.md`, plus the exact kind config and containerd patch that worked. The spike's scratch scripts are throwaway; only the README and `kind-config.yaml` are kept.

**Files:**
- Create: `hack/quickstart/kind-config.yaml`, `hack/quickstart/README.md`, `hack/quickstart/versions.env`

- [ ] **Step 1: Pick the versions.** Pick the newest kind release and the newest gVisor `release-YYYYMMDD.N` that are each at least two weeks old. Write them into `hack/quickstart/versions.env`:

```bash
# SPDX-License-Identifier: Apache-2.0
# Every version the quickstart installs. Bump deliberately; up.sh reads only this file.
KIND_VERSION=v0.XX.X            # the chosen release
KIND_NODE_IMAGE=kindest/node:v1.XX.X@sha256:...   # from that kind release's notes
GVISOR_RELEASE=YYYYMMDD.N
CILIUM_VERSION=1.20.3           # cloud-native-ref opentofu/config.tm.hcl
CERT_MANAGER_VERSION=v1.21.2    # cloud-native-ref infrastructure
CNPG_VERSION=1.30.1             # cloud-native-ref flux/sources/gitrepo-cloudnative-pg.yaml
```

- [ ] **Step 2: Download runsc on the host.**

```bash
. hack/quickstart/versions.env
arch=$(uname -m)   # x86_64 or aarch64
mkdir -p .quickstart/bin
for f in runsc containerd-shim-runsc-v1; do
  curl -fsSLo .quickstart/bin/$f "https://storage.googleapis.com/gvisor/releases/release/${GVISOR_RELEASE}/${arch}/$f"
  curl -fsSLo .quickstart/bin/$f.sha512 "https://storage.googleapis.com/gvisor/releases/release/${GVISOR_RELEASE}/${arch}/$f.sha512"
  (cd .quickstart/bin && sha512sum -c $f.sha512)
  chmod +x .quickstart/bin/$f
done
```

Add `.quickstart/` to `.gitignore`.

- [ ] **Step 3: Write the kind config.** kindest/node images from containerd 2.x use the plugin key `io.containerd.cri.v1.runtime`; 1.x uses `io.containerd.grpc.v1.cri`. Check which one with `docker run --rm --entrypoint containerd $KIND_NODE_IMAGE --version`, keep only the matching block, and say which in a comment.

```yaml
# hack/quickstart/kind-config.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: agent-platform
networking:
  disableDefaultCNI: true     # Cilium
  kubeProxyMode: none         # Cilium kube-proxy replacement
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: .quickstart/bin/runsc
        containerPath: /usr/local/bin/runsc
      - hostPath: .quickstart/bin/containerd-shim-runsc-v1
        containerPath: /usr/local/bin/containerd-shim-runsc-v1
containerdConfigPatches:
  - |-
    [plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runsc]
      runtime_type = "io.containerd.runsc.v1"
```

kind resolves relative `hostPath` values against the current directory, so `up.sh` runs from the repo root (Task 8).

- [ ] **Step 4: Prove it.** Create the cluster, install Cilium (Task 8's command), apply a RuntimeClass, and run one pod under it:

```bash
kind create cluster --config hack/quickstart/kind-config.yaml --image "$KIND_NODE_IMAGE"
# Cilium install as in Task 8 step 3, then:
kubectl apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: {name: gvisor}
handler: runsc
---
apiVersion: v1
kind: Pod
metadata: {name: gvisor-probe}
spec:
  runtimeClassName: gvisor
  restartPolicy: Never
  containers: [{name: c, image: busybox:1.37, command: [dmesg]}]
EOF
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/gvisor-probe --timeout=120s
kubectl logs gvisor-probe | head -3   # gVisor prints "Starting gVisor..."
```

Expected: the pod Succeeded, and its log shows gVisor's banner rather than a Linux kernel log.

- [ ] **Step 5: Record the finding.** In `hack/quickstart/README.md`, write a table of the platforms tried (Linux plus Docker is required; macOS plus Docker Desktop and Podman if available), each with its result and the exact error on failure. If a platform fails, record the `ALLOW_UNSANDBOXED=1` path. Then delete the cluster.

- [ ] **Step 6: Commit.**

```bash
git add hack/quickstart/kind-config.yaml hack/quickstart/README.md hack/quickstart/versions.env .gitignore
git commit -m "chore(quickstart): gVisor-in-kind spike: kind config, pinned versions, platform findings"
```

---

### Task 2: Broker — optional `systemIssuer`

**Files:**
- Modify: `internal/config/config.go` (`Validate`, around line 179), `internal/config/config_test.go`
- Modify: `internal/authn/systems.go`, `internal/authn/systems_test.go`
- Modify: `internal/app/jwks.go` (`authenticators`, the `sys` line)

**Interfaces:**
- Produces: `config.Config.SystemIssuer` may be all-empty (both fields empty) and is then valid. `authn.NewSystems(nil, allow)` is legal, and its `Authenticate` returns `ErrUnauthenticated`.

- [ ] **Step 1: Write the failing tests.**

```go
// internal/config/config_test.go
func TestSystemIssuerOptional(t *testing.T) {
	body := strings.Replace(valid, systemIssuerBlock, "", 1) // the fixture's systemIssuer lines
	if _, err := Load(write(t, body)); err != nil {
		t.Fatalf("no systemIssuer must load: %v", err)
	}
	half := strings.Replace(valid, systemIssuerBlock, "systemIssuer:\n  issuer: https://x\n", 1)
	if _, err := Load(write(t, half)); err == nil {
		t.Fatal("an issuer without jwksURL must be refused")
	}
}
```

Define `systemIssuerBlock` as the exact `systemIssuer:` lines of the `valid` fixture, copied from that fixture.

```go
// internal/authn/systems_test.go
func TestSystemsWithoutIssuerRefuses(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer x")
	if _, err := NewSystems(nil, nil).Authenticate(req); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("want ErrUnauthenticated, got %v", err)
	}
}
```

- [ ] **Step 2: Run them.** `go test ./internal/config/ ./internal/authn/ -run 'SystemIssuerOptional|SystemsWithoutIssuer' -v`. Expected: FAIL (systemIssuer required; nil-pointer panic in `Authenticate`).

- [ ] **Step 3: Implement.**

`config.go` `Validate`: replace `errs = append(errs, issuer("systemIssuer", c.SystemIssuer)...)` with:

```go
	// Only the factory calls with a system token: a broker without one has no systemIssuer.
	if c.SystemIssuer != (IssuerConfig{}) {
		errs = append(errs, issuer("systemIssuer", c.SystemIssuer)...)
	}
```

`systems.go` `Authenticate`, first lines:

```go
	if s.v == nil {
		return Principal{}, fmt.Errorf("%w: no system issuer is configured", ErrUnauthenticated)
	}
```

`jwks.go` `authenticators`: build `sys` only when the issuer is configured:

```go
	var sys *authn.Verifier
	if cfg.SystemIssuer.Issuer != "" {
		if sys, err = verifier(cfg.SystemIssuer.Issuer, cfg.SystemIssuer.JWKSURL, false); err != nil {
			return auth{}, err
		}
	}
```

- [ ] **Step 4: Run.** `go test ./internal/config/ ./internal/authn/ ./internal/app/`. Expected: PASS.

- [ ] **Step 5: Commit.** `git commit -m "feat(broker): systemIssuer is optional; system routes answer 401 without one"`

---

### Task 3: Broker — `tokenreview` run-issuer mode

**Files:**
- Create: `internal/authn/tokenreview.go`, `internal/authn/tokenreview_test.go`
- Modify: `internal/authn/runs.go` (the `RunIssuer.Verifier` type), `internal/config/config.go` (`IssuerConfig.Mode`, `Validate`), `internal/config/config_test.go`
- Modify: `internal/app/jwks.go` (`authenticators`: build the tokenreview verifier), `internal/app/broker.go` (pass a TokenReview client)

**Interfaces:**
- Consumes: `authn.AudienceRun` (`"room-broker"`), `authn.Claims`, `authn.ErrWrongIssuer`, `authn.ErrUnauthenticated`.
- Produces:
  - `type authn.RunVerifier interface{ Verify(ctx context.Context, raw, audience string) (*Claims, error) }`;
  - `authn.RunIssuer.Verifier RunVerifier`;
  - `func authn.NewTokenReview(c TokenReviewer, now func() time.Time) *TokenReview`, where `type TokenReviewer interface{ Create(ctx, *authv1.TokenReview, metav1.CreateOptions) (*authv1.TokenReview, error) }` (client-go's `TokenReviewInterface` satisfies it);
  - `config.IssuerConfig.Mode string` (json `mode,omitempty`), `"" | "jwks" | "tokenreview"`.

- [ ] **Step 1: Write the failing tests.**

```go
// internal/authn/tokenreview_test.go
// SPDX-License-Identifier: Apache-2.0
package authn

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fakeReviewer struct {
	calls  int
	status authv1.TokenReviewStatus
	err    error
}

func (f *fakeReviewer) Create(_ context.Context, tr *authv1.TokenReview, _ metav1.CreateOptions) (*authv1.TokenReview, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := tr.DeepCopy()
	out.Status = f.status
	return out, nil
}

// jwtWithExp is an unsigned JWT-shaped token whose payload carries exp: only the
// apiserver validates it; the verifier reads exp to bound its cache.
func jwtWithExp(exp time.Time) string {
	p := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + itoa(exp.Unix()) + `}`))
	return "e30." + p + ".sig"
}

func ok(user string, aud ...string) authv1.TokenReviewStatus {
	return authv1.TokenReviewStatus{Authenticated: true, User: authv1.UserInfo{Username: user}, Audiences: aud}
}

func TestTokenReview(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tok := jwtWithExp(now.Add(10 * time.Minute))
	for name, tc := range map[string]struct {
		status  authv1.TokenReviewStatus
		err     error
		wantSub string
		wantErr error
	}{
		"authenticated":   {status: ok("system:serviceaccount:agents:run-abcdefgh", AudienceRun), wantSub: "system:serviceaccount:agents:run-abcdefgh"},
		"wrong audience":  {status: ok("system:serviceaccount:agents:run-abcdefgh", "other"), wantErr: ErrUnauthenticated},
		"unauthenticated": {status: authv1.TokenReviewStatus{Authenticated: false}, wantErr: ErrWrongIssuer},
		"api error":       {err: errors.New("boom"), wantErr: ErrUnauthenticated},
	} {
		f := &fakeReviewer{status: tc.status, err: tc.err}
		c, err := NewTokenReview(f, func() time.Time { return now }).Verify(t.Context(), tok, AudienceRun)
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("%s: err %v, want %v", name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || c.Subject != tc.wantSub {
			t.Errorf("%s: claims %+v err %v", name, c, err)
		}
	}
}

func TestTokenReviewCache(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	f := &fakeReviewer{status: ok("system:serviceaccount:agents:run-abcdefgh", AudienceRun)}
	v := NewTokenReview(f, clock)
	tok := jwtWithExp(now.Add(10 * time.Minute))
	for range 3 {
		if _, err := v.Verify(t.Context(), tok, AudienceRun); err != nil {
			t.Fatal(err)
		}
	}
	if f.calls != 1 {
		t.Fatalf("cached: %d apiserver calls, want 1", f.calls)
	}
	now = now.Add(5*time.Minute + time.Second) // past the 5-minute cap
	_, _ = v.Verify(t.Context(), tok, AudienceRun)
	if f.calls != 2 {
		t.Fatalf("after the cap: %d calls, want 2", f.calls)
	}
	short := jwtWithExp(now.Add(30 * time.Second))
	_, _ = v.Verify(t.Context(), short, AudienceRun)
	now = now.Add(31 * time.Second) // past the token's own exp
	_, _ = v.Verify(t.Context(), short, AudienceRun)
	if f.calls != 4 {
		t.Fatalf("exp bounds the cache: %d calls, want 4", f.calls)
	}
	f.status = authv1.TokenReviewStatus{Authenticated: false}
	bad := jwtWithExp(now.Add(time.Hour))
	_, _ = v.Verify(t.Context(), bad, AudienceRun)
	_, _ = v.Verify(t.Context(), bad, AudienceRun)
	if f.calls != 6 {
		t.Fatalf("refusals are not cached: %d calls, want 6", f.calls)
	}
}
```

Use `strconv.FormatInt` for `itoa`.

In `internal/config/config_test.go`:

```go
func TestRunIssuerTokenReviewMode(t *testing.T) {
	tr := "runIssuers:\n  - mode: tokenreview\n    subPattern: '^system:serviceaccount:agents:xplane-run-([a-z2-7]{8})$'\n"
	if _, err := Load(write(t, replaceRunIssuers(valid, tr))); err != nil {
		t.Fatalf("tokenreview issuer must load: %v", err)
	}
	withURL := "runIssuers:\n  - mode: tokenreview\n    issuer: https://x\n    subPattern: '^system:serviceaccount:agents:xplane-run-([a-z2-7]{8})$'\n"
	if _, err := Load(write(t, replaceRunIssuers(valid, withURL))); err == nil {
		t.Fatal("tokenreview with an issuer URL must be refused")
	}
	bad := "runIssuers:\n  - mode: oidc\n    subPattern: '^x-([a-z2-7]{8})$'\n"
	if _, err := Load(write(t, replaceRunIssuers(valid, bad))); err == nil {
		t.Fatal("an unknown mode must be refused")
	}
}
```

`replaceRunIssuers` swaps the fixture's `runIssuers:` block. Write it next to the test, matching the fixture's exact lines.

- [ ] **Step 2: Run them.** `go test ./internal/authn/ ./internal/config/ -run 'TokenReview' -v`. Expected: FAIL to compile.

- [ ] **Step 3: Implement `internal/authn/tokenreview.go`.**

```go
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RunVerifier checks a run's token for audience: a JWKS Verifier, or the cluster's TokenReview
// where no issuer URL is reachable (kind, adoption 3b).
type RunVerifier interface {
	Verify(ctx context.Context, raw, audience string) (*Claims, error)
}

// TokenReviewer is client-go's TokenReviewInterface, narrowed for tests.
type TokenReviewer interface {
	Create(ctx context.Context, tr *authv1.TokenReview, opts metav1.CreateOptions) (*authv1.TokenReview, error)
}

// tokenReviewTTL caps a cached answer: a revoked ServiceAccount stops passing within it.
const tokenReviewTTL = 5 * time.Minute

// TokenReview asks the apiserver about each token, and caches the answers it accepts.
type TokenReview struct {
	c     TokenReviewer
	now   func() time.Time
	mu    sync.Mutex
	cache map[[32]byte]cached
}

type cached struct {
	sub   string
	until time.Time
}

// NewTokenReview reviews tokens through c.
func NewTokenReview(c TokenReviewer, now func() time.Time) *TokenReview {
	return &TokenReview{c: c, now: now, cache: map[[32]byte]cached{}}
}

// Verify returns the token's subject when the apiserver authenticates it for audience.
// An unauthenticated answer is ErrWrongIssuer, so a JWKS issuer listed after it may still try.
func (t *TokenReview) Verify(ctx context.Context, raw, audience string) (*Claims, error) {
	key := sha256.Sum256([]byte(audience + "\x00" + raw))
	now := t.now()
	t.mu.Lock()
	if c, ok := t.cache[key]; ok && now.Before(c.until) {
		t.mu.Unlock()
		return &Claims{RegisteredClaims: jwtRegistered(c.sub)}, nil
	}
	t.mu.Unlock()
	out, err := t.c.Create(ctx, &authv1.TokenReview{Spec: authv1.TokenReviewSpec{Token: raw, Audiences: []string{audience}}}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: token review: %v", ErrUnauthenticated, err)
	}
	if !out.Status.Authenticated {
		return nil, fmt.Errorf("%w: the cluster does not authenticate this token", ErrWrongIssuer)
	}
	if !slices.Contains(out.Status.Audiences, audience) {
		return nil, fmt.Errorf("%w: the token's audiences exclude %s", ErrUnauthenticated, audience)
	}
	until := now.Add(tokenReviewTTL)
	if exp, ok := unverifiedExp(raw); ok && exp.Before(until) {
		until = exp
	}
	t.mu.Lock()
	t.cache[key] = cached{sub: out.Status.User.Username, until: until}
	t.mu.Unlock()
	return &Claims{RegisteredClaims: jwtRegistered(out.Status.User.Username)}, nil
}

// unverifiedExp reads exp from a token the apiserver has just validated: it only shortens the cache.
func unverifiedExp(raw string) (time.Time, bool) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var p struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(b, &p) != nil || p.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(p.Exp, 0), true
}
```

`jwtRegistered(sub)` builds the `RegisteredClaims` value `Claims` embeds, with `Subject: sub`. Read `Claims` in `jwt.go` for the embedded type's name: `jwt.RegisteredClaims` from golang-jwt. Write `jwtRegistered` as a two-line helper in this file. `Runs.Authenticate` reads `c.ExpiresAt.Time`. Set `ExpiresAt` from the cached `until`, so a principal's expiry never outlives the cache: `jwt.NewNumericDate(until)`. Thread `until` through, and store it in the cached struct, which already holds it.

The cache grows by one entry per distinct token, about two per run. Sweep expired entries on every 256th insert (`if len(t.cache)%256 == 0 { delete where until before now }`), which keeps it bounded with no goroutine.

`runs.go`: change `Verifier *Verifier` to `Verifier RunVerifier` in `RunIssuer`. `*Verifier` already has the method, so existing callers compile unchanged.

`config.go`: add `Mode string `json:"mode,omitempty"`` to `IssuerConfig`. In `Validate`'s run-issuer loop, replace the `issuer(...)` call with:

```go
		switch is.Mode {
		case "", "jwks":
			errs = append(errs, issuer(fmt.Sprintf("runIssuers[%d]", i), is)...)
		case "tokenreview":
			if is.Issuer != "" || is.JWKSURL != "" {
				errs = append(errs, fmt.Errorf("runIssuers[%d]: mode tokenreview takes no issuer or jwksURL: the apiserver is the issuer", i))
			}
		default:
			errs = append(errs, fmt.Errorf("runIssuers[%d]: mode %q is not jwks or tokenreview", i, is.Mode))
		}
```

`jwks.go` `authenticators`: take one more parameter, `tr authn.TokenReviewer` (nil when no issuer uses the mode), and branch in the run-issuer loop:

```go
	for _, is := range cfg.RunIssuers {
		var v authn.RunVerifier
		if is.Mode == "tokenreview" {
			if tr == nil {
				return auth{}, errors.New("a tokenreview run issuer needs the in-cluster client")
			}
			v = authn.NewTokenReview(tr, time.Now)
		} else {
			jv, err := verifier(is.Issuer, is.JWKSURL, false)
			if err != nil {
				return auth{}, err
			}
			v = jv
		}
		issuers = append(issuers, authn.RunIssuer{Verifier: v, SubPattern: regexp.MustCompile(is.SubPattern)})
	}
```

`broker.go`, at the `authenticators(...)` call site: the manager's rest config already exists (`mgr`), or is built just before it. Build `kubernetes.NewForConfig(restCfg)` and pass `cs.AuthenticationV1().TokenReviews()`. If the manager is built after `authenticators`, move the clientset construction up using `ctrl.GetConfig()`, and say so in the report. Update the existing `authenticators` test callers to pass `nil`.

- [ ] **Step 4: Run.** `go test ./internal/authn/ ./internal/config/ ./internal/app/`. Expected: PASS.

- [ ] **Step 5: Commit.** `git commit -m "feat(broker): tokenreview run-issuer mode for clusters without a reachable JWKS"`

---

### Task 4: Broker — local human mode

**Files:**
- Modify: `internal/config/config.go` (`HumanConfig.Mode`, `HumanConfig.validate`, `ApplyDefaults`), `internal/config/config_test.go`
- Create: `internal/authn/local.go`, `internal/authn/local_test.go`
- Modify: `internal/app/jwks.go` (`auth.humans` becomes an interface; skip the human verifier in local mode), `internal/app/broker.go` (`humanServer` signature, `humanAddr` per mode, MCP gate), `internal/app/broker_test.go` or a new `internal/app/local_test.go`

**Interfaces:**
- Produces:
  - `config.HumanConfig.Mode string` (json `mode,omitempty`), `"" | "oidc" | "local"`;
  - `const authn.LocalPrincipal = "human:local"`;
  - `func authn.NewLocal(origin string, groups []string) *Local`, with `(*Local).Authenticate(*http.Request) (Principal, error)`;
  - `func app.humanListenAddr(cfg config.Config) string`.

- [ ] **Step 1: Write the failing tests.**

```go
// internal/authn/local_test.go
// SPDX-License-Identifier: Apache-2.0
package authn

import (
	"errors"
	"net/http/httptest"
	"testing"
)

func TestLocal(t *testing.T) {
	l := NewLocal("http://localhost:8080", []string{"admin", "member"})
	r := httptest.NewRequest("POST", "/api/rooms", nil)
	p, err := l.Authenticate(r)
	if err != nil || p.ID != LocalPrincipal || len(p.Groups) != 2 {
		t.Fatalf("no Origin (curl, roomctl): %+v %v", p, err)
	}
	r.Header.Set("Origin", "http://localhost:8080")
	if _, err := l.Authenticate(r); err != nil {
		t.Fatalf("own origin: %v", err)
	}
	r.Header.Set("Origin", "https://evil.example")
	if _, err := l.Authenticate(r); !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign origin must be ErrForbidden, got %v", err)
	}
}
```

In `internal/config/config_test.go`, add the cases below. `localConfig` is a full config with `human: {mode: local}` and the other required blocks: copy `valid` and replace its `human:` block, and set `publicURL: http://localhost:8080`.

```go
func TestHumanLocalMode(t *testing.T) {
	c, err := Load(write(t, localConfig))
	if err != nil {
		t.Fatalf("local mode must load: %v", err)
	}
	if c.Human.Groups.Admin == "" || c.Human.Groups.Member == "" {
		t.Fatal("local mode defaults the group names")
	}
	mixed := strings.Replace(localConfig, "mode: local", "mode: local\n  issuer: https://idp", 1)
	if _, err := Load(write(t, mixed)); err == nil {
		t.Fatal("local mode with OIDC fields must be refused")
	}
	if _, err := Load(write(t, strings.Replace(localConfig, "http://localhost:8080", "http://rooms.example.com", 1))); err == nil {
		t.Fatal("local mode with a non-loopback publicURL must be refused")
	}
}
```

The HTTP-level test lives in `internal/app`, beside the existing broker tests:

```go
func TestHumanListenAddr(t *testing.T) {
	if got := humanListenAddr(config.Config{}); got != ":8080" {
		t.Fatalf("oidc: %q", got)
	}
	if got := humanListenAddr(config.Config{Human: config.HumanConfig{Mode: "local"}}); got != "127.0.0.1:8080" {
		t.Fatalf("local: %q", got)
	}
}
```

- [ ] **Step 2: Run them.** `go test ./internal/authn/ ./internal/config/ ./internal/app/ -run 'Local|HumanListenAddr' -v`. Expected: FAIL to compile.

- [ ] **Step 3: Implement.**

`internal/authn/local.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"fmt"
	"net/http"

	"github.com/Smana/agent-platform/internal/envelope"
)

// LocalPrincipal is the one human of a local-mode broker (adoption 3b): whoever reaches the
// loopback listener through kubectl port-forward, which only a cluster user can open.
const LocalPrincipal = "human:local"

// Local authenticates every request as LocalPrincipal. The only check left is T9's: a browser
// page from another origin must not act through the user's port-forward.
type Local struct {
	origin string
	groups []string
}

// NewLocal accepts requests from origin (or with no Origin, as non-browser clients send).
func NewLocal(origin string, groups []string) *Local { return &Local{origin: origin, groups: groups} }

// Authenticate returns LocalPrincipal, in every group the broker knows.
func (l *Local) Authenticate(r *http.Request) (Principal, error) {
	if o := r.Header.Get("Origin"); o != "" && o != l.origin {
		return Principal{}, fmt.Errorf("%w: foreign origin", ErrForbidden)
	}
	return Principal{Kind: envelope.ActorHuman, ID: LocalPrincipal, Sub: "local", Groups: l.groups}, nil
}
```

Check the `envelope` constant name for humans (`grep -n "ActorHuman\|ActorKind" internal/envelope/envelope.go`).

`config.go`:
- Add `Mode string `json:"mode,omitempty"`` to `HumanConfig`.
- In `ApplyDefaults`, when `c.Human.Mode == "local"`, default `Groups` to `{Admin: "admin", Member: "member"}` and `Origin` to `c.PublicURL`.
- In `HumanConfig.validate`, branch first:

```go
	if h.Mode == "local" {
		var errs []error
		if h.Issuer != "" || h.JWKSURL != "" || h.ClientIDFile != "" || h.ProjectIDFile != "" || h.RoomctlClientIDFile != "" || h.Access != nil {
			errs = append(errs, errors.New("human: mode local takes no issuer, jwksURL, client or project files, or access block"))
		}
		return errs
	}
	if h.Mode != "" && h.Mode != "oidc" {
		return []error{fmt.Errorf("human: mode %q is not oidc or local", h.Mode)}
	}
```

`HumanConfig.validate` has no access to `PublicURL`, so add this check in `Config.Validate`, after `c.Human.validate()`:

```go
	if c.Human.Mode == "local" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") {
			errs = append(errs, fmt.Errorf("publicURL %q: mode local serves only a port-forward, so publicURL is http://localhost:<port>", c.PublicURL))
		}
	}
```

Check where `PublicURL` is validated today. If it is required to be https, relax that requirement for local mode only, and leave a short comment saying why.

`jwks.go`:
- Change `auth.humans` to the interface `interface{ Authenticate(*http.Request) (authn.Principal, error) }`. `humanapi.Authenticator` has this shape; use it directly if importing `humanapi` there causes no cycle.
- In local mode, skip building `hv`, and set `humans: authn.NewLocal(h.Origin, []string{h.Groups.Admin, h.Groups.Member})`.
- In oidc mode, keep today's code.

`broker.go`:
- `humanServer` takes the interface type.
- In local mode, pass constant id functions (`func() string { return "local" }`) for `WebClient`, `RoomctlClient` and `ProjectID`, because `Server.Serve` refuses nil ones.
- `humanSide` and the access check: in local mode `Access` is nil. Confirm the D7 repository check is skipped when `Access` is nil; if it is not, skip it in local mode, with a comment.

Listener and MCP gate:

```go
// humanListenAddr is :8080, on loopback only in local mode: kubectl port-forward reaches it
// inside the pod's network namespace, and nothing else can.
func humanListenAddr(cfg config.Config) string {
	if cfg.Human.Mode == "local" {
		return "127.0.0.1" + humanAddr
	}
	return humanAddr
}
```

Use it at `lc.Listen(ctx, "tcp", humanAddr)`. For MCP, wrap the `mcpLn` listen, its close lines and its `g.Go` in `if serveMCP := !(cfg.Human.Mode == "local" && key == ""); serveMCP { … }`, keeping the error-path closes correct. Log `":8090 not started: local mode without ROOMS_MCP_KEY"` once.

- [ ] **Step 4: Run.** `go test ./internal/authn/ ./internal/config/ ./internal/app/ ./internal/humanapi/`. Expected: PASS, with the existing broker tests unchanged.

- [ ] **Step 5: Commit.** `git commit -m "feat(broker): local human mode — one loopback user behind a port-forward, Origin-checked"`

---

### Task 5: `room-broker migrate`

**Files:**
- Create: `internal/store/migrations/embed.go`, `internal/store/migrate.go`, `internal/store/migrate_test.go`
- Modify: `internal/app/broker.go` (`RunBroker` dispatch), `cmd/room-broker/main.go` (doc comment)

**Interfaces:**
- Produces: `func store.Migrate(ctx context.Context, dsn string, log *slog.Logger) (applied int, err error)`, and the subcommand `room-broker migrate`, which reads `ROOMS_DATABASE_URL`.

- [ ] **Step 1: Embed the directory.** Kustomize's configMapGenerator lists files explicitly, so a Go file in this directory is ignored there.

```go
// internal/store/migrations/embed.go
// SPDX-License-Identifier: Apache-2.0

// Package migrations is the schema, embedded for room-broker migrate (adoption 3b). The same
// files feed cloud-native-ref's Atlas operator through kustomization.yaml.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
```

- [ ] **Step 2: Write the failing test.** It uses the store's testcontainers setup. Read `main_test.go` and `testdb_test.go` for the shared container, and add a fresh, empty database owned by `rooms_owner`, the role `bootstrap` creates.

```go
// internal/store/migrate_test.go
// SPDX-License-Identifier: Apache-2.0
package store

import (
	"log/slog"
	"sync"
	"testing"
)

func TestMigrate(t *testing.T) {
	dsn := emptyOwnedDB(t) // CREATE DATABASE <unique> OWNER rooms_owner; DSN as rooms_owner
	n, err := Migrate(t.Context(), dsn, slog.New(slog.DiscardHandler))
	if err != nil || n != migrationCount(t) {
		t.Fatalf("first run applied %d err %v", n, err)
	}
	if n, err := Migrate(t.Context(), dsn, slog.New(slog.DiscardHandler)); err != nil || n != 0 {
		t.Fatalf("second run must be a no-op: %d %v", n, err)
	}
	exec(t, dsn, "UPDATE room_broker_revisions SET checksum = 'x' WHERE version = (SELECT min(version) FROM room_broker_revisions)")
	if _, err := Migrate(t.Context(), dsn, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("a changed checksum must be refused")
	}
}

func TestMigrateConcurrent(t *testing.T) {
	dsn := emptyOwnedDB(t)
	var wg sync.WaitGroup
	total := make([]int, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := Migrate(t.Context(), dsn, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Error(err)
			}
			total[i] = n
		}()
	}
	wg.Wait()
	if total[0]+total[1] != migrationCount(t) {
		t.Fatalf("each file applied once: %v", total)
	}
}
```

Write `emptyOwnedDB`, `exec` and `migrationCount` as small helpers in this file, reusing `testdb_test.go`'s connection helpers (`as`, `inDB`, `run`).

- [ ] **Step 3: Run it.** `go test ./internal/store/ -run Migrate -v` (needs Docker). Expected: FAIL to compile.

- [ ] **Step 4: Implement `internal/store/migrate.go`.** Use the same pgx package the store already uses: `grep -n "pgx" internal/store/store.go`.

```go
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Smana/agent-platform/internal/store/migrations"
)

// migrateLock is the advisory lock id room-broker migrate holds: two replicas' init
// containers apply each file once.
const migrateLock = 0x726f6f6d73 // "rooms"

// Migrate applies the embedded migrations in order, recording each file's SHA-256. A file whose
// checksum changed after it was applied, or a recorded version this binary does not ship, is
// refused: the database is ahead of or different from the code. It returns how many it applied.
func Migrate(ctx context.Context, dsn string, log *slog.Logger) (int, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return 0, fmt.Errorf("migrate: connect: %w", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLock); err != nil {
		return 0, fmt.Errorf("migrate: lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrateLock) //nolint:errcheck // the session ends anyway
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS room_broker_revisions (
		version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return 0, fmt.Errorf("migrate: revisions table: %w", err)
	}
	applied := map[string]string{}
	rows, err := conn.Query(ctx, "SELECT version, checksum FROM room_broker_revisions")
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var v, c string
		if err := rows.Scan(&v, &c); err != nil {
			return 0, err
		}
		applied[v] = c
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return 0, err
	}
	sort.Strings(names)
	shipped := map[string]bool{}
	n := 0
	for _, name := range names {
		version := strings.SplitN(name, "_", 2)[0]
		shipped[version] = true
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return n, err
		}
		sum := sha256.Sum256(body)
		checksum := hex.EncodeToString(sum[:])
		if c, ok := applied[version]; ok {
			if c != checksum {
				return n, fmt.Errorf("migrate: %s changed after it was applied (recorded %s, now %s)", name, c, checksum)
			}
			continue
		}
		if err := apply(ctx, conn, string(body)); err != nil {
			return n, fmt.Errorf("migrate: %s: %w", name, err)
		}
		if _, err := conn.Exec(ctx, "INSERT INTO room_broker_revisions (version, checksum) VALUES ($1, $2)", version, checksum); err != nil {
			return n, err
		}
		log.Info("migration applied", "file", name)
		n++
	}
	for v := range applied {
		if !shipped[v] {
			return n, fmt.Errorf("migrate: the database has version %s, which this binary does not ship: it is newer than the code", v)
		}
	}
	return n, nil
}

// apply runs a file in one transaction, or statement by statement when it opts out (Atlas's
// txmode none, for CREATE INDEX CONCURRENTLY).
func apply(ctx context.Context, conn *pgx.Conn, sql string) error {
	if !strings.HasPrefix(sql, "-- atlas:txmode none") {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, sql); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		return tx.Commit(ctx)
	}
	for _, stmt := range statements(sql) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%w (a txmode-none file can leave an INVALID index: DROP it, then re-run)", err)
		}
	}
	return nil
}
```

`statements` exists in `testdb_test.go`, a test file. Move it into `migrate.go`, unexported, and make the test file use the moved copy. The bootstrap then shares one splitter with production.

In `RunBroker`, add the dispatch case, and update the error string and the package doc in `cmd/room-broker/main.go`:

```go
	case "migrate":
		dsn := getenv("ROOMS_DATABASE_URL")
		if dsn == "" {
			return errors.New("room-broker migrate: ROOMS_DATABASE_URL is not set")
		}
		n, err := store.Migrate(ctx, dsn, log)
		if err == nil {
			log.Info("schema up to date", "applied", n)
		}
		return err
```

- [ ] **Step 5: Run.** `go test ./internal/store/ ./internal/app/` (Docker), and `task migrations` (atlas validate, unchanged). Expected: PASS.

- [ ] **Step 6: Commit.** `git commit -m "feat(broker): room-broker migrate applies the embedded schema under an advisory lock"`

---

### Task 6: Chart `agent-platform` — skeleton, broker and the `runs` block

**Files:**
- Create: `charts/agent-platform/{Chart.yaml,values.yaml,values-quickstart.yaml,README.md}`, `charts/agent-platform/crds/` (copied by `task crd:gen`), `charts/agent-platform/templates/{_helpers.tpl,broker-*.yaml,agents-namespace.yaml}`, `charts/agent-platform/tests/render.sh`
- Modify: `taskfile.yaml` (`crd:gen` copies `agents.ogenki.io_rooms.yaml` and `agents.ogenki.io_agentruns.yaml` into `charts/agent-platform/crds/`; `chart:check` lints and renders both charts)

**Interfaces:**
- Produces: the values contract every later task uses:

```yaml
# charts/agent-platform/values.yaml (excerpt — the full file is the step below)
runs:                       # the ONE source of the run kind (3a I2)
  group: agents.ogenki.io
  namespace: agents
  namePrefix: run-
broker:
  image: {repository: ghcr.io/smana/room-broker, tag: ""}   # tag: <version>@sha256:<digest>
  human: {mode: oidc}       # oidc block passed through verbatim when mode is oidc
  runIssuer: {mode: tokenreview}   # or jwks with issuer/jwksURL; subPattern is derived from runs
  database:
    brokerSecret: ""        # Secret with key uri: the broker's DSN (rooms_broker)
    ownerSecret: ""         # Secret with key uri: the migrate DSN (rooms_owner); empty = no migrate
  tls: {issuerRef: {kind: ClusterIssuer, name: ""}}
runController: {enabled: true}
factory: {enabled: false}
```

- [ ] **Step 1: Write `tests/render.sh` first.** Copy the structure and helpers (`check`, `ykind`, `ynone`) from `charts/agent-factory/tests/render.sh`, then add these assertions:

```bash
digest='v0.0.0@sha256:0000'
q="$(helm template ap . -n agent-platform -f values-quickstart.yaml \
     --set broker.image.tag=$digest --set runController.image.tag=$digest)"
out="$q"
# runs block → broker config and run issuer agree
check 'namePrefix: run-' 'broker config carries runs.namePrefix'
check "subPattern: '^system:serviceaccount:agents:run-([a-z2-7]{8})\$'" 'the run issuer pattern is derived from runs'
ykind Deployment 'select(.metadata.name=="room-broker") | .spec.template.spec.initContainers[0].args == ["migrate"]' 'migrate init container'
ykind Deployment 'select(.metadata.name=="room-broker") | (.spec.template.spec.containers[0].ports | map(.containerPort) | contains([8443,8080,9090]))' 'broker ports'
ykind ClusterRoleBinding 'select(.roleRef.name=="system:auth-delegator") | .subjects[0].name == "room-broker"' 'tokenreview RBAC'
ykind CiliumNetworkPolicy 'select(.metadata.namespace=="agents") | .spec.endpointSelector == {}' 'default-deny in the runs namespace'
# production-shaped render: no migrate, no tokenreview binding
p="$(helm template ap . -n agent-system --set broker.image.tag=$digest --set runController.enabled=false \
     --set broker.runIssuer.mode=jwks --set broker.runIssuer.issuer=https://oidc --set broker.runIssuer.jwksURL=https://oidc/keys \
     --set runs.group=cloud.ogenki.io --set runs.namePrefix=xplane-run-)"
out="$p"
ynone 'select(.kind=="ClusterRoleBinding")' 'jwks mode binds no auth-delegator'
check 'xplane-run-' 'the production prefix flows through'
# a bare tag is refused unless e2e.unpinnedImages
if helm template ap . -n x -f values-quickstart.yaml --set broker.image.tag=v1 --set runController.image.tag=$digest >/dev/null 2>&1; then
  echo 'FAIL: a bare image tag must be refused'; fail=1; fi
helm template ap . -n x -f values-quickstart.yaml --set e2e.unpinnedImages=true --set broker.image.tag=e2e --set runController.image.tag=e2e \
  | grep -q 'imagePullPolicy: Never' || { echo 'FAIL: e2e.unpinnedImages renders Never'; fail=1; }
```

Run `bash charts/agent-platform/tests/render.sh`. Expected: it fails, because the chart does not exist yet.

- [ ] **Step 2: Write the chart.** The templates are listed below. Each renders only from values, and the run kind comes only from `.Values.runs`.

| Template | Content |
|---|---|
| `_helpers.tpl` | `ap.image` (same `required` and `@sha256` rule as agent-factory, except when `.Values.e2e.unpinnedImages` is true: then a bare tag is accepted and `imagePullPolicy: Never` is rendered, for images `kind load`ed by CI, which have no registry digest), `ap.runSubPattern` = `printf "^system:serviceaccount:%s:%s([a-z2-7]{8})$" .Values.runs.namespace .Values.runs.namePrefix`, labels |
| `broker-config.yaml` | ConfigMap `room-broker-config` with `config.yaml`: `publicURL` (`http://localhost:8080` in local mode), `runs: {{ toYaml .Values.runs }}`, `runIssuers: [{mode, subPattern: ap.runSubPattern, issuer?, jwksURL?}]`, `human: {mode: local}` or the oidc block from values, `tls` paths |
| `broker-deployment.yaml` | One replica by default. Init container `migrate`, rendered only when `database.ownerSecret` is set: same image, args `[migrate]`, env `ROOMS_DATABASE_URL` from `secretKeyRef {name: ownerSecret, key: uri}`. Main container env `ROOMS_CONFIG`, `ROOMS_DATABASE_URL` (secretKeyRef brokerSecret/uri), `POD_NAMESPACE` (downward API). Ports 8443, 8080, 9090. Probes on 9090 (`/healthz`, `/readyz`; check the paths in `internal/app/ops.go`). Restricted security context; requests and limits; TLS volume from the Certificate's Secret |
| `broker-service.yaml` | ClusterIP `room-broker` with ports 8443 (bridge) and 9090. **8080 is not exposed** in local mode; in oidc mode, expose 8080 as today's App claim does |
| `broker-certificate.yaml` | cert-manager `Certificate` `room-broker-tls`, dnsNames `room-broker.<ns>.svc` and `room-broker.<ns>.svc.cluster.local`, issuerRef from values |
| `broker-rbac.yaml` | SA `room-broker`. A Role in the release namespace on `rooms`, `rooms/status`, leases and events. A Role in `runs.namespace` on `agentruns` (get, list, watch, patch) — check the verbs the broker uses (`internal/suspend`, `runwatch`, `roomctrl`) and copy them from ai-native-ref's broker RBAC. A ClusterRoleBinding to `system:auth-delegator` only when `runIssuer.mode == tokenreview` |
| `broker-cnp.yaml` | A default-deny CNP on the broker pod. Ingress allows 8443 from `runs.namespace` and 9090 from host. Egress allows kube-apiserver, the database (CNPG pods by label in the release namespace) and DNS |
| `agents-namespace.yaml` | Namespace `runs.namespace` (when `createRunsNamespace: true`) and a CNP `default-deny` with `endpointSelector: {}`, empty ingress and empty egress. Each run's own CNP adds its allowances |

Write `values.yaml` with the full default set (oidc mode, jwks issuer, no migrate, `runController.enabled: true`, `factory.enabled: false`). Write `values-quickstart.yaml` with `human.mode: local`, `runIssuer.mode: tokenreview`, `database.brokerSecret: rooms-broker-dsn`, `database.ownerSecret: rooms-owner-dsn`, and `tls.issuerRef: {kind: Issuer, name: quickstart-ca}`.

`Chart.yaml`: `name: agent-platform`, with `version` and `appVersion` both `0.0.0` (release sets them), and a `dependencies` entry for `agent-factory` (`repository: file://../agent-factory`, `condition: factory.enabled`).

- [ ] **Step 3: Wire `crd:gen` and `chart:check`.** Add these lines to `taskfile.yaml`:

```yaml
      - mkdir -p charts/agent-platform/crds
      - cp config/crd/agents.ogenki.io_rooms.yaml config/crd/agents.ogenki.io_agentruns.yaml charts/agent-platform/crds/
```

In `chart:check`, add `helm dependency build charts/agent-platform`, `helm lint --strict charts/agent-platform -f charts/agent-platform/values-quickstart.yaml --set broker.image.tag=… --set runController.image.tag=…`, and `bash charts/agent-platform/tests/render.sh`. Add `charts/agent-platform/charts/` (the built dependency) to `.gitignore`.

- [ ] **Step 4: Run.** `task crd:gen && task chart:check`. Expected: exit 0, every render assertion passes.

- [ ] **Step 5: Commit.** `git commit -m "feat(chart): agent-platform umbrella chart — broker, runs block, CRDs, default-deny runs namespace"`

---

### Task 7: Chart — run-controller, and the release job

**Files:**
- Create: `charts/agent-platform/templates/run-controller-{config,deployment,rbac,cnp,runtimeclass}.yaml`
- Modify: `charts/agent-platform/values.yaml`, `values-quickstart.yaml`, `tests/render.sh`, `.github/workflows/release.yaml` (`chart-release` job)

**Interfaces:**
- Consumes: `.Values.runs` (Task 6), and runctl's `Config` keys from 3a (`internal/runctl/config.go`: `namespace`, `namePrefix`, `runtimeClassName`, `allowUnsandboxed`, `modelSecret`, `gitSecret`, `broker{url,namespace,labels,caSecret}`, `images{harness,bridge}`, `egressProfiles`).

- [ ] **Step 1: Extend `render.sh`.**

```bash
out="$q"
check "namespace: agents" 'controller namespace from runs'
check 'namePrefix: run-' 'controller prefix from runs'
check 'url: https://room-broker.agent-platform.svc.cluster.local:8443' 'controller broker URL is the FQDN (3a ruling)'
ykind Deployment 'select(.metadata.name=="run-controller") | (.spec.template.spec.containers[0].ports | map(.containerPort) | contains([9090,9091]))' 'controller ports 9090/9091'
ykind Deployment 'select(.metadata.name=="run-controller") | .spec.template.spec.containers[0].readinessProbe.httpGet.port == 9091' 'probes on 9091'
ykind Role 'select(.metadata.namespace=="agents" and .metadata.name=="run-controller") | ([.rules[].resources[]] | contains(["agentruns","agentruns/status","pods","serviceaccounts","configmaps","ciliumnetworkpolicies","leases","events"]))' 'controller RBAC in the runs namespace'
ykind Role 'select(.metadata.name=="run-controller-ca") | .rules[0].resourceNames == ["room-broker-tls"] and .rules[0].verbs == ["get"]' 'get on the CA Secret by name'
ykind RuntimeClass '.handler == "runsc"' 'RuntimeClass gvisor'
```

Run it. Expected: FAIL, because the templates are missing.

- [ ] **Step 2: Write the templates.**

| Template | Content |
|---|---|
| `run-controller-config.yaml` | ConfigMap with `config.yaml`. `namespace` and `namePrefix` come from `.Values.runs`. `broker.url` is `https://room-broker.{{ .Release.Namespace }}.svc.cluster.local:8443`. `broker.namespace` is the release namespace. `broker.labels` is the broker pod's labels. `broker.caSecret` is `room-broker-tls`. `images` comes from values (each `required`, digest-pinned). `runtimeClassName` defaults to `gvisor`; `allowUnsandboxed` defaults to false. `modelSecret` and `gitSecret` come from values |
| `run-controller-deployment.yaml` | One replica. Env `RUN_CONTROLLER_CONFIG`. Port 9090 for metrics and 9091 for probes; liveness `/healthz` and readiness `/readyz` on 9091. Restricted security context, requests and limits, SA `run-controller` |
| `run-controller-rbac.yaml` | SA. Role `run-controller` in `runs.namespace` with verbs get, list, watch, create, update, patch and delete on `agentruns`, `pods`, `serviceaccounts`, `configmaps`, `ciliumnetworkpolicies`; update and patch on `agentruns/status`; leases (coordination.k8s.io) get, create, update; events create and patch; get on the model and git Secrets by name. Role `run-controller-ca` in the release namespace with get on `secrets` resourceNames `[room-broker-tls]`. A RoleBinding for each |
| `run-controller-cnp.yaml` | A default-deny CNP on the controller pod. Egress allows kube-apiserver and DNS; ingress allows 9091 from host |
| `run-controller-runtimeclass.yaml` | `RuntimeClass gvisor`, handler `runsc`, rendered when `runController.createRuntimeClass` (default true) |

- [ ] **Step 3: Release.** In `release.yaml`'s `chart-release` job, add the same package, push and sign sequence for `charts/agent-platform`. Run `helm dependency build charts/agent-platform` before packaging. Push to `oci://ghcr.io/smana/charts/agent-platform`, read the digest, and run `cosign sign`. Keep the agent-factory steps unchanged.

- [ ] **Step 4: Run.** `task chart:check`. Expected: exit 0.

- [ ] **Step 5: Commit.** `git commit -m "feat(chart): run-controller in the agent-platform chart; release pushes and signs it"`

---

### Task 8: `task quickstart` — the kind script

**Files:**
- Create: `hack/quickstart/up.sh`, `hack/quickstart/down.sh`, `hack/quickstart/run.sh`, `hack/quickstart/manifests/{ca-issuer,cnpg-cluster,room,agentrun}.yaml.tmpl`
- Modify: `taskfile.yaml` (`quickstart`, `quickstart:down`, `quickstart:run`), `mise.toml` (kind pin from Task 1)

**Interfaces:**
- Consumes: `hack/quickstart/versions.env` and `kind-config.yaml` (Task 1); the chart (Tasks 6–7).
- Produces: env contract `MODEL_API_KEY`, `MODEL` (default `claude-sonnet-5-5`), optional `MODEL_BASE_URL`, `GITHUB_TOKEN`, `REPO`, optional `ALLOW_UNSANDBOXED=1`, optional `CHART_REF` (default: the local chart directory). It also produces the `quickstart:run TASK='…'` command.

- [ ] **Step 1: Write `up.sh`.** Every step is idempotent: create only if absent, and use `helm upgrade --install`. The script must start with `set -euo pipefail` and an SPDX header.

```bash
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# task quickstart: kind + Cilium + gVisor + cert-manager + CNPG + agent-platform. Safe to re-run.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
. hack/quickstart/versions.env
step() { printf '\n==> %s\n' "$*"; }
need() { command -v "$1" >/dev/null || { echo "missing tool: $1" >&2; exit 1; }; }
for t in docker kind helm kubectl; do need "$t"; done
for v in MODEL_API_KEY GITHUB_TOKEN REPO; do [ -n "${!v:-}" ] || { echo "set $v" >&2; exit 1; }; done
[[ "$REPO" =~ ^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$ ]] || { echo "REPO is owner/name" >&2; exit 1; }
MODEL="${MODEL:-claude-sonnet-5-5}"
NS=agent-platform

step "gVisor binaries ($GVISOR_RELEASE)"
hack/quickstart/fetch-runsc.sh   # Task 1 step 2, as a script

step "kind cluster"
kind get clusters | grep -qx agent-platform || kind create cluster --config hack/quickstart/kind-config.yaml --image "$KIND_NODE_IMAGE"

step "Cilium $CILIUM_VERSION"
helm repo add cilium https://helm.cilium.io >/dev/null 2>&1 || true
helm upgrade --install cilium cilium/cilium --version "$CILIUM_VERSION" -n kube-system --wait \
  --set kubeProxyReplacement=true --set k8sServiceHost=agent-platform-control-plane --set k8sServicePort=6443 \
  --set ipam.mode=kubernetes --set operator.replicas=1

step "cert-manager $CERT_MANAGER_VERSION"
helm repo add jetstack https://charts.jetstack.io >/dev/null 2>&1 || true
helm upgrade --install cert-manager jetstack/cert-manager --version "$CERT_MANAGER_VERSION" -n cert-manager --create-namespace --wait --set crds.enabled=true
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -n "$NS" -f hack/quickstart/manifests/ca-issuer.yaml.tmpl   # self-signed → CA → Issuer quickstart-ca

step "CloudNativePG $CNPG_VERSION"
kubectl apply --server-side -f "https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-${CNPG_VERSION%.*}/releases/cnpg-${CNPG_VERSION}.yaml"
kubectl -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=180s
hack/quickstart/db.sh "$NS"   # role passwords (generated once), Cluster rooms with managed roles, DSN Secrets

step "secrets"
kubectl create namespace agents --dry-run=client -o yaml | kubectl apply -f -
kubectl -n agents create secret generic agent-model --from-literal=api_key="$MODEL_API_KEY" --from-literal=model="$MODEL" \
  ${MODEL_BASE_URL:+--from-literal=base_url="$MODEL_BASE_URL"} --dry-run=client -o yaml | kubectl apply -f -
kubectl -n agents create secret generic agent-git --from-literal=token="$GITHUB_TOKEN" --dry-run=client -o yaml | kubectl apply -f -

step "agent-platform"
helm upgrade --install ap "${CHART_REF:-charts/agent-platform}" -n "$NS" -f charts/agent-platform/values-quickstart.yaml \
  ${ALLOW_UNSANDBOXED:+--set runController.allowUnsandboxed=true} --wait --timeout 10m

cat <<EOF

Ready. In another terminal:
  kubectl -n $NS port-forward deploy/room-broker 8080:8080
then open http://localhost:8080 and start a run:
  task quickstart:run TASK='Fix a typo in the README'
EOF
```

The chart's image tags come from `values-quickstart.yaml`, pinned by the release that ships it. For a local build, `CHART_REF` plus `--set` overrides are documented in the README; do not invent a build path here.

`fetch-runsc.sh` is the Task 1 step 2 block as a script. `db.sh`:
- generates `rooms_broker` and `rooms_retention` passwords into Secrets once (`kubectl get secret … || kubectl create secret …` with `openssl rand -hex 24`), as `kubernetes.io/basic-auth` Secrets for CNPG managed roles;
- applies `cnpg-cluster.yaml.tmpl`: a `Cluster rooms` with 1 instance, `bootstrap.initdb {database: rooms, owner: rooms_owner}`, and `managed.roles` for `rooms_broker` and `rooms_retention` (login, passwordSecret);
- waits for the cluster to be Ready;
- writes `rooms-owner-dsn` (key `uri`, copied from CNPG's `rooms-app` Secret's `uri`, which is the owner);
- writes `rooms-broker-dsn` (key `uri` = `postgresql://rooms_broker:<pw>@rooms-rw.agent-platform.svc:5432/rooms?sslmode=require`).

- [ ] **Step 2: `run.sh`.** It renders `room.yaml.tmpl` and `agentrun.yaml.tmpl` with `envsubst` and applies them.
- **Room:** `agents.ogenki.io/v1alpha1 Room`, named a fresh C2 id (`head -c5 /dev/urandom | base32 | tr A-Z a-z | tr -d =` gives 8 characters of `[a-z2-7]`), in `agent-platform`. Spec: `owner` and `driver` `human:local`, `dataClass: public`, `repository: $REPO`.
- **AgentRun:** `agents.ogenki.io/v1alpha1 AgentRun` named `run-<another C2 id>` in `agents`. Spec: `role: implementer`, `repository: $REPO`, `principal: human:local`, `dataClass: public`, `roomRef: <room id>`, `task.text: $TASK`.

It then prints `http://localhost:8080/r/<room id>`.

- [ ] **Step 3: `down.sh`** runs `kind delete cluster --name agent-platform`. Add the taskfile entries `quickstart`, `quickstart:down` and `quickstart:run` (`TASK` is required).

- [ ] **Step 4: Run it.** This needs Docker, so run it on a Linux workstation: `MODEL_API_KEY=… GITHUB_TOKEN=… REPO=you/sandbox task quickstart`, then `task quickstart:run TASK='Add a line to the README'`. Watch the room through the port-forward.

Expected: the run reaches Running, the room fills with steps, and the run ends Succeeded, with a branch `agent/<id>` pushed and a pull request opened. Record the wall-clock time from `task quickstart` to the first room event; the target is under ten minutes. Run `task quickstart` a second time: it is a no-op that exits 0. Then run `task quickstart:down`.

- [ ] **Step 5: Commit.** `git commit -m "feat(quickstart): task quickstart — kind, Cilium, gVisor, cert-manager, CNPG and the chart"`

---

### Task 9: `quickstart-e2e` in CI, with a stub model

**Files:**
- Create: `hack/quickstart/stub-model/main.go` (an OpenAI-compatible stub), `hack/quickstart/stub-model/main_test.go`, `hack/quickstart/stub-model.yaml.tmpl` (Deployment and Service in namespace `e2e`), `.github/workflows/quickstart-e2e.yaml`

**Interfaces:**
- Consumes: `up.sh` with `MODEL_BASE_URL=http://stub-model.e2e.svc.cluster.local:8080/v1`. 3a's `ModelFrom` accepts http for `.svc` hosts, and the run's CNP allows that host and port.
- Needs from the owner: a throwaway repository and a fine-grained PAT scoped to it, stored as Actions secrets `QUICKSTART_REPO` and `QUICKSTART_GITHUB_TOKEN`. **The job skips (does not fail) when the secrets are absent**, for example on fork PRs.

- [ ] **Step 1: The stub.** It is a stdlib `net/http` server on `:8080`. `POST /v1/chat/completions` returns a non-streaming completion whose single choice calls the agent's finish tool:

```go
// SPDX-License-Identifier: Apache-2.0

// Command stub-model answers every chat completion with a call to the agent's finish tool, so
// quickstart-e2e proves the whole run path without a real model or its cost.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

func completion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "stub", "object": "chat.completion", "created": time.Now().Unix(), "model": "stub",
		"choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{
			"role": "assistant", "content": "",
			"tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{
				"name": "finish", "arguments": `{"message":"quickstart-e2e: nothing to change"}`}}},
		}}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", completion)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	log.Fatal((&http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe())
}
```

**Verify the tool name.** Read the harness's agent preset (`get_default_agent` in the pinned OpenHands SDK, and `images/agent-harness/agent_run.py`) for the exact name of the finish tool, and the argument key it expects. Put both in the stub, and pin them with `main_test.go`, which asserts the response decodes and names that tool. If the SDK streams by default, also answer `stream: true` with SSE chunks.

- [ ] **Step 2: The workflow.**

```yaml
# .github/workflows/quickstart-e2e.yaml
name: quickstart-e2e
on:
  pull_request:
    paths: [charts/**, hack/quickstart/**, cmd/run-controller/**, internal/runctl/**, internal/runwatch/**, internal/app/**, internal/authn/**, internal/store/**]
  push:
    tags: ["v*"]
permissions: {contents: read}
jobs:
  e2e:
    runs-on: ubuntu-latest
    timeout-minutes: 30
    env:
      QUICKSTART_REPO: ${{ secrets.QUICKSTART_REPO }}
    steps:
      - if: env.QUICKSTART_REPO == ''
        run: echo "QUICKSTART_REPO secret absent (fork PR?): skipping" && exit 0
      - uses: actions/checkout@<sha> # pin as demo.yaml pins it
      - uses: jdx/mise-action@<sha>   # pin as demo.yaml pins it
      # Build the PR's images into kind (no registry push), then up.sh with --set overrides.
      - run: hack/quickstart/e2e.sh
        env:
          GITHUB_TOKEN: ${{ secrets.QUICKSTART_GITHUB_TOKEN }}
          REPO: ${{ secrets.QUICKSTART_REPO }}
```

Every remaining step must be guarded with `if: env.QUICKSTART_REPO != ''`, because `exit 0` only ends the one step.

`hack/quickstart/e2e.sh` does the following:
1. Builds the room-broker, room-bridge, run-controller and agent-harness images with `docker build` at tag `e2e`, then runs `kind load docker-image`.
2. Builds and loads stub-model.
3. Runs `up.sh` with `MODEL_API_KEY=stub MODEL=stub MODEL_BASE_URL=http://stub-model.e2e.svc.cluster.local:8080/v1` and `CHART_REF=charts/agent-platform`. It passes extra Helm args through `UP_HELM_ARGS`: add this env to `up.sh` and append it to the `helm upgrade` line. The args are `--set e2e.unpinnedImages=true --set broker.image.tag=e2e --set runController.image.tag=e2e --set runController.images.harness=ghcr.io/smana/agent-harness:e2e --set runController.images.bridge=ghcr.io/smana/room-bridge:e2e`. Images loaded with `kind load` have no registry digest, so they cannot be pinned by one; the chart's `e2e.unpinnedImages` exists for this path alone. The run-controller config must then also accept a bare tag for `images.*`: check `runctl.validate`, which only requires non-empty values today.
4. Applies the stub, then `run.sh` with `TASK='quickstart-e2e'`.
5. Waits up to 10 minutes for the AgentRun's `.status.phase` to be `Succeeded`, and asserts that the room has at least one event. Through a port-forward, query the broker's room API or `kubectl get room <id> -o jsonpath='{.status.lastSeq}'` > 0.
6. On failure, dumps `kubectl get agentrun,pods -A -o wide`, the controller, broker and bridge logs, and `hubble observe --verdict DROPPED` if available.

- [ ] **Step 3: Run it.** Run `hack/quickstart/e2e.sh` locally on Linux. Expected: exit 0 with the run Succeeded. Then push the branch and confirm the job passes on the PR; if the secrets are not set yet, confirm it skips.

- [ ] **Step 4: Commit.** `git commit -m "ci(quickstart): kind e2e against a stub model, from up.sh to a Succeeded run"`

---

### Task 10: Docs

**Files:**
- Create: `docs/quickstart.md`
- Modify: `README.md` (a "Try it" line under the demo link; the status line), `docs/architecture.md` (the `agent-platform` chart and the broker modes), `charts/agent-platform/README.md`, `docs/integration.md` (broker config: `human.mode`, `runIssuers[].mode`, optional `systemIssuer`)

- [ ] **Step 1: Write `docs/quickstart.md`.** Lead with the one-command path and its prerequisites: docker, kind, helm, kubectl, an API key, a fine-grained PAT on one throwaway repository. Then cover:
- the platform table from Task 1;
- `task quickstart:run`;
- watching the room;
- `task quickstart:down`;
- the **"What differs from production"** table, copied verbatim from the spec, including the PAT row: every role holds the same PAT, so the rules are instructions, not enforcement;
- a troubleshooting section: a run stuck Pending (read the run's `Ready` condition: `SecretMissing`, `CreateFailed`, `WaitingForGC`), egress drops (`hubble observe`), and `ALLOW_UNSANDBOXED`.

- [ ] **Step 2: Update the README.**
- **Status line:** state that the quickstart runs anywhere, and keep `hack/docs.sh`'s one-status rule.
- **"Try it":** add a line linking `docs/quickstart.md`.
- **Guarantee claims:** link "a budget it cannot raise" and "short-lived credentials" to the production-gap table.

- [ ] **Step 3: Run.** `hack/docs.sh`. Expected: exit 0.

- [ ] **Step 4: Commit.** `git commit -m "docs: quickstart guide, production-gap table, chart and broker-mode reference"`

---

## Done when

- [ ] `task check` exits 0, `task chart:check` included. `internal/store` runs where Docker is available.
- [ ] The live ai-native-ref broker and factory configs still load. Check with the Flux variables substituted, as for 3a.
- [ ] `task quickstart` on Linux reaches the first room event in under ten minutes, and a second run is a no-op.
- [ ] `quickstart-e2e` passes on the PR, or skips without the owner's secrets. The PR says which.
- [ ] Every run-kind value in the rendered chart traces to `.Values.runs`. `render.sh` asserts both the quickstart and the production prefix.
