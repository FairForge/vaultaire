#!/bin/bash
# vaultaire-switch_test.sh — tests for deploy/scripts/vaultaire-switch's long-op
# poll. Run from anywhere: bash deploy/scripts/vaultaire-switch_test.sh
# CI runs it (ci.yml "Deploy scripts"). The script itself acts on the box when
# run, so the functions under test are extracted, not sourced.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=$HERE/vaultaire-switch
FAILED=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; FAILED=1; }
expect() { # expect MESSAGE WANT GOT
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (want '$2', got '$3')"; fi
}

if command -v shellcheck >/dev/null; then
  # Warnings and errors: the script predates this test and carries style notes.
  if shellcheck -S warning "$SCRIPT" "$HERE/vaultaire-switch_test.sh"; then pass "shellcheck clean"; else fail "shellcheck"; fi
else
  fail "shellcheck is not installed"
fi

# long_ops_of, with curl and systemctl stubbed.
eval "$(awk '/^long_ops_of\(\) \{/,/^\}/' "$SCRIPT")"
HEALTH=""     # what /health answers ("" = no answer)
UNIT=active   # what systemctl is-active says
curl() { [ -n "$HEALTH" ] && printf '%s' "$HEALTH"; [ -n "$HEALTH" ]; }
systemctl() { [ "$1" = is-active ] && [ "$UNIT" = active ]; }

HEALTH='{"status":"ok","long_ops_in_flight":2}'
expect "a slot that answers reports its count" 2 "$(long_ops_of 8001)"
HEALTH='{"status":"ok","long_ops_in_flight":0}'
expect "a slot with none reports 0" 0 "$(long_ops_of 8001)"
HEALTH=""; UNIT=active
expect "a running slot that does not answer is unknown, not 0" "?" "$(long_ops_of 8001)"
HEALTH='<html>busy</html>'
expect "a running slot that answers garbage is unknown" "?" "$(long_ops_of 8001)"
HEALTH=""; UNIT=inactive
expect "a slot whose unit is not running has nothing left" 0 "$(long_ops_of 8001)"

if [ "$FAILED" != 0 ]; then echo "FAILED"; exit 1; fi
echo "PASS"
