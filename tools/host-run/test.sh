#!/usr/bin/env bash
# shellcheck disable=SC2016 # literal $ and backticks are the test data
# Tests for host-run. A stub ssh on PATH asserts that the remote command is exactly `bash -s`,
# parses it the way a login shell would, and runs it locally with the script on stdin.
set -euo pipefail

cd "$(dirname "$0")"
TOOL="$PWD/host-run"

tmp=$(mktemp -d "${TMPDIR:-/tmp}/host-run-test.XXXXXX")
cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT

mkdir -p "$tmp/bin" "$tmp/t"
export TMPDIR="$tmp/t"
export STUB_SSH_LOG="$tmp/ssh.log"

cat >"$tmp/bin/ssh" <<'STUB'
#!/usr/bin/env bash
# Stub ssh: record argv, check the option shape, then act as the remote login shell.
set -u
{
  printf 'call'
  for a in "$@"; do printf ' [%s]' "$a"; done
  printf '\n'
} >>"$STUB_SSH_LOG"
remote=""
target=""
saw_t=0 saw_batch=0 saw_ct=0
prev=""
for a in "$@"; do
  case "$a" in
    -T) saw_t=1 ;;
    BatchMode=yes) [ "$prev" = "-o" ] && saw_batch=1 ;;
    ConnectTimeout=*) [ "$prev" = "-o" ] && saw_ct=1 ;;
  esac
  prev=$a
  target=$remote
  remote=$a
done
if [ "$remote" != "bash -s" ]; then
  echo "stub-ssh: remote command is not exactly 'bash -s': $remote" >&2
  exit 99
fi
if [ "$saw_t$saw_batch$saw_ct" != "111" ]; then
  echo "stub-ssh: missing -T, BatchMode=yes or ConnectTimeout" >&2
  exit 98
fi
if [ -n "${STUB_SSH_EXIT:-}" ]; then
  echo "ssh: connect to host $target port 22: Connection refused" >&2
  exit "$STUB_SSH_EXIT"
fi
case "$target" in
  *fish*)
    # fish login shell fixture: real fish when installed, otherwise reject any syntax that
    # fish parses differently from POSIX shells before running the command.
    if command -v fish >/dev/null 2>&1; then
      exec fish -c "$remote"
    fi
    case "$remote" in
      *[!A-Za-z0-9\ ._-]*)
        echo "stub-ssh(fish): fish cannot parse: $remote" >&2
        exit 97
        ;;
    esac
    ;;
esac
exec bash -c "$remote"
STUB
chmod +x "$tmp/bin/ssh"
export PATH="$tmp/bin:$PATH"

cat >"$tmp/config.json" <<'JSON'
{
  "hosts": {
    "fishbox": { "ssh": "tester@stub-fish", "shell": "fish" },
    "bashbox": { "ssh": "tester@stub-bash", "shell": "bash" },
    "badbox": { "ssh": "-oProxyCommand=evil" }
  }
}
JSON
export AGENT_DRY_CONFIG="$tmp/config.json"

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

# run captures stdout, stderr and the exit code of the tool.
run() {
  set +e
  "$TOOL" "$@" >"$tmp/out" 2>"$tmp/err"
  rc=$?
  set -e
  out=$(cat "$tmp/out")
  err=$(cat "$tmp/err")
}

calls() {
  if [ -f "$STUB_SSH_LOG" ]; then wc -l <"$STUB_SSH_LOG" | tr -d '[:space:]'; else echo 0; fi
}

expect_rc() { # name expected
  if [ "$rc" -eq "$2" ]; then ok "$1"; else bad "$1" "exit $rc, want $2; stderr: $err"; fi
}

# --help contract
run --help
if [ "$rc" -eq 0 ] && [ "$(head -n1 "$tmp/out")" = "Usage: host-run <host> [options] -- <command> [args...]" ]; then
  ok "--help prints Usage first and exits 0"
else
  bad "--help" "rc=$rc first line: $(head -n1 "$tmp/out")"
fi
run -h
expect_rc "-h exits 0" 0

# usage errors
run
expect_rc "no arguments is a usage error" 2
run bashbox
expect_rc "host without command is a usage error" 2
run bashbox --
expect_rc "empty command after -- is a usage error" 2
run bashbox --bogus -- true
expect_rc "unknown option is a usage error" 2
run bashbox --tail abc -- true
expect_rc "non-numeric --tail is a usage error" 2
run bashbox --script "$tmp/missing.sh"
expect_rc "missing script file is a usage error" 2

