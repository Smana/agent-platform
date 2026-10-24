#!/usr/bin/env bash
# Fetch a repository's merged Renovate PRs as replay cases for semverdiff (merge gate follow-ups,
# offline proof 14). Read-only: it only calls `gh api` and `gh pr list`, and writes nothing to
# GitHub. Run it by hand, never in CI.
#
#   hack/renovate-replay.sh <owner/repo> <since YYYY-MM-DD> <out-dir>
#
# Writes <out-dir>/<n>.json = {"number", "update", "base": {path: text}, "head": {path: text}}.
# "update" is the first cell of the Update column of Renovate's table (patch, minor, major,
# digest, pin, ...), "unknown" when absent. "head" is the files at the merge commit, "base" at its
# first parent; a file absent on a side is "".
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 <owner/repo> <since YYYY-MM-DD> <out-dir>" >&2
  exit 2
fi
repo=$1 since=$2 out=$3
limit=${LIMIT:-400}
command -v gh >/dev/null || { echo "gh is required" >&2; exit 1; }
command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }
mkdir -p "$out"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# content <path> <ref>: the file's text at ref, "" when absent.
content() {
  local path=$1 ref=$2 b64
  if b64=$(gh api "repos/$repo/contents/$path?ref=$ref" --jq '.content' 2>/dev/null); then
    printf '%s' "$b64" | base64 -d
  fi
}

# the PR list is held in a variable: it is bounded by --limit
prs=$(gh pr list --repo "$repo" --author app/renovate --state merged \
  --search "merged:>=$since" --limit "$limit" --json number,body,files,mergeCommit)
total=$(jq length <<<"$prs")
echo "fetching $total Renovate PRs merged since $since from $repo into $out" >&2

done_n=0
while IFS= read -r pr; do
  n=$(jq -r .number <<<"$pr")
  sha=$(jq -r '.mergeCommit.oid // empty' <<<"$pr")
  [[ -n $sha ]] || { echo "#$n: no merge commit, skipped" >&2; continue; }
  # the Update cell of the first data row of Renovate's table (its column is found in the header)
  update=$(jq -r '.body // ""' <<<"$pr" | awk -F'|' '
    function trim(s) { gsub(/^[[:space:]`]+|[[:space:]`]+$/, "", s); return s }
    /^\|[[:space:]]*Package[[:space:]]*\|/ {
      for (i = 2; i <= NF; i++) if (trim($i) == "Update") col = i
      t = col ? 1 : 0; next
    }
    t && /^\|[[:space:]]*-/ { next }
    t && /^\|/ { print trim($col); exit }
    t { exit }')
  update=${update:-unknown}
  parent=$(gh api "repos/$repo/commits/$sha" --jq '.parents[0].sha')
  # files go through temp files: a large PR overflows the argument list
  echo '{}' >"$tmp/base" && echo '{}' >"$tmp/head"
  while IFS= read -r path; do
    [[ -n $path ]] || continue
    for side in base head; do
      ref=$sha
      [[ $side == base ]] && ref=$parent
      content "$path" "$ref" >"$tmp/text"
      jq --arg p "$path" --rawfile t "$tmp/text" '. + {($p): $t}' "$tmp/$side" >"$tmp/next"
      mv "$tmp/next" "$tmp/$side"
    done
  done < <(jq -r '.files[].path' <<<"$pr")
  jq -n --argjson n "$n" --arg u "$update" --slurpfile b "$tmp/base" --slurpfile h "$tmp/head" \
    '{number: $n, update: $u, base: $b[0], head: $h[0]}' >"$out/$n.json"
  done_n=$((done_n + 1))
  echo "#$n $update" >&2
done < <(jq -c '.[]' <<<"$prs")
echo "wrote $done_n cases to $out" >&2
