#!/usr/bin/env bash
# Tests for scripts/deploy.sh. Run: bash scripts/deploy.test.sh (any Bash from 3.2 on).
# Stubs on a fixture PATH stand in for pr-flow, go, host-run and the built rice-agent; git is a
# logging wrapper over the real git, so archive, fetch and the clone script's fast-forward or
# detach run against real fixture repositories. deploy.sh and the clone script run under the Bash
# that runs this file. No network and no real host are used.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
real_git=$(command -v git)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/deploy-test.XXXXXX")
tmp=$(cd "$tmp" && pwd -P)
trap 'rm -rf "$tmp"' EXIT
fails=0

pass() { printf 'ok   %s\n' "$1"; }
fail() {
  printf 'FAIL %s\n' "$1"
  fails=$((fails + 1))
}

# expect_exit DESC WANT CMD...: run CMD with stdout in $tmp/out and stderr in $tmp/err.
expect_exit() {
  local desc="$1" want="$2" got=0
  shift 2
  "$@" >"$tmp/out" 2>"$tmp/err" || got=$?
  if [ "$got" -eq "$want" ]; then
    pass "$desc"
  else
    fail "$desc: exit $got, want $want"
    sed -n '1,12s/^/     /p' "$tmp/err"
    sed -n '1,12s/^/     /p' "$tmp/out"
  fi
}
expect_in() {
  if grep -F -q -e "$3" "$tmp/$2"; then pass "$1"; else fail "$1: $2 lacks '$3'"; fi
}
expect_not_in() {
  if grep -F -q -e "$3" "$tmp/$2"; then fail "$1: $2 has '$3'"; else pass "$1"; fi
}
expect_line() { # DESC FILE REGEX
  if grep -E -q -e "$3" "$tmp/$2"; then pass "$1"; else fail "$1: $2 has no line matching /$3/"; fi
}
expect_no_line() {
  if grep -E -q -e "$3" "$tmp/$2"; then fail "$1: $2 has a line matching /$3/"; else pass "$1"; fi
}
expect_eq() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1: got '$2', want '$3'"; fi
}

# ---------- fixture repositories ----------

export HOME="$tmp/home"
export XDG_CONFIG_HOME="$tmp/home/.config"
export GIT_CONFIG_NOSYSTEM=1
mkdir -p "$HOME/workspace" "$tmp/t" "$tmp/stub" "$tmp/tpl"
g() { "$real_git" -c user.name=deploy-test -c user.email=deploy-test@example.invalid -c advice.detachedHead=false "$@"; }

dev="$tmp/dev"
g init -q --bare "$tmp/origin.git"
g --git-dir="$tmp/origin.git" symbolic-ref HEAD refs/heads/main
g init -q "$dev"
g -C "$dev" symbolic-ref HEAD refs/heads/main
mkdir -p "$dev/cmd/rice-agent" "$dev/registry"
printf 'module example.invalid/rice-agent\n' >"$dev/go.mod"
printf 'package main\n' >"$dev/cmd/rice-agent/main.go"
printf '{}\n' >"$dev/registry/wanted.json"
printf '/bin/\n' >"$dev/.gitignore"
g -C "$dev" add -A
g -C "$dev" commit -q -m A
A=$(g -C "$dev" rev-parse HEAD)
printf 'b\n' >"$dev/b.txt"
g -C "$dev" add -A
g -C "$dev" commit -q -m B
B=$(g -C "$dev" rev-parse HEAD)
g -C "$dev" tag -a v9.9.9 -m v9.9.9
g -C "$dev" tag v9.9.7 "$A"
printf 'c\n' >"$dev/c.txt"
g -C "$dev" add -A
g -C "$dev" commit -q -m C
C=$(g -C "$dev" rev-parse HEAD)
g -C "$dev" remote add origin "$tmp/origin.git"
g -C "$dev" push -q origin main --tags
g -C "$dev" tag v9.9.8 "$A"     # local only
g -C "$dev" tag -f v9.9.7 "$B" >/dev/null # moved locally after the push
# deploy.sh runs from this clone; an untracked file proves the build ignores the working tree.
mkdir -p "$dev/scripts"
cp "$here/deploy.sh" "$dev/scripts/deploy.sh"
printf 'uncommitted\n' >"$dev/WORKTREE_ONLY"

