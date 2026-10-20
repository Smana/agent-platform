#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# task quickstart: kind + Cilium + gVisor + cert-manager + CNPG + agent-platform. Safe to re-run.
# Env: MODEL_API_KEY GITHUB_TOKEN REPO (required); MODEL MODEL_BASE_URL CHART_REF AP_VERSION;
#      ALLOW_UNSANDBOXED=1 (run agents under the default runtime, no gVisor);
#      UP_HELM_ARGS (extra `helm upgrade --install` args for the chart, word-split; setting any
#      image tag there turns off the release-image lookup).
# Prerequisites: docker (with buildx) kind helm kubectl curl git openssl envsubst base32 yq.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=hack/quickstart/versions.env
. hack/quickstart/versions.env
step() { printf '\n==> %s\n' "$*"; }
need() { command -v "$1" >/dev/null || { echo "missing tool: $1" >&2; exit 1; }; }
for t in docker kind helm kubectl curl git openssl envsubst base32 yq; do need "$t"; done
for v in MODEL_API_KEY GITHUB_TOKEN REPO; do [ -n "${!v:-}" ] || { echo "set $v" >&2; exit 1; }; done
[[ "$REPO" =~ ^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$ ]] || { echo "REPO is owner/name" >&2; exit 1; }
MODEL="${MODEL:-claude-sonnet-5-5}"
NS=agent-platform
CHART="${CHART_REF:-charts/agent-platform}"
read -ra extra <<<"${UP_HELM_ARGS:-}"

# --- Preflight: everything that can fail in seconds fails before the cluster exists. ---
if [[ -d $CHART ]]; then
  step "chart dependencies"
  helm dependency build "$CHART" >/dev/null # charts/ and Chart.lock are gitignored
fi

chart_args=(-f charts/agent-platform/values-quickstart.yaml --set createRunsNamespace=false)
if [[ ${ALLOW_UNSANDBOXED:-} == 1 ]]; then
  # runctl uses runtimeClassName whenever it is non-empty; allowUnsandboxed only counts when it is empty.
  chart_args+=(--set runController.allowUnsandboxed=true --set runController.runtimeClassName= --set runController.createRuntimeClass=false)
fi
if [[ ${UP_HELM_ARGS:-} != *image*tag* ]]; then
  step "release images"
  AP_VERSION="${AP_VERSION:-$(curl -fsSL https://api.github.com/repos/Smana/agent-platform/releases/latest | yq -p=json '.tag_name')}"
  digest() { # image → <version>@sha256:…
    local d
    d=$(docker buildx imagetools inspect "ghcr.io/smana/$1:$AP_VERSION" --format '{{json .Manifest.Digest}}' | tr -d '"')
    [[ $d == sha256:* ]] || { echo "no digest for ghcr.io/smana/$1:$AP_VERSION" >&2; exit 1; }
    echo "$AP_VERSION@$d"
  }
  chart_args+=(--set "broker.image.tag=$(digest room-broker)" --set "runController.image.tag=$(digest run-controller)"
    --set "runController.images.harness.tag=$(digest agent-harness)" --set "runController.images.bridge.tag=$(digest room-bridge)")
fi
helm template ap "$CHART" -n "$NS" "${chart_args[@]}" "${extra[@]}" >/dev/null

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
  --set ipam.mode=kubernetes --set operator.replicas=1

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
kubectl -n agents create secret generic agent-model --from-literal=api_key="$MODEL_API_KEY" --from-literal=model="$MODEL" \
  ${MODEL_BASE_URL:+--from-literal=base_url="$MODEL_BASE_URL"} --dry-run=client -o yaml | kubectl apply -f -
kubectl -n agents create secret generic agent-git --from-literal=token="$GITHUB_TOKEN" --dry-run=client -o yaml | kubectl apply -f -

step "agent-platform"
helm upgrade --install ap "$CHART" -n "$NS" "${chart_args[@]}" "${extra[@]}" --wait --timeout 10m

cat <<MSG

Ready. This shell may not point at the cluster; in another terminal:
  export KUBECONFIG=$PWD/.quickstart/kubeconfig
  kubectl -n $NS port-forward deploy/room-broker 8080:8080
then open http://localhost:8080 and start a run:
  task quickstart:run TASK='Fix a typo in the README' REPO=$REPO
MSG
