#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# The rooms database for the quickstart: CNPG Cluster + managed roles + the two DSN Secrets.
# Idempotent: passwords are generated once and kept. Usage: db.sh <namespace>
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
# shellcheck source=hack/quickstart/lib.sh
. hack/quickstart/lib.sh
ns="${1:?usage: db.sh <namespace>}"

for role in rooms-broker rooms-retention; do
  kubectl -n "$ns" get secret "$role-password" >/dev/null 2>&1 && continue
  export QS_ROLE_USER=${role//-/_} QS_ROLE_PASSWORD
  QS_ROLE_PASSWORD=$(openssl rand -hex 24)
  qs_secret "$ns" "$role-password" kubernetes.io/basic-auth username=QS_ROLE_USER password=QS_ROLE_PASSWORD | kubectl apply -f -
done

qs_retry_webhook 30 2 kubectl -n "$ns" apply -f hack/quickstart/manifests/cnpg-cluster.yaml.tmpl
kubectl -n "$ns" wait --for=condition=Ready cluster/rooms --timeout=300s
# Ready means the primary is healthy; the managed roles are created a moment later, and the broker's
# migrations GRANT to them.
for _ in $(seq 60); do
  reconciled=$(kubectl -n "$ns" get cluster rooms -o jsonpath='{.status.managedRolesStatus.byStatus.reconciled}')
  [[ $reconciled == *rooms_broker* && $reconciled == *rooms_retention* ]] && break
  sleep 2
done
[[ $reconciled == *rooms_broker* && $reconciled == *rooms_retention* ]] || { echo "managed roles not reconciled after 120s: $reconciled" >&2; exit 1; }

export QS_OWNER_URI QS_BROKER_URI
QS_OWNER_URI="$(kubectl -n "$ns" get secret rooms-app -o jsonpath='{.data.uri}' | base64 -d)?sslmode=require"
pw=$(kubectl -n "$ns" get secret rooms-broker-password -o jsonpath='{.data.password}' | base64 -d)
QS_BROKER_URI="postgresql://rooms_broker:${pw}@rooms-rw.${ns}.svc:5432/rooms?sslmode=require"
qs_secret "$ns" rooms-owner-dsn Opaque uri=QS_OWNER_URI | kubectl apply -f -
qs_secret "$ns" rooms-broker-dsn Opaque uri=QS_BROKER_URI | kubectl apply -f -
