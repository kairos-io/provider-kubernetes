#!/usr/bin/env bash
# parse-sandbox-image.sh <config.toml>
#
# Runs parse_sandbox_image from scripts/verify-bundle-refs.sh on one file, so
# TestParseSandboxImageMatchesVerifyScript can hold the CI script's sandbox_image
# rule to the same results as test/e2e/bundled_images.go parseSandboxImage.
# Sourcing the script only defines its functions; it returns before its checks.
set -euo pipefail
if [ "$#" -ne 1 ]; then
  echo "usage: $0 <config.toml>" >&2
  exit 2
fi
# shellcheck source=../../../scripts/verify-bundle-refs.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../../scripts/verify-bundle-refs.sh"
parse_sandbox_image "$1"
