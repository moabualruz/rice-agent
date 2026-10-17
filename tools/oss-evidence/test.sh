#!/usr/bin/env bash
# Tests for oss-evidence. Stub gh and curl on PATH serve the fixtures in testdata/ (shapes
# recorded from gh 2.102 and the live APIs on 2026-10-17), including the two traps: gh prints
# the 404 JSON body on stdout before failing, and the Scorecard API answers 404 with an empty
# body. No live network. Run from the tool directory: bash test.sh
# Set OSS_EVIDENCE_TEST_BASH=/bin/bash to run the tool under another Bash (for example 3.2).
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOOL="${HERE}/oss-evidence"
FIX="${HERE}/testdata"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/oss-evidence-test.XXXXXX")"
trap 'rm -rf "${TMP}"' EXIT
TEST_BASH="${OSS_EVIDENCE_TEST_BASH:-}"
NOW="2026-10-17T12:00:00Z"
# A token-shaped fake, composed at run time so no token-shaped literal sits in the file.
FAKE_TOKEN="ghp_$(printf '%s%s' 0123456789abcdefghij ABCDEFGHIJ012345)"

# Never let a real token reach the stubs or the output.
unset GITHUB_TOKEN GH_TOKEN

mkdir -p "${TMP}/bin" "${TMP}/nogh" "${TMP}/state"

cat >"${TMP}/bin/gh" <<'STUB'
#!/usr/bin/env bash
# Stub gh: `gh api <path> --jq <filter>` applies the filter to testdata/gh/<path with _>.json.
set -u
fix="${STUB_FIXTURES:?}/gh"
printf 'gh %s\n' "$*" >>"${STUB_STATE:?}/calls.log"
[ "${1:-}" = api ] || { echo "stub gh: unhandled: $*" >&2; exit 64; }
path="$2"
filter="."
[ "${3:-}" != --jq ] || filter="$4"
case "${path}" in
  repos/ratelimited/*)
    cat "${fix}/403-ratelimit.json"
    echo 'gh: API rate limit exceeded for user ID 1. If you reach out to GitHub Support for help, please include the request ID 0000:0000. (HTTP 403)' >&2
    exit 1 ;;
  repos/slow/*) exec sleep 6 ;;
  repos/leaky/*)
    printf '{"message":"Server Error near %s"}\n' "${STUB_LEAK:?}"
    printf 'gh: Server Error near %s (HTTP 500)\n' "${STUB_LEAK}" >&2
    exit 1 ;;
  repos/noauth/*)
    printf 'To get started with GitHub CLI, please run:  gh auth login\n' >&2
    exit 4 ;;
esac
f="${fix}/$(printf '%s' "${path}" | tr '/' '_').json"
if [ ! -f "${f}" ]; then
  # The trap: the 404 body goes to stdout, then gh fails.
  case "${path}" in
    */releases/latest) cat "${fix}/404-release.json" ;;
    *) cat "${fix}/404-repo.json" ;;
  esac
  echo 'gh: Not Found (HTTP 404)' >&2
  exit 1
fi
jq -c "${filter}" "${f}"
STUB

cat >"${TMP}/bin/curl" <<'STUB'
#!/usr/bin/env bash
# Stub curl: answers from testdata/http/routes.tsv (URL, code or behavior, body file).
set -u
fix="${STUB_FIXTURES:?}/http"
out=""
wfmt=""
url=""
post=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -w) wfmt="$2"; shift 2 ;;
    --data-binary) post=1; shift 2 ;;
    -m | --connect-timeout | --max-filesize | -A | -H | --proto) shift 2 ;;
    https://*) url="$1"; shift ;;
    *) shift ;;
  esac
done
key="${url}"
if [ "${post}" = 1 ]; then
  key="${url}#$(jq -r '"\(.package.ecosystem):\(.package.name)@\(.version)"')"
