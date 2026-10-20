#!/usr/bin/env bash
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
case "${1:-}" in
  '') ;;
  --native)
    [ "$#" -eq 1 ] || exit 2
    if [ -z "${CANONICAL_GATE_NATIVE_ROOT:-}" ]; then
      printf '%s\n' 'native proof requires CANONICAL_GATE_NATIVE_ROOT; refusing mock fallback' >&2
      exit 3
    fi
    export CANONICAL_GATE_TEST_NATIVE=1
    ;;
  *) exit 2 ;;
esac
exec node --test --test-reporter=tap "$here/test.mjs"
