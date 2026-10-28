# Install Anywhere Chart and Quickstart Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Finish install-anywhere on `feat/install-anywhere`: a factory-free `agent-platform` chart, `roomctl run` as the quickstart's one command for a run, and the review fixes the broker foundations stage carried forward.

**Architecture:** The broker still creates no `AgentRun`. `roomctl run` (local brokers only) creates a room with `POST /api/rooms`, queues the task text, sends `start_run` on the room's socket, pipes the broker-rendered `runrequest` claim from the ack into the developer's own `kubectl apply -f -`, then follows the room until the run's terminal `run_phase`. The chart loses its factory dependency and its `factory` values; the quickstart scripts move every secret off argv, stop on unproven platforms and on kubectl skew, and keep `publicURL` and the forwarded port equal. The local listener accepts its own port under all three loopback names without loosening the DNS-rebinding defence.

**Tech Stack:** Go 1.27.3 (stdlib `testing`, hand fakes, `httptest`, `coder/websocket`), Helm 4.3.0 + yq 4.54.1 render assertions, bash + shellcheck, TypeScript + vitest (`web/`), kind v0.33.0 / Cilium / CNPG / gVisor for the one end-to-end run.

**Spec:** `docs/superpowers/specs/2026-10-18-install-anywhere-design.md` (approved, amended 2026-10-27). The uncommitted copy in `/home/smana/Sources/agent-platform` is byte-identical to this worktree's committed copy (`cmp` on 2026-10-27): there is no newer amendment to reconcile.

**Inputs carried forward:** `.superpowers/sdd/2026-10-27-install-anywhere-broker-foundations-plan/progress.md` (Task 1 and Task 6 carry-forwards), its `task-1-report.md` (spike evidence), `.superpowers/sdd/2026-10-20-install-anywhere-plan/final-review.md` and `final-rereview.md`.

## Global Constraints

- Work only in `/home/smana/Sources/agent-platform-3b`, branch `feat/install-anywhere`, base `197818f`. Never push.
- Every broker mode keeps today's default. cloud-native-ref's broker config must load unchanged; do not migrate cloud-native-ref. Keep the chart's current value names (`runs`, `broker.database.brokerSecret`/`ownerSecret`, `broker.runIssuer`, `broker.roomTools`, `runController.*`).
- The broker never creates an `AgentRun`. `roomctl run` is quickstart-only and its usage says so.
- No secret on argv: no `--from-literal`, no token, key, password or DSN in any command line, `kubectl` and `yq` included. Values reach `yq` through `strenv()` of an exported variable; manifests reach `kubectl` on stdin.
- Never override `HOME` (no `HOME=…`, no `env -i`). Isolate roomctl's config with `XDG_CONFIG_HOME` (Linux) or `t.TempDir()` + `Roomctl.Dir` in Go tests.
- The broker runs on the default runtime: never a `runtimeClassName` on the broker pod (a runsc pod cannot be port-forwarded: spike outcome 3).
- Never drop sandboxing silently: `ALLOW_UNSANDBOXED` is unset or exactly `1`; `1` prints a warning and every unsandboxed run shows a room banner.
- Local mode admits `publicURL`'s port under `localhost`, `127.0.0.1` and `[::1]` only; any other Host is 403, any other non-empty Origin is 403, an absent Origin passes.
- Go: SPDX first line on new `.go` files; `ctx` first; errors wrap with `%w` and compare with `errors.Is/As`; stdlib tests, table-driven, hand fakes; `log/slog` only; no `http.DefaultClient`; consumer-owned interfaces.
- Memory: the laptop has crashed several times today. Per-task verification is targeted: `go test -race -count=1 ./internal/<pkg>/...`, `task chart:check`, `task quickstart:check`, `cd web && npx vitest run <file>`. `task check` runs **once** (Task 9), `hack/quickstart/e2e.sh` runs **once** (Task 9, after `task check` has exited). Never run either concurrently with each other, with `task test`, a Docker build or any other suite.
- No commit until Task 9: tasks leave their diff uncommitted for review. Task 9 makes one stage commit with no attribution or co-author trailer (owner rule). No destructive cleanup: move generated artifacts aside into the session scratchpad rather than deleting them; delete only temp dirs a script itself created.
- Out of scope: the Maku rename or branding, any factory dependency or factory install path, an IdP, a meter, other CNIs, managed-cluster guides, cloud-native-ref migration.

## Review Focus

- **The loopback-literal fix must not widen the rebinding boundary.** Task 2 `TestLocalLoopbackAliases` and `TestLocalLoopbackAliasesThroughServer`: `localhost`, `127.0.0.1` and `[::1]` on publicURL's port pass; `evil.example:8080`, `localhost.evil.example:8080`, `127.0.0.1:8081` and `https://127.0.0.1:8080` stay 403, on HTTP and on the WebSocket upgrade.
- **`roomctl run` stops at the step that failed and names it.** Task 4 `TestQuickstartRunStopsAtTheFailingStep`: `room`, `task`, `start_run` (with the broker's reason), `claim` (kubectl's own error passed through), `run` (a terminal phase other than `Succeeded`).
- **`roomctl run` cannot act on a production broker.** Task 4 `TestRoomctlRunIsQuickstartOnly`: an OIDC config is refused before any request; `TestRoomctlNeverSteersOrDecides` still holds.
- **No secret on argv, anywhere in the quickstart.** Task 7 `lib_test.sh` and the `check.sh` grep; the `qs_secret` contract.
- **The chart has no factory and the broker no runtime class.** Task 1 render assertions over both value sets, plus `bad` cases for `factory.*` and `broker.runtimeClassName`.
- **The unsandboxed banner comes from the run, not from a guess.** Task 5: the run controller's `Unsandboxed` condition → `runwatch.Run.Unsandboxed` → the `Running` step's payload and the state frame → the page.

---

## File map

| File | Task | Responsibility |
|---|---|---|
| `charts/agent-platform/{Chart.yaml,values.yaml,README.md}`, `templates/{_helpers.tpl,broker-config.yaml,broker-deployment.yaml}`, `tests/render.sh` | 1 | No factory; local `publicURL`; default-runtime guard |
| `taskfile.yaml`, `.github/workflows/release.yaml`, `.gitignore` | 1, 7 | No dependency build; no `quickstart:run` |
| `internal/authn/local.go` (+test) | 2 | `LoopbackOrigins`; Host/Origin sets |
| `internal/humanapi/server.go`, `internal/app/broker.go` (+tests) | 2 | `Server.Origins` |
| `docs/integration.md` | 2 | `human.mode: local` row |
| `internal/roomctl/client.go`, `internal/roomctl/doctor.go` (+tests) | 3 | `CreateRoom`, `Follow`, port agreement in `Discover` |
| `internal/roomctl/run.go` (+`run_test.go`) | 4 | `QuickstartRun` |
| `internal/app/roomctl.go`, `internal/app/roomctl_run.go` (+`roomctl_run_test.go`) | 4 | `roomctl run` wiring, kubectl exec |
| `docs/roomctl.md` | 4 | the `run` command |
| `internal/runwatch/{run.go,events.go}`, `internal/wire/browser.go`, `internal/humanapi/ws.go`, `internal/roomctl/format.go` | 5 | unsandboxed signal |
| `web/src/{conn.ts,room-state.ts,room.ts}`, `web/test/{room-state,room}.test.ts`, `internal/humanapi/ui/dist/` | 5 | banner |
| `docs/{api.md,event-envelope.md,web-ui.md}` | 5, 6 | wire and UI docs |
| `web/src/view.ts`, `web/test/fork.test.ts` | 6 | humanMode-aware CLI setup |
| `hack/quickstart/{lib.sh,lib_test.sh}` (new), `up.sh`, `db.sh`, `check.sh`, `README.md`; delete `run.sh`, `manifests/{room,agentrun}.yaml.tmpl`; `mise.toml`; `docs/quickstart.md` | 7 | the quickstart flow |
| `hack/quickstart/e2e.sh`, `.github/workflows/quickstart-e2e.yaml` | 8 | the end-to-end proof |

Dependencies: 1, 2, 3 and 5 are independent; 4 needs 3; 6 needs 4 (it prints `roomctl run`) and runs after 5 (both rebuild `ui/dist`); 7 needs 1 and 4; 8 needs 2, 4 and 7; 9 needs all. Execute sequentially in numeric order.

---

### Task 1: Chart without the factory, local `publicURL`, default-runtime guard

**Files:**
- Modify: `charts/agent-platform/Chart.yaml`, `charts/agent-platform/values.yaml`, `charts/agent-platform/templates/_helpers.tpl`, `charts/agent-platform/templates/broker-config.yaml`, `charts/agent-platform/templates/broker-deployment.yaml`, `charts/agent-platform/README.md`, `charts/agent-platform/tests/render.sh`, `taskfile.yaml` (`chart:check`), `.github/workflows/release.yaml:103-107`, `.gitignore:9-10`, `hack/quickstart/check.sh:13` and `hack/quickstart/up.sh:23-26` (only the `helm dependency build` lines)
- Move aside (gitignored build output, not deleted): `charts/agent-platform/charts/`, `charts/agent-platform/Chart.lock`

**Interfaces:**
- Consumes: nothing new.
- Produces: value `broker.publicURL` honoured in local mode (default `http://localhost:8080`, must match `^http://(localhost|127\.0\.0\.1|\[::1\]):[0-9]{1,5}$`); helm errors `factory: …` and `broker.runtimeClassName: …`. Task 7 passes `--set broker.publicURL=http://localhost:$LOCAL_PORT`.

- [ ] **Step 1: Move the stale dependency build aside**

Helm loads every chart under `charts/` whether or not `Chart.yaml` lists it, so the gitignored `agent-factory-0.0.0.tgz` would keep rendering the factory.

```bash
cd /home/smana/Sources/agent-platform-3b
mkdir -p "$SCRATCH/chart-deps" && mv charts/agent-platform/charts charts/agent-platform/Chart.lock "$SCRATCH/chart-deps/"
```

