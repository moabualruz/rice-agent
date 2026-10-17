#!/usr/bin/env bash
# Tests for the good-tool fixture.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ "$("$here/good-tool" hello)" = "hello" ]
if "$here/good-tool" >/dev/null 2>&1; then exit 1; fi
echo "good-tool: all tests passed"
