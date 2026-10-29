#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# migrate-group e2e: v0.30.0 from its release in kind, Rooms and a Task under agents.ogenki.io, the
# upgrade to this checkout, migrate-group dry run then --apply, then the checks: every room's log
# unchanged and readable, the task's status kept, a re-run with nothing to do, the reverse and back.
# No secrets: nothing here starts a run. `.github/workflows/quickstart-e2e.yaml`, job migrate-group.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
# shellcheck source=hack/quickstart/versions.env
. hack/quickstart/versions.env
FROM_VERSION="${FROM_VERSION:-v0.30.0}"
OLD=agents.ogenki.io NEW=maku.ogenki.io NS=maku TAG=e2e ROOMS=(migroom2 migroom3) TASK=migtask2
step() { printf '\n==> %s\n' "$*"; }
fail() { echo "migrate-e2e: $*" >&2; exit 1; }
export MODEL_API_KEY=stub GITHUB_TOKEN=unused REPO=e2e/unused ALLOW_UNSANDBOXED=1 # up.sh requires them; no run starts
mkdir -p .quickstart/e2e
old=$(mktemp -d)
onexit() {
  local rc=$?
  [[ -z ${pf:-} ]] || kill "$pf" 2>/dev/null || true
  if [[ $rc -ne 0 ]]; then
    kubectl get "rooms.$OLD,rooms.$NEW,tasks.$OLD,tasks.$NEW" -A -o yaml >&2 || true
    kubectl -n "$NS" logs deploy/room-broker --tail=200 >&2 || true
    cat .quickstart/e2e/migrate-*.json >&2 || true
  fi
  git worktree remove --force "$old" 2>/dev/null || true
  exit "$rc"
}
trap onexit EXIT
sql() { kubectl -n "$NS" exec rooms-1 -c postgres -- psql -d rooms -tAc "$1"; }
# migrate-group refuses anything but a KUBECONFIG naming one file with exactly one context: this is kind's.
mg() { LOG_FORMAT=json KUBECONFIG="$PWD/.quickstart/kubeconfig" .quickstart/e2e/maku-broker migrate-group "$@"; }

step "install $FROM_VERSION from its release"
git fetch -q origin "refs/tags/$FROM_VERSION:refs/tags/$FROM_VERSION" || true
git worktree add -q --detach "$old" "$FROM_VERSION"
(cd "$old" && AP_VERSION="$FROM_VERSION" hack/quickstart/up.sh)
kind export kubeconfig --name "$KIND_CLUSTER" --kubeconfig "$PWD/.quickstart/kubeconfig"
export KUBECONFIG="$PWD/.quickstart/kubeconfig"

step "rooms and a task under $OLD"
kubectl apply --server-side -f "$old/config/crd/${OLD}_tasks.yaml" # the umbrella chart ships no Task CRD
for r in "${ROOMS[@]}"; do
  kubectl apply -f - <<EOF
apiVersion: $OLD/v1alpha1
kind: Room
metadata: {name: $r, namespace: $NS}
spec: {owner: "human:local", driver: "human:local", dataClass: public, repository: e2e/unused}
EOF
  kubectl -n "$NS" wait "rooms.$OLD/$r" --for=jsonpath='{.status.lastSeq}'=1 --timeout=120s
done
kubectl apply -f - <<EOF
apiVersion: $OLD/v1alpha1
kind: Task
metadata: {name: $TASK, namespace: $NS, labels: {agents.ogenki.io/issue: "7"}}
spec:
  source: {kind: issue, ref: "e2e/unused#7", key: k, requestedBy: "github:e2e", trust: untrusted, contentSHA256: "$(printf '%064d' 0)"}
  repository: e2e/unused
  issue: 7
  text: migrate-group e2e
  dataClass: public
EOF
kubectl -n "$NS" patch "tasks.$OLD/$TASK" --subresource=status --type=merge -p "{\"status\":{\"phase\":\"Done\",\"roomRef\":\"${ROOMS[0]}\"}}"
# A status field no controller recomputes: roomctrl keeps status.task as it is when the log holds no task
# event, so on the new Room it can only be there if migrate-group copied it (lastSeq, phase and the
# rest are re-projected from the store and prove nothing about the copy).
kubectl -n "$NS" patch "rooms.$OLD/${ROOMS[0]}" --subresource=status --type=merge \
  -p '{"status":{"task":{"phase":"Implementing","issue":7,"issueTitle":"migrate-group e2e","labelledBy":"e2e"}}}'
