#!/usr/bin/env bash
# Tests for install-vendir. Run: bash test.sh
# Network is never used: a stub curl and stub uname on PATH stand in for the release download and
# the platform. The success path needs the real release bytes and is verified live separately.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
entry="$here/install-vendir"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/install-vendir-test.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
fails=0

pass() { printf 'ok   %s\n' "$1"; }
fail() {
  printf 'FAIL %s\n' "$1"
  fails=$((fails + 1))
}

expect_exit() {
  local desc="$1" want="$2" got=0
  shift 2
  "$@" >"$tmp/out" 2>"$tmp/err" || got=$?
  if [ "$got" -eq "$want" ]; then
    pass "$desc"
  else
    fail "$desc: exit $got, want $want"
    sed -n '1,5s/^/     /p' "$tmp/err"
  fi
}
expect_in() {
  if grep -F -q -e "$3" "$tmp/$2"; then pass "$1"; else fail "$1: $2 lacks '$3'"; fi
}

# Stubs: uname reports STUB_OS/STUB_ARCH; curl writes STUB_BYTES or fails when STUB_CURL_FAIL is set.
stub="$tmp/bin"
mkdir -p "$stub"
cat >"$stub/uname" <<'EOF'
#!/usr/bin/env bash
case "${1:-}" in -s) echo "${STUB_OS:-Linux}" ;; -m) echo "${STUB_ARCH:-x86_64}" ;; *) echo "${STUB_OS:-Linux}" ;; esac
EOF
cat >"$stub/curl" <<'EOF'
#!/usr/bin/env bash
[ -n "${STUB_CURL_FAIL:-}" ] && exit 22
out=""
while [ "$#" -gt 0 ]; do case "$1" in -o) out="$2"; shift ;; esac; shift; done
printf '%s' "${STUB_BYTES:-not the release}" >"$out"
EOF
chmod +x "$stub/uname" "$stub/curl"
run() { PATH="$stub:$PATH" "$entry" "$@"; }

expect_exit "--help exits 0" 0 "$entry" --help
expect_in "--help starts with Usage" out "Usage: install-vendir"
expect_in "--help documents --dry-run" out "--dry-run"
expect_exit "missing DEST is a usage error" 2 "$entry"
expect_exit "unknown option is a usage error" 2 "$entry" --bogus x
expect_exit "two DESTs is a usage error" 2 "$entry" a b

expect_exit "dry run on linux-amd64" 0 run --dry-run "$tmp/out-bin/vendir"
expect_in "dry run names the linux asset" out "asset vendir-linux-amd64"
expect_in "dry run pins the linux checksum" out "sha256 0b4bad28b765c4cbf0cc2234d0d420bcf2d352b58eb202e9c55263374f95bd71"
if [ ! -e "$tmp/out-bin" ]; then pass "dry run writes nothing"; else fail "dry run created DEST's directory"; fi

STUB_OS=Darwin STUB_ARCH=arm64 expect_exit "dry run on darwin-arm64" 0 run --dry-run "$tmp/x"
expect_in "dry run names the darwin asset" out "asset vendir-darwin-arm64"
STUB_OS=Linux STUB_ARCH=aarch64 expect_exit "aarch64 maps to arm64" 0 run --dry-run "$tmp/x"
expect_in "arm64 asset" out "asset vendir-linux-arm64"

STUB_OS=SunOS STUB_ARCH=sparc expect_exit "unsupported platform exits 1" 1 run "$tmp/x"
expect_in "unsupported platform is named" err "no pinned vendir for sunos-sparc"

STUB_CURL_FAIL=1 expect_exit "download failure exits 1" 1 run "$tmp/dl/vendir"
if [ ! -e "$tmp/dl/vendir" ]; then pass "failed download leaves no DEST"; else fail "failed download left DEST"; fi

expect_exit "checksum mismatch exits 1" 1 run "$tmp/cs/vendir"
expect_in "mismatch is reported" err "checksum mismatch for vendir-linux-amd64"
if [ ! -e "$tmp/cs/vendir" ]; then pass "mismatch leaves no DEST"; else fail "mismatch left DEST"; fi
leftover=$(find "$tmp/cs" -name 'vendir.*' 2>/dev/null | wc -l | tr -d ' ')
if [ "$leftover" = 0 ]; then pass "mismatch removes its temporary file"; else fail "temporary file left behind"; fi

expect_exit "legacy positional DEST after -- still works in dry run" 0 run --dry-run -- "$tmp/y"
expect_in "legacy dest echoed" out "dest $tmp/y"

if [ "$fails" -eq 0 ]; then
  echo "all install-vendir tests passed"
else
  echo "$fails install-vendir test(s) failed" >&2
  exit 1
fi
