#!/usr/bin/env bash
set -euo pipefail
here="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$here"
env -u GOFLAGS go test .
