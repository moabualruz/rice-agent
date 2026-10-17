#!/usr/bin/env bash
# Tests for ratchet in throwaway Git repositories with deterministic metrics: init baseline, keep,
# discard with revert, strict improvement, guard-fail, scope-fail (outside --editable, --frozen,
# a command file, .ratchet/), crash, no number, timeout, measurement leftovers, noise floor from
# --noise and from the baseline spread, budget exhaustion, refusals (default branch, dirty work
# tree, moved branch), SIGTERM mid-trial, baseline rollback, status, finish, --dry-run and --help.
# Runs on macOS and Linux; RATCHET_TEST_BASH=/bin/bash runs the tool under that Bash (3.2).
set -euo pipefail

cd "$(dirname "$0")"
TOOL="$PWD/ratchet"
RB=${RATCHET_TEST_BASH:-}
HANG=3737

tmp=$(mktemp -d "${TMPDIR:-/tmp}/ratchet-test.XXXXXX")
export HOME="$tmp/home" XDG_CONFIG_HOME="$tmp/home/.config" GIT_CONFIG_NOSYSTEM=1
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_CEILING_DIRECTORIES
mkdir -p "$HOME"
git config --global user.name ratchet-test
git config --global user.email ratchet-test@example.invalid
git config --global init.defaultBranch main
git config --global commit.gpgsign false

hangers() { ps -A -o pid= -o command= | awk -v s="sleep $HANG" '{ p = $1; $1 = ""; sub(/^ /, "") } $0 == s { print p }'; }

cleanup() {
  local p
  for p in $(hangers); do kill -KILL "$p" 2>/dev/null || true; done
  cd /
  rm -rf "$tmp"
}
trap cleanup EXIT

pass=0
fail=0
ok() {
  pass=$((pass + 1))
  printf 'ok   %s\n' "$1"
}
fails() {
  fail=$((fail + 1))
  printf 'FAIL %s\n     rc=%s out=%s err=%s\n' "$1" "$rc" "$out" "$err"
}

run() {
  set +e
  if [ -n "$RB" ]; then
    "$RB" "$TOOL" "$@" >"$tmp/out" 2>"$tmp/err"
  else
    "$TOOL" "$@" >"$tmp/out" 2>"$tmp/err"
  fi
  rc=$?
  set -e
  out=$(cat "$tmp/out")
  err=$(cat "$tmp/err")
}

has() { printf '%s\n' "$1" | grep -q -- "$2"; }
lines() { wc -l <notes.txt | tr -d ' '; }
rows() { awk 'END { print NR - 1 }' .ratchet/results.tsv; }
last_field() { tail -n 1 .ratchet/results.tsv | cut -f "$1"; }
clean() { [ -z "$(git status --porcelain --untracked-files=all)" ]; }
branch() { git symbolic-ref -q --short HEAD || echo detached; }
no_ratchet_branch() { [ -z "$(git branch --list 'ratchet/*')" ]; }
drop() { # N: drop the first N lines of notes.txt
  tail -n +"$(($1 + 1))" notes.txt >"$tmp/notes"
  cat "$tmp/notes" >notes.txt
}

new_repo() {
  local d="$tmp/$1" i=1
  mkdir -p "$d/src"
  cd "$d"
  git init -q
  while [ "$i" -le 10 ]; do
    echo "line $i"
    i=$((i + 1))
  done >notes.txt
  cat >metric.sh <<EOF
#!/usr/bin/env bash
# deterministic metric: lines in notes.txt; markers in the file select failure modes
if grep -q CRASH notes.txt; then echo "metric broke" >&2; exit 1; fi
if grep -q HANG notes.txt; then sleep $HANG; fi
if grep -q NONUM notes.txt; then echo "no number here"; exit 0; fi
if grep -q LITTER notes.txt; then echo x >litter.txt; fi
echo "measuring notes.txt"
wc -l <notes.txt | tr -d ' '
EOF
  printf '#!/usr/bin/env bash\n! grep -q BROKEN notes.txt\n' >guard.sh
  echo other >other.txt
  echo fixture >src/fixture.txt
  echo 'exit 0' >src/check.sh
  git add -A
  git commit -qm fixture
}

# --- usage contract ----------------------------------------------------------------------------
run --help
if [ "$rc" -eq 0 ] && [ "$(head -n 1 "$tmp/out" | cut -d' ' -f1-3)" = "Usage: ratchet init" ] && has "$out" "--dry-run"; then
  ok "--help prints Usage first, documents --dry-run and exits 0"