fi
printf 'curl %s\n' "${key}" >>"${STUB_STATE:?}/calls.log"
route=$(awk -F '\t' -v k="${key}" '$1 == k { print $2 "\t" $3; exit }' "${fix}/routes.tsv")
[ -n "${route}" ] || { echo "stub curl: no route for ${key}" >&2; exit 99; }
code="${route%%	*}"
body="${route#*	}"
case "${code}" in
  timeout) echo 'curl: (28) Operation timed out after 1000 milliseconds with 0 bytes received' >&2; exit 28 ;;
  neterr) echo 'curl: (6) Could not resolve host: api.deps.dev' >&2; exit 6 ;;
esac
if [ "${body}" = - ]; then : >"${out}"; else cat "${fix}/${body}" >"${out}"; fi
case "${wfmt}" in *http_code*) printf '%s' "${code}" ;; esac
STUB
chmod +x "${TMP}/bin/gh" "${TMP}/bin/curl"

# PATH without gh, for the missing-dependency case.
for c in bash jq; do ln -s "$(command -v "${c}")" "${TMP}/nogh/${c}"; done
ln -s "${TMP}/bin/curl" "${TMP}/nogh/curl"

PASS=0
FAIL=0
RC=0
OUT=""
ERR=""

# run [VAR=value ...] ARGS...: run the tool with the stubs first on PATH and a fixed clock.
run() {
  : >"${TMP}/state/calls.log"
  RC=0
  env PATH="${TMP}/bin:${PATH}" STUB_FIXTURES="${FIX}" STUB_STATE="${TMP}/state" STUB_LEAK="${FAKE_TOKEN}" \
    OSS_EVIDENCE_NOW="${NOW}" AGENT_DRY_CONFIG="${TMP}/no-config.json" \
    ${TEST_BASH:+"${TEST_BASH}"} "${TOOL}" "$@" >"${TMP}/out" 2>"${TMP}/err" || RC=$?
  OUT="$(cat "${TMP}/out")"
  ERR="$(cat "${TMP}/err")"
}
# runenv VAR=value... -- ARGS...: like run, with extra environment.
runenv() {
  local vars=()
  while [ "$1" != -- ]; do
    vars[${#vars[@]}]="$1"
    shift
  done
  shift
  : >"${TMP}/state/calls.log"
  RC=0
  env PATH="${TMP}/bin:${PATH}" STUB_FIXTURES="${FIX}" STUB_STATE="${TMP}/state" STUB_LEAK="${FAKE_TOKEN}" \
    OSS_EVIDENCE_NOW="${NOW}" AGENT_DRY_CONFIG="${TMP}/no-config.json" "${vars[@]}" \
    ${TEST_BASH:+"${TEST_BASH}"} "${TOOL}" "$@" >"${TMP}/out" 2>"${TMP}/err" || RC=$?
  OUT="$(cat "${TMP}/out")"
  ERR="$(cat "${TMP}/err")"
}

ok() {
  PASS=$((PASS + 1))
  printf 'ok   %s\n' "$1"
}
bad() {
  FAIL=$((FAIL + 1))
  printf 'FAIL %s\n' "$1"
  printf '%s\n' "${OUT}" | sed -n '1,12s/^/     out: /p'
  printf '%s\n' "${ERR}" | sed -n '1,6s/^/     err: /p'
}
expect_rc() { if [ "${RC}" -eq "$2" ]; then ok "$1"; else bad "$1: exit ${RC}, want $2"; fi; }
expect_out() { if printf '%s' "${OUT}" | grep -F -q -e "$2"; then ok "$1"; else bad "$1: stdout lacks '$2'"; fi; }
expect_err() { if printf '%s' "${ERR}" | grep -F -q -e "$2"; then ok "$1"; else bad "$1: stderr lacks '$2'"; fi; }
refute_all() {
  if printf '%s\n%s' "${OUT}" "${ERR}" | grep -F -q -e "$2"; then bad "$1: output contains '$2'"; else ok "$1"; fi
}
# expect_jq <description> <jq expression that must be true on stdout>
expect_jq() {
  if printf '%s' "${OUT}" | jq -e "$2" >/dev/null 2>&1; then ok "$1"; else bad "$1: jq '$2' is not true"; fi
}
calls_has() { if grep -F -q -e "$2" "${TMP}/state/calls.log"; then ok "$1"; else bad "$1: no call matching '$2'"; fi; }
calls_lack() { if grep -F -q -e "$2" "${TMP}/state/calls.log"; then bad "$1: unexpected call '$2'"; else ok "$1"; fi; }

# ---------- help, rubric and usage ----------

run --help
expect_rc "--help exits 0" 0
case "${OUT}" in "Usage: oss-evidence"*) ok "--help starts with Usage" ;; *) bad "--help starts with Usage" ;; esac
expect_out "--help documents exit codes" "Exit codes:"
expect_out "--help documents --package" "--package ECO:NAME"
run -h
expect_rc "-h exits 0" 0

