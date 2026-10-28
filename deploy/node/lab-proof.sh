#!/usr/bin/env bash
# The lab proof of the data node and host=slc (WP-EDGE-3, Stage 1a Prompt 47;
# docs/reviews/WP-EDGE-3.md § The lab proof). Two processes of the binary
# built from this checkout: the control plane on 127.0.0.1:8000 (a private
# migrated DB, a LAB master key, EDGE_SERVICE_TOKEN; STORAGE_MODE=idrive on the
# bench us-central-1 pair, or LAB_BACKEND=lyve for the bench Lyve pair) and a
# data node on 127.0.0.1:8100 (VAULTAIRE_ROLE=node), registered through the
# admin route with address 127.0.0.1.
#
#   deploy/node/lab-proof.sh up            # both up; a lab tenant with edge_ingest, edge_codec, chunking, data_plane_slc
#   deploy/node/lab-proof.sh run <case>    # node | slc | down | big | leaks (the note's rows)
#   deploy/node/lab-proof.sh down [--purge]  # stops both; --purge deletes the lab's blobs and drops the DB
#
# Secrets are never printed: the bench pairs are read from .env.bench in the
# main checkout, the master key and the tokens from files under $LAB (0600).
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
LAB=${LAB:?set LAB to a private scratch directory (chmod 700)}
BENCH_ENV=${BENCH_ENV:-$HOME/fairforge/vaultaire/.env.bench}
DB=${LAB_DB:-vaultaire_nodelab}
BACKEND=${LAB_BACKEND:-idrive}
CP_PORT=8000
NODE_PORT=8100
NODE_HOST=node.lab.test
BUCKET=nodelab
export AWS_CONFIG_FILE=$LAB/aws.cfg AWS_SHARED_CREDENTIALS_FILE=$LAB/aws.creds

aws_node() { aws --endpoint-url http://127.0.0.1:$NODE_PORT --region us-east-1 "$@"; }
aws_cp() { aws --endpoint-url http://127.0.0.1:$CP_PORT --region us-east-1 "$@"; }
sql() { psql -X -q -At -h localhost -d "$DB" -c "$1"; }
tenant() { sql "SELECT tenant_id FROM api_keys WHERE key_id = '$(awk -F' = ' '/aws_access_key_id/ {print $2}' "$AWS_SHARED_CREDENTIALS_FILE")'"; }
metric() { curl -s http://127.0.0.1:$CP_PORT/metrics | grep -E "^$1" || true; }
bench() { bash -c "source '$BENCH_ENV' >/dev/null 2>&1; printf %s \"\$$1\""; }
md5of() { md5 -q "$1" 2>/dev/null || md5sum "$1" | cut -d' ' -f1; }
golden() { (cd "$repo" && go run ./edge/s3-ingest/lab/goldenbody.go "$1" "$2" "$3"); }

backend_env() {
	case $BACKEND in
	idrive) echo "STORAGE_MODE=idrive IDRIVE_ACCESS_KEY=$(bench IDRIVE_US_CENTRAL_1_ACCESS_KEY) IDRIVE_SECRET_KEY=$(bench IDRIVE_US_CENTRAL_1_SECRET_KEY) IDRIVE_REGION=us-central-1 IDRIVE_BUCKET=vaultaire" ;;
	lyve) echo "STORAGE_MODE=lyve LYVE_ACCESS_KEY=$(bench LYVE_ACCESS_KEY) LYVE_SECRET_KEY=$(bench LYVE_SECRET_KEY) LYVE_REGION=${LAB_LYVE_REGION:-us-west-1}" ;;
	*) echo "LAB_BACKEND is idrive or lyve" >&2; exit 2 ;;
	esac
}

