#!/usr/bin/env bash
# Tests for bao-run with a stub curl on PATH that records its argv and serves login, kv read,
# revoke and failure responses. Canary values prove the password and token never reach argv,
# output or the environment of the wrapper's own curl calls. No real OpenBao is contacted.
# shellcheck disable=SC2016,SC2034  # check() evaluates its single-quoted assertion later
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tool="$here/bao-run"
command -v jq >/dev/null 2>&1 || { echo "test.sh needs jq" >&2; exit 1; }
bash_bin=$(command -v bash)

tmp=$(mktemp -d)
cleanup() {
  local p
  for p in "$tmp"/*.pid; do
    [ -f "$p" ] && kill -KILL "$(cat "$p")" 2>/dev/null || true
  done
  rm -rf "$tmp"
}
trap cleanup EXIT

unset BAO_OPERATOR_ADDR BAO_OPERATOR_USER BAO_OPERATOR_ACCESS_FILE BAO_OPERATOR_TOKEN_FILE
unset BAO_ADDR BAO_TOKEN VAULT_ADDR VAULT_TOKEN
export HOME="$tmp/home"
mkdir -p "$HOME" "$tmp/bin" "$tmp/stub"

pw_canary="pw-canary-$$-${RANDOM}x"
tok_canary="tok-canary-$$-${RANDOM}y"
kv_canary="kv-canary-$$-${RANDOM}z"
printf '%s' "$pw_canary" > "$tmp/stub/password"
printf '%s' "$tok_canary" > "$tmp/stub/token"
printf '%s' "$kv_canary" > "$tmp/stub/kv"

cat > "$tmp/bin/curl" <<'STUB'
#!/usr/bin/env bash
# Stub curl: records argv, checks credentials arrive on stdin, serves fixture responses.
set -u
d=$STUB_DIR
{ printf 'CALL'; for a in "$@"; do printf ' [%s]' "$a"; done; printf '\n'; } >> "$d/argv"
url=""
for a in "$@"; do case "$a" in http://*|https://*) url=$a ;; esac; done
in=$(cat)
tok=$(cat "$d/token")
case "$url" in
  */v1/auth/userpass/login/*)
    if env | grep -qF -- "$tok"; then echo env-leak >> "$d/leaks"; fi
    if env | grep -qF -- "$(cat "$d/password")"; then echo env-leak >> "$d/leaks"; fi
    case "${STUB_MODE:-ok}" in
      loginfail) echo login-refused >> "$d/calls"; echo "curl: (22) The requested URL returned error: 400" >&2; exit 22 ;;
      notoken) echo login-notoken >> "$d/calls"; printf '{"auth":null}'; exit 0 ;;
    esac
    pw=$(printf '%s' "$in" | jq -r .password)
    if [ "$pw" != "$(cat "$d/password")" ]; then echo login-bad-password >> "$d/calls"; exit 22; fi
    echo "login ${url##*/}" >> "$d/calls"
    rm -f "$d/revoked"
    printf '{"auth":{"client_token":"%s"}}' "$tok" ;;
  */v1/auth/token/revoke-self)
    if env | grep -qF -- "$tok"; then echo env-leak >> "$d/leaks"; fi
    if [ "$in" != "X-Vault-Token: $tok" ]; then echo revoke-bad-token >> "$d/calls"; exit 22; fi
    if [ "${STUB_MODE:-ok}" = revokefail ]; then echo revoke-refused >> "$d/calls"; exit 22; fi
    echo revoke >> "$d/calls"
    : > "$d/revoked" ;;
  */v1/kv/data/*)
    if [ "$in" != "X-Vault-Token: $tok" ] || [ -e "$d/revoked" ]; then echo kv-denied >> "$d/calls"; exit 22; fi
    echo kv-ok >> "$d/calls"
    printf '{"data":{"data":{"value":"%s"}}}' "$(cat "$d/kv")" ;;
  *) echo "unknown $url" >> "$d/calls"; exit 7 ;;
esac
STUB
chmod +x "$tmp/bin/curl"
export STUB_DIR="$tmp/stub" PATH="$tmp/bin:$PATH"

# Access file fixture, generated at runtime; decoys must never be read.
q='`'
{
  printf '## Forgejo\n- Admin %s: %sforgejo-decoy%s\n\n' password "$q" "$q"
  printf '## OpenBao vault (fixture)\n- Admin login: userpass, username %sfixture-user%s\n' "$q" "$q"
  printf -- '- Admin %s: %s%s%s\n' password "$q" "$pw_canary" "$q"
  printf -- '- Root token (BREAK GLASS ONLY): %sroot-decoy%s\n\n' "$q" "$q"
  printf '## Other admin surfaces\n- Admin %s: %sother-decoy%s\n' password "$q" "$q"
} > "$HOME/access.md"
printf '## OpenBao vault\n- Admin login: userpass\n' > "$tmp/no-password.md"

cat > "$tmp/config.json" <<'EOF'
{"openbao": {"addr": "http://stub.invalid:8200/", "user": "fixture-user", "access_file": "~/access.md"}}
EOF
export AGENT_DRY_CONFIG="$tmp/config.json"

pass=0
fail=0
check() {
  if eval "$2"; then pass=$((pass + 1)); else fail=$((fail + 1)); echo "FAIL: $1" >&2; fi
}
reset_calls() { : > "$tmp/stub/calls"; rm -f "$tmp/stub/revoked"; }
calls() { tr '\n' ' ' < "$tmp/stub/calls" | sed 's/ $//'; }
# run <args...>: run bao-run, capture stdout in $out, stderr in $err, status in $rc; keep a
# copy of every output for the final canary scan.
run() {
  set +e
  "$tool" "$@" > "$tmp/o" 2> "$tmp/e"
  rc=$?
  set -e
  out=$(cat "$tmp/o"); err=$(cat "$tmp/e")
  cat "$tmp/o" "$tmp/e" >> "$tmp/all-output"
}
wait_for() { # wait_for <file>: up to 5 s
  local i=0
  while [ ! -s "$1" ] && [ "$i" -lt 100 ]; do sleep 0.05; i=$((i + 1)); done
  [ -s "$1" ]
}
gone() { # gone <pid>: true once the process has exited (up to 5 s)
  local i=0
  while kill -0 "$1" 2>/dev/null && [ "$i" -lt 100 ]; do sleep 0.05; i=$((i + 1)); done
  ! kill -0 "$1" 2>/dev/null
}

# --help contract
run --help
check "--help exits 0" '[ "$rc" -eq 0 ]'
check "--help starts with Usage: bao-run" 'case "$out" in "Usage: bao-run "*) true ;; *) false ;; esac'
check "--help documents 64, 65 and 66" 'printf "%s" "$out" | grep -q "^  64 " && printf "%s" "$out" | grep -q "^  65 " && printf "%s" "$out" | grep -q "^  66 "'
run -h
check "-h exits 0" '[ "$rc" -eq 0 ]'

# Motivating case: login from config, token only in the child environment, revoke on exit.
reset_calls
run -- bash -c 'printf "%s|%s|%s" "$BAO_ADDR" "${#BAO_TOKEN}" "$([ "$BAO_TOKEN" = "$VAULT_TOKEN" ] && [ "$BAO_ADDR" = "$VAULT_ADDR" ] && echo same)"'
check "success exits 0" '[ "$rc" -eq 0 ]'
check "child sees addr (trailing slash removed), token length and VAULT_* copies" '[ "$out" = "http://stub.invalid:8200|${#tok_canary}|same" ]'
check "login as configured user, then revoke once" '[ "$(calls)" = "login fixture-user revoke" ]'

# The child reads a kv secret with the token on stdin (header from stdin, not argv).
reset_calls
run -- bash -c 'printf "X-Vault-Token: %s\n" "$BAO_TOKEN" | curl -q -fsS -H @- "$BAO_ADDR/v1/kv/data/app/fixture" | jq -r .data.data.value | wc -c | tr -d " "'
check "kv read through the child works" '[ "$rc" -eq 0 ] && [ "$out" -gt 1 ] && [ "$(calls)" = "login fixture-user kv-ok revoke" ]'

# stdin reaches the child (callers pipe payloads through the wrapper).
reset_calls
set +e
out=$(printf 'payload-123' | "$tool" -- bash -c 'cat' 2>>"$tmp/all-output"); rc=$?
set -e
printf '%s\n' "$out" >> "$tmp/all-output"
check "stdin passes through to the child" '[ "$rc" -eq 0 ] && [ "$out" = "payload-123" ]'

# Child failure: status propagates, token still revoked.
reset_calls
run -- bash -c 'exit 7'
check "child status 7 propagates" '[ "$rc" -eq 7 ]'
check "revoked after child failure" '[ "$(calls)" = "login fixture-user revoke" ]'
reset_calls
run -- false
check "child status 1 propagates" '[ "$rc" -eq 1 ] && [ "$(calls)" = "login fixture-user revoke" ]'
reset_calls
run -- "$tmp/does-not-exist"
check "missing command exits 127 and revokes once" '[ "$rc" -eq 127 ] && [ "$(calls)" = "login fixture-user revoke" ]'

# Environment overrides config.
reset_calls
BAO_OPERATOR_ADDR=http://env.invalid:1 BAO_OPERATOR_USER=env-user run -- bash -c 'printf "%s" "$BAO_ADDR"'
check "environment wins over config" '[ "$out" = "http://env.invalid:1" ] && [ "$(calls)" = "login env-user revoke" ]'
reset_calls
BAO_OPERATOR_ACCESS_FILE="$tmp/no-password.md" run -- true
check "env access file wins; password line missing exits 65" '[ "$rc" -eq 65 ] && [ -z "$(calls)" ]'

# Delegated token: no login, no revoke.
printf 'delegated-fixture\n' > "$tmp/reader.token"
reset_calls
BAO_OPERATOR_TOKEN_FILE="$tmp/reader.token" run -- bash -c '[ "$BAO_TOKEN" = delegated-fixture ] && [ "$VAULT_TOKEN" = delegated-fixture ]'
check "delegated token reaches the child" '[ "$rc" -eq 0 ]'
check "delegated token is not revoked and no login happens" '[ -z "$(calls)" ]'
BAO_OPERATOR_TOKEN_FILE="$tmp/reader.token" run -- false
check "delegated: child status propagates" '[ "$rc" -eq 1 ]'
: > "$tmp/empty.token"
BAO_OPERATOR_TOKEN_FILE="$tmp/empty.token" run -- true
check "delegated: empty token file exits 65" '[ "$rc" -eq 65 ]'
BAO_OPERATOR_TOKEN_FILE="$tmp/missing.token" run -- true
check "delegated: missing token file exits 65" '[ "$rc" -eq 65 ]'

# Dry run: plan only, nothing contacted or run.
reset_calls
run --dry-run -- touch "$tmp/should-not-exist"
check "dry run exits 0" '[ "$rc" -eq 0 ]'
check "dry run contacts nothing and runs nothing" '[ -z "$(calls)" ] && [ ! -e "$tmp/should-not-exist" ]'
check "dry run prints endpoint, login and revoke plan" 'printf "%s" "$out" | grep -q "endpoint: http://stub.invalid:8200" && printf "%s" "$out" | grep -q "password line found" && printf "%s" "$out" | grep -q "revoke"'
BAO_OPERATOR_ACCESS_FILE="$tmp/nope.md" run --dry-run -- true
check "dry run reports a missing access file with 65" '[ "$rc" -eq 65 ]'
BAO_OPERATOR_TOKEN_FILE="$tmp/reader.token" run --dry-run -- true
check "dry run with a delegated token" '[ "$rc" -eq 0 ] && printf "%s" "$out" | grep -q "delegated from" && [ -z "$(calls)" ]'

# Exit 64: usage.
run
check "no command exits 64" '[ "$rc" -eq 64 ]'
run --
check "only -- exits 64" '[ "$rc" -eq 64 ]'
run --bogus true
check "unknown option exits 64" '[ "$rc" -eq 64 ]'

# Exit 65: access file and password line.
BAO_OPERATOR_ACCESS_FILE=/nonexistent run -- true
check "missing access file exits 65" '[ "$rc" -eq 65 ]'

# Exit 66: login failure; the child never runs and nothing is revoked.
reset_calls
STUB_MODE=loginfail run -- touch "$tmp/ran-after-failed-login"
check "login failure exits 66" '[ "$rc" -eq 66 ] && [ ! -e "$tmp/ran-after-failed-login" ]'
check "login failure: no revoke" '[ "$(calls)" = "login-refused" ]'
reset_calls
STUB_MODE=notoken run -- true
check "login without a token exits 66" '[ "$rc" -eq 66 ] && [ "$(calls)" = "login-notoken" ]'

# Revoke failure is reported, the child status is kept.
reset_calls
STUB_MODE=revokefail run -- true
check "revoke failure keeps the child status" '[ "$rc" -eq 0 ] && printf "%s" "$err" | grep -q "revoke failed"'

# Exit 3: missing or unusable config, invalid settings, missing dependencies.
AGENT_DRY_CONFIG="$tmp/absent.json" run -- true
check "missing config exits 3" '[ "$rc" -eq 3 ] && printf "%s" "$err" | grep -q "address not set"'
printf '{"openbao": {"addr": "http://stub.invalid:8200"}}' > "$tmp/partial.json"
AGENT_DRY_CONFIG="$tmp/partial.json" run -- true
check "missing openbao.user exits 3" '[ "$rc" -eq 3 ] && printf "%s" "$err" | grep -q "user not set"'
printf '{"openbao": {"addr": "http://stub.invalid:8200", "user": "u"}}' > "$tmp/partial2.json"
AGENT_DRY_CONFIG="$tmp/partial2.json" run -- true
check "missing openbao.access_file exits 3" '[ "$rc" -eq 3 ] && printf "%s" "$err" | grep -q "access file not set"'
printf '{not json' > "$tmp/bad.json"
AGENT_DRY_CONFIG="$tmp/bad.json" run -- true
check "invalid config JSON exits 3" '[ "$rc" -eq 3 ]'
printf '{"openbao": {"addr": 8200}}' > "$tmp/badtype.json"
AGENT_DRY_CONFIG="$tmp/badtype.json" run -- true
check "non-string setting exits 3" '[ "$rc" -eq 3 ]'
BAO_OPERATOR_ADDR=ftp://stub.invalid run -- true
check "non-http address exits 3" '[ "$rc" -eq 3 ]'
BAO_OPERATOR_USER='a/b' run -- true
check "user with a slash exits 3" '[ "$rc" -eq 3 ]'
mkdir -p "$tmp/nocurl" "$tmp/nojq"
for t in bash jq awk cat env; do ln -s "$(command -v "$t")" "$tmp/nocurl/$t"; done
for t in bash awk cat env curl; do ln -s "$(command -v "$t")" "$tmp/nojq/$t"; done
set +e
PATH="$tmp/nocurl" "$bash_bin" "$tool" -- true 2>"$tmp/e"; rc=$?
set -e
check "missing curl exits 3" '[ "$rc" -eq 3 ] && grep -q "missing dependency: curl" "$tmp/e"'
set +e
PATH="$tmp/nojq" "$bash_bin" "$tool" -- true 2>"$tmp/e"; rc=$?
set -e
check "missing jq exits 3" '[ "$rc" -eq 3 ] && grep -q "missing dependency: jq" "$tmp/e"'

# SIGTERM: forwarded to the child, whose cleanup can still use the token; then revoke, exit 143.
reset_calls
rm -f "$tmp/child.pid"
"$tool" -- bash -c '
  trap "printf \"X-Vault-Token: %s\n\" \"\$BAO_TOKEN\" | curl -q -fsS -H @- \"\$BAO_ADDR/v1/kv/data/app/cleanup\" >/dev/null; exit 0" TERM
  echo $$ > "$1"
  while :; do sleep 0.05; done' _ "$tmp/child.pid" >> "$tmp/all-output" 2>&1 &
w=$!
wait_for "$tmp/child.pid" || true
kill -TERM "$w"
set +e; wait "$w"; rc=$?; set -e
check "TERM exits 143" '[ "$rc" -eq 143 ]'
check "TERM: child cleanup used the token before revoke" '[ "$(calls)" = "login fixture-user kv-ok revoke" ]'
check "TERM: child stopped" 'gone "$(cat "$tmp/child.pid")"'

# SIGHUP: same path, exit 129.
reset_calls
rm -f "$tmp/child.pid"
"$tool" -- bash -c 'echo $$ > "$1"; while :; do sleep 0.05; done' _ "$tmp/child.pid" >> "$tmp/all-output" 2>&1 &
w=$!
wait_for "$tmp/child.pid" || true
kill -HUP "$w"
set +e; wait "$w"; rc=$?; set -e
check "HUP exits 129 and revokes" '[ "$rc" -eq 129 ] && [ "$(calls)" = "login fixture-user revoke" ]'
check "HUP: child stopped" 'gone "$(cat "$tmp/child.pid")"'

# A child that ignores TERM is killed by a second TERM; the token is still revoked.
reset_calls
rm -f "$tmp/child.pid"
"$tool" -- bash -c 'trap "" TERM; echo $$ > "$1"; while :; do sleep 0.05; done' _ "$tmp/child.pid" >> "$tmp/all-output" 2>&1 &
w=$!
wait_for "$tmp/child.pid" || true
kill -TERM "$w"
sleep 0.3
check "TERM-ignoring child keeps the wrapper waiting" 'kill -0 "$w" 2>/dev/null'
kill -TERM "$w"
set +e; wait "$w"; rc=$?; set -e
check "second TERM kills the child; exit 143 and revoke" '[ "$rc" -eq 143 ] && [ "$(calls)" = "login fixture-user revoke" ]'
check "second TERM: child stopped" 'gone "$(cat "$tmp/child.pid")"'

# Terminal INT (sent to the whole process group). With Bash 4.4 or newer a child that handles
# INT and keeps running keeps the wrapper waiting, as with a foreground command, and its later
# status is returned. Older Bash cannot pass INT to the child, so the wrapper forwards TERM.
legacy=$(bash -c 'if [ "${BASH_VERSINFO[0]}" -lt 4 ] || { [ "${BASH_VERSINFO[0]}" -eq 4 ] && [ "${BASH_VERSINFO[1]}" -lt 4 ]; }; then echo 1; else echo 0; fi')
reset_calls
rm -f "$tmp/child.pid" "$tmp/child.int" "$tmp/release"
set -m
"$tool" -- bash -c '
  trap "echo int > \"\$2\"" INT
  echo $$ > "$1"
  while [ ! -e "$3" ]; do sleep 0.05; done
  exit 5' _ "$tmp/child.pid" "$tmp/child.int" "$tmp/release" >> "$tmp/all-output" 2>&1 &
w=$!
set +m
wait_for "$tmp/child.pid" || true
kill -INT -- "-$w"
if [ "$legacy" -eq 0 ]; then
  wait_for "$tmp/child.int" || true
  check "INT reaches the child from the process group" '[ -s "$tmp/child.int" ]'
  check "INT-handling child keeps the wrapper running" 'kill -0 "$w" 2>/dev/null'
  : > "$tmp/release"
  set +e; wait "$w"; rc=$?; set -e
  check "after INT the child status is returned and the token revoked" '[ "$rc" -eq 5 ] && [ "$(calls)" = "login fixture-user revoke" ]'
else
  set +e; wait "$w"; rc=$?; set -e
  check "legacy Bash: INT forwards TERM, exits 130 and revokes" '[ "$rc" -eq 130 ] && [ "$(calls)" = "login fixture-user revoke" ]'
  check "legacy Bash: child stopped" 'gone "$(cat "$tmp/child.pid")"'
fi

# Signal before the command starts (during login): exit 143 and the minted token is revoked.
reset_calls
real_jq=$(command -v jq)
cat > "$tmp/bin/jq" <<'EOF'
#!/usr/bin/env bash
# jq shim: the login response parse also signals bao-run, then behaves like jq.
case "$*" in *client_token*) kill -TERM "$(ps -o ppid= -p "$PPID" | tr -d ' ')" 2>/dev/null || true ;; esac
exec "$REAL_JQ" "$@"
EOF
chmod +x "$tmp/bin/jq"
REAL_JQ="$real_jq" run -- touch "$tmp/ran-after-term"
rm -f "$tmp/bin/jq"
check "TERM during login exits 143 without running the command" '[ "$rc" -eq 143 ] && [ ! -e "$tmp/ran-after-term" ]'
check "TERM during login still revokes the minted token" '[ "$(calls)" = "login fixture-user revoke" ]'

# A command that is not executable exits 126 and still revokes.
printf 'echo no\n' > "$tmp/not-executable"
reset_calls
run -- "$tmp/not-executable"
check "non-executable command exits 126 and revokes" '[ "$rc" -eq 126 ] && [ "$(calls)" = "login fixture-user revoke" ]'

# Canaries: the password and token never reach argv, output or the wrapper curl environment.
check "decoy passwords never sent" '! grep -q login-bad-password "$tmp/stub/calls"'
check "password never in recorded argv" '! grep -qF -- "$pw_canary" "$tmp/stub/argv"'
check "token never in recorded argv" '! grep -qF -- "$tok_canary" "$tmp/stub/argv"'
check "password never in output" '! grep -qF -- "$pw_canary" "$tmp/all-output"'
check "token never in output" '! grep -qF -- "$tok_canary" "$tmp/all-output"'
check "kv secret never in output" '! grep -qF -- "$kv_canary" "$tmp/all-output"'
check "argv log is not empty (stub was used)" '[ -s "$tmp/stub/argv" ]'
check "credentials never in the wrapper curl environment" '[ ! -e "$tmp/stub/leaks" ]'

echo "bao-run tests: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
