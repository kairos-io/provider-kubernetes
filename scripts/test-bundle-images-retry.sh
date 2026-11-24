#!/bin/sh
# Unit test for the retry helper in build/bundle-images.sh. The helper is cut out
# of the script between its BEGIN/END retry markers and sourced, so the test runs
# the shipped code without a registry, crane or cosign.
#
# Usage:
#   scripts/test-bundle-images-retry.sh [path/to/bundle-images.sh]
set -eu

SCRIPT="${1:-$(dirname "$0")/../build/bundle-images.sh}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

sed -n '/^# BEGIN retry/,/^# END retry$/p' "$SCRIPT" > "$TMP/retry.sh"
if ! grep -q '^retry()' "$TMP/retry.sh"; then
  echo "FAIL: no retry() between the BEGIN/END retry markers in $SCRIPT" >&2
  exit 1
fi
# shellcheck source=/dev/null
. "$TMP/retry.sh"

fails=0
fail() { echo "FAIL: $*" >&2; fails=$((fails + 1)); }

# sleep is shadowed so the test is instant and the delays can be checked.
sleep() { printf '%s ' "$1" >> "$TMP/sleeps"; }

# flaky <n>: fail the first <n> calls, then print a digest.
flaky() {
  c=$(($(cat "$TMP/count" 2>/dev/null || echo 0) + 1))
  echo "$c" > "$TMP/count"
  if [ "$c" -le "$1" ]; then
    echo "TOOMANYREQUESTS: Quota exceeded" >&2
    return 1
  fi
  echo "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
}

# always_fail: count calls and fail with a distinct status.
always_fail() {
  c=$(($(cat "$TMP/count" 2>/dev/null || echo 0) + 1))
  echo "$c" > "$TMP/count"
  return 7
}

reset() { rm -f "$TMP/count" "$TMP/sleeps"; }
calls() { cat "$TMP/count" 2>/dev/null || echo 0; }
sleeps() { cat "$TMP/sleeps" 2>/dev/null || true; }

# 1. Two failures then success: retried, succeeds, stdout is only the digest.
reset
rc=0
out="$(retry flaky 2 2>"$TMP/err")" || rc=$?
[ "$rc" -eq 0 ] || fail "flaky 2: exit $rc, want 0"
[ "$(calls)" = 3 ] || fail "flaky 2: $(calls) calls, want 3"
[ "$out" = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" ] \
  || fail "flaky 2: stdout '$out', want only the digest"
[ "$(grep -c '^retry: ' "$TMP/err")" = 2 ] || fail "flaky 2: want 2 retry notices on stderr"
[ "$(sleeps)" = "5 15 " ] || fail "flaky 2: sleeps '$(sleeps)', want '5 15 '"

# 2. Always failing: stops after 5 attempts with the command's own exit status.
reset
rc=0
retry always_fail 2>/dev/null || rc=$?
[ "$rc" -eq 7 ] || fail "always_fail: exit $rc, want 7"
[ "$(calls)" = 5 ] || fail "always_fail: $(calls) calls, want 5"
[ "$(sleeps)" = "5 15 30 60 " ] || fail "always_fail: sleeps '$(sleeps)', want '5 15 30 60 '"

# 3. First try succeeds: no retry, no sleep, no notice.
reset
rc=0
out="$(retry flaky 0 2>"$TMP/err")" || rc=$?
if [ "$rc" -ne 0 ] || [ "$(calls)" != 1 ]; then fail "flaky 0: want one call and exit 0"; fi
if [ -s "$TMP/err" ] || [ -n "$(sleeps)" ]; then fail "flaky 0: want no notice and no sleep"; fi

# 4. BUNDLE_RETRY_DELAY_SCALE scales every delay; 0 means no wait.
reset
rc=0
BUNDLE_RETRY_DELAY_SCALE=0 retry always_fail 2>/dev/null || rc=$?
[ "$(sleeps)" = "0 0 0 0 " ] || fail "scale 0: sleeps '$(sleeps)', want '0 0 0 0 '"
reset
rc=0
BUNDLE_RETRY_DELAY_SCALE=x retry flaky 0 >/dev/null 2>&1 || rc=$?
if [ "$rc" -eq 0 ] || [ "$(calls)" != 0 ]; then fail "scale x: want non-zero exit before running the command"; fi

# 5. Under set -e a failing digest capture still stops the script (fail-closed).
rc=0
out="$(BUNDLE_RETRY_DELAY_SCALE=0 sh -c 'set -eu; . "$1"; d="$(retry false)"; echo reached' sh "$TMP/retry.sh" 2>/dev/null)" || rc=$?
if [ "$rc" -eq 0 ] || [ -n "$out" ]; then fail "set -e: script went on after a failed retry (exit $rc, out '$out')"; fi

if [ "$fails" -ne 0 ]; then
  echo "$fails check(s) failed" >&2
  exit 1
fi
echo "ok: bundle-images.sh retry helper"
