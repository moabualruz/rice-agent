#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
[ "$(./dfx-active)" = "hello from dfx-active world" ]
./dfx-active --help | head -1 | grep -q '^Usage: dfx-active'
echo "dfx-active: ok"
