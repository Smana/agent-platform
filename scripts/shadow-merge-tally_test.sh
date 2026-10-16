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
echo ok
