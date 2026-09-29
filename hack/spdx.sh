#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Every Go file, tracked or new, opens with the SPDX license line.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

want='// SPDX-License-Identifier: Apache-2.0'
missing=()
while IFS= read -r f; do
  [[ -f "$f" ]] || continue # deleted in the working tree
  [[ "$(head -n 1 "$f")" == "$want" ]] || missing+=("$f")
done < <(git ls-files --cached --others --exclude-standard -- '*.go')

if ((${#missing[@]})); then
  echo "Missing '$want' as the first line:" >&2
  printf '  %s\n' "${missing[@]}" >&2
  exit 1
fi
