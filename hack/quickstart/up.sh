#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# task quickstart: kind + Cilium + gVisor + cert-manager + CNPG + maku. Safe to re-run.
# Env: MODEL_API_KEY GITHUB_TOKEN REPO (required); MODEL MODEL_BASE_URL CHART_REF AP_VERSION;
#      LOCAL_PORT (default 8080: the port you forward; the broker's publicURL follows it);
#      ROOMCTL (a maku binary to use instead of the release's; required when UP_HELM_ARGS sets the image tags);
#      ALLOW_UNSANDBOXED=1 (exactly 1: run agents under the default runtime, no gVisor; each run's room
#      then shows a Not sandboxed banner);
#      UP_HELM_ARGS (extra `helm upgrade --install` args for the chart, word-split; setting any
#      image tag there turns off the release-image lookup).
# Prerequisites: Linux; docker (with buildx) kind helm kubectl (within one minor of the kind node) curl git openssl envsubst yq sha256sum sha512sum bzip2 tar.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=hack/quickstart/versions.env
. hack/quickstart/versions.env
# shellcheck source=hack/quickstart/lib.sh
. hack/quickstart/lib.sh
qs_platform_ok "$(uname -s)" || exit 1
step() { printf '\n==> %s\n' "$*"; }
need() { command -v "$1" >/dev/null || { echo "missing tool: $1" >&2; exit 1; }; }
for t in docker kind helm kubectl curl git openssl envsubst yq sha256sum sha512sum bzip2 tar; do need "$t"; done
for v in MODEL_API_KEY GITHUB_TOKEN REPO; do [ -n "${!v:-}" ] || { echo "set $v" >&2; exit 1; }; done
[[ "$REPO" =~ ^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$ ]] || { echo "REPO is owner/name" >&2; exit 1; }
unsandboxed=$(qs_unsandboxed "${ALLOW_UNSANDBOXED:-}") || exit 1
LOCAL_PORT="${LOCAL_PORT:-8080}"
qs_port_ok "$LOCAL_PORT" || exit 1
qs_skew_ok "$(kubectl version --client -o json | yq -p=json '.clientVersion.minor')" "$(qs_node_minor "$KIND_NODE_IMAGE")" || exit 1
MODEL="${MODEL:-claude-sonnet-5-5}"
NS=maku
CHART="${CHART_REF:-charts/maku}"
read -ra extra <<<"${UP_HELM_ARGS:-}"

# --- Preflight: everything that can fail in seconds fails before the cluster exists. ---

chart_args=(-f charts/maku/values-quickstart.yaml --set createRunsNamespace=false --set "broker.publicURL=http://localhost:$LOCAL_PORT")
if [[ $unsandboxed == 1 ]]; then
  # runctl uses runtimeClassName whenever it is non-empty; allowUnsandboxed only counts when it is empty.
  chart_args+=(--set runController.allowUnsandboxed=true --set runController.runtimeClassName= --set runController.createRuntimeClass=false)