run --rubric
expect_rc "--rubric exits 0" 0
expect_out "rubric lists vetoes first" "Vetoes first"
expect_out "rubric names the python runtime veto" "Python runtime for a non-Python"
expect_out "rubric names the critical advisory veto" "critical-advisories"
for ring in adopt trial assess hold; do expect_out "rubric ring ${ring}" "  ${ring} "; done
expect_out "rubric shows the default allowlist" "License allowlist (default): MIT, Apache-2.0"
calls_lack "rubric makes no network call" "api"
run --rubric --licenses MIT,ISC
expect_out "rubric shows a --licenses allowlist" "License allowlist (--licenses): MIT, ISC"
printf '{"oss_evidence":{"license_allowlist":["MIT","GPL-3.0"]}}\n' >"${TMP}/config.json"
runenv AGENT_DRY_CONFIG="${TMP}/config.json" -- --rubric
expect_out "rubric shows the config allowlist" "(config oss_evidence.license_allowlist): MIT, GPL-3.0"

run
expect_rc "no candidate is a usage error" 2
expect_err "usage error points to --help" "--help for usage"
run not-a-slug
expect_rc "bad slug is a usage error" 2
run a/b/c/d
expect_rc "slug with extra segments is a usage error" 2
run --bogus ast-grep/ast-grep
expect_rc "unknown option is a usage error" 2
run ast-grep/ast-grep --package
expect_rc "--package without a value is a usage error" 2
run ast-grep/ast-grep --package foo:bar
expect_rc "unknown ecosystem is a usage error" 2
run ast-grep/ast-grep --package npm
expect_rc "--package without a name is a usage error" 2
run ast-grep/ast-grep --package npm:a --package npm:b
expect_rc "two packages for one repository is a usage error" 2
run --package npm:a ast-grep/ast-grep --package npm:b
expect_rc "early and late package for the first repository is a usage error" 2
run --timeout 0 ast-grep/ast-grep
expect_rc "--timeout 0 is a usage error" 2
run --timeout abc ast-grep/ast-grep
expect_rc "--timeout abc is a usage error" 2
run --licenses 'MIT;rm' ast-grep/ast-grep
expect_rc "bad --licenses is a usage error" 2
run --json=1 ast-grep/ast-grep
expect_rc "--json=value is a usage error" 2
run --rubric ast-grep/ast-grep
expect_rc "--rubric with candidates is a usage error" 2
set --
n=0
while [ "${n}" -lt 21 ]; do
  set -- "$@" "o/r${n}"
  n=$((n + 1))
done
run "$@"
expect_rc "more than 20 candidates is a usage error" 2
set --

# ---------- environment failures ----------

RC=0
env PATH="${TMP}/nogh" OSS_EVIDENCE_NOW="${NOW}" AGENT_DRY_CONFIG="${TMP}/no-config.json" \
  "$(command -v bash)" "${TOOL}" ast-grep/ast-grep >"${TMP}/out" 2>"${TMP}/err" || RC=$?
