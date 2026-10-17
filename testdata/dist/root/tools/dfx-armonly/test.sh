#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
[ "$(./dfx-armonly)" = "hello from dfx-armonly world" ]
./dfx-armonly --help | head -1 | grep -q '^Usage: dfx-armonly'
echo "dfx-armonly: ok"
