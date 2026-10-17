#!/usr/bin/env bash
# Tests for check-no-dashes. Run from anywhere: bash test.sh (exit 0 only when every case passes).
# Uses throwaway Git repositories under a private temporary directory that is always removed.
# The glyphs are built from byte escapes, never written literally in this file.
# Run with /bin/bash on macOS to exercise Bash 3.2: the tool runs under the shell running this file.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOOL="$HERE/check-no-dashes"
SH="$BASH"
EM="$(printf '\342\200\224')"
EN="$(printf '\342\200\223')"

TMP="$(mktemp -d "${TMPDIR:-/tmp}/check-no-dashes-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

# Isolate every fixture from the caller's Git environment and configuration.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_PREFIX GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES
export GIT_CONFIG_NOSYSTEM=1
export GIT_CEILING_DIRECTORIES="$TMP"
export GIT_CONFIG_GLOBAL="$TMP/gitconfig"
cat >"$GIT_CONFIG_GLOBAL" <<EOF
[user]
	email = t@t.invalid
	name = t
[init]
	defaultBranch = main
[core]
	hooksPath = $TMP/no-hooks
[commit]
	gpgSign = false
EOF

PASS=0
FAIL=0
RC=0
OUT=""
ERR=""

# in_dir DIR CMD...: run CMD in DIR; sets RC, OUT and ERR.
in_dir() {
  local dir="$1"
  shift
  (cd "$dir" && "$@") >"$TMP/stdout" 2>"$TMP/stderr"
  RC=$?
  OUT="$(cat "$TMP/stdout")"
  ERR="$(cat "$TMP/stderr")"
}

# run DIR ARGS...: run the tool in DIR under the shell running this file.
run() {
  local dir="$1"
  shift
  in_dir "$dir" "$SH" "$TOOL" "$@"
}

has() { case "$1" in *"$2"*) return 0 ;; esac; return 1; }

# expect NAME CODE [out:TEXT] [err:TEXT] [!err:TEXT] [start:TEXT] [noout] [noerr]...
expect() {
  local name="$1" want="$2" why="" a
  shift 2
  [ "$RC" -eq "$want" ] || why="exit $RC, want $want"
  for a in "$@"; do
    case "$a" in
      out:*) has "$OUT" "${a#out:}" || why="${why:+$why; }stdout lacks '${a#out:}'" ;;
      err:*) has "$ERR" "${a#err:}" || why="${why:+$why; }stderr lacks '${a#err:}'" ;;
      '!err:'*) has "$ERR" "${a#!err:}" && why="${why:+$why; }stderr has '${a#!err:}'" ;;
      start:*) case "$OUT" in "${a#start:}"*) ;; *) why="${why:+$why; }stdout does not start with '${a#start:}'" ;; esac ;;
      noout) [ -z "$OUT" ] || why="${why:+$why; }stdout not empty" ;;
      noerr) [ -z "$ERR" ] || why="${why:+$why; }stderr not empty" ;;
    esac
  done
  if [ -z "$why" ]; then
    PASS=$((PASS + 1))
    printf '  ok   %s\n' "$name"
  else
    FAIL=$((FAIL + 1))
    printf '  FAIL %s: %s\n' "$name" "$why"
    printf '%s\n' "$OUT" | head -n 10 | sed 's/^/       out| /'
    printf '%s\n' "$ERR" | head -n 10 | sed 's/^/       err| /'
  fi
}

new_repo() { mkdir -p "$TMP/$1" && git -C "$TMP/$1" init -q && printf '%s\n' "$TMP/$1"; }
commit_all() { git -C "$1" add -A && git -C "$1" commit -qm "$2"; }