OUT="$(cat "${TMP}/out")"
ERR="$(cat "${TMP}/err")"
expect_rc "missing gh exits 3" 3
expect_err "missing gh is named" "missing required command: gh"
printf '[1,2]\n' >"${TMP}/bad-config.json"
runenv AGENT_DRY_CONFIG="${TMP}/bad-config.json" -- ast-grep/ast-grep
expect_rc "unusable config exits 3" 3
expect_err "unusable config is named" "config file unusable"
runenv OSS_EVIDENCE_NOW=yesterday -- ast-grep/ast-grep
expect_rc "bad OSS_EVIDENCE_NOW exits 3" 3

# ---------- motivating case: several candidates, --json ----------

run ast-grep/ast-grep --package npm:@ast-grep/cli jpeddicord/askalono ryoppippi/ccusage \
  pyorg/pylib --package pypi:pylib --json
expect_rc "all repositories found exits 0" 0
expect_jq "json is an array of 4 records" 'type == "array" and length == 4'
expect_jq "records keep candidate order" '[.[].candidate] == ["ast-grep/ast-grep","jpeddicord/askalono","ryoppippi/ccusage","pyorg/pylib"]'
expect_jq "every record carries retrieved_at" "all(.[]; .retrieved_at == \"${NOW}\")"
expect_jq "every field has a value or a missing reason" \
  'all(.[].evidence[]; (.value != null and (has("missing") | not)) or (.value == null and (.missing | type) == "string"))'
expect_jq "every requested field names its source URL" \
  'all(.[].evidence[] | select((.missing // "") | startswith("not requested") | not); .source | startswith("https://"))'
expect_jq "github fields for ast-grep" \
  '.[0].evidence | .full_name.value == "ast-grep/ast-grep" and .language.value == "Rust" and .stars.value == 16255 and .forks.value == 480 and .open_issues.value == 64 and .default_branch.value == "main" and .archived.value == false and .license.value == "MIT"'
expect_jq "github source URL" '.[0].evidence.stars.source == "https://api.github.com/repos/ast-grep/ast-grep"'
expect_jq "latest release tag and date" \
  '.[0].evidence.release_tag.value == "0.51.0" and .[0].evidence.release_date.value == "2026-10-15T15:10:15Z" and (.[0].evidence.release_tag.source | endswith("/releases/latest"))'
expect_jq "scorecard score, date and maintained" \
  '.[0].evidence | .scorecard.value == 7.1 and .scorecard_date.value == "2026-10-12" and .maintained.value == 10 and .scorecard.source == "https://api.scorecard.dev/projects/github.com/ast-grep/ast-grep"'
expect_jq "deps.dev project" '.[0].evidence.depsdev_project.value == "github.com/ast-grep/ast-grep" and .[0].evidence.oss_fuzz.value == false'
expect_jq "package uses the default version, not the newer prerelease" \
  '.[0].package == "npm:@ast-grep/cli" and .[0].evidence.package_latest.value == "0.51.0" and .[0].evidence.package_versions.value == 3'
expect_jq "package license, dependents and advisories" \
  '.[0].evidence | .package_license.value == "MIT" and .dependents.value == 1234 and .osv_advisories.value == 0 and .osv_critical.value == 0 and .package_deprecated.value == false'
expect_jq "dependents source is the deps.dev v3alpha endpoint" \
  '.[0].evidence.dependents.source == "https://api.deps.dev/v3alpha/systems/NPM/packages/%40ast-grep%2Fcli/versions/0.51.0:dependents"'
expect_jq "healthy candidate has no flags" '.[0].flags == []'
expect_jq "archived and stale candidate is flagged" '.[1].flags == ["archived","stale","release-stale"]'
expect_jq "maintained 0 is a value, not missing" '.[1].evidence.maintained.value == 0'
expect_jq "deps.dev 404 text body is recorded as missing" '.[1].evidence.depsdev_project.missing == "project not in deps.dev (HTTP 404)"'
expect_jq "fields without --package are not requested" \
  '.[1].package == null and .[1].evidence.dependents.missing == "not requested (pass --package)" and .[1].evidence.dependents.source == null'
expect_jq "moved repository is flagged and uses the new name" \
  '.[2].evidence.full_name.value == "ccusage/ccusage" and (.[2].flags | index("moved")) != null'
