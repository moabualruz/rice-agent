#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'worktree-doctor: %s\nFix: %s\n' "$1" "$2" >&2
  exit 1
}

root=$(git rev-parse --show-toplevel 2>/dev/null) || fail "not inside a Git checkout" "run workspace-doctor map and enter the existing agent-dry checkout"
[[ -f "$root/go.mod" ]] || fail "Go module is missing" "restore the checkout's go.mod before running repository checks"
[[ -f "$root/justfile" ]] || fail "justfile is missing" "restore the checkout's justfile before running repository checks"
command -v go >/dev/null 2>&1 || fail "Go is unavailable" "install the repository's Go toolchain, then run just ci"
command -v just >/dev/null 2>&1 || fail "just is unavailable" "install just, then run just ci"
command -v jq >/dev/null 2>&1 || fail "jq is unavailable" "install jq, then run just ci"
command -v shellcheck >/dev/null 2>&1 || fail "ShellCheck is unavailable" "install ShellCheck, then run just ci"
command -v gitleaks >/dev/null 2>&1 || fail "gitleaks is unavailable" "install gitleaks, then run just ci"
command -v bash >/dev/null 2>&1 || fail "Bash is unavailable" "install Bash, then run just ci"

mise_path=$(command -v mise || true)
[[ -n "$mise_path" ]] || fail "mise is unavailable" "install mise with Node 24.18.0, then run just ci"
node_dir=$("$mise_path" where node@24.18.0 2>/dev/null) || fail "mise Node 24.18.0 is unavailable" "install Node 24.18.0 through mise, then run just ci"
node_path="$node_dir/bin/node"
[[ -x "$node_path" ]] || fail "mise Node 24.18.0 is unavailable" "install Node 24.18.0 through mise, then run just ci"
node_version=$("$node_path" --version 2>/dev/null) || fail "mise Node 24.18.0 could not run" "repair the mise Node 24.18.0 installation, then run just ci"
[[ "$node_version" == "v24.18.0" ]] || fail "Node 24.18.0 is not active through mise" "activate Node 24.18.0 through mise, then run just ci"

if [[ -f "$root/.gitmodules" ]]; then
  status=$(git -C "$root" submodule status --recursive 2>/dev/null) || fail "submodule state could not be read" "run git submodule update --init --recursive"
  if grep -Eq '^[+-U]' <<<"$status"; then
    fail "a submodule is uninitialized or differs from its recorded revision" "run git submodule update --init --recursive"
  fi
fi

printf 'worktree-doctor: checkout and gate prerequisites are ready\n'
