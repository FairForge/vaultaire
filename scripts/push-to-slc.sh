#!/usr/bin/env bash
# Copy a large file to the SLC box reliably.
#
# Why this exists: plain scp/rsync of a ~38 MB binary to vaultaire-slc fails
# repeatedly. The link is clean (0% loss, ~64 ms RTT) but SSH's per-channel
# window throttles a single long stream to ~2.7 MB/s, and long transfers get
# torn down partway. Splitting into short-lived 4 MB pieces sidesteps it: each
# chunk finishes before anything drops, and a failed chunk retries on its own
# instead of restarting the whole file.
#
# Usage: scripts/push-to-slc.sh <local-file> <remote-path> [ssh-host]
set -euo pipefail

SRC=${1:?usage: push-to-slc.sh <local-file> <remote-path> [ssh-host]}
DEST=${2:?usage: push-to-slc.sh <local-file> <remote-path> [ssh-host]}
HOST=${3:-vaultaire-slc}
CHUNK=${CHUNK_SIZE:-4m}

[[ -f "$SRC" ]] || { echo "no such file: $SRC" >&2; exit 1; }

# A quoted "~/x" never expands on the remote side. ssh starts in $HOME, so
# strip the prefix and let the relative path resolve there instead.
DEST=${DEST#\~/}

size=$(wc -c < "$SRC" | tr -d ' ')
sum=$(shasum -a 256 "$SRC" | awk '{print $1}')
stage="/tmp/push-$$-$(basename "$SRC")"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

split -b "$CHUNK" "$SRC" "$work/part-"
total=$(find "$work" -name 'part-*' | wc -l | tr -d ' ')
echo "pushing $(basename "$SRC") — $size bytes in $total chunks"

ssh -o ConnectTimeout=15 "$HOST" "rm -rf '$stage' && mkdir -p '$stage'"

n=0
for chunk in "$work"/part-*; do
    name=$(basename "$chunk")
    want=$(wc -c < "$chunk" | tr -d ' ')
    n=$((n + 1))
    for attempt in 1 2 3 4 5; do
        if scp -q -o ConnectTimeout=15 "$chunk" "$HOST:$stage/$name" 2>/dev/null; then
            got=$(ssh -o ConnectTimeout=10 "$HOST" "stat -c%s '$stage/$name' 2>/dev/null || echo 0")
            [[ "$got" == "$want" ]] && break
        fi
        [[ $attempt == 5 ]] && { echo "chunk $name failed after 5 attempts" >&2; exit 1; }
        sleep 2
    done
    printf '\r  %d/%d' "$n" "$total"
done
echo

# Reassemble and verify end to end — a per-chunk size match is not enough.
ssh -o ConnectTimeout=20 "$HOST" "
    set -e
    cat '$stage'/part-* > '$DEST'
    rm -rf '$stage'
    got=\$(sha256sum '$DEST' | awk '{print \$1}')
    if [ \"\$got\" != '$sum' ]; then
        echo \"checksum mismatch: \$got != $sum\" >&2
        exit 1
    fi
    echo \"delivered \$(stat -c%s '$DEST') bytes, sha256 verified\"
"
