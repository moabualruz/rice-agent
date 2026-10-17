#!/usr/bin/env bash
# Reject em dash (U+2014) and en dash (U+2013).
#   check-no-dashes.sh           staged additions (pre-commit hook)
#   check-no-dashes.sh --tree    every tracked file (CI)
# The glyphs are built from byte escapes so this file never contains them.
set -uo pipefail
EM="$(printf '\342\200\224')"
EN="$(printf '\342\200\223')"
export LC_ALL=C
if [ "${1:-}" = "--tree" ]; then
  bad=0
  while IFS= read -r -d '' f; do
    [ -f "$f" ] || continue
    if grep -I -n -F -e "$EM" -e "$EN" -- "$f" >/dev/null 2>&1; then
      echo "no-dashes: $f contains an em or en dash" >&2
      bad=1
    fi
  done < <(git ls-files -z)
  [ "$bad" -eq 0 ] || exit 1
  echo "no-dashes: tree clean"
  exit 0
fi
if ! out="$(git diff --cached -U0 --no-color --diff-filter=ACMR)"; then
  echo "no-dashes: git diff failed; failing closed" >&2
  exit 1
fi
if printf '%s\n' "$out" | grep -n -F -e "$EM" -e "$EN" >/dev/null; then
  echo "no-dashes: staged additions contain an em or en dash; use - , ; : or parentheses" >&2
  exit 1
fi
echo "no-dashes: staged additions clean"
