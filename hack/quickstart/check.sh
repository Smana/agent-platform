#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Offline drift check (no cluster): the quickstart scripts, their manifests and the chart agree on
# every name they share. Run by `task quickstart:check`.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
fail=0
bad() { echo "FAIL: $*" >&2; fail=1; }

bash -n hack/quickstart/*.sh
shellcheck -x hack/quickstart/*.sh

export ROOM_ID=abcdefg2 RUN_ID=234567ab REPO=owner/repo TASK_JSON='"say \"hi\": ok"'
# shellcheck disable=SC2016 # envsubst takes a literal variable list
render() { envsubst '${ROOM_ID} ${RUN_ID} ${REPO} ${TASK_JSON}' <"hack/quickstart/manifests/$1.yaml.tmpl"; }
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
for m in ca-issuer cnpg-cluster room agentrun; do render "$m" >"$tmp/$m.yaml"; yq ea '.' "$tmp/$m.yaml" >/dev/null || bad "$m does not parse"; done
[[ $(yq '.spec.task.text' "$tmp/agentrun.yaml") == 'say "hi": ok' ]] || bad "task text does not survive templating"
[[ $(yq '.spec.roomRef' "$tmp/agentrun.yaml") == "$ROOM_ID" ]] || bad "roomRef"

dg='v0.0.0@sha256:0000'
out=$(helm template ap charts/agent-platform -n agent-platform -f charts/agent-platform/values-quickstart.yaml \
  --set createRunsNamespace=false --set broker.image.tag="$dg" --set runController.image.tag="$dg" \
  --set runController.images.harness.tag="$dg" --set runController.images.bridge.tag="$dg")
q() { yq ea "[select(.kind == \"$1\")] | $2" - <<<"$out"; }
has() { grep -qF -- "$1" <<<"$out" || bad "chart does not reference $1 ($2)"; }

# names db.sh / up.sh create, as the chart consumes them
has 'name: rooms-broker-dsn' 'db.sh writes it'
has 'name: rooms-owner-dsn' 'db.sh writes it'
grep -q 'create secret generic rooms-broker-dsn' hack/quickstart/db.sh || bad "db.sh does not create rooms-broker-dsn"
grep -q 'create secret generic rooms-owner-dsn' hack/quickstart/db.sh || bad "db.sh does not create rooms-owner-dsn"
[[ $(q Deployment '[.[] | select(.metadata.name=="room-broker") | .. | select(has("secretKeyRef")) | .secretKeyRef | select(.key != "uri")] | length') == 0 ]] || bad "a broker secretKeyRef uses a key other than uri"
has 'name: quickstart-ca' 'the Issuer from ca-issuer.yaml.tmpl'
[[ $(yq ea 'select(.kind=="Issuer") | .metadata.name' "$tmp/ca-issuer.yaml" | grep -c '^quickstart-ca$') == 1 ]] || bad "ca-issuer lacks Issuer quickstart-ca"
[[ $(q Certificate '.[] | select(.metadata.name=="room-broker-tls") | .spec.issuerRef.name') == quickstart-ca ]] || bad "broker Certificate issuerRef is not quickstart-ca"
# runs namespace and the model/git Secrets up.sh creates
cfg=$(q ConfigMap '.[] | select(.metadata.name=="run-controller-config") | .data[]' )
for s in agent-model agent-git; do
  grep -qF "$s" <<<"$cfg" || bad "run-controller config does not name $s"
  grep -q "secret generic $s " hack/quickstart/up.sh || bad "up.sh does not create $s"
done
grep -qE 'namespace: "?agents"?$' <<<"$cfg" || bad "run-controller config is not on namespace agents"
grep -q 'create namespace agents' hack/quickstart/up.sh || bad "up.sh does not create namespace agents"
[[ $(yq ea 'select(.kind=="AgentRun") | .metadata.namespace' "$tmp/agentrun.yaml") == agents ]] || bad "AgentRun not in agents"
[[ $(q Namespace 'length') == 0 ]] || bad "chart renders a Namespace despite createRunsNamespace=false"
[[ $(q RuntimeClass '.[0].metadata.name') == gvisor ]] || bad "RuntimeClass gvisor not rendered"
# CNPG naming: service and app Secret db.sh relies on
[[ $(yq '.metadata.name' "$tmp/cnpg-cluster.yaml") == rooms ]] || bad "cluster name is not rooms (rooms-rw / rooms-app)"
for r in rooms_broker rooms_retention; do yq -e ".spec.managed.roles[] | select(.name==\"$r\" and .login==true)" "$tmp/cnpg-cluster.yaml" >/dev/null || bad "managed role $r"; done
# local-mode port-forward the script prints
grep -q 'deploy/room-broker 8080:8080' hack/quickstart/up.sh || bad "up.sh port-forward command"
[[ $(q ConfigMap '.[] | select(.metadata.name=="room-broker-config") | .data."config.yaml" | from_yaml | .publicURL') == http://localhost:8080 ]] || bad "publicURL is not http://localhost:8080"

[[ $fail == 0 ]] && echo "quickstart check: ok"
exit "$fail"
