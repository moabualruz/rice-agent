#!/usr/bin/env bash
# Tests for the dup-index fixture.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ "$("$here/dup-index" hello)" = "hello" ]
if "$here/dup-index" >/dev/null 2>&1; then exit 1; fi
echo "dup-index: all tests passed"