(`$SCRATCH` is the executing session's scratchpad directory.)

- [ ] **Step 2: Write the failing render assertions**

In `charts/agent-platform/tests/render.sh`: delete line 9 (`helm dependency build . >/dev/null`) and insert, right after `cd "$(dirname …)/.."`:

```bash
# No dependency: helm would render any chart left under charts/ (a stale dependency build) as a subchart.
[[ $(yq '.dependencies // [] | length' Chart.yaml) == 0 ]] || { echo "FAIL: Chart.yaml lists a dependency" >&2; exit 1; }
[[ ! -e charts ]] || { echo "FAIL: charts/agent-platform/charts/ exists: a stale dependency build helm would render; move it away" >&2; exit 1; }
```

After the quickstart block's last `ykind` (before `# production-shaped render`), add:

```bash
ynone 'select(.metadata.labels."app.kubernetes.io/name" == "agent-factory")' 'the quickstart renders no factory object'
ynone 'select(.kind == "CustomResourceDefinition")' 'templates render no CRD: crds/ holds Room and AgentRun only'
ykind Deployment 'select(.metadata.name=="room-broker") | .spec.template.spec.runtimeClassName == null' 'the broker runs on the default runtime: kubectl port-forward cannot reach a runsc pod'
```

After the production block's last `ykind`, add:

```bash
ynone 'select(.metadata.labels."app.kubernetes.io/name" == "agent-factory")' 'the production render has no factory object'
ykind Deployment 'select(.metadata.name=="room-broker") | .spec.template.spec.runtimeClassName == null' 'production: the broker runs on the default runtime'
```

Before `# a misconfiguration is a helm error`, add the forwarded-port render and the unsandboxed render:

```bash
# local publicURL follows the forwarded port (up.sh's LOCAL_PORT)
fwd="$(helm template ap . -n x -f values-quickstart.yaml "${imgs[@]}" --set broker.publicURL=http://localhost:18080)"
out="$fwd"
ykind ConfigMap 'select(.metadata.name=="room-broker-config") | .data."config.yaml" | from_yaml | .publicURL == "http://localhost:18080"' 'local publicURL takes broker.publicURL'
# ALLOW_UNSANDBOXED=1's overrides never reach the broker pod
out="$(helm template ap . -n x -f values-quickstart.yaml "${imgs[@]}" --set runController.allowUnsandboxed=true \
  --set runController.runtimeClassName= --set runController.createRuntimeClass=false)"
ykind Deployment 'select(.metadata.name=="room-broker") | .spec.template.spec.runtimeClassName == null' 'unsandboxed: the broker still has no runtimeClassName'
```

Add `bad` cases after the existing ones:

```bash
bad 'factory: this chart has no factory' --set factory.enabled=false
bad 'broker.runtimeClassName: the broker runs on the default runtime' --set broker.runtimeClassName=gvisor
bad 'broker.publicURL "https://rooms.example" in local mode' --set broker.publicURL=https://rooms.example
bad 'broker.publicURL "http://0.0.0.0:8080" in local mode' --set broker.publicURL=http://0.0.0.0:8080
```

Add to the loader section (after the `quickstart.yaml` extraction):

```bash
yq 'select(.metadata.name=="room-broker-config") | .data."config.yaml"' - <<<"$fwd" >"$tmp/forwarded.yaml"
```

and change the broker loader line to `go run ./tests/loadconfig broker "$tmp/quickstart.yaml" "$tmp/production.yaml" "$tmp/forwarded.yaml" || fail=1`.

- [ ] **Step 3: Run it and see it fail**

Run: `bash charts/agent-platform/tests/render.sh`
Expected: `FAIL: Chart.yaml lists a dependency` (exit 1).

- [ ] **Step 4: Remove the dependency and the factory surface**

`Chart.yaml`: delete the `dependencies:` block and set `description: The agent platform in one chart: the room broker and the run controller.`

`values.yaml`: delete the `factory:` block (`factory:` / `enabled: false`) and replace the two `publicURL` comment lines with:

```yaml
  # oidc mode: the https URL humans reach the broker at. local mode: http://localhost:<the port you
  # forward>, default http://localhost:8080; the broker answers that port only.
  publicURL: ""
```

`templates/_helpers.tpl`, inside `ap.validate` before its closing `{{- end -}}`:

```gotemplate
{{- if hasKey .Values "factory" -}}
{{- fail "factory: this chart has no factory: install charts/agent-factory on its own" -}}
{{- end -}}
{{- if hasKey $b "runtimeClassName" -}}
{{- fail "broker.runtimeClassName: the broker runs on the default runtime: kubectl port-forward cannot reach a runsc pod (install-anywhere spike, outcome 3)" -}}
{{- end -}}
{{- if and (eq ($b.human.mode | default "oidc") "local") $b.publicURL (not (regexMatch "^http://(localhost|127\\.0\\.0\\.1|\\[::1\\]):[0-9]{1,5}$" $b.publicURL)) -}}
{{- fail (printf "broker.publicURL %q in local mode: it is http://localhost:<the port you forward>" $b.publicURL) -}}
{{- end -}}
```

`templates/broker-config.yaml`: replace `publicURL: http://localhost:8080` with `publicURL: {{ $b.publicURL | default "http://localhost:8080" | quote }}`.

`templates/broker-deployment.yaml`: above `serviceAccountName: room-broker` add the comment `# No runtimeClassName, ever: kubectl port-forward, local mode's only way in, cannot reach a runsc pod.`

`taskfile.yaml` `chart:check`: delete `- helm dependency build charts/agent-platform`. `hack/quickstart/check.sh`: delete line 13 (`helm dependency build …`). `hack/quickstart/up.sh`: delete the `if [[ -d $CHART ]]; then … fi` dependency block (lines 23-26).

`.github/workflows/release.yaml`: replace lines 103-107 with the one comment `# The umbrella chart, same version: broker and run controller, no dependency.` (keep `helm package charts/agent-platform …`, push and sign).

`.gitignore`: delete `charts/agent-platform/charts/` and `charts/agent-platform/Chart.lock` (with no dependency, anything there is a stale artifact `git status` should show).

`README.md` (chart): first paragraph becomes "The umbrella chart: the room broker, the run controller (`runController`), the `Room` and `AgentRun` CRDs (`crds/`), and the runs namespace with its default-deny policy. It has no factory: install [agent-factory](../agent-factory/README.md) on its own." Drop the `helm dependency build` line from the example. Add rows:

| Value | Meaning |
|---|---|
| `broker.publicURL` | `oidc`: the https URL humans use. `local`: `http://localhost:<forwarded port>` (default `:8080`); the broker answers that port only, under `localhost`, `127.0.0.1` or `[::1]` |

and a caveat row: `| Broker runtime | The broker pod never takes a runtimeClassName (refused at render): kubectl port-forward cannot reach a runsc pod |`.

- [ ] **Step 5: Run the chart gate**

Run: `task chart:check`
Expected: both charts lint, `PASS` from both `render.sh`, exit 0.

- [ ] **Step 6: Run the quickstart drift check**

Run: `task quickstart:check`
Expected: `quickstart check: ok` (no other change to those scripts yet).

**Review gate:** both value sets render no factory object and no broker `runtimeClassName`; a `factory.*` value or `broker.runtimeClassName` is a named helm error; `git diff` touches no factory chart file.

---

### Task 2: Local mode admits its port under all three loopback names

**Files:**
- Modify: `internal/authn/local.go`, `internal/humanapi/server.go:105-109,205-221`, `internal/app/broker.go:331-338`, `docs/integration.md:117`
- Test: `internal/authn/local_test.go`, `internal/humanapi/local_test.go`, `internal/app/local_test.go:82-84`

**Interfaces:**
- Produces:
  - `func authn.LoopbackOrigins(publicURL string) []string`: `publicURL` first, then `http://localhost:<port>`, `http://127.0.0.1:<port>`, `http://[::1]:<port>` (each once); `publicURL` alone when it is not `http://…:<port>`.
  - `authn.NewLocal(publicURL string, groups []string) *Local` and `NewLocalWithClock` keep their signatures; the first parameter is now named `publicURL`.
  - `humanapi.Server.Origins []string` replaces `Origin string` (local mode only; nil installs no gate).

- [ ] **Step 1: Write the failing authn tests**

Append to `internal/authn/local_test.go`:

```go
func TestLoopbackOrigins(t *testing.T) {
	for _, tc := range []struct{ public string; want []string }{
		{"http://localhost:8080", []string{"http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"}},
		{"http://127.0.0.1:18080", []string{"http://127.0.0.1:18080", "http://localhost:18080", "http://[::1]:18080"}},
		{"http://LOCALHOST:8080", []string{"http://LOCALHOST:8080", "http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"}},
		{"https://rooms.example", []string{"https://rooms.example"}},
	} {
		if got := LoopbackOrigins(tc.public); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.public, got, tc.want)
		}
	}
}

// kubectl port-forward listens on 127.0.0.1 and [::1] and keeps the client's Host: the three
// loopback names on publicURL's port are the listener's own; anything else is a rebound page.
func TestLocalLoopbackAliases(t *testing.T) {
	l := NewLocal("http://localhost:8080", []string{"admin", "member"})
	for _, tc := range []struct {
		host, origin string
		want         error
	}{
		{"localhost:8080", "", nil},
		{"127.0.0.1:8080", "", nil},
		{"[::1]:8080", "", nil},
		{"LOCALHOST:8080", "", nil},
		{"127.0.0.1:8080", "http://127.0.0.1:8080", nil},
		{"[::1]:8080", "http://[::1]:8080", nil},
		{"localhost:8080", "http://127.0.0.1:8080", nil},
		{"evil.example:8080", "", ErrForbidden},
		{"localhost.evil.example:8080", "", ErrForbidden},
		{"127.0.0.1:8081", "", ErrForbidden},
		{"127.0.0.2:8080", "", ErrForbidden},
		{"127.0.0.1:8080", "https://127.0.0.1:8080", ErrForbidden},
		{"127.0.0.1:8080", "http://localhost:8081", ErrForbidden},
		{"127.0.0.1:8080", "http://evil.example", ErrForbidden},
	} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/rooms", nil)
		r.Host = tc.host
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		_, err := l.Authenticate(r)
		if (tc.want == nil) != (err == nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("host %q origin %q: %v, want %v", tc.host, tc.origin, err, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run them red**

Run: `go test -race -count=1 ./internal/authn -run 'Test(LoopbackOrigins|LocalLoopbackAliases)$'`
Expected: FAIL, `undefined: LoopbackOrigins`.

- [ ] **Step 3: Implement `LoopbackOrigins` and the two sets**

In `internal/authn/local.go` (add `slices`, `strings` imports):

```go
// LoopbackOrigins is every origin a local broker answers as itself: publicURL first, then its port
// under each loopback name. kubectl port-forward listens on 127.0.0.1 and [::1] and keeps the Host
// the client sent, so these three names on that port are the listener's own; a DNS-rebound page
// keeps a Host of its own and matches none. A publicURL that is not http://host:port gives itself.
func LoopbackOrigins(publicURL string) []string {
	out := []string{publicURL}
	u, err := url.Parse(publicURL)
	if err != nil || u.Scheme != "http" || u.Port() == "" {
		return out
	}
	for _, h := range []string{"localhost", "127.0.0.1", "[::1]"} {
		if o := "http://" + h + ":" + u.Port(); !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	return out
}
```

Replace the `Local` fields `origin, host string` with `origins, hosts []string`; in `NewLocalWithClock(publicURL string, …)`:

```go
	origins := LoopbackOrigins(publicURL)
	hosts := make([]string, 0, len(origins))
	for _, o := range origins {
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			hosts = append(hosts, strings.ToLower(u.Host))
		}
	}
	return &Local{origins: origins, hosts: hosts, groups: groups, now: now}
```

and in `Authenticate`:

```go
	if !slices.Contains(l.hosts, strings.ToLower(r.Host)) {
		return Principal{}, fmt.Errorf("%w: foreign host", ErrForbidden)
	}
	if o := r.Header.Get("Origin"); o != "" && !slices.Contains(l.origins, o) {
		return Principal{}, fmt.Errorf("%w: foreign origin", ErrForbidden)
	}
```

Update the `Local` doc comment: "…and the Host (one of publicURL's loopback names on its port: a DNS-rebound page keeps its own Host, which kubectl port-forward preserves)".

- [ ] **Step 4: Run authn green**

Run: `go test -race -count=1 ./internal/authn`
Expected: PASS (existing `TestLocal`, `TestLocalForeignHost`, `TestLocalHumanAuthenticate` unchanged and green).

- [ ] **Step 5: Write the failing server test**

In `internal/humanapi/local_test.go` change the two setups `s.Origin = local` (lines 90 and 154) to `s.Origins = authn.LoopbackOrigins(local)`, then append:

```go
// The page reached as http://127.0.0.1:8080 or http://[::1]:8080 is the same listener: it loads,
// lists and upgrades; a rebound Host still gets 403 on both.
func TestLocalLoopbackAliasesThroughServer(t *testing.T) {
	const local = "http://localhost:8080"
	e := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) {
		s.Humans = authn.NewLocal(local, []string{"admin", "member"})
		s.Groups = policy.Groups{Admin: "admin", Member: "member"}
		s.WebClient = func() string { return authn.LocalClientID }
		s.HumanMode, s.Origins = config.HumanModeLocal, authn.LoopbackOrigins(local)
	})
	for _, tc := range []struct {
		host, origin string
		want         int
	}{
		{"127.0.0.1:8080", "http://127.0.0.1:8080", http.StatusOK},
		{"[::1]:8080", "http://[::1]:8080", http.StatusOK},
		{"127.0.0.1:8080", "", http.StatusOK},
		{"evil.example:8080", "", http.StatusForbidden},
		{"127.0.0.1:8080", "http://127.0.0.1:9999", http.StatusForbidden},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.ts.URL+"/api/rooms", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = tc.host
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("host %q origin %q: %d, want %d", tc.host, tc.origin, resp.StatusCode, tc.want)
		}
	}
	addr := e.ts.Listener.Addr().String()
	dialer := &net.Dialer{}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, addr)
	}}}
	for _, tc := range []struct {
		url, origin string
		ok          bool
	}{
		{"ws://127.0.0.1:8080/v1/ws?room=" + roomID, "http://127.0.0.1:8080", true},
		{"ws://[::1]:8080/v1/ws?room=" + roomID, "http://[::1]:8080", true},
		{"ws://127.0.0.1:8080/v1/ws?room=" + roomID, "https://127.0.0.1:8080", false},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		c, resp, err := websocket.Dial(ctx, tc.url, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Origin": {tc.origin}}})
		cancel()
		if c != nil {
			_ = c.CloseNow()
		}
		if tc.ok != (err == nil) || (!tc.ok && (resp == nil || resp.StatusCode != http.StatusForbidden)) {
			t.Errorf("%s origin %s: resp %v err %v", tc.url, tc.origin, resp, err)
		}
	}
}
```

In `internal/app/local_test.go:82-84` change `s.Origin != local.PublicURL` to `!slices.Equal(s.Origins, authn.LoopbackOrigins(local.PublicURL))` and the format verb to `origins %q` with `s.Origins`; the OIDC case asserts `s.Origins == nil`.

- [ ] **Step 6: Run red**

Run: `go test -race -count=1 ./internal/humanapi ./internal/app -run 'TestLocal'`
Expected: compile FAIL, `s.Origins undefined`.

- [ ] **Step 7: Implement `Server.Origins`**

`internal/humanapi/server.go`: replace the `Origin string` field with

```go
	// Origins are the origins requests may come from, set in local mode alone (authn.LoopbackOrigins
	// of publicURL): there is no token there, so an every-route check on Origin is the only CSRF
	// gate, and it covers the public and static routes too. Nil installs no such gate; an OIDC
	// install relies on its authenticator's own Origin check, which every authenticated route calls.
	Origins []string
```

`if s.Origin != ""` → `if len(s.Origins) > 0 { return sameOrigin(s.Origins, h) }`; `sameOrigin(origins []string, next http.Handler)` refuses `o != "" && !slices.Contains(origins, o)`.

`internal/app/broker.go:333-338`: `var origins []string; if mode == config.HumanModeLocal { origins = authn.LoopbackOrigins(cfg.PublicURL) }` and `HumanMode: mode, Origins: origins,`.

`docs/integration.md:117`: replace "`publicURL` exactly `http://localhost:<port>` (or `127.0.0.1`) and the only accepted host" with "`publicURL` exactly `http://localhost:<port>` (or a loopback address); the broker answers that port under `localhost`, `127.0.0.1` and `[::1]` and refuses any other Host or Origin".

- [ ] **Step 8: Run green**

Run: `go test -race -count=1 ./internal/authn ./internal/humanapi ./internal/app`
Expected: PASS.

**Review gate:** the only new accepted values are the three loopback names on publicURL's own port, scheme `http`; every pre-existing refusal test still passes unmodified.

---

### Task 3: roomctl client: `CreateRoom`, `Follow`, and port agreement at discovery

**Files:**
- Modify: `internal/roomctl/client.go`, `internal/roomctl/doctor.go:115-117`
- Test: `internal/roomctl/roomctl_test.go`, `internal/roomctl/doctor_test.go:478`

**Interfaces:**
- Produces:
  - `func (c Client) CreateRoom(ctx context.Context, repository, dataClass string) (string, error)`: `POST /api/rooms`, `201 {"id"}` → id; 401/403 → `*Refusal` (op `create room`); other non-201 → `create room: <code>: <broker's text>`.
  - `func (c Client) Follow(ctx context.Context, room string, tail int, out io.Writer, stop func(envelope.Event) bool) error`: `Watch` that returns nil once `stop` reports true for a printed event. `Watch(ctx, room, tail, out)` becomes `Follow(ctx, room, tail, out, nil)`.
  - `Discover` refuses a local broker whose reported `url` names another port than the base URL.