start_cp() {
	mkdir -p "$LAB/data"
	# shellcheck disable=SC2046 # the backend's assignments, one word each
	env -i PATH="$PATH" HOME="$HOME" PORT=$CP_PORT DB_HOST=localhost DB_PORT=5432 DB_NAME="$DB" DB_USER="$(whoami)" DB_PASSWORD= \
		DATA_PATH="$LAB/data" JWT_SECRET=node-lab $(backend_env) ENCRYPTION_MASTER_KEY_FILE="$LAB/master.key" \
		EDGE_SERVICE_TOKEN_FILE="$LAB/edge.token" EDGE_MIN_BYTES=4MiB \
		nohup "$LAB/vaultaire-lab" </dev/null >"$LAB/cp.log" 2>&1 &
	echo $! >"$LAB/cp.pid"
	for _ in $(seq 60); do curl -s http://127.0.0.1:$CP_PORT/health | grep -q '"status"' && return 0; sleep 1; done
	echo "control plane did not come up (see $LAB/cp.log)" >&2
	exit 1
}

start_node() {
	env -i PATH="$PATH" HOME="$HOME" VAULTAIRE_ROLE=node PORT=$NODE_PORT NODE_CONTROL_PLANE_URL=http://127.0.0.1:$CP_PORT \
		NODE_TOKEN_FILE="$LAB/node.token" NODE_PUBLIC_HOST=$NODE_HOST \
		nohup "$LAB/vaultaire-lab" </dev/null >"$LAB/node.log" 2>&1 &
	echo $! >"$LAB/node.pid"
	for _ in $(seq 30); do curl -s http://127.0.0.1:$NODE_PORT/health | grep -q '"role":"node"' && return 0; sleep 1; done
	echo "node did not come up (see $LAB/node.log)" >&2
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
	email="node-lab-$(date +%s)@lab.local" pass="node-lab-pass-$(openssl rand -hex 6)"
	printf '{"email":"%s","password":"%s"}' "$email" "$pass" >"$LAB/admin.json"
	curl -s -X POST http://127.0.0.1:$CP_PORT/auth/register -H 'Content-Type: application/json' \
		-d "{\"email\":\"$email\",\"password\":\"$pass\",\"company\":\"Node Lab\"}" >"$LAB/reg.json"
	python3 - "$LAB" <<'PY'
import json, sys, os
lab = sys.argv[1]
d = json.load(open(f"{lab}/reg.json"))
open(f"{lab}/aws.creds", "w").write(f"[default]\naws_access_key_id = {d['accessKeyId']}\naws_secret_access_key = {d['secretAccessKey']}\n")
os.chmod(f"{lab}/aws.creds", 0o600)
open(f"{lab}/aws.cfg", "w").write("[default]\nrequest_checksum_calculation = when_required\nresponse_checksum_validation = when_required\ns3 =\n  multipart_threshold = 200MB\n")
PY
	sql "UPDATE users SET role = 'admin' WHERE email = '$email'"
	for f in edge_ingest edge_codec chunking; do flag "$f" true; done
	# The node, registered through the admin route: its token is answered ONCE.
	curl -s -X PUT "http://127.0.0.1:$CP_PORT/api/v1/admin/data-plane-hosts/$NODE_HOST" -H "Authorization: Bearer $(jwt)" \
		-H 'Content-Type: application/json' -d '{"kind":"node","address":"127.0.0.1","weight":1}' |
		python3 -c 'import json,sys,os; d=json.load(sys.stdin); t=d.pop("token"); open(os.environ["LAB"]+"/node.token","w").write(t); print("registered:", json.dumps(d))'
	chmod 600 "$LAB/node.token"
	sleep 16 # the flags cache (~15 s)
	start_node
	aws_cp s3 mb "s3://$BUCKET" >/dev/null
	sleep 2
	echo "up: tenant $(tenant), bucket $BUCKET, backend $BACKEND; node health: $(sql "SELECT health || ' ' || COALESCE(version,'') FROM data_plane_hosts WHERE host = '$NODE_HOST'")"
}

