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

nvm_dir=${NVM_DIR:-"$HOME/.nvm"}
[[ -s "$nvm_dir/nvm.sh" ]] || fail "NVM is unavailable" "install NVM and Node 24, then run just ci"
# shellcheck disable=SC1090
source "$nvm_dir/nvm.sh"
nvm use 24 >/dev/null 2>&1 || fail "NVM Node 24 is unavailable" "install Node 24 through NVM, then run just ci"
node_path=$(command -v node || true)
[[ -n "$node_path" && "$node_path" == "$nvm_dir"/* ]] || fail "Node is not provided by NVM" "activate Node 24 through NVM, then run just ci"

if [[ -f "$root/.gitmodules" ]]; then
  status=$(git -C "$root" submodule status --recursive 2>/dev/null) || fail "submodule state could not be read" "run git submodule update --init --recursive"
  if grep -Eq '^[+-U]' <<<"$status"; then
    fail "a submodule is uninitialized or differs from its recorded revision" "run git submodule update --init --recursive"
  fi
fi

printf 'worktree-doctor: checkout and gate prerequisites are ready\n'