else fails "--help"; fi
run
if [ "$rc" -eq 2 ]; then ok "no arguments is a usage error"; else fails "no arguments"; fi
run frobnicate
if [ "$rc" -eq 2 ]; then ok "unknown subcommand is a usage error"; else fails "unknown subcommand"; fi
run init --direction lower --guard true --editable x
if [ "$rc" -eq 2 ]; then ok "init without --metric is a usage error"; else fails "init without --metric"; fi
run init --metric "echo 1" --direction sideways --guard true --editable x
if [ "$rc" -eq 2 ]; then ok "init with a bad --direction is a usage error"; else fails "bad --direction"; fi
run init --metric "echo 1" --direction lower --guard true --editable x --runs 0
if [ "$rc" -eq 2 ]; then ok "init with --runs 0 is a usage error"; else fails "--runs 0"; fi
run try
if [ "$rc" -eq 2 ]; then ok "try without -m is a usage error"; else fails "try without -m"; fi

# --- main repository: lower is better, lines of notes.txt ---------------------------------------
new_repo r1
start=$(git rev-parse HEAD)

run try -m "nothing set up"
if [ "$rc" -eq 3 ] && has "$err" "no ratchet on main"; then
  ok "try on the default branch without a ratchet refuses with 3"
else fails "try on main"; fi

echo scratch >untracked.txt
run init --metric "bash metric.sh" --direction lower --guard "bash guard.sh" --editable notes.txt
if [ "$rc" -eq 3 ] && has "$err" "uncommitted" && no_ratchet_branch; then
  ok "init refuses a dirty work tree with 3 and creates no branch"
else fails "init on a dirty work tree"; fi
rm -f untracked.txt

run init --metric "bash metric.sh" --direction lower --guard "bash guard.sh && bash src/check.sh" \
  --editable notes.txt src/ --frozen src/fixture.txt --timeout 3 --dry-run
if [ "$rc" -eq 0 ] && has "$out" "dry run: would create branch ratchet/" && no_ratchet_branch && [ ! -e .ratchet ] &&
  [ "$(branch)" = main ]; then
  ok "init --dry-run prints the plan and creates nothing"
else fails "init --dry-run"; fi

run init --metric "bash metric.sh" --direction lower --guard "bash guard.sh && bash src/check.sh" \
  --editable notes.txt src/ --frozen src/fixture.txt --timeout 3
rb=$(branch)
if [ "$rc" -eq 0 ] && has "$out" "^baseline metric=10 spread=0 runs=10,10,10 best=10 floor=0 trial=0/20$"; then
  ok "init measures the baseline (median of 3 runs) and prints it"
else fails "init baseline"; fi
if printf '%s\n' "$rb" | grep -Eq '^ratchet/[0-9]{8}-1$'; then
  ok "init switches to a new ratchet/<date>-1 branch"
else fails "init branch name ($rb)"; fi
if [ -n "$(git ls-files .ratchet/config)" ] && [ -z "$(git ls-files .ratchet/results.tsv)" ] && clean; then
  ok "init commits .ratchet/config on the branch; results stay untracked"
else fails "init files"; fi
if has "$out" "^frozen: src/fixture.txt metric.sh guard.sh src/check.sh$"; then
  ok "init freezes --frozen paths and the files named by the commands"
else fails "init frozen list"; fi
if [ "$(git rev-parse main)" = "$start" ]; then ok "init leaves main at the start commit"; else fails "main moved"; fi
if [ "$(head -n 1 .ratchet/results.tsv | cut -f 1,7)" = "time	status" ] && [ "$(rows)" -eq 1 ] && [ "$(last_field 7)" = baseline ]; then
  ok "results.tsv has the header and one baseline row"
else fails "results.tsv after init"; fi

run init --metric "bash metric.sh" --direction lower --guard true --editable notes.txt
if [ "$rc" -eq 3 ] && has "$err" "already carries a ratchet"; then
  ok "init refuses on a branch that already carries a ratchet"
else fails "nested init"; fi

run try -m "nothing changed"
if [ "$rc" -eq 2 ]; then ok "try without a candidate change is a usage error"; else fails "try without a change"; fi

best=$(git rev-parse HEAD)
drop 2
run try -m "drop two lines"
if [ "$rc" -eq 0 ] && [ "$out" = "keep metric=8 best=8 floor=0 trial=1/20" ] && [ "$(git rev-parse HEAD~1)" = "$best" ] && clean &&
  [ "$(last_field 7)" = keep ]; then
  ok "keep: a strict improvement advances the branch (exit 0)"
else fails "keep"; fi
best=$(git rev-parse HEAD)

