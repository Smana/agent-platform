#!/usr/bin/env bash
# scripts/shadow-merge-tally.sh against a fake gh: only the factory's head-pinned would-arm
# comments count, the last one decides, and a merge at another head, a revert or an auto-merge
# is not "merged unchanged". The marker's paused= and capped= (AM4) follow the head.
set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
# shellcheck disable=SC2016 # the backticks are the comment's markdown
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
 {"number":8,"headRefOid":"999","mergedBy":{"login":"Smana"},"comments":[{"author":{"login":"ogenki-agent-factory"},"createdAt":"2026-10-01T00:00:00Z","body":"would auto-merge: \`docs-links\` <!-- agent-factory scope=x event=would-arm-1 -->"}]},
 {"number":9,"headRefOid":"ab9","mergedBy":{"login":"Smana"},"comments":[{"author":{"login":"ogenki-agent-factory"},"createdAt":"2026-10-01T00:00:00Z","body":"would auto-merge: \`docs-links\` <!-- agent-factory scope=x event=would-arm-1 head=ab9 paused=true capped=false -->"}]}
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
#9 merged_unchanged
merged_unchanged=3 changed=1 reverted=1 auto_merged=1 drill=0'
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
merged_unchanged=1 changed=1 reverted=0 auto_merged=0 drill=0'
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
# A drill revert (AM6) is listed apart, never bad: prs-drill.json, reverts-drill.json. #200 is
# healthy, #201 reverted by a factory/drill revert (no verdict: inconclusive), #202 reverted for real.
out="$(TALLY_FIXTURE=drill tally)"
expect "$(grep '^#201' <<<"$out")" "#201 mature none - drill" "a drill-reverted merge is listed as a drill, not reverted"
expect "$(grep -o 'conclusive=.*drill=[0-9]*' <<<"$out")" "conclusive=2 inconclusive=1 bad=1 reverted=1 follow_up=0 regression=0 drill=1" "a drill is counted apart from bad"

# The decision record (AM8-AM10) on prs-decisions.json (merged pull requests, docs-links unless
# said) and reverts-drill.json. The newest factory record of each pull request counts:
#   #21 would-arm a21 files=1/3 lines=12/20 glm-5.3, merged at a21, drill-reverted
#   #22 holdout_red hand-off, then the newer would-arm a22 files=2/3 lines=4/20 glm-5.3, merged at a22
#   #23 would-arm a23 files=1/3 lines=8/20 kimi-k3, merged at b23 (changed)
#   #24 hand-off patch_too_large a24 files=1/3 lines=30/20, merged at a24
#   #25 hand-off patch_too_large a25 files=-/3 lines=-/20 (diff unread), merged at a25
#   #26 hand-off file_not_allowed a26, merged at b26 (changed)
#   #27 hand-off not_mergeable whose record reads arm:armed files=1/3 lines=5/20, merged at a27
#   #28 class review: left out by --class
#   #29 would-arm a29 files=1/3 lines=3/20 reviewer=- (no model named), merged at a29, reverted
#   #31 armed a31 files=1/3 lines=15/20, merged by the merger
#   #32 a record in someone else's comment: left out
cat > "$tmp/gh-decisions" <<'EOF4'
#!/usr/bin/env bash
case "$*" in *"Revert in:title"*) cat scripts/testdata/tally/reverts-drill.json ;; *) cat scripts/testdata/tally/prs-decisions.json ;; esac
EOF4
chmod +x "$tmp/gh-decisions"
decisions() { GH="$tmp/gh-decisions" scripts/shadow-merge-tally.sh --class docs-links "$@"; }
# Would-arm pull requests: #21 and #22 merged unchanged (#21 a drill too), #23 changed, #29 reverted.
# Per model: glm-5.3 #21 #22; kimi-k3 #23; #29 names none.
out="$(decisions)"
want='#21 merged_unchanged drill
#22 merged_unchanged
#23 changed
#29 reverted
merged_unchanged=2 changed=1 reverted=1 auto_merged=0 drill=1
reviewer glm-5.3: merged_unchanged=2 changed=0
reviewer kimi-k3: merged_unchanged=0 changed=1'
if [ "$out" != "$want" ]; then printf 'got:\n%s\nwant:\n%s\n' "$out" "$want"; exit 1; fi
# --replay: the would-arm and arm decisions (#21 #22 #23 #29 #31) and the patch_too_large ones
# (#24, #25 unread). Not #26 (file_not_allowed) nor #27 (a hand-off, never an arm).
# lines=10: would arm #22(4) #23(8) #29(3); refuse #21(12) #24(30) #31(15); #25 unreplayable.
expect "$(decisions --replay lines=10 | tail -1)" "replay lines=10: would_arm=3 would_refuse=3 unreplayable=1" "--replay lines=10"
# lines=40: all six would arm (#24's files 1 is within its cap of 3).
expect "$(decisions --replay lines=40 | tail -1)" "replay lines=40: would_arm=6 would_refuse=0 unreplayable=1" "--replay lines=40"
# files=1: would arm #21 #23 #29 #31; refuse #22 (files 2) and #24 (lines 30 over its cap of 20).
expect "$(decisions --replay files=1 | tail -1)" "replay files=1: would_arm=4 would_refuse=2 unreplayable=1" "--replay files=1"
# --strictness: refused by a class row #24 #25 (patch_too_large) #26 (file_not_allowed); merged
# unchanged at that head #24 #25. #22's holdout_red is not its newest decision; #27 is not a row's.
expect "$(decisions --strictness | tail -1)" "strictness: refused=3 refused_merged_unchanged=2" "--strictness"
for bad in lines files=x size=3 lines=; do
  if decisions --replay "$bad" >/dev/null 2>&1; then echo "FAIL: --replay $bad is refused" >&2; exit 1; fi
done
echo ok
