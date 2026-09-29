#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# golangci-lint v2 under the Go toolchain go.mod pins. Arguments pass through
# (`hack/lint.sh ./internal/store/...`).
#
# Which binary: $GOLANGCI_LINT, else the one mise pins for this repo, else PATH.
# A v1 binary shadowing v2 on PATH is common (`go install …/golangci-lint` puts
# one in $GOPATH/bin), and v1 cannot read a v2 config, so a wrong major fails here
# with the fix instead of deep inside the run.
#
# GOTOOLCHAIN: the staticcheck bundled in golangci-lint panics building its IR
# under a Go newer than the one it was released against, which aborts the whole
# run. Pinning to go.mod's version keeps the full `standard` set; never work
# around it with --disable=staticcheck.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

bin="${GOLANGCI_LINT:-}"
if [[ -z "$bin" ]] && command -v mise >/dev/null 2>&1; then
  bin="$(mise which golangci-lint 2>/dev/null || true)"
fi
bin="${bin:-golangci-lint}"

if ! command -v "$bin" >/dev/null 2>&1; then
  echo "hack/lint.sh: golangci-lint not found; run 'mise trust && mise install' (pinned in mise.toml)" >&2
  exit 127
fi

version="$("$bin" version --short 2>/dev/null || "$bin" --version)"
if [[ "$version" != 2.* && "$version" != v2.* ]]; then
  echo "hack/lint.sh: $(command -v "$bin") is golangci-lint '$version'; this repo needs v2." >&2
  echo "  run 'mise trust && mise install', or set GOLANGCI_LINT=/path/to/golangci-lint-v2" >&2
  exit 1
fi

toolchain="$(awk '/^toolchain /{print $2; exit}' go.mod)"
[[ -n "$toolchain" ]] || toolchain="go$(awk '/^go /{print $2; exit}' go.mod)"

echo "hack/lint.sh: GOTOOLCHAIN=$toolchain $bin run ${*:-./...}"
GOTOOLCHAIN="$toolchain" exec "$bin" run "${@:-./...}"
