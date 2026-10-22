# Run provenance (v3 review R10) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every factory run records on its `Task` what it ran on: the factory build that created
it, the `AgentRun` CompositionRevision it was bound to, the harness image digest its pod ran, and
the models that served it; the room shows the current run's.

**Architecture:** Three writers fill one record. The factory stamps its own `version.Version` on
every claim it creates, as a CREATE-only annotation. Crossplane already writes the claim's
`spec.crossplane.compositionRevisionRef`. The `AgentRun` composition (crossplane-configuration)
projects the harness container's `imageID` from the run's pod, which it already reads, into the
claim's `status.harnessImage`. The reconciler copies those three into `status.runs[].provenance`,
each once, and adds the models the meter saw serve the run (`gen_ai_request_model`, handed off in
`meter.Cost.Served`). The task facts it posts to the room carry the current run's.

**Tech Stack:** Go 1.27 (controller-runtime, controller-gen, envtest with kube-apiserver 1.36),
KCL 0.11.3 under function-kcl, Crossplane 2.4 XRDs, the room broker's strict JSON decoder.

**Spec:** ai-native-ref [`docs/superpowers/specs/2026-10-22-run-provenance-design.md`](https://github.com/Smana/ai-native-ref/blob/main/docs/superpowers/specs/2026-10-22-run-provenance-design.md):
decisions P1–P8, its "Pull requests, in order" and offline proofs 1–7. Open owner decisions OP1–OP3
do not change this plan's code: OP1's default (always on) is what it builds; OP2 and OP3 are
release choices.

**Out of this plan:** the bench, its row script and its attribution rule (P8 is the bench design's
to implement). The portable runtime's run-controller (its native `AgentRun` gets the factory
version only). API runs keep their provenance on their claim alone.

### This plan's rulings

| # | Ruling | Why |
|---|---|---|
| R1 | **Names.** Go: `v1alpha1.RunProvenance`, `RunRecord.Provenance`, `runs.AnnFactoryVersion`, `runs.Run.{FactoryVersion,CompositionRevision,HarnessImage}`, `meter.Cost.Served`, `meter.Usage.served()`, the reconciler's `stamp`, `served` and `maxServedModels`, `envelope.ProvenanceFact`, `envelope.RunFact.Provenance`. KCL: `_harnessImage` | Left to the builder; these are the prototype's |
| R2 | **The `imageID` pattern** is `^[a-z0-9][a-z0-9./:_-]{0,190}@sha256:[0-9a-f]{64}$`: a registry host with an optional port, a path, and the digest, at most 262 characters, under the XRD's and the CRD's 320 | A tag-only or empty `imageID` must project nothing (spec P4) |
| R3 | **`runs.Build` stamps every claim**, so its annotations are never empty and `SetAnnotations` becomes unconditional. Tests see `version.Version` as `dev` | P2: the run API's claims carry it too. P8 refuses `dev` runs, so a local build is visible as such |
| R4 | **`meter.Cost` stops being comparable** (it holds a slice). The one test that compared a `Cost` with `!=` compares its fields | The alternative, a joined string, would be parsed again by the reconciler |
| R5 | **The served models are set after the pricing verdict**, only on a seen, typed reading, and only for model labels with tokens. An untyped reading or one without `gen_ai_request_model` hands over none | Only a label names a served model; the claim's model is an alias (P5) |
| R6 | **No provenance, no `provenance` key** in the facts: `RunFact.Provenance` is a pointer, nil while all four are empty. A task whose runs predate the field hashes as before (`factsKey`), so an upgrade re-posts nothing | The room seals at its event limit |
| R7 | **No new `TaskFacts.Validate` rule.** The factory is the facts' one writer and the CRD bounds every field, as `issue.title` | YAGNI |
| R8 | **The envtest proof lives in the reconciler package**, beside `envtest_test.go`, skipped without `KUBEBUILDER_ASSETS` as that one is; `task test` sets it | The package already has `scheme()`, `issueTask` and `rid` |
| R9 | **The meter change lands after #155** (`feat/daily-caps-usd`, which rewrites `Tick`). Its insertion point survives: #155 keeps the `if why != "" { costs[r.ID] = Cost{Why: why} … }` block, and Task 4's lines go right after that block | Measured on `1b2c530`, the branch's head on 2026-10-22 |

