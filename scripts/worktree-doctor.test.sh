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

# Without mise, a Node 24.18.0 on PATH is accepted; any other version is refused.
plain_bin="$root/plain"
mkdir -p "$plain_bin"
for command in go just jq shellcheck gitleaks; do ln -s /usr/bin/true "$plain_bin/$command"; done
for command in git bash dirname; do ln -s "$(command -v "$command")" "$plain_bin/$command"; done
if PATH="$plain_bin:/usr/bin:/bin" command -v mise >/dev/null 2>&1; then
  printf "worktree-doctor.test: mise is on the base PATH; skipping the no-mise cases\n"
else
  ln -s "$mise_node_dir/bin/node" "$plain_bin/node"
  output=$(cd "$ready" && PATH="$plain_bin:/usr/bin:/bin" bash .githooks/pre-push)
  [[ "$output" == "worktree-doctor: checkout and gate prerequisites are ready" ]]
  rm "$plain_bin/node"
  printf "#!/usr/bin/env bash\nprintf v22.0.0\\\n" >"$plain_bin/node"
  chmod +x "$plain_bin/node"
  if output=$(cd "$ready" && PATH="$plain_bin:/usr/bin:/bin" bash .githooks/pre-push 2>&1); then
    printf "expected a wrong Node version to be refused\n" >&2
    exit 1
  fi
  [[ "$output" == *"Node 24.18.0 is not active"* ]]
fi

printf "worktree-doctor.test: ready, no-mise and fail-closed pre-push cases passed\n"
