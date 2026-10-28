#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# quickstart-e2e: this checkout's images and roomctl in kind, a stub model, one run to Succeeded through roomctl run.
# Env: GITHUB_TOKEN REPO (the throwaway repo the harness clones). `.github/workflows/quickstart-e2e.yaml`.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
# shellcheck source=hack/quickstart/versions.env
. hack/quickstart/versions.env
: "${GITHUB_TOKEN:?}" "${REPO:?}"
step() { printf '\n==> %s\n' "$*"; }
export KUBECONFIG="$PWD/.quickstart/kubeconfig"
TAG=e2e
STUB_URL=http://stub-model.e2e.svc.cluster.local:8080/v1

dump() {
  echo "--- e2e failed: diagnostics ---" >&2
  kubectl get agentrun,rooms,pods -A -o wide >&2 || true
  kubectl get events -A --sort-by=.lastTimestamp >&2 || true
  kubectl get cnp -A -o yaml >&2 || true
  for ns in agent-platform agents e2e; do kubectl -n "$ns" describe pods >&2 || true; done
  kubectl -n agents describe agentrun >&2 || true
  kubectl -n agent-platform logs deploy/run-controller --tail=200 >&2 || true
  kubectl -n agent-platform logs deploy/room-broker --tail=200 >&2 || true
  for p in $(kubectl -n agents get pods -o name 2>/dev/null); do
    kubectl -n agents logs "$p" --all-containers --prefix --tail=200 >&2 || true
  done
  kubectl -n e2e logs deploy/stub-model --tail=50 >&2 || true
  cat .quickstart/e2e/port-forward.log >&2 || true
  # The agent's own ring buffer: drops that happened minutes ago, not a live stream.
  kubectl -n kube-system exec ds/cilium -c cilium-agent -- hubble observe --verdict DROPPED --last 200 >&2 || true
}
onexit() {
  local rc=$?
  [[ -z ${pf:-} ]] || kill "$pf" 2>/dev/null || true
  [[ $rc -eq 0 ]] || dump
  exit "$rc"
}
trap onexit EXIT

step "build the images"
docker build -t "ghcr.io/smana/agent-harness:$TAG" images/agent-harness
for i in room-broker room-bridge run-controller; do
  docker build -f "images/$i/Dockerfile" -t "ghcr.io/smana/$i:$TAG" .
done
docker build -f hack/quickstart/stub-model/Dockerfile -t "ghcr.io/smana/stub-model:$TAG" .
mkdir -p .quickstart/e2e && go build -o .quickstart/e2e/roomctl ./cmd/roomctl

step "cluster and image load"
hack/quickstart/fetch-runsc.sh
clusters=$(kind get clusters 2>/dev/null || true)
grep -qx "$KIND_CLUSTER" <<<"$clusters" || kind create cluster --config hack/quickstart/kind-config.yaml --image "$KIND_NODE_IMAGE"
for i in agent-harness room-broker room-bridge run-controller stub-model; do
  kind load docker-image --name "$KIND_CLUSTER" "ghcr.io/smana/$i:$TAG"
  # CI only: the runner's disk is tight and the node holds the image now; locally this would wipe your caches.
  [[ -z ${CI:-} ]] || docker rmi "ghcr.io/smana/$i:$TAG" >/dev/null
done
[[ -z ${CI:-} ]] || docker builder prune -af >/dev/null

step "up.sh (it keeps the cluster that exists)"
export MODEL_API_KEY=stub MODEL=stub MODEL_BASE_URL="$STUB_URL" CHART_REF=charts/agent-platform
# A non-default port exercises the publicURL/port agreement.
export ROOMCTL="$PWD/.quickstart/e2e/roomctl" LOCAL_PORT=18080
export UP_HELM_ARGS="--set e2e.unpinnedImages=true --set broker.image.tag=$TAG --set runController.image.tag=$TAG --set runController.images.harness.tag=$TAG --set runController.images.bridge.tag=$TAG"
hack/quickstart/up.sh

step "local mode through a real port-forward"
kubectl -n agent-platform port-forward deploy/room-broker 18080:8080 >.quickstart/e2e/port-forward.log 2>&1 &
pf=$!
ready=0
for _ in $(seq 30); do
  if curl -fsS -o /dev/null http://localhost:18080/.well-known/roomctl 2>/dev/null; then ready=1; break; fi
  sleep 1
done
((ready)) || { echo "port-forward never ready" >&2; exit 1; }
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
((rc == 0)) || { echo "roomctl run exited $rc" >&2; exit 1; }
[[ -n $room && -n $claim ]] || { echo "roomctl run printed no room or claim" >&2; exit 1; }
phase=$(kubectl -n agents get agentrun "$claim" -o jsonpath='{.status.phase}')
[[ $phase == Succeeded ]] || { echo "AgentRun $claim is '$phase', roomctl run said Succeeded" >&2; exit 1; }

# lastSeq is not evidence: the Room's Open event and the broker's own run events raise it with no
# bridge. Only a harness-origin row proves the bridge mirrored the agent into the room.
n=$(kubectl -n agent-platform exec rooms-1 -c postgres -- psql -d rooms -tAc \
  "select count(*) from events where room_id='$room' and origin='harness'")
((${n:-0} > 0)) || { echo "room $room has no harness-origin event" >&2; exit 1; }
echo "quickstart-e2e OK: roomctl run took $claim to Succeeded; room $room has $n harness events"
