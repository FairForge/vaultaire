#!/usr/bin/env bash
# Origin lockdown: allow 80/443 ONLY from Cloudflare's published edge ranges.
#
# DO NOT RUN until s3.stored.ge is proxied (orange-cloud) AND the plan tier's
# proxy limits (Free/Pro: 100 MB per request body, 100 s origin response) are
# confirmed acceptable — today s3.stored.ge is DNS-only on purpose so >100 MB
# single PUTs can hit the origin directly. Locking the origin while that record
# is grey-cloud takes the S3 API down for everyone.
#
# What it does (idempotent, re-runnable — Cloudflare re-publishes ranges):
#   1. fetch https://www.cloudflare.com/ips-v4 and ips-v6
#   2. insert `ufw allow from <range> to any port 80,443 proto tcp` for each
#   3. delete the catch-all `allow 80/tcp` + `allow 443/tcp` (v4 + v6)
#   4. leave 22/tcp untouched
# Rollback: `sudo ufw allow 80/tcp && sudo ufw allow 443/tcp` restores the
# open state instantly; the per-range rules can stay.
#
# Verification after apply (from a NON-Cloudflare IP, e.g. the Mac):
#   curl -m 5 -sk https://38.147.105.54/health   → must TIME OUT
#   curl -s https://stored.ge/health              → 200 (via Cloudflare)
#   curl -s https://s3.stored.ge/health           → 200 only if proxied
#
# Snapshot of the ranges on 2026-09-24 (14 v4 + 6 v6) is in
# .private/CLOUDFLARE_READINESS_2026-09-23.md; the script always fetches live.
set -euo pipefail

if [[ "${1:-}" != "--apply" ]]; then
  echo "dry run — pass --apply to change the firewall" >&2
  DRY=1
else
  DRY=0
fi

run() { if [[ $DRY -eq 1 ]]; then echo "+ $*"; else "$@"; fi; }

v4=$(curl -fsS https://www.cloudflare.com/ips-v4)
v6=$(curl -fsS https://www.cloudflare.com/ips-v6)
[[ $(wc -l <<<"$v4") -ge 10 && $(wc -l <<<"$v6") -ge 4 ]] || { echo "range lists look truncated, aborting" >&2; exit 1; }

for r in $v4 $v6; do
  run ufw allow proto tcp from "$r" to any port 80,443 comment "cloudflare edge"
done

# Catch-alls last, only after the allow-list exists (no window without access).
run ufw delete allow 80/tcp
run ufw delete allow 443/tcp

run ufw status numbered
