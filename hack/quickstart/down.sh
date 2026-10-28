#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
# shellcheck source=hack/quickstart/versions.env
. hack/quickstart/versions.env
kind delete cluster --name "$KIND_CLUSTER"
rm -f .quickstart/kubeconfig
