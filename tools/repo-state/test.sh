#!/usr/bin/env bash
# Tests for repo-state on fixture repositories: clean, dirty, detached, ahead of a local
# upstream, an extra worktree, an empty repository, several paths, --json, --remote-name,
# --max, --help and the failure exit codes.
# shellcheck disable=SC2016,SC2034  # check() evaluates its single-quoted assertion later
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tool="$here/repo-state"
command -v git >/dev/null 2>&1 || { echo "test.sh needs git" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "test.sh needs jq" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
tmp=$(cd "$tmp" && pwd -P)

# Isolate Git from the caller's configuration.
export HOME="$tmp/home" GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL="$tmp/home/.gitconfig"
export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@example.invalid
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@example.invalid
mkdir -p "$HOME"
: > "$GIT_CONFIG_GLOBAL"

pass=0
fail=0
check() {
  if eval "$2"; then pass=$((pass + 1)); else fail=$((fail + 1)); echo "FAIL: $1" >&2; fi
}

mkrepo() {
  git init -q "$1"
  git -C "$1" symbolic-ref HEAD refs/heads/main
  git -C "$1" config commit.gpgsign false
}
commit() { # commit <repo> <file> <content>
  printf '%s\n' "$3" > "$1/$2"
  git -C "$1" add -- "$2"
  git -C "$1" commit -q -m "$2"
}

# clean: one commit, an origin remote whose URL carries a credential canary.
canary="cred-canary-$$"
mkrepo "$tmp/clean"
commit "$tmp/clean" a.txt one
git -C "$tmp/clean" remote add origin "https://user:$canary@host-canary.example.invalid:8443/acme/widget.git"
mkdir -p "$tmp/clean/sub/dir"

# dirty: one staged change, one unstaged change, one staged-and-modified file,
# two untracked entries (a file and a new directory).
mkrepo "$tmp/dirty"
commit "$tmp/dirty" a.txt one
commit "$tmp/dirty" b.txt one
commit "$tmp/dirty" c.txt one
printf 'two\n' >> "$tmp/dirty/a.txt"; git -C "$tmp/dirty" add a.txt
printf 'two\n' >> "$tmp/dirty/b.txt"
printf 'two\n' >> "$tmp/dirty/c.txt"; git -C "$tmp/dirty" add c.txt; printf 'three\n' >> "$tmp/dirty/c.txt"
printf 'new\n' > "$tmp/dirty/u.txt"
mkdir -p "$tmp/dirty/newdir"; printf 'x\n' > "$tmp/dirty/newdir/f1"; printf 'y\n' > "$tmp/dirty/newdir/f2"
git -C "$tmp/dirty" remote add origin "git@scp-host.example.invalid:octo/gadget.git"

# detached HEAD
mkrepo "$tmp/detached"
commit "$tmp/detached" a.txt one
commit "$tmp/detached" b.txt two
git -C "$tmp/detached" checkout -q --detach HEAD~1

# ahead of a local upstream by 2, then behind by 1 on another branch
mkrepo "$tmp/ahead"
commit "$tmp/ahead" a.txt one
git -C "$tmp/ahead" branch base
commit "$tmp/ahead" b.txt two
commit "$tmp/ahead" c.txt three
git -C "$tmp/ahead" branch --set-upstream-to=base main >/dev/null

# an extra worktree on a new branch
mkrepo "$tmp/wtmain"
commit "$tmp/wtmain" a.txt one
git -C "$tmp/wtmain" worktree add -q -b feature "$tmp/wtextra"

# empty repository (no commits) and a directory name that needs JSON escaping
mkrepo "$tmp/empty"
odd="$tmp/we\"ird \\ dir"
mkrepo "$odd"
commit "$odd" a.txt one

mkdir -p "$tmp/plain"

# --help contract
out=$("$tool" --help); rc=$?
check "--help exits 0" '[ "$rc" -eq 0 ]'
check "--help starts with Usage: repo-state" 'case "$out" in "Usage: repo-state "*) true ;; *) false ;; esac'
check "-h works" '"$tool" -h | grep -q "^Usage: repo-state"'
check "--help documents exit codes" 'printf "%s" "$out" | grep -q "Exit codes"'

# clean repository, run from a subdirectory with no path argument
out=$(cd "$tmp/clean/sub/dir" && "$tool")
check "clean: root printed" 'printf "%s\n" "$out" | head -n 1 | grep -qxF "$tmp/clean"'
check "clean: branch and head" 'printf "%s" "$out" | grep -q "branch: main  head: $(git -C "$tmp/clean" rev-parse --short HEAD)  upstream: none"'
check "clean: zero counts" 'printf "%s" "$out" | grep -q "staged: 0  unstaged: 0  untracked: 0  worktrees: 1"'
check "clean: diff none" 'printf "%s" "$out" | grep -q "diff: none"'
check "clean: block is 4 lines" '[ "$(printf "%s\n" "$out" | wc -l | tr -d " ")" -eq 4 ]'
check "clean: no remote line without --remote-name" '! printf "%s" "$out" | grep -q "remote:"'

# dirty repository
out=$("$tool" "$tmp/dirty")
check "dirty: staged 2 unstaged 2 untracked 2" 'printf "%s" "$out" | grep -q "staged: 2  unstaged: 2  untracked: 2  worktrees: 1"'
check "dirty: diff summary line matches git diff --stat" '[ "$(printf "%s" "$out" | sed -n "s/^  diff: //p")" = "$(git -C "$tmp/dirty" diff --stat | tail -n 1 | sed "s/^ *//")" ]'

# detached HEAD
out=$("$tool" "$tmp/detached")
check "detached: shown as detached" 'printf "%s" "$out" | grep -q "branch: (detached)  head: $(git -C "$tmp/detached" rev-parse --short HEAD)"'

# ahead of a local upstream
out=$("$tool" "$tmp/ahead")
check "ahead: upstream base ahead 2 behind 0" 'printf "%s" "$out" | grep -q "upstream: base  ahead: 2  behind: 0"'
git -C "$tmp/ahead" checkout -q -b behind base
git -C "$tmp/ahead" branch --set-upstream-to=main behind >/dev/null
out=$("$tool" "$tmp/ahead")
check "behind: upstream main ahead 0 behind 2" 'printf "%s" "$out" | grep -q "branch: behind .*upstream: main  ahead: 0  behind: 2"'

# worktrees
out=$("$tool" "$tmp/wtmain" "$tmp/wtextra")
check "worktree: both roots shown" '[ "$(printf "%s\n" "$out" | grep -c "^/")" -eq 2 ]'
check "worktree: count 2 on both" '[ "$(printf "%s\n" "$out" | grep -c "worktrees: 2")" -eq 2 ]'
check "worktree: extra is on feature" 'printf "%s" "$out" | grep -q "branch: feature"'

# empty repository
out=$("$tool" "$tmp/empty")
check "empty: no head" 'printf "%s" "$out" | grep -q "branch: main  head: (none)  upstream: none"'

# several paths, deduplicated by root, non-repository path exit 1
set +e
out=$("$tool" "$tmp/clean" "$tmp/clean/sub" "$tmp/plain" "$tmp/dirty" "$tmp/missing" 2>"$tmp/err"); rc=$?
set -e
check "multi: exit 1 when a path is not a repository" '[ "$rc" -eq 1 ]'
check "multi: two blocks (dedup by root)" '[ "$(printf "%s\n" "$out" | grep -c "^/")" -eq 2 ]'
check "multi: stderr names the non-repository path" 'grep -qF "$tmp/plain: not inside a Git work tree" "$tmp/err"'
check "multi: stderr names the missing path" 'grep -qF "$tmp/missing: not a directory" "$tmp/err"'

# --max bounds the output
set +e
out=$("$tool" --max 2 "$tmp/clean" "$tmp/dirty" "$tmp/detached" "$tmp/ahead" 2>"$tmp/err"); rc=$?
set -e
check "max: exit 0" '[ "$rc" -eq 0 ]'
check "max: two blocks" '[ "$(printf "%s\n" "$out" | grep -c "^/")" -eq 2 ]'
check "max: rest counted on stderr" 'grep -q "2 more repositories not shown" "$tmp/err"'

# --remote-name prints owner/repo only, never the URL, host or credential
out=$("$tool" --remote-name "$tmp/clean" "$tmp/dirty" "$tmp/detached")
check "remote: https owner/repo" 'printf "%s" "$out" | grep -qx "  remote: acme/widget"'
check "remote: scp-like owner/repo" 'printf "%s" "$out" | grep -qx "  remote: octo/gadget"'
check "remote: none when no remote" 'printf "%s" "$out" | grep -qx "  remote: none"'
check "remote: credential, host and port never printed" '! printf "%s" "$out" | grep -qE "$canary|host-canary|scp-host|8443|user:"'

# --json
out=$("$tool" --json --remote-name "$tmp/dirty" "$tmp/detached" "$tmp/ahead" "$tmp/empty" "$odd")
check "json: valid array of 5" '[ "$(printf "%s" "$out" | jq length)" -eq 5 ]'
check "json: dirty counts" '[ "$(printf "%s" "$out" | jq -c ".[0] | [.staged, .unstaged, .untracked, .unmerged, .worktrees]")" = "[2,2,2,0,1]" ]'
check "json: dirty remote" '[ "$(printf "%s" "$out" | jq -r ".[0].remote")" = "octo/gadget" ]'
check "json: detached" '[ "$(printf "%s" "$out" | jq -c ".[1] | [.detached, .branch]")" = "[true,null]" ]'
check "json: behind branch ahead and behind" '[ "$(printf "%s" "$out" | jq -c ".[2] | [.branch, .upstream, .ahead, .behind]")" = "[\"behind\",\"main\",0,2]" ]'
check "json: empty repo head null" '[ "$(printf "%s" "$out" | jq -c ".[3] | [.head, .ahead, .diff]")" = "[null,null,\"\"]" ]'
check "json: escaped root round-trips" '[ "$(printf "%s" "$out" | jq -r ".[4].root")" = "$odd" ]'
set +e
out=$("$tool" --json "$tmp/plain" "$tmp/clean" 2>/dev/null); rc=$?
set -e
check "json: error object for a non-repository" '[ "$rc" -eq 1 ] && [ "$(printf "%s" "$out" | jq -r ".[0].error")" = "not inside a Git work tree" ]'
check "json: repository after the error still listed" '[ "$(printf "%s" "$out" | jq -r ".[1].branch")" = "main" ]'
out=$("$tool" --json "$tmp/clean")
check "json: no remote field without --remote-name" '[ "$(printf "%s" "$out" | jq ".[0] | has(\"remote\")")" = false ]'

# usage errors exit 2
set +e
"$tool" --bogus >/dev/null 2>&1; rc=$?
check "unknown option exits 2" '[ "$rc" -eq 2 ]'
"$tool" --max >/dev/null 2>&1; rc=$?
check "--max without a value exits 2" '[ "$rc" -eq 2 ]'
"$tool" --max 0 "$tmp/clean" >/dev/null 2>&1; rc=$?
check "--max 0 exits 2" '[ "$rc" -eq 2 ]'

# missing git exits 3
mkdir -p "$tmp/nogit"
for t in bash awk sed tr cat; do ln -s "$(command -v "$t")" "$tmp/nogit/$t"; done
PATH="$tmp/nogit" "$(command -v bash)" "$tool" "$tmp/clean" >/dev/null 2>&1; rc=$?
check "missing git exits 3" '[ "$rc" -eq 3 ]'
set -e

# Bash 3.2 (macOS /bin/bash) gives the same JSON when present
if [ -x /bin/bash ] && /bin/bash -c '[ "${BASH_VERSINFO[0]}" -lt 4 ]'; then
  a=$("$tool" --json --remote-name "$tmp/dirty" "$odd")
  b=$(/bin/bash "$tool" --json --remote-name "$tmp/dirty" "$odd")
  check "bash 3.2 output matches" '[ "$a" = "$b" ]'
fi

echo "repo-state tests: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
