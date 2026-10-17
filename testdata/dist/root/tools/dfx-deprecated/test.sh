#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
[ "$(./dfx-deprecated)" = "hello from dfx-deprecated world" ]
./dfx-deprecated --help | head -1 | grep -q '^Usage: dfx-deprecated'
echo "dfx-deprecated: ok"
