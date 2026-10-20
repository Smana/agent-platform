#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# quickstart-e2e: this checkout's images in kind, a stub model, one AgentRun to Succeeded.
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
  # The agent's own ring buffer: drops that happened minutes ago, not a live stream.
  kubectl -n kube-system exec ds/cilium -c cilium-agent -- hubble observe --verdict DROPPED --last 200 >&2 || true
}
onexit() {
  local rc=$?
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
export UP_HELM_ARGS="--set e2e.unpinnedImages=true --set broker.image.tag=$TAG --set runController.image.tag=$TAG --set runController.images.harness.tag=$TAG --set runController.images.bridge.tag=$TAG"
hack/quickstart/up.sh

step "stub model"
kubectl apply -f hack/quickstart/stub-model.yaml.tmpl
kubectl -n e2e rollout status deploy/stub-model --timeout=120s

step "run"
out=$(RUN_PROMPT='quickstart-e2e' hack/quickstart/run.sh)
echo "$out"
room=$(sed -n 's#^room: http://localhost:8080/r/\([a-z0-9]*\) .*#\1#p' <<<"$out")
run=$(sed -n 's#.*agentrun run-\([a-z0-9]*\))$#run-\1#p' <<<"$out")
[[ -n $room && -n $run ]] || { echo "could not read the room/run ids from run.sh" >&2; exit 1; }

step "wait for $run to Succeed (10 min)"
deadline=$((SECONDS + 600))
phase=
while ((SECONDS < deadline)); do
  phase=$(kubectl -n agents get agentrun "$run" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  echo "phase: ${phase:-<none>}"
  [[ $phase == Succeeded ]] && break
  case $phase in Failed | BudgetExhausted | Revoked) break ;; esac
  sleep 10
done
[[ $phase == Succeeded ]] || { echo "AgentRun ended as '${phase:-<none>}'" >&2; exit 1; }

# lastSeq is not evidence: the Room's Open event and the broker's own run events raise it with no
# bridge. Only a harness-origin row proves the bridge mirrored the agent into the room.
n=$(kubectl -n agent-platform exec rooms-1 -c postgres -- psql -d rooms -tAc \
  "select count(*) from events where room_id='$room' and origin='harness'")
((${n:-0} > 0)) || { echo "room $room has no harness-origin event" >&2; exit 1; }
echo "quickstart-e2e OK: $run Succeeded, room $room has $n harness events"
