#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Renders the umbrella chart and asserts what the platform relies on: the run kind comes from
# `runs` alone, the broker's ports, probes and RBAC, tokenreview's binding only in tokenreview
# mode, default-deny in the runs namespace, and an image by digest unless e2e says otherwise.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
helm dependency build . >/dev/null
digest='v0.0.0@sha256:0000'
fail=0
out=''
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

q="$(helm template ap . -n agent-platform -f values-quickstart.yaml \
     --set broker.image.tag=$digest --set runController.image.tag=$digest)"
out="$q"
# runs block → broker config and run issuer agree
check 'namePrefix: run-' 'broker config carries runs.namePrefix'
check "subPattern: '^system:serviceaccount:agents:run-([a-z2-7]{8})\$'" 'the run issuer pattern is derived from runs'
ykind ConfigMap '.data."config.yaml" | from_yaml | (.human.mode == "local" and .publicURL == "http://localhost:8080")' 'local mode config'
ykind Deployment 'select(.metadata.name=="room-broker") | (.spec.template.spec.initContainers[0].args | join(",")) == "migrate"' 'migrate init container'
ykind Deployment 'select(.metadata.name=="room-broker") | (.spec.template.spec.containers[0].ports | map(.containerPort) | contains([8443,8080,9090]))' 'broker ports'
ykind Deployment 'select(.metadata.name=="room-broker") | (.spec.template.spec.containers[0] | (.livenessProbe.httpGet.path == "/healthz" and .readinessProbe.httpGet.path == "/readyz" and .startupProbe.httpGet.path == "/startupz" and .readinessProbe.httpGet.port == 9090))' 'probes on the ops port'
ykind Deployment 'select(.metadata.name=="room-broker") | .spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem == true' 'restricted securityContext'
ykind Deployment 'select(.metadata.name=="room-broker") | (.spec.template.spec.containers[0].env | map(.name) | contains(["ROOMS_CONFIG","ROOMS_DATABASE_URL","POD_NAMESPACE"]))' 'the three env vars brokerEnv requires'
ykind Service 'select(.metadata.name=="room-broker") | ((.spec.ports | map(.port) | contains([8443,9090])) and ((.spec.ports | map(.port) | contains([8080])) == false))' 'local mode does not expose :8080'
ykind ClusterRoleBinding 'select(.roleRef.name=="system:auth-delegator") | .subjects[0].name == "room-broker"' 'tokenreview RBAC'
ykind Role 'select(.metadata.namespace=="agents") | .rules[] | select(.resources[]=="agentruns") | (.apiGroups[0] == "agents.ogenki.io" and ((.verbs|sort|join(",")) == "delete,get,list,patch,watch"))' 'AgentRun verbs in the runs namespace'
ykind Role 'select(.metadata.namespace==null) | .rules[] | select(.resources[]=="rooms") | ((.verbs|sort|join(",")) == "create,get,list,update,watch")' 'Room verbs (create: new room and fork)'
ykind CiliumNetworkPolicy 'select(.metadata.name=="room-broker") | .spec.egress[] | select(.toEntities[0]=="kube-apiserver") | (.toPorts[0].ports | map(.port) | join(",")) == "443,6443"' 'the apiserver on 443 and kind'
ykind Deployment 'select(.metadata.name=="room-broker") | .spec.template.metadata.annotations."checksum/config" | length == 64' 'a config change rolls the broker'
ykind Certificate '.spec.issuerRef.name == "quickstart-ca"' 'the Certificate issuer'
ykind Namespace '.metadata.name == "agents"' 'the runs namespace'
ykind CiliumNetworkPolicy 'select(.metadata.namespace=="agents") | (.spec.endpointSelector | length) == 0' 'default-deny in the runs namespace'
ykind CiliumNetworkPolicy 'select(.metadata.namespace=="agents") | ((.spec.ingress | length) == 1 and (.spec.ingress[0] | length) == 0 and (.spec.egress | length) == 1 and (.spec.egress[0] | length) == 0)' 'default-deny: enforcement on, no peer allowed (the empty-rule idiom)'
ykind CiliumNetworkPolicy 'select(.metadata.name=="room-broker") | .spec.endpointSelector.matchLabels."app.kubernetes.io/name" == "room-broker"' 'broker CNP selects the broker'

# production-shaped render: no migrate, no tokenreview binding
p="$(helm template ap . -n agent-system --set broker.image.tag=$digest --set runController.enabled=false \
     --set broker.runIssuer.mode=jwks --set broker.runIssuer.issuer=https://oidc --set broker.runIssuer.jwksURL=https://oidc/keys \
     --set broker.database.brokerSecret=dsn --set broker.publicURL=https://rooms.example.com \
     --set broker.human.issuer=https://idp.example.com --set broker.human.jwksURL=https://idp.example.com/keys \
     --set broker.human.clientIDFile=/c --set broker.human.projectIDFile=/p --set broker.human.origin=https://rooms.example.com \
     --set broker.human.groups.admin=a --set broker.human.groups.member=m \
     --set runs.group=cloud.ogenki.io --set runs.namePrefix=xplane-run-)"
out="$p"
ynone 'select(.kind=="ClusterRoleBinding")' 'jwks mode binds no auth-delegator'
ynone 'select(.kind=="Deployment") | .spec.template.spec.initContainers | select(. != null)' 'no owner Secret, no migrate'
check 'xplane-run-' 'the production prefix flows through'
check 'cloud.ogenki.io' 'the production group flows through'
ykind Service 'select(.metadata.name=="room-broker") | (.spec.ports | map(.port) | contains([8443,8080,9090]))' 'oidc mode exposes :8080'

# the runs namespace brought by the user keeps its default-deny
out="$(helm template ap . -n x -f values-quickstart.yaml --set createRunsNamespace=false --set broker.image.tag=$digest --set runController.image.tag=$digest)"
ykind CiliumNetworkPolicy 'select(.metadata.namespace=="agents") | (.spec.endpointSelector | length) == 0' 'default-deny without the Namespace object'
ynone 'select(.kind=="Namespace")' 'createRunsNamespace=false renders no Namespace'

# a misconfiguration is a helm error
for bad in '--set broker.human.mode=bogus' '--set broker.runIssuer.mode=tokenReview' '--set broker.database.brokerSecret='; do
  # shellcheck disable=SC2086
  if helm template ap . -n x -f values-quickstart.yaml --set broker.image.tag=$digest $bad >/dev/null 2>&1; then
    echo "FAIL: $bad must not render" >&2; fail=1; fi
done

# both rendered configs load through the broker's real loader
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
yq 'select(.kind=="ConfigMap") | .data."config.yaml"' - <<<"$q" >"$tmp/quickstart.yaml"
yq 'select(.kind=="ConfigMap") | .data."config.yaml"' - <<<"$p" >"$tmp/production.yaml"
go run ./tests/loadconfig "$tmp/quickstart.yaml" "$tmp/production.yaml" || fail=1

# a bare tag is refused unless e2e.unpinnedImages
if helm template ap . -n x -f values-quickstart.yaml --set broker.image.tag=v1 --set runController.image.tag=$digest >/dev/null 2>&1; then
  echo 'FAIL: a bare image tag must be refused' >&2; fail=1; fi
helm template ap . -n x -f values-quickstart.yaml --set e2e.unpinnedImages=true --set broker.image.tag=e2e --set runController.image.tag=e2e \
  | grep -q 'imagePullPolicy: Never' || { echo 'FAIL: e2e.unpinnedImages renders Never' >&2; fail=1; }
[ "$fail" -eq 0 ] && echo PASS
exit "$fail"
