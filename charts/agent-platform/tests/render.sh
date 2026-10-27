#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Renders the umbrella chart and asserts what the platform relies on: the run kind comes from
# `runs` alone, the broker's ports, probes and RBAC, tokenreview's binding only in tokenreview
# mode, the run-controller's config, RBAC and CNP, default-deny in the runs namespace, and an image by digest unless e2e says otherwise.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
helm dependency build . >/dev/null
digest='v0.0.0@sha256:0000'
# every render needs the controller's three images; the cases below add only what they test
imgs=(--set broker.image.tag=$digest --set runController.image.tag=$digest
      --set runController.images.harness.tag=$digest --set runController.images.bridge.tag=$digest)
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
     "${imgs[@]}")"
out="$q"
# runs block → broker config and run issuer agree
check 'namePrefix: run-' 'broker config carries runs.namePrefix'
check "subPattern: '^system:serviceaccount:agents:run-([a-z2-7]{8})\$'" 'the run issuer pattern is derived from runs'
ykind ConfigMap 'select(.metadata.name=="room-broker-config") | .data."config.yaml" | from_yaml | (.human.mode == "local" and .publicURL == "http://localhost:8080")' 'local mode config'
ykind ConfigMap 'select(.metadata.name=="room-broker-config") | .data."config.yaml" | from_yaml | .roomTools.enabled == false' 'the quickstart turns the :8090 room tools off'
ykind Deployment 'select(.metadata.name=="room-broker") | (.spec.template.spec.initContainers[0].args | join(",")) == "migrate"' 'migrate init container'
ykind Deployment 'select(.metadata.name=="room-broker") | (.spec.template.spec.initContainers[0].env | ((length == 1) and (.[0].name == "ROOMS_MIGRATION_DATABASE_URL") and (.[0].valueFrom.secretKeyRef.name == "rooms-owner-dsn") and (.[0].valueFrom.secretKeyRef.key == "uri")))' 'the migrate init container reads the owner Secret through ROOMS_MIGRATION_DATABASE_URL only'
ykind Deployment 'select(.metadata.name=="room-broker") | (.spec.template.spec.containers[0].env | map(select(.name == "ROOMS_DATABASE_URL")) | ((length == 1) and (.[0].valueFrom.secretKeyRef.name == "rooms-broker-dsn")))' 'the broker reads its own Secret through ROOMS_DATABASE_URL'
ykind Deployment 'select(.metadata.name=="room-broker") | (.spec.template.spec.containers[0].ports | map(.containerPort) | sort | join(",")) == "8080,8443,9090"' 'broker ports'
ykind Deployment 'select(.metadata.name=="room-broker") | (.spec.template.spec.containers[0] | (.livenessProbe.httpGet.path == "/healthz" and .readinessProbe.httpGet.path == "/readyz" and .startupProbe.httpGet.path == "/startupz" and .readinessProbe.httpGet.port == 9090))' 'probes on the ops port'
ykind Deployment 'select(.metadata.name=="room-broker") | .spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem == true' 'restricted securityContext'
ykind Deployment 'select(.metadata.name=="room-broker") | (.spec.template.spec.containers[0].env | map(.name) | sort | join(",")) == "POD_NAMESPACE,ROOMS_CONFIG,ROOMS_DATABASE_URL"' 'the three env vars brokerEnv requires'
ykind Service 'select(.metadata.name=="room-broker") | (.spec.ports | map(.port) | sort | join(",")) == "8443,9090"' 'local mode does not expose :8080'
ykind ClusterRoleBinding 'select(.roleRef.name=="system:auth-delegator") | .subjects[0].name == "room-broker"' 'tokenreview RBAC'
ykind Role 'select(.metadata.namespace=="agents" and .metadata.name=="room-broker") | .rules[] | select(.resources[]=="agentruns") | (.apiGroups[0] == "agents.ogenki.io" and ((.verbs|sort|join(",")) == "delete,get,list,patch,watch"))' 'AgentRun verbs in the runs namespace'
ykind Role 'select(.metadata.namespace==null) | .rules[] | select(.resources[]=="rooms") | ((.verbs|sort|join(",")) == "create,get,list,update,watch")' 'Room verbs (create: new room and fork)'
ykind CiliumNetworkPolicy 'select(.metadata.name=="room-broker") | .spec.egress[] | select(.toEntities[0]=="kube-apiserver") | (.toPorts[0].ports | map(.port) | join(",")) == "443,6443"' 'the apiserver on 443 and kind'
ykind Deployment 'select(.metadata.name=="room-broker") | .spec.template.metadata.annotations."checksum/config" | length == 64' 'a config change rolls the broker'
ykind Certificate '.spec.issuerRef.name == "quickstart-ca"' 'the Certificate issuer'
ykind Namespace '.metadata.name == "agents"' 'the runs namespace'
ykind Namespace '.metadata.annotations."helm.sh/resource-policy" == "keep"' 'uninstall keeps the runs namespace'
ykind CiliumNetworkPolicy 'select(.metadata.namespace=="agents") | (.spec.endpointSelector | length) == 0' 'default-deny in the runs namespace'
ykind CiliumNetworkPolicy 'select(.metadata.namespace=="agents") | ((.spec.ingress | length) == 1 and (.spec.ingress[0] | length) == 0 and (.spec.egress | length) == 1 and (.spec.egress[0] | length) == 0)' 'default-deny: enforcement on, no peer allowed (the empty-rule idiom)'
ykind CiliumNetworkPolicy 'select(.metadata.name=="room-broker") | .spec.endpointSelector.matchLabels."app.kubernetes.io/name" == "room-broker"' 'broker CNP selects the broker'