drop 1
run try --dry-run -m "drop one more"
if [ "$rc" -eq 0 ] && has "$out" "dry run: trial 2/20 would commit" && [ "$(git rev-parse HEAD)" = "$best" ] && [ "$(lines)" -eq 7 ] &&
  [ "$(rows)" -eq 2 ]; then
  ok "try --dry-run prints the plan; commits, resets and logs nothing"
else fails "try --dry-run"; fi
git checkout -q -- notes.txt

echo "extra" >>notes.txt
run try -m "add a line"
if [ "$rc" -eq 1 ] && [ "$out" = "discard metric=9 best=8 floor=0 trial=2/20" ] && [ "$(git rev-parse HEAD)" = "$best" ] &&
  [ "$(lines)" -eq 8 ] && clean; then
  ok "discard: a worse metric resets to the best commit (exit 1)"
else fails "discard"; fi
discarded=$(last_field 2)
if git cat-file -e "$discarded^{commit}" && git show "$discarded:notes.txt" | grep -q extra; then
  ok "a discarded candidate stays recoverable by the commit in its row"
else fails "discarded commit $discarded"; fi

{
  echo "rewritten"
  tail -n +2 notes.txt
} >"$tmp/notes"
cat "$tmp/notes" >notes.txt
run try -m "same length"
if [ "$rc" -eq 1 ] && has "$out" "^discard metric=8 best=8" && clean; then
  ok "discard: an equal metric is not a strict improvement"
else fails "equal metric"; fi

drop 3
echo BROKEN >>notes.txt
run try -m "break the guard"
if [ "$rc" -eq 1 ] && has "$out" "^guard-fail metric=- best=8" && [ "$(git rev-parse HEAD)" = "$best" ] && ! grep -q BROKEN notes.txt &&
  clean && [ "$(last_field 6)" = fail ]; then
  ok "guard-fail: a failing guard resets even when the metric would improve"
else fails "guard-fail"; fi

drop 1
echo changed >other.txt
run try -m "touch other"
if [ "$rc" -eq 1 ] && has "$out" "^scope-fail" && has "$err" "other.txt (outside --editable)" && [ "$(cat other.txt)" = other ] &&
  [ "$(lines)" -eq 8 ] && clean; then
  ok "scope-fail: a path outside --editable resets the whole candidate"
else fails "scope-fail outside"; fi

echo new >newfile.txt
run try -m "new file outside"
if [ "$rc" -eq 1 ] && [ ! -e newfile.txt ] && clean; then
  ok "scope-fail: a new untracked file outside --editable is removed"
else fails "scope-fail new file"; fi

echo changed >src/fixture.txt
run try -m "edit fixture"
if [ "$rc" -eq 1 ] && has "$err" "src/fixture.txt (frozen src/fixture.txt)" && [ "$(cat src/fixture.txt)" = fixture ]; then
  ok "scope-fail: a --frozen path inside --editable"
else fails "scope-fail frozen"; fi

echo 'exit 0 # weakened' >src/check.sh
run try -m "edit guard script"
if [ "$rc" -eq 1 ] && has "$err" "src/check.sh (named by the metric or guard command)" && [ "$(cat src/check.sh)" = "exit 0" ]; then
  ok "scope-fail: a file named by the guard command, even inside --editable"
else fails "scope-fail command file"; fi

echo 'guard=true' >>.ratchet/config
run try -m "edit config"
if [ "$rc" -eq 1 ] && has "$err" ".ratchet/config (ratchet state)" && ! grep -q '^guard=true$' .ratchet/config && clean; then
  ok "scope-fail: .ratchet/ is always frozen"
else fails "scope-fail .ratchet"; fi

echo changed >other.txt
run try --dry-run -m "dry scope"
if [ "$rc" -eq 1 ] && has "$out" "would scope-fail" && [ "$(cat other.txt)" = changed ]; then
  ok "try --dry-run reports a scope-fail (exit 1) and reverts nothing"
else fails "dry-run scope-fail"; fi
git checkout -q -- other.txt

drop 1
echo helper >src/helper.txt
run try -m "drop a line and add a helper"
if [ "$rc" -eq 0 ] && has "$out" "^keep metric=7 best=7" && [ -n "$(git ls-files src/helper.txt)" ] && clean; then
  ok "keep: a new untracked file inside --editable is committed"
else fails "keep with a new file"; fi
best=$(git rev-parse HEAD)

drop 2
echo CRASH >>notes.txt
run try -m "crash the metric"
if [ "$rc" -eq 3 ] && has "$out" "^crash metric=- best=7" && has "$err" "metric run 1 exited 1" && [ "$(git rev-parse HEAD)" = "$best" ] &&
  clean; then
  ok "crash: a failing metric resets and exits 3"
