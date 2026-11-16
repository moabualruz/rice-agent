#!/usr/bin/env bash
# deploy.sh: release a tag of rice-agent to this machine and every configured host, then verify.
# Run `scripts/deploy.sh --help` for usage. Bash 3.2 compatible.
set -euo pipefail

PROG="deploy"
GH_REPO="moabualruz/rice-agent"
LDVAR="github.com/moabualruz/rice-agent/internal/commands.Version"
# shellcheck disable=SC2088 # a literal ~ for the remote host; the clone script expands it there
DEFAULT_CLONE_ROOT='~/workspace/agent-dry'
CLONE_TIMEOUT=1200
VERIFY_TIMEOUT=120

usage() {
  cat <<'EOF'
Usage: scripts/deploy.sh <tag> [--host <name>]... [--dry-run] [--skip-ci-check]
       just deploy <tag> [options]

Release <tag> of rice-agent to this machine and to every host in the rice-agent config that sets
hosts.<name>.install, then verify each one.

  1. check   <tag> exists locally and on origin at the same commit, and the latest hosted CI run
             for that commit (pr-flow runs <sha> --repo moabualruz/rice-agent --limit 1 --json)
             concluded success
  2. build   git archive of the tag's commit into a temporary root (never the working tree), a
             VERSION file, and env -u GOFLAGS CGO_ENABLED=0 go build -o <tmp>/bin/rice-agent with
             the version ldflag
  3. local   copy the binary to <repo>/bin/rice-agent and run rice-agent install (the root stays
             this clone)
  4. remote  by hosts.<name>.install:
               bundle  AGENT_DRY_ROOT=<tmp> <tmp>/bin/rice-agent install --remote <name>
                       (for hosts without Go or GitHub access)
               clone   host-run <name> --script: in the clone at hosts.<name>.root (default
                       ~/workspace/agent-dry) refuse uncommitted changes, git fetch --tags,
                       fast-forward main to the tag's commit, or check the tag out detached
                       when main cannot fast-forward (reported), build bin/rice-agent with the
                       same ldflags and run bin/rice-agent install
             hosts without install are skipped with a note
  5. verify  on every host rice-agent version equals <tag> and rice-agent doctor ends healthy;
             one summary line per host. A failed host does not stop the others.

Options:
  --host <name>     deploy only to this host; repeatable; "local" is this machine
  --dry-run         run the read-only checks of step 1, then print every later step and command;
                    nothing is built or written and no configured host is contacted
  --skip-ci-check   do not require a successful CI run for the tag's commit
  -h, --help        show this help

Config: $AGENT_DRY_CONFIG, else ~/.config/agent-dry/config.json: hosts.<name>.install (bundle or
clone) and hosts.<name>.root (clone directory); host-run reads hosts.<name>.ssh.
Needs git, jq, tar and go; pr-flow unless --skip-ci-check; host-run for remote hosts.

Exit codes:
  0  every selected host deployed and verified (skipped hosts included), or the dry-run plan
     was printed
  1  refused or failed: tag missing or moved, CI not successful, build failed, or a host failed
  2  usage error, including a --host that is not configured
  3  environment: missing command, unreadable config, invalid install value, origin or GitHub
     unreachable

Example:
  scripts/deploy.sh v0.2.3 --dry-run && scripts/deploy.sh v0.2.3
EOF
}

say() { printf '%s: %s\n' "$PROG" "$*"; }
warn() { printf '%s: %s\n' "$PROG" "$*" >&2; }
die() {
  printf '%s: %s\n' "$PROG" "$1" >&2
  exit "${2:-2}"
}
refuse() { die "refused: $1" 1; }

need() {
  local c
  for c in "$@"; do
    command -v "$c" >/dev/null 2>&1 || die "missing command: $c" 3
  done
}

# redact strips URL credentials and GitHub tokens from captured stderr and keeps 5 lines.
redact() {
  sed -E 's#(://)[^/@[:space:]]+@#\1<redacted>@#g; s#(gh[pousr]_|github_pat_)[A-Za-z0-9_]+#<redacted>#g' | head -n 5
}