echo "interface"
run "$TMP" --help
expect "--help exits 0 and prints Usage first" 0 "start:Usage: check-no-dashes " "out:--tree" "out:--limit" "out:Exit codes:" noerr
run "$TMP" -h
expect "-h matches --help" 0 "start:Usage: check-no-dashes "
run "$TMP" --bogus
expect "unknown option is a usage error" 2 "err:unknown option: --bogus" noout
run "$TMP" a...b c...d
expect "two ranges are a usage error" 2 "err:at most one RANGE"
run "$TMP" --tree HEAD~1..HEAD
expect "--tree with a RANGE is a usage error" 2 "err:--tree takes no RANGE"
run "$TMP" --limit
expect "--limit without a value is a usage error" 2 "err:--limit needs"
run "$TMP" --limit x
expect "--limit with a non-number is a usage error" 2 "err:non-negative integer"
run "$TMP" --limit=-1
expect "--limit=-1 is a usage error" 2 "err:non-negative integer"
run "$TMP" ""
expect "an empty RANGE is a usage error" 2 "err:RANGE is empty"
run "$TMP" ...HEAD
expect "a RANGE with an empty base is a usage error" 2 "err:empty endpoint"
run "$TMP" main..
expect "a RANGE with an empty tip is a usage error" 2 "err:empty endpoint"
mkdir -p "$TMP/no-path"
in_dir "$TMP" env PATH="$TMP/no-path" "$SH" "$TOOL"
expect "missing git exits 3" 3 "err:git not found"

echo "staged additions (default mode)"
S="$(new_repo staged)"
printf 'a line with %s inside\n' "$EM" >"$S/em.md"
git -C "$S" add em.md
run "$S"
expect "staged em dash exits 1 with path:line on stderr" 1 "err:no-dashes: em.md:1: a line with <EM-DASH> inside" "err:1 added line(s)" noout
git -C "$S" rm -q --cached em.md && rm "$S/em.md"
printf 'range 1%s2\n' "$EN" >"$S/en.md"
git -C "$S" add en.md
run "$S"
expect "staged en dash exits 1" 1 "err:en.md:1: range 1<EN-DASH>2"
git -C "$S" rm -q --cached en.md && rm "$S/en.md"
printf 'plain hyphen-only line\n' >"$S/clean.md"
git -C "$S" add clean.md
run "$S"
expect "clean staged file passes" 0 "out:no-dashes: staged additions clean" noerr
printf 'one\ntwo %s dash\nthree\n' "$EM" >"$S/legacy.md"
commit_all "$S" seed
printf 'new clean line\n' >>"$S/clean.md"
git -C "$S" add clean.md
run "$S"
expect "pre-existing dash elsewhere does not block" 0 "out:staged additions clean"
printf 'one\nthree\n' >"$S/legacy.md"
git -C "$S" add legacy.md
run "$S"
expect "removing a dash line passes (only added lines count)" 0
printf 'one\ntwo %s dash\nthree\n' "$EM" >"$S/legacy.md"
git -C "$S" add legacy.md
commit_all "$S" restore
printf 'ONE\ntwo %s dash\nthree\nfour\n' "$EM" >"$S/legacy.md"
git -C "$S" add legacy.md
run "$S"
expect "clean edits to a dash-bearing file pass" 0
printf 'ONE\ntwo %s dash\nthree\nfour\n%s five\n' "$EM" "$EN" >"$S/legacy.md"
git -C "$S" add legacy.md
run "$S"
expect "new dash line in a second hunk reports its line" 1 "err:legacy.md:5: <EN-DASH> five" "!err:legacy.md:2:"
printf 'one\ntwo %s dash\nthree\n' "$EM" >"$S/legacy.md"
git -C "$S" add legacy.md
printf '++ pretends to be a diff header and hides an %s here\n' "$EM" >"$S/bypass.md"
git -C "$S" add bypass.md
run "$S"
expect "added line starting with ++ cannot pose as a file header" 1 "err:bypass.md:1:"
printf 'spaced %s name\n' "$EM" >"$S/my file.md"
git -C "$S" add "my file.md"
run "$S"
expect "path with a space is reported cleanly" 1 "err:no-dashes: my file.md:1: spaced <EM-DASH> name"
commit_all "$S" "dash commits for later range cases"
printf 'bin\000%s\000\n' "$EM" >"$S/blob.bin"
git -C "$S" add blob.bin
run "$S"
expect "staged binary file is skipped" 0 "out:staged additions clean"
commit_all "$S" binary
printf '%s 1\n%s 2\n%s 3\n' "$EM" "$EN" "$EM" >"$S/many.md"
git -C "$S" add many.md
run "$S"
expect "default limit shows every finding" 1 "err:many.md:3:" "err:3 added line(s)" "!err:more not shown"
run "$S" --limit 1
expect "--limit 1 bounds findings and keeps the total" 1 "err:many.md:1:" "!err:many.md:2:" "err:2 more not shown" "err:3 added line(s)"
run "$S" --limit=2
expect "--limit=N form" 1 "err:many.md:2:" "!err:many.md:3:" "err:1 more not shown"
run "$S" --limit 0
expect "--limit 0 shows all" 1 "err:many.md:3:" "!err:more not shown"
git -C "$S" rm -q --cached many.md && rm "$S/many.md"
long="$(printf '%0400d' 0)"
printf '%s %s\n' "$long" "$EM" >"$S/long.md"
git -C "$S" add long.md
run "$S"
expect "very long lines are truncated" 1 "err:long.md:1: 000" "!err:$long"
git -C "$S" rm -q --cached long.md && rm "$S/long.md"