make_clone() { # DIR COMMIT
  rm -rf "$1"
  g clone -q "$tmp/origin.git" "$1"
  g -C "$1" reset -q --hard "$2"
}

cat >"$tmp/config.json" <<EOF
{
  "root": "$dev",
  "openbao": { "addr": "http://bao.invalid:8200", "user": "u", "access_file": "/nonexistent" },
  "hosts": {
    "hz": { "ssh": "t@hz.invalid", "shell": "bash", "install": "bundle" },
    "pc": { "ssh": "t@pc.invalid", "shell": "fish", "install": "clone", "root": "$tmp/clone-pc" },
    "dflt": { "ssh": "t@dflt.invalid", "install": "clone" },
    "bare": { "ssh": "t@bare.invalid" }
  }
}
EOF

# ---------- stubs ----------

stub="$tmp/stub"
cat >"$stub/git" <<'EOF'
#!/usr/bin/env bash
printf 'git %s\n' "$*" >>"$STUB_LOG"
for a in "$@"; do
  if [ "$a" = ls-remote ] && [ -n "${STUB_LSREMOTE_FAIL:-}" ]; then
    echo "fatal: unable to access 'https://x-access-token:ghs_SECRETSECRET1234@github.invalid/o/r/': Could not resolve host" >&2
    exit 128
  fi
done
exec "$STUB_REAL_GIT" "$@"
EOF

cat >"$stub/pr-flow" <<'EOF'
#!/usr/bin/env bash
printf 'pr-flow %s\n' "$*" >>"$STUB_LOG"
if [ "$#" -ne 7 ] || [ "$1" != runs ] || [ "$3 $4 $5 $6 $7" != "--repo moabualruz/agent-dry --limit 1 --json" ]; then
  echo "stub-pr-flow: unexpected arguments: $*" >&2
  exit 99
fi
[ -z "${STUB_PRFLOW_EXIT:-}" ] || exit "$STUB_PRFLOW_EXIT"
if [ -n "${STUB_CI_NONE:-}" ]; then
  printf '{"repo":"moabualruz/agent-dry","ref":"%s","ref_kind":"sha","runs":[]}\n' "$2"
  echo "pr-flow: no workflow runs for sha $2" >&2
  exit 1
fi
printf '{"repo":"moabualruz/agent-dry","ref":"%s","ref_kind":"sha","runs":[{"id":4242,"workflow":"CI","status":"%s","conclusion":"%s","head_sha":"%s"}]}\n' \
  "$2" "${STUB_CI_STATUS:-completed}" "${STUB_CI_CONCLUSION-success}" "$2"
EOF

cat >"$stub/go" <<'EOF'
#!/usr/bin/env bash
printf 'go %s (in %s)\n' "$*" "$PWD" >>"$STUB_LOG"
if [ -n "${GOFLAGS+x}" ]; then echo "stub-go: GOFLAGS is set" >&2; exit 97; fi
if [ "${CGO_ENABLED:-}" != 0 ]; then echo "stub-go: CGO_ENABLED is not 0" >&2; exit 96; fi
if [ -n "${STUB_GO_FAIL:-}" ]; then echo "stub-go: ./cmd/rice-agent: compile error" >&2; exit 1; fi
if [ -e WORKTREE_ONLY ]; then echo "stub-go: building a working tree, not a clean export" >&2; exit 95; fi
if [ ! -f cmd/rice-agent/main.go ]; then echo "stub-go: no ./cmd/rice-agent in $PWD" >&2; exit 94; fi
out="" ld=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift ;;
    -ldflags) ld=$2; shift ;;
  esac
  shift