# qword prints one word as the shell would need it, single-quoted only when necessary.
qword() {
  case "$1" in
    '' | *[!A-Za-z0-9_./:=@,+%^{}-]*) printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")" ;;
    *) printf '%s' "$1" ;;
  esac
}

quote_words() {
  local w out=""
  for w in "$@"; do
    out="${out}${out:+ }$(qword "$w")"
  done
  printf '%s' "$out"
}

show() { printf '  + %s\n' "$(quote_words "$@")"; }

# run_logged LOG CMD...: run CMD with stdin from /dev/null, its output indented and saved to LOG.
run_logged() {
  local log=$1 rc=0
  shift
  "$@" </dev/null 2>&1 | tee "$log" | sed 's/^/    /' || rc=$?
  return "$rc"
}

valid_name() {
  case "$1" in
    '' | -* | *[!A-Za-z0-9._-]*) return 1 ;;
  esac
  return 0
}

# ---------- clone host script ----------
# deploy writes these functions to a file with declare -f and runs it with host-run --script.

# shellcheck disable=SC2329 # written out with declare -f
clone_fail() {
  printf 'deploy-clone: %s\n' "$1" >&2
  exit "$2"
}

# shellcheck disable=SC2329 # written out with declare -f
clone_redact() {
  sed -E 's#(://)[^/@[:space:]]+@#\1<redacted>@#g; s#(gh[pousr]_|github_pat_)[A-Za-z0-9_]+#<redacted>#g' | head -n 5
}

# clone_main <tag> <commit> <clone dir, leading ~ allowed> <version ldflag variable>
# Exit: 0 installed, 1 refused or failed, 3 environment.
# shellcheck disable=SC2329 # written out with declare -f
clone_main() {
  set -euo pipefail
  local tag=$1 sha=$2 dir=$3 ldvar=$4 dirty err cur old how got rc
  # shellcheck disable=SC2088 # matching a literal ~ from the config
  case "$dir" in
    '~') dir=$HOME ;;
    '~/'*) dir="$HOME/${dir:2}" ;;
  esac
  command -v git >/dev/null 2>&1 || clone_fail "git not found on PATH" 3
  command -v go >/dev/null 2>&1 || clone_fail "go not found on PATH" 3
  [ -d "$dir" ] || clone_fail "clone $dir does not exist" 3
  cd "$dir" || clone_fail "cannot enter $dir" 3
  git rev-parse --show-toplevel >/dev/null 2>&1 || clone_fail "$dir is not a git clone" 3
  dirty=$(git status --porcelain) || clone_fail "git status failed in $dir" 3
  if [ -n "$dirty" ]; then
    printf '%s\n' "$dirty" | head -n 20 >&2
    clone_fail "refused: $dir has uncommitted changes; nothing was checked out" 1
  fi
  export GIT_TERMINAL_PROMPT=0
  if ! err=$(git fetch --quiet --tags origin 2>&1); then
    printf '%s\n' "$err" | clone_redact >&2
    clone_fail "git fetch --tags origin failed in $dir" 3
  fi
  got=$(git rev-parse -q --verify "refs/tags/$tag^{commit}") || clone_fail "tag $tag not found in $dir after fetch" 1
  [ "$got" = "$sha" ] || clone_fail "tag $tag is $got in $dir, but the release is $sha" 1
  cur=$(git symbolic-ref -q --short HEAD) || cur=""
  if [ -n "$cur" ] && [ "$cur" != main ]; then
    clone_fail "refused: $dir is on branch $cur; deploy moves only main or a detached HEAD" 1
  fi
  if git show-ref -q --verify refs/heads/main && git merge-base --is-ancestor refs/heads/main "$sha"; then
    old=$(git rev-parse refs/heads/main)
    if [ "$cur" != main ]; then
      git checkout -q main || clone_fail "git checkout main failed in $dir" 1
    fi
    if [ "$old" = "$sha" ]; then
      how="main already at $tag"
    else
      git merge -q --ff-only "$sha" || clone_fail "git merge --ff-only $sha failed in $dir" 1
      how="main fast-forwarded ${old:0:7}..${sha:0:7} to $tag"
    fi
  else
    git checkout -q --detach "refs/tags/$tag" || clone_fail "git checkout --detach $tag failed in $dir" 1
    how="detached at $tag (main cannot fast-forward to it)"
  fi
  printf 'deploy-clone: checkout: %s\n' "$how"
  mkdir -p bin
  env -u GOFLAGS CGO_ENABLED=0 go build -o bin/rice-agent -ldflags "-X $ldvar=$tag" ./cmd/rice-agent ||
    clone_fail "go build failed in $dir" 1
  got=$(bin/rice-agent version) || clone_fail "bin/rice-agent version failed in $dir" 1
  [ "$got" = "$tag" ] || clone_fail "built bin/rice-agent reports $got, want $tag" 1
  rc=0
  AGENT_DRY_ROOT="$dir" bin/rice-agent install || rc=$?
  [ "$rc" -eq 0 ] || clone_fail "bin/rice-agent install exited $rc in $dir" 1
}

