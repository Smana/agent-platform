#!/usr/bin/env bash
# hack/names.sh refuses an old artifact name outside the historical docs, and passes the
# identities that keep their names (the maku rebrand, phase 1, plan D1 and D6).
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
git -C "$tmp" init -q
# With no instrument found, the gate refuses to run rather than pass every old metric name.
out=$(bash "$here/names.sh" "$tmp" 2>&1 || true)
grep -q 'FAIL: no instrument name found' <<<"$out" || { echo "names.sh ran with no metric list: $out"; exit 1; }
# The fixture gives names.sh its metric list in the temporary tree.
mkdir -p "$tmp/internal/metrics" && printf 'const x = "rooms_events_appended_total"\nconst y = "rooms_fanout_seconds"\n' > "$tmp/internal/metrics/m.go"
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
# Phase 2: the group as a group fails; a label or annotation key and the lease pass (plan D1, D12).
printf 'apiVersion: agents.ogenki.io/v1alpha1\n' > "$tmp/docs/bad-group.md"
printf '  - apiGroups: [agents.ogenki.io]\n' > "$tmp/bad-rbac.yaml"
printf 'kubectl get rooms.agents.ogenki.io\n' > "$tmp/docs/bad-plural.md"
printf 'the `rooms_events_appended_total` counter\n' > "$tmp/docs/bad-metric.md"
printf 'kubectl annotate task x agents.ogenki.io/stop=true; lease `agent-factory.agents.ogenki.io`; maku_rooms_events_appended_total; maku_rooms_fanout_seconds_bucket\n' > "$tmp/docs/ok-keys.md"
printf 'rate(rooms_fanout_seconds_bucket[5m])\n' > "$tmp/docs/bad-bucket.md"
printf 'see config/crd/agents.ogenki.io_rooms.yaml\n' > "$tmp/docs/bad-crdfile.md"
printf 'group: agents.ogenki.io\n' > "$tmp/bad-group.yaml"
out=$(bash "$here/names.sh" "$tmp" 2>&1 || true)
for f in docs/bad-group.md bad-rbac.yaml docs/bad-plural.md docs/bad-metric.md docs/bad-bucket.md docs/bad-crdfile.md bad-group.yaml; do
  grep -q "^$f:1:" <<<"$out" || { echo "names.sh missed $f"; exit 1; }
done
if grep -q 'ok-keys\.md' <<<"$out"; then echo "a key, the lease or a maku_ name was refused: $out"; exit 1; fi
echo "==> names_test: ok"