case_node() { # 100 MiB of the golden body through the node: codec rows, read back through the node MD5-equal, no body at the control plane
	golden 20261026 104857600 "$LAB/golden100.dat"
	local start end
	: >"$LAB/cp.log.mark"
	start=$(date +%s)
	aws_node s3 cp "$LAB/golden100.dat" "s3://$BUCKET/node/golden100.dat" --no-progress
	end=$(date +%s)
	aws_node s3 cp "s3://$BUCKET/node/golden100.dat" "$LAB/golden100.node-back" --no-progress >/dev/null
	echo "node: $(md5of "$LAB/golden100.dat") sent, $(md5of "$LAB/golden100.node-back") read back through the node; $((end - start)) s"
	sql "SELECT 'node: row ' || backend_name || ' ' || COALESCE(encryption_algorithm,'') || ' ' || size_bytes || ' ' || etag FROM object_head_cache WHERE tenant_id = '$(tenant)' AND object_key = 'node/golden100.dat'"
	sql "SELECT 'node: chunks ' || COUNT(*) FROM tenant_chunk_refs r JOIN object_head_cache h ON h.tenant_id = r.tenant_id AND h.storage_key = r.object_key WHERE h.tenant_id = '$(tenant)' AND h.object_key = 'node/golden100.dat'"
	sql "SELECT 'node: admit ' || op || ' host=' || COALESCE(host,'') || ' ' || state FROM edge_admits WHERE tenant_id = '$(tenant)' AND object_key = 'node/golden100.dat' ORDER BY created_at"
	echo "node: control-plane request lines carrying the object path: $(grep -c '"path":"/'$BUCKET'/node/golden100.dat"' "$LAB/cp.log" || true) (a GET of a chunked row is the origin's: pass-through)"
	echo "node: control-plane PUT lines for the object: $(grep '"method":"PUT"' "$LAB/cp.log" | grep -c 'node/golden100.dat' || true)"
	metric 'vaultaire_data_plane_(admits|bytes)_total\{host="'$NODE_HOST'"'
}

case_slc() { # host=slc: the same through the control plane itself with data_plane_slc ON
	flag data_plane_slc true
	sleep 16
	golden 4701 12582912 "$LAB/golden12.dat"
	aws_cp s3 cp "$LAB/golden12.dat" "s3://$BUCKET/slc/golden12.dat" --no-progress
	aws_cp s3 cp "s3://$BUCKET/slc/golden12.dat" "$LAB/golden12.back" --no-progress >/dev/null
	echo "slc: $(md5of "$LAB/golden12.dat") sent, $(md5of "$LAB/golden12.back") read back"
	sql "SELECT 'slc: admit ' || op || ' host=' || COALESCE(host,'') || ' ' || state FROM edge_admits WHERE tenant_id = '$(tenant)' AND object_key = 'slc/golden12.dat' ORDER BY created_at"
	sql "SELECT 'slc: row ' || backend_name || ' ' || COALESCE(encryption_algorithm,'') || ' ' || size_bytes || ' ' || etag FROM object_head_cache WHERE tenant_id = '$(tenant)' AND object_key = 'slc/golden12.dat'"
	flag data_plane_slc false
}

case_down() { # kill the node: host_up 0 within 90 s, an admit with its token refused 503
	kill "$(cat "$LAB/node.pid")" 2>/dev/null || true
	rm -f "$LAB/node.pid"
	local t0
	t0=$(date +%s)
	for _ in $(seq 40); do
		metric 'vaultaire_data_plane_host_up\{host="'$NODE_HOST'"' | grep -q ' 0$' && break
		sleep 5
	done
	echo "down: host_up 0 after $(($(date +%s) - t0)) s: $(metric 'vaultaire_data_plane_host_up\{host="'$NODE_HOST'"')"
	echo "down: an admit with its token from 127.0.0.1: $(curl -s -o /dev/null -w '%{http_code} retry-after=' -D "$LAB/down.hdr" -X POST http://127.0.0.1:$CP_PORT/api/v1/edge/admit \
		-H "X-Vaultaire-Edge-Token: $(cat "$LAB/node.token")" -H 'Content-Type: application/json' -d '{"op":"put","method":"PUT","path":"/x/y"}')$(grep -i '^retry-after' "$LAB/down.hdr" | tr -d '\r' | awk '{print $2}')"
}

