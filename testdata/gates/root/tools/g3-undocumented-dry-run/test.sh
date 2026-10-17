#!/usr/bin/env bash
# Tests for the g3-undocumented-dry-run fixture.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ "$("$here/g3-undocumented-dry-run" hello)" = "hello" ]
if "$here/g3-undocumented-dry-run" >/dev/null 2>&1; then exit 1; fi
echo "g3-undocumented-dry-run: all tests passed"
