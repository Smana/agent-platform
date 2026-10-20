#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Offline drift check (no cluster, no docker): the quickstart scripts, their manifests and the chart
# agree on every name they share. Expected values come from their single source (the chart's
# values, kind-config.yaml, versions.env), never from a second literal here. `task quickstart:check`.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
fail=0
bad() { echo "FAIL: $*" >&2; fail=1; }

bash -n hack/quickstart/*.sh
shellcheck -x hack/quickstart/*.sh
helm dependency build charts/agent-platform >/dev/null # charts/ is gitignored

qs=hack/quickstart
vals=charts/agent-platform/values.yaml
qvals=charts/agent-platform/values-quickstart.yaml
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT

# ---- sources of truth ----
# shellcheck source=hack/quickstart/versions.env
. "$qs/versions.env"
ns=$(sed -n 's/^NS=//p' "$qs/up.sh")                          # the release namespace
runs_ns=$(yq '.runs.namespace' "$vals")
prefix=$(yq '.runs.namePrefix' "$vals")
issuer_kind=$(yq '.broker.tls.issuerRef.kind' "$qvals")
issuer_name=$(yq '.broker.tls.issuerRef.name' "$qvals")
[[ -n $ns && -n $runs_ns && -n $prefix && -n $KIND_CLUSTER ]] || bad "a source of truth is empty"

# ---- templates render and parse ----
export ROOM_ID=abcdefg2 RUN_ID=234567ab REPO=owner/repo TASK=$'say "hi": ok\n😀 second line'
# shellcheck disable=SC2016 # envsubst takes a literal variable list
render() { envsubst '${ROOM_ID} ${RUN_ID} ${REPO}' <"$qs/manifests/$1.yaml.tmpl"; }
for m in ca-issuer cnpg-cluster room agentrun; do render "$m" >"$tmp/$m.yaml"; yq ea '.' "$tmp/$m.yaml" >/dev/null || bad "$m does not parse"; done
render agentrun | yq '.spec.task.text = strenv(TASK)' >"$tmp/run-final.yaml"
[[ $(yq '.spec.task.text' "$tmp/run-final.yaml") == "$TASK" ]] || bad "task text does not survive run.sh's templating"

# ---- the chart as up.sh installs it ----
dg='v0.0.0@sha256:0000'
out=$(helm template ap charts/agent-platform -n "$ns" -f "$qvals" --set createRunsNamespace=false \
  --set broker.image.tag="$dg" --set runController.image.tag="$dg" \
  --set runController.images.harness.tag="$dg" --set runController.images.bridge.tag="$dg")
q() { yq ea "[select(.kind == \"$1\")] | $2" - <<<"$out"; }

# Issuer: the one ca-issuer.yaml.tmpl defines is the one the chart's Certificate asks for (kind and name)
[[ $(yq ea "select(.kind==\"$issuer_kind\" and .metadata.name==\"$issuer_name\") | .metadata.name" "$tmp/ca-issuer.yaml") == "$issuer_name" ]] || bad "ca-issuer.yaml.tmpl has no $issuer_kind/$issuer_name"
[[ $(q Certificate '.[] | select(.metadata.name=="room-broker-tls") | .spec.issuerRef.kind + "/" + .spec.issuerRef.name') == "$issuer_kind/$issuer_name" ]] || bad "broker Certificate issuerRef is not $issuer_kind/$issuer_name"
if [[ $issuer_kind == Issuer ]]; then :; else bad "Issuer is namespaced in the release namespace; $issuer_kind needs a different ca-issuer apply"; fi

# DSN Secrets: every secret the broker reads, under the key it reads, is created by db.sh with that key
dbflat=$(sed -e ':a' -e '/\\$/{N;s/\\\n *//;ba' -e '}' "$qs/db.sh") # join continuation lines
while read -r name key; do
  [[ -n $name ]] || continue
  grep -qE -- "create secret generic $name .*--from-literal=$key=" <<<"$dbflat" || bad "db.sh does not create $name with key $key"
done < <(yq ea '[select(.kind=="Deployment") | .. | select(has("secretKeyRef")) | .secretKeyRef | .name + " " + .key] | unique | .[]' - <<<"$out")
[[ $(q Deployment '[.. | select(has("secretKeyRef")) | .secretKeyRef.name] | unique | length') -ge 2 ]] || bad "the broker no longer reads two DSN secrets"

