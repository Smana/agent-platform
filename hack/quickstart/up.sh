#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# task quickstart: kind + Cilium + gVisor + cert-manager + CNPG + agent-platform. Safe to re-run.
# Env: MODEL_API_KEY GITHUB_TOKEN REPO (required); MODEL MODEL_BASE_URL ALLOW_UNSANDBOXED CHART_REF
# UP_HELM_ARGS (extra `helm upgrade --install` args for the chart, word-split; e.g. CI image overrides).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=hack/quickstart/versions.env
. hack/quickstart/versions.env
step() { printf '\n==> %s\n' "$*"; }
need() { command -v "$1" >/dev/null || { echo "missing tool: $1" >&2; exit 1; }; }
for t in docker kind helm kubectl openssl; do need "$t"; done
for v in MODEL_API_KEY GITHUB_TOKEN REPO; do [ -n "${!v:-}" ] || { echo "set $v" >&2; exit 1; }; done
[[ "$REPO" =~ ^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$ ]] || { echo "REPO is owner/name" >&2; exit 1; }
MODEL="${MODEL:-claude-sonnet-5-5}"
NS=agent-platform

step "gVisor binaries ($GVISOR_RELEASE)"
hack/quickstart/fetch-runsc.sh

step "kind cluster"
# Relative hostPaths in kind-config.yaml resolve from here (the repo root).
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
kubectl apply -n "$NS" -f hack/quickstart/manifests/ca-issuer.yaml.tmpl
kubectl -n "$NS" wait --for=condition=Ready issuer/quickstart-ca --timeout=120s

step "CloudNativePG $CNPG_VERSION"
kubectl apply --server-side -f "https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-${CNPG_VERSION%.*}/releases/cnpg-${CNPG_VERSION}.yaml"
kubectl -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=180s
hack/quickstart/db.sh "$NS"

step "secrets"
# up.sh owns the runs namespace (the Secrets must exist before the chart's controller reads them),
# so the chart installs with createRunsNamespace=false: Helm cannot adopt a namespace it did not create.
kubectl create namespace agents --dry-run=client -o yaml | kubectl apply -f -
kubectl -n agents create secret generic agent-model --from-literal=api_key="$MODEL_API_KEY" --from-literal=model="$MODEL" \
  ${MODEL_BASE_URL:+--from-literal=base_url="$MODEL_BASE_URL"} --dry-run=client -o yaml | kubectl apply -f -
kubectl -n agents create secret generic agent-git --from-literal=token="$GITHUB_TOKEN" --dry-run=client -o yaml | kubectl apply -f -

step "agent-platform"
# shellcheck disable=SC2086 # UP_HELM_ARGS is deliberately word-split
helm upgrade --install ap "${CHART_REF:-charts/agent-platform}" -n "$NS" -f charts/agent-platform/values-quickstart.yaml \
  --set createRunsNamespace=false \
  ${ALLOW_UNSANDBOXED:+--set runController.allowUnsandboxed=true} ${UP_HELM_ARGS:-} --wait --timeout 10m

cat <<MSG

Ready. In another terminal:
  kubectl -n $NS port-forward deploy/room-broker 8080:8080
then open http://localhost:8080 and start a run:
  task quickstart:run TASK='Fix a typo in the README'
MSG
