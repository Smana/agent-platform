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

qs=hack/quickstart
vals=charts/agent-platform/values.yaml
qvals=charts/agent-platform/values-quickstart.yaml
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT

# ---- sources of truth ----
# shellcheck source=hack/quickstart/versions.env
. "$qs/versions.env"
# shellcheck source=hack/quickstart/lib.sh
. "$qs/lib.sh"
ns=$(sed -n 's/^NS=//p' "$qs/up.sh")                          # the release namespace
runs_ns=$(yq '.runs.namespace' "$vals")
issuer_kind=$(yq '.broker.tls.issuerRef.kind' "$qvals")
issuer_name=$(yq '.broker.tls.issuerRef.name' "$qvals")
[[ -n $ns && -n $runs_ns && -n $KIND_CLUSTER ]] || bad "a source of truth is empty"

# ---- templates render and parse ----
export RUNS_NS=$runs_ns NS=$ns PROBE_IMAGE CLUSTER_DOMAIN=cluster.local
# shellcheck disable=SC2016 # envsubst takes a literal variable list
envsubst '${RUNS_NS} ${NS} ${PROBE_IMAGE} ${CLUSTER_DOMAIN}' <"$qs/manifests/gvisor-probe.yaml.tmpl" >"$tmp/gvisor-probe.yaml"
yq ea '.' "$tmp/gvisor-probe.yaml" >/dev/null || bad "gvisor-probe does not parse"
# both templates are applied raw by up.sh and db.sh, no envsubst
for m in ca-issuer cnpg-cluster; do cat "$qs/manifests/$m.yaml.tmpl" >"$tmp/$m.yaml"; yq ea '.' "$tmp/$m.yaml" >/dev/null || bad "$m does not parse"; done

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
while read -r name key; do
  [[ -n $name ]] || continue
  # shellcheck disable=SC2016 # a literal "$ns" is what db.sh must contain
  grep -qF -- "qs_secret \"\$ns\" $name Opaque $key=" "$qs/db.sh" || bad "db.sh does not create $name with key $key"
done < <(yq ea '[select(.kind=="Deployment") | .. | select(has("secretKeyRef")) | .secretKeyRef | .name + " " + .key] | unique | .[]' - <<<"$out")
[[ $(q Deployment '[.. | select(has("secretKeyRef")) | .secretKeyRef.name] | unique | length') -ge 2 ]] || bad "the broker no longer reads two DSN secrets"

# Model/git Secrets and the runs namespace
cfg=$(q ConfigMap '.[] | select(.metadata.name=="run-controller-config") | .data."config.yaml" | from_yaml')
for k in modelSecret gitSecret; do
  s=$(yq ".. | select(has(\"$k\")) | .$k" - <<<"$cfg")
  grep -q "qs_secret agents $s " "$qs/up.sh" || bad "up.sh does not create $k ($s)"
done
[[ $(yq ".. | select(has(\"namespace\")) | .namespace" - <<<"$cfg" | head -1) == "$runs_ns" ]] || bad "run-controller namespace is not $runs_ns"
grep -q "create namespace $runs_ns " "$qs/up.sh" || bad "up.sh does not create namespace $runs_ns"
[[ $(q Namespace 'length') == 0 ]] || bad "chart renders a Namespace despite createRunsNamespace=false"
[[ $(q RuntimeClass '.[0].metadata.name') == $(yq '.runController.runtimeClassName' "$vals") ]] || bad "RuntimeClass not rendered"

