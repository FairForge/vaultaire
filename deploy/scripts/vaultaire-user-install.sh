#!/bin/bash
# vaultaire-user-install.sh — the slots run as a dedicated system user
# (Stage 1a Prompt 52E PR E; F-14's code prerequisite: the master-key
# credential, LoadCredentialEncrypted=, is decrypted for this unit and read by
# this user only). docs/DEPLOY.md "The service user". Idempotent; run as root:
#
#   sudo deploy/scripts/vaultaire-user-install.sh             the cutover
#   sudo deploy/scripts/vaultaire-user-install.sh --rollback  back to user1
#
# then `sudo vaultaire-switch restart` twice (the active slot, then the other)
# — this script never starts or stops a slot. Between this script and the
# first restart the running user1 slot can no longer write data/: run it at a
# quiet hour and restart at once.
#
# What it does (the cutover; --rollback the same with user1):
#   - creates `vaultaire` (system user, own group, no shell, no home);
#   - adds user1 to that group: the deploy's migrations, pg-backup.sh and
#     health-check.sh read configs/.env as user1;
#   - data/ (multipart staging, the translog high-water file, the local
#     driver's tree) recursively, bin/ and the slot binaries in it — never
#     user1's scripts there — and the service's staging trees under /tmp
#     (the Sync stripes, the pack store, the default multipart dir) to it;
#   - configs/.env root:vaultaire 0640;
#   - User=/Group= of the installed vaultaire@.service, then daemon-reload.
# user1's own files — backups/, logs/, metrics/, the Sia user units in its
# home — are not touched. VAULTAIRE_ROOT prefixes every path (the tests).
set -euo pipefail

ROOT=${VAULTAIRE_ROOT:-}
OPT=$ROOT/opt/vaultaire
UNIT=$ROOT/etc/systemd/system/vaultaire@.service
SVC=vaultaire
OPERATOR=user1
STAGING_TMP="vaultaire-stripes vaultaire-packs vaultaire-multipart"

log() { echo "[vaultaire-user-install] $*"; }
die() { log "ERROR: $*" >&2; exit 1; }

case "${1:-}" in
  "") mode=install ;;
  --rollback) mode=rollback ;;
  *) die "usage: $0 [--rollback]" ;;
esac
[ -n "$ROOT" ] || [ "$(id -u)" = 0 ] || die "run as root"
[ -f "$UNIT" ] || die "$UNIT is not installed"

if [ "$mode" = install ]; then
  owner=$SVC
  if ! getent passwd "$SVC" >/dev/null; then
    useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$SVC"
    log "created the system user $SVC"
  fi
  if ! id -nG "$OPERATOR" | tr ' ' '\n' | grep -qx "$SVC"; then
    usermod -aG "$SVC" "$OPERATOR"
    log "$OPERATOR joined the group $SVC (reads configs/.env)"
  fi
else
  owner=$OPERATOR
fi

chown -R "$owner:$owner" "$OPT/data"
chown "$owner:$owner" "$OPT/bin"
for f in "$OPT"/bin/vaultaire-8000 "$OPT"/bin/vaultaire-8001 "$OPT"/bin/vaultaire-*.prev; do
  [ -f "$f" ] && [ ! -L "$f" ] && chown "$owner:$owner" "$f"
done
for d in $STAGING_TMP; do
  [ -d "$ROOT/tmp/$d" ] && chown -R "$owner:$owner" "$ROOT/tmp/$d"
done
chown "root:$owner" "$OPT/configs/.env"
chmod 0640 "$OPT/configs/.env"

sed -i.bak -e "s/^User=.*/User=$owner/" -e "s/^Group=.*/Group=$owner/" "$UNIT"
rm -f "$UNIT.bak"
systemctl daemon-reload

log "the slots will run as $owner: now run 'sudo vaultaire-switch restart' twice (the active slot, then the other)"
