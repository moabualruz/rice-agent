#!/usr/bin/env bash
set -euo pipefail

root=$(mktemp -d "${TMPDIR:-/tmp}/agent-dry-worktree-doctor.XXXXXX")
trap 'rm -rf "$root"' EXIT

fake_bin="$root/bin"
mise_node_dir="$root/mise/node/24.18.0"
mkdir -p "$fake_bin" "$mise_node_dir/bin"
for command in go just jq shellcheck gitleaks; do ln -s /usr/bin/true "$fake_bin/$command"; done
cat >"$fake_bin/mise" <<'BASH'
#!/usr/bin/env bash
[[ "$1" == "where" && "$2" == "node@24.18.0" ]]
printf '%s\n' "$MISE_NODE_DIR"
BASH
chmod +x "$fake_bin/mise"
cat >"$mise_node_dir/bin/node" <<'BASH'
#!/usr/bin/env bash
[[ "$1" == "--version" ]]
printf '%s\n' v24.18.0
BASH
chmod +x "$mise_node_dir/bin/node"

make_checkout() {
  local checkout=$1
  mkdir -p "$checkout/scripts" "$checkout/.githooks"
  cp "$(dirname "$0")/worktree-doctor.sh" "$checkout/scripts/worktree-doctor.sh"
  cp "$(dirname "$0")/../.githooks/pre-push" "$checkout/.githooks/pre-push"
  git -C "$checkout" init -q
}

ready="$root/ready"
make_checkout "$ready"
printf 'module example.invalid/rice-agent\n\ngo 1.27\n' >"$ready/go.mod"
printf 'check:\n\t@true\n' >"$ready/justfile"
output=$(cd "$ready" && PATH="$fake_bin:$PATH" MISE_NODE_DIR="$mise_node_dir" bash .githooks/pre-push)
[[ "$output" == "worktree-doctor: checkout and gate prerequisites are ready" ]]

missing="$root/missing-module"
make_checkout "$missing"
printf 'check:\n\t@true\n' >"$missing/justfile"
if output=$(cd "$missing" && PATH="$fake_bin:$PATH" MISE_NODE_DIR="$mise_node_dir" bash .githooks/pre-push 2>&1); then
  printf 'expected pre-push to refuse a missing Go module\n' >&2
  exit 1
fi
[[ "$output" == *"worktree-doctor: Go module is missing"* ]]
[[ "$output" == *"restore the checkout's go.mod"* ]]

printf 'worktree-doctor.test: ready and fail-closed pre-push cases passed\n'