**Measured 2026-10-22** on a prototype (crossplane-configuration at `origin/main` exported to
`$SCRATCH/xpc/`, and this branch before the plan commit, both discarded since; the code below is
the prototype's):

- **KCL:** the three new tests fail 2 of 108 before the change and pass 108 of 108 after it.
  Dropping the latch (`previous if previous else`) fails `test_the_harness_image_latches`.
  `kcl fmt` leaves both files unchanged. `scripts/validate-schemas.sh` passes with the XRD field
  (25 example claims valid against 7 XRDs). `scripts/render_check.py` cannot run here: it needs
  Docker ("Cannot connect to the Docker daemon"), so CI runs it.
- **Go:** the `runs` and reconciler tests pass. Writing over a filled field
  (`cmp.Or(x.CompositionRevision, p.CompositionRevision)`) fails `TestARunRecordsWhatItRanOn` with
  the newer revision in the record. `go test` passes on `./internal/factory/...`,
  `./internal/envelope/...`, `./internal/summary/...`, `./internal/bridgeapi/...` and `./api/...`
  (the CRD bounds test included). `task lint`: 0 issues, once `gofmt` realigned the `Run` struct
  (Task 3 shows the aligned form).
- **envtest, kube-apiserver 1.36.2:** a status write keeps all four fields. Nine served models are
  refused: `status.runs[0].provenance.servedModels: Too many: 9: must have at most 8 items`. A CRD
  carrying a transition rule under `status.runs` is refused at creation: `oldSelf cannot be used on
  the uncorrelatable portion of the schema within …properties[runs]` (spec P6).
- **Today's broker** answers task facts that carry `run.provenance` with 400 `bad_message`: its
  decoder refuses unknown fields. No phase waits on facts (`reconciler.go:217`).
- **Crossplane 2.4** sets an XR's `spec.crossplane.compositionRevisionRef` at creation whatever its
  `compositionUpdatePolicy` (Crossplane docs, v2.4, "Composition revisions").

## Global Constraints

- **Branches.** agent-platform: `feat/run-provenance` in `/home/smana/Sources/agent-platform-provenance`,
  from `origin/main` (`d40876a`); this plan is its first commit. crossplane-configuration: its own
  worktree, `git -C /home/smana/Sources/crossplane-configuration worktree add /home/smana/Sources/crossplane-configuration-provenance -b feat/agentrun-harness-image origin/main`,
  then `mise trust mise.toml` there. The main checkout of crossplane-configuration is behind
  `origin/main`: never work in it.
- **`$SCRATCH`** is a scratch directory outside both repositories, for logs.
- **#155 first for Task 4.** Rebase `feat/run-provenance` onto a freshly fetched `origin/main` once
  #155 merges, and before every push (R9).
- **`task check` is the evidence** in each repository. agent-platform's store tests use
  testcontainers and its UI steps need npm: where Docker is not running, run the steps that do not
  need it (`task lint`, `task crd:check`, `task docs`, and `go test` on the packages each task
  names) and let CI run the whole `task check`. crossplane-configuration's `task render` needs
  Docker too; CI runs it.
- **envtest:** `export KUBEBUILDER_ASSETS="$(go tool setup-envtest use -p path 1.36.x)"`, as `task test` does.
- **Generated files are never edited by hand:** `task crd:gen` writes `config/crd/`, the chart's
  CRD copy and the deepcopy; `task generate` writes crossplane-configuration's `composition.yaml`.
- **Every `.go` file starts with `// SPDX-License-Identifier: Apache-2.0`** (`hack/spdx.sh`).
- **Commits** are conventional, in English, with **no trailers**. `git add` named paths only.
- **Verbatim from the spec:**
  - P1: "`status.runs[].provenance{factoryVersion, compositionRevision, harnessImage, servedModels[]}`.
    Each field fills once as it becomes known; `servedModels` only grows, sorted, and stops at 8
    (a ninth model is not recorded). Empty means unknown, never a guess".
  - P2: "`runs.Build` writes `agents.ogenki.io/factory-version: <version.Version>` on every claim;
    `Annotate` refuses the key; the record reads it back".
  - P3: "The factory reads `spec.crossplane.compositionRevisionRef.name` from the claim; the first
    value it sees stays".
  - P4: "The composition projects the `harness` container's `imageID` into the claim's
    `status.harnessImage`, only when it reads `<repository>@sha256:<64 hex>`, and keeps the first
    value".
  - P5: "`meter.Cost` gains `Served`, the models with tokens in the run's typed reading; the
    reconciler adds them to the record where it takes the cost (`price`)".
  - P7: "The task facts' `run` gains `provenance`, copied from the record; `summary/v1` and
    `roomctl status` pass it through".
  - Interfaces: `factoryVersion` (≤ 64), `compositionRevision` (≤ 253), `harnessImage` (≤ 320),
    `servedModels` (≤ 8 items of ≤ 64); the XRD's `status.harnessImage` has `maxLength: 320`.

## File structure

| Repository | File | Change | Responsibility |
|---|---|---|---|
| crossplane-configuration | `apis/agentrun/kcl/main.k` | modify | `_harnessImage`; `status.harnessImage` |
| crossplane-configuration | `apis/agentrun/kcl/main_test.k` | modify: append | Offline proof 1 |
| crossplane-configuration | `apis/agentrun/definition.yaml` | modify | `status.harnessImage` in the XRD |
| crossplane-configuration | `apis/agentrun/composition.yaml` | generated | `task generate` |
| agent-platform | `api/factory/v1alpha1/task_types.go` | modify | `RunProvenance`, `RunRecord.Provenance` |
| agent-platform | `config/crd/agents.ogenki.io_tasks.yaml`, `charts/agent-factory/crds/agents.ogenki.io_tasks.yaml`, `api/factory/v1alpha1/zz_generated.deepcopy.go` | generated | `task crd:gen` |
| agent-platform | `internal/factory/reconciler/provenance_envtest_test.go` | create | Offline proof 6 |
| agent-platform | `internal/factory/runs/runs.go` | modify | The annotation; the claim reads |
| agent-platform | `internal/factory/runs/provenance_test.go` | create | Offline proof 3 |
| agent-platform | `internal/factory/meter/meter.go` | modify | `Cost.Served`, `Usage.served` |
| agent-platform | `internal/factory/meter/usd_test.go` | modify: one comparison | R4 |
| agent-platform | `internal/factory/meter/provenance_test.go` | create | Offline proof 5 |
| agent-platform | `internal/factory/reconciler/implement.go` | modify | `stamp`, `served`; `absorb` stamps |
| agent-platform | `internal/factory/reconciler/reconciler.go` | modify | `price` adds the served models |
| agent-platform | `internal/factory/reconciler/provenance_test.go` | create | Offline proof 4 |
| agent-platform | `internal/envelope/payloads.go` | modify | `ProvenanceFact`, `RunFact.Provenance` |
| agent-platform | `internal/factory/reconciler/facts.go` | modify | `factsOf` copies the current run's |
| agent-platform | `internal/bridgeapi/provenance_test.go`, `internal/summary/provenance_test.go` | create | Offline proof 7 |
| agent-platform | `docs/factory.md`, `docs/event-envelope.md`, `docs/integration.md` | modify | The record, the facts, the composition's contract |

---

## Part A: crossplane-configuration

### Task 1: The composition records the harness image its pod ran

**Files:**
- Modify: `apis/agentrun/kcl/main.k` (a lambda before `# Whether the Sandbox controls the pod.`,
  one line after `_tp = _traceparent(_ann)`, two lines after the status's `pullRequest`)
- Modify: `apis/agentrun/kcl/main_test.k` (append)
- Modify: `apis/agentrun/definition.yaml` (`status.properties`, after `pullRequest`)
- Generated: `apis/agentrun/composition.yaml`

**Interfaces:**
- Consumes: `_runPod`, the run's pod when the Sandbox owns it; `_previous`, the XR's last status;
  the test helpers `_endWith`, `_appliedSandbox`, `_suspendedSandbox`, `_status`, `_run`, `_OWNED`.
- Produces: the claim's `status.harnessImage: <repository>@sha256:<64 hex>`, which Task 3's
  `runs.FromUnstructured` reads.

- [ ] **Step 1: Write the failing tests**

Append to `apis/agentrun/kcl/main_test.k`:

```python
# ---- R10: the image the run ran on ----
_DIGEST = "ghcr.io/smana/agent-harness@sha256:c49cece37477cc70a1f88283438d588abd7610bda00830b73fbfc53fc7f99893"
_runningPod = lambda imageID: str -> any {
    {
        metadata = {name = _NAME, namespace = "agents", ownerReferences = _OWNED}
        status = {phase = "Running", containerStatuses = [
            {name = "room-bridge", imageID = "ghcr.io/smana/room-bridge@sha256:" + "0" * 64, state.running = {}}
            {name = "harness", imageID = imageID, state.running = {}}
        ]}
    }
}

test_the_run_records_the_harness_image_its_pod_ran = lambda {
    _st = _endWith({phase = "Running"}, _appliedSandbox(False, [{type = "Ready", status = "True"}]), [_runningPod(_DIGEST)])
    assert _st.harnessImage == _DIGEST, "the harness container's, not a sidecar's"
}

test_the_harness_image_latches = lambda {
    _other = "ghcr.io/smana/agent-harness@sha256:" + "a" * 64
    _st = _endWith({phase = "Running", harnessImage = _DIGEST}, _appliedSandbox(False, [{type = "Ready", status = "True"}]), [_runningPod(_other)])
    assert _st.harnessImage == _DIGEST, "a resumed pod on another image keeps the first"
    _gone = _endWith({phase = "Succeeded", harnessImage = _DIGEST}, _suspendedSandbox("Succeeded"), [])
    assert _gone.harnessImage == _DIGEST, "it outlives the pod"
}

test_no_pod_or_a_malformed_image_id_projects_nothing = lambda {
    assert "harnessImage" not in _endWith({phase = "Running"}, _appliedSandbox(False, [{type = "Ready", status = "True"}]), [])
    assert "harnessImage" not in _endWith({phase = "Running"}, _appliedSandbox(False, [{type = "Ready", status = "True"}]), [_runningPod("ghcr.io/smana/agent-harness:v0.15.2")]), "a tag is not a digest"
    assert "harnessImage" not in _endWith({phase = "Running"}, _appliedSandbox(False, [{type = "Ready", status = "True"}]), [_runningPod("")])
    assert "harnessImage" not in _status(_run({})), "kcl test: no requiredResources at all"
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd apis/agentrun/kcl && kcl test . -Y settings-example.yaml 2>&1 | grep -E "harness_image|image_id|PASS:|FAIL:"`
Expected: `test_the_run_records_the_harness_image_its_pod_ran: FAIL`,
`test_the_harness_image_latches: FAIL`, `test_no_pod_or_a_malformed_image_id_projects_nothing: PASS`,
`FAIL: 2/108`.

- [ ] **Step 3: Write the lambda**

In `apis/agentrun/kcl/main.k`, insert right before the line `# Whether the Sandbox controls the pod.
A claim re-created under the same name could otherwise read`:

```python
# R10: the image the run's harness container ran, as the kubelet reports it (imageID): the digest it
# pulled, not the reference asked for. Latched: the first value stays, so a resumed pod never rewrites
# what the run started on. A value that is not <repository>@sha256:<digest> is never projected.
_harnessImage = lambda pod: any, previous: any -> any {
    _ids = [s.imageID for s in _get(pod?.status, "containerStatuses") or [] if s?.name == "harness" and s?.imageID]
    previous if previous else _ids[0] if _ids and regex.match(_ids[0], "^[a-z0-9][a-z0-9./:_-]{0,190}@sha256:[0-9a-f]{64}$") else None
}

```

In `_renderRun`, right after `    _tp = _traceparent(_ann)`, add:

```python
    _image = _harnessImage(_runPod, _get(_previous, "harnessImage"))
```

`_get`, not `_previous?.harnessImage`: the comment above `_prevReason` in the same function says why.

In the status block, right after

```python
            if _pr:
                pullRequest = _pr
```

add:

```python
            if _image:
                harnessImage = _image
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd apis/agentrun/kcl && kcl fmt . && git diff --stat -- . && kcl test . -Y settings-example.yaml 2>&1 | tail -2`
Expected: `kcl fmt` changes nothing beyond your edits (the diff lists only `main.k` and
`main_test.k`); `PASS: 108/108`.

- [ ] **Step 5: The XRD field**

In `apis/agentrun/definition.yaml`, under `status.properties`, right after

```yaml
                pullRequest:
                  type: string
```

add:

```yaml
                harnessImage:
                  description: >-
                    The image the run's harness container ran, <repository>@sha256:<digest>, as the
                    kubelet reports it (imageID). Set once the pod reports it, then never changed (R10).
                  type: string
                  maxLength: 320
```

Without it the API server prunes the field from every status write.

- [ ] **Step 6: Generate and check**

Run: `task generate && task test && task schema`
Expected: `composition.yaml` regenerated; every API's KCL tests pass; `25 example claim(s) valid
against 7 shipped XRD(s)`. Then `task check`: on a machine without Docker its `render` step fails
to start functions ("Cannot connect to the Docker daemon"); CI runs it. No golden file changes:
no example renders a pod.

- [ ] **Step 7: Commit**

```bash
git add apis/agentrun/kcl/main.k apis/agentrun/kcl/main_test.k apis/agentrun/definition.yaml apis/agentrun/composition.yaml
git commit -m "feat(agentrun): record the harness image the run's pod ran in status.harnessImage"
```

---

## Part B: agent-platform

### Task 2: The run record's provenance field

**Files:**
- Modify: `api/factory/v1alpha1/task_types.go` (`RunRecord`, after `Finished`; a new type after it)
- Generated: `config/crd/agents.ogenki.io_tasks.yaml`, `charts/agent-factory/crds/agents.ogenki.io_tasks.yaml`, `api/factory/v1alpha1/zz_generated.deepcopy.go`
- Create: `internal/factory/reconciler/provenance_envtest_test.go`

**Interfaces:**
- Produces: `v1alpha1.RunProvenance{FactoryVersion, CompositionRevision, HarnessImage string; ServedModels []string}`
  and `v1alpha1.RunRecord.Provenance RunProvenance` (a value, `json:"provenance,omitempty"`), which
  Tasks 5 and 6 fill and read.

- [ ] **Step 1: Write the failing test**

Create `internal/factory/reconciler/provenance_envtest_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
)

// R10: the real API server keeps a run's provenance through a status write, and bounds its models.
func TestEnvtestRunProvenance(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset: run `task test`, which sets it")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true}
	restCfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = env.Stop() }()
	c, err := client.New(restCfg, client.Options{Scheme: scheme()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agent-system"}}); err != nil {
		t.Fatal(err)
	}
	tk := issueTask("3buqdlot", 7, "x")
	if err := c.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}
	want := v1alpha1.RunProvenance{FactoryVersion: "v0.16.0", CompositionRevision: "xagentruns.cloud.ogenki.io-41b6efe",
		HarnessImage: "ghcr.io/smana/agent-harness@sha256:" + strings.Repeat("c", 64), ServedModels: []string{"claude-opus-5-5", "glm-5.3"}}
	tk.Status.Phase = v1alpha1.PhaseImplementing
	tk.Status.Runs = []v1alpha1.RunRecord{{ID: rid(0), Role: "implementer", Trigger: "initial", Provenance: want}}
	if err := c.Status().Update(ctx, tk); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.Task
	if err := c.Get(ctx, types.NamespacedName{Namespace: "agent-system", Name: "3buqdlot"}, &got); err != nil {
		t.Fatal(err)
	}
	p := got.Status.Runs[0].Provenance
	if p.FactoryVersion != want.FactoryVersion || p.CompositionRevision != want.CompositionRevision ||
		p.HarnessImage != want.HarnessImage || !slices.Equal(p.ServedModels, want.ServedModels) {
		t.Fatalf("the API server changed it: %+v", p)
	}
	got.Status.Runs[0].Provenance.ServedModels = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	if err := c.Status().Update(ctx, &got); err == nil || !strings.Contains(err.Error(), "must have at most 8 items") {
		t.Fatalf("a ninth served model: %v", err)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/factory/reconciler/ -run TestEnvtestRunProvenance`
Expected: FAIL to build: `undefined: v1alpha1.RunProvenance`.

- [ ] **Step 3: Add the type**

In `api/factory/v1alpha1/task_types.go`, replace the end of `RunRecord`:

```go
	// +optional
	Started *metav1.Time `json:"started,omitempty"`
	// +optional
	Finished *metav1.Time `json:"finished,omitempty"`
}
```

with:

```go
	// +optional
	Started *metav1.Time `json:"started,omitempty"`
	// +optional
	Finished *metav1.Time `json:"finished,omitempty"`
	// What the run ran on (R10), each field filled once as it becomes known.
	// +optional
	Provenance RunProvenance `json:"provenance,omitempty"`
}