# gVisor probe: run-shaped (runs namespace, run-id label, the chart's RuntimeClass), asks the broker's own name and port
rc=$(yq '.runController.runtimeClassName' "$vals")
[[ $(yq ea 'select(.kind=="Pod") | .spec.runtimeClassName' "$tmp/gvisor-probe.yaml") == "$rc" ]] || bad "probe runtimeClassName is not $rc"
[[ $(yq ea '[.metadata.namespace] | unique | join(",")' "$tmp/gvisor-probe.yaml") == "$runs_ns" ]] || bad "probe is not in $runs_ns"
[[ $(yq ea 'select(.kind=="Pod") | .metadata.labels."agents.ogenki.io/run-id"' "$tmp/gvisor-probe.yaml") != null ]] || bad "probe lacks the run-id label the broker CNP admits"
[[ $(yq ea 'select(.kind=="CiliumNetworkPolicy") | .spec.egress[1].toEndpoints[0].matchLabels."app.kubernetes.io/name"' "$tmp/gvisor-probe.yaml") == room-broker ]] || bad "probe CNP does not target the broker"
[[ $(yq ea 'select(.kind=="CiliumNetworkPolicy") | .spec.egress[1].toPorts[0].ports[0].port' "$tmp/gvisor-probe.yaml") == 8443 ]] || bad "probe CNP port is not the broker's 8443"
for c in ok:dmesg ok:dns ok:tcp; do grep -q "$c" "$tmp/gvisor-probe.yaml" || bad "probe does not print $c"; done
[[ $(yq '.clusterDomain' "$vals") == cluster.local ]] || bad "up.sh's probe assumes clusterDomain cluster.local"
grep -qF 'unsandboxed == 0 ]]; then' "$qs/up.sh" || bad "up.sh does not skip the probe under ALLOW_UNSANDBOXED=1"
for t in sha256sum sha512sum bzip2 tar; do grep -E "^for t in " "$qs/up.sh" | grep -qw "$t" || bad "up.sh does not check for $t (fetch-runsc.sh needs sha512sum, bzip2 and tar; qs_fetch_roomctl needs sha256sum)"; done

# Cilium must keep socket-LB out of pod namespaces, or gVisor run pods cannot reach any ClusterIP
grep -q 'socketLB.hostNamespaceOnly=true' "$qs/up.sh" || bad "up.sh does not set socketLB.hostNamespaceOnly=true (gVisor)"

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
# shellcheck disable=SC2016 # the literal $VARs are what up.sh must contain
{
grep -qF 'deploy/room-broker $LOCAL_PORT:8080' "$qs/up.sh" || bad "up.sh port-forward command"
grep -qF 'broker.publicURL=http://localhost:$LOCAL_PORT' "$qs/up.sh" || bad "up.sh does not set publicURL from LOCAL_PORT"
grep -qF 'roomctl configure --url http://localhost:$LOCAL_PORT' "$qs/up.sh" || bad "up.sh's configure line does not use LOCAL_PORT"
grep -qF 'roomctl run --repo $REPO' "$qs/up.sh" || bad "up.sh does not print roomctl run"
grep -qF 'qs_platform_ok "$(uname -s)"' "$qs/up.sh" || bad "up.sh does not stop on an unproven platform"
}
[[ $(q ConfigMap '.[] | select(.metadata.name=="room-broker-config") | .data."config.yaml" | from_yaml | .publicURL') == http://localhost:8080 ]] || bad "default publicURL is not http://localhost:8080"
# the repo's kubectl pin can drive the kind node
qs_skew_ok "$(sed -n 's/^kubectl = "1\.\([0-9]*\)\..*"/\1/p' mise.toml)" "$(qs_node_minor "$KIND_NODE_IMAGE")" || bad "mise.toml's kubectl is outside the kind node's skew"
grep -qF 'qs_skew_ok' "$qs/up.sh" || bad "up.sh has no kubectl skew preflight"

# ALLOW_UNSANDBOXED=1: the three overrides really yield an unsandboxed config
un=$(helm template ap charts/agent-platform -n "$ns" -f "$qvals" --set createRunsNamespace=false \
  --set runController.allowUnsandboxed=true --set runController.runtimeClassName= --set runController.createRuntimeClass=false \
  --set broker.image.tag="$dg" --set runController.image.tag="$dg" --set runController.images.harness.tag="$dg" --set runController.images.bridge.tag="$dg")
grep -q 'kind: RuntimeClass' <<<"$un" && bad "unsandboxed render still has a RuntimeClass"
grep -q 'allowUnsandboxed: true' <<<"$un" || bad "unsandboxed render lacks allowUnsandboxed"
grep -q 'runController.runtimeClassName=' "$qs/up.sh" || bad "up.sh does not clear runtimeClassName for ALLOW_UNSANDBOXED"

bash hack/quickstart/lib_test.sh || fail=1

[[ $fail == 0 ]] && echo "quickstart check: ok"
exit "$fail"
