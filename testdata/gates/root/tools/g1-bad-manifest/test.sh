#!/usr/bin/env bash
# Tests for the g1-bad-manifest fixture.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ "$("$here/g1-bad-manifest" hello)" = "hello" ]
if "$here/g1-bad-manifest" >/dev/null 2>&1; then exit 1; fi
echo "g1-bad-manifest: all tests passed"