# the run-controller: config from `runs`, its own identity, the verbs the reconciler calls
ykind ConfigMap 'select(.metadata.name=="run-controller-config") | .data."config.yaml" | from_yaml | (.namespace == "agents" and .namePrefix == "run-" and .runtimeClassName == "gvisor" and .broker.url == "https://room-broker.agent-platform.svc.cluster.local:8443" and .broker.namespace == "agent-platform" and .broker.caSecret == "room-broker-tls" and .broker.labels."app.kubernetes.io/name" == "room-broker")' 'controller config: runs, FQDN broker URL, broker labels, CA from broker.tls.secretName'
ykind ConfigMap 'select(.metadata.name=="run-controller-config") | .data."config.yaml" | from_yaml | ((.broker.labels | keys | join(",")) == "app.kubernetes.io/name")' 'broker.labels is the one stable selector label (a version label would cut in-flight runs off on upgrade)'
ykind Role 'select(.metadata.namespace=="agents" and .metadata.name=="run-controller") | .rules[] | select(.resources[]=="agentruns/finalizers") | ((.verbs | join(",")) == "update")' 'controller can set blockOwnerDeletion'
ykind Deployment 'select(.metadata.name=="run-controller") | (.spec.template.spec.containers[0].ports | map(.containerPort) | sort | join(",")) == "9090,9091"' 'controller ports 9090/9091'
ykind Deployment 'select(.metadata.name=="run-controller") | (.spec.template.spec.containers[0] | (.livenessProbe.httpGet.port == 9091 and .livenessProbe.httpGet.path == "/healthz" and .readinessProbe.httpGet.port == 9091 and .readinessProbe.httpGet.path == "/readyz"))' 'controller probes on 9091'
ykind Deployment 'select(.metadata.name=="run-controller") | (.spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem == true and .spec.template.metadata.annotations."checksum/config" != null and .spec.template.spec.serviceAccountName == "run-controller")' 'controller: restricted context, config checksum, own SA'
ykind Deployment 'select(.metadata.name=="run-controller") | (.spec.selector.matchLabels."app.kubernetes.io/name" == "run-controller" and .spec.template.metadata.labels."app.kubernetes.io/name" == "run-controller")' 'controller has its own name label'
ykind Role 'select(.metadata.namespace=="agents" and .metadata.name=="run-controller") | (.rules | map(.resources) | flatten | sort | join(",")) == "agentruns,agentruns/finalizers,agentruns/status,ciliumnetworkpolicies,configmaps,events,leases,pods,secrets,serviceaccounts"' 'controller RBAC in the runs namespace'
ykind Role 'select(.metadata.namespace=="agents" and .metadata.name=="run-controller") | .rules[] | select(.resources[]=="pods") | (.verbs | sort | join(",")) == "create,delete,get,list,watch"' 'controller can revoke (delete pods)'
ykind Role 'select(.metadata.namespace=="agents" and .metadata.name=="run-controller") | .rules[] | select(.resources[]=="secrets") | ((.resourceNames | join(",")) == "agent-model,agent-git" and (.verbs | join(",")) == "get")' 'get on the model and git Secrets by name'
ykind Role 'select(.metadata.name=="run-controller-ca") | (.metadata.namespace == null and (.rules[0].resourceNames | join(",")) == "room-broker-tls" and (.rules[0].verbs | join(",")) == "get")' 'get on the CA Secret by name, in the release namespace'
ykind CiliumNetworkPolicy 'select(.metadata.name=="run-controller") | (.spec.endpointSelector.matchLabels."app.kubernetes.io/name" == "run-controller" and (.spec.egress[] | select(.toEntities[0]=="kube-apiserver") | (.toPorts[0].ports | map(.port) | join(",")) == "443,6443"))' 'controller CNP: apiserver on apiServerPorts, own selector'
ykind RuntimeClass '(.handler == "runsc" and .metadata.name == "gvisor")' 'RuntimeClass gvisor'