done
case "$ld" in
  "-X github.com/moabualruz/rice-agent/internal/commands.Version="?*) ;;
  *) echo "stub-go: unexpected -ldflags '$ld'" >&2; exit 93 ;;
esac
mkdir -p "$(dirname "$out")"
sed "s/@VERSION@/${ld##*=}/" "$STUB_TPL/rice-agent" >"$out"
chmod +x "$out"
EOF

# The binary stub go "builds": logs every call with its root and path.
cat >"$tmp/tpl/rice-agent" <<'EOF'
#!/usr/bin/env bash
printf 'rice-agent %s root=%s exe=%s\n' "$*" "${AGENT_DRY_ROOT:-}" "$0" >>"$STUB_LOG"
case "${1:-}" in
  version) echo "@VERSION@" ;;
  install)
    if [ "${2:-}" = --remote ]; then
      [ "$(cat "$AGENT_DRY_ROOT/VERSION")" = "@VERSION@" ] || { echo "fake: VERSION mismatch in root" >&2; exit 3; }
      case " ${STUB_BUNDLE_FAIL:-} " in
        *" $3 "*) echo "rice-agent: install: remote $3 exited 255" >&2; exit 255 ;;
      esac
      echo "rice-agent: install: remote $3 exited 0" >&2
    else
      echo "installed 3 link(s)"
    fi
    ;;
  doctor)
    if [ -n "${STUB_LOCAL_UNHEALTHY:-}" ]; then
      echo "FAIL path: ~/.local/bin is not on PATH"
      echo "doctor: 1 problem(s)"
      exit 1
    fi
    echo "PASS config: ok"
    echo "doctor: healthy"
    ;;
  *) exit 2 ;;
esac
EOF

# rice-agent on PATH is the link install makes into the clone's bin.
cat >"$stub/rice-agent" <<'EOF'
#!/usr/bin/env bash
exec "$STUB_REPO/bin/rice-agent" "$@"
EOF

cat >"$stub/host-run" <<'EOF'
#!/usr/bin/env bash
printf 'host-run %s\n' "$*" >>"$STUB_LOG"
host=$1
shift
timeout="" mode="" file=""
while [ $# -gt 0 ]; do
  case "$1" in
    --timeout) timeout=$2; shift 2 ;;
    --script)
      mode=script file=$2
      shift 2
      [ "${1:-}" != -- ] || shift
      break
      ;;
    --) mode=command; shift; break ;;
    *) echo "stub-host-run: unexpected $1" >&2; exit 2 ;;
  esac
done
[ -n "$timeout" ] || { echo "stub-host-run: no --timeout" >&2; exit 92; }
case " ${STUB_DOWN:-} " in
  *" $host "*) echo "host-run: connection failed: ssh to $host exited 255" >&2; exit 3 ;;
esac
if [ "$mode" = script ]; then
  "$STUB_BASH" -n "$file" || exit 1
  cd "$HOME" || exit 3
  exec "$STUB_BASH" "$file" "$@" </dev/null
fi
case "$*" in
  "rice-agent version")
    case " ${STUB_BADVERSION:-} " in *" $host "*) echo v0.0.1 ;; *) echo "$STUB_TAG" ;; esac
    ;;
  "rice-agent doctor")
    case " ${STUB_UNHEALTHY:-} " in
      *" $host "*) echo "FAIL install: no receipt"; echo "doctor: 1 problem(s)"; exit 1 ;;
    esac
    echo "PASS config: ok"
    echo "doctor: healthy"
    ;;
  *) echo "stub-host-run: unexpected command: $*" >&2; exit 91 ;;