# ---------- arguments ----------

tag=""
dry_run=0
skip_ci=0
sel=()

if [ $# -eq 0 ]; then
  usage >&2
  exit 2
fi

add_sel() {
  local s
  valid_name "$1" || die "invalid host name '$1'"
  for s in ${sel[@]+"${sel[@]}"}; do
    [ "$s" != "$1" ] || return 0
  done
  sel+=("$1")
}

while [ $# -gt 0 ]; do
  case "$1" in
    -h | --help)
      usage
      exit 0
      ;;
    --dry-run)
      dry_run=1
      shift
      ;;
    --skip-ci-check)
      skip_ci=1
      shift
      ;;
    --host)
      [ $# -ge 2 ] || die "--host needs a name"
      add_sel "$2"
      shift 2
      ;;
    --host=*)
      add_sel "${1#--host=}"
      shift
      ;;
    -*) die "unknown option: $1 (see --help)" ;;
    *)
      [ -z "$tag" ] || die "unexpected argument '$1'; give one tag"
      tag=$1
      shift
      ;;
  esac
done

[ -n "$tag" ] || die "missing <tag> (see --help)"
[[ $tag =~ ^[A-Za-z0-9][A-Za-z0-9._+-]*$ ]] || die "invalid tag '$tag'"
[ "$tag" != current ] || die "invalid tag '$tag'"

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)

# ---------- hosts ----------

need git jq tar go
[ "$skip_ci" -eq 1 ] || need pr-flow