// RunProvenance is what a run ran on (R10). The reconciler fills each field once, from the
// run's claim or the meter, and never changes it; ServedModels only grows. Empty: not known
// yet, or the claim predates the field — never a guess.
type RunProvenance struct {
	// The factory build that created the claim (version.Version), from its CREATE-only annotation.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	FactoryVersion string `json:"factoryVersion,omitempty"`
	// The CompositionRevision Crossplane bound the claim to, as first read.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	CompositionRevision string `json:"compositionRevision,omitempty"`
	// The harness container's imageID, <repository>@sha256:<digest>, as the composition read it
	// from the run's pod.
	// +kubebuilder:validation:MaxLength=320
	// +optional
	HarnessImage string `json:"harnessImage,omitempty"`
	// The models that served the run, as the meter read gen_ai_request_model; sorted.
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=64
	// +optional
	ServedModels []string `json:"servedModels,omitempty"`
}
```

- [ ] **Step 4: Generate**

Run: `task crd:gen && git status --short`
Expected: the type file, the deepcopy, and both CRD copies modified; nothing else.

- [ ] **Step 5: Run the tests to verify they pass**

Run:
```bash
export KUBEBUILDER_ASSETS="$(go tool setup-envtest use -p path 1.36.x)"
go test -count=1 ./internal/factory/reconciler/ -run TestEnvtestRunProvenance -v
go test -count=1 ./api/...
```
Expected: `--- PASS: TestEnvtestRunProvenance`; `ok` for both API packages, the bounds test
(`TestTaskCRDBoundsEveryStringAndList`) included.

- [ ] **Step 6: Commit**

```bash
git add api/factory/v1alpha1/task_types.go api/factory/v1alpha1/zz_generated.deepcopy.go \
  config/crd/agents.ogenki.io_tasks.yaml charts/agent-factory/crds/agents.ogenki.io_tasks.yaml \
  internal/factory/reconciler/provenance_envtest_test.go
