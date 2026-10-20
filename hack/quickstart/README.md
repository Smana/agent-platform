# Quickstart spike: gVisor inside kind

**Question:** can a kind node (a Docker container) run pods under gVisor (`runsc`), so the quickstart
sandboxes agent workloads the same way a real cluster does?

**Status:** artifacts built and verified statically; the live proof is the `quickstart-e2e` CI job
(GitHub runners have Docker). Nothing below marked "pending" has been run yet.

## Pinned versions (`versions.env`)

| Component | Version | Released | Why |
|---|---|---|---|
| kind | v0.33.0 | 2026-08-26 | Newest stable release at least two weeks old |
| node image | `kindest/node:v1.37.0@sha256:a1ed56cf…` | with kind v0.33.0 | Newest Kubernetes minor in that release's notes; digest pinned as the notes require |
| gVisor | 20260928.0 | 2026-09-30 | Newest `YYYYMMDD.N` at least two weeks old (20261005.0 and 20261012.0 are too recent) |
| Cilium / cert-manager / CNPG | 1.20.3 / v1.21.2 / 1.30.1 | 2026-10-15 / 2026-09-11 / 2026-09-23 | cloud-native-ref's pins, so both repos agree |

## What the spike changed from the plan

- **gVisor publishes a tarball, not bare binaries.** `…/<release>/<arch>/runsc` returns 404 for recent
  releases; `gvisor.tar.bz2` (+ `.sha512`) holds `runsc`, `containerd-shim-runsc-v1` and a `gvisor-bin/`
  directory. `fetch-runsc.sh` verifies the tarball's sha512 and extracts it.
- **Containerd plugin key is `io.containerd.grpc.v1.cri`, not `io.containerd.cri.v1.runtime`.**
  Evidence (no Docker needed), kind v0.33.0:
  - `images/base/Dockerfile`: `CONTAINERD_VERSION="v2.3.4"`, runc v1.4.3.
  - `images/base/files/etc/containerd/config.toml` starts with `version = 2` and configures
    `[plugins."io.containerd.grpc.v1.cri"…]`.
  - kind merges `containerdConfigPatches` into that file, so a patch in the v3-only key would be ignored.
    The comment in `kind-config.yaml` repeats this.

## Platforms

| Platform | Result |
|---|---|
| Linux + Docker (amd64, arm64) | pending live run (`quickstart-e2e`) |
| macOS + Docker Desktop | untested |
| Podman (rootful) | untested |

`gvisor-bin/` is required: runsc 20260928.0 looks up its sidecars there, next to itself (its embedded
fallback stops working after 2026-10). `fetch-runsc.sh` extracts it and `kind-config.yaml` mounts it at
`/usr/local/bin/gvisor-bin`.

## Escape hatch

If a platform cannot run gVisor, `ALLOW_UNSANDBOXED=1` (exactly `1`; `up.sh`) clears the chart's `runtimeClassName`, skips the
`gvisor` RuntimeClass and runs agents under the default runtime. The environment is then **not
sandboxed**; use it only on a machine you trust with the agent's code.

## Live check

`up.sh` runs it on every install (skipped under `ALLOW_UNSANDBOXED=1`): `manifests/gvisor-probe.yaml.tmpl`
is a run-shaped pod under `runsc` that must print `ok:dmesg` (gVisor's own kernel log), `ok:dns` (the
broker's FQDN resolves) and `ok:tcp` (the ClusterIP :8443 answers, which needs Cilium
`socketLB.hostNamespaceOnly`). Otherwise `up.sh` stops, names the failed check and offers
`ALLOW_UNSANDBOXED=1`. `quickstart-e2e` runs `up.sh`, so CI exercises it.

Prerequisites, starting a run, the CI secrets and the model-URL rule are in
[`docs/quickstart.md`](../../docs/quickstart.md), which owns them.
