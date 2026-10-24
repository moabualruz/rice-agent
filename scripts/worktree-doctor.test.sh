#!/usr/bin/env bash
set -euo pipefail

root=$(mktemp -d "${TMPDIR:-/tmp}/agent-dry-worktree-doctor.XXXXXX")
trap 'rm -rf "$root"' EXIT

fake_bin="$root/bin"
nvm_dir="$root/nvm"
mkdir -p "$fake_bin" "$nvm_dir/versions/node/v24/bin"
for command in go just jq shellcheck gitleaks; do ln -s /usr/bin/true "$fake_bin/$command"; done
ln -s /usr/bin/true "$nvm_dir/versions/node/v24/bin/node"
printf 'nvm() { PATH="$NVM_DIR/versions/node/v24/bin:$PATH"; export PATH; return 0; }\n' >"$nvm_dir/nvm.sh"

make_checkout() {
  local checkout=$1
  mkdir -p "$checkout/scripts" "$checkout/.githooks"
  cp "$(dirname "$0")/worktree-doctor.sh" "$checkout/scripts/worktree-doctor.sh"
  cp "$(dirname "$0")/../.githooks/pre-push" "$checkout/.githooks/pre-push"
  git -C "$checkout" init -q
}

ready="$root/ready"
make_checkout "$ready"
printf 'module example.invalid/agent-dry\n\ngo 1.27\n' >"$ready/go.mod"
printf 'check:\n\t@true\n' >"$ready/justfile"
output=$(cd "$ready" && PATH="$fake_bin:$PATH" NVM_DIR="$nvm_dir" bash .githooks/pre-push)
[[ "$output" == "worktree-doctor: checkout and gate prerequisites are ready" ]]

missing="$root/missing-module"
make_checkout "$missing"
printf 'check:\n\t@true\n' >"$missing/justfile"
if output=$(cd "$missing" && PATH="$fake_bin:$PATH" NVM_DIR="$nvm_dir" bash .githooks/pre-push 2>&1); then
  printf 'expected pre-push to refuse a missing Go module\n' >&2
  exit 1
fi
[[ "$output" == *"worktree-doctor: Go module is missing"* ]]
[[ "$output" == *"restore the checkout's go.mod"* ]]

printf 'worktree-doctor.test: ready and fail-closed pre-push cases passed\n'
