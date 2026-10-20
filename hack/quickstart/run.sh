#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Start a Room and an AgentRun in it. Needs REPO (owner/name) and TASK.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
: "${REPO:?set REPO=owner/name}" "${TASK:?set TASK}"
[[ "$REPO" =~ ^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$ ]] || { echo "REPO is owner/name" >&2; exit 1; }
id() { head -c5 /dev/urandom | base32 | tr '[:upper:]' '[:lower:]' | tr -d =; }
ROOM_ID=$(id) RUN_ID=$(id)
# A JSON string is a valid YAML scalar: quoting survives any TASK text.
TASK_JSON=$(printf '%s' "$TASK" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')
export ROOM_ID RUN_ID REPO TASK_JSON
# shellcheck disable=SC2016 # envsubst takes a literal variable list
render() { envsubst '${ROOM_ID} ${RUN_ID} ${REPO} ${TASK_JSON}' <"hack/quickstart/manifests/$1.yaml.tmpl"; }
render room | kubectl apply -f -
render agentrun | kubectl apply -f -
echo "room: http://localhost:8080/r/${ROOM_ID}   (run: kubectl -n agents get agentrun run-${RUN_ID})"
