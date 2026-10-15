#!/usr/bin/env bash
# hack/docs.sh refuses the status vocabulary and dead relative links, and ignores docs/superpowers.
set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/docs/superpowers"
printf '# A\n\nSee [b](b.md#there) and [gone](nope.md). Status: AP-1. Planned, phase 2.\n' > "$tmp/docs/a.md"
printf '# B\n\n## There\n' > "$tmp/docs/b.md"
printf '# Spec\n\nPhase 1 is planned (AP-1).\n' > "$tmp/docs/superpowers/spec.md"
out=$(hack/docs.sh "$tmp/docs/a.md" "$tmp/docs/b.md" "$tmp/docs/superpowers/spec.md" 2>&1 || true)
grep -q 'a.md:3: status vocabulary: AP-1' <<<"$out"
grep -q 'a.md:3: status vocabulary: Planned' <<<"$out"
grep -q 'a.md:3: dead link: nope.md' <<<"$out"
if grep -q 'b.md' <<<"$out"; then echo "unexpected match: 'b.md'"; exit 1; fi
if grep -q 'superpowers' <<<"$out"; then echo "unexpected match: 'superpowers'"; exit 1; fi
printf '# C\n\nSee [b](b.md#nowhere). Since v0.7 the broker serves it.\n' > "$tmp/docs/c.md"
out=$(hack/docs.sh "$tmp/docs/c.md" "$tmp/docs/b.md" 2>&1 || true)
grep -q 'c.md:3: dead anchor: b.md#nowhere' <<<"$out"
if grep -q 'status vocabulary' <<<"$out"; then echo "unexpected match: 'status vocabulary'"; exit 1; fi

# Anchors: backticks and punctuation dropped, duplicates numbered, same-file fragments.
printf '# T\n\n## The `Room` CRD: v1, (beta)!\n\n## Metrics\n\n## Metrics\n\n```\n## Fenced\n```\n' > "$tmp/docs/t.md"
printf '# D\n\n[a](t.md#the-room-crd-v1-beta) [b](t.md#metrics-1) [c](t.md#metrics) [d](#d)\n' > "$tmp/docs/d.md"
hack/docs.sh "$tmp/docs/d.md"
printf '# E\n\n[x](t.md#metrics-2) [y](t.md#fenced) [z](#nope) [W](t.md#The-Room-CRD-v1-beta)\n' > "$tmp/docs/e.md"
out=$(hack/docs.sh "$tmp/docs/e.md" 2>&1 || true)
grep -q 'e.md:3: dead anchor: t.md#metrics-2' <<<"$out"
grep -q 'e.md:3: dead anchor: t.md#fenced' <<<"$out"
grep -q 'e.md:3: dead anchor: #nope' <<<"$out"
if grep -q 'The-Room' <<<"$out"; then echo "unexpected match: 'The-Room'"; exit 1; fi

# Ignored: fenced code, inline code, absolute URLs, mailto.
printf '# F\n\n`[x](gone.md)` [u](https://example.com/x.md) [m](mailto:a@b.c)\n\n```md\n[y](gone.md)\n```\n' > "$tmp/docs/f.md"
hack/docs.sh "$tmp/docs/f.md"

# Vocabulary is whole-word and code-free.
printf '# G\n\nUnplanned, in reviewer, not built-in, MAP-1, `in review`, x-AP-1.\n\n```\nPlanned AP-1\n```\n' > "$tmp/docs/g.md"
hack/docs.sh "$tmp/docs/g.md"
printf '# H\n\nAP-12 is (phase 3) and in review.\n' > "$tmp/docs/h.md"
out=$(hack/docs.sh "$tmp/docs/h.md" 2>&1 || true)
grep -q 'h.md:3: status vocabulary: AP-12' <<<"$out"
grep -q 'h.md:3: status vocabulary: (phase 3' <<<"$out"
grep -q 'h.md:3: status vocabulary: in review' <<<"$out"

# Exit codes: 1 on a hit, 0 clean.
rc=0; hack/docs.sh "$tmp/docs/h.md" >/dev/null 2>&1 || rc=$?; [ "$rc" = 1 ]
rc=0; hack/docs.sh "$tmp/docs/g.md" >/dev/null 2>&1 || rc=$?; [ "$rc" = 0 ]

# Unicode anchors.
printf '# U\n\n## 🚀 Démarrage rapide\n\n## Écrire\n\n## Ça va ?\n' > "$tmp/docs/u.md"
printf '# V\n\n[a](u.md#-démarrage-rapide) [b](u.md#écrire) [c](u.md#ça-va-)\n' > "$tmp/docs/v.md"
hack/docs.sh "$tmp/docs/v.md"
echo ok