echo "commit range"
git -C "$S" branch -f ci-base
printf 'ci line with %s inside\n' "$EM" >"$S/ci.md"
commit_all "$S" "add dash"
run "$S" ci-base...HEAD
expect "range catches a dash added on the branch" 1 "err:ci.md:1: ci line with <EM-DASH> inside" "!err:legacy.md"
git -C "$S" branch -f ci-base2
printf 'plain ci line\n' >"$S/ci2.md"
commit_all "$S" clean
run "$S" ci-base2...HEAD
expect "range passes on a clean branch" 0 "out:no-dashes: additions clean (ci-base2...HEAD)" noerr
run "$S" ci-base2..HEAD
expect "two-dot range form" 0 "out:additions clean (ci-base2..HEAD)"
run "$S" no-such-ref...HEAD
expect "bad range fails closed" 1 "err:failing closed"
empty="$(git -C "$S" hash-object -t tree /dev/null)"
run "$S" "$empty..HEAD"
expect "empty-tree range checks every line at HEAD" 1 "err:legacy.md:2:" "err:ci.md:1:" "err:bypass.md:1:" "!err:blob.bin"
printf 'x\n' >"$S/ci-base2...HEAD"
run "$S" ci-base2...HEAD
expect "a file named like the range does not confuse git" 0 "out:additions clean (ci-base2...HEAD)"
rm "$S/ci-base2...HEAD"

echo "tracked tree (--tree)"
T="$(new_repo tree)"
mkdir -p "$T/docs"
printf 'clean\n' >"$T/docs/a.md"
commit_all "$T" base
run "$T" --tree
expect "--tree passes on a clean tree" 0 "out:no-dashes: tree clean" noerr
printf 'a\nb %s c\n' "$EM" >"$T/docs/a.md"
printf 'root %s line\n' "$EN" >"$T/root.md"
commit_all "$T" dashes
run "$T" --tree
expect "--tree reports every tracked line" 1 "err:no-dashes: docs/a.md:2: b <EM-DASH> c" "err:root.md:1: root <EN-DASH> line" "err:2 line(s) in tracked files" noout
run "$T/docs" --tree
expect "--tree from a subdirectory scans the whole repository" 1 "err:no-dashes: root.md:1:" "err:docs/a.md:2:"
run "$T" --tree --limit 1
expect "--tree honours --limit" 1 "err:1 more not shown" "err:2 line(s) in tracked files"
git -C "$T" rm -q -f docs/a.md root.md
git -C "$T" commit -qm "remove dashes"
printf 'untracked %s\n' "$EM" >"$T/untracked.md"
printf 'bin\000%s\n' "$EM" >"$T/blob.bin"
git -C "$T" add blob.bin
run "$T" --tree
expect "--tree skips untracked and binary files" 0 "out:tree clean"
printf 'x %s y\n' "$EM" >"$T/planted.txt"
git -C "$T" add planted.txt
run "$T" --tree
expect "--tree sees a staged but uncommitted file" 1 "err:planted.txt:1:"
git -C "$T" rm -q -f planted.txt
run "$T" --tree
expect "--tree passes again after removal" 0 "out:tree clean"

