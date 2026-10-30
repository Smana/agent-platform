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
# A revert labelled factory/drill (AM6) is a drill, never "reverted": the pull request keeps its
# verdict, its line ends in "drill" and drill= counts it apart. A line per reviewer model follows
# (AM10), read from the decision record beside the would-arm marker; reviewer=- names no model
# and has no line.
#
#   scripts/shadow-merge-tally.sh --class docs-links [--replay lines=40 | --replay files=3] [--strictness]
#
# Both read each merged pull request's newest decision record (AM8, `agent-factory-decision ...
# class=<class>`, the factory's own comments only) whose class= is --class. A hand-off's record
# is a hand-off whatever its decision= says: a not_mergeable hand-off after an arm reads arm:armed.
# --replay recounts the would-arm and armed decisions and those refused patch_too_large on their
# recorded lines (or files) against N, the other count against its recorded cap (0 = none). A
# record whose count was not read (`-`) is unreplayable and counted apart.
# --strictness counts the decisions refused by a class row that a human then merged at that head.
#
# --author names who opened the pull requests (default app/ogenki-agents, the factory's agents App).
# An adopted class's are Renovate's: scripts/shadow-merge-tally.sh --class deps-patch --author app/renovate.
# Its would-arm comments (WouldArmAdopted) carry the same head= marker, so the count is the same.
# For an adopted class merged_unchanged is Renovate's own automerge of the judged head, not a
# maintainer's judgement: the evidence for going live is --outcomes.
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

class="" outcomes="" since="" replay="" strictness="" window="168h" budget="0.10" repo="${GH_REPO:-Smana/cloud-native-ref}" limit=500
usage() { echo "usage: $0 --class <class> [--repo owner/name] [--limit n] [--author login] [--replay lines=N|files=N] [--strictness] [--outcomes --since <date> [--window 168h] [--budget 0.10]]" >&2; }
author="app/ogenki-agents" factory="ogenki-agent-factory" merger="ogenki-agent-merger"
while [ $# -gt 0 ]; do
  case "$1" in
    --class) class="$2"; shift 2 ;;
    --repo) repo="$2"; shift 2 ;;
    --limit) limit="$2"; shift 2 ;;
    --author) author="$2"; shift 2 ;;
    --replay) replay="$2"; shift 2 ;;
    --strictness) strictness=1; shift ;;
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
[ -z "$replay" ] || [[ "$replay" =~ ^(lines|files)=[0-9]+$ ]] || { echo "--replay is lines=N or files=N" >&2; exit 2; }
gh="${GH:-gh}"

