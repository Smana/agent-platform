#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Renders the chart and asserts what the platform relies on: the ServiceAccount name SP2's
# broker CNP and allowlist expect, restricted security, the probes, the rooms-system token, the
# broker CA (ruling SC), the config rendered where FACTORY_CONFIG points, an image by digest.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
digest='v0.0.0@sha256:0000'
out="$(helm template agent-factory . --namespace agent-system --set image.tag="$digest" \
  --set config.repository=Smana/cloud-native-ref)"
fail=0
check() { grep -qF -- "$1" <<<"$out" || { echo "FAIL: $2" >&2; fail=1; }; }
# yq assertion on one rendered kind: ykind KIND 'expr' 'message'; expr must yield at least one
# value, and every value it yields, across every document of that kind, must be true.
ykind() {
  yq ea -e "[select(.kind == \"$1\") | $2] | (length > 0 and all)" - <<<"$out" >/dev/null 2>&1 || { echo "FAIL: $3" >&2; fail=1; }
}
# yq query that must succeed and print nothing: ynone 'expr' 'message'.
ynone() {
  local got
  got=$(yq "$1" - <<<"$out") || { echo "FAIL: yq errored: $2" >&2; fail=1; return; }
  [ -z "$got" ] || { echo "FAIL: $2" >&2; fail=1; }
}
pod='.spec.template.spec'
ctr="$pod.containers[0]"
# Parenthesised so a path appended after the helper applies to the selected item only.
vol() { echo "($pod.volumes[] | select(.name == \"$1\"))"; }
mnt() { echo "($ctr.volumeMounts[] | select(.name == \"$1\"))"; }
ykind Deployment "$pod.serviceAccountName == \"agent-factory\"" "the factory's ServiceAccount is agent-factory"
ykind Deployment '.spec.template.metadata.labels."app.kubernetes.io/name" == "agent-factory"' "the pod label SP2's broker CNP admits"
ykind Deployment "$(vol rooms-token).projected.sources[0].serviceAccountToken.audience == \"rooms-system\"" "the broker's system audience (SP2 P3)"
ykind Deployment "$(mnt rooms-token).mountPath == \"/var/run/secrets/agents/rooms\"" "the rooms token where config.broker.tokenFile reads it"
ykind Deployment "$(vol broker-ca).secret.secretName == \"openbao-ca\"" "the broker's CA (ruling SC)"
ykind Deployment "$(mnt broker-ca).mountPath == \"/etc/agent-factory/openbao-ca\"" "the CA where config.broker.caFile reads it"
ykind Deployment "$(vol oidc).secret.secretName == \"agent-factory-oidc\"" "the ZITADEL client ids the API reads tokens from"
ykind Deployment "$(mnt oidc).mountPath == \"/etc/agent-factory-oidc\"" "the client ids where config.api.clientIDFiles reads them"
ykind Deployment "$ctr.env[] | select(.name == \"FACTORY_CONFIG\") | .value == \"/etc/agent-factory/config/config.yaml\"" "FACTORY_CONFIG names the rendered config"
ykind Deployment "$(mnt config).mountPath == \"/etc/agent-factory/config\"" "the config mounted where FACTORY_CONFIG points"
check "name: api" "the run-request API port"
api_port="$(yq 'select(.kind == "Service") | .spec.ports[] | select(.name == "api") | .port' <<<"$out")"
intake_port="$(yq 'select(.kind == "Service") | .spec.ports[] | select(.name == "intake") | .port' <<<"$out")"
[ "$api_port" = 8082 ] || { echo "FAIL: the run-request API Service port is 8082, got '$api_port'" >&2; fail=1; }
[ "$api_port" != "$intake_port" ] || { echo "FAIL: the api and intake ports must differ" >&2; fail=1; }
check "containerPort: 8082" "the run-request API container port"
check "name: intake" "the RunLore intake port (FA-8)"
ykind Deployment "$(vol intake).secret.secretName == \"agent-factory-intake\"" "the intake bearer token Secret (FA-8)"
ykind Deployment "$(mnt intake).mountPath == \"/etc/agent-factory-intake\"" "the intake token where config.runlore.tokenFile reads it"
ykind Deployment "$(vol zitadel-reader).secret.secretName == \"agent-factory-zitadel-reader\"" "the ZITADEL reader D4 resolves callers with"
ykind Deployment "$(vol zitadel-reader).secret.optional == true" "an absent reader refuses human runs, never blocks the pod"
ykind Deployment "$(mnt zitadel-reader).mountPath == \"/etc/agent-factory-zitadel-reader\"" "the reader where config.api.access.readerFile reads it"
ykind ConfigMap '.data."config.yaml" | from_yaml | .repository == "Smana/cloud-native-ref"' "the config is rendered verbatim"
ykind Deployment "$ctr.image == \"ghcr.io/smana/agent-factory:$digest\"" "the image by digest"
ykind Deployment "$ctr.securityContext.readOnlyRootFilesystem == true" "restricted securityContext"
ykind Deployment "$pod.securityContext.seccompProfile.type == \"RuntimeDefault\"" "seccomp RuntimeDefault"
ykind Deployment "($ctr.securityContext.capabilities.drop | join(\",\")) == \"ALL\"" "every capability dropped"
ykind Deployment "$ctr.startupProbe.httpGet.path == \"/startupz\"" "startup probe"
ykind Deployment "$ctr.livenessProbe.httpGet.path == \"/healthz\"" "liveness probe"
ykind Deployment "$ctr.readinessProbe.httpGet.path == \"/readyz\"" "readiness probe"
ykind Deployment "($ctr.resources.limits.cpu != null and $ctr.resources.limits.memory != null)" "resource limits"
ykind Deployment "($ctr.resources.requests.cpu != null and $ctr.resources.requests.memory != null)" "resource requests"
ykind PodDisruptionBudget '.spec.minAvailable == 1' "PDB"
ykind PodDisruptionBudget '.spec.unhealthyPodEvictionPolicy == "AlwaysAllow"' "a drain evicts unready pods (#58)"
ykind Deployment "$(vol merger).secret | (.secretName == \"agent-factory-merger\" and .defaultMode == \"0440\" and .optional == true)" "the merger key is optional (#57)"
ykind Deployment "$(vol intake).secret | (.secretName == \"agent-factory-intake\" and .defaultMode == \"0440\" and .optional == true)" "the intake token is optional (#57)"
ykind Role '.rules[] | select(.resources[] == "agentruns") | has("verbs")' "AgentRun RBAC"
# Review C1: a 0400 root-owned key is unreadable to uid 65532; the group bit plus fsGroup is not.
ykind Deployment "$pod.securityContext.fsGroup == 65532" "the pod's fsGroup owns the mounted Secrets"
ykind Deployment "$(vol github).secret | (.secretName == \"agent-factory-github\" and .defaultMode == \"0440\")" "the App key readable by the group only"
ykind Deployment "$(mnt merger).mountPath == \"/etc/agent-factory-merger\"" "the merger key (R16) where config.github.merger*File reads it"
# Review M4: exactly the verbs the code uses.
ykind Role '.rules[] | select(.resources[] == "tasks") | ((.verbs | sort | join(",")) == "create,get,list,patch,update,watch")' "tasks without delete"
ykind Role '.rules[] | select(.resources[] == "leases") | ((.verbs | sort | join(",")) == "create,get,update")' "leases: what leader election calls"
# ConfigMaps: one rule, bounded by the Role's namespace (the release's own, so no metadata.namespace
# in the render). A resourceNames rule would be subsumed by the ledger rule's wider one.
role=$(yq eval-all 'select(.kind == "Role" and .metadata.namespace == null)' - <<<"$out")
n=$(yq '[.rules[] | select(.resources[] == "configmaps")] | length' - <<<"$role")
[ "$n" = 1 ] || { echo "FAIL: want one configmaps rule, got $n" >&2; fail=1; }
yq -e '.rules[] | select(.resources[] == "configmaps") | ((has("resourceNames") | not) and ((.verbs | sort | join(",")) == "create,delete,get,list,update,watch"))' - <<<"$role" >/dev/null \
  || { echo "FAIL: the configmaps rule: no resourceNames, verbs = stop object's watch + R50 ledgers" >&2; fail=1; }
ynone 'select(.kind == "Role" or .kind == "ClusterRole") | .rules[] | select(.resources[] == "secrets")' "the factory never reads Secrets through the API"
# defaultMode renders as 0400 or 0440 (yq keeps the octal spelling); only the group-readable one is allowed.
ynone 'select(.kind == "Deployment") | .spec.template.spec.volumes[].secret.defaultMode | select(. == "0400" or . == "400")' "a 0400 Secret is root-only: the factory runs as 65532"
for bad in "" "v0.0.0" "latest"; do
  if helm template agent-factory . --namespace agent-system --set image.tag="$bad" >/dev/null 2>&1; then
    echo "FAIL: image.tag '$bad' must not render: only <version>@sha256:<digest>" >&2; fail=1
  fi
done
[ "$fail" -eq 0 ] && echo PASS
exit "$fail"
