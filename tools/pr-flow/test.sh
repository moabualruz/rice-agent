#!/usr/bin/env bash
# Tests for pr-flow. A stub gh on PATH serves the JSON fixtures in testdata/ (shapes recorded from
# gh 2.102 pr view, run list and run view output); a stub sleep records the watch backoff.
# Run from the tool directory: bash test.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOOL="${HERE}/pr-flow"
FIX="${HERE}/testdata"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/pr-flow-test.XXXXXX")"
trap 'rm -rf "${TMP}"' EXIT

# Never let a real token reach the stubs or the output.
unset GITHUB_TOKEN GH_TOKEN
FAKE_TOKEN="test-token-not-a-real-credential"
HEAD_SHA="4f2c9a1e7b3d5c8f0a6e2d4b1c9f7a3e5d8b0c2a"
OTHER_SHA="9a8b7c6d5e4f30211203f4e5d6c7b8a9f0e1d2c3"

mkdir -p "${TMP}/bin" "${TMP}/sleepbin" "${TMP}/minbin" "${TMP}/fix"

cat >"${TMP}/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -u
fix="${STUB_FIXTURES:?}"
state="${STUB_STATE:?}"
tok=unset
if [ -n "${GITHUB_TOKEN:-}${GH_TOKEN:-}" ]; then tok=set; fi
printf '%s|token=%s\n' "$*" "${tok}" >>"${state}/calls.log"
case "${STUB_AUTH:-ok}" in
  env-bad)
    if [ "${tok}" = set ]; then
      printf 'HTTP 401: Bad credentials (https://api.github.com/graphql)\nTry authenticating with:  gh auth login\n' >&2
      exit 1
    fi ;;
  integration)
    if [ "${tok}" = set ]; then
      echo 'GraphQL: Resource not accessible by integration (repository.pullRequest)' >&2
      exit 1
    fi ;;
  all-bad)
    echo 'HTTP 401: Bad credentials (https://api.github.com/graphql)' >&2
    exit 1 ;;
