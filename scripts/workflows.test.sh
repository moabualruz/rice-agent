#!/usr/bin/env bash
# Public repository safety: no workflow may run fork pull request code on a self-hosted runner,
# use pull_request_target, or hand a secret to a pull request.
set -euo pipefail
cd "$(dirname "$0")/.."

fail=0
shopt -s nullglob
files=(.github/workflows/*.yml .github/workflows/*.yaml)
[ "${#files[@]}" -gt 0 ] || { echo "no workflows found"; exit 1; }
for f in "${files[@]}"; do
  if grep -nE '^[[:space:]]*(pull_request_target|workflow_run)[[:space:]]*:' "$f"; then
    echo "FAIL $f: pull_request_target and workflow_run are not allowed"; fail=1
  fi
  if grep -qE '^[[:space:]]*pull_request[[:space:]]*:' "$f" && grep -nE 'runs-on:.*self-hosted' "$f"; then
    echo "FAIL $f: a workflow triggered by pull_request must not use a self-hosted runner"; fail=1
  fi
  if grep -nE 'secrets\.' "$f" | grep -v GITHUB_TOKEN; then
    echo "FAIL $f: secrets are not allowed in workflows that run for pull requests"; fail=1
  fi
done
[ "$fail" -eq 0 ] && echo "workflows ok"
exit "$fail"
