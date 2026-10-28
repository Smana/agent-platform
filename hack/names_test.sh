#!/usr/bin/env bash
# hack/names.sh refuses an old artifact name outside the historical docs, and passes the
# identities that keep their names (the maku rebrand, phase 1, plan D1 and D6).
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
git -C "$tmp" init -q
mkdir -p "$tmp/docs/superpowers/plans" "$tmp/internal/humanapi"
printf 'old plan: ghcr.io/smana/room-broker and roomctl\n' > "$tmp/docs/superpowers/plans/p.md"
printf 'mux.HandleFunc("GET /api/roomctl", s.setup)\nViaRoomctl = "roomctl"\n' > "$tmp/internal/humanapi/server.go"
printf 'The audience room-broker and the Service room-broker.agent-system keep their names.\n' > "$tmp/docs/ok.md"
printf 'Releases before v0.30.0 carry the `Smana/agent-platform` identity.\n' > "$tmp/docs/history.md"
bash "$here/names.sh" "$tmp"
printf 'image: ghcr.io/smana/room-broker:v0.29.9\n' > "$tmp/docs/bad-image.md"
printf 'run roomctl status\n' > "$tmp/docs/bad-cli.md"
printf 'import "github.com/Smana/agent-platform/internal/store"\n' > "$tmp/bad.go"
printf 'see cmd/agent-factory/main.go\n' > "$tmp/docs/bad-path.md"
out=$(bash "$here/names.sh" "$tmp" 2>&1 || true)
for f in docs/bad-image.md docs/bad-cli.md bad.go docs/bad-path.md; do
  grep -q "^$f:1:" <<<"$out" || { echo "names.sh missed $f"; exit 1; }
done
grep -q 'FAIL: an old name' <<<"$out"
if grep -qE 'superpowers|humanapi|ok\.md|history\.md' <<<"$out"; then echo "unexpected match: $out"; exit 1; fi
echo "==> names_test: ok"
