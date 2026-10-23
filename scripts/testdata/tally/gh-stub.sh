#!/usr/bin/env bash
# A stand-in for gh for scripts/shadow-merge-tally_test.sh: logs its arguments to $GH_LOG and
# serves the fixtures next to it, so nothing reaches GitHub.
d="$(dirname "$0")"
echo "$*" >> "${GH_LOG:-/dev/null}"
case "$*" in
  *"Revert in:title"*) echo '[]' ;;
  # A later pull request carries factory/follow-up-of:#<n>: only fixture "followup" has one, of #100.
  *'label:"factory/follow-up-of:#100"'*) [ "${TALLY_FIXTURE:-}" = followup ] && echo '[{"number":900}]' || echo '[]' ;;
  *'label:"factory/follow-up-of:#'*) echo '[]' ;;
  *'label:"factory/merged:'*) cat "$d/prs${TALLY_FIXTURE:+-$TALLY_FIXTURE}.json" ;;
  *) echo '[]' ;;
esac