else fails "crash"; fi

drop 2
echo NONUM >>notes.txt
run try -m "no number"
if [ "$rc" -eq 3 ] && has "$err" "printed no number" && clean; then
  ok "crash: a metric without a number on its last line"
else fails "no number"; fi

drop 2
echo HANG >>notes.txt
t0=$(date +%s)
run try -m "hang the metric"
t1=$(date +%s)
if [ "$rc" -eq 3 ] && has "$err" "timed out after 3s" && [ $((t1 - t0)) -lt 15 ] && [ "$(git rev-parse HEAD)" = "$best" ] && clean; then
  ok "crash: a metric over --timeout is killed and reset (exit 3, $((t1 - t0))s)"
else fails "timeout"; fi
if [ -z "$(hangers)" ]; then ok "timeout kills the whole process group of the metric"; else fails "leftover sleep $HANG"; fi

drop 2
echo HANG >>notes.txt
if [ -n "$RB" ]; then "$RB" "$TOOL" try -m "interrupted trial" >"$tmp/out" 2>"$tmp/err" & else
  "$TOOL" try -m "interrupted trial" >"$tmp/out" 2>"$tmp/err" &
fi
rp=$!
i=0
while [ -z "$(hangers)" ] && [ "$i" -lt 50 ]; do
  sleep 0.1
  i=$((i + 1))
done
kill -TERM "$rp"
set +e
wait "$rp"
rc=$?
set -e
out=$(cat "$tmp/out")
err=$(cat "$tmp/err")
if [ "$rc" -eq 3 ] && has "$err" "interrupted; reset to the best kept commit" && [ "$(git rev-parse HEAD)" = "$best" ] && clean &&
  [ "$(last_field 7)" = crash ] && has "$(last_field 8)" "(interrupted)" && [ ! -e .ratchet/lock ] && [ -z "$(hangers)" ]; then
  ok "SIGTERM during a trial kills the metric, resets, logs crash and exits 3"
else fails "interrupt"; fi

drop 2
echo LITTER >>notes.txt
run try -m "metric writes a file"
if [ "$rc" -eq 3 ] && has "$err" "changed the work tree" && [ ! -e litter.txt ] && clean; then
  ok "crash: a measurement that changes the work tree is reset and cleaned"
else fails "litter"; fi

git commit -q --allow-empty -m "manual commit"
drop 1
run try -m "after a manual commit"
if [ "$rc" -eq 3 ] && has "$err" "not the best kept commit" && [ "$(lines)" -eq 6 ]; then
  ok "try refuses when the branch moved outside ratchet and reverts nothing"
else fails "moved branch"; fi
git checkout -q -- notes.txt
git reset -q --hard "$best"

git checkout -q main
echo 1 >>notes.txt
run try -m "on main"
if [ "$rc" -eq 3 ] && [ "$(lines)" -eq 11 ]; then
  ok "try on the default branch refuses with 3 and leaves the change alone"
else fails "try on main after init"; fi
git checkout -q -- notes.txt
git checkout -q "$rb"

run status
if [ "$rc" -eq 0 ] && has "$out" "^best: 7 at " && has "$out" "^trials: 15 of 20 (keep 2, discard 2, guard-fail 1, crash 5, scope-fail 5)" &&
  has "$out" "head: at the best kept commit" && has "$out" "metric writes a file"; then
  ok "status shows best, trials by status and the last rows"
else fails "status"; fi

run finish
if [ "$rc" -eq 0 ] && has "$out" "metric 8  ratchet trial 1: drop two lines" && has "$out" "metric 7  ratchet trial 10: drop a line and add a helper" &&
  has "$out" "notes.txt" && has "$out" "src/helper.txt" && ! has "$out" '\.ratchet/config *|'; then
  ok "finish lists the kept commits and the diff stat without .ratchet/"
else fails "finish"; fi
if [ "$(git rev-parse main)" = "$start" ] && [ "$(branch)" = "$rb" ] && [ -z "$(git remote)" ]; then
  ok "finish never merges, pushes or switches"
else fails "finish side effects"; fi

# --- higher is better, --noise percent and the budget -------------------------------------------
mkdir "$tmp/r2"
cd "$tmp/r2"
git init -q
echo 100 >value.txt
git add -A
git commit -qm value
run init --metric "cat value.txt" --direction higher --guard "test -s value.txt" --editable value.txt --runs 1 --noise 5 --budget 3
if [ "$rc" -eq 0 ] && has "$out" "best=100 floor=5 trial=0/3" && has "$out" "^frozen: (none)$"; then
  ok "init with --noise 5 sets the floor to 5 percent of the best"
