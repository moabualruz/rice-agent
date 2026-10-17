#!/usr/bin/env bash
# Tests for the g2-declared-distinct fixture.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ "$("$here/g2-declared-distinct" hello)" = "hello" ]
if "$here/g2-declared-distinct" >/dev/null 2>&1; then exit 1; fi
echo "g2-declared-distinct: all tests passed"
