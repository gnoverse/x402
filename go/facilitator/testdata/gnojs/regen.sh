#!/usr/bin/env bash
# Regenerates the JS-signed interop fixture. The fixture is committed so the Go
# test needs no toolchain; this script is only for bumping the SDK or changing
# what the buyer signs.
set -o errexit
set -o nounset
set -o pipefail

# %/* strips the last path segment, which yields nothing to cd to when the script
# was named without one — `bash regen.sh` from this directory. cd to the script's
# own directory whichever way it was invoked.
HERE="${BASH_SOURCE[0]%/*}"
[[ "${HERE}" == "${BASH_SOURCE[0]}" ]] && HERE="."
readonly HERE
cd "${HERE}"

if ! command -v npm > /dev/null; then
  echo "Error: npm is required to regenerate the interop fixture" >&2
  exit 1
fi

npm install --silent --no-fund --no-audit

# Written aside and moved into place: redirecting straight at the fixture
# truncates it before node runs, so a failure here would leave an empty fixture
# where a committed one was.
readonly FIXTURE="../gnojs_signed_send.b64"
tmp="$(mktemp)"
trap 'rm -f "${tmp}"' EXIT
node sign.mjs > "${tmp}"
mv "${tmp}" "${FIXTURE}"
