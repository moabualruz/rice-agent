#!/usr/bin/env bash
# Tests for the g6-bad-origin fixture.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ "$("$here/g6-bad-origin" hello)" = "hello" ]
if "$here/g6-bad-origin" >/dev/null 2>&1; then exit 1; fi
echo "g6-bad-origin: all tests passed"