# A revert of any merged PR: GitHub's revert body, or the merger's revert-of label (D1).
reverts_of() {
  "$gh" pr list -R "$repo" --state all --search 'Revert in:title' --limit "$limit" --json body,labels,state
}
# $reverted: the pull requests an open or merged revert reverts, but a drill (AM6): those are $drilled.
# shellcheck disable=SC2016 # a jq program: its $ names are jq's
revert_defs='
  def login: ascii_downcase | sub("^app/"; "") | sub("\\[bot\\]$"; "");
  def drill: [.labels[].name] | index("factory/drill") != null;
  def targets: ((.body // "") | [scan("Reverts \($repo)#([0-9]+)") | .[0] | tonumber])
    + [.labels[].name | select(startswith("factory/revert-of:#")) | ltrimstr("factory/revert-of:#") | tonumber];
  ([$reverts[] | select(.state != "CLOSED") | select(drill | not) | targets[]] | unique) as $reverted
  | ([$reverts[] | select(.state != "CLOSED") | select(drill) | targets[]] | unique) as $drilled
  |'

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
    --argjson reverts "$(reverts_of)" --argjson followed "$followed" "$revert_defs"'
    (($now | fromdateiso8601) - ($window | tonumber) * 3600) as $cutoff
    | [ .[] | . as $pr
        | ([.comments[] | select((.author.login | login) == $factory)
            | select(.body | test("event=monitor-[0-9]+ verdict=[a-z]+"))] | sort_by(.createdAt) | last) as $c
        | (if $c == null then "none" else ($c.body | capture("event=monitor-[0-9]+ verdict=(?<v>[a-z]+)").v) end) as $verdict
        | [ (select($reverted | index($pr.number)) | "reverted"),
            (select($followed[$pr.number | tostring]) | "follow_up"),
            (select(([$pr.labels[].name] | index("factory/monitor:regression")) or $verdict == "regression") | "regression") ] as $bad
        | { n: .number, mature: ((.mergedAt | fromdateiso8601) <= $cutoff), verdict: $verdict, bad: $bad,
            drill: ($drilled | index($pr.number) != null),
            conclusive: ($verdict == "healthy" or $verdict == "regression" or ($bad | length) > 0) } ]
    | sort_by(.n)
    | (.[] | "#\(.n) \(if .mature then "mature" else "immature" end) \(.verdict) \(if (.bad | length) > 0 then (.bad | join(",")) else "-" end)\(if .drill then " drill" else "" end)"),
      ([.[] | select(.mature)] as $m
       | ($m | map(select(.conclusive))) as $k
       | "merges=\(length) mature=\($m | length) conclusive=\($k | length) inconclusive=\(($m | length) - ($k | length)) "
         + "bad=\([$k[] | select(.bad | length > 0)] | length) reverted=\([$k[] | select(.bad | index("reverted"))] | length) "
         + "follow_up=\([$k[] | select(.bad | index("follow_up"))] | length) regression=\([$k[] | select(.bad | index("regression"))] | length) "
         + "drill=\([.[] | select(.drill)] | length)")
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

jq -r --arg class "$class" --arg repo "$repo" --arg factory "$factory" --arg merger "$merger" \
  --arg replay "$replay" --arg strictness "$strictness" --argjson reverts "$(reverts_of)" "$revert_defs"'
  # The decision record (AM8), or null. Unread counts are "-".
  def record: [capture("agent-factory-decision head=(?<head>[^ ]+) class=(?<class>[^ ]+) decision=(?<v>[a-z_]+):(?<r>[a-z_]+) files=(?<f>[0-9]+|-)/(?<fm>[0-9]+) lines=(?<l>[0-9]+|-)/(?<lm>[0-9]+) .*reviewer=(?<m>[^ ]+) -->")] | first;
  # The class rows whose refusal --strictness counts.
  ["patch_too_large", "file_not_allowed", "file_protected", "file_added_or_deleted", "class_mismatch",
   "holdout_red", "oracle_finding_open", "oracle_unrequested", "release_age_red"] as $rows
  | [ .[]
      | . as $pr
      | [ .comments[] | select((.author.login | login) == $factory) ] as $own
      | ([ $own[] | select(.body | test("event=would-arm-[0-9]+ head=[0-9a-f]+")) ] | sort_by(.createdAt) | last) as $last
      | ([ $own[] | select(.body | test("agent-factory-decision ")) ] | sort_by(.createdAt) | last) as $dc
      | { n: .number,
          judged: (if $last != null and ($last.body | contains("would auto-merge: `\($class)`")) then
              ($last.body | capture("event=would-arm-[0-9]+ head=(?<h>[0-9a-f]+)").h) as $head
              | { verdict: (if ((.mergedBy.login // "") | login) == $merger then "auto_merged"
                            elif ($reverted | index($pr.number)) != null then "reverted"
                            elif .headRefOid == $head then "merged_unchanged"
                            else "changed" end),
                  drill: ($drilled | index($pr.number) != null),
                  model: ((($last.body | record) // {}).m // "-") }
            else null end),
          decision: (if $dc == null then null else
              ($dc.body | record) as $d
              | if $d == null or $d.class != $class then null else
                  # A hand-off is never an arm: it takes the reason its event names.
                  ([$dc.body | capture("event=human-(?<r>[a-z_]+)-[0-9]+")] | first) as $ho
                  | $d + (if $ho != null then {v: "human", r: $ho.r} else {} end)
                       + {unchanged: ($pr.headRefOid == $d.head and ((($pr.mergedBy.login // "") | login) != $merger))}
                end
            end) } ]
  | [ .[] | select(.judged != null) | {n} + .judged ] as $j
  | [ .[] | .decision | select(. != null) ] as $d
  | ($j[] | "#\(.n) \(.verdict)\(if .drill then " drill" else "" end)"),
    "merged_unchanged=\([$j[] | select(.verdict == "merged_unchanged")] | length) changed=\([$j[] | select(.verdict == "changed")] | length) reverted=\([$j[] | select(.verdict == "reverted")] | length) auto_merged=\([$j[] | select(.verdict == "auto_merged")] | length) drill=\([$j[] | select(.drill)] | length)",
    ([$j[] | select(.model != "-")] | group_by(.model)[]
      | "reviewer \(.[0].model): merged_unchanged=\(map(select(.verdict == "merged_unchanged")) | length) changed=\(map(select(.verdict == "changed")) | length)"),
    (if $replay == "" then empty else
      ($replay | split("=")) as [$key, $cap]
      | (if $key == "lines" then {k: "l", o: "f", om: "fm"} else {k: "f", o: "l", om: "lm"} end) as $x
      | [ $d[] | select(.v == "shadow" or (.v == "arm" and .r == "armed") or (.v == "human" and .r == "patch_too_large")) ] as $set
      | [ $set[] | select(.[$x.k] != "-")
          | (.[$x.k] | tonumber) <= ($cap | tonumber) and ((.[$x.om] | tonumber) == 0 or (.[$x.o] | tonumber) <= (.[$x.om] | tonumber)) ] as $r
      | "replay \($replay): would_arm=\([$r[] | select(.)] | length) would_refuse=\([$r[] | select(not)] | length) unreplayable=\([$set[] | select(.[$x.k] == "-")] | length)"
    end),
    (if $strictness == "" then empty else
      [ $d[] | select(.v == "human" and (.r as $r | $rows | index($r) != null)) ] as $s
      | "strictness: refused=\($s | length) refused_merged_unchanged=\([$s[] | select(.unchanged)] | length)"
    end)
' <<<"$prs"
