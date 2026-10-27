#!/usr/bin/env bash
# The lab proof of the Worker 0.3.0's codec pipeline (WP-EDGE-2 Part B,
# docs/reviews/WP-EDGE-2B.md § The lab proof). The topology is Part A's
# (docs/reviews/WP-EDGE-1A.md D-EDGE-7): an origin built from this checkout
# on 127.0.0.1:8099 against a private migrated DB, STORAGE_MODE=idrive on the
# bench us-central-1 pair, a LAB master key; the Worker in `wrangler dev
# --local` on :8787 with the origin as its upstream; aws-cli through the dev
# server as an HTTP proxy, so the Host it signs is the origin's.
#
#   lab/codec-proof.sh up          # origin + Worker, a lab tenant with edge_ingest, edge_codec, chunking ON
#   lab/codec-proof.sh run <case>  # a b c d e f g h (the note's table)
#   lab/codec-proof.sh down        # stops both; `down --purge` also deletes the lab's blobs and drops the DB
#
# Secrets are never printed: the bench pair is read from .env.bench in the
# main checkout, the master key and the token from files under $LAB.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../../.." && pwd)
LAB=${LAB:?set LAB to a private scratch directory (chmod 700)}
BENCH_ENV=${BENCH_ENV:-$HOME/fairforge/vaultaire/.env.bench}
DB=${LAB_DB:-vaultaire_edgelab}
ORIGIN_PORT=8099
WORKER_PORT=8787
BUCKET=codeclab
export AWS_CONFIG_FILE=$LAB/aws.cfg AWS_SHARED_CREDENTIALS_FILE=$LAB/aws.creds