expect_jq "NOASSERTION license is flagged license-unknown" '(.[2].flags | index("license-unknown")) != null'
expect_jq "empty-body Scorecard 404 is missing, never zero" \
  '.[2].evidence.scorecard == {"value": null, "missing": "not scanned by OpenSSF Scorecard (HTTP 404)", "source": "https://api.scorecard.dev/projects/github.com/ccusage/ccusage"} and .[2].evidence.maintained.value == null'
expect_jq "gh 404 body on stdout is not taken as a release" \
  '.[2].evidence.release_tag.value == null and .[2].evidence.release_tag.missing == "no GitHub release (HTTP 404)"'
refute_all "gh 404 body never reaches the output" "documentation_url"
calls_has "scorecard is queried with the canonical name" "api.scorecard.dev/projects/github.com/ccusage/ccusage"
calls_lack "scorecard is not queried with the old name" "github.com/ryoppippi"
expect_jq "oss-fuzz presence from deps.dev" '.[2].evidence.oss_fuzz.value == true'
expect_jq "python, license, deprecated, unlinked and advisory flags" \
  '.[3].flags == ["license-not-allowlisted","python","package-deprecated","package-unlinked","advisories","critical-advisories"]'
expect_jq "advisory counts include critical and unrated" \
  '.[3].evidence | .osv_advisories.value == 3 and .osv_critical.value == 1 and .osv_unrated.value == 1 and .osv_ids.value == ["GHSA-aaaa-bbbb-cccc","GHSA-dddd-eeee-ffff","PYSEC-2026-1"]'
expect_jq "latest version falls back to the newest when none is default" '.[3].evidence.package_latest.value == "2.0.0"'
expect_jq "inconclusive Maintained check is missing with its reason" \
  '.[3].evidence.maintained.missing | startswith("inconclusive (Scorecard -1): project was created")'
expect_jq "dependents 404 is missing" '.[3].evidence.dependents.missing == "dependents not available in deps.dev (HTTP 404)"'
expect_jq "non-JSON deps.dev answer is missing as invalid JSON" '.[3].evidence.depsdev_project.missing == "invalid JSON (HTTP 200, api.deps.dev)"'
calls_has "OSV is queried for the latest version" "api.osv.dev/v1/query#PyPI:pylib@2.0.0"

# ---------- text output ----------

run ast-grep/ast-grep --package npm:@ast-grep/cli jpeddicord/askalono ryoppippi/ccusage
expect_rc "text run exits 0" 0
case "${OUT}" in "REPO "*) ok "text starts with the header row" ;; *) bad "text starts with the header row" ;; esac
expect_out "header names every column" "LANG"
expect_out "row shows release and scorecard dates" "0.51.0 2026-10-15"
expect_out "row shows scorecard with its date" "7.1 2026-10-12"
expect_out "row for the archived candidate shows its flags" "archived,stale,release-stale"
expect_out "row uses the canonical name of a moved repository" "ccusage/ccusage"
expect_out "package line" "  package npm:@ast-grep/cli: latest 0.51.0 (2026-10-15), 3 versions, license MIT, dependents 1234, advisories 0 (critical 0, unrated 0)"
expect_out "missing line groups fields by reason" "  missing scorecard, scorecard_date, maintained: not scanned by OpenSSF Scorecard (HTTP 404)"
expect_out "missing release is explained" "  missing release_tag, release_date: no GitHub release (HTTP 404)"
expect_out "retrieved line" "retrieved ${NOW}"
refute_all "text hides not-requested fields" "not requested"
if [ "$(printf '%s\n' "${OUT}" | grep -c '^[a-z]')" -eq 4 ]; then ok "one row per candidate plus the retrieved line"; else bad "one row per candidate plus the retrieved line"; fi

# ---------- varied inputs ----------

