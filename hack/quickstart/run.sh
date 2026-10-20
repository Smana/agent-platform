#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Start a Room and an AgentRun in it. Needs REPO (owner/name) and TASK.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
for t in kubectl envsubst base32 yq; do command -v "$t" >/dev/null || { echo "missing tool: $t" >&2; exit 1; }; done
[ -f .quickstart/kubeconfig ] || { echo "no cluster: run task quickstart first" >&2; exit 1; }
export KUBECONFIG="$PWD/.quickstart/kubeconfig"
: "${REPO:?set REPO=owner/name}" "${TASK:?set TASK}"
[[ "$REPO" =~ ^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$ ]] || { echo "REPO is owner/name" >&2; exit 1; }
id() { head -c5 /dev/urandom | base32 | tr '[:upper:]' '[:lower:]' | tr -d =; }
ROOM_ID=$(id) RUN_ID=$(id)
export ROOM_ID RUN_ID REPO TASK
# shellcheck disable=SC2016 # envsubst takes a literal variable list
render() { envsubst '${ROOM_ID} ${RUN_ID} ${REPO}' <"hack/quickstart/manifests/$1.yaml.tmpl"; }
render room | kubectl apply -f -
# yq escapes any task text (quotes, newlines, emoji) that envsubst would have to splice into YAML.
render agentrun | yq '.spec.task.text = strenv(TASK)' | kubectl apply -f -
echo "room: http://localhost:8080/r/${ROOM_ID}   (run: kubectl -n agents get agentrun run-${RUN_ID})"
