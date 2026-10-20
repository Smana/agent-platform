#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Download gVisor (runsc + containerd-shim-runsc-v1) into .quickstart/bin, verified against
# the published sha512. Idempotent: a verified bundle for the pinned release is left alone.
# Recent gVisor releases publish only gvisor.tar.bz2, not the bare binaries.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
# shellcheck source=hack/quickstart/versions.env
. hack/quickstart/versions.env

case "$(uname -m)" in
  x86_64 | amd64) arch=x86_64 ;;
  aarch64 | arm64) arch=aarch64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

dir=.quickstart/bin
stamp="$dir/.gvisor-release"
want="${GVISOR_RELEASE}-${arch}"
if [[ -x $dir/runsc && -x $dir/containerd-shim-runsc-v1 && "$(cat "$stamp" 2>/dev/null)" == "$want" ]]; then
  echo "runsc ${want} already present"
  exit 0
fi

base="https://storage.googleapis.com/gvisor/releases/release/${GVISOR_RELEASE}/${arch}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSLo "$tmp/gvisor.tar.bz2" "$base/gvisor.tar.bz2"
curl -fsSLo "$tmp/gvisor.tar.bz2.sha512" "$base/gvisor.tar.bz2.sha512"
(cd "$tmp" && sha512sum -c gvisor.tar.bz2.sha512)

rm -rf "$dir"
mkdir -p "$dir"
tar -xjf "$tmp/gvisor.tar.bz2" -C "$dir"
chmod +x "$dir/runsc" "$dir/containerd-shim-runsc-v1"
echo "$want" >"$stamp"
echo "runsc ${want} ready in $dir"