aws_lab() { HTTP_PROXY=http://127.0.0.1:$WORKER_PORT aws --endpoint-url http://127.0.0.1:$ORIGIN_PORT --region us-east-1 "$@"; }
aws_origin() { aws --endpoint-url http://127.0.0.1:$ORIGIN_PORT --region us-east-1 "$@"; }
sql() { psql -X -q -At -h localhost -d "$DB" -c "$1"; }
tenant() { sql "SELECT tenant_id FROM api_keys WHERE key_id = '$(awk -F' = ' '/aws_access_key_id/ {print $2}' "$AWS_SHARED_CREDENTIALS_FILE")'"; }
metric() { curl -s http://127.0.0.1:$ORIGIN_PORT/metrics | grep -E "^$1" || true; }

start_origin() {
	# shellcheck disable=SC1090
	ak=$(bash -c "source '$BENCH_ENV' >/dev/null 2>&1; printf %s \"\$IDRIVE_US_CENTRAL_1_ACCESS_KEY\"")
	sk=$(bash -c "source '$BENCH_ENV' >/dev/null 2>&1; printf %s \"\$IDRIVE_US_CENTRAL_1_SECRET_KEY\"")
	mkdir -p "$LAB/data"
	env -i PATH="$PATH" HOME="$HOME" PORT=$ORIGIN_PORT DB_HOST=localhost DB_PORT=5432 DB_NAME="$DB" DB_USER="$(whoami)" DB_PASSWORD= \
		DATA_PATH="$LAB/data" JWT_SECRET=codec-lab STORAGE_MODE=idrive IDRIVE_ACCESS_KEY="$ak" IDRIVE_SECRET_KEY="$sk" \
		IDRIVE_REGION=us-central-1 IDRIVE_BUCKET=vaultaire ENCRYPTION_MASTER_KEY_FILE="$LAB/master.key" \
		EDGE_SERVICE_TOKEN_FILE="$LAB/edge.token" EDGE_TRUST_ANY_PEER=true \
		nohup "$LAB/vaultaire-lab" </dev/null >"$LAB/origin.log" 2>&1 &
	echo $! >"$LAB/origin.pid"
	for _ in $(seq 60); do curl -s http://127.0.0.1:$ORIGIN_PORT/health | grep -q '"status"' && return 0; sleep 1; done
	echo "origin did not come up (see $LAB/origin.log)" >&2
	exit 1
}

start_worker() {
	local tamper=${1:-}
	{
		echo "VAULTAIRE_ORIGIN=http://127.0.0.1:$ORIGIN_PORT"
		echo "VAULTAIRE_EDGE_TOKEN=$(cat "$LAB/edge.token")"
		echo "ENVIRONMENT=lab"
		[ -n "$tamper" ] && echo "CODEC_TAMPER=$tamper"
	} >"$repo/edge/s3-ingest/.dev.vars"
	chmod 600 "$repo/edge/s3-ingest/.dev.vars"
	: >"$LAB/worker.log"
	(cd "$repo/edge/s3-ingest" && nohup npx -y wrangler@4.151.3 dev --local --port $WORKER_PORT --local-upstream 127.0.0.1:$ORIGIN_PORT </dev/null >"$LAB/worker.log" 2>&1 &
		echo $! >"$LAB/worker.pid")
	for _ in $(seq 90); do grep -q "Ready on" "$LAB/worker.log" 2>/dev/null && return 0; sleep 1; done
	echo "worker did not come up (see $LAB/worker.log)" >&2
	exit 1
}

# tree prints a process and every descendant (wrangler's npx → node → node →
# workerd chain: killing the listener alone lets the parent respawn it).
tree() {
	local p=$1 c
	echo "$p"
	for c in $(pgrep -P "$p" 2>/dev/null); do tree "$c"; done
}

stop_worker() {
	if [ -f "$LAB/worker.pid" ]; then
		kill $(tree "$(cat "$LAB/worker.pid")") 2>/dev/null || true
		rm -f "$LAB/worker.pid"
	fi
	for _ in $(seq 20); do lsof -ti tcp:$WORKER_PORT -sTCP:LISTEN >/dev/null 2>&1 || break; sleep 0.5; done
	if lsof -ti tcp:$WORKER_PORT -sTCP:LISTEN >/dev/null 2>&1; then
		echo "port $WORKER_PORT still held: $(lsof -ti tcp:$WORKER_PORT -sTCP:LISTEN | tr '\n' ' ')" >&2
		exit 1
	fi
	rm -f "$repo/edge/s3-ingest/.dev.vars"
}

up() {
	(cd "$repo" && make edge-wasm >/dev/null)
	start_origin
	local email="codec-lab-$(date +%s)@lab.local" pass="codec-lab-pass-$(openssl rand -hex 6)"
	curl -s -X POST http://127.0.0.1:$ORIGIN_PORT/auth/register -H 'Content-Type: application/json' \
		-d "{\"email\":\"$email\",\"password\":\"$pass\",\"company\":\"Codec Lab\"}" >"$LAB/reg.json"
	python3 - "$LAB" <<'PY'
import json, sys, os
lab = sys.argv[1]
d = json.load(open(f"{lab}/reg.json"))
open(f"{lab}/aws.creds", "w").write(f"[default]\naws_access_key_id = {d['accessKeyId']}\naws_secret_access_key = {d['secretAccessKey']}\n")
os.chmod(f"{lab}/aws.creds", 0o600)
open(f"{lab}/aws.cfg", "w").write("[default]\nrequest_checksum_calculation = when_required\nresponse_checksum_validation = when_required\n")
PY
	sql "UPDATE users SET role = 'admin' WHERE email = '$email'"
	local jwt
	jwt=$(curl -s -X POST http://127.0.0.1:$ORIGIN_PORT/auth/login -H 'Content-Type: application/json' -d "{\"email\":\"$email\",\"password\":\"$pass\"}" |
		python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
	local t
	t=$(tenant)
	for f in edge_ingest edge_codec chunking; do
		curl -s -o /dev/null -w "flag $f: %{http_code}\n" -X PUT "http://127.0.0.1:$ORIGIN_PORT/api/v1/admin/flags/$f" \
			-H "Authorization: Bearer $jwt" -H 'Content-Type: application/json' -d "{\"enabled\":true,\"tenant_id\":\"$t\"}"
	done
	sleep 16 # the flags cache (~15 s)
	start_worker "${TAMPER:-}"
	aws_origin s3 mb "s3://$BUCKET" >/dev/null
	echo "up: tenant $t, bucket $BUCKET"
}

golden() { # golden <seed> <bytes> <file>: crypto.GoldenBody
	(cd "$repo" && go run ./edge/s3-ingest/lab/goldenbody.go "$1" "$2" "$3")
}

md5of() { md5 -q "$1" 2>/dev/null || md5sum "$1" | cut -d' ' -f1; }

case_a() { # 100 MiB of the golden body through the Worker: codec, the golden chunk count, read back MD5-equal
	golden 20261026 104857600 "$LAB/golden100.dat"
	aws configure set default.s3.multipart_threshold 200MB
	local start end
	start=$(date +%s)
	aws_lab s3 cp "$LAB/golden100.dat" "s3://$BUCKET/a/golden100.dat" --no-progress
	end=$(date +%s)
	aws_origin s3 cp "s3://$BUCKET/a/golden100.dat" "$LAB/golden100.back" --no-progress >/dev/null
	echo "a: $(md5of "$LAB/golden100.dat") sent, $(md5of "$LAB/golden100.back") read back; $((end - start)) s"
	sql "SELECT 'a: chunks ' || COUNT(*) FROM tenant_chunk_refs r JOIN object_head_cache h ON h.tenant_id = r.tenant_id AND h.storage_key = r.object_key WHERE h.tenant_id = '$(tenant)' AND h.object_key = 'a/golden100.dat'"
	sql "SELECT 'a: row ' || backend_name || ' ' || COALESCE(encryption_algorithm,'') || ' ' || size_bytes || ' ' || etag FROM object_head_cache WHERE tenant_id = '$(tenant)' AND object_key = 'a/golden100.dat'"
}

case_b() { # the same file again: 0 PUTs on the backend, 100 % hits
	local before after
	before=$(metric 'vaultaire_edge_codec_chunks_total\{outcome="stored"\}')
	aws_lab s3 cp "$LAB/golden100.dat" "s3://$BUCKET/b/golden100-again.dat" --no-progress
	after=$(metric 'vaultaire_edge_codec_chunks_total\{outcome="stored"\}')
	echo "b: stored before: $before / after: $after"
	metric 'vaultaire_edge_codec_chunks_total'
	grep -c '"edge":"codec"' "$LAB/worker.log" || true
	grep '"edge":"codec"' "$LAB/worker.log" | tail -1
}

case_c() { # the Worker's refs against the harness's rule: count, offsets, sizes (sizes from the golden vectors)
	sql "SELECT 'c: ' || COUNT(*) || ' refs, offsets ' || string_agg(r.chunk_offset::text, ',' ORDER BY r.chunk_index) FROM tenant_chunk_refs r JOIN object_head_cache h ON h.tenant_id = r.tenant_id AND h.storage_key = r.object_key WHERE h.tenant_id = '$(tenant)' AND h.object_key = 'a/golden100.dat'" >"$LAB/c-worker.txt"
	python3 - "$repo/internal/crypto/testdata/chunker_golden.json" "$LAB/c-worker.txt" <<'PY'
import json, sys
v = json.load(open(sys.argv[1]))
got = open(sys.argv[2]).read().strip()
offs, o = [], 0
for n in v["chunk_lengths"]:
    offs.append(o); o += n
want = f"c: {len(offs)} refs, offsets " + ",".join(map(str, offs))
print("c: the Worker's refs equal the golden boundaries (the harness's chunker):", got == want, f"({len(offs)} chunks)")
PY
}

case_d() { # aws-cli multipart: default 8 MiB parts, then 64 MB parts
	golden 46201 52428800 "$LAB/mp50.dat"
	aws configure set default.s3.multipart_threshold 8MB
	aws configure set default.s3.multipart_chunksize 8MB
	aws_lab s3 cp "$LAB/mp50.dat" "s3://$BUCKET/d/mp50-8m.dat" --no-progress
	aws configure set default.s3.multipart_chunksize 64MB
	golden 46202 157286400 "$LAB/mp150.dat"
	aws_lab s3 cp "$LAB/mp150.dat" "s3://$BUCKET/d/mp150-64m.dat" --no-progress
	for k in mp50-8m mp150-64m; do
		aws_origin s3 cp "s3://$BUCKET/d/$k.dat" "$LAB/$k.back" --no-progress >/dev/null
	done
	echo "d: 8 MiB parts $(md5of "$LAB/mp50.dat") / $(md5of "$LAB/mp50-8m.back"); 64 MB parts $(md5of "$LAB/mp150.dat") / $(md5of "$LAB/mp150-64m.back")"
	sql "SELECT 'd: ' || object_key || ' ' || backend_name || ' ' || etag FROM object_head_cache WHERE tenant_id = '$(tenant)' AND object_key LIKE 'd/%' ORDER BY 1"
}

case_d48() { # 48 MiB in 8 MiB parts (no tail under EDGE_MIN_BYTES): every part the codec's
	golden 46207 50331648 "$LAB/mp48.dat"
	aws configure set default.s3.multipart_threshold 8MB
	aws configure set default.s3.multipart_chunksize 8MB
	aws_lab s3 cp "$LAB/mp48.dat" "s3://$BUCKET/d/mp48-8m.dat" --no-progress
	aws_origin s3 cp "s3://$BUCKET/d/mp48-8m.dat" "$LAB/mp48.back" --no-progress >/dev/null
	echo "d48: $(md5of "$LAB/mp48.dat") / $(md5of "$LAB/mp48.back")"
	sql "SELECT 'd48: ' || object_key || ' ' || backend_name || ' ' || etag FROM object_head_cache WHERE tenant_id = '$(tenant)' AND object_key = 'd/mp48-8m.dat'"
}

case_e() { # 300 MiB of one 4 MiB block repeated: parallel parts share chunks — no 503
	python3 - "$LAB/rep300.dat" <<'PY'
import sys, hashlib
block = b"".join(hashlib.sha256(i.to_bytes(4, "big")).digest() for i in range(4 * 1024 * 1024 // 32))
with open(sys.argv[1], "wb") as f:
    for _ in range(75):
        f.write(block)
PY
	aws configure set default.s3.multipart_chunksize 8MB
	aws_lab s3 cp "$LAB/rep300.dat" "s3://$BUCKET/e/rep300.dat" --no-progress --debug 2>"$LAB/e-debug.log"
	echo "e: 503s in the aws-cli debug log: $(grep -c 'HTTP/1.1" 503\|status_code=503\|503 Service' "$LAB/e-debug.log" || true)"
	aws_origin s3 cp "s3://$BUCKET/e/rep300.dat" "$LAB/rep300.back" --no-progress >/dev/null
	echo "e: $(md5of "$LAB/rep300.dat") / $(md5of "$LAB/rep300.back")"
	metric 'vaultaire_edge_codec_chunks_total'
}

case_f() { # aws-cli killed at ~50 %: the reaper drops the claims after the admit's expiry
	golden 46203 209715200 "$LAB/kill200.dat"
	aws configure set default.s3.multipart_chunksize 8MB
	aws_lab s3 cp "$LAB/kill200.dat" "s3://$BUCKET/f/kill200.dat" --no-progress &
	local pid=$!
	for _ in $(seq 600); do
		n=$(sql "SELECT COUNT(*) FROM multipart_parts p JOIN multipart_uploads u ON u.upload_id = p.upload_id WHERE u.tenant_id = '$(tenant)' AND u.object_key = 'f/kill200.dat'")
		[ "${n:-0}" -ge 12 ] && break
		sleep 0.2
	done
	kill -9 $pid 2>/dev/null || true
	echo "f: killed at $n of 25 parts; pending admits now: $(sql "SELECT COUNT(*) FROM edge_admits WHERE tenant_id = '$(tenant)' AND state = 'pending'")"
	echo "f: the reaper's run after 25 min: lab/codec-proof.sh run f2"
}

case_f_host() { # the HOST dies mid-upload (the Worker killed): its admits stay pending, the reaper drops their claims after the expiry
	golden 46208 209715200 "$LAB/hostkill200.dat"
	aws configure set default.s3.multipart_threshold 8MB
	aws configure set default.s3.multipart_chunksize 8MB
	aws_lab s3 cp "$LAB/hostkill200.dat" "s3://$BUCKET/f/hostkill200.dat" --no-progress >/dev/null 2>&1 &
	local pid=$! n=0
	for _ in $(seq 600); do
		n=$(sql "SELECT COUNT(*) FROM edge_admits WHERE tenant_id = '$(tenant)' AND object_key = 'f/hostkill200.dat' AND state = 'committed'")
		[ "${n:-0}" -ge 8 ] && break
		sleep 0.2
	done
	stop_worker
	kill -9 $pid 2>/dev/null || true
	echo "f_host: the Worker killed after $n committed parts at $(date -u +%H:%M:%S); pending admits $(sql "SELECT COUNT(*) FROM edge_admits WHERE tenant_id = '$(tenant)' AND object_key = 'f/hostkill200.dat' AND state = 'pending'"), pending claims $(sql "SELECT COUNT(*) FROM global_content_index WHERE dedup_scope = '$(tenant)' AND state = 'pending'")"
	start_worker ""
	echo "f_host: run f2 after the expiry (20 min) and one reaper run (5 min)"
}

case_f2() {
	echo "f2: pending admits $(sql "SELECT COUNT(*) FROM edge_admits WHERE tenant_id = '$(tenant)' AND state = 'pending'"), expired $(sql "SELECT COUNT(*) FROM edge_admits WHERE tenant_id = '$(tenant)' AND state = 'expired'") (codec: $(sql "SELECT COUNT(*) FROM edge_admits WHERE tenant_id = '$(tenant)' AND state = 'expired' AND codec"))"
	sql "SELECT 'f2: job ' || last_outcome || ' at ' || last_finished_at || ' ' || COALESCE(result::text, '') FROM job_runs WHERE job = 'edge_admit_reaper'"
	metric 'vaultaire_edge_admit_reaper_total'
	echo "f2: claims left pending: $(sql "SELECT COUNT(*) FROM global_content_index WHERE dedup_scope = '$(tenant)' AND state = 'pending'")"
}

case_g() { # a tampered Worker (TAMPER=etag | key): the commit refused 400, nothing stored for the object
	local mode=${1:?g etag|key}
	stop_worker
	start_worker "$mode"
	golden 46204 12582912 "$LAB/tamper.dat"
	aws configure set default.s3.multipart_threshold 200MB
	set +e
	aws_lab s3 cp "$LAB/tamper.dat" "s3://$BUCKET/g/tamper-$mode.dat" --no-progress 2>&1 | tail -2
	set -e
	echo "g($mode): head row: $(sql "SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = '$(tenant)' AND object_key = 'g/tamper-$mode.dat'")"
	grep -E "edge codec commit|head_mismatch" "$LAB/origin.log" | tail -2 | cut -c1-300
	metric 'vaultaire_edge_codec_chunks_total\{outcome="head_mismatch"\}'
	stop_worker
	start_worker ""
}

case_h() { # throughput: 1 GiB through the codec vs through 0.2.1's pass-through (the flag off)
	golden 46205 1073741824 "$LAB/g1.dat"
	aws configure set default.s3.multipart_threshold 8MB
	aws configure set default.s3.multipart_chunksize 64MB
	local s e
	s=$(date +%s.%N)
	aws_lab s3 cp "$LAB/g1.dat" "s3://$BUCKET/h/codec1g.dat" --no-progress
	e=$(date +%s.%N)
	echo "h: codec 1 GiB: $(python3 -c "print(round(1073.741824/($e-$s),1))") MB/s ($(python3 -c "print(round($e-$s,1))") s)"
	grep '"edge":"codec_index"' "$LAB/worker.log" | python3 -c '
import json,sys
ms=sorted(json.loads(l[l.index("{"):])["ms"] for l in sys.stdin if "{" in l)
print("h: index calls", len(ms), "p50", ms[len(ms)//2] if ms else 0, "ms p95", ms[int(len(ms)*0.95)] if ms else 0, "ms")'
	echo "h: the pass-through baseline: TOGGLE=off lab/codec-proof.sh run h_off"
}

case_h_off() { # the 0.2.1 mover's path: edge_codec OFF and the bucket without SSE for this run (an SSE bucket's write is the origin's; the free tier allows one bucket)
	sql "UPDATE feature_flags SET enabled = false WHERE flag_key = 'edge_codec' AND tenant_id = '$(tenant)'"
	sql "UPDATE buckets SET sse_enabled = false WHERE name = '$BUCKET' AND tenant_id = '$(tenant)'" # no DeleteBucketEncryption on the S3 API: the lab DB
	sleep 20 # the flags cache
	golden 46206 1073741824 "$LAB/g1b.dat"
	aws configure set default.s3.multipart_threshold 8MB
	aws configure set default.s3.multipart_chunksize 64MB
	local s e
	s=$(date +%s.%N)
	aws_lab s3 cp "$LAB/g1b.dat" "s3://$BUCKET/h/pass1g-mover.dat" --no-progress
	e=$(date +%s.%N)
	echo "h_off: pass-through mover 1 GiB: $(python3 -c "print(round(1073.741824/($e-$s),1))") MB/s"
	sql "SELECT 'h_off: ' || object_key || ' ' || backend_name || ' ' || etag FROM object_head_cache WHERE tenant_id = '$(tenant)' AND object_key = 'h/pass1g-mover.dat'"
	sql "UPDATE buckets SET sse_enabled = true WHERE name = '$BUCKET' AND tenant_id = '$(tenant)'"
	sql "UPDATE feature_flags SET enabled = true WHERE flag_key = 'edge_codec' AND tenant_id = '$(tenant)'"
}

# --- Prompt 46c Part 0 (WP-EDGE-2C.md § The lab) ---

admitted() { metric "vaultaire_data_plane_admits_total\\{host=\"edge\",op=\"$1\",outcome=\"$2\"\\}" | awk '{print $2+0}'; }

case_p0_parts() { # RP70-2: 50 MiB at aws-cli's default 8 MiB parts, ten times — every part admitted, never staged, never a 400
	golden 46301 52428800 "$LAB/p0-50.dat"
	aws configure set default.s3.multipart_threshold 8MB
	aws configure set default.s3.multipart_chunksize 8MB
	local before_adm before_pass before_small n=0 bad=0
	before_adm=$(admitted part admitted)
	before_pass=$(admitted part pass_through)
	before_small=$(admitted part pass_through_small)
	for i in $(seq 10); do
		aws_lab s3 cp "$LAB/p0-50.dat" "s3://$BUCKET/p0/fifty-$i.dat" --no-progress --debug 2>"$LAB/p0-$i.log" >/dev/null || bad=$((bad + 1))
		grep -qE '"PUT /[^"]*partNumber[^"]*" 400|status_code=400|HTTP/1.1" 400' "$LAB/p0-$i.log" && bad=$((bad + 1))
		n=$((n + 7))
	done
	echo "p0_parts: parts admitted +$(( $(admitted part admitted) - before_adm )) (expected $n), pass_through +$(( $(admitted part pass_through) - before_pass )), small +$(( $(admitted part pass_through_small) - before_small )), failed or 400 runs: $bad"
	echo "p0_parts: staged_parts lines in the worker log: $(grep -c '"reason":"staged_parts"' "$LAB/worker.log" || true)"
	aws_origin s3 cp "s3://$BUCKET/p0/fifty-10.dat" "$LAB/p0-50.back" --no-progress >/dev/null
	echo "p0_parts: $(md5of "$LAB/p0-50.dat") / $(md5of "$LAB/p0-50.back")"
	sql "SELECT 'p0_parts: rows ' || COUNT(*) || ' ' || string_agg(DISTINCT backend_name, ',') FROM object_head_cache WHERE tenant_id = '$(tenant)' AND object_key LIKE 'p0/fifty-%'"
}

# rss_watch <pidfile-out>: samples the workerd processes' RSS every 0.5 s
# until killed; prints the peak in MiB at the end (the lab's memory number —
# the local workerd process, not an edge isolate; RB3-6).
rss_watch() {
	local peak=0 r
	trap 'echo $((peak / 1024)); exit 0' TERM
	while :; do
		r=$(ps -ax -o rss=,comm= | awk '/workerd/ {s += $1} END {print s+0}')
		[ "$r" -gt "$peak" ] && peak=$r
		sleep 0.5
	done
}

case_p0_bytes() { # RP70-1: zeros, 16 MiB constant runs (the chunker's maximum), a 1 GiB sparse image — the peak in BYTES
	python3 - "$LAB" <<'PY2'
import sys
lab = sys.argv[1]
with open(f"{lab}/zeros256.dat", "wb") as f:
    f.write(b"\0" * (256 << 20))
with open(f"{lab}/runs256.dat", "wb") as f:
    for k in range(16):
        f.write(bytes([0x11 + k]) * (16 << 20))
PY2
	rm -f "$LAB/sparse1g.img"
	dd if=/dev/zero of="$LAB/sparse1g.img" bs=1 count=0 seek=1073741824 2>/dev/null
	for off in 1 97 300 511 777 1000; do dd if=/dev/urandom of="$LAB/sparse1g.img" bs=1048576 count=3 seek="$off" conv=notrunc 2>/dev/null; done
	aws configure set default.s3.multipart_threshold 8MB
	aws configure set default.s3.multipart_chunksize 64MB
	for f in zeros256.dat runs256.dat sparse1g.img; do
		local mark w peak
		mark=$(wc -l <"$LAB/worker.log")
		rss_watch >"$LAB/rss-$f.txt" &
		w=$!
		aws_lab s3 cp "$LAB/$f" "s3://$BUCKET/p0/$f" --no-progress >/dev/null
		kill -TERM $w; wait $w 2>/dev/null || true
		peak=$(tail -n +"$((mark + 1))" "$LAB/worker.log" | grep '"edge":"codec"' | python3 -c '
import json,sys
v=[json.loads(l[l.index("{"):]) for l in sys.stdin if "{" in l]
print(max((x.get("peak_in_flight_bytes",0) for x in v), default=0)>>20, "MiB peak over", len(v), "requests; chunks", sum(x["chunks"] for x in v), "misses", sum(x["misses"] for x in v))')
		aws_origin s3 cp "s3://$BUCKET/p0/$f" "$LAB/$f.back" --no-progress >/dev/null
		echo "p0_bytes $f: $peak; workerd RSS peak $(cat "$LAB/rss-$f.txt") MiB; $(md5of "$LAB/$f") / $(md5of "$LAB/$f.back")"
	done
}

# --- Prompt 46c PR A: aws-cli's DEFAULT upload at the edge (WP-EDGE-2C.md § The lab) ---

default_cfg() { # aws-cli's defaults: no request_checksum_calculation line (aws-chunked + CRC64NVME)
	printf '[default]\n' >"$AWS_CONFIG_FILE"
}
when_required_cfg() {
	printf '[default]\nrequest_checksum_calculation = when_required\nresponse_checksum_validation = when_required\n' >"$AWS_CONFIG_FILE"
}
flag_codec() { # flag_codec on|off: the lab tenant's edge_codec row (the flags cache is ~15 s)
	local v=true
	[ "$1" = off ] && v=false
	sql "UPDATE feature_flags SET enabled = $v WHERE flag_key = 'edge_codec' AND tenant_id = '$(tenant)'"
	sql "UPDATE buckets SET sse_enabled = $([ "$1" = off ] && echo false || echo true) WHERE name = '$BUCKET' AND tenant_id = '$(tenant)'"
	sleep 20
}
creds() { # the lab tenant's pair in the environment (awschunkedput.go)
	AWS_ACCESS_KEY_ID=$(awk -F' = ' '/aws_access_key_id/ {print $2}' "$AWS_SHARED_CREDENTIALS_FILE")
	AWS_SECRET_ACCESS_KEY=$(awk -F' = ' '/aws_secret_access_key/ {print $2}' "$AWS_SHARED_CREDENTIALS_FILE")
	export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
}

chunked_upload() { # chunked_upload <file> <key>: aws-cli's TLS shape (aws-chunked, CRC64NVME trailer, 8 MiB × 10) through the Worker
	creds
	(cd "$repo" && go run ./edge/s3-ingest/lab/awschunkedput.go -endpoint http://127.0.0.1:$ORIGIN_PORT -proxy http://127.0.0.1:$WORKER_PORT \
		-bucket $BUCKET -key "$2" -file "$1" -multipart)
}

case_pa_mover() { # 100 MiB in aws-cli's aws-chunked shape, edge_codec OFF: every part header-signed to the backend, none through the origin, MD5-equal
	flag_codec off
	golden 46401 104857600 "$LAB/pa100.dat"
	local adm pass unv
	adm=$(admitted part admitted); unv=$(admitted part admitted_unverified); pass=$(admitted part pass_through)
	echo "pa_mover: $(chunked_upload "$LAB/pa100.dat" pa/mover100.dat 2>&1 | grep -v '^goroutine\|^[[:space:]]' | head -3)"
	echo "pa_mover: parts admitted +$(( $(admitted part admitted) - adm )), admitted_unverified +$(( $(admitted part admitted_unverified) - unv )), pass_through +$(( $(admitted part pass_through) - pass ))"
	echo "pa_mover: parts staged on the origin: $(sql "SELECT COUNT(*) FROM multipart_parts p JOIN multipart_uploads u ON u.upload_id = p.upload_id WHERE u.tenant_id = '$(tenant)' AND u.object_key = 'pa/mover100.dat' AND p.backend_etag IS NULL"), edge parts: $(sql "SELECT COUNT(*) FROM multipart_parts p JOIN multipart_uploads u ON u.upload_id = p.upload_id WHERE u.tenant_id = '$(tenant)' AND u.object_key = 'pa/mover100.dat' AND p.backend_etag IS NOT NULL")"
	echo "pa_mover: header-signed admits $(grep -c '"aws_chunked":true' "$LAB/origin.log")"
	aws_origin s3 cp "s3://$BUCKET/pa/mover100.dat" "$LAB/pa-mover.back" --no-progress >/dev/null
	echo "pa_mover: $(md5of "$LAB/pa100.dat") / $(md5of "$LAB/pa-mover.back")"
	sql "SELECT 'pa_mover: row ' || backend_name || ' ' || etag || ' ' || size_bytes FROM object_head_cache WHERE tenant_id = '$(tenant)' AND object_key = 'pa/mover100.dat'"
	# aws-cli itself with its DEFAULT config over the lab's plain HTTP: a header checksum, no trailer (botocore uses trailers over TLS only).
	default_cfg
	adm=$(admitted part admitted)
	aws_lab s3 cp "$LAB/pa100.dat" "s3://$BUCKET/pa/awscli100.dat" --no-progress --debug 2>"$LAB/pa-awscli.log" >/dev/null
	echo "pa_mover aws-cli default over http: parts admitted +$(( $(admitted part admitted) - adm )); its payload hashes: $(grep -o "'x-amz-content-sha256': b'[A-Z-]*" "$LAB/pa-awscli.log" | sort | uniq -c | tr -s ' ' | tr '\n' ' ') trailer headers $(grep -c "'x-amz-trailer'" "$LAB/pa-awscli.log")"
	when_required_cfg
	flag_codec on
}

case_pa_codec() { # 100 MiB in aws-cli's aws-chunked shape, edge_codec ON: the codec decodes and verifies the trailer, MD5-equal
	golden 46402 104857600 "$LAB/pa100c.dat"
	local adm mark
	adm=$(admitted part admitted)
	mark=$(wc -l <"$LAB/worker.log")
	echo "pa_codec: $(chunked_upload "$LAB/pa100c.dat" pa/codec100.dat 2>&1 | grep -v '^goroutine\|^[[:space:]]' | head -3)"
	echo "pa_codec: parts admitted +$(( $(admitted part admitted) - adm )); codec requests $(tail -n +"$((mark + 1))" "$LAB/worker.log" | grep -c '"edge":"codec"'), checksum mismatches $(tail -n +"$((mark + 1))" "$LAB/worker.log" | grep -c codec_checksum_mismatch)"
	aws_origin s3 cp "s3://$BUCKET/pa/codec100.dat" "$LAB/pa-codec.back" --no-progress >/dev/null
	echo "pa_codec: $(md5of "$LAB/pa100c.dat") / $(md5of "$LAB/pa-codec.back")"
	sql "SELECT 'pa_codec: row ' || backend_name || ' ' || COALESCE(encryption_algorithm, '') || ' ' || etag || ' ' || size_bytes FROM object_head_cache WHERE tenant_id = '$(tenant)' AND object_key = 'pa/codec100.dat'"
}

case_pa_trailer() { # a corrupted trailer, the body's length right: 400 on the client — the codec's decoder (a PUT) and the backend's (a part, edge_codec OFF)
	creds
	golden 46403 6291456 "$LAB/pa6.dat"
	echo "pa_trailer codec PutObject, corrupted: $(cd "$repo" && go run ./edge/s3-ingest/lab/awschunkedput.go -endpoint http://127.0.0.1:$ORIGIN_PORT -proxy http://127.0.0.1:$WORKER_PORT -bucket $BUCKET -key pa/bad-put.bin -file "$LAB/pa6.dat" -corrupt)"
	echo "pa_trailer codec PutObject, intact:    $(cd "$repo" && go run ./edge/s3-ingest/lab/awschunkedput.go -endpoint http://127.0.0.1:$ORIGIN_PORT -proxy http://127.0.0.1:$WORKER_PORT -bucket $BUCKET -key pa/good-put.bin -file "$LAB/pa6.dat")"
	echo "pa_trailer rows: bad $(sql "SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = '$(tenant)' AND object_key = 'pa/bad-put.bin'"), good $(sql "SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = '$(tenant)' AND object_key = 'pa/good-put.bin'")"
	flag_codec off
	local up
	up=$(aws_origin s3api create-multipart-upload --bucket $BUCKET --key pa/bad-part.bin --query UploadId --output text)
	echo "pa_trailer mover UploadPart, corrupted: $(cd "$repo" && go run ./edge/s3-ingest/lab/awschunkedput.go -endpoint http://127.0.0.1:$ORIGIN_PORT -proxy http://127.0.0.1:$WORKER_PORT -bucket $BUCKET -key pa/bad-part.bin -file "$LAB/pa6.dat" -upload-id "$up" -part 1 -corrupt)"
	echo "pa_trailer mover UploadPart, intact:    $(cd "$repo" && go run ./edge/s3-ingest/lab/awschunkedput.go -endpoint http://127.0.0.1:$ORIGIN_PORT -proxy http://127.0.0.1:$WORKER_PORT -bucket $BUCKET -key pa/bad-part.bin -file "$LAB/pa6.dat" -upload-id "$up" -part 1)"
	aws_origin s3api abort-multipart-upload --bucket $BUCKET --key pa/bad-part.bin --upload-id "$up"
	echo "pa_trailer mover PutObject on iDrive (D-EDGE-33): $(cd "$repo" && go run ./edge/s3-ingest/lab/awschunkedput.go -endpoint http://127.0.0.1:$ORIGIN_PORT -proxy http://127.0.0.1:$WORKER_PORT -bucket $BUCKET -key pa/origin-put.bin -file "$LAB/pa6.dat")"
	grep '"reason":"aws_chunked_unbounded"' "$LAB/worker.log" | tail -1 | cut -c1-200
	flag_codec on
}

down() {
	stop_worker
	if [ -f "$LAB/origin.pid" ]; then kill "$(cat "$LAB/origin.pid")" 2>/dev/null || true; rm -f "$LAB/origin.pid"; fi
	if [ "${1:-}" = "--purge" ]; then
		echo "purge: the lab's objects are deleted through a fresh origin, then the DB dropped"
	fi
}

cmd=${1:-}
shift || true
case "$cmd" in
up) up ;;
origin) start_origin; echo "origin up" ;;
worker) stop_worker; start_worker "${TAMPER:-}"; echo "worker up" ;;
run) "case_$1" "${@:2}" ;;
down) down "$@" ;;
*) echo "usage: $0 up | origin | run <a|b|c|d|e|f|f2|g etag|g key|h|h_off|p0_parts|p0_bytes|pa_mover|pa_codec|pa_trailer> | down" >&2; exit 2 ;;
esac
