#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
[ "$(./dfx-project)" = "hello from dfx-project world" ]
./dfx-project --help | head -1 | grep -q '^Usage: dfx-project'
echo "dfx-project: ok"
