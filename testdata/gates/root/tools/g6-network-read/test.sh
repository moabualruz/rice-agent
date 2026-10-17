#!/usr/bin/env bash
# Tests for the g6-network-read fixture.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ "$("$here/g6-network-read" hello)" = "hello" ]
if "$here/g6-network-read" >/dev/null 2>&1; then exit 1; fi
echo "g6-network-read: all tests passed"