# production-shaped render: no migrate, no tokenreview binding
p="$(helm template ap . -n agent-system "${imgs[@]}" \
     --set broker.runIssuer.mode=jwks --set broker.runIssuer.issuer=https://oidc --set broker.runIssuer.jwksURL=https://oidc/keys \
     --set broker.database.brokerSecret=dsn --set broker.publicURL=https://rooms.example.com \
     --set broker.human.issuer=https://idp.example.com --set broker.human.jwksURL=https://idp.example.com/keys \
     --set broker.human.clientIDFile=/c --set broker.human.projectIDFile=/p --set broker.human.origin=https://rooms.example.com \
     --set broker.human.groups.admin=a --set broker.human.groups.member=m \
     --set runs.group=cloud.ogenki.io --set runs.namePrefix=xplane-run- \
     --set broker.tls.secretName=broker-cert --set apiServerPorts='{6443}' --set clusterDomain=corp.local)"
out="$p"
ynone 'select(.kind=="ClusterRoleBinding")' 'jwks mode binds no auth-delegator'
ynone 'select(.kind=="RuntimeClass")' 'production renders no RuntimeClass: the cluster-scoped gvisor may already exist'
ynone 'select(.kind=="Deployment") | .spec.template.spec.initContainers | select(. != null)' 'no owner Secret, no migrate'
check 'xplane-run-' 'the production prefix flows through'
check 'cloud.ogenki.io' 'the production group flows through'
ykind Service 'select(.metadata.name=="room-broker") | (.spec.ports | map(.port) | sort | join(",")) == "8080,8443,9090"' 'oidc mode exposes :8080'
ykind ConfigMap 'select(.metadata.name=="room-broker-config") | .data."config.yaml" | from_yaml | .roomTools.enabled == true' 'the default keeps the :8090 room tools on, as every existing deployment runs it'
ykind ConfigMap 'select(.metadata.name=="run-controller-config") | .data."config.yaml" | from_yaml | (.namePrefix == "xplane-run-" and .broker.caSecret == "broker-cert" and .broker.url == "https://room-broker.agent-system.svc.corp.local:8443")' 'controller follows runs, broker.tls.secretName, clusterDomain'
ykind Role 'select(.metadata.name=="run-controller-ca") | (.rules[0].resourceNames | join(",")) == "broker-cert"' 'the CA Role follows broker.tls.secretName'
ykind Role 'select(.metadata.name=="run-controller" and .metadata.namespace=="agents") | .rules[0].apiGroups[0] == "cloud.ogenki.io"' 'controller RBAC follows runs.group'
ykind CiliumNetworkPolicy 'select(.metadata.name=="run-controller") | (.spec.egress[] | select(.toEntities[0]=="kube-apiserver") | (.toPorts[0].ports | map(.port) | join(",")) == "6443")' 'controller CNP follows apiServerPorts'

# two DSNs, two roles, one pod: each container reads the Secret meant for it, through the variable
# its subcommand reads, and the broker never sees the owner's credential
out="$(helm template ap . -n x -f values-quickstart.yaml "${imgs[@]}" \
     --set broker.database.ownerSecret=owner-dsn --set broker.database.brokerSecret=broker-dsn)"
ykind Deployment 'select(.metadata.name=="room-broker") | (.spec.template.spec.initContainers[0].env | ((length == 1) and (.[0].name == "ROOMS_MIGRATION_DATABASE_URL") and (.[0].valueFrom.secretKeyRef.name == "owner-dsn")))' 'the migrate init container takes broker.database.ownerSecret, under ROOMS_MIGRATION_DATABASE_URL'
ykind Deployment 'select(.metadata.name=="room-broker") | (.spec.template.spec.containers[0].env | map(select(.name == "ROOMS_DATABASE_URL")) | ((length == 1) and (.[0].valueFrom.secretKeyRef.name == "broker-dsn")))' 'the broker takes broker.database.brokerSecret, never the owner secret'
ykind Deployment 'select(.metadata.name=="room-broker") | (.spec.template.spec.containers[0].env | map(select(.name == "ROOMS_MIGRATION_DATABASE_URL")) | length == 0)' 'the broker container never sees the migration DSN'

