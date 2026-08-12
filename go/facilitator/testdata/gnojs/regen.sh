#!/usr/bin/env bash
# Regenerates the JS-signed interop fixture. The fixture is committed so the Go
# test needs no toolchain; this script is only for bumping the SDK or changing
# what the buyer signs.
set -o errexit
set -o nounset
set -o pipefail

readonly HERE="${BASH_SOURCE[0]%/*}"
cd "${HERE}"

if ! command -v npm > /dev/null; then
  echo "Error: npm is required to regenerate the interop fixture" >&2
  exit 1
fi

npm install --silent --no-fund --no-audit
node sign.mjs > ../gnojs_signed_send.b64