- [ ] **Step 1: Write the failing tests**

Append to `internal/roomctl/roomctl_test.go` (imports: `envelope`, `wire`, `websocket`, `wsjson` as the file already uses):

```go
func TestCreateRoom(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string // the id, or a substring of the error
		ok     bool
	}{
		{"created", http.StatusCreated, `{"id":"r2om2abc"}`, "r2om2abc", true},
		{"no id", http.StatusCreated, `{}`, "no room id", false},
		{"unknown repository", http.StatusNotFound, "no such repository\n", "create room: 404: no such repository", false},
		{"refused", http.StatusForbidden, "", "create room: 403", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/rooms" || r.Header.Get("Authorization") != "" {
					http.Error(w, "unexpected", http.StatusTeapot)
					return
				}
				_ = json.NewDecoder(r.Body).Decode(&got)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := Client{URL: srv.URL, HC: srv.Client(), Token: func(context.Context) (string, error) { return "", nil }}
			id, err := c.CreateRoom(t.Context(), "owner/repo", "public")
			if tc.ok && (err != nil || id != tc.want) || !tc.ok && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("id %q err %v, want %q", id, err, tc.want)
			}
			if got["repository"] != "owner/repo" || got["dataClass"] != "public" {
				t.Fatalf("body %v", got)
			}
		})
	}
}

// Follow prints like Watch and returns nil at the first event stop accepts, leaving later ones unread.
func TestFollowStopsWhereTold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		var hello wire.ClientFrame
		if wsjson.Read(r.Context(), c, &hello) != nil {
			return
		}
		_ = wsjson.Write(r.Context(), c, wire.ServerFrame{Type: wire.FrameState, Snapshot: &wire.Snapshot{RoomID: hello.RoomID}})
		for _, p := range []struct {
			seq   int64
			phase string
		}{{1, "Running"}, {2, "Succeeded"}, {3, "Failed"}} {
			ev := envelope.Event{Seq: p.seq, RoomID: hello.RoomID, RunID: "a2b3c4d5", Type: envelope.StateChanged,
				Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"},
				Payload: envelope.StatePayload("run_phase", map[string]any{"phase": p.phase})}
			_ = wsjson.Write(r.Context(), c, wire.ServerFrame{Type: wire.FrameEvent, Event: &ev})
		}
		var f wire.ClientFrame
		_ = wsjson.Read(r.Context(), c, &f) // until roomctl closes
	}))
	defer srv.Close()
	c := Client{URL: srv.URL, HC: srv.Client(), Stream: srv.Client(), Token: func(context.Context) (string, error) { return "", nil }}
	var out strings.Builder
	err := c.Follow(t.Context(), "r2om2abc", 10, &out, func(ev envelope.Event) bool {
		return strings.Contains(string(ev.Payload), `"Succeeded"`)
	})
	if err != nil || !strings.Contains(out.String(), "#2 ") || strings.Contains(out.String(), "#3 ") {
		t.Fatalf("err %v, out:\n%s", err, out.String())
	}
}
```

Append to `internal/roomctl/doctor_test.go`:

```go
// A local broker's Host check answers publicURL's port only: discovery refuses another port up
// front, with the port-forward that fixes it, instead of letting every later call get 403.
func TestDiscoverLocalPortAgreement(t *testing.T) {
	var answer atomic.Value // the test writes it, the server goroutine reads it
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(answer.Load().(string))) }))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	for _, tc := range []struct {
		url, wantErr string
	}{
		{fmt.Sprintf("http://localhost:%d", port), ""},
		{fmt.Sprintf("http://[::1]:%d", port), ""},
		{"", ""}, // an older broker sends no url: not checked
		{"http://localhost:1", "answers only on port 1"},
		{"http://localhost", "names no port"},
	} {
		answer.Store(`{"url":"` + tc.url + `","humanMode":"local"}`)
		_, err := Discover(t.Context(), srv.Client(), srv.URL)
		if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("url %q: %v, want %q", tc.url, err, tc.wantErr)
		}
	}
}
```

(`doctor_test.go` gains the `sync/atomic` import if it lacks it.) Fix the fixture at `internal/roomctl/doctor_test.go:478`: `{"url":"` + r.Host + `"…` → `{"url":"http://` + r.Host + `"…`.

- [ ] **Step 2: Run red**

Run: `go test -race -count=1 ./internal/roomctl -run 'Test(CreateRoom|FollowStopsWhereTold|DiscoverLocalPortAgreement)$'`
Expected: compile FAIL (`c.CreateRoom undefined`, `c.Follow undefined`).

- [ ] **Step 3: Implement**

`internal/roomctl/client.go` (add `bytes` and `envelope` imports):

```go
// CreateRoom is POST /api/rooms: a new room the developer owns and drives, on repository
// (owner/name) and dataClass (public or internal). It returns the room's id.
func (c Client) CreateRoom(ctx context.Context, repository, dataClass string) (string, error) {
	h, err := c.header(ctx)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]string{"repository": repository, "dataClass": dataClass})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/api/rooms", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header = h
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HC.Do(req)
	if err != nil {
		return "", fmt.Errorf("create room: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusCreated:
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", refusal("create room", resp)
	default:
		msg, _ := httpx.ReadBody(resp.Body, 1<<10)
		return "", fmt.Errorf("create room: %d: %s", resp.StatusCode, OneLine(string(msg)))
	}
	raw, err := httpx.ReadBody(resp.Body, 1<<10)
	if err != nil {
		return "", fmt.Errorf("create room: %w", err)
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &out) != nil || out.ID == "" {
		return "", errors.New("create room: the broker's answer has no room id")
	}
	return out.ID, nil
}

// errFollowed ends a Follow whose stop accepted an event; Follow returns nil for it.
var errFollowed = errors.New("followed to the event asked for")
```

Rename the body of `Watch` to `Follow` with the extra parameter, store it in `watch{c: c, out: out, stop: stop}`, and add as the first case of its `switch` after `err := w.follow(ctx, hello)`:

```go
		case errors.Is(err, errFollowed):
			return nil
```

`Watch` becomes:

```go
// Watch prints the room's state, its last tail events, then follows it until ctx ends.
func (c Client) Watch(ctx context.Context, room string, tail int, out io.Writer) error {
	return c.Follow(ctx, room, tail, out, nil)
}
```

with `Follow`'s comment: "Follow is Watch until stop, called on each event it prints, reports true: it returns nil then. A nil stop follows until ctx ends." Add `stop func(envelope.Event) bool` to `type watch`, and in `follow` after the event line is printed:

```go
			if w.stop != nil && w.stop(*f.Event) {
				return errFollowed
			}
```

`internal/roomctl/doctor.go`, in `Discover`'s `case got.HumanMode == HumanModeLocal:` add `if err := samePort(u, got.URL); err != nil { return Config{}, err }` and:

```go
// samePort refuses a local broker reached on another port than its publicURL names: its Host
// check answers only that port, so every later call would be a bare 403. An answer with no url
// (a broker before install-anywhere) is not checked.
func samePort(base *url.URL, public string) error {
	if public == "" {
		return nil
	}
	p, err := url.Parse(public)
	if err != nil || p.Port() == "" {
		return fmt.Errorf("%s reports publicURL %q, which names no port: its config is wrong", base, public)
	}
	if p.Port() != base.Port() {
		return fmt.Errorf("%s: the broker's publicURL is %s, so it answers only on port %s: forward that port "+
			"(kubectl port-forward deploy/room-broker %s:8080) and configure --url http://localhost:%s, or reinstall with LOCAL_PORT=%s",
			base, public, p.Port(), p.Port(), p.Port(), base.Port())
	}
	return nil
}
```

- [ ] **Step 4: Run green**

Run: `go test -race -count=1 ./internal/roomctl/...`
Expected: PASS, including `TestWatchFollowsAcrossReauth` and `TestWatchStopsWhereRetryingCannotHelp` unchanged.

- [ ] **Step 5: Run the app suites that drive configure**

Run: `go test -race -count=1 ./internal/app -run 'TestRoomctl'`
Expected: PASS. If a local-mode fixture in `internal/app/roomctl_onboarding_test.go` answers a `url` on another port, change the fixture to the server's own URL; never relax `samePort`.

**Review gate:** `Watch`'s behaviour is unchanged (nil stop); `CreateRoom` sends no `Authorization` when the token is empty; the port error names the exact fix.

---

### Task 4: `roomctl run`

**Files:**
- Create: `internal/roomctl/run.go`, `internal/roomctl/run_test.go`, `internal/app/roomctl_run.go`, `internal/app/roomctl_run_test.go`
- Modify: `internal/app/roomctl.go` (usage, dispatch, `Roomctl.Apply`), `internal/app/roomctl_test.go` (`TestRoomctlNeverSteersOrDecides`), `docs/roomctl.md`

**Interfaces:**
- Consumes: Task 3 `Client.CreateRoom`, `Client.Follow`, `Discover`'s port check; existing `Client.Act`, `Explain`, `OneLine`, the `start_run` ack's `Result` (runrequest's rendered claim, `internal/humanapi/startrun.go:172`).
- Produces:

```go
// package roomctl
type QuickstartRun struct {
	Client     Client
	Repository string // owner/name
	Task       string
	Role       string // one of QuickstartRoles
	Apply      func(ctx context.Context, manifest []byte) error
	Out        io.Writer
	ActTimeout time.Duration // per act; zero is a minute
}
type RunEnd struct{ Room, Claim, Phase, Reason string }
type StepError struct {
	Step string // room, task, start_run, claim, follow, run
	Err  error
}
var QuickstartRoles = []string{"implementer", "tester", "triager"}
func (q QuickstartRun) Do(ctx context.Context) (RunEnd, error)

// package app
// Roomctl gains: Apply func(ctx context.Context, manifest []byte) error // nil: kubectl apply -f -
func kubectlApply(out io.Writer) func(ctx context.Context, manifest []byte) error
```

- CLI: `roomctl run --repo <owner/name> --task '<text>' [--role implementer|tester|triager]`. Output lines on stdout: `room <id>: <url>/r/<id>`, `task queued: seq <n>`, `run requested: <ns>/<name>`, the watch lines, `run <ns>/<name> ended <Phase>[: <reason>]`. Exit 0 only when the run ended `Succeeded`.

- [ ] **Step 1: Write the failing `QuickstartRun` tests**

`internal/roomctl/run_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/wire"
)

const testClaim = `{"apiVersion":"agents.ogenki.io/v1alpha1","kind":"AgentRun","metadata":{"name":"run-a2b3c4d5","namespace":"agents"},"spec":{"role":"implementer"}}`

// runBroker is a local broker for roomctl run: plain http, no token, one room, one run.
type runBroker struct {
	createStatus int               // 0: 201
	reject       map[string]string // act kind → rejection
	claim        string            // start_run's ack result; "" sends none
	end          string            // the run's terminal phase
	mu           sync.Mutex
	acts         []map[string]any
	created      int
}

func (b *runBroker) start(t *testing.T) Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/rooms" {
			b.mu.Lock()
			b.created++
			b.mu.Unlock()
			if b.createStatus != 0 {
				http.Error(w, "no such repository", b.createStatus)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"r2om2abc"}`))
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		var hello wire.ClientFrame
		if wsjson.Read(r.Context(), c, &hello) != nil {
			return
		}
		if hello.AfterSeq == nil || *hello.AfterSeq != math.MaxInt64 { // a follow, not an act
			_ = wsjson.Write(r.Context(), c, wire.ServerFrame{Type: wire.FrameState, Snapshot: &wire.Snapshot{RoomID: hello.RoomID}})
			for i, phase := range []string{"Running", b.end} {
				ev := envelope.Event{Seq: int64(10 + i), RoomID: hello.RoomID, RunID: "a2b3c4d5", Type: envelope.StateChanged,
					Actor:   envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"},
					Payload: envelope.StatePayload("run_phase", map[string]any{"phase": phase, "reason": "agent_finished"})}
				_ = wsjson.Write(r.Context(), c, wire.ServerFrame{Type: wire.FrameEvent, Event: &ev})
			}
			var f wire.ClientFrame
			_ = wsjson.Read(r.Context(), c, &f) // until roomctl closes
			return
		}
		for {
			var f wire.ClientFrame
			if wsjson.Read(r.Context(), c, &f) != nil {
				return
			}
			var act map[string]any
			_ = json.Unmarshal(f.Action, &act)
			b.mu.Lock()
			b.acts = append(b.acts, act)
			b.mu.Unlock()
			ack := wire.ServerFrame{Type: wire.FrameAck, ClientSeq: f.ClientSeq, Seq: 3}
			kind, _ := act["kind"].(string)
			if why := b.reject[kind]; why != "" {
				ack.Rejected, ack.Seq = why, 0
			} else if kind == "start_run" && b.claim != "" {
				ack.Result = json.RawMessage(b.claim)
			}
			_ = wsjson.Write(r.Context(), c, ack)
		}
	}))
	t.Cleanup(srv.Close)
	return Client{URL: srv.URL, HC: srv.Client(), Stream: srv.Client(), Token: func(context.Context) (string, error) { return "", nil }}
}

func TestQuickstartRunHappyPath(t *testing.T) {
	b := &runBroker{claim: testClaim, end: "Succeeded"}
	var applied []byte
	var out strings.Builder
	end, err := QuickstartRun{Client: b.start(t), Repository: "owner/repo", Task: "Fix a typo in the README", Role: "implementer",
		Apply: func(_ context.Context, m []byte) error { applied = m; return nil }, Out: &out}.Do(t.Context())
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if end != (RunEnd{Room: "r2om2abc", Claim: "agents/run-a2b3c4d5", Phase: "Succeeded", Reason: "agent_finished"}) {
		t.Fatalf("end %+v", end)
	}
	if string(applied) != testClaim {
		t.Fatalf("applied %s", applied)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	want := []map[string]any{{"kind": "message", "delivery": "queued", "text": "Fix a typo in the README"}, {"kind": "start_run", "role": "implementer"}}
	if len(b.acts) != 2 || !mapsEqual(b.acts[0], want[0]) || !mapsEqual(b.acts[1], want[1]) {
		t.Fatalf("acts %v", b.acts)
	}
	for _, line := range []string{"room r2om2abc: ", "/r/r2om2abc\n", "task queued: seq 3\n", "run requested: agents/run-a2b3c4d5\n",
		"run agents/run-a2b3c4d5 ended Succeeded: agent_finished\n"} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("output lacks %q:\n%s", line, out.String())
		}
	}
}