cfg=${AGENT_DRY_CONFIG:-$HOME/.config/rice-agent/config.json}
c_name=()
c_mode=()
c_root=()
if [ -f "$cfg" ]; then
  # Three lines per host, each prefixed with "=" so command substitution keeps empty fields.
  rows=$(jq -r '(.hosts // {}) | to_entries[]
    | [.key, ((.value.install // "") | tostring), ((.value.root // "") | tostring)]
    | if (map(test("[\n\r]")) | any) then error("newline in hosts.\(.[0])") else .[] | "=" + . end' "$cfg") ||
    die "cannot read hosts from $cfg" 3
  if [ -n "$rows" ]; then
    while IFS= read -r n && IFS= read -r m && IFS= read -r r; do
      c_name+=("${n#=}")
      c_mode+=("${m#=}")
      c_root+=("${r#=}")
    done <<EOF
$rows
EOF
  fi
fi

# host_index NAME prints the config index of NAME.
host_index() {
  local i=0
  while [ "$i" -lt "${#c_name[@]}" ]; do
    if [ "${c_name[$i]}" = "$1" ]; then
      printf '%s' "$i"
      return 0
    fi
    i=$((i + 1))
  done
  return 1
}

if host_index local >/dev/null; then
  die "hosts.local in $cfg: the name local is reserved for this machine" 3
fi

targets=()
if [ "${#sel[@]}" -eq 0 ]; then
  targets=(local ${c_name[@]+"${c_name[@]}"})
else
  for s in "${sel[@]}"; do
    if [ "$s" != local ] && ! host_index "$s" >/dev/null; then
      known="local${c_name[*]+ ${c_name[*]}}"
      die "unknown host '$s' (configured: ${known// /, }; config $cfg)"
    fi
  done
  targets=("${sel[@]}")
fi

t_mode=()
t_root=()
remote=0
for t in "${targets[@]}"; do
  if [ "$t" = local ]; then
    t_mode+=(local)
    t_root+=("$repo")
    continue
  fi
  valid_name "$t" || die "hosts.$t in $cfg: invalid host name" 3
  i=$(host_index "$t")
  m=${c_mode[$i]}
  case "$m" in
    '' | bundle | clone) ;;
    *) die "hosts.$t.install in $cfg must be bundle or clone, got '$m'" 3 ;;
  esac
  r=${c_root[$i]}
  [ -n "$r" ] || r=$DEFAULT_CLONE_ROOT
  t_mode+=("${m:-skip}")
  t_root+=("$r")
  [ -z "$m" ] || remote=1
done
[ "$remote" -eq 0 ] || need host-run

# ---------- 1. check ----------

if [ "$dry_run" -eq 1 ]; then
  say "dry run: read-only checks run; nothing is built or written and no configured host is contacted"
fi
say "check: release $tag from $repo"
show git -C "$repo" rev-parse -q --verify "refs/tags/$tag^{commit}"
sha=$(git -C "$repo" rev-parse -q --verify "refs/tags/$tag^{commit}") || refuse "tag $tag not found locally (git fetch --tags)"
short=${sha:0:12}
say "check: tag $tag is commit $short locally"

show git -C "$repo" ls-remote --tags origin "refs/tags/$tag" "refs/tags/$tag^{}"
rc=0
refs=$(GIT_TERMINAL_PROMPT=0 git -C "$repo" ls-remote --tags origin "refs/tags/$tag" "refs/tags/$tag^{}" 2>&1) || rc=$?
if [ "$rc" -ne 0 ]; then
  printf '%s\n' "$refs" | redact >&2
  die "git ls-remote origin failed (exit $rc); origin unreachable or not authorized" 3
fi
peeled=$(printf '%s\n' "$refs" | awk -F '\t' -v r="refs/tags/$tag^{}" '$2 == r { print $1 }')
plain=$(printf '%s\n' "$refs" | awk -F '\t' -v r="refs/tags/$tag" '$2 == r { print $1 }')
origin_sha=${peeled:-$plain}
[ -n "$origin_sha" ] || refuse "tag $tag is not on origin (git push origin $tag)"
[ "$origin_sha" = "$sha" ] || refuse "tag $tag is ${origin_sha:0:12} on origin but $short locally"
say "check: tag $tag is $short on origin"

if [ "$skip_ci" -eq 1 ]; then
  warn "check: CI not checked (--skip-ci-check)"
else
  show pr-flow runs "$sha" --repo "$GH_REPO" --limit 1 --json
  rc=0
  runs=$(pr-flow runs "$sha" --repo "$GH_REPO" --limit 1 --json) || rc=$?
  case "$rc" in
    0) ;;
    1) refuse "no CI run found for $short; wait for CI or pass --skip-ci-check" ;;
    *) die "pr-flow runs failed (exit $rc)" 3 ;;
  esac
  ci_id=$(printf '%s' "$runs" | jq -r '.runs[0].id // ""') || die "cannot parse pr-flow runs output" 3
  ci_wf=$(printf '%s' "$runs" | jq -r '.runs[0].workflow // ""')
  ci_status=$(printf '%s' "$runs" | jq -r '.runs[0].status // ""')
  ci_concl=$(printf '%s' "$runs" | jq -r '.runs[0].conclusion // ""')
  if [ "$ci_concl" != success ]; then
    refuse "latest CI run $ci_id ($ci_wf) for $short is ${ci_status:-unknown}/${ci_concl:--}, not success; wait or pass --skip-ci-check"
  fi
  say "check: CI run $ci_id ($ci_wf) for $short concluded success"
fi

# ---------- 2. build ----------

