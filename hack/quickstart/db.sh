#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# The rooms database for the quickstart: CNPG Cluster + managed roles + the two DSN Secrets.
# Idempotent: passwords are generated once and kept. Usage: db.sh <namespace>
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
ns="${1:?usage: db.sh <namespace>}"

for role in rooms-broker rooms-retention; do
  kubectl -n "$ns" get secret "$role-password" >/dev/null 2>&1 ||
    kubectl -n "$ns" create secret generic "$role-password" --type=kubernetes.io/basic-auth \
      --from-literal=username="${role//-/_}" --from-literal=password="$(openssl rand -hex 24)"
done

kubectl -n "$ns" apply -f hack/quickstart/manifests/cnpg-cluster.yaml.tmpl
kubectl -n "$ns" wait --for=condition=Ready cluster/rooms --timeout=300s

owner_uri=$(kubectl -n "$ns" get secret rooms-app -o jsonpath='{.data.uri}' | base64 -d)
pw=$(kubectl -n "$ns" get secret rooms-broker-password -o jsonpath='{.data.password}' | base64 -d)
kubectl -n "$ns" create secret generic rooms-owner-dsn --from-literal=uri="$owner_uri" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$ns" create secret generic rooms-broker-dsn \
  --from-literal=uri="postgresql://rooms_broker:${pw}@rooms-rw.${ns}.svc:5432/rooms?sslmode=require" --dry-run=client -o yaml | kubectl apply -f -