func mapsEqual(a, b map[string]any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func TestQuickstartRunStopsAtTheFailingStep(t *testing.T) {
	for _, tc := range []struct {
		name     string
		broker   runBroker
		apply    error
		step     string
		contains string
	}{
		{"room", runBroker{createStatus: http.StatusNotFound}, nil, "room", "404: no such repository"},
		{"task", runBroker{reject: map[string]string{"message": "sealed"}, claim: testClaim}, nil, "task", "sealed"},
		{"busy", runBroker{reject: map[string]string{"start_run": "room_busy"}, claim: testClaim}, nil, "start_run", "room_busy: a run is already running"},
		{"not permitted", runBroker{reject: map[string]string{"start_run": "not_permitted"}, claim: testClaim}, nil, "start_run", "not_permitted"},
		{"no claim", runBroker{}, nil, "start_run", "no claim to apply"},
		{"kubectl", runBroker{claim: testClaim, end: "Succeeded"}, errors.New(`error: the server doesn't have a resource type "agentruns"`), "claim", `resource type "agentruns"`},
		{"failed run", runBroker{claim: testClaim, end: "Failed"}, nil, "run", "ended Failed: agent_finished"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.broker
			_, err := QuickstartRun{Client: b.start(t), Repository: "owner/repo", Task: "x", Role: "implementer",
				Apply: func(context.Context, []byte) error { return tc.apply }, Out: io.Discard}.Do(t.Context())
			var se *StepError
			if !errors.As(err, &se) || se.Step != tc.step || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("err %v, want step %s containing %q", err, tc.step, tc.contains)
			}
		})
	}
}

func TestQuickstartRunRefusesRolesBeforeAnyRequest(t *testing.T) {
	b := &runBroker{claim: testClaim, end: "Succeeded"}
	_, err := QuickstartRun{Client: b.start(t), Repository: "owner/repo", Task: "x", Role: "reviewer",
		Apply: func(context.Context, []byte) error { return nil }, Out: io.Discard}.Do(t.Context())
	b.mu.Lock()
	defer b.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "implementer, a tester or a triager") || b.created != 0 {
		t.Fatalf("err %v, rooms created %d", err, b.created)
	}
}

// roomctl cannot import runwatch (it would pull client-go into the CLI): its copy of the
// terminal phases must match.
func TestTerminalPhasesMatchRunwatch(t *testing.T) {
	for _, p := range []string{"Pending", "Running", "Succeeded", "Failed", "BudgetExhausted", "Revoked", "Suspended"} {
		if terminalPhases[p] != runwatch.Terminal(p) {
			t.Errorf("%s: roomctl %v, runwatch %v", p, terminalPhases[p], runwatch.Terminal(p))
		}
	}
}
```

- [ ] **Step 2: Run red**

Run: `go test -race -count=1 ./internal/roomctl -run 'TestQuickstartRun|TestTerminalPhasesMatchRunwatch'`
Expected: compile FAIL, `undefined: QuickstartRun`.

- [ ] **Step 3: Implement `internal/roomctl/run.go`**

```go
// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/wire"
)

// QuickstartRoles are the roles roomctl run starts: a reviewer needs a pull request, and a new
// room names none.
var QuickstartRoles = []string{"implementer", "tester", "triager"}

// terminalPhases are runwatch.Terminal's (TestTerminalPhasesMatchRunwatch): a run_phase with one
// of them is the run's end.
var terminalPhases = map[string]bool{"Succeeded": true, "Failed": true, "BudgetExhausted": true, "Revoked": true}

// QuickstartRun is `roomctl run` (install-anywhere I7): on a local broker, one command makes the
// room, queues the task for the run's brief, asks for the run, applies the claim the broker
// rendered with the developer's own kubectl, and follows the room until the run ends. The broker
// never creates an AgentRun: the claim is runrequest's manifest, on the broker's runs seam, so the
// CLI cannot disagree with the chart about the kind, the namespace or the name prefix.
type QuickstartRun struct {
	Client     Client
	Repository string // owner/name
	Task       string
	Role       string // one of QuickstartRoles
	// Apply creates the claim from its JSON manifest: kubectl apply -f - under the developer's kubeconfig.
	Apply      func(ctx context.Context, manifest []byte) error
	Out        io.Writer
	ActTimeout time.Duration // per act; zero is a minute
}

// RunEnd is where the run ended: its room, its claim (namespace/name), and the log's phase and reason.
type RunEnd struct{ Room, Claim, Phase, Reason string }

// StepError is QuickstartRun stopping at Step (room, task, start_run, claim, follow or run),
// with the cause as the broker or kubectl gave it.
type StepError struct {
	Step string
	Err  error
}

func (e *StepError) Error() string { return e.Step + ": " + e.Err.Error() }
func (e *StepError) Unwrap() error { return e.Err }

// Do runs the steps in order and stops at the first that fails. It returns nil only when the run
// ended Succeeded.
func (q QuickstartRun) Do(ctx context.Context) (RunEnd, error) {
	if !slices.Contains(QuickstartRoles, q.Role) {
		return RunEnd{}, &StepError{"start_run", fmt.Errorf("role %q: roomctl run starts an implementer, a tester or a triager", q.Role)}
	}
	act := func(room string, action map[string]any) (wire.ServerFrame, error) {
		actx, cancel := context.WithTimeout(ctx, cmp.Or(q.ActTimeout, time.Minute))
		defer cancel()
		f, err := q.Client.Act(actx, room, action)
		if err == nil && f.Rejected != "" {
			err = fmt.Errorf("rejected: %s", Explain(f.Rejected))
		}
		return f, err
	}
	room, err := q.Client.CreateRoom(ctx, q.Repository, "public")
	if err != nil {
		return RunEnd{}, &StepError{"room", err}
	}
	end := RunEnd{Room: room}
	_, _ = fmt.Fprintf(q.Out, "room %s: %s/r/%s\n", room, q.Client.URL, room)
	// start_run carries no text: a run's task is its brief, which quotes what the room queued.
	f, err := act(room, map[string]any{"kind": "message", "delivery": "queued", "text": q.Task})
	if err != nil {
		return end, &StepError{"task", err}
	}
	_, _ = fmt.Fprintf(q.Out, "task queued: seq %d\n", f.Seq)
	if f, err = act(room, map[string]any{"kind": "start_run", "role": q.Role}); err != nil {
		return end, &StepError{"start_run", err}
	}
	var claim struct {
		Metadata struct{ Name, Namespace string } `json:"metadata"`
	}
	if json.Unmarshal(f.Result, &claim) != nil || claim.Metadata.Name == "" {
		return end, &StepError{"start_run", errors.New("the broker sent no claim to apply: roomctl run needs a broker without a factory (the quickstart's)")}
	}
	end.Claim = OneLine(claim.Metadata.Namespace + "/" + claim.Metadata.Name)
	_, _ = fmt.Fprintf(q.Out, "run requested: %s\n", end.Claim)
	if err := q.Apply(ctx, f.Result); err != nil {
		return end, &StepError{"claim", err}
	}
	stop := func(ev envelope.Event) bool {
		if ev.Type != envelope.StateChanged || ev.RunID == "" || !strings.HasSuffix(claim.Metadata.Name, ev.RunID) {
			return false
		}
		var p struct{ Kind, Phase, Reason string }
		if json.Unmarshal(ev.Payload, &p) != nil || p.Kind != "run_phase" || !terminalPhases[p.Phase] {
			return false
		}
		end.Phase, end.Reason = p.Phase, OneLine(p.Reason)
		return true
	}
	if err := q.Client.Follow(ctx, room, 50, q.Out, stop); err != nil {
		return end, &StepError{"follow", err}
	}
	if end.Phase == "" {
		return end, &StepError{"follow", fmt.Errorf("stopped before the run ended; it goes on: kubectl -n %s get agentrun %s",
			OneLine(claim.Metadata.Namespace), OneLine(claim.Metadata.Name))}
	}
	why := ""
	if end.Reason != "" {
		why = ": " + end.Reason
	}
	_, _ = fmt.Fprintf(q.Out, "run %s ended %s%s\n", end.Claim, end.Phase, why)
	if end.Phase != "Succeeded" {
		return end, &StepError{"run", fmt.Errorf("%s ended %s%s", end.Claim, end.Phase, why)}
	}
	return end, nil
}
```

- [ ] **Step 4: Run green**

Run: `go test -race -count=1 ./internal/roomctl/...`
Expected: PASS.

- [ ] **Step 5: Write the failing CLI tests**

`internal/app/roomctl_run_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Smana/agent-platform/internal/roomctl"
)

func TestRoomctlRunFlags(t *testing.T) {
	r := Roomctl{Dir: t.TempDir(), Out: &strings.Builder{}, Err: &strings.Builder{}}
	if err := roomctl.SaveJSON(filepath.Join(r.Dir, "config.json"), roomctl.Config{URL: "http://localhost:8080", HumanMode: roomctl.HumanModeLocal}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"run"},
		{"run", "--task", "x"},
		{"run", "--repo", "owner/repo"},
		{"run", "--repo", "not a repo", "--task", "x"},
		{"run", "--repo", "owner/repo", "--task", "   "},
		{"run", "--repo", "owner/repo", "--task", "x", "extra"},
	} {
		if err := r.Run(t.Context(), args); err == nil || !strings.Contains(err.Error(), "--repo owner/name --task TEXT") {
			t.Errorf("%q: %v", args, err)
		}
	}
}

// run is quickstart-only: against a production (OIDC) broker it refuses before any request.
func TestRoomctlRunIsQuickstartOnly(t *testing.T) {
	b := newRoomctlBroker(t)
	r, _ := configured(t, b)
	b.mu.Lock()
	b.query = ""
	b.mu.Unlock()
	err := r.Run(t.Context(), []string{"run", "--repo", "owner/repo", "--task", "x"})
	if err == nil || !strings.Contains(err.Error(), "quickstart-only") {
		t.Fatalf("err %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.acts) != 0 || b.query != "" {
		t.Fatalf("acts %v, request %q", b.acts, b.query)
	}
}

// A port-forward on another port than publicURL's is refused at the preflight discovery, before
// a room exists.
func TestRoomctlRunChecksTheForwardedPort(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		_, _ = w.Write([]byte(`{"url":"http://localhost:1","humanMode":"local"}`))
	}))
	defer srv.Close()
	r := Roomctl{Dir: t.TempDir(), HC: srv.Client(), Stream: srv.Client(), Out: &strings.Builder{}, Err: &strings.Builder{},
		Apply: func(context.Context, []byte) error { t.Fatal("applied"); return nil }}
	if err := roomctl.SaveJSON(filepath.Join(r.Dir, "config.json"), roomctl.Config{URL: srv.URL, HumanMode: roomctl.HumanModeLocal}); err != nil {
		t.Fatal(err)
	}
	err := r.Run(t.Context(), []string{"run", "--repo", "owner/repo", "--task", "x"})
	if err == nil || !strings.Contains(err.Error(), "answers only on port 1") || posts.Load() != 0 {
		t.Fatalf("err %v, posts %d", err, posts.Load())
	}
}

// kubectlApply runs the developer's kubectl with the manifest on stdin (never argv), and kubectl's
// own words reach the developer unchanged.
func TestKubectlApply(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$FAKE_KUBECTL_DIR/argv\"\ncat > \"$FAKE_KUBECTL_DIR/stdin\"\n" +
		"if [ -n \"$FAKE_KUBECTL_FAIL\" ]; then echo \"$FAKE_KUBECTL_FAIL\" >&2; exit 1; fi\necho 'agentrun.agents.ogenki.io/run-a2b3c4d5 created'\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_KUBECTL_DIR", dir)
	manifest := []byte(`{"kind":"AgentRun"}`)
	var out strings.Builder
	if err := kubectlApply(&out)(t.Context(), manifest); err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if string(argv) != "apply -f -\n" || string(stdin) != string(manifest) || !strings.Contains(out.String(), "run-a2b3c4d5 created") {
		t.Fatalf("argv %q stdin %q out %q", argv, stdin, out.String())
	}
	t.Setenv("FAKE_KUBECTL_FAIL", `error: the server doesn't have a resource type "agentruns"`)
	out.Reset()
	err := kubectlApply(&out)(t.Context(), manifest)
	if err == nil || !strings.Contains(out.String(), `resource type "agentruns"`) {
		t.Fatalf("err %v out %q", err, out.String())
	}
}
```

In `internal/app/roomctl_test.go` `TestRoomctlNeverSteersOrDecides`, add after the first loop:

```go
	// run starts a run only on a local broker: on this OIDC one it refuses and sends nothing.
	if err := r.Run(t.Context(), []string{"run", "--repo", "Smana/a", "--task", "x"}); err == nil || !strings.Contains(err.Error(), "quickstart-only") {
		t.Errorf("run on an OIDC broker: %v", err)
	}
```

- [ ] **Step 6: Run red**

Run: `go test -race -count=1 ./internal/app -run 'TestRoomctlRun|TestKubectlApply|TestRoomctlNeverSteersOrDecides'`
Expected: FAIL (`unknown command "run"`, `undefined: kubectlApply`, `Apply` unknown field).

- [ ] **Step 7: Wire the command**

`internal/app/roomctl.go`:
- `Roomctl` gains, after `RunCmd`: `// Apply creates a run's claim (roomctl run); nil runs kubectl apply -f - with the manifest on stdin.` `Apply func(ctx context.Context, manifest []byte) error`.
- `roomctlUsage`: after the `fork` entry add
  ```
    run --repo owner/name --task TEXT [--role implementer|tester|triager]
                                    quickstart only (a local broker): create a room, queue TEXT as the run's
                                    task, start the run, apply its claim with your kubectl, follow it to the end
  ```
