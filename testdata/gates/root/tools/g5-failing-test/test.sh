#!/usr/bin/env bash
# Tests for the g5-failing-test fixture: the motivating case fails on purpose.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ "$("$here/g5-failing-test" hello)" != "goodbye" ]; then
  echo "FAIL motivating case: want goodbye"
  exit 1
fi
