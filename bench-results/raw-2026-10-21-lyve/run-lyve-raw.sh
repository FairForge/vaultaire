#!/usr/bin/env bash
set -u
cd ~/vaultaire-bench/lyve-2026-10-20
set -a; . ~/vaultaire-bench/.env.bench >/dev/null 2>&1; set +a
log() { echo "[$(date -u +%H:%M:%S)] $*"; }
log "== lyve raw: us-west-1, every workload"
./bench-compare-linux -only lyve-us-west-1 -host slc -out raw-usw1.json > raw-usw1.log 2>&1
log "== lyve raw: the other six regions, every workload but sustained"
./bench-compare-linux -only lyve-us-east-1,lyve-us-central-2,lyve-eu-west-1,lyve-eu-central-1,lyve-ap-southeast-1,lyve-ap-northeast-1 -skip-workload sustained -host slc -out raw-rest.json > raw-rest.log 2>&1
log "== done"