run https://github.com/ast-grep/ast-grep.git --json
expect_jq "a GitHub URL is accepted" '.[0].candidate == "ast-grep/ast-grep" and .[0].evidence.full_name.value == "ast-grep/ast-grep"'
run --package npm:@ast-grep/cli github.com/ast-grep/ast-grep/tree/main --json
expect_jq "--package before the operand binds to the first repository" '.[0].package == "npm:@ast-grep/cli"'
run ast-grep/ast-grep --licenses Apache-2.0 --json
expect_jq "--licenses overrides the allowlist" '.[0].flags == ["license-not-allowlisted"]'
runenv AGENT_DRY_CONFIG="${TMP}/config.json" -- pyorg/pylib --json
expect_jq "config allowlist is honored" '(.[0].flags | index("license-not-allowlisted")) == null'
run nolicense/tool --package npm:no-such-pkg --json
expect_rc "repository without license or release exits 0" 0
expect_jq "no license is flagged" '.[0].flags == ["no-license"] and .[0].evidence.license.value == "NONE"'
expect_jq "null language is missing, not empty" '.[0].evidence.language.missing == "absent from the response"'
expect_jq "unknown package is missing and its dependents are skipped" \
  '.[0].evidence.package_versions.missing == "package not found in deps.dev (HTTP 404)" and (.[0].evidence.dependents.missing | startswith("skipped: package not found"))'

# ---------- negative and environment results ----------

run ast-grep/ast-grep nobody/missing --json
expect_rc "a missing repository exits 1" 1
expect_err "missing repository is named" "not found on GitHub: nobody/missing"
expect_jq "other candidates are still reported" 'length == 2 and .[0].evidence.stars.value == 16255'
expect_jq "missing repository fields say not found" '.[1].evidence.stars.missing == "repository not found on GitHub (HTTP 404)"'
expect_jq "dependent sources are skipped, not fetched" '.[1].evidence.scorecard.missing == "skipped: repository not found"'
calls_lack "no API call for a missing repository" "nobody/missing/releases"
calls_lack "no scorecard call for a missing repository" "github.com/nobody"

run ratelimited/repo --json
expect_rc "GitHub rate limit exits 3" 3
expect_jq "rate limit is recorded as missing" '.[0].evidence.stars.missing == "rate limited (HTTP 403, api.github.com)" and .[0].evidence.stars.value == null'
expect_jq "scorecard 429 is recorded as rate limited" '.[0].evidence.scorecard.missing == "rate limited (HTTP 429, api.scorecard.dev)"'
expect_jq "empty 200 is recorded as missing" '.[0].evidence.depsdev_project.missing == "empty response (HTTP 200, api.deps.dev)"'
expect_err "unavailable candidate is named" "GitHub metadata unavailable (see the missing reasons): ratelimited/repo"

start=$(date +%s)
run --timeout 1 slow/repo --json
elapsed=$(($(date +%s) - start))
expect_rc "gh timeout exits 3" 3
expect_jq "gh timeout is recorded" '.[0].evidence.stars.missing == "timeout after 1s (api.github.com)"'
expect_jq "curl timeout is recorded" '.[0].evidence.scorecard.missing == "timeout after 1s (api.scorecard.dev)"'
expect_jq "network error is recorded" '.[0].evidence.depsdev_project.missing == "network error: curl exit 6 (api.deps.dev)"'
if [ "${elapsed}" -le 4 ]; then ok "timeout bounds the run (${elapsed}s)"; else bad "timeout bounds the run (${elapsed}s)"; fi

run leaky/repo --json
expect_rc "GitHub server error exits 3" 3
expect_jq "server error is recorded with a redacted message" '.[0].evidence.stars.missing == "GitHub HTTP 500: Server Error near [redacted]"'
expect_jq "scorecard 503 is recorded" '.[0].evidence.scorecard.missing == "HTTP 503 (api.scorecard.dev)"'
refute_all "token-like strings never reach the output" "${FAKE_TOKEN}"

run noauth/repo --json
expect_rc "gh without login exits 3" 3
expect_jq "missing gh login is recorded" '.[0].evidence.stars.missing == "gh not authenticated (run gh auth login)"'

printf '\n%d passed, %d failed\n' "${PASS}" "${FAIL}"
[ "${FAIL}" -eq 0 ]
