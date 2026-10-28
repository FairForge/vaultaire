#!/usr/bin/env bash
# The lab proof of the mirror at the edge (WP-EDGE-4 Part A, Stage 1a Prompt
# 48; docs/reviews/WP-EDGE-4.md § The lab proof). One process of the binary
# built from this checkout: the control plane on 127.0.0.1:8000 (a private
# migrated DB, a LAB master key, EDGE_SERVICE_TOKEN) on the bench iDrive
# us-central-1 pair as the primary and the bench Lyve pair as the mirror leg
# (MIRROR_LEGS=lyve). The writes go through host=slc (data_plane_slc ON): the
# control plane's own Go data-plane host, the same pipeline as a node's —
# the tee into two PUTs, the commit's copy rows. Each leg is then asked
# directly (aws-cli with the bench pair), never through the control plane.
#
#   deploy/edge/mirror-proof.sh up              # the control plane and a lab tenant (edge_ingest, data_plane_slc, edge_mirror)
#   deploy/edge/mirror-proof.sh run <case>      # whole | fallback | backfill | delete (no master key), codec (LAB_KEY=1)
#   LAB_KEY=1 deploy/edge/mirror-proof.sh restart  # the control plane again, with (LAB_KEY=1) or without the lab master key
#
# With a master key the origin seals a plain Standard PUT, and a sealed
# write is the origin's, never the mover's: the mover's cases run without
# it, the codec's with it (LAB_KEY=1 restart).
#   deploy/edge/mirror-proof.sh down [--purge]  # stops it; --purge deletes the lab's blobs on both legs and drops the DB
#
# Secrets are never printed: the bench pairs are read from .env.bench in the
# main checkout, the master key and the token from files under $LAB (0600).
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
LAB=${LAB:?set LAB to a private scratch directory (chmod 700)}
BENCH_ENV=${BENCH_ENV:-$HOME/fairforge/vaultaire/.env.bench}
DB=${LAB_DB:-vaultaire_mirrorlab}
LYVE_REGION=${LAB_LYVE_REGION:-us-west-1}
CP_PORT=8000
BUCKET=mirrorlab
export AWS_CONFIG_FILE=$LAB/aws.cfg AWS_SHARED_CREDENTIALS_FILE=$LAB/aws.creds

aws_cp() { aws --endpoint-url http://127.0.0.1:$CP_PORT --region us-east-1 "$@"; }
sql() { psql -X -q -At -h localhost -d "$DB" -c "$1"; }
tenant() { sql "SELECT tenant_id FROM api_keys WHERE key_id = '$(awk -F' = ' '/aws_access_key_id/ {print $2}' "$AWS_SHARED_CREDENTIALS_FILE")'"; }
metric() { curl -s http://127.0.0.1:$CP_PORT/metrics | grep -E "^$1" || true; }
bench() { bash -c "source '$BENCH_ENV' >/dev/null 2>&1; printf %s \"\$$1\""; }
md5of() { md5 -q "$1" 2>/dev/null || md5sum "$1" | cut -d' ' -f1; }
golden() { (cd "$repo" && go run ./edge/s3-ingest/lab/goldenbody.go "$1" "$2" "$3"); }

# leg <idrive|lyve> <aws args…>: aws-cli on a leg itself, with the bench pair.
leg() {
	local which=$1
	shift
	case $which in
	idrive) AWS_ACCESS_KEY_ID=$(bench IDRIVE_US_CENTRAL_1_ACCESS_KEY) AWS_SECRET_ACCESS_KEY=$(bench IDRIVE_US_CENTRAL_1_SECRET_KEY) \
		AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null aws --endpoint-url https://s3.us-central-1.idrivee2.com --region us-central-1 "$@" ;;
	lyve) AWS_ACCESS_KEY_ID=$(bench LYVE_ACCESS_KEY) AWS_SECRET_ACCESS_KEY=$(bench LYVE_SECRET_KEY) \
		AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null aws --endpoint-url "https://s3.$LYVE_REGION.global.lyve.seagate.com" --region "$LYVE_REGION" "$@" ;;
	esac
}
leg_bucket() { case $1 in idrive) echo vaultaire ;; lyve) echo "stored-$LYVE_REGION" ;; esac; }
# on_leg <leg> <key>: "yes <size>" when the leg holds the blob of an object, else "no".
on_leg() {
	local sk t
	t=$(tenant)
	sk=$(sql "SELECT COALESCE(storage_key, object_key) FROM object_head_cache WHERE tenant_id = '$t' AND object_key = '$2'")
	[ -n "$sk" ] || sk=$(cat "$LAB/last.sk" 2>/dev/null || true)
	if out=$(leg "$1" s3api head-object --bucket "$(leg_bucket "$1")" --key "t-$t/${t}_$BUCKET/$sk" --query ContentLength --output text 2>/dev/null); then
		echo "yes $out"
	else
		echo "no"
	fi
}

