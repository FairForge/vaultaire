#!/usr/bin/env bash
# indexd-backup.sh <instance> — daily backup of a self-hosted indexd
# (~/indexd-<instance>/): pg_dump of its database (custom format) and a copy of
# indexd.yml (it holds the recovery phrase, i.e. the wallet: 0600, never off the
# box unencrypted). consensus.db is not copied: `-instant` re-syncs it from a
# checkpoint in minutes. Keeps 7 days; on success writes
# vaultaire_sia_indexd_backup_last_success_timestamp_seconds{instance} to
# $SIA_METRICS_DIR (default /opt/vaultaire/metrics). The operations page asks
# for WAL archiving too on a production indexer — this is the lab's daily floor.
set -euo pipefail
INST=${1:?usage: indexd-backup.sh <instance>}
DIR=$HOME/indexd-$INST
OUT=$DIR/backups
METRICS_DIR=${SIA_METRICS_DIR:-/opt/vaultaire/metrics}
umask 077
mkdir -p "$OUT"
cfg() { python3 -I -c "
import re,sys
s=open(sys.argv[1]).read(); db=s.split('database:',1)[1]
print(re.search(r'\n\s+'+sys.argv[2]+r':\s*\"?([^\"\n]+)\"?', db).group(1))" "$DIR/indexd.yml" "$1"; }
TS=$(date -u +%Y%m%dT%H%M%SZ)
PGPASSWORD=$(cfg password) pg_dump -h "$(cfg host)" -p "$(cfg port)" -U "$(cfg user)" -d "$(cfg database)" -Fc -f "$OUT/indexd-$TS.dump"
pg_restore -l "$OUT/indexd-$TS.dump" > /dev/null    # the dump must list cleanly
cp "$DIR/indexd.yml" "$OUT/indexd-$TS.yml"
find "$OUT" -type f -mtime +7 -delete
tmp=$(mktemp "$METRICS_DIR/.indexd-backup.XXXXXX")
echo "vaultaire_sia_indexd_backup_last_success_timestamp_seconds{instance=\"$INST\"} $(date +%s)" > "$tmp"
chmod 644 "$tmp"
mv -f "$tmp" "$METRICS_DIR/vaultaire_sia_indexd_backup_$INST.prom"
echo "indexd $INST backed up: $OUT/indexd-$TS.dump ($(stat -c %s "$OUT/indexd-$TS.dump") bytes)"