esac
EOF
chmod +x "$stub"/*

export STUB_LOG="$tmp/calls.log" STUB_REAL_GIT="$real_git" STUB_TPL="$tmp/tpl" STUB_REPO="$dev"
export STUB_BASH="$BASH" STUB_TAG=v9.9.9
export AGENT_DRY_CONFIG="$tmp/config.json" TMPDIR="$tmp/t" GOFLAGS=-mod=mod
export PATH="$stub:$PATH"

# deploy ARGS...: a fresh call log, then deploy.sh from the fixture clone under this Bash.
deploy() {
  : >"$STUB_LOG"
  "$BASH" "$dev/scripts/deploy.sh" "$@"
}
log() { cp "$STUB_LOG" "$tmp/log"; }
head_of() { g -C "$1" rev-parse HEAD; }
branch_of() { g -C "$1" symbolic-ref -q --short HEAD || echo detached; }

printf "# deploy.sh under bash %s\n" "$("$BASH" -c "echo \"\$BASH_VERSION\"")"

# ---------- usage ----------

expect_exit "--help exits 0" 0 deploy --help
expect_in "--help starts with Usage" out "Usage: scripts/deploy.sh <tag>"
expect_in "--help documents exit codes" out "Exit codes:"
expect_exit "no arguments is a usage error" 2 deploy
expect_exit "unknown option is a usage error" 2 deploy v9.9.9 --bogus
expect_exit "--host without a name is a usage error" 2 deploy v9.9.9 --host
expect_exit "two tags is a usage error" 2 deploy v9.9.9 v9.9.8
expect_exit "a tag with shell characters is a usage error" 2 deploy 'v1;true'
expect_exit "an invalid --host name is a usage error" 2 deploy v9.9.9 --host 'a b'
expect_exit "an unconfigured --host is a usage error" 2 deploy v9.9.9 --host nowhere --dry-run
expect_in "unknown host is named with the configured ones" err "unknown host 'nowhere' (configured: local, hz, pc, dflt, bare"
log
if [ -s "$tmp/log" ]; then fail "usage errors ran a command"; else pass "usage errors run nothing"; fi

# ---------- environment ----------

jq '.hosts.pc.install = "rsync"' "$tmp/config.json" >"$tmp/bad-install.json"
AGENT_DRY_CONFIG="$tmp/bad-install.json" expect_exit "an invalid install value exits 3" 3 deploy v9.9.9 --dry-run
expect_in "invalid install is named" err "hosts.pc.install"
jq '.hosts.local = {"ssh": "t@x.invalid"}' "$tmp/config.json" >"$tmp/local-host.json"
AGENT_DRY_CONFIG="$tmp/local-host.json" expect_exit "a host named local exits 3" 3 deploy v9.9.9 --dry-run
printf '{ not json' >"$tmp/broken.json"
AGENT_DRY_CONFIG="$tmp/broken.json" expect_exit "an unparsable config exits 3" 3 deploy v9.9.9 --dry-run
AGENT_DRY_CONFIG="$tmp/absent.json" expect_exit "no config deploys only this machine (dry run)" 0 deploy v9.9.9 --dry-run
expect_line "no config: one planned local line" out '^  planned +local +local '

# ---------- tag checks ----------

expect_exit "a tag missing locally is refused" 1 deploy v0.0.0 --dry-run
expect_in "missing tag is named" err "tag v0.0.0 not found locally"
expect_exit "a tag missing on origin is refused" 1 deploy v9.9.8 --dry-run
expect_in "tag not on origin is named" err "tag v9.9.8 is not on origin"
expect_exit "a tag moved since the push is refused" 1 deploy v9.9.7 --dry-run
expect_in "moved tag shows both commits" err "is ${A:0:12} on origin but ${B:0:12} locally"
STUB_LSREMOTE_FAIL=1 expect_exit "an unreachable origin exits 3" 3 deploy v9.9.9 --dry-run
expect_in "origin failure is reported" err "git ls-remote origin failed"
expect_not_in "origin failure output hides the token" err "ghs_SECRETSECRET1234"
expect_in "origin failure output keeps the redacted line" err "<redacted>@github.invalid"

# ---------- CI ----------

STUB_CI_CONCLUSION=failure expect_exit "a failed CI run is refused" 1 deploy v9.9.9
expect_in "CI refusal names the run and the override" err "run 4242 (CI) for ${B:0:12} is completed/failure, not success; wait or pass --skip-ci-check"
log
expect_line "CI was asked about the tag's commit" log "^pr-flow runs $B --repo moabualruz/agent-dry --limit 1 --json$"
expect_no_line "CI refusal builds nothing" log '^go '
expect_no_line "CI refusal contacts no host" log '^host-run '
STUB_CI_STATUS=in_progress STUB_CI_CONCLUSION="" expect_exit "a running CI run is refused" 1 deploy v9.9.9 --dry-run
expect_in "running CI is shown" err "is in_progress/-, not success"
STUB_CI_NONE=1 expect_exit "no CI run is refused" 1 deploy v9.9.9 --dry-run
expect_in "no CI run is named" err "no CI run found for ${B:0:12}"
STUB_PRFLOW_EXIT=3 expect_exit "a pr-flow environment failure exits 3" 3 deploy v9.9.9 --dry-run
STUB_CI_CONCLUSION=failure expect_exit "--skip-ci-check skips CI" 0 deploy v9.9.9 --dry-run --skip-ci-check
log
expect_no_line "--skip-ci-check never calls pr-flow" log '^pr-flow '
expect_in "--skip-ci-check says so" err "CI not checked (--skip-ci-check)"

# ---------- dry run ----------

make_clone "$tmp/clone-pc" "$A"
make_clone "$HOME/workspace/rice-agent" "$C"
expect_exit "dry run exits 0" 0 deploy v9.9.9 --dry-run
expect_in "dry run says nothing is contacted" out "no configured host is contacted"
expect_in "dry run prints the clean export" out "+ git -C $dev archive $B | tar -x -C $tmp/t/rice-agent-deploy.XXXXXX/root"
expect_in "dry run prints the VERSION file" out "+ printf '%s\n' v9.9.9 > $tmp/t/rice-agent-deploy.XXXXXX/root/VERSION"
expect_in "dry run prints the build" out "&& env -u GOFLAGS CGO_ENABLED=0 go build -o $tmp/t/rice-agent-deploy.XXXXXX/root/bin/rice-agent -ldflags '-X github.com/moabualruz/rice-agent/internal/commands.Version=v9.9.9' ./cmd/rice-agent)"
expect_in "dry run prints the local install" out "+ env AGENT_DRY_ROOT=$dev $dev/bin/rice-agent install"
expect_in "dry run prints the bundle install" out "+ env AGENT_DRY_ROOT=$tmp/t/rice-agent-deploy.XXXXXX/root $tmp/t/rice-agent-deploy.XXXXXX/root/bin/rice-agent install --remote hz"
expect_in "dry run prints the clone script call" out "+ host-run pc --timeout 1200 --script $tmp/t/rice-agent-deploy.XXXXXX/clone.sh -- v9.9.9 $B $tmp/clone-pc"
expect_in "dry run keeps the default clone root literal" out "-- v9.9.9 $B '~/workspace/agent-dry' github.com/moabualruz"
expect_in "dry run prints the remote verification" out "+ host-run hz --timeout 120 -- rice-agent doctor"
expect_in "dry run prints the local verification" out "+ rice-agent version"
expect_in "dry run notes the skipped host" out "bare: skipped: no hosts.bare.install"
expect_line "dry run summary: local planned" out '^  planned +local +local '
expect_line "dry run summary: hz planned" out '^  planned +hz +bundle '
expect_line "dry run summary: bare skipped" out '^  skipped +bare +skip '
log
expect_no_line "dry run builds nothing" log '^go '
expect_no_line "dry run contacts no host" log '^host-run '
expect_no_line "dry run runs no rice-agent" log '^rice-agent '
expect_no_line "dry run exports nothing" log ' archive '
if [ ! -e "$dev/bin/rice-agent" ]; then pass "dry run writes no binary"; else fail "dry run wrote $dev/bin/rice-agent"; fi
expect_eq "dry run leaves the clone alone" "$(head_of "$tmp/clone-pc")" "$A"

# ---------- one failing host, the others continue ----------

STUB_BUNDLE_FAIL=hz expect_exit "a failed host exits 1 after the others" 1 deploy v9.9.9
log
expect_line "summary: local ok" out '^  ok +local +local +version v9\.9\.9, doctor healthy$'
expect_line "summary: hz FAIL" out '^  FAIL +hz +bundle +rice-agent install --remote hz exited 255$'
expect_line "summary: pc ok and fast-forwarded" out "^  ok +pc +clone +version v9\\.9\\.9, doctor healthy; main fast-forwarded ${A:0:7}\\.\\.${B:0:7} to v9\\.9\\.9$"
expect_line "summary: dflt ok and detached" out '^  ok +dflt +clone +version v9\.9\.9, doctor healthy; detached at v9\.9\.9 \(main cannot fast-forward to it\)$'
expect_line "summary: bare skipped" out '^  skipped +bare +skip +skipped: no hosts\.bare\.install$'
expect_in "the failure is reported as it happens" err "hz: FAIL: rice-agent install --remote hz exited 255; continuing with the other hosts"
if awk '/install --remote hz/ { h = NR } /^host-run pc --timeout 1200 --script/ { p = NR } END { exit !(h && p && h < p) }' "$tmp/log"; then
  pass "pc is deployed after hz failed"
else
  fail "pc was not deployed after hz failed"
fi
expect_line "the local install runs the copied binary with the clone as root" log "^rice-agent install root=$dev exe=$dev/bin/rice-agent$"
expect_line "the bundle install runs from the temporary root" log "^rice-agent install --remote hz root=$tmp/t/rice-agent-deploy\\.[A-Za-z0-9]+/root exe=$tmp/t/rice-agent-deploy\\.[A-Za-z0-9]+/root/bin/rice-agent$"
expect_line "the build ran in the clean export" log "^go build -o $tmp/t/rice-agent-deploy\\.[A-Za-z0-9]+/root/bin/rice-agent .* \\(in $tmp/t/rice-agent-deploy\\.[A-Za-z0-9]+/root\\)$"
expect_eq "the local binary reports the tag" "$("$dev/bin/rice-agent" version)" v9.9.9
expect_eq "pc main is at the tag" "$(branch_of "$tmp/clone-pc") $(head_of "$tmp/clone-pc")" "main $B"
expect_eq "dflt is detached at the tag" "$(branch_of "$HOME/workspace/rice-agent") $(head_of "$HOME/workspace/rice-agent")" "detached $B"
expect_eq "dflt main is untouched" "$(g -C "$HOME/workspace/rice-agent" rev-parse refs/heads/main)" "$C"
expect_eq "the clone build reports the tag" "$("$tmp/clone-pc/bin/rice-agent" version)" v9.9.9
expect_line "the clone install uses the clone as root" log "^rice-agent install root=$tmp/clone-pc exe=bin/rice-agent$"
leftover=$(find "$tmp/t" -maxdepth 1 -name 'rice-agent-deploy.*' | wc -l | tr -d ' ')
expect_eq "the temporary root is removed" "$leftover" 0

expect_exit "a second deploy to a current clone exits 0" 0 deploy v9.9.9 --host pc
expect_line "a current main is reported" out '^  ok +pc +clone +.*; main already at v9\.9\.9$'

# ---------- dirty clone and other clone refusals ----------

make_clone "$tmp/clone-pc" "$A"
printf 'local edit\n' >"$tmp/clone-pc/notes.txt"
expect_exit "a dirty clone is refused" 1 deploy v9.9.9 --host pc
expect_in "the dirty refusal is explained" out "has uncommitted changes; nothing was checked out"
expect_line "summary: pc FAIL" out '^  FAIL +pc +clone +clone script exited 1: refused: .* has uncommitted changes; nothing was checked out$'
log
expect_no_line "a dirty clone is not fetched" log ' fetch '
expect_no_line "a dirty clone is not built" log "^go .*\\(in $tmp/clone-pc\\)$"
expect_eq "a dirty clone keeps its HEAD" "$(head_of "$tmp/clone-pc")" "$A"

make_clone "$tmp/clone-pc" "$A"
g -C "$tmp/clone-pc" checkout -q -b feature
expect_exit "a clone on another branch is refused" 1 deploy v9.9.9 --host pc
expect_in "the branch refusal is explained" out "is on branch feature"

STUB_DOWN=pc expect_exit "an unreachable clone host fails" 1 deploy v9.9.9 --host pc
expect_line "summary: unreachable pc" out '^  FAIL +pc +clone +clone script exited 3'

# ---------- --host filtering ----------

make_clone "$tmp/clone-pc" "$A"
rm -f "$dev/bin/rice-agent"
expect_exit "--host pc --host local deploys only those" 0 deploy v9.9.9 --host pc --host local
log
expect_line "--host: pc deployed" log '^host-run pc --timeout 1200 --script '
expect_line "--host: local deployed" log "^rice-agent install root=$dev "
expect_no_line "--host: hz not deployed" log 'remote hz|^host-run hz '
expect_no_line "--host: dflt not deployed" log '^host-run dflt '
summary=$(sed -n '/summary for v9.9.9/,$p' "$tmp/out" | sed -n '2,$p' | awk '{ print $2 }' | tr '\n' ' ')
expect_eq "--host: summary in the given order" "$summary" "pc local "

expect_exit "--host local touches no remote host" 0 deploy v9.9.9 --host local
log
expect_no_line "--host local: no host-run" log '^host-run '
expect_no_line "--host local: no bundle" log 'install --remote'
expect_exit "--host bare is skipped with a note" 0 deploy v9.9.9 --host bare --host bare
expect_in "--host bare: skip note" out "bare: skipped: no hosts.bare.install"
expect_eq "--host given twice is deployed once" "$(grep -c '^  skipped' "$tmp/out")" 1

# ---------- verification ----------

STUB_UNHEALTHY=hz expect_exit "an unhealthy doctor fails the host" 1 deploy v9.9.9 --host hz
expect_line "summary: unhealthy hz" out '^  FAIL +hz +bundle +version v9\.9\.9, but doctor: doctor: 1 problem\(s\) \(exit 1\)$'
expect_in "the doctor FAIL lines are shown" out "    FAIL install: no receipt"
STUB_BADVERSION=hz expect_exit "a wrong remote version fails the host" 1 deploy v9.9.9 --host hz
expect_in "the wrong version is named" out "rice-agent version is 'v0.0.1', want v9.9.9"
STUB_LOCAL_UNHEALTHY=1 expect_exit "an unhealthy local doctor fails" 1 deploy v9.9.9 --host local
expect_line "summary: unhealthy local" out '^  FAIL +local +local +version v9\.9\.9, but doctor'

# ---------- build failure ----------

STUB_GO_FAIL=1 expect_exit "a build failure exits 1" 1 deploy v9.9.9
expect_in "the build failure is reported" err "build failed: go build exited 1"
expect_in "the build output tail is shown" err "compile error"
log
expect_no_line "a build failure contacts no host" log '^host-run '
expect_no_line "a build failure installs nothing" log '^rice-agent install'

if [ "$fails" -eq 0 ]; then
  echo "all deploy.sh tests passed"
else
  echo "$fails deploy.sh test(s) failed" >&2
  exit 1
fi
