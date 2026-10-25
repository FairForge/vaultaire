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
HEALTH='{"status":"ok","long_ops_in_flight":1,"chunk_puts_in_flight":3}'
expect "chunk PUTs that outlived their requests count too (Prompt 14)" 4 "$(long_ops_of 8001)"
HEALTH=""; UNIT=active
expect "a running slot that does not answer is unknown, not 0" "?" "$(long_ops_of 8001)"
HEALTH='<html>busy</html>'
expect "a running slot that answers garbage is unknown" "?" "$(long_ops_of 8001)"
HEALTH=""; UNIT=inactive
expect "a slot whose unit is not running has nothing left" 0 "$(long_ops_of 8001)"

# credential_guard (Stage 1a Prompt 42 PR C, plan 43.7): a build without the
# wrap's marker is refused while credential_plaintext_drop is on or unreadable.
eval "$(awk '/^credential_drop_on\(\) \{/,/^\}/' "$SCRIPT")"
eval "$(awk '/^credential_guard\(\) \{/,/^\}/' "$SCRIPT")"
CREDENTIAL_MARKER=$(sed -n "s/^CREDENTIAL_MARKER='\(.*\)'$/\1/p" "$SCRIPT")
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
ENV_FILE=$TMP/env; printf 'DB_NAME="vaultaire_lab"\n' > "$ENV_FILE"
printf 'ELF...%s...' "$CREDENTIAL_MARKER" > "$TMP/new"; printf 'ELF...an older build...' > "$TMP/old"
FLAG=f; PSQL_RC=0
runuser() { while [ "$1" != -- ]; do shift; done; shift; echo "$*" > "$TMP/psql-args"; [ "$PSQL_RC" = 0 ] && echo "$FLAG"; return "$PSQL_RC"; }
expect "the marker is the wrap's HKDF label" stored/credential/v1 "$CREDENTIAL_MARKER"
FLAG=t; expect "a build with the marker passes whatever the flag" 0 "$(credential_guard "$TMP/new" >/dev/null; echo $?)"
FLAG=f; expect "an older build passes while the flag is off" 0 "$(credential_guard "$TMP/old" >/dev/null; echo $?)"
FLAG=t; expect "an older build is refused while the flag is on" 1 "$(credential_guard "$TMP/old" >/dev/null; echo $?)"
case "$(credential_guard "$TMP/old")" in *"credential_plaintext_drop is on"*) pass "the refusal says why" ;; *) fail "the refusal says why" ;; esac
grep -q -- "-d vaultaire_lab" "$TMP/psql-args" && pass "the flag is read from DB_NAME's database" || fail "the flag is read from DB_NAME's database ($(cat "$TMP/psql-args"))"
PSQL_RC=2; expect "an older build is refused when the flag cannot be read (fail closed)" 1 "$(credential_guard "$TMP/old" >/dev/null; echo $?)"
PSQL_RC=0; FLAG=""; expect "an empty answer is not 'off'" 1 "$(credential_guard "$TMP/old" >/dev/null; echo $?)"
grep -q 'why=$(credential_guard "$BIN_DIR/vaultaire-$idle") || fail "$why"' "$SCRIPT" && pass "start_and_switch (deploy, restart, rollback) runs the guard before it starts a slot" || fail "start_and_switch runs the guard"

if [ "$FAILED" != 0 ]; then echo "FAILED"; exit 1; fi
echo "PASS"