case_big() { # Step 0.2: a 1 GiB codec PUT through the node; MB/s and the node's CPU
	golden 20261027 1073741824 "$LAB/golden1g.dat"
	local start end cpu0 cpu1
	cpu0=$(ps -o cputime= -p "$(cat "$LAB/node.pid")")
	start=$(date +%s)
	aws_node s3 cp "$LAB/golden1g.dat" "s3://$BUCKET/big/golden1g.dat" --no-progress
	end=$(date +%s)
	cpu1=$(ps -o cputime= -p "$(cat "$LAB/node.pid")")
	echo "big: 1 GiB in $((end - start)) s = $((1024 / (end - start + 1))) MiB/s; node CPU $cpu0 → $cpu1; backend $BACKEND"
	sql "SELECT 'big: row ' || backend_name || ' ' || COALESCE(encryption_algorithm,'') || ' ' || size_bytes FROM object_head_cache WHERE tenant_id = '$(tenant)' AND object_key = 'big/golden1g.dat'"
}

case_leaks() { # Step 0.3: what the node's log holds — admit ids and sizes, never a hash, a key, a bucket or a tenant
	local t
	t=$(tenant)
	echo "leaks: node log lines $(wc -l <"$LAB/node.log")"
	for needle in "$BUCKET" "golden100" "golden1g" "$t" "$(shasum -a 256 "$LAB/golden100.dat" | cut -c1-16)"; do
		echo "leaks: '$needle' in the node log: $(grep -c -- "$needle" "$LAB/node.log" || true)"
	done
	# A chunk hash: the first chunk's plain sha256 (the harness's GoldenBody, the first 1–16 MiB).
	echo "leaks: fields seen: $(python3 -c 'import json,sys
keys=set()
for l in open(sys.argv[1]):
    try: keys.update(json.loads(l).keys())
    except Exception: pass
print(sorted(keys))' "$LAB/node.log")"
}

down() {
	for p in node cp; do
		[ -f "$LAB/$p.pid" ] && kill "$(cat "$LAB/$p.pid")" 2>/dev/null || true
		rm -f "$LAB/$p.pid"
	done
	if [ "${1:-}" = "--purge" ]; then
		local t
		t=$(tenant 2>/dev/null || true)
		if [ -n "$t" ]; then
			case $BACKEND in
			idrive)
				AWS_ACCESS_KEY_ID=$(bench IDRIVE_US_CENTRAL_1_ACCESS_KEY) AWS_SECRET_ACCESS_KEY=$(bench IDRIVE_US_CENTRAL_1_SECRET_KEY) AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null \
					sh -c 'for p in "t-$1/" "t-_global/_global/_chunks/$1/"; do aws --endpoint-url https://s3.us-central-1.idrivee2.com --region us-central-1 s3 rm --recursive --only-show-errors "s3://vaultaire/$p"; done' _ "$t"
				;;
			lyve)
				AWS_ACCESS_KEY_ID=$(bench LYVE_ACCESS_KEY) AWS_SECRET_ACCESS_KEY=$(bench LYVE_SECRET_KEY) AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null \
					sh -c 'r=$2; for p in "t-$1/" "t-_global/_global/_chunks/$1/"; do aws --endpoint-url "https://s3.$r.global.lyve.seagate.com" --region "$r" s3 rm --recursive --only-show-errors "s3://stored-$r/$p"; done' _ "$t" "${LAB_LYVE_REGION:-us-west-1}"
				;;
			esac
			echo "purged the lab tenant's blobs ($t) on $BACKEND"
		fi
		dropdb -h localhost --if-exists "$DB"
		rm -f "$LAB"/*.dat "$LAB"/*.back "$LAB"/*.node-back "$LAB/master.key" "$LAB/edge.token" "$LAB/node.token" "$LAB/aws.creds" "$LAB/admin.json" "$LAB/reg.json"
	fi
}

case ${1:-} in
up) up ;;
run) "case_${2:?case}" ;;
down) down "${2:-}" ;;
*) sed -n '2,16p' "$0" >&2; exit 2 ;;
esac
