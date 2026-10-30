#!/bin/bash
# vaultaire-maint_test.sh — tests for deploy/scripts/vaultaire-maint's lock.
# Run from anywhere: bash deploy/scripts/vaultaire-maint_test.sh (needs flock(1):
# util-linux — CI's runner and the box have it, macOS does not).
# CI runs it (ci.yml "Deploy scripts"). The lock and its note point into a temp
# directory; upgrade and reboot act on the box and are not run here.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=$HERE/vaultaire-maint
FAILED=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; FAILED=1; }
expect() { # expect MESSAGE WANT GOT
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (want '$2', got '$3')"; fi
}

if command -v shellcheck >/dev/null; then
  if shellcheck -S warning "$SCRIPT" "$HERE/vaultaire-maint_test.sh"; then pass "shellcheck clean"; else fail "shellcheck"; fi
else
  fail "shellcheck is not installed"
fi
command -v flock >/dev/null || { echo "FAIL flock(1) is not installed"; exit 1; }

TMP=$(mktemp -d)
BG=""
cleanup() { [ -n "$BG" ] && kill "$BG" 2>/dev/null; rm -rf "$TMP"; }
trap cleanup EXIT
export VAULTAIRE_MAINT_LOCK=$TMP/switch.lock VAULTAIRE_MAINT_HOLDER=$TMP/maint.holder
maint() { bash "$SCRIPT" "$@"; }

# No lock file yet (nothing took it since boot): free.
expect "no lock file is free" "free" "$(maint status)"
rc=0; maint status >/dev/null || rc=$?
expect "status exits 0 when free" 0 "$rc"

# run holds the lock for its command and writes the note the next operator reads.
inside=$(maint run "test reason" -- bash "$SCRIPT" status || true)
expect "the lock is held while the command runs" "held" "$(printf '%s\n' "$inside" | head -1)"
case "$inside" in *"reason=test reason"*) pass "the note names the reason";; *) fail "the note names the reason ($inside)";; esac
expect "the note is removed after the run" "no" "$([ -e "$TMP/maint.holder" ] && echo yes || echo no)"
expect "the lock is free after the run" "free" "$(maint status)"

# The command's exit status is the run's.
rc=0; maint run "failing" -- sh -c 'exit 3' >/dev/null 2>&1 || rc=$?
expect "the command's exit status is propagated" 3 "$rc"

# Held by someone else (vaultaire-switch's own flock): refused at once, holder shown.
flock -o "$VAULTAIRE_MAINT_LOCK" sleep 30 & BG=$!
for _ in $(seq 50); do maint status >/dev/null 2>&1 || break; sleep 0.1; done
rc=0; out=$(maint run "second" -- true 2>&1) || rc=$?
expect "a held lock refuses the run" 1 "$rc"
case "$out" in *"vaultaire-switch"*) pass "no note = vaultaire-switch holds it";; *) fail "holder named ($out)";; esac
rc=0; maint status >/dev/null || rc=$?
expect "status exits 1 when held" 1 "$rc"
kill "$BG" 2>/dev/null; wait "$BG" 2>/dev/null || true; BG=""

# A daemon the command starts never keeps the lock (flock -o).
maint run "spawns a child" -- sh -c "sleep 30 >/dev/null 2>&1 & echo \$! > '$TMP/child'"
expect "a background child does not hold the lock" "free" "$(maint status)"
kill "$(cat "$TMP/child")" 2>/dev/null || true

# Wrapping vaultaire-switch would deadlock on the same lock: refused.
rc=0; maint run "deploy" -- /usr/local/sbin/vaultaire-switch restart >/dev/null 2>&1 || rc=$?
expect "vaultaire-switch is refused inside run" 1 "$rc"

# A note left by a killed run, with the lock free, is reported as stale.
printf 'by=x\nreason=old\n' > "$TMP/maint.holder"
case "$(maint status)" in "free"*stale*) pass "a leftover note is reported stale";; *) fail "stale note";; esac
rm -f "$TMP/maint.holder"

# Usage errors.
rc=0; maint run "no separator" true >/dev/null 2>&1 || rc=$?
expect "run without -- is a usage error" 2 "$rc"
rc=0; maint run "" -- true >/dev/null 2>&1 || rc=$?
expect "an empty reason is refused" 1 "$rc"

[ "$FAILED" = 0 ] && echo "PASS" || { echo "FAILED"; exit 1; }
