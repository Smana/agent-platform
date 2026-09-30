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
check "serviceAccountName: agent-factory" "the factory's ServiceAccount is agent-factory"
check "app.kubernetes.io/name: agent-factory" "the pod label SP2's broker CNP admits"
check "audience: rooms-system" "the broker's system audience (SP2 P3)"
check "mountPath: /var/run/secrets/agents/rooms" "the rooms token where config.broker.tokenFile reads it"
check "secretName: openbao-ca" "the broker's CA (ruling SC)"
check "mountPath: /etc/agent-factory/openbao-ca" "the CA where config.broker.caFile reads it"
check "value: /etc/agent-factory/config/config.yaml" "FACTORY_CONFIG names the rendered config"
check "mountPath: /etc/agent-factory/config" "the config mounted where FACTORY_CONFIG points"
check "repository: Smana/cloud-native-ref" "the config is rendered verbatim"
check "image: \"ghcr.io/smana/agent-factory:$digest\"" "the image by digest"
check "readOnlyRootFilesystem: true" "restricted securityContext"
check "type: RuntimeDefault" "seccomp RuntimeDefault"
check "drop: [ALL]" "every capability dropped"
check "path: /startupz" "startup probe"
check "path: /healthz" "liveness probe"
check "path: /readyz" "readiness probe"
check "limits:" "resource limits"
check "requests:" "resource requests"
check "minAvailable: 1" "PDB"
check "resources: [agentruns]" "AgentRun RBAC"
if grep -qF "resources: [secrets]" <<<"$out"; then
  echo "FAIL: the factory never reads Secrets through the API" >&2; fail=1
fi
for bad in "" "v0.0.0" "latest"; do
  if helm template agent-factory . --namespace agent-system --set image.tag="$bad" >/dev/null 2>&1; then
    echo "FAIL: image.tag '$bad' must not render: only <version>@sha256:<digest>" >&2; fail=1
  fi
done
[ "$fail" -eq 0 ] && echo PASS
exit "$fail"