- Add `"run"` to the `case "login", "token", …` list, and before the final `return r.fork(…)`: `case "run": return r.quickstartRun(ctx, cfg, c, args)` inside the existing `switch cmd`.

`internal/app/roomctl_run.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/Smana/agent-platform/internal/roomctl"
)

const runUsage = "run --repo owner/name --task TEXT [--role implementer|tester|triager]"

// runRepo is the broker's own repository form (humanapi's repositoryRE).
var runRepo = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

// quickstartRun is `roomctl run`: quickstart-only, so a broker that is not local is refused before
// anything is created; a local one is discovered again, which also proves the forwarded port.
func (r Roomctl) quickstartRun(ctx context.Context, cfg roomctl.Config, c roomctl.Client, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	repo := fs.String("repo", "", "the repository, owner/name: the one your PAT can write")
	task := fs.String("task", "", "what the run should do")
	role := fs.String("role", "implementer", "implementer, tester or triager")
	pos, err := flags(fs, args)
	if err != nil || len(pos) != 0 || !runRepo.MatchString(*repo) || strings.TrimSpace(*task) == "" {
		return fmt.Errorf("%s: %w", runUsage, errors.Join(err, errors.New("--repo owner/name --task TEXT")))
	}
	if cfg.HumanMode != roomctl.HumanModeLocal {
		return errors.New("run is quickstart-only: it needs a local broker (roomctl configure --url http://localhost:<port>); " +
			"on a platform install the factory starts runs")
	}
	if _, err := roomctl.Discover(ctx, r.HC, cfg.URL); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	hints := r.Err
	if hints == nil {
		hints = os.Stderr
	}
	apply := r.Apply
	if apply == nil {
		if _, err := exec.LookPath("kubectl"); err != nil {
			return errors.New("run: kubectl is not on PATH: roomctl run applies the run's claim with your own kubectl and kubeconfig")
		}
		apply = kubectlApply(hints)
	}
	_, err = roomctl.QuickstartRun{Client: c, Repository: *repo, Task: *task, Role: *role, Apply: apply, Out: r.Out}.Do(ctx)
	return err
}

// kubectlApply creates a claim with the developer's kubectl and kubeconfig: the manifest goes on
// stdin, never argv, and kubectl's own output and error reach out as kubectl wrote them.
func kubectlApply(out io.Writer) func(ctx context.Context, manifest []byte) error {
	return func(ctx context.Context, manifest []byte) error {
		cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(manifest), out, out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("kubectl apply -f -: %w (kubectl's message is above)", err)
		}
		return nil
	}
}
```

- [ ] **Step 8: Run green**

Run: `go test -race -count=1 ./internal/app -run 'TestRoomctl|TestKubectlApply'` then `go test -race -count=1 ./internal/roomctl/...`
Expected: PASS.

- [ ] **Step 9: Document the command**

`docs/roomctl.md`, `## Commands` table, add a row:

`| run --repo owner/name --task TEXT [--role implementer\|tester\|triager] | Quickstart only, on a local broker ([quickstart](quickstart.md)): creates a room, queues TEXT for the run's brief, starts the run, applies the claim the broker rendered with your own kubectl and kubeconfig, and follows the room until the run ends. Exit 0 only when the run ended Succeeded. It stops at the first failing step and names it: room, task, start_run (with the broker's reason), claim (kubectl's own error), run. On any other broker it refuses: the factory starts runs there |`

and in the intro sentence change "It never steers a run…" to keep that sentence, adding: "On a local install (the quickstart) `run` also starts one."

Run: `task docs`
Expected: exit 0.

**Review gate:** the claim reaches kubectl on stdin only; every step's failure names its step; an OIDC config is refused before any HTTP request.

---

### Task 5: Unsandboxed runs: the log, the state frame and a room banner

