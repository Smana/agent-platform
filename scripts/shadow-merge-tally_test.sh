#!/usr/bin/env bash
# scripts/shadow-merge-tally.sh against a fake gh: only the factory's head-pinned would-arm
# comments count, the last one decides, and a merge at another head, a revert or an auto-merge
# is not "merged unchanged".
set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
wa() { printf '{"author":{"login":"%s"},"createdAt":"%s","body":"would auto-merge: `%s` <!-- agent-factory scope=x event=would-arm-1 head=%s -->"}' "$1" "$2" "$3" "$4"; }
cat > "$tmp/prs.json" <<JSON
[
 {"number":1,"headRefOid":"aaa","mergedBy":{"login":"Smana"},"comments":[$(wa ogenki-agent-factory 2026-10-01T00:00:00Z docs-links aaa)]},
 {"number":2,"headRefOid":"bbb","mergedBy":{"login":"Smana"},"comments":[$(wa app/ogenki-agent-factory 2026-10-01T00:00:00Z docs-links aaa)]},
 {"number":3,"headRefOid":"ccc","mergedBy":{"login":"Smana"},"comments":[$(wa ogenki-agent-factory 2026-10-01T00:00:00Z docs-links bbb),$(wa ogenki-agent-factory 2026-10-02T00:00:00Z docs-links ccc)]},
 {"number":4,"headRefOid":"ddd","mergedBy":{"login":"Smana"},"comments":[$(wa someone 2026-10-01T00:00:00Z docs-links ddd)]},
 {"number":5,"headRefOid":"eee","mergedBy":{"login":"Smana"},"comments":[$(wa ogenki-agent-factory 2026-10-01T00:00:00Z review eee)]},
 {"number":6,"headRefOid":"fff","mergedBy":{"login":"Smana"},"comments":[$(wa ogenki-agent-factory 2026-10-01T00:00:00Z docs-links fff)]},
 {"number":7,"headRefOid":"abc","mergedBy":{"login":"ogenki-agent-merger"},"comments":[$(wa ogenki-agent-factory 2026-10-01T00:00:00Z docs-links abc)]},
 {"number":8,"headRefOid":"999","mergedBy":{"login":"Smana"},"comments":[{"author":{"login":"ogenki-agent-factory"},"createdAt":"2026-10-01T00:00:00Z","body":"would auto-merge: \`docs-links\` <!-- agent-factory scope=x event=would-arm-1 -->"}]}
]
JSON
cat > "$tmp/reverts.json" <<'JSON'
[{"body":"Reverts Smana/cloud-native-ref#6","labels":[],"state":"MERGED"},
 {"body":"","labels":[{"name":"factory/revert-of:#1"}],"state":"CLOSED"}]
JSON
cat > "$tmp/gh" <<EOF2
#!/usr/bin/env bash
case "\$*" in *"Revert in:title"*) cat "$tmp/reverts.json" ;; *) cat "$tmp/prs.json" ;; esac
EOF2
chmod +x "$tmp/gh"
out=$(GH="$tmp/gh" scripts/shadow-merge-tally.sh --class docs-links)
want='#1 merged_unchanged
#2 changed
#3 merged_unchanged
#6 reverted
#7 auto_merged
merged_unchanged=2 changed=1 reverted=1 auto_merged=1'
if [ "$out" != "$want" ]; then printf 'got:\n%s\nwant:\n%s\n' "$out" "$want"; exit 1; fi
if scripts/shadow-merge-tally.sh 2>/dev/null; then echo "--class is required"; exit 1; fi

# --author reads another opener's pull requests: Renovate's, judged by WouldArmAdopted comments.
cat > "$tmp/gh-adopted" <<EOF3
#!/usr/bin/env bash
echo "\$*" >> "$tmp/adopted.log"
case "\$*" in *"Revert in:title"*) echo '[]' ;; *) cat scripts/testdata/tally/prs-adopted.json ;; esac
EOF3
chmod +x "$tmp/gh-adopted"
out=$(GH="$tmp/gh-adopted" scripts/shadow-merge-tally.sh --class deps-patch --author app/renovate)
want='#10 merged_unchanged
#11 changed
merged_unchanged=1 changed=1 reverted=0 auto_merged=0'
if [ "$out" != "$want" ]; then printf 'got:\n%s\nwant:\n%s\n' "$out" "$want"; exit 1; fi
grep -qF -- '--author app/renovate' "$tmp/adopted.log" || { echo "FAIL: --author reaches the pull request list" >&2; exit 1; }