start_cp() {
	mkdir -p "$LAB/data"
	env -i PATH="$PATH" HOME="$HOME" PORT=$CP_PORT DB_HOST=localhost DB_PORT=5432 DB_NAME="$DB" DB_USER="$(whoami)" DB_PASSWORD= \
		DATA_PATH="$LAB/data" JWT_SECRET=mirror-lab STORAGE_MODE=idrive \
		IDRIVE_ACCESS_KEY="$(bench IDRIVE_US_CENTRAL_1_ACCESS_KEY)" IDRIVE_SECRET_KEY="$(bench IDRIVE_US_CENTRAL_1_SECRET_KEY)" \
		IDRIVE_REGION=us-central-1 IDRIVE_BUCKET=vaultaire \
		LYVE_ACCESS_KEY="$(bench LYVE_ACCESS_KEY)" LYVE_SECRET_KEY="$(bench LYVE_SECRET_KEY)" LYVE_REGION="$LYVE_REGION" \
		MIRROR_LEGS=lyve ${LAB_KEY:+ENCRYPTION_MASTER_KEY_FILE="$LAB/master.key"} EDGE_SERVICE_TOKEN_FILE="$LAB/edge.token" EDGE_MIN_BYTES=4MiB \
		nohup "$LAB/vaultaire-lab" </dev/null >"$LAB/cp.log" 2>&1 &
	echo $! >"$LAB/cp.pid"
	for _ in $(seq 60); do curl -s http://127.0.0.1:$CP_PORT/health | grep -q '"status"' && return 0; sleep 1; done
	echo "control plane did not come up (see $LAB/cp.log)" >&2
	exit 1
}

jwt() {
	curl -s -X POST http://127.0.0.1:$CP_PORT/auth/login -H 'Content-Type: application/json' -d "$(cat "$LAB/admin.json")" |
		python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])'
}

flag() { # flag <name> <true|false>
	curl -s -o /dev/null -w "flag $1=$2: %{http_code}\n" -X PUT "http://127.0.0.1:$CP_PORT/api/v1/admin/flags/$1" \
		-H "Authorization: Bearer $(jwt)" -H 'Content-Type: application/json' -d "{\"enabled\":$2,\"tenant_id\":\"$(tenant)\"}"
}

copies() { # the copy rows of an object (or of the tenant's chunks with "chunks")
	if [ "$1" = chunks ]; then
		sql "SELECT 'copies: chunks on ' || leg || ' ' || state || ' ' || COUNT(*) FROM blob_copies WHERE tenant_id = '$(tenant)' AND bucket = '' GROUP BY leg, state"
	else
		sql "SELECT 'copies: ' || object_key || ' on ' || leg || ' ' || state || ' ' || size || ' ' || etag FROM blob_copies WHERE tenant_id = '$(tenant)' AND object_key = '$1'"
	fi
}