fi
if [[ ${UP_HELM_ARGS:-} != *image*tag* ]]; then
  step "release images"
  docker buildx version >/dev/null 2>&1 || { echo "the docker buildx plugin is required (it resolves image digests)" >&2; exit 1; }
  if [[ -z ${AP_VERSION:-} ]]; then
    AP_VERSION=$(curl -fsSL https://api.github.com/repos/Smana/maku/releases/latest | yq -p=json '.tag_name') ||
      { echo "could not read the latest release from the GitHub API (unauthenticated limit: 60 requests/hour); set AP_VERSION=<release tag>" >&2; exit 1; }
    [[ -n $AP_VERSION && $AP_VERSION != null ]] || { echo "no release found; set AP_VERSION=<release tag>" >&2; exit 1; }
  fi
  digest() { # image → <version>@sha256:…
    local d
    d=$(docker buildx imagetools inspect "ghcr.io/smana/$1:$AP_VERSION" --format '{{json .Manifest.Digest}}' | tr -d '"')
    [[ $d == sha256:* ]] || { echo "no digest for ghcr.io/smana/$1:$AP_VERSION" >&2; exit 1; }
    echo "$AP_VERSION@$d"
  }
  # One assignment each: a failure inside a substitution embedded in an array only counts for the last one.
  broker_tag=$(digest maku-broker)
  controller_tag=$(digest maku-run-controller)
  harness_tag=$(digest maku-harness)
  bridge_tag=$(digest maku-bridge)
  chart_args+=(--set "broker.image.tag=$broker_tag" --set "runController.image.tag=$controller_tag"
    --set "runController.images.harness.tag=$harness_tag" --set "runController.images.bridge.tag=$bridge_tag")
fi
helm template ap "$CHART" -n "$NS" "${chart_args[@]}" "${extra[@]}" >/dev/null

step "maku"
mkdir -p .quickstart/bin
if [[ -n ${ROOMCTL:-} ]]; then
  install -m 0755 "$ROOMCTL" .quickstart/bin/maku
elif [[ -n ${AP_VERSION:-} ]]; then
  qs_fetch_roomctl "$AP_VERSION" .quickstart/bin/maku
else
  echo "set ROOMCTL=<a maku built from this checkout>: UP_HELM_ARGS sets the images, so there is no release to take maku from" >&2
  exit 1
fi
.quickstart/bin/maku help | grep -q '^  run --repo' ||
  { echo ".quickstart/bin/maku has no run command: use a release that has it (AP_VERSION) or ROOMCTL" >&2; exit 1; }

step "gVisor binaries ($GVISOR_RELEASE)"
hack/quickstart/fetch-runsc.sh

step "kind cluster"
# Relative hostPaths in kind-config.yaml resolve from here (the repo root).
clusters=$(kind get clusters 2>/dev/null || true)
grep -qx "$KIND_CLUSTER" <<<"$clusters" || kind create cluster --config hack/quickstart/kind-config.yaml --image "$KIND_NODE_IMAGE"
# Pin the context: a re-run must never helm-upgrade whatever cluster kubectl currently points at.
kind export kubeconfig --name "$KIND_CLUSTER" --kubeconfig "$PWD/.quickstart/kubeconfig"
export KUBECONFIG="$PWD/.quickstart/kubeconfig"

step "Cilium $CILIUM_VERSION"
# socketLB.hostNamespaceOnly: gVisor's netstack never calls connect() in the host kernel, so socket-LB
# cannot translate a ClusterIP for a run pod (no DNS, no broker). Per-packet LB must stay on.
helm upgrade --install cilium cilium --repo https://helm.cilium.io --version "$CILIUM_VERSION" -n kube-system --wait \
  --set kubeProxyReplacement=true --set socketLB.hostNamespaceOnly=true \
  --set k8sServiceHost="${KIND_CLUSTER}-control-plane" --set k8sServicePort=6443 \
  --set ipam.mode=kubernetes --set operator.replicas=1 --set hubble.enabled=true # hubble: quickstart-e2e's drop diagnostics

step "cert-manager $CERT_MANAGER_VERSION"
helm upgrade --install cert-manager cert-manager --repo https://charts.jetstack.io --version "$CERT_MANAGER_VERSION" \
  -n cert-manager --create-namespace --wait --set crds.enabled=true
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -n "$NS" -f hack/quickstart/manifests/ca-issuer.yaml.tmpl
kubectl -n "$NS" wait --for=condition=Ready issuer/quickstart-ca --timeout=120s

step "CloudNativePG $CNPG_VERSION"
kubectl apply --server-side -f "https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-${CNPG_VERSION%.*}/releases/cnpg-${CNPG_VERSION}.yaml"
kubectl -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=180s
hack/quickstart/db.sh "$NS"

step "secrets"
# up.sh owns the runs namespace (the Secrets must exist before the controller reads them), so the
# chart installs with createRunsNamespace=false: Helm cannot adopt a namespace it did not create.
kubectl create namespace agents --dry-run=client -o yaml | kubectl apply -f -
export MODEL
model_keys=(api_key=MODEL_API_KEY model=MODEL)
[[ -z ${MODEL_BASE_URL:-} ]] || model_keys+=(base_url=MODEL_BASE_URL)
qs_secret agents agent-model Opaque "${model_keys[@]}" | kubectl apply -f -
qs_secret agents agent-git Opaque token=GITHUB_TOKEN | kubectl apply -f -

step "maku"
helm upgrade --install ap "$CHART" -n "$NS" "${chart_args[@]}" "${extra[@]}" --wait --timeout 10m

if [[ $unsandboxed == 0 ]]; then
  step "gVisor probe"
  # A run-shaped pod: proves runsc runs, DNS resolves the broker, and the ClusterIP answers from inside the sandbox.
  export RUNS_NS=agents NS PROBE_IMAGE CLUSTER_DOMAIN=cluster.local
  # shellcheck disable=SC2016 # envsubst takes a literal variable list
  probe() { envsubst '${RUNS_NS} ${NS} ${PROBE_IMAGE} ${CLUSTER_DOMAIN}' <hack/quickstart/manifests/gvisor-probe.yaml.tmpl; }
  cleanup_probe() { probe | kubectl delete --ignore-not-found --wait=false -f - >/dev/null 2>&1 || true; }
  cleanup_probe
  probe | kubectl apply -f -
  phase=
  for _ in $(seq 90); do # 180 s; stop at the first verdict, a failed check should not wait out the clock
    phase=$(kubectl -n agents get pod ap-probe -o jsonpath='{.status.phase}')
    [[ $phase == Succeeded || $phase == Failed ]] && break
    sleep 2
  done
  if [[ $phase != Succeeded ]]; then
    echo "gVisor probe FAILED. What it saw:" >&2
    kubectl -n agents logs ap-probe >&2 || true
    kubectl -n agents get events --field-selector involvedObject.name=ap-probe >&2 || true
    cleanup_probe
    echo "Runs cannot work on this host. Fix the cause above, or ALLOW_UNSANDBOXED=1 to run agents under the default runtime (no gVisor)." >&2
    exit 1
  fi
  kubectl -n agents logs ap-probe
  cleanup_probe
fi

if [[ $unsandboxed == 1 ]]; then
  printf '\n!!! NOT SANDBOXED: ALLOW_UNSANDBOXED=1. Agents run under the default runtime with your API key and PAT;\n!!! every such run shows a "Not sandboxed" banner in its room.\n' >&2
fi
cat <<MSG

Ready. In a second terminal, keep the broker forwarded (local mode answers this port only):
  export KUBECONFIG=$PWD/.quickstart/kubeconfig
  kubectl -n $NS port-forward deploy/room-broker $LOCAL_PORT:8080
Then, here (XDG_CONFIG_HOME keeps maku's quickstart config apart from any other):
  export KUBECONFIG=$PWD/.quickstart/kubeconfig XDG_CONFIG_HOME=$PWD/.quickstart/config PATH=$PWD/.quickstart/bin:\$PATH
  maku configure --url http://localhost:$LOCAL_PORT
  maku run --repo $REPO --task 'Fix a typo in the README'
The room page: http://localhost:$LOCAL_PORT
MSG