work=""
# shellcheck disable=SC2329 # invoked by trap
cleanup() {
  if [ -n "$work" ]; then rm -rf "$work"; fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

tmp_base=${TMPDIR:-/tmp}
tmp_base=${tmp_base%/}
if [ "$dry_run" -eq 1 ]; then
  root="$tmp_base/rice-agent-deploy.XXXXXX/root"
else
  work=$(mktemp -d "$tmp_base/rice-agent-deploy.XXXXXX") || die "cannot create a temporary directory" 3
  root="$work/root"
  mkdir -p "$root/bin"
fi
ldflags="-X $LDVAR=$tag"
build_cmd=(env -u GOFLAGS CGO_ENABLED=0 go build -o "$root/bin/rice-agent" -ldflags "$ldflags" ./cmd/rice-agent)

say "build: $tag ($short) from a clean export in $root"
printf '  + %s | %s\n' "$(quote_words git -C "$repo" archive "$sha")" "$(quote_words tar -x -C "$root")"
printf '  + %s > %s\n' "$(quote_words printf '%s\n' "$tag")" "$(qword "$root/VERSION")"
printf '  + (cd %s && %s)\n' "$(qword "$root")" "$(quote_words "${build_cmd[@]}")"
if [ "$dry_run" -eq 0 ]; then
  git -C "$repo" archive "$sha" | tar -x -C "$root" || die "git archive $sha into $root failed" 1
  printf '%s\n' "$tag" >"$root/VERSION"
  rc=0
  (cd "$root" && "${build_cmd[@]}") >"$work/build.log" 2>&1 || rc=$?
  if [ "$rc" -ne 0 ]; then
    tail -n 40 "$work/build.log" | sed 's/^/    /' >&2
    die "build failed: go build exited $rc" 1
  fi
  got=$("$root/bin/rice-agent" version 2>/dev/null) || got=""
  [ "$got" = "$tag" ] || die "build failed: $root/bin/rice-agent reports '$got', want $tag" 1
  say "build: $root/bin/rice-agent reports $tag"
fi

clone_script="${work:-$tmp_base/rice-agent-deploy.XXXXXX}/clone.sh"
if [ "$dry_run" -eq 0 ]; then
  {
    printf '%s\n' '#!/usr/bin/env bash' '# Written by rice-agent scripts/deploy.sh; runs on a clone host.'
    declare -f clone_fail clone_redact clone_main
    printf '%s\n' 'clone_main "$@"'
  } >"$clone_script"
fi

# ---------- 3, 4 and 5. deploy and verify each host ----------

r_status=()
r_detail=()
detail=""

# verify NAME: rice-agent version equals the tag and rice-agent doctor ends healthy.
verify() {
  local name=$1 ver rc out last via=()
  [ "$name" = local ] || via=(host-run "$name" --timeout "$VERIFY_TIMEOUT" --)
  show ${via[@]+"${via[@]}"} rice-agent version
  show ${via[@]+"${via[@]}"} rice-agent doctor
  if [ "$dry_run" -eq 1 ]; then
    detail="would verify version $tag and doctor healthy"
    return 0
  fi
  rc=0
  ver=$(${via[@]+"${via[@]}"} rice-agent version </dev/null 2>"$work/verify.err") || rc=$?
  ver=$(printf '%s' "$ver" | tr -d '[:space:]')
  if [ "$rc" -ne 0 ]; then
    sed 's/^/    /' "$work/verify.err" | head -n 5
    detail="rice-agent version exited $rc"
    return 1
  fi
  if [ "$ver" != "$tag" ]; then
    detail="rice-agent version is '$ver', want $tag"
    return 1
  fi
  rc=0
  out=$(${via[@]+"${via[@]}"} rice-agent doctor </dev/null 2>"$work/verify.err") || rc=$?
  last=$(printf '%s\n' "$out" | tail -n 1)
  if [ "$rc" -ne 0 ] || [ "$last" != "doctor: healthy" ]; then
    printf '%s\n' "$out" | grep '^FAIL' | sed 's/^/    /' || :
    detail="version $tag, but doctor: ${last:-no output} (exit $rc)"
    return 1
  fi
  detail="version $tag, doctor healthy"
}

# deploy_host INDEX: install on the host, then verify it.
deploy_host() {
  local i=$1 name mode croot log rc=0 how=""
  name=${targets[$i]}
  mode=${t_mode[$i]}
  croot=${t_root[$i]}
  log="$work/$name.log"
  case "$mode" in
    skip)
      say "$name: skipped: no hosts.$name.install in $cfg"
      detail="skipped: no hosts.$name.install"
      return 2
      ;;
    local)
      say "$name: install the $tag binary into $repo (root stays this clone)"
      show mkdir -p "$repo/bin"
      show cp "$root/bin/rice-agent" "$repo/bin/.rice-agent.deploy"
      show mv -f "$repo/bin/.rice-agent.deploy" "$repo/bin/rice-agent"
      show env AGENT_DRY_ROOT="$repo" "$repo/bin/rice-agent" install
      if [ "$dry_run" -eq 0 ]; then
        if ! { mkdir -p "$repo/bin" && cp "$root/bin/rice-agent" "$repo/bin/.rice-agent.deploy" &&
          mv -f "$repo/bin/.rice-agent.deploy" "$repo/bin/rice-agent"; }; then
          detail="cannot copy the binary to $repo/bin/rice-agent"
          return 1
        fi
        run_logged "$log" env AGENT_DRY_ROOT="$repo" "$repo/bin/rice-agent" install || rc=$?
        if [ "$rc" -ne 0 ]; then
          detail="rice-agent install exited $rc"
          return 1
        fi
      fi
      ;;
    bundle)
      say "$name: bundle install (rice-agent install --remote $name)"
      show env AGENT_DRY_ROOT="$root" "$root/bin/rice-agent" install --remote "$name"
      if [ "$dry_run" -eq 0 ]; then
        run_logged "$log" env AGENT_DRY_ROOT="$root" "$root/bin/rice-agent" install --remote "$name" || rc=$?
        if [ "$rc" -ne 0 ]; then
          detail="rice-agent install --remote $name exited $rc"
          return 1
        fi
      fi
      ;;
    clone)
      say "$name: clone install in $croot"
      show host-run "$name" --timeout "$CLONE_TIMEOUT" --script "$clone_script" -- "$tag" "$sha" "$croot" "$LDVAR"
      printf '    script: refuse when git status --porcelain is non-empty; git fetch --tags origin;\n'
      printf '    require refs/tags/%s at %s; git merge --ff-only %s on main, or\n' "$tag" "$short" "$short"
      printf '    git checkout --detach refs/tags/%s when main cannot fast-forward;\n' "$tag"
      printf '    env -u GOFLAGS CGO_ENABLED=0 go build -o bin/rice-agent -ldflags %s ./cmd/rice-agent;\n' "$(qword "$ldflags")"
      printf '    AGENT_DRY_ROOT=<clone> bin/rice-agent install\n'
      if [ "$dry_run" -eq 0 ]; then
        run_logged "$log" host-run "$name" --timeout "$CLONE_TIMEOUT" --script "$clone_script" -- \
          "$tag" "$sha" "$croot" "$LDVAR" || rc=$?
        how=$(sed -n 's/^deploy-clone: checkout: //p' "$log" | tail -n 1)
        if [ "$rc" -ne 0 ]; then
          detail=$(sed -n 's/^deploy-clone: //p' "$log" | tail -n 1)
          detail="clone script exited $rc${detail:+: $detail}"
          return 1
        fi
      fi
      ;;
  esac
  say "$name: verify"
  verify "$name" || return 1
  [ -z "$how" ] || detail="$detail; $how"
  return 0
}

failed=0
i=0
while [ "$i" -lt "${#targets[@]}" ]; do
  rc=0
  deploy_host "$i" || rc=$?
  case "$rc" in
    0) r_status+=("$([ "$dry_run" -eq 1 ] && echo planned || echo ok)") ;;
    2) r_status+=(skipped) ;;
    *)
      r_status+=(FAIL)
      failed=1
      warn "${targets[$i]}: FAIL: $detail; continuing with the other hosts"
      ;;
  esac
  r_detail+=("$detail")
  i=$((i + 1))
done

say "summary for $tag ($short)$([ "$dry_run" -eq 1 ] && echo ', dry run')"
i=0
while [ "$i" -lt "${#targets[@]}" ]; do
  printf '  %-8s %-12s %-7s %s\n' "${r_status[$i]}" "${targets[$i]}" "${t_mode[$i]}" "${r_detail[$i]}"
  i=$((i + 1))
done
exit "$failed"
