#!/usr/bin/env bash
# scripts/stall-steps.sh against a fake gh and a stall-steps.sql output: only runs named by a merged,
# unreverted agent PR count as accepted; N stays 80 below 20 of them, then sits above their p90.
set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
pr() { printf '{"number":%s,"commits":{"nodes":[%s]}}' "$1" "$2"; }
commit() { printf '{"commit":{"message":"fix: x\\n\\n---\\nAgent-Run: %s\\nAgent-Task: t1"}}' "$1"; }
page() { printf '{"data":{"search":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[%s]}}}\n' "$1"; }
fake_gh() {
  cat > "$tmp/gh" <<EOF2
#!/usr/bin/env bash
case "\$*" in *"Revert in:title"*) cat "$tmp/reverts.json" ;; *"api graphql --paginate"*) cat "$tmp/prs.json" ;; *) exit 9 ;; esac
EOF2
  chmod +x "$tmp/gh"
}
check() {
  local out
  out=$(GH="$tmp/gh" scripts/stall-steps.sh --steps "$tmp/steps.csv")
  if [ "$out" != "$1" ]; then printf 'got:\n%s\nwant:\n%s\n' "$out" "$1"; exit 1; fi
}

# Few accepted runs: the default. #2 was reverted; #3's commit names no run.
# Two pages, as --paginate prints them.
{ page "$(pr 1 "$(commit aaaaaaaa),$(commit bbbbbbbb)"),$(pr 2 "$(commit cccccccc)")"
  page "$(pr 3 '{"commit":{"message":"chore: y"}}')"; } > "$tmp/prs.json"
echo '[{"body":"Reverts Smana/cloud-native-ref#2","labels":[],"state":"MERGED"}]' > "$tmp/reverts.json"
printf 'run_id,steps,first_commit_step\naaaaaaaa,30,12\nbbbbbbbb,50,45\ncccccccc,20,5\ndddddddd,319,\neeeeeeee,90,\n' > "$tmp/steps.csv"
fake_gh
check 'accepted runs: 2, with a first commit in the room log: 2
steps to the first commit: p50 12, p90 45, max 45
N: 80 (the default: 2 accepted runs, 20 needed)
accepted runs N would have stopped: 0 of 2
implementer runs past N with no commit: 2 of 5'

# Twenty accepted runs, first commits at 5, 10 … 95 and 150: p90 is 90, so N is 100.
prs="" csv="run_id,steps,first_commit_step"
for i in $(seq 1 20); do
  run=$(printf 'run%05d' "$i" | tr 0-9 a-j | tr ij 23); first=$((i * 5)); [ "$i" -eq 20 ] && first=150
  prs="$prs${prs:+,}$(pr "$i" "$(commit "$run")")"; csv="$csv
$run,$((first + 10)),$first"
done
page "$prs" > "$tmp/prs.json"; echo '[]' > "$tmp/reverts.json"; printf '%s\n' "$csv" > "$tmp/steps.csv"
check 'accepted runs: 20, with a first commit in the room log: 20
steps to the first commit: p50 50, p90 90, max 150
N: 100 (above the p90)
accepted runs N would have stopped: 1 of 20
implementer runs past N with no commit: 0 of 20'

if scripts/stall-steps.sh 2>/dev/null; then echo "--steps is required"; exit 1; fi
echo ok