task_status='{.status.task.phase}/{.status.task.issue}/{.status.task.issueTitle}/{.status.task.labelledBy}'
[[ $(kubectl -n "$NS" get "rooms.$OLD/${ROOMS[0]}" -o jsonpath="$task_status") == "Implementing/7/migrate-group e2e/e2e" ]] || fail "the old room lost its status.task before the upgrade"
# The Postgres-backed checks below guard the store (a move writes no event, seals nothing), not the copy.
before=$(sql "select count(*) || '/' || max(seq) from events where room_id in ('${ROOMS[0]}','${ROOMS[1]}')")
uid0=$(kubectl -n "$NS" get "rooms.$OLD/${ROOMS[0]}" -o jsonpath='{.metadata.uid}')

step "upgrade to this checkout: the CRDs first (helm upgrade skips crds/), then the chart"
for i in maku-broker maku-run-controller; do
  docker build -f "images/$i/Dockerfile" -t "ghcr.io/smana/$i:$TAG" .
  kind load docker-image --name "$KIND_CLUSTER" "ghcr.io/smana/$i:$TAG"
  [[ -z ${CI:-} ]] || docker rmi "ghcr.io/smana/$i:$TAG" >/dev/null # the runner's disk is tight; the node holds it now
done
[[ -z ${CI:-} ]] || docker builder prune -af >/dev/null
go build -o .quickstart/e2e/maku ./cmd/maku
go build -o .quickstart/e2e/maku-broker ./cmd/maku-broker
kubectl apply --server-side -f "config/crd/${NEW}_rooms.yaml" -f "config/crd/${NEW}_agentruns.yaml" -f "config/crd/${NEW}_tasks.yaml"
export ROOMCTL="$PWD/.quickstart/e2e/maku" LOCAL_PORT=18080
export UP_HELM_ARGS="--set e2e.unpinnedImages=true --set broker.image.tag=$TAG --set runController.image.tag=$TAG --set runController.images.harness.tag=$TAG --set runController.images.bridge.tag=$TAG"
hack/quickstart/up.sh
kubectl get "crd/rooms.$OLD" >/dev/null || fail "the upgrade removed the old Room CRD"

step "dry run: three to copy, nothing written"
mg --from "$OLD" --to "$NEW" >.quickstart/e2e/migrate-dry.json
[[ $(grep -c '"outcome":"would-copy"' .quickstart/e2e/migrate-dry.json) == 3 ]] || fail "the dry run does not list 3 objects"
[[ -z $(kubectl get "rooms.$NEW" -n "$NS" -o name) ]] || fail "the dry run wrote a copy"

step "--apply"
mg --from "$OLD" --to "$NEW" --apply >.quickstart/e2e/migrate-apply.json
[[ $(grep -c '"outcome":"migrated"' .quickstart/e2e/migrate-apply.json) == 3 ]] || fail "--apply did not migrate 3 objects"
[[ -z $(kubectl get "rooms.$OLD,tasks.$OLD" -A -o name) ]] || fail "originals are left under $OLD"

[[ $(kubectl -n "$NS" get "rooms.$NEW/${ROOMS[0]}" -o jsonpath="$task_status") == "Implementing/7/migrate-group e2e/e2e" ]] || fail "the copy's status.task is not the original's: migrate-group did not copy the Room status"

step "the logs came through, and the broker serves them"
for r in "${ROOMS[@]}"; do
  kubectl -n "$NS" wait "rooms.$NEW/$r" --for=jsonpath='{.status.lastSeq}'=1 --timeout=60s
  [[ -z $(kubectl -n "$NS" get "rooms.$NEW/$r" -o jsonpath='{.metadata.annotations.maku\.ogenki\.io/migrating}') ]] || fail "$r is still guarded"
  kubectl -n "$NS" get "rooms.$NEW/$r" -o jsonpath='{.metadata.finalizers}' | grep -q "$NEW/room-log" || fail "$r has no $NEW/room-log finalizer"
