#!/usr/bin/env bash
# Public repository safety for .github/workflows: only GitHub-hosted runners, no trigger that runs
# with base-repository privileges on fork input, and no secrets. The rules are deliberately blunt
# (whole-word matches anywhere in the file, runner literals only) so one-line, list and expression
# spellings cannot slip past. A workflow that truly needs more edits this test on purpose.
set -euo pipefail
cd "$(dirname "$0")/.."

# Prints one line per violation for the workflow file $1.
violations() {
  local f=$1
  grep -nE '(^|[^A-Za-z_])(pull_request_target|workflow_run|issue_comment)([^A-Za-z_]|$)' "$f" |
    sed 's/^/trigger not allowed: /' || true
  grep -nE 'self-hosted' "$f" | sed 's/^/self-hosted not allowed: /' || true
  grep -nE '^[[:space:]]*(-[[:space:]]+)?runs-on:' "$f" |
    grep -vE 'runs-on:[[:space:]]*(ubuntu|macos|windows)-[A-Za-z0-9.-]+[[:space:]]*(#.*)?$' |
    sed 's/^/runs-on must be a hosted runner literal: /' || true
  grep -nE '(^|[^A-Za-z_])secrets([^A-Za-z_]|$)' "$f" | sed 's/^/secrets not allowed: /' || true
}

# Self-test: each unsafe spelling must be reported, the safe one must not.
tmp=$(mktemp -d "${TMPDIR:-/tmp}/workflows-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
n=0
while IFS= read -r bad; do
  n=$((n + 1))
  printf 'on: push\njobs:\n  a:\n%s\n' "$bad" >"$tmp/bad$n.yml"
  [ -n "$(violations "$tmp/bad$n.yml")" ] || { echo "FAIL self-test: not caught: $bad"; exit 1; }
done <<'BAD'
    runs-on: [self-hosted, linux]
    runs-on: ${{ matrix.os }}
    runs-on:
    env: { T: "${{ secrets['X'] }}" }
    secrets: inherit
    on: [push, pull_request_target]
    on: issue_comment
BAD
printf 'on: [push, pull_request]\njobs:\n  a:\n    runs-on: ubuntu-latest\n' >"$tmp/good.yml"
[ -z "$(violations "$tmp/good.yml")" ] || { echo "FAIL self-test: safe workflow flagged"; exit 1; }

fail=0
shopt -s nullglob
files=(.github/workflows/*.yml .github/workflows/*.yaml)
[ "${#files[@]}" -gt 0 ] || { echo "no workflows found"; exit 1; }
for f in "${files[@]}"; do
  v=$(violations "$f")
  if [ -n "$v" ]; then
    printf 'FAIL %s\n%s\n' "$f" "$v"
    fail=1
  fi
done
[ "$fail" -eq 0 ] && echo "workflows ok"
exit "$fail"
