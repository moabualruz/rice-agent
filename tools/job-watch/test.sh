#!/usr/bin/env bash
# shellcheck disable=SC2016 # single-quoted job commands expand in the job, not here
# Tests for job-watch: start, status, wait (exit 0, non-zero, timeout), PID reuse, stop of the
# whole process group, log truncation, list and the usage contract. Runs on macOS and Linux.
set -euo pipefail

cd "$(dirname "$0")"
TOOL="$PWD/job-watch"

tmp=$(mktemp -d "${TMPDIR:-/tmp}/job-watch-test.XXXXXX")
export AGENT_DRY_STATE="$tmp/state"
outsider=""

cleanup() {
  local d pid pgid
  if [ -d "$AGENT_DRY_STATE/jobs" ]; then
    for d in "$AGENT_DRY_STATE"/jobs/*/; do
      [ -f "$d/pgid" ] || continue
      pid=$(cat "$d/pid" 2>/dev/null || true)
      pgid=$(cat "$d/pgid" 2>/dev/null || true)
      if [ -n "$pgid" ] && [ "$pgid" = "$pid" ]; then
        kill -KILL -- "-$pgid" 2>/dev/null || true
      fi
    done
  fi
  if [ -n "$outsider" ]; then kill "$outsider" 2>/dev/null || true; fi
  rm -rf "$tmp"
}
trap cleanup EXIT

pass=0
fail=0
ok() {
  pass=$((pass + 1))
  printf 'ok   %s\n' "$1"
}
bad() {
  fail=$((fail + 1))
  printf 'FAIL %s\n' "$1"
  if [ $# -gt 1 ]; then printf '     %s\n' "$2"; fi
}

run() {
  set +e
  "$TOOL" "$@" >"$tmp/out" 2>"$tmp/err"
  rc=$?
  set -e
  out=$(cat "$tmp/out")
  err=$(cat "$tmp/err")
}

expect_rc() {
  if [ "$rc" -eq "$2" ]; then ok "$1"; else bad "$1" "exit $rc, want $2; out=$out err=$err"; fi
}

field() { # key, from $out
  printf '%s\n' "$out" | sed -n "s/^$1: //p" | head -n1
}

group_count() {
  ps -A -o pgid= -o stat= | awk -v g="$1" '$1 == g && $2 !~ /^Z/ { n++ } END { print n + 0 }'
}

jdir() { printf '%s/jobs/%s' "$AGENT_DRY_STATE" "$1"; }

# --help contract and usage errors
run --help
if [ "$rc" -eq 0 ] && [ "$(head -n1 "$tmp/out")" = "Usage: job-watch start --name <name> [--dry-run] -- <command> [args...]" ]; then
  ok "--help prints Usage first and exits 0"
else
  bad "--help" "rc=$rc first line: $(head -n1 "$tmp/out")"
fi
run
expect_rc "no arguments is a usage error" 2
run frobnicate
expect_rc "unknown subcommand is a usage error" 2
run start -- true
expect_rc "start without --name is a usage error" 2
run start --name x
expect_rc "start without a command is a usage error" 2
run start --name ../escape -- true
expect_rc "start rejects a path-like name" 2
run status nosuchjob
expect_rc "status of an unknown job exits 2" 2
run wait nosuchjob --timeout 1
expect_rc "wait on an unknown job exits 2" 2
run wait x --timeout soon
expect_rc "wait with a bad --timeout is a usage error" 2

# start --dry-run changes nothing
run start --name dry --dry-run -- sleep 30
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'dry run: would start' && [ ! -e "$(jdir dry)" ]; then
  ok "start --dry-run prints the plan and creates nothing"
else
  bad "start --dry-run" "rc=$rc out=$out"
fi

# start returns at once, detached, in its own process group
my_pgid=$(ps -o pgid= -p $$ | tr -d '[:space:]')
t0=$(date +%s)
out=$("$TOOL" start --name sleeper -- bash -c 'echo hello-from-job; sleep 30')
t1=$(date +%s)
pid=$(field pid)
pgid=$(field pgid)
if [ $((t1 - t0)) -le 3 ] && [ -n "$pid" ] && [ "$pid" = "$pgid" ] && [ "$pgid" != "$my_pgid" ]; then
  ok "start returns at once with the job as leader of its own group (pgid $pgid)"
else
  bad "start detach" "took $((t1 - t0))s pid=$pid pgid=$pgid caller=$my_pgid out=$out"
fi
for f in pid birth pgid cmd log; do
  if [ ! -s "$(jdir sleeper)/$f" ] && [ "$f" != log ]; then bad "record file $f"; fi
done
if [ "$(ps -o pgid= -p "$pid" | tr -d '[:space:]')" = "$pgid" ]; then ok "recorded pgid matches ps"; else bad "pgid matches ps"; fi

run wait sleeper --timeout 1
if [ "$rc" -eq 4 ] && [ "$(field state)" = running ] && printf '%s' "$out" | grep -q 'hello-from-job'; then
  ok "wait --timeout 1 on a running job exits 4 and shows the log tail"
else
  bad "wait timeout" "rc=$rc out=$out err=$err"
fi
run status sleeper
if [ "$rc" -eq 0 ] && [ "$(field state)" = running ]; then ok "status reports running"; else bad "status running" "rc=$rc out=$out"; fi
run start --name sleeper -- true
if [ "$rc" -eq 1 ] && printf '%s' "$err" | grep -q 'still running'; then ok "start refuses a name that is still running"; else bad "start refuse" "rc=$rc err=$err"; fi

# exit code capture
"$TOOL" start --name good -- bash -c 'echo fine' >/dev/null
run wait good --timeout 10
if [ "$rc" -eq 0 ] && [ "$(field state)" = exited ] && [ "$(field exit)" = 0 ]; then ok "wait returns 0 for a job that exits 0"; else bad "wait 0" "rc=$rc out=$out"; fi
"$TOOL" start --name failing -- bash -c 'echo oops >&2; exit 7' >/dev/null
run wait failing --timeout 10
if [ "$rc" -eq 7 ] && [ "$(field exit)" = 7 ] && printf '%s' "$out" | grep -q oops; then ok "wait returns the job's exit status 7 and stderr is logged"; else bad "wait 7" "rc=$rc out=$out"; fi
run status failing
expect_rc "status of a job that exited non-zero exits 1" 1
"$TOOL" start --name missing -- /nonexistent/command >/dev/null
run wait missing --timeout 10
expect_rc "a command that cannot run reports 127" 127
run start --name good -- bash -c 'exit 0'
expect_rc "start replaces a finished job of the same name" 0
"$TOOL" wait good --timeout 10 >/dev/null

# log truncation
"$TOOL" start --name noisy -- bash -c 'i=1; while [ $i -le 500 ]; do echo line$i; i=$((i + 1)); done' >/dev/null
"$TOOL" wait noisy --timeout 10 >/dev/null
run status noisy --tail 5
tail_lines=$(printf '%s\n' "$out" | grep -c '^line' || true)
if [ "$rc" -eq 0 ] && [ "$tail_lines" -eq 5 ] && printf '%s\n' "$out" | tail -n1 | grep -qx 'line500' &&
  printf '%s' "$out" | grep -q 'last 5 of 500 lines'; then
  ok "status --tail 5 prints only the last 5 of 500 log lines"
else
  bad "log tail" "lines=$tail_lines out=$out"
fi
run status noisy
tail_lines=$(printf '%s\n' "$out" | grep -c '^line' || true)
if [ "$tail_lines" -eq 20 ]; then ok "status defaults to the last 20 log lines"; else bad "default tail" "lines=$tail_lines"; fi
run status noisy --tail 500 --max-bytes 30
log_bytes=$(printf '%s\n' "$out" | sed -n '/^log: /,$p' | sed 1d | wc -c | tr -d '[:space:]')
if [ "$log_bytes" -le 31 ]; then ok "--max-bytes caps the printed log ($log_bytes bytes)"; else bad "max-bytes" "bytes=$log_bytes"; fi

# PID reuse: a record whose birth differs from the live process is never the job
"$TOOL" start --name reuse -- sleep 60 >/dev/null
reuse_pid=$(cat "$(jdir reuse)/pid")
printf 'Thu Jan 1 00:00:00 1970\n' >"$(jdir reuse)/birth"
run status reuse
if [ "$rc" -eq 1 ] && [ "$(field state)" = lost ] && printf '%s' "$out" | grep -q 'belongs to another process'; then
  ok "status never reports a PID with a different birth time as the job"
else
  bad "pid reuse status" "rc=$rc out=$out"
fi
run stop reuse
if [ "$rc" -eq 0 ] && kill -0 "$reuse_pid" 2>/dev/null && printf '%s' "$out" | grep -q 'no signal sent'; then
  ok "stop does not signal a PID whose birth time differs"
else
  bad "pid reuse stop" "rc=$rc out=$out"
fi
run wait reuse --timeout 0
expect_rc "wait on a lost job exits 1" 1
kill -KILL -- "-$reuse_pid" 2>/dev/null || true

# stop ends every process in the group and nothing outside it
sleep 60 &
outsider=$!
"$TOOL" start --name grp -- bash -c 'sleep 60 & sleep 60 & wait' >/dev/null
gpid=$(cat "$(jdir grp)/pgid")
i=0
while [ "$(group_count "$gpid")" -lt 4 ] && [ "$i" -lt 50 ]; do
  sleep 0.1
  i=$((i + 1))
done
members=$(group_count "$gpid")
run stop grp --dry-run
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'dry run: would send SIGTERM' && [ "$(group_count "$gpid")" -eq "$members" ]; then
  ok "stop --dry-run sends nothing"
else
  bad "stop --dry-run" "rc=$rc out=$out"
fi
run stop grp
if [ "$rc" -eq 0 ] && [ "$members" -ge 4 ] && [ "$(group_count "$gpid")" -eq 0 ] && kill -0 "$outsider" 2>/dev/null &&
  [ "$(field exit)" = 143 ] && printf '%s' "$out" | grep -q 'stopped: SIGTERM'; then
  ok "stop terminates all $members processes in the group, exit 143, outsider untouched"
else
  bad "stop group" "rc=$rc members=$members left=$(group_count "$gpid") out=$out"
fi
kill "$outsider" 2>/dev/null || true
wait "$outsider" 2>/dev/null || true
outsider=""
run stop grp
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'nothing to stop'; then ok "stop of a finished job is a no-op"; else bad "stop no-op" "rc=$rc out=$out"; fi

# a job that ignores SIGTERM is killed after the grace period
"$TOOL" start --name stubborn -- bash -c 'trap "" TERM; sleep 60' >/dev/null
spgid=$(cat "$(jdir stubborn)/pgid")
sleep 0.3
run stop stubborn --grace 1
if [ "$rc" -eq 0 ] && [ "$(group_count "$spgid")" -eq 0 ] && printf '%s' "$out" | grep -q 'stopped: SIGKILL'; then
  ok "stop escalates to SIGKILL after --grace"
else
  bad "stop escalation" "rc=$rc out=$out"
fi

# leftovers: the job exits but leaves a child in its group; stop still ends it
"$TOOL" start --name leaver -- bash -c 'sleep 60 & echo spawned' >/dev/null
"$TOOL" wait leaver --timeout 10 >/dev/null
lpgid=$(cat "$(jdir leaver)/pgid")
run status leaver
if printf '%s' "$out" | grep -q 'processes still in group'; then ok "status reports processes left in the group"; else bad "leftover status" "$out"; fi
run stop leaver
if [ "$rc" -eq 0 ] && [ "$(group_count "$lpgid")" -eq 0 ]; then ok "stop ends processes the job left in its group"; else bad "leftover stop" "rc=$rc out=$out"; fi

# list
run list
if [ "$rc" -eq 0 ] && printf '%s\n' "$out" | grep -q '^NAME' && printf '%s\n' "$out" | grep -Eq '^sleeper +running' &&
  printf '%s\n' "$out" | grep -Eq '^failing +exited +7'; then
  ok "list shows each job with state and exit code"
else
  bad "list" "$out"
fi

"$TOOL" stop sleeper >/dev/null

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
