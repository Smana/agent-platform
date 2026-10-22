#!/usr/bin/env bash
# stall-steps.sh: the loop policy's stall limit N, read from the factory's history (loops design L3).
#
# An accepted run is an implementer run named by an `Agent-Run:` trailer in a merged agent pull
# request that no revert undid. Its steps to its first commit come from the room log, which outlives
# a teardown (rooms-pre-destroy): stall-steps.sql, run in a platform session. N is the smallest
# multiple of 10 above the p90 of those steps once 20 accepted runs have one; 80, the design's
# default, until then.
#
#   rooms_psql agent-system xplane-rooms-cnpg-cluster < scripts/stall-steps.sql > steps.csv
#   scripts/stall-steps.sh --steps steps.csv [--repo Smana/cloud-native-ref] [--limit 500]
#
# rooms_psql is the helper of ai-native-ref's runbook docs/runbooks/agent-factory/13-rooms-restore.md.
# Needs gh, authenticated, and jq.
set -euo pipefail

steps="" repo="${GH_REPO:-Smana/cloud-native-ref}" limit=500
author="app/ogenki-agents" min_runs=20 default_n=80
usage() { echo "usage: $0 --steps <stall-steps.sql output> [--repo owner/name] [--limit n]" >&2; exit 2; }
while [ $# -gt 0 ]; do
  case "$1" in
    --steps) steps="$2"; shift 2 ;;
    --repo) repo="$2"; shift 2 ;;
    --limit) limit="$2"; shift 2 ;;
    *) usage ;;
  esac
done
[ -n "$steps" ] && [ -r "$steps" ] || usage
gh="${GH:-gh}"

# GraphQL, paged: `gh pr list --json commits` also asks for each commit's authors, which passes
# GitHub's 500,000-node limit at 50 pull requests.
query='query($q: String!, $endCursor: String) { search(query: $q, type: ISSUE, first: 50, after: $endCursor) {
  pageInfo { hasNextPage endCursor }
  nodes { ... on PullRequest { number commits(first: 100) { nodes { commit { message } } } } } } }'
prs=$("$gh" api graphql --paginate -f q="repo:$repo is:pr is:merged author:$author" -f query="$query" \
  | jq -s '[.[].data.search.nodes[] | {number, messages: [.commits.nodes[].commit.message]}]')
# A revert of any merged PR: GitHub's revert body, or the merger's revert-of label (v2 D1).
reverts=$("$gh" pr list -R "$repo" --state all --search 'Revert in:title' --limit "$limit" \
  --json body,labels,state)

jq -nr --arg repo "$repo" --argjson prs "$prs" --argjson reverts "$reverts" --rawfile csv "$steps" \
  --argjson min "$min_runs" --argjson dflt "$default_n" '
  def p($xs; $q): ($xs | sort) as $s | $s[(($q * ($s | length)) | ceil) - 1];
  ([$reverts[] | select(.state != "CLOSED")
    | ((.body // "") | [scan("Reverts \($repo)#([0-9]+)") | .[0] | tonumber]),
      ([.labels[].name | select(startswith("factory/revert-of:#")) | ltrimstr("factory/revert-of:#") | tonumber])
    | .[]] | unique) as $reverted
  | ([$prs[] | select(.number as $n | $reverted | index($n) | not)
      | .messages[] | [scan("(?m)^Agent-Run: ([a-z2-7]{8})$") | .[0]] | .[]] | unique) as $accepted
  | ([$csv | split("\n")[1:][] | select(length > 0) | split(",")
      | {run: .[0], steps: (.[1] | tonumber), first: (if .[2] == "" then null else (.[2] | tonumber) end)}]) as $runs
  | ([$runs[] | select(.first != null and (.run as $r | $accepted | index($r)))] | map(.first)) as $firsts
  | (if ($firsts | length) >= $min then ((p($firsts; 0.9) / 10 | floor) + 1) * 10 else $dflt end) as $n
  | "accepted runs: \($accepted | length), with a first commit in the room log: \($firsts | length)",
    (if ($firsts | length) > 0
     then "steps to the first commit: p50 \(p($firsts; 0.5)), p90 \(p($firsts; 0.9)), max \($firsts | max)"
     else "steps to the first commit: none" end),
    (if ($firsts | length) >= $min then "N: \($n) (above the p90)"
     else "N: \($n) (the default: \($firsts | length) accepted runs, \($min) needed)" end),
    "accepted runs N would have stopped: \([$firsts[] | select(. > $n)] | length) of \($firsts | length)",
    "implementer runs past N with no commit: \([$runs[] | select(.first == null and .steps >= $n)] | length) of \($runs | length)"
'
