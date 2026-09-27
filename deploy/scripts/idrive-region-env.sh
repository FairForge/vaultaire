#!/usr/bin/env bash
# idrive-region-env.sh — turn the reseller console's per-region key file into
# the IDRIVE_<REGION>_* lines Vaultaire reads (WP-R7-1).
#
# The reseller account mints one key pair per region and names them by city
# prefix (IDRIVE_LA_ACCESS_KEY, IDRIVE_LA_SECRET_KEY, IDRIVE_LA_ENDPOINT, …).
# Vaultaire keys regions by their id, taken from the endpoint host
# s3.<region>.idrivee2.com (IDRIVE_US_WEST_2_ACCESS_KEY, …). This script
# derives the region from each *_ENDPOINT line and prints the three lines per
# region. It prints secrets — redirect to the .env, never to a terminal you
# share. The primary pair (IDRIVE_ACCESS_KEY / IDRIVE_SECRET_KEY /
# IDRIVE_ENDPOINT / IDRIVE_REGION) is left as-is.
#
#   deploy/scripts/idrive-region-env.sh .private/idrive-keys-2026-09-20.env >> /opt/vaultaire/configs/.env
#
# A region is ENABLED by the presence of its pair: only add the regions you
# intend to sell. On the next boot Vaultaire registers `idrive-<region>`,
# creates the region's IDRIVE_BUCKET if absent, probes it, and offers it in
# the bucket-creation region picker.
set -euo pipefail

file="${1:-}"
if [[ -z "$file" || ! -r "$file" ]]; then
	echo "usage: $0 <reseller-keys.env>" >&2
	exit 2
fi

# prefix -> endpoint
while IFS='=' read -r key value; do
	[[ "$key" =~ ^IDRIVE_([A-Z0-9]+)_ENDPOINT$ ]] || continue
	prefix="${BASH_REMATCH[1]}"
	host="${value#https://}"
	host="${host%%/*}"
	[[ "$host" =~ ^s3\.([a-z0-9-]+)\.idrivee2\.com$ ]] || {
		echo "# skip $prefix: unexpected endpoint host $host" >&2
		continue
	}
	region="${BASH_REMATCH[1]}"
	envregion="$(echo "$region" | tr 'a-z-' 'A-Z_')"
	ak="$(grep -E "^IDRIVE_${prefix}_ACCESS_KEY=" "$file" | head -1 | cut -d= -f2-)"
	sk="$(grep -E "^IDRIVE_${prefix}_SECRET_KEY=" "$file" | head -1 | cut -d= -f2-)"
	if [[ -z "$ak" || -z "$sk" ]]; then
		echo "# skip $prefix ($region): incomplete pair" >&2
		continue
	fi
	echo "# iDrive $region (console prefix $prefix)"
	echo "IDRIVE_${envregion}_ACCESS_KEY=$ak"
	echo "IDRIVE_${envregion}_SECRET_KEY=$sk"
	echo "IDRIVE_${envregion}_ENDPOINT=https://$host"
done < "$file"
