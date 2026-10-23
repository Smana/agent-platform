#!/usr/bin/env bash
# shadow-merge-tally.sh: the replayable count behind the live auto-merge gate (v2 D3, G4).
#
# A shadow decision is "merged unchanged" when a maintainer merged the pull request at the head
# its last would-arm comment judged (`event=would-arm-<n> head=<sha>`, D2). Only the factory's
# own comments count. Comments from before D2 carry no head and never count.
#
#   scripts/shadow-merge-tally.sh --class docs-links [--repo Smana/cloud-native-ref]
#
# Prints one line per judged pull request, then the totals. Needs gh, authenticated, and jq.
#
#   scripts/shadow-merge-tally.sh --class dead-links --outcomes --since 2026-08-01 [--window 168h] [--budget 0.10]
#
# --outcomes reads what became of the merges of a class instead: the pull requests labelled
# factory/merged:<class> or factory/auto-merged:<class> since a date, each one's revert, the merged
# follow-up fixes (merged pull requests, of any author, labelled factory/follow-up-of:#<n>, one search
# per merge), its factory/monitor:regression label and its newest monitor verdict; a posted
# verdict=regression counts as bad even when its label is missing. A merge is mature once --window has passed (the class's followUpWindow),
# conclusive when it holds a healthy or regression verdict, or a bad outcome of its own. The
# Clopper-Pearson upper bound (95%) on the bad outcomes of the mature, conclusive merges is
# compared with --budget: widen=yes only below it. --window is whole hours (168h). NOW overrides the clock.
set -euo pipefail

class="" outcomes="" since="" window="168h" budget="0.10" repo="${GH_REPO:-Smana/cloud-native-ref}" limit=500
usage() { echo "usage: $0 --class <class> [--repo owner/name] [--limit n] [--outcomes --since <date> [--window 168h] [--budget 0.10]]" >&2; }
author="app/ogenki-agents" factory="ogenki-agent-factory" merger="ogenki-agent-merger"
while [ $# -gt 0 ]; do
  case "$1" in
    --class) class="$2"; shift 2 ;;
    --repo) repo="$2"; shift 2 ;;
    --limit) limit="$2"; shift 2 ;;
    --outcomes) outcomes=1; shift ;;
    --since) since="$2"; shift 2 ;;
    --window) window="$2"; shift 2 ;;
    --budget) budget="$2"; shift 2 ;;
    *) usage; exit 2 ;;
  esac
done
[ -n "$class" ] || { usage; exit 2; }
[[ "$window" =~ ^[0-9]+h$ ]] || { echo "--window is whole hours, like 168h" >&2; exit 2; }
[ -z "$outcomes" ] || [ -n "$since" ] || { echo "--outcomes needs --since <date>" >&2; exit 2; }
gh="${GH:-gh}"

# A revert of any merged PR: GitHub's revert body, or the merger's revert-of label (D1).
reverts_of() {
  "$gh" pr list -R "$repo" --state all --search 'Revert in:title' --limit "$limit" --json body,labels,state
}

