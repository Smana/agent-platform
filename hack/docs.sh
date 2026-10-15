#!/usr/bin/env bash
# Reader pages state status once (README) and never per row: this refuses the status vocabulary
# that let the docs drift (review 2026-10-15), and dead relative links and anchors. Specs and
# plans under docs/superpowers speak of phases by design and are skipped, also when named
# as an argument.
#
# Links: only `](target)` outside fenced code blocks and inline code are checked; absolute
# URLs (scheme:, //) are skipped. A relative target must exist; a #fragment on a markdown
# target must match a heading's GitHub slug: lowercase, backticks/emphasis dropped, link
# markup reduced to its text, every character but letters, digits, `_`, `-` and space
# removed, spaces to `-`, and a repeated slug gets -1, -2 ... in document order.
set -euo pipefail
export LC_ALL=C.UTF-8
cd "$(dirname "$0")/.."
if [ $# -eq 0 ]; then
  files=()
  while IFS= read -r f; do files+=("$f"); done < <(find README.md CONTRIBUTING.md AGENTS.md docs charts images -name '*.md' -not -path 'docs/superpowers/*' 2>/dev/null | sort)
else
  files=("$@")
fi

# status FILE: "line<TAB>word" for each status word, whole words only, outside code.
# A hit needs a non-word character (not a letter, digit or hyphen) on both sides, so
# Unplanned, in reviewer, not built-in and MAP-1 stay clean.
status() {
  awk '
    /^ ? ? ?(```|~~~)/ { fence = !fence; next }
    fence { next }
    {
      s = $0
      gsub(/`[^`]*`/, " ", s)
      re = "(^|[^[:alnum:]-])(AP-[0-9]+|[(]phase [0-9]+|phase [0-9]+ /|Planned|planned,|not built|not deployed|Nothing is deployed|Not merged|in review)([^[:alnum:]-]|$)"
      while (match(s, re)) {
        m = substr(s, RSTART, RLENGTH)
        sub(/^[^[:alnum:](]/, "", m); sub(/[^[:alnum:]]$/, "", m)
        if (m ~ /^planned$/) m = "planned,"
        print NR "\t" m
        s = substr(s, RSTART + RLENGTH - 1)
      }
    }' "$1"
}

# slugs FILE: one GitHub heading slug per line, fenced code skipped, repeats numbered.
# perl, not awk: lowercasing and "letter" must hold for any script (Démarrage, Écrire),
# and the default awk on Ubuntu (mawk) is byte-only. Only punctuation and symbols go.
slugs() {
  perl -CSDA -ne '
    if (/^ {0,3}(```|~~~)/) { $f = !$f; next }
    next if $f || !/^#{1,6}[ \t]/;
    s/^#+[ \t]+//; s/[ \t]+#+[ \t]*$//; s/\n$//;
    s/\]\([^)]*\)//g; s/[\[\]]//g;
    $_ = lc;
    s/[^\p{L}\p{N}\p{M}_ \t-]//g;
    s/[ \t]/-/g;
    $n = $seen{$_}++;
    print $n ? "$_-$n\n" : "$_\n";
  ' "$1"
}

# links FILE: "line<TAB>target" for each relative-looking markdown link outside code.
links() {
  awk '
    /^[ ]? ? ?(```|~~~)/ { fence = !fence; next }
    fence { next }
    {
      s = $0
      gsub(/`[^`]*`/, "", s)
      while (match(s, /\]\([^) ]+/)) {
        t = substr(s, RSTART + 2, RLENGTH - 2)
        s = substr(s, RSTART + RLENGTH)
        sub(/^</, "", t); sub(/>$/, "", t)
        if (t ~ /^[A-Za-z][A-Za-z0-9+.-]*:/ || t ~ /^\/\//) continue
        print NR "\t" t
      }
    }' "$1"
}

rc=0
for f in "${files[@]}"; do
  case "$f" in *docs/superpowers/*) continue;; esac
  while IFS=$'\t' read -r n word; do
    echo "$f:$n: status vocabulary: $word"; rc=1
  done < <(status "$f" | awk '!seen[$0]++')
  dir=$(dirname "$f")
  while IFS=$'\t' read -r n link; do
    target=${link%%#*}; frag=""
    case "$link" in *#*) frag=${link#*#};; esac
    path="$f"; [ -n "$target" ] && path="$dir/$target"
    if [ ! -e "$path" ]; then echo "$f:$n: dead link: $link"; rc=1; continue; fi
    if [ -n "$frag" ] && [ -f "$path" ]; then
      case "$path" in
        *.md) if ! slugs "$path" | grep -qxF -- "$(tr '[:upper:]' '[:lower:]' <<<"$frag")"; then
                echo "$f:$n: dead anchor: $link"; rc=1
              fi;;
      esac
    fi
  done < <(links "$f")
done
exit $rc