# the runs namespace brought by the user keeps its default-deny
out="$(helm template ap . -n x -f values-quickstart.yaml --set createRunsNamespace=false "${imgs[@]}")"
ykind CiliumNetworkPolicy 'select(.metadata.namespace=="agents") | (.spec.endpointSelector | length) == 0' 'default-deny without the Namespace object'
ynone 'select(.kind=="Namespace")' 'createRunsNamespace=false renders no Namespace'

# the broker Certificate covers the host the controller hands the bridge, on a non-default cluster domain
out="$(helm template ap . -n x -f values-quickstart.yaml "${imgs[@]}" --set clusterDomain=corp.local)"
ykind Certificate '.spec.dnsNames | contains(["room-broker.x.svc.corp.local"])' 'the Certificate names room-broker.<ns>.svc.<clusterDomain>'
ynone '[select(.kind=="Certificate") | .spec.dnsNames[] | select(test("cluster.local"))] | .[]' 'no cluster.local in the Certificate when clusterDomain=corp.local'
bhost="$(yq 'select(.metadata.name=="run-controller-config") | .data."config.yaml" | from_yaml | .broker.url | sub("^https://", "") | sub(":[0-9]+$", "")' - <<<"$out")"
BHOST="$bhost" ykind Certificate '.spec.dnsNames | contains([strenv(BHOST)])' "a dnsName covers the controller's broker.url host ($bhost)"

# a misconfiguration is a helm error, for the intended reason: each case names its message
bad() { # bad 'expected message' --set ...
  local want="$1" err; shift
  if err="$(helm template ap . -n x -f values-quickstart.yaml "${imgs[@]}" "$@" 2>&1 >/dev/null)"; then
    echo "FAIL: $* must not render" >&2; fail=1
  elif ! grep -qF -- "$want" <<<"$err"; then
    echo "FAIL: $* failed, but not with \"$want\": $err" >&2; fail=1
  fi
}
bad 'broker.human.mode "bogus" is not oidc or local' --set broker.human.mode=bogus
bad 'broker.runIssuer.mode "tokenReview" is not jwks or tokenreview' --set broker.runIssuer.mode=tokenReview
bad 'no database' --set broker.database.brokerSecret=
bad 'apiServerPorts is empty' --set apiServerPorts=null
bad 'runController.modelSecret is required' --set runController.modelSecret=
bad 'image.tag is <version>@sha256:<digest>, never a bare tag' --set runController.images.bridge.tag=v1
bad 'image.tag is <version>@sha256:<digest>' --set runController.images.harness.tag=

# both rendered configs load through the broker's real loader
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
yq 'select(.metadata.name=="room-broker-config") | .data."config.yaml"' - <<<"$q" >"$tmp/quickstart.yaml"
yq 'select(.metadata.name=="room-broker-config") | .data."config.yaml"' - <<<"$p" >"$tmp/production.yaml"
yq 'select(.metadata.name=="run-controller-config") | .data."config.yaml"' - <<<"$q" >"$tmp/rc-quickstart.yaml"
yq 'select(.metadata.name=="run-controller-config") | .data."config.yaml"' - <<<"$p" >"$tmp/rc-production.yaml"
go run ./tests/loadconfig broker "$tmp/quickstart.yaml" "$tmp/production.yaml" || fail=1
go run ./tests/loadconfig broker-roomtools-off "$tmp/quickstart.yaml" || fail=1
go run ./tests/loadconfig broker-roomtools-on "$tmp/production.yaml" || fail=1
go run ./tests/loadconfig run-controller "$tmp/rc-quickstart.yaml" "$tmp/rc-production.yaml" || fail=1

# a bare tag is refused unless e2e.unpinnedImages
if helm template ap . -n x -f values-quickstart.yaml "${imgs[@]}" --set broker.image.tag=v1 >/dev/null 2>&1; then
  echo 'FAIL: a bare image tag must be refused' >&2; fail=1; fi
helm template ap . -n x -f values-quickstart.yaml --set e2e.unpinnedImages=true --set broker.image.tag=e2e --set runController.image.tag=e2e \
  --set runController.images.harness.tag=e2e --set runController.images.bridge.tag=e2e \
  | grep -q 'imagePullPolicy: Never' || { echo 'FAIL: e2e.unpinnedImages renders Never' >&2; fail=1; }
[ "$fail" -eq 0 ] && echo PASS
exit "$fail"