else fails "init --noise"; fi
echo 103 >value.txt
run try -m "gain of 3"
if [ "$rc" -eq 1 ] && [ "$out" = "discard metric=103 best=100 floor=5 trial=1/3" ] && [ "$(cat value.txt)" = 100 ]; then
  ok "noise floor: a gain inside --noise is discarded"
else fails "noise discard"; fi
echo 106 >value.txt
run try -m "gain of 6"
if [ "$rc" -eq 0 ] && [ "$out" = "keep metric=106 best=106 floor=5.3 trial=2/3" ]; then
  ok "noise floor: a gain beyond --noise is kept (higher is better)"
else fails "noise keep"; fi
echo 108 >value.txt
run try -m "gain of 2"
if [ "$rc" -eq 1 ] && has "$out" "trial=3/3"; then ok "the last trial of the budget still runs"; else fails "last trial"; fi
echo 200 >value.txt
run try -m "over budget"
if [ "$rc" -eq 4 ] && has "$out" "^budget-exhausted" && [ "$(cat value.txt)" = 200 ] && [ "$(rows)" -eq 4 ]; then
  ok "budget exhausted: exit 4, nothing run, changed or logged"
else fails "budget"; fi
git checkout -q -- value.txt

# --- the baseline spread as the noise floor -----------------------------------------------------
mkdir "$tmp/r3"
cd "$tmp/r3"
git init -q
echo 100 >value.txt
git add -A
git commit -qm value
cat >"$tmp/jitter.sh" <<EOF
#!/usr/bin/env bash
# value.txt plus a repeating offset of 0, 2, 4, so the baseline has a spread of 4
n=\$(cat "$tmp/counter" 2>/dev/null || echo 0)
echo \$((n + 1)) >"$tmp/counter"
echo \$((\$(cat value.txt) + (n % 3) * 2))
EOF
run init --metric "bash $tmp/jitter.sh" --direction lower --guard true --editable value.txt
if [ "$rc" -eq 0 ] && has "$out" "metric=102 spread=4 runs=100,102,104 best=102 floor=4"; then
  ok "baseline spread: median 102 of 100,102,104 and floor 4"
else fails "baseline spread"; fi
echo 99 >value.txt
run try -m "gain of 1"
if [ "$rc" -eq 1 ] && has "$out" "^discard metric=101 best=102 floor=4"; then
  ok "noise floor: a gain inside the baseline spread is discarded"
else fails "spread discard"; fi
echo 95 >value.txt
run try -m "gain of 5"
if [ "$rc" -eq 0 ] && has "$out" "^keep metric=97 best=97 floor=4"; then
  ok "noise floor: a gain beyond the baseline spread is kept"
else fails "spread keep"; fi

# --- baseline failures roll back; branch numbering ----------------------------------------------
new_repo r4
start=$(git rev-parse HEAD)
run init --metric "bash metric.sh" --direction lower --guard false --editable notes.txt
if [ "$rc" -eq 1 ] && [ "$(branch)" = main ] && no_ratchet_branch && [ ! -e .ratchet ] && clean && [ "$(git rev-parse HEAD)" = "$start" ]; then
  ok "init: a failing guard at the baseline exits 1, removes the branch and returns to main"
else fails "baseline guard failure"; fi
run init --metric "exit 5" --direction lower --guard true --editable notes.txt
if [ "$rc" -eq 3 ] && [ "$(branch)" = main ] && no_ratchet_branch && clean; then
  ok "init: a crashing metric at the baseline exits 3 and removes the branch"
else fails "baseline metric crash"; fi
run init --metric "bash metric.sh" --direction lower --guard true --editable notes.txt
first=$(branch)
git checkout -q main
run init --metric "bash metric.sh" --direction lower --guard true --editable notes.txt
if [ "$rc" -eq 0 ] && [ "${first%-1}-2" = "$(branch)" ] && [ "$(git rev-parse main)" = "$start" ]; then
  ok "a second init from main creates the next branch number"
else fails "second init"; fi
archived=$(find .ratchet/archive -name results.tsv 2>/dev/null | head -n 1)
if [ -n "$archived" ] && [ "$(awk 'END { print NR }' "$archived")" -eq 2 ] && has "$err" "moved the earlier untracked log" && [ "$(rows)" -eq 1 ]; then
  ok "the second init archives the earlier untracked log instead of overwriting it"
else fails "archive ($archived)"; fi
git checkout -q main
run init --metric "bash metric.sh" --direction lower --guard true --editable ../outside
if [ "$rc" -eq 2 ]; then ok "init rejects an --editable path outside the repository"; else fails "outside path"; fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
