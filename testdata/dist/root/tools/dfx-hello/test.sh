#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
[ "$(./dfx-hello)" = "hello from dfx-hello world" ]
./dfx-hello --help | head -1 | grep -q '^Usage: dfx-hello'
echo "dfx-hello: ok"