esac
if [ -n "${STUB_FAIL:-}" ]; then echo "${STUB_FAIL}" >&2; exit 1; fi
fixture() { case "$1" in /*) cat "$1" ;; *) cat "${fix}/$1" ;; esac; }
case "$1 ${2:-}" in
  "repo view")
    if [ -n "${STUB_NO_REPO:-}" ]; then echo 'no git remotes found' >&2; exit 1; fi
    echo '{"nameWithOwner":"acme/widgets"}' ;;
  "pr view")
    n=$(($(cat "${state}/prview.n" 2>/dev/null || echo 0) + 1))
    echo "${n}" >"${state}/prview.n"
    set -- ${STUB_PR_VIEW:?}
    [ "${n}" -le "$#" ] || n=$#
    shift $((n - 1))
    fixture "$1" ;;
  "pr merge")
    echo merge >>"${state}/merge.log"
    if [ -n "${STUB_MERGE_FAIL:-}" ]; then echo "${STUB_MERGE_FAIL}" >&2; exit 1; fi ;;
  "run list") fixture "${STUB_RUN_LIST:-run-list.json}" ;;
  "run view")
    case " $* " in
      *" --log-failed "*)
        if [ -n "${STUB_LOG_FAIL:-}" ]; then echo "${STUB_LOG_FAIL}" >&2; exit 1; fi
        cat "${state}/../job.log" ;;
      *) fixture "${STUB_RUN_VIEW:-run-view-failed.json}" ;;
    esac ;;
  "api -H")
    case "$*" in
      *commits/4f2c9a1) echo "4f2c9a1e7b3d5c8f0a6e2d4b1c9f7a3e5d8b0c2a" ;;
      *) echo 'gh: Not Found (HTTP 404)' >&2; exit 1 ;;
    esac ;;
  *) echo "stub gh: unhandled: $*" >&2; exit 64 ;;
esac
STUB

cat >"${TMP}/sleepbin/sleep" <<'STUB'
#!/usr/bin/env bash
printf '%s ' "$1" >>"${STUB_STATE:?}/sleep.log"
STUB
chmod +x "${TMP}/bin/gh" "${TMP}/sleepbin/sleep"

# Minimal PATH without gh, for the missing-dependency case.
ln -s "$(command -v bash)" "${TMP}/minbin/bash"
ln -s "$(command -v jq)" "${TMP}/minbin/jq"

# Failed-step log in gh's --log-failed format: job, step, BOM, timestamp, color codes, then
# post-job cleanup lines that must not appear in the tail.
{
  printf 'test\tUNKNOWN STEP\t\357\273\2772026-10-16T09:12:11.1234567Z Current runner version: 2.337.0\n'
  i=1
  while [ "${i}" -le 120 ]; do
    printf 'test\tUNKNOWN STEP\t2026-10-16T09:13:%02d.0000000Z \033[32mok\033[0m case %d\n' $((i % 60)) "${i}"
    i=$((i + 1))
  done
  printf 'test\tUNKNOWN STEP\t2026-10-16T09:14:58.0000000Z \033[31mFAIL\033[0m widgets sort order\n'
  printf 'test\tUNKNOWN STEP\t2026-10-16T09:14:58.5000000Z ^[[1m^[[33mwarning^[[0m: build failed\n'
  printf 'test\tUNKNOWN STEP\t2026-10-16T09:14:59.0000000Z ##[error]Process completed with exit code 1.\n'
  printf 'test\tUNKNOWN STEP\t2026-10-16T09:15:00.0000000Z ##[endgroup]\n'
  printf 'test\tUNKNOWN STEP\t2026-10-16T09:15:01.0000000Z Post job cleanup.\n'
  printf 'test\tUNKNOWN STEP\t2026-10-16T09:15:02.0000000Z Cleaning up orphan processes\n'
} >"${TMP}/job.log"

jq '.isDraft = true' "${FIX}/pr-pass.json" >"${TMP}/fix/pr-draft.json"
jq '.mergeable = "CONFLICTING" | .mergeStateStatus = "DIRTY"' "${FIX}/pr-pass.json" >"${TMP}/fix/pr-conflict.json"

PASS=0
FAIL=0
CASE=""
CASE_OK=1
RC=0
OUT=""
ERR=""

# run [VAR=value ...] ARGS...: run pr-flow with the stub gh first on PATH.
run() {
  rm -rf "${TMP}/state"
  mkdir "${TMP}/state"
  RC=0
  env PATH="${TMP}/bin:${PATH}" STUB_FIXTURES="${FIX}" STUB_STATE="${TMP}/state" "$@" \
    >"${TMP}/out" 2>"${TMP}/err" || RC=$?
  OUT="$(cat "${TMP}/out")"
  ERR="$(cat "${TMP}/err")"
}

begin() {
  CASE="$1"
  CASE_OK=1
}
bad() {
  CASE_OK=0
  printf '  %s: %s\n' "${CASE}" "$1"
}
end() {
  if [ "${CASE_OK}" = 1 ]; then
    PASS=$((PASS + 1))
    printf 'ok   %s\n' "${CASE}"
  else
    FAIL=$((FAIL + 1))
    printf 'FAIL %s\n' "${CASE}"
    printf '     stdout: %s\n' "$(printf '%s' "${OUT}" | head -n 12)"
    printf '     stderr: %s\n' "$(printf '%s' "${ERR}" | head -n 8)"
  fi
}
rc_is() { [ "${RC}" = "$1" ] || bad "exit ${RC}, want $1"; }
out_has() { case "${OUT}" in *"$1"*) ;; *) bad "stdout lacks: $1" ;; esac; }
out_lacks() { case "${OUT}" in *"$1"*) bad "stdout has: $1" ;; esac; }
err_has() { case "${ERR}" in *"$1"*) ;; *) bad "stderr lacks: $1" ;; esac; }
calls() { cat "${TMP}/state/calls.log" 2>/dev/null || true; }
calls_has() { case "$(calls)" in *"$1"*) ;; *) bad "gh calls lack: $1" ;; esac; }
calls_lack() { case "$(calls)" in *"$1"*) bad "gh calls have: $1" ;; esac; }
ncalls_is() {
  local n
  n="$(calls | wc -l | tr -d ' ')"
  [ "${n}" = "$1" ] || bad "gh called ${n} times, want $1"
}
no_merge() { [ ! -e "${TMP}/state/merge.log" ] || bad "merge endpoint was called"; }
json_is() { # JQ_EXPR: stdout must be one JSON value for which the expression is true
  printf '%s' "${OUT}" | jq -e "$1" >/dev/null 2>&1 || bad "json check failed: $1"
}
sleeps_are() {
  local got
  got="$(cat "${TMP}/state/sleep.log" 2>/dev/null || true)"
  [ "${got}" = "$1" ] || bad "sleeps '${got}', want '$1'"
}

# ---------- help and usage ----------

begin "--help prints usage, exit codes and example"
run "${TOOL}" --help
rc_is 0
[ "$(printf '%s\n' "${OUT}" | head -n 1 | cut -c1-15)" = "Usage: pr-flow " ] || bad "first line is not Usage: pr-flow"
out_has "Exit codes:"
out_has "Example:"
out_has "Why: gh prefers GITHUB_TOKEN/GH_TOKEN"
ncalls_is 0
end

begin "-h and subcommand --help"
run "${TOOL}" -h
rc_is 0
out_has "Usage: pr-flow"
run "${TOOL}" merge --help
rc_is 0
out_has "--match-head"
end

begin "usage errors exit 2 without calling gh"
for args in "" "frobnicate 1" "status" "status 42 --bogus" "status abc" "status 42 --auth bogus" \
  "status 42 --watch" "checks 42 --interval abc" "checks 42 --interval 0" "checks 42 --timeout -1" \
  "status 42 --repo not-a-repo" "status 42 --repo ../x" "status other/thing#42 --repo acme/widgets" \
  "runs main --limit 0" "runs main --limit x" "failed-log abc" "failed-log 1 --tail 0" "status 1 2" \
  "merge 42 --method squash" "merge 42 --match-head abc1234 --method squash" \
  "merge 42 --match-head ${HEAD_SHA}" "merge 42 --match-head ${HEAD_SHA} --method octopus" \
  "status 42 --repo"; do
  # shellcheck disable=SC2086 # word splitting of the case string is intended
  run "${TOOL}" ${args}
  [ "${RC}" = 2 ] || bad "'${args}' exit ${RC}, want 2"
  [ -z "$(calls)" ] || bad "'${args}' called gh"
done
end

begin "missing gh exits 3"
rm -rf "${TMP}/state"
mkdir "${TMP}/state"
RC=0
env PATH="${TMP}/minbin" "${TOOL}" status 42 --repo acme/widgets >"${TMP}/out" 2>"${TMP}/err" || RC=$?
OUT="$(cat "${TMP}/out")"
ERR="$(cat "${TMP}/err")"
rc_is 3
err_has "missing required command: gh"
end

# ---------- status ----------

begin "status text: state, head, mergeable, review, rollup"
run STUB_PR_VIEW=pr-pass.json "${TOOL}" status 42 --repo acme/widgets
rc_is 0
out_has 'acme/widgets#42 OPEN "Add widget sorting"'
out_has "head ${HEAD_SHA} (feature/sort -> main)"
out_has "mergeable MERGEABLE, merge state CLEAN, review APPROVED"
out_has "checks pass: 3 total, 2 pass, 1 skip, 0 fail, 0 pending"
calls_has "pr view 42 --repo acme/widgets --json"
end

begin "status --json names failing and pending checks (cancelled and error fail)"
run STUB_PR_VIEW=pr-fail.json "${TOOL}" status 42 --repo acme/widgets --json
rc_is 0
json_is '.checks.verdict == "fail" and .checks.fail == 3 and .checks.pending == 1 and .checks.pass == 1'
json_is '[.checks.non_passing[].name] == ["CI / lint","CI / test","ci/external","CI / build"]'
json_is '.head_sha == "'"${HEAD_SHA}"'" and .review_decision == "APPROVED" and .merge_state == "CLEAN"'
end

begin "status text lists failing before pending"
run STUB_PR_VIEW=pr-fail.json "${TOOL}" status 42 --repo acme/widgets
rc_is 0
out_has "  fail    CI / test (FAILURE)"
out_has "  fail    CI / lint (CANCELLED)"
out_has "  pending CI / build (IN_PROGRESS)"
end

begin "status resolves the repository with gh repo view"
run STUB_PR_VIEW=pr-pass.json "${TOOL}" status 42
rc_is 0
[ "$(calls | head -n 1)" = "repo view --json nameWithOwner|token=unset" ] || bad "first call is not repo view"
calls_has "pr view 42 --repo acme/widgets"
end

begin "status accepts owner/name#N and pull request URLs"
run STUB_PR_VIEW=pr-pass.json "${TOOL}" status other/thing#7
rc_is 0
calls_has "pr view 7 --repo other/thing"
calls_lack "repo view"
run STUB_PR_VIEW=pr-pass.json "${TOOL}" status https://github.com/other/thing/pull/8/files
rc_is 0
calls_has "pr view 8 --repo other/thing"
end

begin "unresolvable repository exits 3"
run STUB_NO_REPO=1 "${TOOL}" status 42
rc_is 3
err_has "pass --repo owner/name"
end

begin "missing pull request exits 1, network error exits 3"
run "STUB_FAIL=GraphQL: Could not resolve to a PullRequest with the number of 999. (repository.pullRequest)" \
  "${TOOL}" status 999 --repo acme/widgets
rc_is 1
err_has "not found"
run "STUB_FAIL=error connecting to api.github.com" "${TOOL}" status 42 --repo acme/widgets
rc_is 3
err_has "gh failed"
end

# ---------- auth selection ----------

begin "auth auto: stale env token retries once with keyring, then stays on keyring"
run GITHUB_TOKEN="${FAKE_TOKEN}" STUB_AUTH=env-bad STUB_PR_VIEW=pr-pass.json "${TOOL}" status 42
rc_is 0
err_has "keyring login succeeded"
ncalls_is 3
[ "$(calls | sed -n 1p)" = "repo view --json nameWithOwner|token=set" ] || bad "first try did not use the env token"
[ "$(calls | sed -n 2p)" = "repo view --json nameWithOwner|token=unset" ] || bad "retry did not unset the env token"
case "$(calls | sed -n 3p)" in "pr view 42 "*"|token=unset") ;; *) bad "later call did not stay on keyring" ;; esac
case "${OUT}${ERR}" in *"${FAKE_TOKEN}"*) bad "token value printed" ;; esac
end

begin "auth auto: integration permission error with GH_TOKEN also falls back"
run GH_TOKEN="${FAKE_TOKEN}" STUB_AUTH=integration STUB_PR_VIEW=pr-pass.json "${TOOL}" status 42 --repo acme/widgets
rc_is 0
err_has "keyring login succeeded"
ncalls_is 2
end

begin "auth env: no retry, exit 3"
run GITHUB_TOKEN="${FAKE_TOKEN}" STUB_AUTH=env-bad STUB_PR_VIEW=pr-pass.json "${TOOL}" status 42 --repo acme/widgets --auth env
rc_is 3
err_has "authentication failed (tried: environment as is)"
ncalls_is 1
end

begin "auth keyring: env tokens unset from the first call"
run GITHUB_TOKEN="${FAKE_TOKEN}" GH_TOKEN="${FAKE_TOKEN}" STUB_AUTH=env-bad STUB_PR_VIEW=pr-pass.json \
  "${TOOL}" status 42 --repo acme/widgets --auth keyring
rc_is 0
ncalls_is 1
calls_has "|token=unset"
end

begin "auth auto: both modes failing exits 3 naming both"
run GITHUB_TOKEN="${FAKE_TOKEN}" STUB_AUTH=all-bad "${TOOL}" status 42 --repo acme/widgets
rc_is 3
err_has "tried: environment token, then keyring login"
ncalls_is 2
run STUB_AUTH=all-bad "${TOOL}" status 42 --repo acme/widgets
rc_is 3
ncalls_is 1
end

# ---------- checks ----------

begin "checks: pass 0, fail 1, pending 4, none 1"
run STUB_PR_VIEW=pr-pass.json "${TOOL}" checks 42 --repo acme/widgets
rc_is 0
out_has "checks pass: 3 total"
run STUB_PR_VIEW=pr-fail.json "${TOOL}" checks 42 --repo acme/widgets
rc_is 1
out_has "  fail    CI / test (FAILURE)"
run STUB_PR_VIEW=pr-pending.json "${TOOL}" checks 42 --repo acme/widgets
rc_is 4
out_has "checks pending: 3 total, 0 pass, 0 skip, 0 fail, 3 pending"
run STUB_PR_VIEW=pr-empty.json "${TOOL}" checks 42 --repo acme/widgets
rc_is 1
out_has "checks none: 0 total"
end

begin "checks --json"
run STUB_PR_VIEW=pr-fail.json "${TOOL}" checks 42 --repo acme/widgets --json
rc_is 1
json_is '.verdict == "fail" and .number == 42 and (.checks | length) == 5'
end

begin "watch prints only transitions and backs off while unchanged"
run PATH="${TMP}/sleepbin:${TMP}/bin:${PATH}" \
  "STUB_PR_VIEW=watch-1-pending.json watch-1-pending.json watch-2-partial.json watch-3-pass.json" \
  "${TOOL}" checks 42 --repo acme/widgets --watch --interval 10
rc_is 0
out_has "watching acme/widgets#42 head 4f2c9a1e7b3d: pending: 2 total"
out_has "CI / lint: pending -> pass (SUCCESS)"
out_has "CI / test: pending -> pass (SUCCESS)"
[ "$(printf '%s\n' "${OUT}" | grep -c -- ' -> ')" = 2 ] || bad "want exactly 2 transition lines"
out_has "checks pass: 2 total, 2 pass"
sleeps_are "10 20 10 "
end

begin "watch stops at the first failure"
run PATH="${TMP}/sleepbin:${TMP}/bin:${PATH}" "STUB_PR_VIEW=watch-1-pending.json watch-fail.json" \
  "${TOOL}" checks 42 --repo acme/widgets --watch --interval 5
rc_is 1
out_has "CI / test: pending -> fail (FAILURE)"
out_has "  fail    CI / test (FAILURE)"
end

begin "watch timeout exits 4 and never sleeps past the deadline"
run PATH="${TMP}/sleepbin:${TMP}/bin:${PATH}" STUB_PR_VIEW=watch-1-pending.json \
  "${TOOL}" checks 42 --repo acme/widgets --watch --interval 10 --timeout 45
rc_is 4
out_has "timeout after 45s, still pending"
sleeps_are "10 20 15 "
end

begin "watch timeout with real sleep"
run STUB_PR_VIEW=watch-1-pending.json "${TOOL}" checks 42 --repo acme/widgets --watch --interval 1 --timeout 1
rc_is 4
out_has "timeout after"
end

begin "watch with no checks attached exits 1 after the grace"
run STUB_PR_VIEW=pr-empty.json "${TOOL}" checks 42 --repo acme/widgets --watch --timeout 0
rc_is 1
out_has "no check attached to this head"
end

begin "watch follows a new head"
run PATH="${TMP}/sleepbin:${TMP}/bin:${PATH}" \
  "STUB_PR_VIEW=watch-1-pending.json watch-newhead-pending.json watch-newhead-pass.json" \
  "${TOOL}" checks 42 --repo acme/widgets --watch --interval 3
rc_is 0
out_has "head changed 4f2c9a1e7b3d -> 9a8b7c6d5e4f"
out_has "head 9a8b7c6d5e4f checks pass"
end

begin "watch --json: one JSON result on stdout, transitions on stderr"
run PATH="${TMP}/sleepbin:${TMP}/bin:${PATH}" "STUB_PR_VIEW=watch-1-pending.json watch-3-pass.json" \
  "${TOOL}" checks 42 --repo acme/widgets --watch --interval 2 --json
rc_is 0
json_is '.verdict == "pass" and .polls == 2'
err_has "CI / lint: pending -> pass"
end

# ---------- runs ----------

begin "runs for a branch: compact table"
run "${TOOL}" runs feature/sort --repo acme/widgets
rc_is 0
calls_has "run list --repo acme/widgets --branch feature/sort --limit 5"
out_has "acme/widgets runs for branch feature/sort: 2 shown"
out_has "ID           WORKFLOW                 STATUS      CONCLUSION      AGE   EVENT         HEAD"
out_has "39046326026  CI                       in_progress -               "
out_has "39037289804  Release candidate packag completed   failure         "
end

begin "runs --json and --limit"
run "${TOOL}" runs main --repo acme/widgets --json --limit 2
rc_is 0
calls_has "--branch main --limit 2"
json_is '(.runs | length) == 2 and .runs[0].id == 39046326026 and .runs[1].conclusion == "failure" and (.runs[0].age | test("^[0-9]+[smhd]$"))'
end

begin "runs for a full SHA, a short SHA and an unknown hex name"
run "${TOOL}" runs "${HEAD_SHA}" --repo acme/widgets
rc_is 0
calls_has "run list --repo acme/widgets --commit ${HEAD_SHA}"
calls_lack "api "
run "${TOOL}" runs 4f2c9a1 --repo acme/widgets
rc_is 0
calls_has "api -H Accept: application/vnd.github.sha repos/acme/widgets/commits/4f2c9a1"
calls_has "--commit ${HEAD_SHA}"
run "${TOOL}" runs deadbee --repo acme/widgets
rc_is 0
calls_has "--branch deadbee"
end

begin "runs with none found exits 1"
run STUB_RUN_LIST=run-list-empty.json "${TOOL}" runs main --repo acme/widgets
rc_is 1
err_has "no workflow runs"
end

# ---------- failed-log ----------

begin "failed-log: failed job tail ending at the last error, cleaned"
run "${TOOL}" failed-log 39037289804 --repo acme/widgets --tail 5
rc_is 0
calls_has "run view --repo acme/widgets --job 108543044561 --log-failed"
calls_lack "--job 108543044560"
out_has "run 39037289804 CI completed/failure head 4f2c9a1 (feature/sort): 1 failed job(s)"
out_has "== job test (failure; failed steps: Run tests)"
out_has "-- job log: lines 120-124 of 126, ending at the last ##[error]"
out_has "FAIL widgets sort order"
out_has "warning: build failed"
out_lacks "^[["
out_has "##[error]Process completed with exit code 1."
out_lacks "Post job cleanup"
out_lacks "2026-10-16T"
out_lacks "case 116"
case "${OUT}" in *"$(printf '\033')"* | *"$(printf '\357\273\277')"*) bad "color codes or BOM left in output" ;; esac
[ "$(printf '%s\n' "${OUT}" | wc -l | tr -d ' ')" = 8 ] || bad "want 8 output lines (3 headers + 5 log lines)"
end

begin "failed-log default tail is bounded at 80 lines"
run "${TOOL}" failed-log 39037289804 --repo acme/widgets
rc_is 0
out_has "-- job log: lines 45-124 of 126, ending at the last ##[error]"
end

begin "failed-log --json"
run "${TOOL}" failed-log 39037289804 --repo acme/widgets --tail 3 --json
rc_is 0
json_is '.run_id == 39037289804 and (.failed_jobs | length) == 1 and .failed_jobs[0].failed_steps == ["Run tests"]'
json_is '(.failed_jobs[0].log | length) == 4 and (.failed_jobs[0].log[-1] | startswith("##[error]"))'
end

begin "failed-log on a green run exits 1"
run STUB_RUN_VIEW=run-view-success.json "${TOOL}" failed-log 39037289804 --repo acme/widgets
rc_is 1
err_has "has no failed jobs (completed/success)"
end

begin "failed-log when gh cannot serve the log yet exits 1"
run STUB_RUN_VIEW=run-view-inprogress.json "STUB_LOG_FAIL=run 39037289804 is still in progress; logs will be available when it is complete" \
  "${TOOL}" failed-log 39037289804 --repo acme/widgets
rc_is 1
out_has "-- log unavailable: run 39037289804 is still in progress"
err_has "no failed job log is available yet"
end

# ---------- merge ----------

begin "merge refuses a head mismatch without calling merge"
run STUB_PR_VIEW=pr-pass.json "${TOOL}" merge 42 --repo acme/widgets --match-head "${OTHER_SHA}" --method squash
rc_is 1
out_has "refused acme/widgets#42: head is ${HEAD_SHA}, not ${OTHER_SHA}"
no_merge
calls_lack "pr merge"
end

begin "merge --dry-run verifies and never calls the merge endpoint"
run STUB_PR_VIEW=pr-pass.json "${TOOL}" merge 42 --repo acme/widgets --match-head "${HEAD_SHA}" --method squash --dry-run
rc_is 0
out_has "verified acme/widgets#42 head ${HEAD_SHA}"
out_has "would run gh pr merge 42 --repo acme/widgets --squash --match-head-commit ${HEAD_SHA}"
no_merge
calls_lack "pr merge"
end

begin "merge --dry-run --json warns about non-green checks"
run STUB_PR_VIEW=pr-fail.json "${TOOL}" merge 42 --repo acme/widgets --match-head "${HEAD_SHA}" --method rebase --dry-run --json
rc_is 0
json_is '.result == "would-merge" and .dry_run == true and .method == "rebase"'
err_has "warning: checks not green"
no_merge
end

begin "merge passes --match-head-commit and reports the merge commit"
run "STUB_PR_VIEW=pr-pass.json pr-merged.json" "${TOOL}" merge 42 --repo acme/widgets --match-head "${HEAD_SHA}" --method merge
rc_is 0
calls_has "pr merge 42 --repo acme/widgets --merge --match-head-commit ${HEAD_SHA}|"
out_has "merged acme/widgets#42: merged with --merge at head ${HEAD_SHA}"
end

begin "merge --json result"
run "STUB_PR_VIEW=pr-pass.json pr-merged.json" "${TOOL}" merge acme/widgets#42 --match-head "${HEAD_SHA}" --method squash --json
rc_is 0
json_is '.result == "merged" and .merge_commit == "0d1e2f3a4b5c6d7e8f90a1b2c3d4e5f6a7b8c9d0"'
end

begin "merge refuses draft, conflicting and closed states; already merged is 0"
run STUB_PR_VIEW="${TMP}/fix/pr-draft.json" "${TOOL}" merge 42 --repo acme/widgets --match-head "${HEAD_SHA}" --method squash
rc_is 1
out_has "draft"
run STUB_PR_VIEW="${TMP}/fix/pr-conflict.json" "${TOOL}" merge 42 --repo acme/widgets --match-head "${HEAD_SHA}" --method squash
rc_is 1
out_has "merge conflicts"
run STUB_PR_VIEW=pr-merged.json "${TOOL}" merge 42 --repo acme/widgets --match-head "${HEAD_SHA}" --method squash
rc_is 0
out_has "already-merged"
no_merge
end

begin "merge rejected by gh exits 1 and shows the reason"
run STUB_PR_VIEW=pr-pass.json "STUB_MERGE_FAIL=Pull request is not mergeable: the base branch policy prohibits the merge" \
  "${TOOL}" merge 42 --repo acme/widgets --match-head "${HEAD_SHA}" --method squash
rc_is 1
out_has "rejected acme/widgets#42"
err_has "gh: Pull request is not mergeable"
end

printf '\n%d passed, %d failed\n' "${PASS}" "${FAIL}"
[ "${FAIL}" = 0 ]
