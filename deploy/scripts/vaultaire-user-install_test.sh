#!/bin/bash
# vaultaire-user-install_test.sh — tests for deploy/scripts/vaultaire-user-install.sh
# (Stage 1a Prompt 52E PR E). Run from anywhere:
#   bash deploy/scripts/vaultaire-user-install_test.sh
# CI runs it beside vaultaire-switch_test.sh. The script acts on the box, so
# it runs here under VAULTAIRE_ROOT (a temporary tree) with the system
# commands it calls stubbed: each stub records its arguments.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=$HERE/vaultaire-user-install.sh
FAILED=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; FAILED=1; }
has() { if grep -qF -- "$2" "$CALLS"; then pass "$1"; else fail "$1 (no '$2' in: $(tr '\n' '|' < "$CALLS"))"; fi; }
hasnt() { if grep -qF -- "$2" "$CALLS"; then fail "$1 ('$2' called)"; else pass "$1"; fi; }

if command -v shellcheck >/dev/null; then
  if shellcheck -S warning "$SCRIPT" "$HERE/vaultaire-user-install_test.sh"; then pass "shellcheck clean"; else fail "shellcheck"; fi
else
  fail "shellcheck is not installed"
fi

TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
ROOT=$TMP/root
CALLS=$TMP/calls
STUBS=$TMP/bin
mkdir -p "$STUBS" "$ROOT/opt/vaultaire/data/multipart" "$ROOT/opt/vaultaire/bin" "$ROOT/opt/vaultaire/configs" \
  "$ROOT/etc/systemd/system" "$ROOT/tmp/vaultaire-stripes" "$ROOT/tmp/vaultaire-packs" "$ROOT/tmp/vaultaire-data"
touch "$ROOT/opt/vaultaire/bin/vaultaire-8000" "$ROOT/opt/vaultaire/bin/vaultaire-8001" "$ROOT/opt/vaultaire/bin/pg-backup.sh" \
  "$ROOT/opt/vaultaire/configs/.env"
printf '[Service]\nUser=user1\nGroup=user1\nWorkingDirectory=/opt/vaultaire\n' > "$ROOT/etc/systemd/system/vaultaire@.service"

# The stubs: the user exists once useradd ran; user1 is in the group once
# usermod ran.
for c in useradd usermod chown chmod systemctl; do
  printf '#!/bin/bash\necho "%s $*" >> "%s"\n' "$c" "$CALLS" > "$STUBS/$c"
done
cat >> "$STUBS/useradd" <<EOF
touch "$TMP/user-exists"
EOF
cat >> "$STUBS/usermod" <<EOF
touch "$TMP/user1-in-group"
EOF
cat > "$STUBS/getent" <<EOF
#!/bin/bash
[ "\$1 \$2" = "passwd vaultaire" ] && [ -f "$TMP/user-exists" ]
EOF
cat > "$STUBS/id" <<EOF
#!/bin/bash
if [ "\$1" = -nG ]; then if [ -f "$TMP/user1-in-group" ]; then echo "user1 sudo vaultaire"; else echo "user1 sudo"; fi; else echo 0; fi
EOF
chmod +x "$STUBS"/*

run() { : > "$CALLS"; PATH="$STUBS:$PATH" VAULTAIRE_ROOT="$ROOT" bash "$SCRIPT" "$@" > "$TMP/out" 2>&1; }

# The cutover.
run || fail "the install runs ($(cat "$TMP/out"))"
has "creates a system user with no shell and no home" "useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin vaultaire"
has "user1 joins the group (the deploy's migrations and pg-backup.sh read .env)" "usermod -aG vaultaire user1"
has "data/ is the service's" "chown -R vaultaire:vaultaire $ROOT/opt/vaultaire/data"
has "the binaries' directory is the service's" "chown vaultaire:vaultaire $ROOT/opt/vaultaire/bin"
has "the slot binaries are the service's" "chown vaultaire:vaultaire $ROOT/opt/vaultaire/bin/vaultaire-8000"
hasnt "user1's scripts in bin/ stay user1's" "bin/pg-backup.sh"
has ".env is root:vaultaire" "chown root:vaultaire $ROOT/opt/vaultaire/configs/.env"
has ".env is 0640" "chmod 0640 $ROOT/opt/vaultaire/configs/.env"
has "the Sync stripe staging (in use under /tmp) is the service's" "chown -R vaultaire:vaultaire $ROOT/tmp/vaultaire-stripes"
has "the pack staging is the service's" "chown -R vaultaire:vaultaire $ROOT/tmp/vaultaire-packs"
hasnt "a lab's /tmp tree is left alone" "vaultaire-data"
hasnt "user1's home (the Sia user units) is never touched" "/home/"
grep -q '^User=vaultaire$' "$ROOT/etc/systemd/system/vaultaire@.service" && pass "the unit runs as vaultaire" || fail "the unit runs as vaultaire"
grep -q '^Group=vaultaire$' "$ROOT/etc/systemd/system/vaultaire@.service" && pass "the unit's group is vaultaire" || fail "the unit's group is vaultaire"
grep -q '^WorkingDirectory=/opt/vaultaire$' "$ROOT/etc/systemd/system/vaultaire@.service" && pass "the rest of the unit is kept" || fail "the rest of the unit is kept"
has "systemd re-reads the unit" "systemctl daemon-reload"
hasnt "the script never restarts a slot itself (vaultaire-switch does)" "systemctl restart"
grep -q "vaultaire-switch restart" "$TMP/out" && pass "it says what to run next" || fail "it says what to run next"

# Idempotent: a second run creates nothing again.
run || fail "a second run ($(cat "$TMP/out"))"
hasnt "the user is not created twice" "useradd"
hasnt "user1 is not added twice" "usermod"
has "ownership is re-asserted" "chown -R vaultaire:vaultaire $ROOT/opt/vaultaire/data"

# The rollback.
run --rollback || fail "the rollback runs ($(cat "$TMP/out"))"
grep -q '^User=user1$' "$ROOT/etc/systemd/system/vaultaire@.service" && pass "rollback: the unit runs as user1" || fail "rollback: the unit runs as user1"
has "rollback: data/ is user1's" "chown -R user1:user1 $ROOT/opt/vaultaire/data"
has "rollback: bin/ is user1's" "chown user1:user1 $ROOT/opt/vaultaire/bin"
has "rollback: .env is root:user1" "chown root:user1 $ROOT/opt/vaultaire/configs/.env"
has "rollback: systemd re-reads the unit" "systemctl daemon-reload"

run --bogus && fail "an unknown argument is refused" || pass "an unknown argument is refused"

if [ "$FAILED" != 0 ]; then echo "FAILED"; exit 1; fi
echo "PASS"
