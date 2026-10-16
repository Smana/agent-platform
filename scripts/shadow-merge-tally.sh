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
set -euo pipefail

class="" repo="${GH_REPO:-Smana/cloud-native-ref}" limit=500
author="app/ogenki-agents" factory="ogenki-agent-factory" merger="ogenki-agent-merger"
while [ $# -gt 0 ]; do
  case "$1" in
    --class) class="$2"; shift 2 ;;
    --repo) repo="$2"; shift 2 ;;
    --limit) limit="$2"; shift 2 ;;
    *) echo "usage: $0 --class <class> [--repo owner/name] [--limit n]" >&2; exit 2 ;;
  esac
done
[ -n "$class" ] || { echo "usage: $0 --class <class> [--repo owner/name] [--limit n]" >&2; exit 2; }
gh="${GH:-gh}"

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
