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

## Live check (to run in `quickstart-e2e`)

```bash
. hack/quickstart/versions.env && hack/quickstart/fetch-runsc.sh
kind create cluster --config hack/quickstart/kind-config.yaml --image "$KIND_NODE_IMAGE"
# install Cilium, then a RuntimeClass{handler: runsc} and a pod running `dmesg` under it
# pass: pod Succeeded and its log shows gVisor's banner, not a Linux kernel log
```

## Prerequisites (`task quickstart`)

`docker` (with buildx), `kind`, `helm`, `kubectl`, `curl`, `git`, `openssl`, `envsubst`, `base32`, `yq`
(mikefarah v4); `fetch-runsc.sh` also needs `sha512sum` and `bzip2`. `up.sh` checks the list before it
does anything. Images default to the latest `Smana/agent-platform` release (`AP_VERSION` overrides);
`UP_HELM_ARGS` with your own `*.image.tag` values turns the lookup off. Cluster access is pinned to
`.quickstart/kubeconfig`: `export KUBECONFIG=$PWD/.quickstart/kubeconfig` in your own shell.

## Starting a run

`RUN_PROMPT='Fix a typo in the README' REPO=owner/name task quickstart:run`. Pass both in the environment,
not as `task … NAME=value`: go-task runs those values through Go templating (`{{ … }}` is mangled).

## CI: `quickstart-e2e`

Needs two Actions secrets: `QUICKSTART_REPO` (a throwaway `owner/name`) and `QUICKSTART_GITHUB_TOKEN`
(a fine-grained PAT scoped to that repo, Contents read/write). The repo needs a `main` branch with at
least one commit: the harness clones it and branches from `main`, which fails on an empty repo.
Without the secrets (fork PRs) the job skips. A run's model is the stub (`stub-model/`); its
`base_url` must be `http://<svc>.<ns>.svc.<cluster-domain>:<port>` with the port equal to the
Service's `targetPort` (the run's network policy sees the backend port).