git commit -m "feat(api): a run record keeps what the run ran on (R10)"
```

### Task 3: The claim names its factory build, and a read returns the three claim facts

**Files:**
- Modify: `internal/factory/runs/runs.go`
- Create: `internal/factory/runs/provenance_test.go`

**Interfaces:**
- Produces: `runs.AnnFactoryVersion = "agents.ogenki.io/factory-version"`; `runs.Run.FactoryVersion`,
  `runs.Run.CompositionRevision`, `runs.Run.HarnessImage` (strings), read by Task 5's `stamp`.

- [ ] **Step 1: Write the failing test**

Create `internal/factory/runs/provenance_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Smana/agent-platform/internal/version"
)

// R10: every claim names the factory build that created it, CREATE only; a read takes the
// CompositionRevision Crossplane bound it to and the harness image the composition read.
func TestProvenanceRoundTrip(t *testing.T) {
	u := Build(spec())
	if got := u.GetAnnotations()[AnnFactoryVersion]; got != version.Version {
		t.Fatalf("factory-version = %q, want %q", got, version.Version)
	}
	image := "ghcr.io/smana/agent-harness@sha256:" + strings.Repeat("c", 64)
	_ = unstructured.SetNestedField(u.Object, "xagentruns.cloud.ogenki.io-41b6efe", "spec", "crossplane", "compositionRevisionRef", "name")
	_ = unstructured.SetNestedField(u.Object, image, "status", "harnessImage")
	r, ok := FromUnstructured(u)
	if !ok || r.FactoryVersion != version.Version || r.CompositionRevision != "xagentruns.cloud.ogenki.io-41b6efe" || r.HarnessImage != image {
		t.Fatalf("%+v", r)
	}
	if err := (Client{}).Annotate(t.Context(), "7f3cq2xz", map[string]string{AnnFactoryVersion: "v9"}); err == nil {
		t.Fatal("factory-version is CREATE only")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/factory/runs/ -run TestProvenanceRoundTrip`
Expected: FAIL to build: `undefined: AnnFactoryVersion`.

- [ ] **Step 3: Implement**

In `internal/factory/runs/runs.go`:

1. Add `"github.com/Smana/agent-platform/internal/version"` to the imports, after `".../internal/runwatch"`.
2. In the annotation constants, after `AnnChargedTo = "agents.ogenki.io/charged-to"`, add:

```go
	// The factory build that created the claim (R10): version.Version, CREATE only, so a replay
	// that finds the claim reads back the build that made it, not its own.
	AnnFactoryVersion = "agents.ogenki.io/factory-version"
```

3. In `Build`, replace `	ann := map[string]string{}` with:

```go
	ann := map[string]string{AnnFactoryVersion: version.Version}
```

   and replace

```go
	if len(ann) > 0 {
		u.SetAnnotations(ann)
	}
	return u
```

   with:

```go
	u.SetAnnotations(ann)
	return u
```

4. Replace the `Run` struct's fields up to `Created, Finished` with (this is `gofmt`'s alignment):

```go
type Run struct {
	ID, TaskID, Role, Principal, Phase, Reason, PullRequest, Revoked, Branch, RoomRef, Repository string
	Head, TaskText, RequestHash, Tier, Model, ChargedTo                                           string
	// What the run ran on (R10): the factory build that created the claim, the
	// CompositionRevision Crossplane bound it to, and the harness image the composition read
	// from its pod. Empty until known.
	FactoryVersion, CompositionRevision, HarnessImage string
	Tokens, MaxTokens, StartSeq                       int64
	Created, Finished                                 time.Time
```

   keeping the `Deleting` field and its comment as they are.

5. In `FromUnstructured`, replace

```go
		TaskText: str("spec", "task", "text"), ChargedTo: u.GetAnnotations()[AnnChargedTo]}
```

   with:

```go
		TaskText: str("spec", "task", "text"), ChargedTo: u.GetAnnotations()[AnnChargedTo],
		FactoryVersion: u.GetAnnotations()[AnnFactoryVersion], CompositionRevision: str("spec", "crossplane", "compositionRevisionRef", "name"),
		HarnessImage: str("status", "harnessImage")}
```

6. In `Annotate`, add `AnnFactoryVersion` to the CREATE-only list:

```go
	for _, k := range []string{AnnTaskURL, AnnTraceparent, AnnStartSeq, AnnHead, AnnRequestHash, AnnChargedTo, AnnFactoryVersion} {
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -count=1 ./internal/factory/runs/ ./internal/factory/api/ ./internal/factory/reconciler/ && gofmt -l internal/factory/runs`
Expected: `ok` three times (the existing `Build` and replay tests are unaffected); `gofmt -l` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/factory/runs/runs.go internal/factory/runs/provenance_test.go
git commit -m "feat(runs): every claim names the factory build that created it (R10)"
```

### Task 4: The meter hands over the models that served each run

Do this task after #155 merges and the branch is rebased (R9).

**Files:**
- Modify: `internal/factory/meter/meter.go` (`Cost`; a method before `byModel`'s comment; `Tick`)
- Modify: `internal/factory/meter/usd_test.go` (one comparison, one import)
- Create: `internal/factory/meter/provenance_test.go`

**Interfaces:**
- Produces: `meter.Cost.Served []string`: the run's served models, sorted, nil when none is named.
  Task 5's `price` reads it through `costReader.Get`.

- [ ] **Step 1: Write the failing test**

Create `internal/factory/meter/provenance_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"slices"
	"testing"

	"github.com/Smana/agent-platform/internal/factory/runs"
)

// R10: the served models are the model labels with tokens in a typed reading; a reading without
// that label, or an untyped one, names none.
func TestServedModelsAreTheReadingsModelLabels(t *testing.T) {
	for name, c := range map[string]struct {
		u    Usage
		want []string
	}{
		"two models":     {byModel(map[string]ModelUsage{"glm-5.3": {Input: 10, Output: 1}, "claude-opus-5-5": {Input: 5, Output: 1}}), []string{"claude-opus-5-5", "glm-5.3"}},
		"a zero series":  {byModel(map[string]ModelUsage{"glm-5.3": {Input: 10, Output: 1}, "glm-5.3-flash": {}}), []string{"glm-5.3"}},
		"no model label": {typedUsage(Usage{Input: 10, Output: 1}), nil},
		"untyped":        {Usage{Tokens: 11}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{standardRun("aaaaaaaa")}}
			var revoked []revocation
			var fell []string
			m := usdMeter(st, typed{"aaaaaaaa": c.u}, &revoked, &fell)
			m.Prices, m.Costs = served, &Costs{}
			if err := m.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got, _ := m.Costs.Get("aaaaaaaa"); !slices.Equal(got.Served, c.want) {
				t.Errorf("served %v, want %v", got.Served, c.want)
			}
		})
	}
}
```

(`served`, `byModel`, `typedUsage`, `standardRun`, `usdMeter`, `typed`, `store` and `revocation`
are the package's existing test helpers, in `usd_test.go` and `meter_test.go`.)

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/factory/meter/ -run TestServedModelsAreTheReadingsModelLabels`
Expected: FAIL to build: `got.Served undefined (type Cost has no field or method Served)`.

- [ ] **Step 3: Implement**

In `internal/factory/meter/meter.go`:

1. Replace

```go
type Cost struct {
	MicroUSD int64
	Why      string
}
```

   with:

```go
type Cost struct {
	MicroUSD int64
	Why      string
	// Served is the models that served the run's typed reading, by gen_ai_request_model, sorted
	// (R10); nil when the query does not group by model.
	Served []string
}
```

2. Right before the line `// byModel is the typed counters by served model, "" holding a reading
   without the model label.`, add:

```go
// served is the models with tokens in a typed reading, sorted; "" (no model label) is none.
func (u Usage) served() []string {
	var out []string
	for name, mu := range u.Models {
		if name != "" && mu != (ModelUsage{}) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

```

3. In `Tick`, right after the block that starts `if why != "" {` and sets `costs[r.ID] = Cost{Why: why}`
   (on `main` it is three lines; after #155 it also calls `m.measure`), add:

```go
		if seen && u.Typed {
			c := costs[r.ID]
			c.Served = u.served()
			costs[r.ID] = c
		}
```

- [ ] **Step 4: Fix the one comparison `Cost` no longer allows**

`go vet ./internal/factory/meter/` now fails: `invalid operation: c != (Cost{…}) (struct containing
[]string cannot be compared)`. In `internal/factory/meter/usd_test.go`, in
`TestARunIsPricedByTheModelThatServedIt`, replace

```go
			if c, _ := m.Costs.Get("aaaaaaaa"); c != (Cost{MicroUSD: want.micro}) {
```

with:

```go
			if c, _ := m.Costs.Get("aaaaaaaa"); c.MicroUSD != want.micro || c.Why != "" || !slices.Equal(c.Served, []string{model}) {
```

and add `"slices"` to that file's imports, after `"context"`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go vet ./internal/factory/meter/ && go test -count=1 ./internal/factory/meter/`
Expected: no vet output; `ok`.

- [ ] **Step 6: Commit**

```bash
git add internal/factory/meter/meter.go internal/factory/meter/usd_test.go internal/factory/meter/provenance_test.go
git commit -m "feat(meter): hand over the models that served each run (R10)"
```

### Task 5: The record takes each fact once

**Files:**
- Modify: `internal/factory/reconciler/implement.go` (import `cmp`; one line in `absorb`; two functions and a constant at the end)
- Modify: `internal/factory/reconciler/reconciler.go` (one line in `price`)
- Create: `internal/factory/reconciler/provenance_test.go`

**Interfaces:**
- Consumes: `runs.Run.{FactoryVersion,CompositionRevision,HarnessImage}` (Task 3);
  `meter.Cost.Served` (Task 4); `RunRecord.Provenance` (Task 2).
- Produces: `status.runs[].provenance`, filled; read by Task 6's `factsOf`.

- [ ] **Step 1: Write the failing test**

Create `internal/factory/reconciler/provenance_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"slices"
	"strings"
	"testing"

	"github.com/Smana/agent-platform/internal/factory/meter"
)

// R10: the record takes what the run ran on from its claim as each fact appears, never changes a
// fact once written, and gathers the models the meter saw serve it.
func TestARunRecordsWhatItRanOn(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.reconcile(t, "3buqdlot", 3)
	image := "ghcr.io/smana/agent-harness@sha256:" + strings.Repeat("c", 64)
	r := g.runs.runs[rid(0)]
	r.Phase, r.FactoryVersion, r.CompositionRevision = "Running", "v0.16.0", "xagentruns.cloud.ogenki.io-41b6efe"
	g.runs.runs[rid(0)] = r
	costs := fakeCosts{rid(0): {MicroUSD: 10, Served: []string{"glm-5.3"}}}
	g.r.Costs = costs
	p := g.reconcile(t, "3buqdlot", 1).Status.Runs[0].Provenance
	if p.FactoryVersion != "v0.16.0" || p.CompositionRevision != "xagentruns.cloud.ogenki.io-41b6efe" || p.HarnessImage != "" ||
		!slices.Equal(p.ServedModels, []string{"glm-5.3"}) {
		t.Fatalf("before the pod: %+v", p)
	}
	// The pod started; then a newer CompositionRevision re-binds the claim, and a second model serves.
	r.HarnessImage = image
	g.runs.runs[rid(0)] = r
	g.reconcile(t, "3buqdlot", 1)
	r.CompositionRevision, r.HarnessImage = "xagentruns.cloud.ogenki.io-9f00d1e", "ghcr.io/smana/agent-harness@sha256:"+strings.Repeat("d", 64)
	g.runs.runs[rid(0)] = r
	costs[rid(0)] = meter.Cost{MicroUSD: 20, Served: []string{"claude-opus-5-5", "glm-5.3"}}
	p = g.reconcile(t, "3buqdlot", 1).Status.Runs[0].Provenance
	if p.CompositionRevision != "xagentruns.cloud.ogenki.io-41b6efe" || p.HarnessImage != image ||
		!slices.Equal(p.ServedModels, []string{"claude-opus-5-5", "glm-5.3"}) {
		t.Fatalf("after: %+v", p)
	}
}
```

(`fakeCosts` is `usd_test.go`'s; `newRig`, `issueTask`, `rid` and `g.runs` are `reconciler_test.go`'s.)

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/factory/reconciler/ -run TestARunRecordsWhatItRanOn`
Expected: FAIL: `before the pod: {FactoryVersion: CompositionRevision: HarnessImage: ServedModels:[]}`.

- [ ] **Step 3: Implement**

In `internal/factory/reconciler/implement.go`, add `"cmp"` as the first import. In `absorb`, replace

```go
		if x, ok := claims[rec.ID]; ok {
			rec.Tokens = max(rec.Tokens, x.Tokens)
```

with:

```go
		if x, ok := claims[rec.ID]; ok {
			rec.Tokens = max(rec.Tokens, x.Tokens)
			stamp(&rec.Provenance, x)
```

and append to the file:

```go
// stamp fills what the run ran on from its claim (R10): each field once, never changed, so a later
// read of a claim a newer CompositionRevision re-bound still records the one the run started on.
func stamp(p *v1alpha1.RunProvenance, x runs.Run) {
	p.FactoryVersion = cmp.Or(p.FactoryVersion, x.FactoryVersion)
	p.CompositionRevision = cmp.Or(p.CompositionRevision, x.CompositionRevision)
	p.HarnessImage = cmp.Or(p.HarnessImage, x.HarnessImage)
}

// served adds the models the meter saw serve the run (R10): a set that only grows, sorted, and
// bounded by the CRD's maxItems.
func served(p *v1alpha1.RunProvenance, models []string) {
	for _, m := range models {
		if m != "" && !slices.Contains(p.ServedModels, m) && len(p.ServedModels) < maxServedModels {
			p.ServedModels = append(p.ServedModels, m)
		}
	}
	slices.Sort(p.ServedModels)
}

// maxServedModels is RunProvenance.ServedModels' maxItems.
const maxServedModels = 8
```

In `internal/factory/reconciler/reconciler.go`, in `price`, replace

```go
		if c, ok := r.Costs.Get(rec.ID); ok {
			rec.CostMicroUSD = max(rec.CostMicroUSD, c.MicroUSD)
```

with:

```go
		if c, ok := r.Costs.Get(rec.ID); ok {
			served(&rec.Provenance, c.Served)
			rec.CostMicroUSD = max(rec.CostMicroUSD, c.MicroUSD)
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -count=1 ./internal/factory/reconciler/`
Expected: `ok`.

- [ ] **Step 5: Prove the test guards write-once**

Temporarily swap the arguments of the `CompositionRevision` line in `stamp` to
`cmp.Or(x.CompositionRevision, p.CompositionRevision)` and rerun
`go test ./internal/factory/reconciler/ -run TestARunRecordsWhatItRanOn`.
Expected: FAIL with `after: {… CompositionRevision:xagentruns.cloud.ogenki.io-9f00d1e …}`. Restore the line.

- [ ] **Step 6: Commit**

```bash
git add internal/factory/reconciler/implement.go internal/factory/reconciler/reconciler.go internal/factory/reconciler/provenance_test.go
git commit -m "feat(reconciler): a run record takes what the run ran on, each fact once (R10)"
```

### Task 6: The room's facts carry the current run's provenance

**Files:**
- Modify: `internal/envelope/payloads.go` (`RunFact`; a new type after it)
- Modify: `internal/factory/reconciler/facts.go` (`factsOf`)
- Modify: `internal/factory/reconciler/provenance_test.go` (append)
- Create: `internal/bridgeapi/provenance_test.go`
- Create: `internal/summary/provenance_test.go`

**Interfaces:**
- Consumes: `RunRecord.Provenance` (Task 2).
- Produces: `envelope.ProvenanceFact{FactoryVersion, CompositionRevision, HarnessImage string; ServedModels []string}`
  and `envelope.RunFact.Provenance *ProvenanceFact` (`json:"provenance,omitempty"`), in the broker's
  `state_changed{kind: task}` and in `summary/v1`'s `status.run`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/factory/reconciler/provenance_test.go`, adding `v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"` to its imports:

```go
// R10 with ruling R6: the facts carry the current run's provenance, and no provenance key at all
// while it is empty, so a task whose runs predate the field hashes as before.
func TestFactsCarryTheCurrentRunsProvenance(t *testing.T) {
	tk := issueTask("3buqdlot", 7, "x")
	tk.Status.Phase = v1alpha1.PhaseImplementing
	tk.Status.Runs = []v1alpha1.RunRecord{{ID: rid(0), Role: "implementer", Trigger: "initial"}}
	if f := factsOf(tk); f.Run.Provenance != nil {
		t.Fatalf("no fact yet, no provenance: %+v", f.Run.Provenance)
	}
	tk.Status.Runs[0].Provenance = v1alpha1.RunProvenance{FactoryVersion: "v0.16.0", ServedModels: []string{"glm-5.3"}}
	f := factsOf(tk)
	if p := f.Run.Provenance; p == nil || p.FactoryVersion != "v0.16.0" || !slices.Equal(p.ServedModels, []string{"glm-5.3"}) {
		t.Fatalf("%+v", f.Run)
	}
}
```

Create `internal/bridgeapi/provenance_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

// R10: the broker stores a run's provenance with the task's facts. Before this change its strict
// decoder answered 400 bad_message.
func TestTaskFactsCarryARunsProvenance(t *testing.T) {
	s, log, _ := newServer(t)
	body := []byte(`{"clientSeq":7,"facts":{"phase":"Implementing","run":{"id":"cf4ato2x","role":"implementer",` +
		`"provenance":{"factoryVersion":"v0.16.0","servedModels":["glm-5.3"]}}}}`)
	rec := call(t, s.Routes(), http.MethodPost, "/v1/rooms/"+room+"/task", "sys:"+factory, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	evs, _ := log.Range(t.Context(), room, 0, 10)
	var p struct {
		Run struct {
			Provenance struct {
				FactoryVersion string   `json:"factoryVersion"`
				ServedModels   []string `json:"servedModels"`
			} `json:"provenance"`
		} `json:"run"`
	}
	if len(evs) != 1 {
		t.Fatalf("events %+v", evs)
	}
	if err := json.Unmarshal(evs[0].Payload, &p); err != nil || p.Run.Provenance.FactoryVersion != "v0.16.0" ||
		len(p.Run.Provenance.ServedModels) != 1 {
		t.Fatalf("payload %s: %v", evs[0].Payload, err)
	}
}
```

Create `internal/summary/provenance_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package summary_test

import (
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/summary"
)

// R10: the room's summary shows the current run's provenance as the factory posted it.
func TestTheSummaryShowsTheRunsProvenance(t *testing.T) {
	factory := envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"}
	facts := envelope.TaskFacts{Phase: "Implementing", Run: &envelope.RunFact{ID: "cf4ato2x", Role: "implementer",
		Provenance: &envelope.ProvenanceFact{FactoryVersion: "v0.16.0", CompositionRevision: "xagentruns.cloud.ogenki.io-41b6efe"}}}
	s := summary.View(summary.Fold([]envelope.Event{ev(1, envelope.StateChanged, factory, envelope.TaskStatePayload(facts))}),
		"r", "u", owner(), 0, time.Now())
	if s.Status.Run == nil || s.Status.Run.Provenance == nil || s.Status.Run.Provenance.FactoryVersion != "v0.16.0" ||
		s.Status.Run.Provenance.CompositionRevision != "xagentruns.cloud.ogenki.io-41b6efe" {
		t.Fatalf("%+v", s.Status.Run)
	}
}
```

(`ev` and `owner` are `summary_test.go`'s helpers; `newServer`, `call`, `room` and `factory` are
`server_test.go`'s.)

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/factory/reconciler/ ./internal/summary/ ./internal/bridgeapi/ -run 'Provenance'`
Expected: the reconciler and summary packages FAIL to build (`f.Run.Provenance undefined`,
`unknown field Provenance in struct literal of type envelope.RunFact`); bridgeapi FAILS with
`code 400: {"error":"bad_message"}`.

- [ ] **Step 3: Implement**

In `internal/envelope/payloads.go`, replace

```go
type RunFact struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	Trigger   string    `json:"trigger,omitempty"`
	StartedAt time.Time `json:"startedAt,omitzero"`
}
```

with:

```go
type RunFact struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	Trigger   string    `json:"trigger,omitempty"`
	StartedAt time.Time `json:"startedAt,omitzero"`
	// What the run ran on (R10), as far as the factory knows it yet.
	Provenance *ProvenanceFact `json:"provenance,omitempty"`
}

// ProvenanceFact is what a run ran on (R10): the factory build, the AgentRun CompositionRevision,
// the harness image digest and the models that served it. Each empty until known.
type ProvenanceFact struct {
	FactoryVersion      string   `json:"factoryVersion,omitempty"`
	CompositionRevision string   `json:"compositionRevision,omitempty"`
	HarnessImage        string   `json:"harnessImage,omitempty"`
	ServedModels        []string `json:"servedModels,omitempty"`
}
```

In `internal/factory/reconciler/facts.go`, in `factsOf`, right after

```go
		f.Run = &envelope.RunFact{ID: run.ID, Role: run.Role, Trigger: run.Trigger}
```

add:

```go
		if p := run.Provenance; p.FactoryVersion != "" || p.CompositionRevision != "" || p.HarnessImage != "" || len(p.ServedModels) > 0 {
			f.Run.Provenance = &envelope.ProvenanceFact{FactoryVersion: p.FactoryVersion, CompositionRevision: p.CompositionRevision,
				HarnessImage: p.HarnessImage, ServedModels: p.ServedModels}
		}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -count=1 ./internal/envelope/... ./internal/factory/... ./internal/summary/... ./internal/bridgeapi/... ./internal/humanapi/...`
Expected: `ok` for each; `TestFoldGolden`'s goldens are unchanged (no golden carries provenance).

- [ ] **Step 5: Commit**

```bash
git add internal/envelope/payloads.go internal/factory/reconciler/facts.go internal/factory/reconciler/provenance_test.go \
  internal/bridgeapi/provenance_test.go internal/summary/provenance_test.go
git commit -m "feat(facts): the room's task facts carry the current run's provenance (R10)"
```

### Task 7: Docs, then the whole gate

**Files:**
- Modify: `docs/factory.md` (the `status.runs[]` row of the Task reference table)
- Modify: `docs/event-envelope.md` (the `task` row of the `state_changed` kinds)
- Modify: `docs/integration.md` (the crossplane-configuration table)

- [ ] **Step 1: The Task reference**

In `docs/factory.md`, replace the row

```markdown
| `status.runs[]` | Each run: `id`, `role`, `trigger` (`initial`, `review`, `human`, `ci`, `retry`, `resume`), `round`, `phase`, `reason`, `verdict`, `tokens`, `costMicroUSD`, `unpriced`, `startSeq`, `headSHA`, `started`, `finished` |
```

with:

```markdown
| `status.runs[]` | Each run: `id`, `role`, `trigger` (`initial`, `review`, `human`, `ci`, `retry`, `resume`), `round`, `phase`, `reason`, `verdict`, `tokens`, `costMicroUSD`, `unpriced`, `startSeq`, `headSHA`, `started`, `finished`, and `provenance`, what it ran on: `factoryVersion` (the claim's CREATE-only `agents.ogenki.io/factory-version`), `compositionRevision` (the claim's `spec.crossplane.compositionRevisionRef.name`), `harnessImage` (the claim's `status.harnessImage`, the harness container's `imageID`) and `servedModels` (the meter's `gen_ai_request_model`, at most 8). Each is written once, as it becomes known; empty means unknown |
```

- [ ] **Step 2: The facts' shape**

In `docs/event-envelope.md`, in the `task` row, replace `run?{id, role, trigger?, startedAt?}` with
`run?{id, role, trigger?, startedAt?, provenance?{factoryVersion?, compositionRevision?, harnessImage?, servedModels?}}`.

- [ ] **Step 3: The composition's contract**

In `docs/integration.md`, in the crossplane-configuration table, after the row that starts
`| \`AgentRun\`, the harness |`, add:

```markdown
| `AgentRun`, provenance | `status.harnessImage`: the `harness` container's `imageID` from the run's pod, only as `<repository>@sha256:<digest>`, kept once set. The factory records it per run (R10); without it the record's `harnessImage` stays empty |
```

- [ ] **Step 4: Run the gate**

Run:
```bash
task lint && task crd:check && task docs 2>&1 | tail -3
export KUBEBUILDER_ASSETS="$(go tool setup-envtest use -p path 1.36.x)"
go test -race -count=1 ./api/... ./internal/envelope/... ./internal/factory/... ./internal/summary/... ./internal/bridgeapi/... ./internal/humanapi/... 2>&1 | tail -25
```
Expected: `0 issues.`; `crd:check` with no diff; `docs` passes; every package `ok`. With Docker
running, `task check 2>&1 | tee $SCRATCH/check-provenance.log` exits 0; without it, CI's `check`
job is the proof.

- [ ] **Step 5: Commit**

```bash
git add docs/factory.md docs/event-envelope.md docs/integration.md
git commit -m "docs: a run's provenance on the Task, in the room's facts, and from the composition (R10)"
```

---

## Part C: releases and pins

### Task 8: Ship it

Each step is its own pull request in its own repository; none merges before the one it depends on
is released.

- [ ] **Step 1: crossplane-configuration.** Open the pull request for Task 1; after it merges, tag
  `v0.9.16`, the patch after `v0.9.15` (the latest tag on 2026-10-22; take the next free patch if
  another release landed first): `git tag v0.9.16 && git push origin v0.9.16` (its CLAUDE.md,
  "Releasing"). The release workflow re-runs `task check` with Docker.
- [ ] **Step 2: agent-platform.** Rebase onto `origin/main` after #155 (R9), push
  `feat/run-provenance`, open the pull request; CI's `check` and `analyze` must pass. Release per
  OP3: in the release that carries #155 if both are merged when it is cut, else the next patch.
- [ ] **Step 3: cloud-native-ref (host).** Pin Step 1's version (`v0.9.15` → `v0.9.16`) in
  `infrastructure/base/crossplane/configuration-core/configuration-core.yaml`,
  `infrastructure/base/crossplane/configuration-aws/configuration-packages.yaml`,
  `infrastructure/base/crossplane/configuration-gcp/configuration-packages.yaml`, and the
  `fetch-crossplane-configuration` clone tag in `apps/platform/app-wizard/app.yaml`, in one commit.
- [ ] **Step 4: ai-native-ref.** Pin Step 2's release in one commit, factory and broker together
  (`flux/sources/ocirepo-agent-factory.yaml`, `tooling/base/agent-factory/helm-values-configmap.yaml`,
  `infrastructure/base/room-broker/{app,retention-cronjob,crd-rooms,sqlinstance}.yaml`;
  `test-agent-factory-pins` holds them equal): a factory ahead of its broker would retry its facts
  until the broker rolls. Move `HOST_REF` past Step 3. Mark R10 done in
  `website/content/docs/platform/ai-platform/status.md` (item 5 of the roadmap's "Before v3") and
  add a status line to the spec. `task check` exits 0.
- [ ] **Step 5: The live proofs** join the next gcp-0 session's checklist: the spec's live proofs
  1–3.