if [ -n "$outcomes" ]; then
  # The label is quoted inside the search (L11): unquoted, the ":" and "/" split it and nothing comes back.
  merged_with() {
    "$gh" pr list -R "$repo" --state merged --limit "$limit" --json number,mergedAt,labels,comments \
      --search "label:\"$1:$class\" merged:>=$since"
  }
  prs=$(jq -s 'add | unique_by(.number)' <(merged_with factory/merged) <(merged_with factory/auto-merged))
  # The follow-up label sits on the later fixing pull request, not on a merge of the class: one
  # quoted search per merge (L11) says which merges were followed up.
  followed="{}"
  while read -r n; do
    hit=$("$gh" pr list -R "$repo" --state merged --limit 1 --json number --search "label:\"factory/follow-up-of:#$n\"")
    followed=$(jq -c --arg n "$n" --argjson hit "$hit" '. + {($n): ($hit | length > 0)}' <<<"$followed")
  done < <(jq -r '.[].number' <<<"$prs")
  now="${NOW:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
  tmp=$(mktemp); trap 'rm -f "$tmp"' EXIT
  jq -r --arg repo "$repo" --arg factory "$factory" --arg now "$now" --arg window "${window%h}" \
    --argjson reverts "$(reverts_of)" --argjson followed "$followed" '
    def login: ascii_downcase | sub("^app/"; "") | sub("\\[bot\\]$"; "");
    ([$reverts[] | select(.state != "CLOSED")
      | ((.body // "") | [scan("Reverts \($repo)#([0-9]+)") | .[0] | tonumber]),
        ([.labels[].name | select(startswith("factory/revert-of:#")) | ltrimstr("factory/revert-of:#") | tonumber])
      | .[]] | unique) as $reverted
    | (($now | fromdateiso8601) - ($window | tonumber) * 3600) as $cutoff
    | [ .[] | . as $pr
        | ([.comments[] | select((.author.login | login) == $factory)
            | select(.body | test("event=monitor-[0-9]+ verdict=[a-z]+"))] | sort_by(.createdAt) | last) as $c
        | (if $c == null then "none" else ($c.body | capture("event=monitor-[0-9]+ verdict=(?<v>[a-z]+)").v) end) as $verdict
        | [ (select($reverted | index($pr.number)) | "reverted"),
            (select($followed[$pr.number | tostring]) | "follow_up"),
            (select(([$pr.labels[].name] | index("factory/monitor:regression")) or $verdict == "regression") | "regression") ] as $bad
        | { n: .number, mature: ((.mergedAt | fromdateiso8601) <= $cutoff), verdict: $verdict, bad: $bad,
            conclusive: ($verdict == "healthy" or $verdict == "regression" or ($bad | length) > 0) } ]
    | sort_by(.n)
    | (.[] | "#\(.n) \(if .mature then "mature" else "immature" end) \(.verdict) \(if (.bad | length) > 0 then (.bad | join(",")) else "-" end)"),
      ([.[] | select(.mature)] as $m
       | ($m | map(select(.conclusive))) as $k
       | "merges=\(length) mature=\($m | length) conclusive=\($k | length) inconclusive=\(($m | length) - ($k | length)) "
         + "bad=\([$k[] | select(.bad | length > 0)] | length) reverted=\([$k[] | select(.bad | index("reverted"))] | length) "
         + "follow_up=\([$k[] | select(.bad | index("follow_up"))] | length) regression=\([$k[] | select(.bad | index("regression"))] | length)")
  ' <<<"$prs" > "$tmp"
  sed '$d' "$tmp"
  totals=$(tail -1 "$tmp")
  nk=$(grep -o ' conclusive=[0-9]*' <<<"$totals" | cut -d= -f2) nb=$(grep -o ' bad=[0-9]*' <<<"$totals" | cut -d= -f2)
  bound=$(python3 -I "$(dirname "$0")/clopper-pearson.py" "$nb" "$nk")
  widen=$(awk -v b="$bound" -v m="$budget" 'BEGIN { print (b < m) ? "yes" : "no" }')
  echo "$totals bound=$bound budget=$budget widen=$widen"
  exit 0
fi

prs=$("$gh" pr list -R "$repo" --state merged --author "$author" --limit "$limit" \
  --json number,headRefOid,mergedBy,comments)
# A revert of any merged PR: GitHub's revert body, or the merger's revert-of label (D1).
reverts=$("$gh" pr list -R "$repo" --state all --search 'Revert in:title' --limit "$limit" \
  --json body,labels,state)

jq -r --arg class "$class" --arg repo "$repo" --arg factory "$factory" --arg merger "$merger" \
  --argjson reverts "$reverts" '
  def login: ascii_downcase | sub("^app/"; "") | sub("\\[bot\\]$"; "");
  ([$reverts[] | select(.state != "CLOSED")
    | ((.body // "") | [scan("Reverts \($repo)#([0-9]+)") | .[0] | tonumber]),
      ([.labels[].name | select(startswith("factory/revert-of:#")) | ltrimstr("factory/revert-of:#") | tonumber])
    | .[]] | unique) as $reverted
  | [ .[]
      | . as $pr
      | [ .comments[]
          | select((.author.login | login) == $factory)
          | select(.body | test("event=would-arm-[0-9]+ head=[0-9a-f]+ -->")) ] as $wa
      | select($wa | length > 0)
      | ($wa | sort_by(.createdAt) | last) as $last
      | select($last.body | contains("would auto-merge: `\($class)`"))
      | ($last.body | capture("event=would-arm-[0-9]+ head=(?<h>[0-9a-f]+) -->").h) as $head
      | { n: .number,
          verdict: (if ((.mergedBy.login // "") | login) == $merger then "auto_merged"
                    elif ($reverted | index($pr.number)) != null then "reverted"
                    elif .headRefOid == $head then "merged_unchanged"
                    else "changed" end) } ]
  | (.[] | "#\(.n) \(.verdict)"),
    "merged_unchanged=\([.[] | select(.verdict == "merged_unchanged")] | length) changed=\([.[] | select(.verdict == "changed")] | length) reverted=\([.[] | select(.verdict == "reverted")] | length) auto_merged=\([.[] | select(.verdict == "auto_merged")] | length)"
' <<<"$prs"