# Model/git Secrets and the runs namespace
cfg=$(q ConfigMap '.[] | select(.metadata.name=="run-controller-config") | .data."config.yaml" | from_yaml')
for k in modelSecret gitSecret; do
  s=$(yq ".. | select(has(\"$k\")) | .$k" - <<<"$cfg")
  grep -q "secret generic $s " "$qs/up.sh" || bad "up.sh does not create $k ($s)"
done
[[ $(yq ".. | select(has(\"namespace\")) | .namespace" - <<<"$cfg" | head -1) == "$runs_ns" ]] || bad "run-controller namespace is not $runs_ns"
grep -q "create namespace $runs_ns " "$qs/up.sh" || bad "up.sh does not create namespace $runs_ns"
[[ $(yq '.metadata.namespace' "$tmp/agentrun.yaml") == "$runs_ns" ]] || bad "AgentRun is not in $runs_ns"
[[ $(yq '.metadata.name' "$tmp/agentrun.yaml") == "$prefix$RUN_ID" ]] || bad "AgentRun name is not $prefix<id> (runs.namePrefix)"
[[ $(yq '.metadata.namespace' "$tmp/room.yaml") == "$ns" ]] || bad "Room is not in the release namespace $ns (where the broker reads it)"
[[ $(q Namespace 'length') == 0 ]] || bad "chart renders a Namespace despite createRunsNamespace=false"
[[ $(q RuntimeClass '.[0].metadata.name') == $(yq '.runController.runtimeClassName' "$vals") ]] || bad "RuntimeClass not rendered"

# kind cluster name: kind-config, Cilium's apiserver host, and every script agree
[[ $(yq '.name' "$qs/kind-config.yaml") == "$KIND_CLUSTER" ]] || bad "kind-config name != KIND_CLUSTER"
# shellcheck disable=SC2016 # a literal ${KIND_CLUSTER} is what up.sh must contain
grep -q 'k8sServiceHost="${KIND_CLUSTER}-control-plane"' "$qs/up.sh" || bad "Cilium k8sServiceHost is not \${KIND_CLUSTER}-control-plane"
[[ $(grep -c 'KIND_CLUSTER' "$qs/down.sh") -ge 1 ]] || bad "down.sh does not use KIND_CLUSTER"

# CNPG naming: service and app Secret db.sh relies on
[[ $(yq '.metadata.name' "$tmp/cnpg-cluster.yaml") == rooms ]] || bad "cluster name is not rooms (rooms-rw / rooms-app)"
for r in rooms_broker rooms_retention; do yq -e ".spec.managed.roles[] | select(.name==\"$r\" and .login==true)" "$tmp/cnpg-cluster.yaml" >/dev/null || bad "managed role $r"; done
grep -q "@rooms-rw.\${ns}.svc" "$qs/db.sh" || bad "db.sh does not point the broker DSN at rooms-rw.<ns>"

# local mode: the printed port-forward and URL
grep -q 'deploy/room-broker 8080:8080' "$qs/up.sh" || bad "up.sh port-forward command"
[[ $(q ConfigMap '.[] | select(.metadata.name=="room-broker-config") | .data."config.yaml" | from_yaml | .publicURL') == http://localhost:8080 ]] || bad "publicURL is not http://localhost:8080"

# ALLOW_UNSANDBOXED=1: the three overrides really yield an unsandboxed config
un=$(helm template ap charts/agent-platform -n "$ns" -f "$qvals" --set createRunsNamespace=false \
  --set runController.allowUnsandboxed=true --set runController.runtimeClassName= --set runController.createRuntimeClass=false \
  --set broker.image.tag="$dg" --set runController.image.tag="$dg" --set runController.images.harness.tag="$dg" --set runController.images.bridge.tag="$dg")
grep -q 'kind: RuntimeClass' <<<"$un" && bad "unsandboxed render still has a RuntimeClass"
grep -q 'allowUnsandboxed: true' <<<"$un" || bad "unsandboxed render lacks allowUnsandboxed"
grep -q 'runController.runtimeClassName=' "$qs/up.sh" || bad "up.sh does not clear runtimeClassName for ALLOW_UNSANDBOXED"

[[ $fail == 0 ]] && echo "quickstart check: ok"
exit "$fail"