done
[[ $(kubectl -n "$NS" get "rooms.$NEW/${ROOMS[0]}" -o jsonpath='{.metadata.annotations.maku\.ogenki\.io/migrated-from}') == "$uid0" ]] || fail "migrated-from is not the original's uid"
[[ $(sql "select count(*) || '/' || max(seq) from events where room_id in ('${ROOMS[0]}','${ROOMS[1]}')") == "$before" ]] || fail "the logs changed: no event may be written by a move"
[[ $(kubectl -n "$NS" get "tasks.$NEW/$TASK" -o jsonpath='{.status.phase}/{.status.roomRef}/{.metadata.labels.agents\.ogenki\.io/issue}') == "Done/${ROOMS[0]}/7" ]] || fail "the task's status or label did not come through"
kubectl -n "$NS" port-forward deploy/room-broker 18080:8080 >.quickstart/e2e/port-forward.log 2>&1 &
pf=$!
for _ in $(seq 30); do curl -fsS -o /dev/null http://localhost:18080/api/rooms 2>/dev/null && break; sleep 1; done
listed=$(curl -fsS http://localhost:18080/api/rooms | yq -p=json '[.[].id] | sort | join(",")')
[[ $listed == *migroom2*migroom3* ]] || fail "the room list is '$listed'"
for r in "${ROOMS[@]}"; do
  [[ $(curl -s -o /dev/null -w '%{http_code}' "http://localhost:18080/api/rooms/$r/summary") == 200 ]] || fail "no summary for $r"
done

step "a re-run has nothing to do"
mg --from "$OLD" --to "$NEW" --apply >.quickstart/e2e/migrate-again.json
grep -q '"msg":"migrate-group done".*"migrated":0' .quickstart/e2e/migrate-again.json || fail "a re-run moved something"
mg --from "$OLD" --to "$NEW" >.quickstart/e2e/migrate-dry2.json
grep -q '"msg":"migrate-group done".*"wouldCopy":0.*"wouldResume":0' .quickstart/e2e/migrate-dry2.json || fail "a fresh dry run still finds something to do"

step "the rollback path: reverse, then forward again; nothing sealed"
mg --from "$NEW" --to "$OLD" --apply >.quickstart/e2e/migrate-reverse.json
[[ $(kubectl get "rooms.$OLD" -n "$NS" -o name | wc -l) == 2 && -z $(kubectl get "rooms.$NEW" -n "$NS" -o name) ]] || fail "the reverse did not move both rooms back"
[[ $(kubectl get "tasks.$OLD" -n "$NS" -o name | wc -l) == 1 && -z $(kubectl get "tasks.$NEW" -n "$NS" -o name) ]] || fail "the reverse did not move the task back"
mg --from "$OLD" --to "$NEW" --apply >.quickstart/e2e/migrate-forward.json
[[ $(kubectl get "rooms.$NEW" -n "$NS" -o name | wc -l) == 2 && -z $(kubectl get "rooms.$OLD,tasks.$OLD" -n "$NS" -o name) ]] || fail "the forward leg did not leave 2 rooms under $NEW and nothing under $OLD"
[[ $(kubectl -n "$NS" get "tasks.$NEW/$TASK" -o jsonpath='{.status.phase}/{.status.roomRef}/{.metadata.labels.agents\.ogenki\.io/issue}') == "Done/${ROOMS[0]}/7" ]] || fail "the task did not survive the round trip"
[[ $(kubectl -n "$NS" get "rooms.$NEW/${ROOMS[0]}" -o jsonpath="$task_status") == "Implementing/7/migrate-group e2e/e2e" ]] || fail "the room status did not survive the round trip"
# A seal would come from the controller, asynchronously: watch for 60 s rather than look once.
for _ in $(seq 30); do
  [[ $(sql "select count(*) from rooms where room_id in ('${ROOMS[0]}','${ROOMS[1]}') and sealed") == 0 ]] || fail "a round trip sealed a log"
  sleep 2
done
[[ $(sql "select count(*) || '/' || max(seq) from events where room_id in ('${ROOMS[0]}','${ROOMS[1]}')") == "$before" ]] || fail "a round trip wrote an event"
echo "migrate-e2e OK: ${#ROOMS[@]} rooms and 1 task moved $OLD -> $NEW and back and forth, logs unchanged ($before)"