echo "fail closed on Git errors"
mkdir -p "$TMP/not-a-repo"
run "$TMP/not-a-repo"
expect "staged mode outside a repository fails closed" 1 "err:failing closed"
run "$TMP/not-a-repo" --tree
expect "--tree outside a repository fails closed" 1 "err:failing closed"
run "$TMP/not-a-repo" HEAD~1..HEAD
expect "range outside a repository fails closed" 1 "err:failing closed"
mkdir -p "$TMP/fail-git"
cat >"$TMP/fail-git/git" <<'EOF'
#!/bin/sh
echo "fatal: simulated failure" >&2
exit 128
EOF
chmod +x "$TMP/fail-git/git"
in_dir "$S" env PATH="$TMP/fail-git:$PATH" "$SH" "$TOOL"
expect "failing git diff fails closed" 1 "err:simulated failure" "err:git diff --cached failed; failing closed"
in_dir "$T" env PATH="$TMP/fail-git:$PATH" "$SH" "$TOOL" --tree
expect "failing git rev-parse fails closed" 1 "err:failing closed"
mkdir -p "$TMP/grep-fail-git"
cat >"$TMP/grep-fail-git/git" <<'EOF'
#!/bin/sh
for a in "$@"; do
  if [ "$a" = grep ]; then echo "fatal: simulated grep failure" >&2; exit 2; fi
done
exec "$REAL_GIT" "$@"
EOF
chmod +x "$TMP/grep-fail-git/git"
in_dir "$T" env REAL_GIT="$(command -v git)" PATH="$TMP/grep-fail-git:$PATH" "$SH" "$TOOL" --tree
expect "failing git grep fails closed" 1 "err:git grep failed (exit 2); failing closed"

echo "hostile Git configuration"
H="$(new_repo hostile)"
cat >"$TMP/hostile-config" <<EOF
[include]
	path = $GIT_CONFIG_GLOBAL
[diff]
	external = true
	noprefix = true
	mnemonicPrefix = true
	relative = true
[diff "hide"]
	textconv = true
[color]
	ui = always
[core]
	quotePath = true
[grep]
	column = true
	lineNumber = false
	fullName = true
EOF
mkdir -p "$H/sub"
printf 'keep\n' >"$H/sub/keep.md"
printf '*.md diff=hide\n' >"$H/.gitattributes"
commit_all "$H" base
printf 'x %s y\n' "$EM" >"$H/root.md"
git -C "$H" add root.md
in_dir "$H/sub" env GIT_CONFIG_GLOBAL="$TMP/hostile-config" "$SH" "$TOOL"
expect "staged scan ignores external diff, textconv, relative and colour config" 1 "err:no-dashes: root.md:1: x <EM-DASH> y"
commit_all "$H" dash
in_dir "$H/sub" env GIT_CONFIG_GLOBAL="$TMP/hostile-config" "$SH" "$TOOL" HEAD~1..HEAD
expect "range scan ignores the same config" 1 "err:no-dashes: root.md:1: x <EM-DASH> y"
in_dir "$H/sub" env GIT_CONFIG_GLOBAL="$TMP/hostile-config" "$SH" "$TOOL" --tree
expect "--tree ignores grep column, line number and colour config" 1 "err:no-dashes: root.md:1: x <EM-DASH> y"

echo "vendored drop-in as scripts/check-no-dashes.sh"
V="$(new_repo vendored)"
mkdir -p "$V/scripts"
cp "$TOOL" "$V/scripts/check-no-dashes.sh"
cp "$HERE/test.sh" "$V/scripts/check-no-dashes.test.sh"
chmod +x "$V/scripts/check-no-dashes.sh"
commit_all "$V" vendor
in_dir "$V" scripts/check-no-dashes.sh --help
expect "executed directly, usage names the vendored file" 0 "start:Usage: check-no-dashes.sh "
in_dir "$V" "$SH" scripts/check-no-dashes.sh --tree
expect "the tool and its tests contain no dash glyph" 0 "out:no-dashes: tree clean"
printf 'hook %s case\n' "$EN" >"$V/hook.md"
git -C "$V" add hook.md
in_dir "$V" "$V/scripts/check-no-dashes.sh"
expect "pre-commit hook call by absolute path" 1 "err:hook.md:1: hook <EN-DASH> case"

echo ""
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