# Offline proof 7: the bound on fixtures. 0 bad in 28 fails 10%, 0 in 29 passes; 1 in 46 passes;
# immature (merged under followUpWindow ago) and inconclusive merges are left out of the bound. A
# stub gh serves scripts/testdata/tally/, so nothing reaches GitHub.
bound() { python3 -I scripts/clopper-pearson.py "$1" "$2" | awk '{print ($1 < 0.10) ? "pass" : "fail"}'; }
expect() { [ "$1" = "$2" ] || { echo "FAIL: $3: got $1, want $2" >&2; exit 1; }; }
expect "$(bound 0 28)" fail "0 in 28 is not under 10%"
expect "$(bound 0 29)" pass "0 in 29 is under 10%"
expect "$(bound 1 46)" pass "1 in 46 is under 10%"
expect "$(bound 1 45)" fail "1 in 45 is not"
expect "$(bound 2 61)" pass "2 in 61 is under 10%"
export GH_LOG="$tmp/gh.log"
tally() { GH=scripts/testdata/tally/gh-stub.sh NOW=2026-10-23T00:00:00Z scripts/shadow-merge-tally.sh \
  --class dead-links --outcomes --since 2026-08-01 --window 168h; }
out="$(tally)"
expect "$(grep -c '^#' <<<"$out")" 31 "31 merges listed"
expect "$(grep -o 'mature=[0-9]*' <<<"$out")" mature=30 "an immature merge left out"
expect "$(grep -o ' conclusive=[0-9]*' <<<"$out")" " conclusive=29" "the bound's merges: mature and conclusive"
expect "$(grep -o ' inconclusive=[0-9]*' <<<"$out")" " inconclusive=1" "an inconclusive merge counted apart"
expect "$(grep -o 'bad=[0-9]*' <<<"$out")" bad=0 "no bad outcome"
expect "$(grep -o 'widen=[a-z]*' <<<"$out")" widen=yes "0 bad in 29 conclusive mature merges: widen"
out="$(TALLY_FIXTURE=regression tally)"
expect "$(grep -o 'bad=[0-9]*' <<<"$out") $(grep -o 'regression=[0-9]*' <<<"$out")" "bad=1 regression=1" "a regression label is a bad outcome"
expect "$(grep -o 'widen=[a-z]*' <<<"$out")" widen=no "1 bad in 29: no widening"
# A follow-up is a later PR carrying factory/follow-up-of:#<n>, which is not one of the class's merges.
out="$(TALLY_FIXTURE=followup tally)"
expect "$(grep -o 'bad=[0-9]*' <<<"$out") $(grep -o 'follow_up=[0-9]*' <<<"$out")" "bad=1 follow_up=1" "a follow-up label on a later PR is a bad outcome"
expect "$(grep -o 'widen=[a-z]*' <<<"$out")" widen=no "a follow-up blocks widening"
grep -qF 'label:"factory/follow-up-of:#100"' "$GH_LOG" || { echo "FAIL: the follow-up search quotes the label (L11)" >&2; exit 1; }
# A posted regression verdict counts as bad even when its label write failed.
out="$(TALLY_FIXTURE=regcomment tally)"
expect "$(grep -o 'bad=[0-9]*' <<<"$out") $(grep -o 'regression=[0-9]*' <<<"$out")" "bad=1 regression=1" "a regression comment without its label is bad"
# --window is whole hours: anything else is refused, not miscomputed.
for w in 7d 72h30m 168; do
  if GH=scripts/testdata/tally/gh-stub.sh scripts/shadow-merge-tally.sh --class dead-links --outcomes --since 2026-08-01 --window "$w" >/dev/null 2>&1; then
    echo "FAIL: --window $w is refused" >&2; exit 1
  fi
done
grep -qF 'label:"factory/merged:dead-links"' "$GH_LOG" || { echo "FAIL: the search quotes the label (L11)" >&2; exit 1; }
echo ok
