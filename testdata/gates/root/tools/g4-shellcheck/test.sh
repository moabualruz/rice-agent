#!/usr/bin/env bash
# Tests for the g4-shellcheck fixture.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ "$("$here/g4-shellcheck" hello)" = "hello" ]
if "$here/g4-shellcheck" >/dev/null 2>&1; then exit 1; fi
echo "g4-shellcheck: all tests passed"