**Files:**
- Modify: `internal/runwatch/run.go` (Run, `FromUnstructured`), `internal/runwatch/events.go` (`Observe`'s Running step), `internal/wire/browser.go:36-40` (`RunView`), `internal/humanapi/ws.go:593`, `internal/roomctl/format.go` (`Line`), `web/src/conn.ts:13`, `web/src/room-state.ts`, `web/src/room.ts`, `internal/humanapi/ui/dist/` (rebuilt), `docs/api.md:417`, `docs/event-envelope.md:122`, `docs/web-ui.md`
- Test: `internal/runwatch/runwatch_test.go`, `internal/humanapi/ws_test.go`, `internal/roomctl/roomctl_test.go`, `web/test/room-state.test.ts`, `web/test/room.test.ts`

**Interfaces:**
- Consumes: the run controller's `Unsandboxed` condition (`internal/runctl/reconciler.go:113-115`, kept by `merge`), unchanged.
- Produces: `runwatch.Run.Unsandboxed bool`; `run_phase` `Running` payload field `"unsandboxed": true`; `wire.RunView.Unsandboxed bool \`json:"unsandboxed,omitempty"\``; `RoomState.unsandboxed(): string[]`; the page element `.banner-unsandboxed`; `Line` appends `unsandboxed`.

- [ ] **Step 1: Write the failing Go tests**

`internal/runwatch/runwatch_test.go` (its `claim(name, room, phase, revoked)` builder and `fakeStore` already exist):

```go
func TestFromUnstructuredUnsandboxed(t *testing.T) {
	for _, tc := range []struct {
		conds []any
		want  bool
	}{
		{[]any{map[string]any{"type": "Unsandboxed", "status": "True", "reason": "AllowUnsandboxed"}}, true},
		{[]any{map[string]any{"type": "Unsandboxed", "status": "False"}}, false},
		{[]any{map[string]any{"type": "Ready", "status": "True"}}, false},
		{nil, false},
	} {
		u := claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", "")
		if tc.conds != nil {
			if err := unstructured.SetNestedSlice(u.Object, tc.conds, "status", "conditions"); err != nil {
				t.Fatal(err)
			}
		}
		r, ok := FromUnstructured(u)
		if !ok || r.Unsandboxed != tc.want {
			t.Errorf("%v: ok %v unsandboxed %v", tc.conds, ok, r.Unsandboxed)
		}
	}
}

func TestObserveRecordsAnUnsandboxedRun(t *testing.T) {
	for _, tc := range []struct {
		unsandboxed bool
		want        string
	}{
		{true, `state_changed:{"kind":"run_phase","phase":"Running","unsandboxed":true}`},
		{false, `state_changed:{"kind":"run_phase","phase":"Running"}`},
	} {
		fs := &fakeStore{keys: map[string]bool{}}
		r, _ := FromUnstructured(claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", ""))
		r.Unsandboxed = tc.unsandboxed
		if err := (&Events{Store: fs}).Observe(t.Context(), r); err != nil {
			t.Fatal(err)
		}
		if got := payloads(fs.drafts); len(got) != 2 || got[1] != tc.want {
			t.Errorf("unsandboxed %v: %v", tc.unsandboxed, got)
		}
	}
}
```

`internal/roomctl/roomctl_test.go`:

```go
func TestLineSaysUnsandboxed(t *testing.T) {
	ev := envelope.Event{Seq: 7, Type: envelope.StateChanged, Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"},
		Payload: envelope.StatePayload("run_phase", map[string]any{"phase": "Running", "unsandboxed": true})}
	if got := Line(ev); got != "#7 system:room-broker run_phase Running unsandboxed" {
		t.Fatalf("%q", got)
	}
}
```

`internal/humanapi/ws_test.go`, beside `TestSnapshotRuns` (its `agentRun` builder and `runwatch.New()`):

```go
func TestSnapshotRunsSayUnsandboxed(t *testing.T) {
	e := setup(t)
	runs := runwatch.New()
	e.srv.Runs = runs
	u := agentRun("7f3cq2xz", roomID, "implementer")
	if err := unstructured.SetNestedSlice(u.Object, []any{map[string]any{"type": "Unsandboxed", "status": "True"}}, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	runs.Upsert(t.Context(), u)
	runs.Upsert(t.Context(), agentRun("2abcdefg", roomID, "reviewer"))
	c := dial(t, e, "dev", hello(nil, 0))
	b, _ := json.Marshal(read(t, c).Snapshot.Runs)
	if want := `[{"id":"2abcdefg","role":"reviewer","phase":"Running"},{"id":"7f3cq2xz","role":"implementer","phase":"Running","unsandboxed":true}]`; string(b) != want {
		t.Fatalf("runs %s", b)
	}
}
```

If `agentRun` sets `status` as a whole map with `phase`, `SetNestedSlice` adds `conditions` beside it; `TestSnapshotRuns` stays unchanged and still proves the field is omitted when false.

- [ ] **Step 2: Run red**

Run: `go test -race -count=1 ./internal/runwatch ./internal/roomctl ./internal/humanapi -run 'Unsandboxed|TestLineSaysUnsandboxed'`
Expected: compile FAIL (`r.Unsandboxed undefined`).

- [ ] **Step 3: Implement the Go side**

`internal/runwatch/run.go`: add to `Run` `// Unsandboxed is the run controller's Unsandboxed condition: its pod has no RuntimeClass (3a R5).` `Unsandboxed bool`; in `FromUnstructured` before `return r, true`: `r.Unsandboxed = conditionTrue(u, "Unsandboxed")`; and

```go
// conditionTrue reports whether the claim's status carries condition typ with status True.
func conditionTrue(u *unstructured.Unstructured, typ string) bool {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == typ {
			return m["status"] == "True"
		}
	}
	return false
}
```

`internal/runwatch/events.go`, the Running step:

```go
	if r.Phase == "Running" || Terminal(r.Phase) {
		running := map[string]any{"phase": "Running"}
		if r.Unsandboxed { // the room says so: the controller set the condition when it created the pod
			running["unsandboxed"] = true
		}
		if err := put(stepRunning, envelope.StateChanged, envelope.StatePayload("run_phase", running)); err != nil {
			return err
		}
	}
```

`internal/wire/browser.go` `RunView`: `Unsandboxed bool \`json:"unsandboxed,omitempty"\` // the run's pod has no RuntimeClass`. `internal/humanapi/ws.go:593`: `wire.RunView{ID: run.ID, Role: run.Role, Phase: run.Phase, Unsandboxed: run.Unsandboxed}`.

`internal/roomctl/format.go`, `case envelope.StateChanged:` becomes:

```go
	case envelope.StateChanged:
		what = []string{field("kind"), field("phase"), field("reason")}
		if field("unsandboxed") == "true" {
			what = append(what, "unsandboxed")
		}
```

- [ ] **Step 4: Run Go green**

Run: `go test -race -count=1 ./internal/runwatch/... ./internal/roomctl/... ./internal/humanapi/... ./internal/wire/...`
Expected: PASS.

- [ ] **Step 5: Write the failing UI tests**

`web/test/room-state.test.ts`:

```ts
  it("knows the runs that are not sandboxed, from the state frame and the log", () => {
    const s = new RoomState();
    s.reset({ driver: "", driverEpoch: 0, runs: [{ id: "b2b3c4d5", role: "implementer", phase: "Running", unsandboxed: true },
      { id: "c2b3c4d5", role: "tester", phase: "Running" }] }, 1);
    expect(s.unsandboxed()).toEqual(["b2b3c4d5"]);
    s.apply({ ...ev(2, "state_changed", { kind: "run_phase", phase: "Running", unsandboxed: true }), runId: "a2b3c4d5" });
    s.apply({ ...ev(3, "state_changed", { kind: "run_phase", phase: "Running" }), runId: "d2b3c4d5" });
    expect(s.unsandboxed()).toEqual(["a2b3c4d5", "b2b3c4d5"]);
  });
```

`web/test/room.test.ts`, inside `describe("the room page", …)`:

```ts
  it("says above everything that a run is not sandboxed", () => {
    const p = page();
    p.join({ ...snapshot(), runs: [{ id: "ikely2yk", role: "implementer", phase: "Running", unsandboxed: true }] }, 0);
    const banner = p.app.querySelector<HTMLElement>(".banner-unsandboxed")!;
    expect(banner.hidden).toBe(false);
    expect(banner.getAttribute("role")).toBe("alert");
    expect(banner.textContent).toContain("Not sandboxed: run ikely2yk runs without gVisor (ALLOW_UNSANDBOXED=1)");
    const q = page();
    q.join(snapshot(), 0);
    expect(q.app.querySelector<HTMLElement>(".banner-unsandboxed")!.hidden).toBe(true);
    const f = event(1, "state_changed", { kind: "run_phase", phase: "Running", unsandboxed: true });
    q.last().recv({ ...f, event: { ...f.event, runId: "a2b3c4d5" } });
    expect(q.app.querySelector<HTMLElement>(".banner-unsandboxed")!.textContent).toContain("run a2b3c4d5");
  });
```

- [ ] **Step 6: Run red**

Run: `cd web && npx vitest run test/room-state.test.ts test/room.test.ts`
Expected: FAIL (`s.unsandboxed is not a function`, no `.banner-unsandboxed`).

- [ ] **Step 7: Implement the UI**

`web/src/conn.ts:13`: `runs?: { id: string; role: string; phase: string; unsandboxed?: boolean }[];`

`web/src/room-state.ts`: extend `reset`'s `runs` element type with `unsandboxed?: boolean`; add the field and accessor:

```ts
  // Runs the controller placed without gVisor (their Unsandboxed condition): from the state
  // frame's runs and from a Running run_phase that says so. Never cleared: the page keeps saying it.
  private unsandboxed_ = new Set<string>();
  unsandboxed(): string[] { return [...this.unsandboxed_].sort(); }
```

In `reset`, inside `if (s.runs) {`: `for (const r of s.runs) if (r.unsandboxed) this.unsandboxed_.add(r.id);`. In `apply`'s `case "state_changed":`: `if (p.kind === "run_phase" && p.unsandboxed === true && ev.runId) this.unsandboxed_.add(ev.runId);`.

`web/src/room.ts`, after the reconnect `banner` element:

```ts
  // A run without gVisor (ALLOW_UNSANDBOXED=1): said above everything, for as long as the page is open.
  const sandbox = document.createElement("div");
  sandbox.className = "banner banner-unsandboxed";
  sandbox.setAttribute("role", "alert");
  sandbox.hidden = true;
```

`app.replaceChildren(header, sandbox, banner, needsPanel, …)`, and next to `showBanner`:

```ts
  const showSandbox = () => {
    const ids = state.unsandboxed();
    sandbox.hidden = ids.length === 0;
    sandbox.textContent = sandbox.hidden ? "" : `Not sandboxed: run ${ids.join(", ")} runs without gVisor (ALLOW_UNSANDBOXED=1). ` +
      "Its code runs as a plain container that holds your model key and GitHub token.";
  };
```

Call `showSandbox()` immediately before each of the five `refreshStrip()` calls at `room.ts:141,200,229,291,297` (they follow every `state.reset` and `state.apply`).

- [ ] **Step 8: Run UI green and rebuild the bundle**

Run: `cd web && npx tsc --noEmit && npx vitest run test/room-state.test.ts test/room.test.ts`
Expected: PASS.
Run: `task ui:build && git add internal/humanapi/ui/dist && task ui:check`
Expected: exit 0. `ui:check` compares the build with the git index (`git diff --exit-code`, no untracked file), so the rebuilt bundle is staged, not committed.

- [ ] **Step 9: Document**

- `docs/api.md:417`: `runs` becomes "`runs` (`[{id, role, phase, unsandboxed}]`; `unsandboxed` is true for a run whose pod has no RuntimeClass)".
- `docs/event-envelope.md:122`: "`phase`, and when it ended a `reason`; the `Running` step carries `unsandboxed: true` for a run the controller placed without a RuntimeClass (`ALLOW_UNSANDBOXED=1`)".
- `docs/web-ui.md`, `## The room page`: add "A run without gVisor (`ALLOW_UNSANDBOXED=1`) puts a `Not sandboxed` banner above the page for as long as it is open."

Run: `task docs`
Expected: exit 0.

**Review gate:** the banner comes only from the controller's condition; no broker config field claims sandboxing for a whole install; the `Running` step stays idempotent (same scope and step number).

---

### Task 6: The CLI setup view knows a local broker

**Files:**
- Modify: `web/src/view.ts:254-260`, `internal/humanapi/ui/dist/` (rebuilt), `docs/web-ui.md:55-56`
- Test: `web/test/fork.test.ts` (`describe("cliSetup")`)

**Interfaces:**
- Consumes: `GET /api/roomctl`'s `humanMode` (`internal/humanapi/rooms.go:167`); Task 4's command line.
- Produces: in local mode, the block `install…\nroomctl configure --url <url>\nroomctl run --repo <owner/name> --task '<what the run should do>'`, no issuer, ids or login.

- [ ] **Step 1: Write the failing test**

In `web/test/fork.test.ts`, `describe("cliSetup", …)`:

```ts
  it("a local broker: configure by URL alone, no login, then roomctl run", async () => {
    const { text } = await open({ url: "http://localhost:8080", issuer: "", clientID: "local", projectID: "local", version: "dev", humanMode: "local" });
    const lines = text.split("\n");
    expect(lines.slice(-2)).toEqual([
      "roomctl configure --url http://localhost:8080",
      "roomctl run --repo <owner/name> --task '<what the run should do>'",
    ]);
    expect(text).not.toContain("--client-id");
    expect(text).not.toContain("roomctl login");
  });
```

- [ ] **Step 2: Run red**

Run: `cd web && npx vitest run test/fork.test.ts`
Expected: FAIL (the text ends with `roomctl login`).

- [ ] **Step 3: Implement**

`web/src/view.ts`: widen the cast with `humanMode?: string` and replace the `out.textContent = …` expression:

```ts
      out.textContent = !r.ok
        ? "roomctl is not set up on this broker: it has no roomctl client."
        : c.humanMode === "local"
          ? `${installRoomctl(c.version)}\nroomctl configure --url ${c.url}\nroomctl run --repo <owner/name> --task '<what the run should do>'`
          : c.clientID
            ? `${installRoomctl(c.version)}\nroomctl configure --url ${c.url} --issuer ${c.issuer} --client-id ${c.clientID} --project-id ${c.projectID}\nroomctl login`
            : "roomctl is not set up on this broker: it has no roomctl client.";
```

- [ ] **Step 4: Run green and rebuild**

Run: `cd web && npx tsc --noEmit && npx vitest run test/fork.test.ts`
Expected: PASS (all four `cliSetup` cases).
Run: `task ui:build && git add internal/humanapi/ui/dist && task ui:check`
Expected: exit 0 (the rebuilt bundle staged, not committed).

- [ ] **Step 5: Document**

`docs/web-ui.md:55-56`: "A `CLI setup` fold under the list prints the one block that installs roomctl and configures it for this broker ([roomctl](roomctl.md)); on a local broker it configures by URL alone and shows `roomctl run` instead of a login."

Run: `task docs`
Expected: exit 0.

---

### Task 7: The quickstart flow: `lib.sh`, `up.sh`, `db.sh`, `check.sh`, docs

**Files:**
- Create: `hack/quickstart/lib.sh`, `hack/quickstart/lib_test.sh`
- Modify: `hack/quickstart/up.sh`, `hack/quickstart/db.sh`, `hack/quickstart/check.sh`, `hack/quickstart/README.md`, `taskfile.yaml` (`quickstart`, drop `quickstart:run`), `mise.toml`, `docs/quickstart.md`
- Delete (git rm): `hack/quickstart/run.sh`, `hack/quickstart/manifests/room.yaml.tmpl`, `hack/quickstart/manifests/agentrun.yaml.tmpl`

**Interfaces:**
- Consumes: Task 1 `broker.publicURL`; Task 4 `roomctl run` and its usage line `  run --repo owner/name`.
- Produces (sourced by `up.sh`, `db.sh`, `check.sh`, `e2e.sh`):

```bash
qs_platform_ok OS           # 0 for Linux; else the reason on stderr, 1
qs_node_minor IMAGE         # kindest/node:v1.37.0@sha256:… → 37
qs_skew_ok CLIENT SERVER    # minors, "37+" accepted; |client-server| <= 1, else reason, 1
qs_unsandboxed VALUE        # "" → 0, "1" → 1, anything else → reason, 1
qs_port_ok PORT             # 1024..65535
qs_secret NS NAME TYPE KEY=VAR...   # a Secret manifest on stdout, each value via yq strenv(VAR)
qs_fetch_roomctl VERSION DEST       # release roomctl-linux-<arch>, sha256-checked, installed 0755
```

- [ ] **Step 1: Write the failing shell tests**

`hack/quickstart/lib_test.sh`:

```bash
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Unit tests of lib.sh's pure functions; run by check.sh (task quickstart:check). No cluster, no docker.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
# shellcheck source=hack/quickstart/versions.env
. hack/quickstart/versions.env
# shellcheck source=hack/quickstart/lib.sh
. hack/quickstart/lib.sh
fail=0
ok() { "$@" 2>/dev/null || { echo "FAIL: expected success: $*" >&2; fail=1; }; }
no() { if "$@" 2>/dev/null; then echo "FAIL: expected failure: $*" >&2; fail=1; fi; }
eq() { [[ $1 == "$2" ]] || { echo "FAIL: $3: got '$1', want '$2'" >&2; fail=1; }; }

ok qs_platform_ok Linux
no qs_platform_ok Darwin
no qs_platform_ok MINGW64_NT-10.0
[[ $(qs_platform_ok Darwin 2>&1) == *"Linux with Docker only"* ]] || { echo "FAIL: the platform refusal names the reason" >&2; fail=1; }

eq "$(qs_node_minor "$KIND_NODE_IMAGE")" 37 "node minor of KIND_NODE_IMAGE"
eq "$(qs_node_minor kindest/node:v1.36.2)" 36 "node minor without a digest"
ok qs_skew_ok 37 37; ok qs_skew_ok 36 37; ok qs_skew_ok 38 37; ok qs_skew_ok "37+" 37
no qs_skew_ok 35 37; no qs_skew_ok 29 37; no qs_skew_ok "" 37
[[ $(qs_skew_ok 29 37 2>&1) == *"kubectl 1.29 cannot drive a 1.37 cluster"* ]] || { echo "FAIL: the skew refusal names both versions" >&2; fail=1; }

eq "$(qs_unsandboxed "")" 0 "unset ALLOW_UNSANDBOXED"
eq "$(qs_unsandboxed 1)" 1 "ALLOW_UNSANDBOXED=1"
no qs_unsandboxed true; no qs_unsandboxed 0; no qs_unsandboxed yes

ok qs_port_ok 8080; ok qs_port_ok 18080; no qs_port_ok 80; no qs_port_ok 70000; no qs_port_ok 8o80

# qs_secret: the value comes from the environment, survives quoting, and is never an argument
export QS_TEST_KEY=$'s3cr"et $HOME `id`\nline2'
m=$(qs_secret agents agent-model Opaque api_key=QS_TEST_KEY)
eq "$(yq '.stringData.api_key' <<<"$m")" "$QS_TEST_KEY" "secret value round-trips"
eq "$(yq '.metadata.namespace + "/" + .metadata.name + " " + .type' <<<"$m")" "agents/agent-model Opaque" "secret metadata"
no qs_secret agents x Opaque 'api key=QS_TEST_KEY'
no qs_secret agents x Opaque api_key=lowercase_var
unset QS_UNSET_VAR || true
no qs_secret agents x Opaque api_key=QS_UNSET_VAR
# the function body never expands a value: every value reaches yq through strenv()
body=$(declare -f qs_secret)
[[ $body != *'"${!var}"'* && $body != *'${!var}"'* ]] || { echo "FAIL: qs_secret expands a variable's value" >&2; fail=1; }

# no script passes a literal secret on a command line
if grep -n -- '--from-literal' hack/quickstart/*.sh | grep -v '^hack/quickstart/lib_test.sh:'; then
  echo "FAIL: --from-literal puts a value on argv: use qs_secret" >&2; fail=1
fi
[[ $fail == 0 ]] && echo "lib_test: ok"
exit "$fail"
```

Add to `hack/quickstart/check.sh`, before its final `[[ $fail == 0 ]]`: `bash hack/quickstart/lib_test.sh || fail=1`.

- [ ] **Step 2: Run red**

Run: `bash hack/quickstart/lib_test.sh`
Expected: exit 1, `hack/quickstart/lib.sh: No such file or directory`.

- [ ] **Step 3: Implement `lib.sh`**

```bash
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# The quickstart's shared checks and helpers, sourced by up.sh, db.sh, check.sh, e2e.sh and lib_test.sh.
# Functions only: sourcing it runs nothing.

# qs_platform_ok OS: Linux is the one platform the quickstart is proven on (install-anywhere spike,
# 2026-10-27). Elsewhere gVisor in kind and a port-forward to a loopback-only listener never ran.
qs_platform_ok() {
  [[ $1 == Linux ]] && return 0
  echo "the quickstart runs on Linux with Docker only: $1 is unproven (gVisor in kind and a port-forward to a loopback-only listener were never run there); see docs/quickstart.md#prerequisites" >&2
  return 1
}

# qs_node_minor IMAGE: the Kubernetes minor of a kind node image, kindest/node:v1.37.0@sha256:… → 37.
qs_node_minor() {
  local v=${1#*:v}
  v=${v%%@*}
  v=${v#*.}
  echo "${v%%.*}"
}

# qs_skew_ok CLIENT SERVER: kubectl supports one minor version of skew either way. Minors may carry
# a suffix ("37+").
qs_skew_ok() {
  local c=${1//[!0-9]/} s=${2//[!0-9]/}
  if [[ -n $c && -n $s ]] && ((c - s <= 1 && s - c <= 1)); then return 0; fi
  echo "kubectl 1.${c:-?} cannot drive a 1.$s cluster: kubectl supports one minor version of skew (1.$((s - 1)) to 1.$((s + 1))); install a matching kubectl (mise install kubectl)" >&2
  return 1
}

# qs_unsandboxed VALUE: ALLOW_UNSANDBOXED is unset (0) or exactly 1 (1); anything else stops, so a
# typo never silently drops or keeps the sandbox.
qs_unsandboxed() {
  case $1 in
  "") echo 0 ;;
  1) echo 1 ;;
  *) echo "ALLOW_UNSANDBOXED is exactly 1 or unset, not '$1'" >&2; return 1 ;;
  esac
}

# qs_port_ok PORT: the local port the broker is forwarded to, and its publicURL's port.
qs_port_ok() {
  [[ $1 =~ ^[0-9]+$ ]] && (($1 >= 1024 && $1 <= 65535)) && return 0
  echo "LOCAL_PORT '$1' is not a port in 1024-65535" >&2
  return 1
}

# qs_secret NS NAME TYPE KEY=VAR...: a Secret manifest on stdout. Each value is read by yq from the
# environment variable VAR (strenv), so no value is ever a command-line argument (/proc/<pid>/cmdline
# is world-readable). Pipe it to kubectl apply -f -.
qs_secret() {
  local ns=$1 name=$2 type=$3 pair key var
  shift 3
  local expr='.apiVersion = "v1" | .kind = "Secret" | .metadata.name = strenv(QS_NAME) | .metadata.namespace = strenv(QS_NS) | .type = strenv(QS_TYPE)'
  for pair in "$@"; do
    key=${pair%%=*} var=${pair#*=}
    [[ $key =~ ^[A-Za-z0-9._-]+$ && $var =~ ^[A-Z_][A-Z0-9_]*$ ]] || { echo "qs_secret: '$pair' is not key=ENV_VAR" >&2; return 1; }
    [[ -n ${!var+set} ]] || { echo "qs_secret: $var is unset" >&2; return 1; }
    export "${var?}"
    expr+=" | .stringData.\"$key\" = strenv($var)"
  done
  QS_NS=$ns QS_NAME=$name QS_TYPE=$type yq -n "$expr"
}

# qs_fetch_roomctl VERSION DEST: the release's roomctl for this Linux machine, checked against the
# release's roomctl.sha256, installed 0755 at DEST.
qs_fetch_roomctl() {
  local arch tmp base rc=0
  case $(uname -m) in
  x86_64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "no roomctl release for $(uname -m)" >&2; return 1 ;;
  esac
  base="https://github.com/Smana/agent-platform/releases/download/$1"
  tmp=$(mktemp -d)
  if ! {
    curl -fsSL -o "$tmp/roomctl-linux-$arch" "$base/roomctl-linux-$arch" &&
      curl -fsSL -o "$tmp/roomctl.sha256" "$base/roomctl.sha256" &&
      (cd "$tmp" && grep " roomctl-linux-$arch\$" roomctl.sha256 | sha256sum -c - >/dev/null) &&
      install -m 0755 "$tmp/roomctl-linux-$arch" "$2"
  }; then
    rc=1
  fi
  rm -rf "$tmp" # this function's own temp dir
  ((rc == 0)) || echo "could not fetch roomctl $1 for linux-$arch (or its checksum did not match)" >&2
  return "$rc"
}
```

`${!var+set}` reads only whether `VAR` is set, never its value; the test's `declare -f` guard forbids `${!var}` itself.

- [ ] **Step 4: Run the function tests**

Run: `bash hack/quickstart/lib_test.sh`
Expected: every function assertion passes; the one remaining `FAIL: --from-literal puts a value on argv` names `up.sh` and `db.sh` lines (exit 1) until Step 5.

- [ ] **Step 5: Rewrite `up.sh`'s preflight, secrets, roomctl and next steps; move `db.sh` onto `qs_secret`**

`hack/quickstart/up.sh`:
- Header env list: add `LOCAL_PORT (default 8080: the port you forward; the broker's publicURL follows it)`, `ROOMCTL (a roomctl binary to use instead of the release's; required when UP_HELM_ARGS sets the image tags)`; `ALLOW_UNSANDBOXED=1 (exactly 1 …; each run's room then shows a Not sandboxed banner)`. Prerequisites line: `Linux; docker (with buildx) kind helm kubectl (within one minor of the kind node) curl git openssl envsubst yq sha256sum sha512sum bzip2 tar`.
- After `. hack/quickstart/versions.env`: `# shellcheck source=hack/quickstart/lib.sh` / `. hack/quickstart/lib.sh`, then `qs_platform_ok "$(uname -s)" || exit 1`.
- Tool loop: `for t in docker kind helm kubectl curl git openssl envsubst yq sha256sum sha512sum bzip2 tar; do need "$t"; done` (`base32` was run.sh's only).
- After the `REPO` check:

```bash
unsandboxed=$(qs_unsandboxed "${ALLOW_UNSANDBOXED:-}") || exit 1
LOCAL_PORT="${LOCAL_PORT:-8080}"
qs_port_ok "$LOCAL_PORT" || exit 1
qs_skew_ok "$(kubectl version --client -o json | yq -p=json '.clientVersion.minor')" "$(qs_node_minor "$KIND_NODE_IMAGE")" || exit 1
```

- `chart_args=(-f charts/agent-platform/values-quickstart.yaml --set createRunsNamespace=false --set "broker.publicURL=http://localhost:$LOCAL_PORT")`; `if [[ $unsandboxed == 1 ]]; then` replaces the `ALLOW_UNSANDBOXED` test, and `if [[ $unsandboxed == 0 ]]; then` replaces the probe guard.
- After the `helm template … >/dev/null` preflight render, add:

```bash
step "roomctl"
mkdir -p .quickstart/bin
if [[ -n ${ROOMCTL:-} ]]; then
  install -m 0755 "$ROOMCTL" .quickstart/bin/roomctl
elif [[ -n ${AP_VERSION:-} ]]; then
  qs_fetch_roomctl "$AP_VERSION" .quickstart/bin/roomctl
else
  echo "set ROOMCTL=<a roomctl built from this checkout>: UP_HELM_ARGS sets the images, so there is no release to take roomctl from" >&2
  exit 1
fi
.quickstart/bin/roomctl help | grep -q '^  run --repo' ||
  { echo ".quickstart/bin/roomctl has no run command: use a release that has it (AP_VERSION) or ROOMCTL" >&2; exit 1; }
```

- `step "secrets"` body after the namespace line:

```bash
export MODEL
model_keys=(api_key=MODEL_API_KEY model=MODEL)
[[ -z ${MODEL_BASE_URL:-} ]] || model_keys+=(base_url=MODEL_BASE_URL)
qs_secret agents agent-model Opaque "${model_keys[@]}" | kubectl apply -f -
qs_secret agents agent-git Opaque token=GITHUB_TOKEN | kubectl apply -f -
```

- Replace the closing `cat <<MSG … MSG` with:

```bash
if [[ $unsandboxed == 1 ]]; then
  printf '\n!!! NOT SANDBOXED: ALLOW_UNSANDBOXED=1. Agents run under the default runtime with your API key and PAT;\n!!! every such run shows a "Not sandboxed" banner in its room.\n' >&2
fi
cat <<MSG

Ready. In a second terminal, keep the broker forwarded (local mode answers this port only):
  export KUBECONFIG=$PWD/.quickstart/kubeconfig
  kubectl -n $NS port-forward deploy/room-broker $LOCAL_PORT:8080
Then, here (XDG_CONFIG_HOME keeps roomctl's quickstart config apart from any other):
  export KUBECONFIG=$PWD/.quickstart/kubeconfig XDG_CONFIG_HOME=$PWD/.quickstart/config PATH=$PWD/.quickstart/bin:\$PATH
  roomctl configure --url http://localhost:$LOCAL_PORT
  roomctl run --repo $REPO --task 'Fix a typo in the README'
The room page: http://localhost:$LOCAL_PORT
MSG
```

`hack/quickstart/db.sh`: add `# shellcheck source=hack/quickstart/lib.sh` / `. hack/quickstart/lib.sh` after the `cd`; replace the role-password loop body with:

```bash
for role in rooms-broker rooms-retention; do
  kubectl -n "$ns" get secret "$role-password" >/dev/null 2>&1 && continue
  QS_ROLE_USER=${role//-/_} QS_ROLE_PASSWORD=$(openssl rand -hex 24)
  qs_secret "$ns" "$role-password" kubernetes.io/basic-auth username=QS_ROLE_USER password=QS_ROLE_PASSWORD | kubectl apply -f -
done
```

and the two DSN Secrets with:

```bash
QS_OWNER_URI="$(kubectl -n "$ns" get secret rooms-app -o jsonpath='{.data.uri}' | base64 -d)?sslmode=require"
pw=$(kubectl -n "$ns" get secret rooms-broker-password -o jsonpath='{.data.password}' | base64 -d)
QS_BROKER_URI="postgresql://rooms_broker:${pw}@rooms-rw.${ns}.svc:5432/rooms?sslmode=require"
qs_secret "$ns" rooms-owner-dsn Opaque uri=QS_OWNER_URI | kubectl apply -f -
qs_secret "$ns" rooms-broker-dsn Opaque uri=QS_BROKER_URI | kubectl apply -f -
```

`git rm hack/quickstart/run.sh hack/quickstart/manifests/room.yaml.tmpl hack/quickstart/manifests/agentrun.yaml.tmpl`.

`taskfile.yaml`: delete the whole `quickstart:run` task; `quickstart` desc becomes `kind + Cilium + gVisor + cert-manager + CNPG + the chart + roomctl (Linux, docker; MODEL_API_KEY, GITHUB_TOKEN, REPO)`.

`mise.toml`, after the kind line: `# The quickstart's kubectl: within one minor of KIND_NODE_IMAGE (check.sh holds them together).` / `kubectl = "1.37.0"`.

- [ ] **Step 6: Update `check.sh` to the new flow**

- Remove the `ROOM_ID/RUN_ID/REPO/RUN_PROMPT` export, the `render()` helper, `room` and `agentrun` from the `for m in …` loop (keep `ca-issuer cnpg-cluster`), the `run-final.yaml` task-text check, and the three AgentRun/Room namespace and name assertions.
- Source the lib after versions.env: `# shellcheck source=hack/quickstart/lib.sh` / `. "$qs/lib.sh"`.
- Model/git Secrets: `grep -q "qs_secret agents $s " "$qs/up.sh" || bad "up.sh does not create $k ($s)"`.
- DSN Secrets loop body: `grep -qF -- "qs_secret \"\$ns\" $name Opaque $key=" "$qs/db.sh" || bad "db.sh does not create $name with key $key"` (drop the `dbflat` join).
- Local mode block becomes:

```bash
# local mode: one port, everywhere: the forward, publicURL, the configure line
grep -qF 'deploy/room-broker $LOCAL_PORT:8080' "$qs/up.sh" || bad "up.sh port-forward command"
grep -qF 'broker.publicURL=http://localhost:$LOCAL_PORT' "$qs/up.sh" || bad "up.sh does not set publicURL from LOCAL_PORT"
grep -qF 'roomctl configure --url http://localhost:$LOCAL_PORT' "$qs/up.sh" || bad "up.sh's configure line does not use LOCAL_PORT"
grep -qF 'roomctl run --repo $REPO' "$qs/up.sh" || bad "up.sh does not print roomctl run"
[[ $(q ConfigMap '.[] | select(.metadata.name=="room-broker-config") | .data."config.yaml" | from_yaml | .publicURL') == http://localhost:8080 ]] || bad "default publicURL is not http://localhost:8080"
# the repo's kubectl pin can drive the kind node
qs_skew_ok "$(sed -n 's/^kubectl = "1\.\([0-9]*\)\..*"/\1/p' mise.toml)" "$(qs_node_minor "$KIND_NODE_IMAGE")" || bad "mise.toml's kubectl is outside the kind node's skew"
grep -qF 'qs_skew_ok' "$qs/up.sh" || bad "up.sh has no kubectl skew preflight"
grep -qF 'qs_platform_ok "$(uname -s)"' "$qs/up.sh" || bad "up.sh does not stop on an unproven platform"
```

- Probe guard: `grep -qF 'unsandboxed == 0 ]]; then' "$qs/up.sh"` || bad "up.sh does not skip the probe under ALLOW_UNSANDBOXED=1"`; unsandboxed override grep stays.
- Tools: replace `for t in sha512sum bzip2 tar` with `for t in sha256sum sha512sum bzip2 tar`.

- [ ] **Step 7: Run the quickstart check**

Run: `task quickstart:check`
Expected: `lib_test: ok` then `quickstart check: ok`, exit 0 (shellcheck clean on `lib.sh` and `lib_test.sh`).

- [ ] **Step 8: Rewrite `docs/quickstart.md` and `hack/quickstart/README.md`**

`docs/quickstart.md`:
- Opening command block:

```bash
export MODEL_API_KEY=… GITHUB_TOKEN=… REPO=owner/throwaway
task quickstart                 # kind + Cilium + gVisor + cert-manager + CNPG + the chart + roomctl
```

- Prerequisites `Platform` row: "Linux with Docker (and the `buildx` plugin). `up.sh` stops on any other system and says why: gVisor in kind and a port-forward to a loopback-only listener are proven on Linux only (spike, 2026-10-27). Podman is untested." Add a row `| kubectl | Within one minor of the kind node (1.36–1.38 for 1.37): up.sh stops otherwise. mise.toml pins one: mise install kubectl |`. Add `sha256sum` to the tools row, drop `base32`. Add a row `| LOCAL_PORT | The port you forward to the broker, default 8080. The broker's publicURL follows it and it answers that port only, as localhost, 127.0.0.1 or [::1] |`. `Images` row: "…and `roomctl` from the same release (checksum-verified into `.quickstart/bin/`). `roomctl run` ships from the release after this change: until then set `AP_VERSION` to one that has it, or `ROOMCTL=<path>` to a build of this checkout".
- Mermaid: replace `F["print the port-forward command"] --> R["task quickstart:run<br/>Room + AgentRun"] --> W["watch the room"]` with `F["print the port-forward and roomctl commands"] --> R["roomctl run<br/>room, start_run, your kubectl applies the claim"] --> W["roomctl follows the room to the run's end"]`.
- Replace `## Start a run` and `## Watch the room` with one section `## Start a run` holding the two-terminal commands printed by `up.sh` (verbatim from Step 5) and: "`roomctl run` creates a room, queues your task for the run's brief, starts the run, applies the claim the broker rendered with your own `kubectl`, and follows the room until the run ends: exit 0 when it `Succeeded`. It stops at the first step that fails and names it. It is the quickstart's command only: on a platform install the factory starts runs." Then the local-mode table:

| Local mode | Consequence |
|---|---|
| The broker answers `publicURL`'s port only | Forward that port (`LOCAL_PORT`); `localhost`, `127.0.0.1` and `[::1]` all work, any other host or port answers `403`; `roomctl configure` refuses a mismatched port and prints the fix |
| One user, `human:local`, admin and member | No login; the port-forward is the only way in |
| The room page's Start run shows the claim | It does not apply it: `roomctl run` does |
| `XDG_CONFIG_HOME=$PWD/.quickstart/config` | Keeps the quickstart's roomctl config apart from a production one |

- Troubleshooting: `ALLOW_UNSANDBOXED` row ends "…The controller sets the run's `Unsandboxed` condition and its room shows a `Not sandboxed` banner. Any value other than `1` stops `up.sh`." Add rows: `| up.sh stops at "kubectl … cannot drive" | Your kubectl is more than one minor away from the kind node: mise install kubectl, or install a matching one |`, `| A browser or roomctl gets 403 | You forwarded another port than publicURL's: forward LOCAL_PORT, or re-run up.sh with LOCAL_PORT=<your port> |`, `| roomctl run stops at claim | kubectl's own error is printed above it: usually KUBECONFIG is not .quickstart/kubeconfig |`. Replace the `task quickstart:check` row text with "Offline drift check of the scripts and their helpers against the chart; no cluster needed".
- `## CI: quickstart-e2e`: "The job runs the whole path against a stub model: up.sh, a port-forward, `curl` checks of local mode through it, then `roomctl configure` and `roomctl run` to `Succeeded`, and a harness event in the room's log."

`hack/quickstart/README.md`: Platforms table rows become `| Linux + Docker (amd64) | pass: install-anywhere spike, 2026-10-27 (gVisor systrap in kind v0.32/node v1.37; port-forward to a 127.0.0.1-only listener) |`, `| macOS + Docker Desktop | unverified: up.sh stops there |`, `| Podman (rootful) | unverified |`; the `**Status:**` line becomes "Linux evidence from the 2026-10-27 spike; the live proof of the whole path is `quickstart-e2e`." Add one line under `## Escape hatch`: "Each unsandboxed run's room shows a `Not sandboxed` banner."

Run: `task docs`
Expected: exit 0.

**Review gate:** no `--from-literal` remains; every value in a Secret reaches yq by `strenv`; a non-Linux host, a skewed kubectl and a non-`1` `ALLOW_UNSANDBOXED` each stop `up.sh` before the cluster exists; the forwarded port, `publicURL` and the configure line come from one `LOCAL_PORT`.

---

### Task 8: `quickstart-e2e` through `roomctl run`

**Files:**
- Modify: `hack/quickstart/e2e.sh`, `.github/workflows/quickstart-e2e.yaml`
- Test: the script itself, statically now (`task quickstart:check`, `actionlint` if on PATH); live once in Task 9.

**Interfaces:**
- Consumes: Task 7's `up.sh` (`ROOMCTL`, `LOCAL_PORT`), Task 4's output lines `room <id>: …` and `run requested: <ns>/<name>`, Task 2's alias behaviour.
- Produces: one end-to-end proof: local-mode HTTP checks through the real port-forward, the broker on the default runtime, `roomctl run` exit 0, the claim `Succeeded`, a harness-origin event in the room.

- [ ] **Step 1: Rewrite the run section of `e2e.sh`**

- Header: `# quickstart-e2e: this checkout's images and roomctl in kind, a stub model, one run to Succeeded through roomctl run.`
- After `step "build the images"`'s loop add: `mkdir -p .quickstart/e2e && go build -o .quickstart/e2e/roomctl ./cmd/roomctl`.
- In `step "up.sh …"`: add `ROOMCTL="$PWD/.quickstart/e2e/roomctl" LOCAL_PORT=18080` to the exports (`export … ROOMCTL LOCAL_PORT`). A non-default port exercises the publicURL/port agreement.
- `onexit`: before `[[ $rc -eq 0 ]] || dump` add `[[ -z ${pf:-} ]] || kill "$pf" 2>/dev/null || true` (this script's own port-forward child). `dump` adds `cat .quickstart/e2e/port-forward.log >&2 || true`.
- Replace everything from `step "run"` to the end with:

```bash
step "local mode through a real port-forward"
kubectl -n agent-platform port-forward deploy/room-broker 18080:8080 >.quickstart/e2e/port-forward.log 2>&1 &
pf=$!
for _ in $(seq 30); do curl -fsS -o /dev/null http://localhost:18080/.well-known/roomctl 2>/dev/null && break; sleep 1; done
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
[[ $(curl -fsS http://localhost:18080/.well-known/roomctl | yq -p=json '.humanMode + " " + .url') == "local http://localhost:18080" ]] ||
  { echo "discovery is not local at http://localhost:18080" >&2; exit 1; }
[[ $(code http://localhost:18080/api/rooms) == 200 ]] || { echo "localhost: no 200" >&2; exit 1; }
[[ $(code http://127.0.0.1:18080/api/rooms) == 200 ]] || { echo "127.0.0.1 (the loopback-literal fix): no 200" >&2; exit 1; }
[[ $(code -H 'Origin: https://evil.example' http://localhost:18080/api/rooms) == 403 ]] || { echo "a foreign Origin is not 403" >&2; exit 1; }
[[ $(code -H 'Host: evil.example:18080' http://127.0.0.1:18080/api/rooms) == 403 ]] || { echo "a rebound Host is not 403" >&2; exit 1; }
[[ -z $(kubectl -n agent-platform get pods -l app.kubernetes.io/name=room-broker -o jsonpath='{.items[*].spec.runtimeClassName}') ]] ||
  { echo "the broker pod has a runtimeClassName: port-forward cannot reach a runsc pod" >&2; exit 1; }

step "stub model"
kubectl apply -f hack/quickstart/stub-model.yaml.tmpl
kubectl -n e2e rollout status deploy/stub-model --timeout=120s

step "roomctl run"
export XDG_CONFIG_HOME="$PWD/.quickstart/e2e/config" # never the developer's roomctl config, never HOME
mkdir -p "$XDG_CONFIG_HOME"
.quickstart/e2e/roomctl configure --url http://localhost:18080
rc=0
timeout 900 .quickstart/e2e/roomctl run --repo "$REPO" --task 'quickstart-e2e: change nothing' | tee .quickstart/e2e/run.log || rc=$?
room=$(sed -n 's/^room \([a-z2-7]\{8\}\): .*/\1/p' .quickstart/e2e/run.log)
claim=$(sed -n 's#^run requested: \([a-z0-9-]*\)/\([a-z0-9-]*\)$#\2#p' .quickstart/e2e/run.log)
[[ -n $room && -n $claim ]] || { echo "roomctl run printed no room or claim" >&2; exit 1; }
((rc == 0)) || { echo "roomctl run exited $rc" >&2; exit 1; }
phase=$(kubectl -n agents get agentrun "$claim" -o jsonpath='{.status.phase}')
[[ $phase == Succeeded ]] || { echo "AgentRun $claim is '$phase', roomctl run said Succeeded" >&2; exit 1; }

# lastSeq is not evidence: the Room's Open event and the broker's own run events raise it with no
# bridge. Only a harness-origin row proves the bridge mirrored the agent into the room.
n=$(kubectl -n agent-platform exec rooms-1 -c postgres -- psql -d rooms -tAc \
  "select count(*) from events where room_id='$room' and origin='harness'")
((${n:-0} > 0)) || { echo "room $room has no harness-origin event" >&2; exit 1; }
echo "quickstart-e2e OK: roomctl run took $claim to Succeeded; room $room has $n harness events"
```

`$room` is validated by its `[a-z2-7]{8}` capture before it reaches the SQL string.

- [ ] **Step 2: Update the workflow**

`.github/workflows/quickstart-e2e.yaml`, the `jdx/mise-action` step: `install_args: go kubectl aqua:kubernetes-sigs/kind helm aqua:mikefarah/yq # go builds roomctl; kubectl is the mise.toml pin, within the kind node's skew`. Header comment line 1: `# The whole quickstart in CI: this checkout's images and roomctl in kind, a stub model, one run to Succeeded through roomctl run.`

- [ ] **Step 3: Static checks**

Run: `task quickstart:check` and, if `actionlint` is on PATH, `actionlint .github/workflows/quickstart-e2e.yaml`
Expected: `quickstart check: ok`; actionlint exit 0 (or not installed: say so in the task report).

Do **not** run `e2e.sh` here: it runs once, in Task 9.

**Review gate:** no step reads or writes the developer's roomctl config or `HOME`; the run's success is proven three ways (roomctl's exit, the claim's phase, a harness event); the port-forward child is the only process the trap kills.

---

### Task 9: Final gate, the one end-to-end run, the stage commit

**Files:** none new. Evidence goes into the SDD ledger, not the repository.

- [ ] **Step 1: Make sure nothing heavy runs**

Run: `pgrep -af 'go test|vitest|docker build|kind create|task check' || echo idle`
Expected: `idle`. If anything is listed, wait for it; never start the gate beside it.

- [ ] **Step 2: Stage the stage, then run the full gate once**

`crd:check` and `ui:check` compare generated files with the git index, so stage every change first (staging is not committing):

```bash
git add -A charts hack internal web taskfile.yaml mise.toml .gitignore .github \
  docs/quickstart.md docs/roomctl.md docs/api.md docs/event-envelope.md docs/web-ui.md docs/integration.md \
  docs/superpowers/plans/2026-10-27-install-anywhere-chart-quickstart-plan.md
git status --short   # only this plan's files staged; docs/naming.md and the Maku plan stay untracked (out of scope)
```

Run: `task check 2>&1 | tee "$SCRATCH/task-check.log"; echo "exit=${PIPESTATUS[0]}"`
Expected: `exit=0`. On a failure: fix only what the log names (lint, generated files, a test), re-run only the failing step's own task (`task lint`, `task ui:check`, a targeted `go test`), then the full `task check` once more. A fix that changes runtime behaviour goes back to its task's reviewer first.

- [ ] **Step 3: Preconditions for the end-to-end run**

The e2e needs the owner's throwaway repository and its PAT, and must not reuse an existing quickstart cluster.

```bash
[[ -n ${REPO:-} && -n ${GITHUB_TOKEN:-} ]] || echo "STOP: ask the owner for REPO and GITHUB_TOKEN (throwaway repo, Contents read)"
kind get clusters | grep -qx agent-platform && echo "STOP: a kind cluster 'agent-platform' exists: ask the owner before e2e reuses it"
mise install kubectl && kubectl version --client -o json | yq -p=json '.clientVersion.minor'   # expect 36, 37 or 38
pgrep -af 'go test|vitest|docker build|task check' || echo idle
```

Any `STOP` ends the task here with a report; never fake the inputs, never delete a cluster you did not create.

- [ ] **Step 4: Run quickstart-e2e once**

Run: `hack/quickstart/e2e.sh 2>&1 | tee "$SCRATCH/quickstart-e2e.log"; echo "exit=${PIPESTATUS[0]}"`
Expected: last line `quickstart-e2e OK: roomctl run took run-<id> to Succeeded; room <id> has <n> harness events`, `exit=0`.
On failure: keep the cluster and the log for diagnosis, report the failing step; do not re-run blindly and do not run `task check` beside it.

- [ ] **Step 5: Leave the cluster to the owner**

Report the cluster as left running. `task quickstart:down` only on the owner's word, since it deletes the `agent-platform` kind cluster.

- [ ] **Step 6: One stage commit**

```bash
git add -A charts hack internal web taskfile.yaml mise.toml .gitignore .github \
  docs/quickstart.md docs/roomctl.md docs/api.md docs/event-envelope.md docs/web-ui.md docs/integration.md \
  docs/superpowers/plans/2026-10-27-install-anywhere-chart-quickstart-plan.md   # any fix since Step 2
git status --short     # expect only this plan's files; no .quickstart/, no charts/agent-platform/charts/
git commit -m "feat(quickstart): roomctl run, a factory-free chart, and the local-mode review fixes

The agent-platform chart drops its agent-factory dependency and factory values,
honours broker.publicURL in local mode and refuses a broker runtimeClassName.
roomctl run creates a room, queues the task, starts the run, applies the
broker-rendered claim with the user's kubectl and follows the room to its end.
Local mode admits its port under localhost, 127.0.0.1 and [::1]; the quickstart
keeps secrets off argv, stops on unproven platforms and kubectl skew, ties
publicURL to the forwarded port, and every unsandboxed run shows a room banner."
```

No `Co-Authored-By` or generated-with trailer (owner rule). Do not push.

---

## Spec coverage

| Spec requirement | Task |
|---|---|
| I4: one chart, broker + run-controller, no factory dependency | 1 |
| I1 + port-forward boundary, `publicURL` as the origin | 1, 2, 3, 7 |
| I7: `roomctl run --repo --task [--role implementer]`, broker creates no AgentRun, quickstart-only help | 4 |
| Error handling: `roomctl run` names the failing step, passes kubectl's error through | 4 |
| Risks: non-Linux stops with the reason; gVisor failure offers `ALLOW_UNSANDBOXED=1`; condition + banner | 5, 7 |
| Quickstart flow: port-forward, print URL and next command, `roomctl run` | 7 |
| Testing: chart render over both value sets, no factory | 1 |
| Testing: e2e on a stub model and throwaway repo, room events and `Succeeded` | 8, 9 |
| Carry-forwards: loopback literals, no argv secrets, broker default runtime, port agreement, kubectl skew | 2, 7, 1, 3/7, 7 |
| Deferred minor: web CLI setup in local mode | 6 |

## Resolved ambiguities

1. **`start_run` carries no task text.** The spec's `roomctl run` "sends `start_run`"; the broker builds a run's task from the room's queue (`brief.Build`). `roomctl run` therefore queues `--task` first, then sends `start_run`: the broker's only path from human text to a task, and the brief fences it as untrusted data.
2. **"Reuse current-main roomctl start".** `main` has no `roomctl start` command: #216's "start" is `issue_file`'s factory start, which local mode refuses. The reused seams are `Client.Act`, `configure`/`Discover`, `Watch` (generalised to `Follow`) and the `start_run` ack's runrequest claim.
3. **Origins beyond `publicURL`.** The spec makes a non-`publicURL` Origin a 403. The plan admits exactly publicURL's port under the three loopback names, which are the same listener through the same port-forward; other hosts, ports and schemes stay refused.
4. **The banner.** The 2026-10-20 fix wave wrote "no banner"; the amended spec (authoritative) requires one. Task 5 builds it from the controller's per-run condition.
5. **Platforms.** The spec lists Linux and macOS risks; the spike proved Linux only, so `up.sh` stops on every other `uname -s`.
6. **Gate order.** `task check` runs before the single e2e (no concurrency); the commit follows both.
