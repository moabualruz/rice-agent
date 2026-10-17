#!/usr/bin/env bash
# Tests for the g3-bad-help fixture.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ "$("$here/g3-bad-help" hello)" = "hello" ]
if "$here/g3-bad-help" >/dev/null 2>&1; then exit 1; fi
echo "g3-bad-help: all tests passed"