# setup failures
AGENT_DRY_CONFIG="$tmp/nope.json" run bashbox -- true
if [ "$rc" -eq 3 ] && printf '%s' "$err" | grep -q 'config not found'; then ok "missing config exits 3"; else bad "missing config" "rc=$rc err=$err"; fi
run nosuchhost -- true
if [ "$rc" -eq 3 ] && printf '%s' "$err" | grep -q "unknown host 'nosuchhost'"; then ok "unknown host exits 3"; else bad "unknown host" "rc=$rc err=$err"; fi
run badbox -- true
expect_rc "ssh target with a leading dash exits 3" 3

# basic run and option shape
: >"$STUB_SSH_LOG"
run bashbox -- echo hi
if [ "$rc" -eq 0 ] && [ "$out" = "hi" ]; then ok "simple command"; else bad "simple command" "rc=$rc out=$out err=$err"; fi
line=$(tail -n1 "$STUB_SSH_LOG")
if [ "$line" = "call [-T] [-o] [BatchMode=yes] [-o] [ConnectTimeout=10] [--] [tester@stub-bash] [bash -s]" ]; then
  ok "ssh argv is -T, BatchMode, ConnectTimeout, target and exactly 'bash -s'"
else
  bad "ssh argv" "$line"
fi

# quoting: every argument must arrive byte for byte
args=('a b' "it's" 'say "hi"' '$HOME' '`id`' '$(id)' '*' 'two
lines' '' '-n' 'back\slash' "$(printf 'tab\there')" ';|&<>' '~' '~root' 'a=~/x' "it's~" '{a,b}' '!x')
want=$(printf '<%s>\n' "${args[@]}")
for h in bashbox fishbox; do
  run "$h" -- printf '<%s>\n' "${args[@]}"
  if [ "$rc" -eq 0 ] && [ "$out" = "$want" ]; then
    ok "quoting survives the $h login shell (spaces, quotes, \$, backticks, globs, newlines)"
  else
    bad "quoting on $h" "rc=$rc err=$err out=$out"
  fi
done

# fish fixture: the dry run shows the only remote string is bash -s
run fishbox --dry-run -- echo "it's" '$HOME'
if [ "$rc" -eq 0 ] && printf '%s\n' "$out" | grep -qF "'bash -s'" && printf '%s\n' "$out" | grep -qF 'login shell fish'; then
  ok "fish host dry run sends only 'bash -s' to the login shell"
else
  bad "fish dry run" "$out"
fi

# exit status propagation
run bashbox -- bash -c 'exit 7'
expect_rc "remote exit 7 is returned" 7
run bashbox -- false
expect_rc "remote false returns 1" 1

# ssh 255 maps to 3
STUB_SSH_EXIT=255 run bashbox -- true
if [ "$rc" -eq 3 ] && printf '%s' "$err" | grep -q 'connection failed' && printf '%s' "$err" | grep -q 'Connection refused'; then
  ok "ssh exit 255 becomes exit 3 with a connection failed message"
else
  bad "255 mapping" "rc=$rc err=$err"
fi

# stdin of the caller is never consumed, even when the remote command reads stdin
n=0
while IFS= read -r x; do
  "$TOOL" bashbox -- cat >/dev/null
  "$TOOL" bashbox -- echo "got $x" >>"$tmp/loop"
  n=$((n + 1))
done <<'LOOP'
one
two
three
LOOP
if [ "$n" -eq 3 ] && [ "$(cat "$tmp/loop")" = "got one
got two
got three" ]; then
  ok "inside while read, stdin is not consumed (3 of 3 iterations)"
else
  bad "stdin in loop" "iterations=$n output=$(cat "$tmp/loop")"
fi

# --script: args, stdin readers in the body, extglob, exit status
cat >"$tmp/args.sh" <<'EOF'
printf 'n=%s\n' "$#"
printf '[%s]\n' "$@"
EOF
run bashbox --script "$tmp/args.sh" 'a b' "it's" '$HOME' '`id`' ''
want=$(printf 'n=5\n[a b]\n[it'"'"'s]\n[$HOME]\n[`id`]\n[]')
if [ "$rc" -eq 0 ] && [ "$out" = "$want" ]; then ok "--script passes quoted args"; else bad "--script args" "rc=$rc out=$out err=$err"; fi
run fishbox --script "$tmp/args.sh" -- --flag
if [ "$rc" -eq 0 ] && [ "$out" = "n=1
[--flag]" ]; then ok "--script on fish host, leading -- dropped"; else bad "--script fish" "rc=$rc out=$out err=$err"; fi
run bashbox --script "$tmp/args.sh"
if [ "$rc" -eq 0 ] && [ "$out" = "n=0
[]" ]; then ok "--script with no args"; else bad "--script no args" "rc=$rc out=$out err=$err"; fi

cat >"$tmp/stdin.sh" <<'EOF'
echo before
cat
read -r x || echo "read got eof"
echo after
EOF
run bashbox --script "$tmp/stdin.sh"
if [ "$rc" -eq 0 ] && [ "$out" = "before
read got eof
after" ]; then ok "commands reading stdin inside --script do not eat the script"; else bad "--script stdin" "out=$out err=$err"; fi

cat >"$tmp/extglob.sh" <<'EOF'
shopt -s extglob
x=abc
case $x in
  @(abc|def)) echo extglob-ok ;;
esac
EOF
run bashbox --script "$tmp/extglob.sh"
if [ "$rc" -eq 0 ] && [ "$out" = "extglob-ok" ]; then ok "--script honors shopt set earlier in the body"; else bad "--script extglob" "rc=$rc out=$out err=$err"; fi

printf 'echo partial\nexit 5\n' >"$tmp/exit5.sh"
run bashbox --script "$tmp/exit5.sh"
if [ "$rc" -eq 5 ] && [ "$out" = "partial" ]; then ok "--script exit status 5 is returned"; else bad "--script exit" "rc=$rc out=$out"; fi

printf 'if then fi\n' >"$tmp/broken.sh"
before=$(calls)
run bashbox --script "$tmp/broken.sh"
after=$(calls)
if [ "$rc" -eq 1 ] && [ "$before" = "$after" ] && printf '%s' "$err" | grep -q 'bash -n failed'; then
  ok "--script syntax error exits 1 without contacting the host"
else
  bad "--script syntax error" "rc=$rc calls $before -> $after err=$err"
fi

# output bounding
run bashbox --max-bytes 100 -- seq 1 2000
saved=$(printf '%s\n' "$err" | sed -n 's/.*full stdout: //p')
shown=$(wc -c <"$tmp/out" | tr -d '[:space:]')
if [ "$rc" -eq 0 ] && [ "$shown" -le 100 ] && [ -n "$saved" ] && [ "$(wc -l <"$saved" | tr -d '[:space:]')" = 2000 ]; then
  ok "--max-bytes cuts stdout and saves the full output ($shown bytes shown)"
else
  bad "--max-bytes" "rc=$rc shown=$shown saved=$saved err=$err"
fi
run bashbox -- seq 1 10000
shown=$(wc -c <"$tmp/out" | tr -d '[:space:]')
if [ "$shown" -le 20000 ] && printf '%s' "$err" | grep -q 'cut to the last'; then ok "default bound is 20000 bytes"; else bad "default bound" "shown=$shown"; fi
run bashbox --tail 3 -- seq 1 2000
if [ "$out" = "1998
1999
2000" ] && printf '%s' "$err" | grep -q 'full stdout'; then ok "--tail 3 prints the last 3 lines"; else bad "--tail" "out=$out"; fi
run bashbox --max-bytes 50 -- bash -c 'seq 1 2000 >&2'
if [ "$rc" -eq 0 ] && [ -z "$out" ] && printf '%s' "$err" | grep -q 'full stderr'; then ok "stderr is bounded separately"; else bad "stderr bound" "out=$out"; fi
rm -rf "${TMPDIR:?}"/*
run bashbox -- seq 1 5
if [ -z "$err" ] && [ -z "$(ls "$TMPDIR")" ]; then ok "small output leaves no temporary files"; else bad "temp cleanup" "err=$err tmp=$(ls "$TMPDIR")"; fi

# --dry-run contacts nothing
before=$(calls)
run bashbox --dry-run --timeout 30 -- printf '%s\n' 'a b'
after=$(calls)
if [ "$rc" -eq 0 ] && [ "$before" = "$after" ] &&
  printf '%s\n' "$out" | grep -qF "invocation: ssh -T -o BatchMode=yes -o ConnectTimeout=10 -- tester@stub-bash 'bash -s' < script" &&
  printf '%s\n' "$out" | grep -qF "printf %s\\\\n a\\ b </dev/null"; then
  ok "--dry-run prints the invocation and the script without running ssh"
else
  bad "--dry-run" "rc=$rc calls $before -> $after out=$out"
fi
run bashbox --dry-run --script "$tmp/broken.sh"
expect_rc "--dry-run still runs bash -n" 1

# --timeout: overall limit and connect timeout cap
start=$SECONDS
run bashbox --timeout 1 -- sleep 5
elapsed=$((SECONDS - start))
if [ "$rc" -eq 124 ] && [ "$elapsed" -le 3 ] && tail -n1 "$STUB_SSH_LOG" | grep -qF '[ConnectTimeout=1]'; then
  ok "--timeout 1 stops a 5s command with exit 124 (${elapsed}s)"
else
  bad "--timeout" "rc=$rc elapsed=$elapsed err=$err"
fi

# --list
run --list
if [ "$rc" -eq 0 ] && printf '%s\n' "$out" | grep -q '^fishbox' && printf '%s\n' "$out" | grep -q '^bashbox'; then ok "--list prints configured hosts"; else bad "--list" "$out"; fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