run_job() {
	curl -s -o /dev/null -w "blob_copies run: %{http_code}\n" -X POST "http://127.0.0.1:$CP_PORT/api/v1/admin/jobs/blob_copies/run" -H "Authorization: Bearer $(jwt)"
	for _ in $(seq 60); do
		[ "$(sql "SELECT last_outcome FROM job_runs WHERE job = 'blob_copies'")" != running ] && break
		sleep 2
	done
	sql "SELECT 'blob_copies: ' || last_outcome || ' ' || COALESCE(result::text, '') FROM job_runs WHERE job = 'blob_copies'"
}

up() {
	umask 077
	mkdir -p "$LAB"
	(cd "$repo" && go build -o "$LAB/vaultaire-lab" ./cmd/vaultaire)
	psql -X -q -h localhost -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='$DB'" | grep -q 1 || createdb -h localhost "$DB"
	for f in "$repo"/internal/database/migrations/*.sql; do psql -X -q -v ON_ERROR_STOP=1 -h localhost -d "$DB" -f "$f" >/dev/null 2>&1; done
	[ -s "$LAB/master.key" ] || openssl rand -hex 32 >"$LAB/master.key"
	[ -s "$LAB/edge.token" ] || openssl rand -hex 32 >"$LAB/edge.token"
	start_cp
	local email pass
	email="mirror-lab-$(date +%s)@lab.local" pass="mirror-lab-pass-$(openssl rand -hex 6)"
	printf '{"email":"%s","password":"%s"}' "$email" "$pass" >"$LAB/admin.json"
	curl -s -X POST http://127.0.0.1:$CP_PORT/auth/register -H 'Content-Type: application/json' \
		-d "{\"email\":\"$email\",\"password\":\"$pass\",\"company\":\"Mirror Lab\"}" >"$LAB/reg.json"
	python3 - "$LAB" <<'PY'
import json, sys, os
lab = sys.argv[1]
d = json.load(open(f"{lab}/reg.json"))
open(f"{lab}/aws.creds", "w").write(f"[default]\naws_access_key_id = {d['accessKeyId']}\naws_secret_access_key = {d['secretAccessKey']}\n")
os.chmod(f"{lab}/aws.creds", 0o600)
open(f"{lab}/aws.cfg", "w").write("[default]\nrequest_checksum_calculation = when_required\nresponse_checksum_validation = when_required\ns3 =\n  multipart_threshold = 200MB\n")
PY
	sql "UPDATE users SET role = 'admin' WHERE email = '$email'"
	for f in edge_ingest data_plane_slc edge_mirror; do flag "$f" true; done
	sleep 16 # the flags cache (~15 s)
	aws_cp s3 mb "s3://$BUCKET" >/dev/null
	echo "up: tenant $(tenant), bucket $BUCKET, primary idrive us-central-1, mirror leg lyve $LYVE_REGION"
}

case_whole() { # a 12 MiB whole PUT: one commit, the blob on both legs, the copy row ready with the body's MD5
	golden 4801 12582912 "$LAB/whole12.dat"
	aws_cp s3 cp "$LAB/whole12.dat" "s3://$BUCKET/whole/12.dat" --no-progress
	echo "whole: sent $(md5of "$LAB/whole12.dat")"
	sql "SELECT 'whole: row ' || backend_name || ' ' || size_bytes || ' ' || etag FROM object_head_cache WHERE tenant_id = '$(tenant)' AND object_key = 'whole/12.dat'"
	copies whole/12.dat
	echo "whole: on idrive: $(on_leg idrive whole/12.dat); on lyve: $(on_leg lyve whole/12.dat)"
	sql "SELECT 'whole: admit ' || op || ' host=' || COALESCE(host,'') || ' ' || state || ' mirror_leg=' || COALESCE(attrs->>'mirror_leg','') FROM edge_admits WHERE tenant_id = '$(tenant)' AND object_key = 'whole/12.dat'"
}

case_codec() { # a 12 MiB codec PUT: every stored chunk's copy on the leg
	flag edge_codec true
	flag chunking true
	sleep 16
	golden 4802 12582912 "$LAB/codec12.dat"
	aws_cp s3 cp "$LAB/codec12.dat" "s3://$BUCKET/codec/12.dat" --no-progress
	aws_cp s3 cp "s3://$BUCKET/codec/12.dat" "$LAB/codec12.back" --no-progress >/dev/null
	echo "codec: $(md5of "$LAB/codec12.dat") sent, $(md5of "$LAB/codec12.back") read back"
	copies chunks
	flag edge_codec false
	sleep 16
}

case_fallback() { # the primary's blob removed on idrive itself: the GET is served from the lyve copy, counted
	local t sk
	t=$(tenant)
	sk=$(sql "SELECT storage_key FROM object_head_cache WHERE tenant_id = '$t' AND object_key = 'whole/12.dat'")
	leg idrive s3api delete-object --bucket vaultaire --key "t-$t/${t}_$BUCKET/$sk" >/dev/null
	echo "fallback: on idrive now: $(on_leg idrive whole/12.dat)"
	aws_cp s3 cp "s3://$BUCKET/whole/12.dat" "$LAB/whole12.back" --no-progress >/dev/null
	echo "fallback: $(md5of "$LAB/whole12.dat") sent, $(md5of "$LAB/whole12.back") read back"
	metric 'vaultaire_blob_copies_reads_total'
}

case_backfill() { # an object written with edge_mirror OFF gets its copy from the job once the flag is on
	flag edge_mirror false
	sleep 16
	golden 4803 5242880 "$LAB/back5.dat"
	aws_cp s3 cp "$LAB/back5.dat" "s3://$BUCKET/backfill/5.dat" --no-progress
	echo "backfill: before: $(copies backfill/5.dat | wc -l | tr -d ' ') copy rows; on lyve: $(on_leg lyve backfill/5.dat)"
	flag edge_mirror true
	sleep 16
	run_job
	copies backfill/5.dat
	echo "backfill: on lyve: $(on_leg lyve backfill/5.dat)"
}

case_delete() { # DELETE removes the object on both legs; the job drops the row
	local t sk
	t=$(tenant)
	sk=$(sql "SELECT storage_key FROM object_head_cache WHERE tenant_id = '$t' AND object_key = 'backfill/5.dat'")
	echo "$sk" >"$LAB/last.sk"
	aws_cp s3 rm "s3://$BUCKET/backfill/5.dat" >/dev/null
	echo "delete: on idrive: $(on_leg idrive backfill/5.dat); on lyve: $(on_leg lyve backfill/5.dat)"
	sql "UPDATE blob_copies SET updated_at = NOW() - interval '2 hours' WHERE tenant_id = '$t' AND object_key = 'backfill/5.dat'"
	run_job
	echo "delete: copy rows left: $(sql "SELECT COUNT(*) FROM blob_copies WHERE tenant_id = '$t' AND object_key = 'backfill/5.dat'")"
}

down() {
	[ -f "$LAB/cp.pid" ] && kill "$(cat "$LAB/cp.pid")" 2>/dev/null || true
	rm -f "$LAB/cp.pid"
	if [ "${1:-}" = --purge ]; then
		local t
		t=$(tenant || true)
		if [ -n "$t" ]; then
			leg idrive s3 rm "s3://vaultaire/t-$t/" --recursive >/dev/null || true
			leg lyve s3 rm "s3://stored-$LYVE_REGION/t-$t/" --recursive >/dev/null || true
			leg idrive s3 rm "s3://vaultaire/t-_global/_global/_chunks/$t/" --recursive >/dev/null || true
			leg lyve s3 rm "s3://stored-$LYVE_REGION/t-_global/_global/_chunks/$t/" --recursive >/dev/null || true
		fi
		dropdb -h localhost --if-exists "$DB"
	fi
	echo down
}

case ${1:-} in
up) up ;;
restart)
	[ -f "$LAB/cp.pid" ] && kill "$(cat "$LAB/cp.pid")" 2>/dev/null || true
	sleep 2
	start_cp
	echo "restarted: master key $([ -n "${LAB_KEY:-}" ] && echo on || echo off)"
	;;
run) "case_${2:?case}" ;;
down) down "${2:-}" ;;
*) sed -n '2,17p' "$0" >&2; exit 2 ;;
esac
