#!/bin/bash
# tenant-zero-proof.sh — rule 2 of the lanes: a flag flips globally only after a proof on
# tenant zero (Stage 1a Prompt 39 Part B, docs/reviews/WP-P39.md § Tenant zero). This runs
# that proof: each dark flag is turned ON FOR ONE TENANT (never a '*' row — flag_body
# refuses it), exercised through the customer's own path (aws-cli, SigV4, against
# TZP_ENDPOINT — prod: https://stored.ge, through Cloudflare and HAProxy) and every number
# it reports is read back from the database or /metrics, never typed.
#
#   sudo -S -p '' TZP_TENANT=tenant-… bash tenant-zero-proof.sh preflight chunking …
#
# Steps, in order — a free-tier tenant has ONE bucket, so the bench bucket (made by
# chunking) is gone before modes and dogfood make theirs:
#   evening: preflight chunking multipart parallel_get versioned_chunking
#            (the bench objects stay overnight: the 05:30 UTC routing_truth run samples
#            chunked objects, and the night's dedup_gc / retired_blobs runs see the v1 delete)
#   morning: morning bench_cleanup modes dogfood transparency
# `all` = both lists in one go (a lab: no night in between).
#
# The admin JWT is a real sign-in's (RP56-5, Stage 1a Prompt 40): tenant zero's user is
# the admin, MFA on — sign in with the password AND the authenticator code, keep the token
# in a file only root reads, and pass the file. The script never mints a token (it used to
# sign its own with the box's JWT signing secret: audit rows and admin reads with no sign-in or MFA
# behind them); it checks the token names tenant zero's user and lasts the run.
#
#   curl -s https://stored.ge/auth/login -H 'Content-Type: application/json' \
#     -d '{"email":"…","password":"…","code":"123456"}' | jq -r .token > tzp.jwt   # 24 h
#   (copy it to the box as /root/tzp.jwt, mode 0600)
#   sudo -S -p '' TZP_TENANT=tenant-… TZP_ADMIN_JWT_FILE=/root/tzp.jwt bash tenant-zero-proof.sh …
#
# Root on the box: psql as the postgres role, /metrics on the active slot. The management
# and admin API go to the active slot; the S3 calls go to TZP_ENDPOINT. A temporary key
# (`p39-proof-temp`) is minted through POST /api/v1/manage/keys and deleted on exit.
#
# Lab: TZP_ADMIN_BASE, TZP_ENDPOINT and TZP_PSQL point it at a local server;
# TZP_ADMIN_JWT (the token itself) may stand in for the file there.
set -euo pipefail

TZP_JWT_MIN_LEFT=${TZP_JWT_MIN_LEFT:-1800} # the token must outlive the run: refused with less left
TZP_ENDPOINT=${TZP_ENDPOINT:-https://stored.ge}
TZP_PSQL=${TZP_PSQL:-runuser -u postgres -- psql -d vaultaire}
TZP_FLAG_SETTLE=${TZP_FLAG_SETTLE:-20} # the flag service caches ~15 s
TZP_KEY_NAME=${TZP_KEY_NAME:-p39-proof-temp-$(date -u +%Y%m%dT%H%M%S)}
TZP_DOGFOOD_BUCKET=${TZP_DOGFOOD_BUCKET:-stored-tenant-zero}

MIB=1048576

# --- pure helpers (pg-backup.sh's pattern: the _test.sh sources this file) -------------

# jwt_check TOKEN TENANT USER_ID MIN_LEFT — the token names USER_ID and TENANT and is
# valid for at least MIN_LEFT more seconds. Its payload only: the server checks the
# signature (and users.password_changed_at) on every call.
jwt_check() {
  T="$1" TT="$2" U="$3" L="$4" python3 - <<'PY'
import os, sys, json, base64, time
try:
    p = os.environ["T"].split(".")[1]
    c = json.loads(base64.urlsafe_b64decode(p + "=" * (-len(p) % 4)))
except Exception:
    sys.exit("the admin JWT is not a JWT")
if c.get("tenant_id") != os.environ["TT"] or c.get("user_id") != os.environ["U"]:
    sys.exit("the admin JWT is not tenant zero's user's (user_id/tenant_id)")
left = int(c.get("exp", 0)) - int(time.time())
if left < int(os.environ["L"]):
    sys.exit("the admin JWT has %ds left; the run needs %ss — sign in again" % (max(left, 0), os.environ["L"]))
PY
}

# valid_tenant ID — a real tenant id: never empty, never the global '*' row.
valid_tenant() {
  [[ "$1" =~ ^tenant-[a-z0-9-]+$ ]]
}

# flag_body ENABLED TENANT — the body of PUT /api/v1/admin/flags/{key}. An empty
# tenant_id there means the GLOBAL row: refused here, so no step can flip a flag for
# every tenant by a missing variable.
flag_body() {
  case "$1" in true|false) ;; *) echo "flag_body: enabled must be true|false" >&2; return 1 ;; esac
  if ! valid_tenant "$2"; then
    echo "flag_body: refusing tenant <$2>: a proof never writes a global row" >&2
    return 1
  fi
  printf '{"enabled":%s,"tenant_id":"%s"}' "$1" "$2"
}

# sha256_of FILE
sha256_of() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# make_body SIZE SEED FILE — SIZE bytes of AES-CTR keystream keyed by SEED: incompressible,
# never deduplicated against anything but itself, the same bytes for the same seed.
make_body() {
  local key iv
  key=$(printf '%s' "key:$2" | { sha256sum 2>/dev/null || shasum -a 256; } | cut -c1-64)
  iv=$(printf '%s' "iv:$2" | { sha256sum 2>/dev/null || shasum -a 256; } | cut -c1-32)
  head -c "$1" /dev/zero | openssl enc -aes-256-ctr -K "$key" -iv "$iv" -nosalt > "$3"
}

# slice FILE OFFSET LENGTH — the bytes a Range GET of bytes=OFFSET-(OFFSET+LENGTH-1) returns.
# A seek and one read: `tail | head` ends in SIGPIPE, which pipefail turns into a failure.
slice() {
  python3 -c 'import sys; f = open(sys.argv[1], "rb"); f.seek(int(sys.argv[2])); sys.stdout.buffer.write(f.read(int(sys.argv[3])))' "$1" "$2" "$3"
}

# metric_sum NAME [LABEL_SUBSTRING...] — sums every sample of NAME on stdin (Prometheus
# text) whose label set contains every LABEL_SUBSTRING; 0 when there is none.
metric_sum() {
  local name=$1; shift
  awk -v n="$name" -v want="$*" '
    BEGIN { k = split(want, w, " ") }
    $1 !~ /^#/ {
      split($1, p, "{"); if (p[1] != n) next
      for (i = 1; i <= k; i++) if (index($1, w[i]) == 0) next
      s += $NF
    }
    END { printf "%.0f\n", s + 0 }'
}

# mbps BYTES SECONDS
mbps() { awk -v b="$1" -v s="$2" 'BEGIN { if (s <= 0) print "n/a"; else printf "%.1f\n", b / s / 1e6 }'; }

now() { python3 -c 'import time; print("%.3f" % time.time())'; }
elapsed() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.3f\n", b - a }'; }

# --- the run ---------------------------------------------------------------------------

RESULTS=()
result() { # result STEP CHECK VALUE — one line of the verdict table
  RESULTS+=("$1|$2|$3")
  printf 'RESULT %-20s %-34s %s\n' "$1" "$2" "$3"
}
die() { echo "FATAL $*" >&2; exit 1; }

q() { # q SQL — one psql statement, unaligned
  local -a psql
  read -ra psql <<< "$TZP_PSQL"
  "${psql[@]}" -At -v ON_ERROR_STOP=1 -c "$1"
}

# load_admin_jwt — the token from TZP_ADMIN_JWT_FILE (a file other users cannot read)
# or TZP_ADMIN_JWT, into $WORK/auth; refuses to start without one.
ADMIN_JWT=
load_admin_jwt() {
  if [ -n "${TZP_ADMIN_JWT_FILE:-}" ]; then
    [ -f "$TZP_ADMIN_JWT_FILE" ] || die "TZP_ADMIN_JWT_FILE=$TZP_ADMIN_JWT_FILE is not a file"
    local mode
    mode=$(stat -c '%a' "$TZP_ADMIN_JWT_FILE" 2>/dev/null || stat -f '%Lp' "$TZP_ADMIN_JWT_FILE")
    [[ "$mode" =~ ^[0-7]?[0-7]00$ ]] || die "TZP_ADMIN_JWT_FILE: other users can read it (mode $mode); chmod 600"
    ADMIN_JWT=$(tr -d ' \n\r' < "$TZP_ADMIN_JWT_FILE")
  else
    ADMIN_JWT=${TZP_ADMIN_JWT:-}
  fi
  [ -n "$ADMIN_JWT" ] || die "no admin JWT: sign in with MFA (POST /auth/login with \"code\") and pass TZP_ADMIN_JWT_FILE — see the header"
}

# refresh_auth — the header of the next call; the token is re-checked (it must still have
# a minute) so a run that outlives it stops at a clean refusal, not a 401 mid-step.
refresh_auth() {
  jwt_check "$ADMIN_JWT" "$TZP_TENANT" "$USER_ID" 60 || die "admin JWT"
  printf 'Authorization: Bearer %s\n' "$ADMIN_JWT" > "$WORK/auth"
}

API_CODE=
api() { # api METHOD PATH [JSON] — management/admin API on the slot; body in $WORK/body, API_CODE set
  # (never called in $(…): API_CODE must land in this shell)
  refresh_auth
  if [ $# -ge 3 ]; then
    API_CODE=$(curl -s -o "$WORK/body" -w '%{http_code}' -H @"$WORK/auth" -H 'Content-Type: application/json' -X "$1" "$TZP_ADMIN_BASE$2" -d "$3")
  else
    API_CODE=$(curl -s -o "$WORK/body" -w '%{http_code}' -H @"$WORK/auth" -X "$1" "$TZP_ADMIN_BASE$2")
  fi
}

set_flag() { # set_flag KEY true|false — the tenant's row only
  local body
  body=$(flag_body "$2" "$TZP_TENANT") || die "set_flag $1"
  api PUT "/api/v1/admin/flags/$1" "$body"
  [ "$API_CODE" = 200 ] || die "PUT flag $1 for the tenant: HTTP $API_CODE"
  result flags "$1 for tenant zero" "$2 (row: $(q "SELECT enabled FROM feature_flags WHERE flag_key='$1' AND tenant_id='$TZP_TENANT'"); global row: $(q "SELECT coalesce((SELECT enabled::text FROM feature_flags WHERE flag_key='$1' AND tenant_id='*'),'none')"))"
  sleep "$TZP_FLAG_SETTLE"
}

s3() { aws --endpoint-url "$TZP_ENDPOINT" --output json s3api "$@"; }
s3cp() { aws --endpoint-url "$TZP_ENDPOINT" --only-show-errors s3 cp "$@"; }

metrics() { curl -s "$TZP_ADMIN_BASE/metrics"; }

create_bucket() { # create_bucket NAME [MODE]
  local body="{\"name\":\"$1\"}"
  [ $# -ge 2 ] && body="{\"name\":\"$1\",\"mode\":\"$2\"}"
  api POST /api/v1/manage/buckets "$body"
  [ "$API_CODE" = 201 ] || die "create bucket $1: HTTP $API_CODE"
}

empty_and_delete_bucket() { # every version, delete marker and open upload, then the bucket
  local b=$1 k v
  s3 list-multipart-uploads --bucket "$b" | python3 -c '
import sys, json
for u in (json.load(sys.stdin) or {}).get("Uploads") or []: print(u["Key"] + "\t" + u["UploadId"])' |
    while IFS=$'\t' read -r k v; do s3 abort-multipart-upload --bucket "$b" --key "$k" --upload-id "$v" > /dev/null; done
  s3 list-object-versions --bucket "$b" | python3 -c '
import sys, json
d = json.load(sys.stdin) or {}
for x in (d.get("Versions") or []) + (d.get("DeleteMarkers") or []): print(x["Key"] + "\t" + x["VersionId"])' |
    while IFS=$'\t' read -r k v; do s3 delete-object --bucket "$b" --key "$k" --version-id "$v" > /dev/null; done
  s3 list-objects-v2 --bucket "$b" | python3 -c '
import sys, json
for x in (json.load(sys.stdin) or {}).get("Contents") or []: print(x["Key"])' |
    while IFS= read -r k; do s3 delete-object --bucket "$b" --key "$k" > /dev/null; done
  api DELETE "/api/v1/manage/buckets/$b"
  [ "$API_CODE" = 204 ] || [ "$API_CODE" = 200 ] || die "delete bucket $b: HTTP $API_CODE"
}

bench_bucket() { # the one bench bucket of this run (created by chunking, removed by bench_cleanup)
  q "SELECT name FROM buckets WHERE tenant_id='$TZP_TENANT' AND name LIKE 'tz-proof-%' ORDER BY created_at DESC LIMIT 1"
}

head_row() { # head_row BUCKET KEY — backend / chunked / size / floor / storage_key
  q "SELECT coalesce(backend_name,'NULL')||' / '||is_chunked||' / '||size_bytes||' / '||coalesce(floor,'')||' / '||coalesce(storage_key,'') FROM object_head_cache WHERE tenant_id='$TZP_TENANT' AND bucket='$1' AND object_key='$2'"
}

head_addr() { # head_addr BUCKET KEY — where the bytes (or the manifest) are: storage_key, else the key
  q "SELECT coalesce(storage_key, object_key) FROM object_head_cache WHERE tenant_id='$TZP_TENANT' AND bucket='$1' AND object_key='$2'"
}

chunk_refs() { # chunk_refs BUCKET MANIFEST_ADDRESS — refs, distinct chunks, ready GCI rows
  q "SELECT count(*)||' refs, '||count(DISTINCT r.plaintext_hash)||' chunks, '||count(DISTINCT g.plaintext_hash) FILTER (WHERE g.state='ready')||' ready GCI rows' FROM tenant_chunk_refs r LEFT JOIN global_content_index g ON g.plaintext_hash=r.plaintext_hash AND g.dedup_scope=r.dedup_scope WHERE r.tenant_id='$TZP_TENANT' AND r.bucket_name='$1' AND r.object_key='$2'"
}

get_compare() { # get_compare STEP LABEL BUCKET KEY FILE [extra get-object args…] — whole GET, sha, MB/s
  local step=$1 label=$2 b=$3 k=$4 f=$5 t0 t1 got
  shift 5
  t0=$(now)
  s3 get-object --bucket "$b" --key "$k" "$@" "$WORK/got" > /dev/null
  t1=$(now)
  got=$(sha256_of "$WORK/got")
  if [ "$got" = "$(sha256_of "$f")" ]; then
    result "$step" "$label" "byte-identical ($(stat -c %s "$WORK/got" 2>/dev/null || stat -f %z "$WORK/got") B, $(mbps "$(wc -c < "$f")" "$(elapsed "$t0" "$t1")") MB/s)"
  else
    result "$step" "$label" "MISMATCH sha $got"
  fi
  rm -f "$WORK/got"
}

range_compare() { # range_compare STEP BUCKET KEY FILE OFFSET LENGTH
  local end=$(( $5 + $6 - 1 ))
  s3 get-object --bucket "$2" --key "$3" --range "bytes=$5-$end" "$WORK/range" > /dev/null
  slice "$4" "$5" "$6" > "$WORK/want"
  if cmp -s "$WORK/range" "$WORK/want"; then result "$1" "Range bytes=$5-$end" "identical ($6 B)"
  else result "$1" "Range bytes=$5-$end" "MISMATCH"; fi
  rm -f "$WORK/range" "$WORK/want"
}

put_size() { # put_size STEP KEY FILE BUCKET — a single PUT when it can pass the edge, else multipart
  local step=$1 k=$2 f=$3 b=$4 err
  if err=$(s3 put-object --bucket "$b" --key "$k" --body "$f" 2>&1 >/dev/null); then
    result "$step" "single PUT $k" "200"
  else
    result "$step" "single PUT $k" "refused: $(printf '%s' "$err" | tr '\n' ' ' | cut -c1-160) → aws s3 cp (multipart)"
    s3cp "$f" "s3://$b/$k"
  fi
}

# --- steps -----------------------------------------------------------------------------

step_preflight() {
  result preflight "slot version" "$(curl -s "$TZP_ADMIN_BASE/version")"
  result preflight "tenant" "$TZP_TENANT ($(q "SELECT count(*) FROM tenants WHERE id='$TZP_TENANT'") row)"
  result preflight "objects / buckets" "$(q "SELECT count(*) FROM object_head_cache WHERE tenant_id='$TZP_TENANT'") / $(q "SELECT count(*) FROM buckets WHERE tenant_id='$TZP_TENANT'")"
  result preflight "quota (limit, used, tier)" "$(q "SELECT storage_limit_bytes||', '||storage_used_bytes||', '||coalesce(tier,'') FROM tenant_quotas WHERE tenant_id='$TZP_TENANT'")"
  result preflight "tenant flag rows" "$(q "SELECT coalesce(string_agg(flag_key||'='||enabled, ' ' ORDER BY flag_key),'none') FROM feature_flags WHERE tenant_id='$TZP_TENANT'")"
  result preflight "global rows of the proven flags" "$(q "SELECT coalesce(string_agg(flag_key||'='||enabled, ' '),'none') FROM feature_flags WHERE tenant_id='*' AND flag_key IN ('chunking','parallel_get','versioned_chunking','modes_copy')")"
  result preflight "chunked head rows on prod (routing_truth samples 20)" "$(q "SELECT count(*) FROM object_head_cache WHERE is_chunked")"
}

step_chunking() {
  local b k=chunked-100m.bin
  set_flag chunking true
  b=tz-proof-$(date +%s)
  create_bucket "$b"
  result chunking "bench bucket" "$b (standard)"
  make_body $((100 * MIB)) "chunk-$b" "$WORK/c100"
  put_size chunking "$k" "$WORK/c100" "$b"
  result chunking "head row (backend / chunked / size / floor / addr)" "$(head_row "$b" "$k")"
  result chunking "chunk index" "$(chunk_refs "$b" "$(head_addr "$b" "$k")")"
  local etag; etag=$(s3 head-object --bucket "$b" --key "$k" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d["ETag"], d["ContentLength"], d.get("StorageClass","STANDARD"))')
  result chunking "HEAD (etag size class)" "$etag (cache row etag $(q "SELECT etag FROM object_head_cache WHERE tenant_id='$TZP_TENANT' AND bucket='$b' AND object_key='$k'"))"
  get_compare chunking "whole GET" "$b" "$k" "$WORK/c100"
  range_compare chunking "$b" "$k" "$WORK/c100" 1000000 2000000
  range_compare chunking "$b" "$k" "$WORK/c100" $((100 * MIB - 4096)) 4096
}

step_multipart() {
  local b k=multipart-96m.bin before after
  b=$(bench_bucket); [ -n "$b" ] || die "multipart: no bench bucket (run chunking first)"
  make_body $((96 * MIB)) "mpu-$b" "$WORK/m96"
  before=$(q "SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id='$TZP_TENANT'")
  s3cp "$WORK/m96" "s3://$b/$k"
  after=$(q "SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id='$TZP_TENANT'")
  result multipart "ETag" "$(s3 head-object --bucket "$b" --key "$k" | python3 -c 'import sys,json; print(json.load(sys.stdin)["ETag"])')"
  result multipart "head row (backend / chunked / size / floor / addr)" "$(head_row "$b" "$k")"
  result multipart "chunk index" "$(chunk_refs "$b" "$(head_addr "$b" "$k")")"
  result multipart "upload row (status, reserved_bytes)" "$(q "SELECT status||', '||reserved_bytes FROM multipart_uploads WHERE tenant_id='$TZP_TENANT' AND bucket='$b' AND object_key='$k' ORDER BY created_at DESC LIMIT 1")"
  result multipart "tenant_quotas used delta" "$((after - before)) (object $((96 * MIB)))"
  result multipart "active holdings of the tenant" "$(q "SELECT count(*)||' rows, '||coalesce(sum(reserved_bytes),0)||' B' FROM multipart_uploads WHERE tenant_id='$TZP_TENANT' AND status='active'")"
  get_compare multipart "whole GET" "$b" "$k" "$WORK/m96"
}

step_parallel_get() {
  local b k=whole-256m.bin c0 c1 c2 t0 t1 size=$((256 * MIB))
  b=$(bench_bucket); [ -n "$b" ] || die "parallel_get: no bench bucket (run chunking first)"
  # A whole object on the primary: a versioned bucket stores whole while the tenant's
  # versioned_chunking is off (placement.go) — so this step runs BEFORE that flag.
  [ "$(q "SELECT coalesce((SELECT enabled::text FROM feature_flags WHERE flag_key='versioned_chunking' AND tenant_id='$TZP_TENANT'),'none')")" != true ] ||
    die "parallel_get: versioned_chunking is already on for the tenant; the 256 MiB object would be chunked"
  s3 put-bucket-versioning --bucket "$b" --versioning-configuration Status=Enabled
  make_body "$size" "whole-$b" "$WORK/w256"
  s3cp "$WORK/w256" "s3://$b/$k"
  result parallel_get "head row (backend / chunked / size / floor / addr)" "$(head_row "$b" "$k")"
  c0=$(metrics | metric_sum vaultaire_large_get_parallel_reads_total)
  t0=$(now); s3 get-object --bucket "$b" --key "$k" "$WORK/got" > /dev/null; t1=$(now)
  c1=$(metrics | metric_sum vaultaire_large_get_parallel_reads_total)
  result parallel_get "GET, flag OFF" "$(mbps "$size" "$(elapsed "$t0" "$t1")") MB/s, parallel reads +$((c1 - c0))"
  rm -f "$WORK/got"
  set_flag parallel_get true
  c1=$(metrics | metric_sum vaultaire_large_get_parallel_reads_total)
  local r1; r1=$(metrics | metric_sum vaultaire_large_get_parallel_reads_total 'path="ranged"')
  get_compare parallel_get "GET, flag ON" "$b" "$k" "$WORK/w256"
  c2=$(metrics | metric_sum vaultaire_large_get_parallel_reads_total)
  result parallel_get "parallel reads (all / ranged)" "+$((c2 - c1)) / +$(( $(metrics | metric_sum vaultaire_large_get_parallel_reads_total 'path="ranged"') - r1 ))"
  result parallel_get "counter by backend,path now" "$(metrics | grep '^vaultaire_large_get_parallel_reads_total' | tr '\n' ' ')"
  range_compare parallel_get "$b" "$k" "$WORK/w256" $((100 * MIB)) $((80 * MIB))
}

step_versioned_chunking() {
  local b k=versioned-100m.bin v1 v2 addr1
  b=$(bench_bucket); [ -n "$b" ] || die "versioned_chunking: no bench bucket (run chunking first)"
  result versioned_chunking "bucket versioning" "$(q "SELECT coalesce(versioning_status,'') FROM buckets WHERE tenant_id='$TZP_TENANT' AND name='$b'")"
  set_flag versioned_chunking true
  make_body $((100 * MIB)) "v1-$b" "$WORK/v1"
  make_body $((100 * MIB)) "v2-$b" "$WORK/v2"
  put_size versioned_chunking "$k" "$WORK/v1" "$b"
  v1=$(s3 head-object --bucket "$b" --key "$k" | python3 -c 'import sys,json; print(json.load(sys.stdin)["VersionId"])')
  addr1=$(q "SELECT storage_key FROM object_versions WHERE tenant_id='$TZP_TENANT' AND bucket='$b' AND object_key='$k' AND version_id='$v1'")
  result versioned_chunking "v1 head row" "$(head_row "$b" "$k")"
  put_size versioned_chunking "$k" "$WORK/v2" "$b"
  v2=$(s3 head-object --bucket "$b" --key "$k" | python3 -c 'import sys,json; print(json.load(sys.stdin)["VersionId"])')
  result versioned_chunking "versions (id backend latest addr)" "$(q "SELECT string_agg(version_id||' '||coalesce(backend_name,'')||' '||is_latest||' '||coalesce(storage_key,''), ' ; ' ORDER BY created_at) FROM object_versions WHERE tenant_id='$TZP_TENANT' AND bucket='$b' AND object_key='$k'")"
  result versioned_chunking "v1 manifest still indexed" "$(chunk_refs "$b" "$addr1")"
  get_compare versioned_chunking "GET ?versionId=v1" "$b" "$k" "$WORK/v1" --version-id "$v1"
  get_compare versioned_chunking "GET latest (v2 $v2)" "$b" "$k" "$WORK/v2"
  s3 delete-object --bucket "$b" --key "$k" --version-id "$v1" > /dev/null
  result versioned_chunking "after DELETE ?versionId=v1: retired_blobs" "$(q "SELECT coalesce(string_agg(backend_id||' '||reason||' '||retire_after, ' ; '),'none') FROM retired_blobs WHERE tenant_id='$TZP_TENANT' AND storage_key='$addr1'")"
  result versioned_chunking "after DELETE: v1 rows" "$(q "SELECT count(*) FROM object_versions WHERE tenant_id='$TZP_TENANT' AND bucket='$b' AND version_id='$v1'")"
}

step_bench_cleanup() {
  local b
  b=$(bench_bucket); [ -n "$b" ] || { result bench_cleanup "bench bucket" "none"; return; }
  empty_and_delete_bucket "$b"
  result bench_cleanup "bench bucket $b" "deleted; heads $(q "SELECT count(*) FROM object_head_cache WHERE tenant_id='$TZP_TENANT' AND bucket='$b'"), versions $(q "SELECT count(*) FROM object_versions WHERE tenant_id='$TZP_TENANT' AND bucket='$b'"), buckets $(q "SELECT count(*) FROM buckets WHERE tenant_id='$TZP_TENANT' AND name='$b'")"
  result bench_cleanup "retired_blobs of the tenant (await the collector)" "$(q "SELECT coalesce(string_agg(backend_id||':'||reason||':'||n, ' '),'none') FROM (SELECT backend_id, reason, count(*) n FROM retired_blobs WHERE tenant_id='$TZP_TENANT' GROUP BY 1,2) x")"
  result bench_cleanup "tenant_quotas used" "$(q "SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id='$TZP_TENANT'")"
}

step_modes() {
  local b err key='tzp-ssec-key-0123456789abcdef-01' wrong='tzp-wrong-key-0123456789abcdef-1'
  b=tz-proof-private-$(date +%s)
  create_bucket "$b" private
  result modes "create mode=private" "201; row mode=$(q "SELECT mode||' ssec_required='||ssec_required FROM buckets WHERE tenant_id='$TZP_TENANT' AND name='$b'")"
  api GET "/api/v1/manage/buckets/$b"
  result modes "management API mode" "HTTP $API_CODE, mode=$(python3 -c 'import sys,json; print(json.load(open(sys.argv[1])).get("mode"))' "$WORK/body")"
  make_body $MIB "ssec-$b" "$WORK/s1"
  if err=$(s3 put-object --bucket "$b" --key unkeyed.bin --body "$WORK/s1" 2>&1 >/dev/null); then
    result modes "unkeyed PUT" "ACCEPTED (expected 400)"
  else
    result modes "unkeyed PUT" "refused: $(grep -o 'InvalidRequest\|([0-9]*)' <<< "$err" | head -1)"
  fi
  s3 put-object --bucket "$b" --key keyed.bin --body "$WORK/s1" --sse-customer-algorithm AES256 --sse-customer-key "$key" > /dev/null
  result modes "SSE-C PUT" "200; row enc=$(q "SELECT coalesce(encryption_algorithm,'none')||' key_md5_set='||(coalesce(ssec_key_md5,'')<>'') FROM object_head_cache WHERE tenant_id='$TZP_TENANT' AND bucket='$b' AND object_key='keyed.bin'")"
  get_compare modes "SSE-C GET with the key" "$b" keyed.bin "$WORK/s1" --sse-customer-algorithm AES256 --sse-customer-key "$key"
  if err=$(s3 head-object --bucket "$b" --key keyed.bin --sse-customer-algorithm AES256 --sse-customer-key "$wrong" 2>&1 >/dev/null); then
    result modes "HEAD with another key" "200 (expected 403)"
  else
    result modes "HEAD with another key" "refused: $(grep -o '([0-9]*)\|Forbidden' <<< "$err" | head -1)"
  fi
  if err=$(s3 get-object --bucket "$b" --key keyed.bin "$WORK/x" 2>&1 >/dev/null); then
    result modes "GET without the key" "200 (expected 400)"
  else
    result modes "GET without the key" "refused: $(grep -o '([0-9A-Za-z]*)' <<< "$err" | head -1)"
  fi
  rm -f "$WORK/x"
  result modes "bucket.mode_changed audit rows (create)" "$(q "SELECT count(*) FROM audit_logs WHERE tenant_id='$TZP_TENANT' AND action='bucket.mode_changed' AND metadata::text LIKE '%$b%'")"
  empty_and_delete_bucket "$b"
  result modes "private bucket removed" "buckets left named $b: $(q "SELECT count(*) FROM buckets WHERE tenant_id='$TZP_TENANT' AND name='$b'")"
  result modes "modes_copy for the tenant" "$(q "SELECT coalesce((SELECT enabled::text FROM feature_flags WHERE flag_key='modes_copy' AND tenant_id='$TZP_TENANT'),'none')") (renders on the dashboard bucket page — an MFA session, the owner's)"
}

step_dogfood() {
  local b=$TZP_DOGFOOD_BUCKET f n=0
  if [ -z "${TZP_DOGFOOD_DIR:-}" ] || [ ! -d "$TZP_DOGFOOD_DIR" ]; then
    die "dogfood: TZP_DOGFOOD_DIR names no directory"
  fi
  if [ "$(q "SELECT count(*) FROM buckets WHERE tenant_id='$TZP_TENANT' AND name='$b'")" = 0 ]; then
    create_bucket "$b"
  fi
  for f in "$TZP_DOGFOOD_DIR"/*; do
    [ -f "$f" ] || continue
    s3 put-object --bucket "$b" --key "$(basename "$f")" --body "$f" > /dev/null
    n=$((n + 1))
  done
  result dogfood "bucket $b" "$n files put; rows $(q "SELECT count(*)||' objects, '||coalesce(sum(size_bytes),0)||' B, backends '||coalesce(string_agg(DISTINCT backend_name, ','),'') FROM object_head_cache WHERE tenant_id='$TZP_TENANT' AND bucket='$b'")"
}

step_transparency() {
  # One admin read whose subject is the tenant: its own audit trail (the flag rows this
  # proof wrote). translog_admin_reads is on for tenant zero, so it is a logged entry.
  local n
  api GET "/api/v1/admin/audit?tenant_id=$TZP_TENANT&limit=50"
  result transparency "admin read: GET /api/v1/admin/audit?tenant_id=…" "HTTP $API_CODE, flag.set rows for the tenant: $(q "SELECT count(*) FROM audit_logs WHERE tenant_id='$TZP_TENANT' AND action LIKE 'flag.%'")"
  sleep 2
  api GET /api/v1/manage/account/transparency
  cp "$WORK/body" "$WORK/tr.json"
  n=$(python3 -c '
import sys, json
d = json.load(open(sys.argv[1]))
xs = d.get("entries") or d.get("data") or d.get("reads") or []
print(len(xs))' "$WORK/tr.json" 2>/dev/null || echo "?")
  result transparency "GET /api/v1/manage/account/transparency" "HTTP $API_CODE, $n entries; $(head -c 400 "$WORK/tr.json" | tr '\n' ' ')"
  result transparency "translog entries naming the tenant's reads" "$(q "SELECT count(*) FROM translog_entries WHERE kind='admin_read'" 2>/dev/null || echo '?') admin_read entries in the log"
}

step_morning() {
  local j
  for j in routing_truth dedup_gc retired_blobs chunk_orphans; do
    result morning "job $j" "$(q "SELECT last_outcome||' '||coalesce(last_success_at::text,'')||' '||left(coalesce(last_error,''),120) FROM job_runs WHERE job='$j'")"
  done
  api GET /api/v1/admin/routing-truth
  cp "$WORK/body" "$WORK/rt.json"
  result morning "GET /api/v1/admin/routing-truth" "HTTP $API_CODE; chunked: $(python3 -c '
import sys, json
d = json.load(open(sys.argv[1]))
def find(o, k):
    if isinstance(o, dict):
        for kk, v in o.items():
            if k in kk: yield kk, v
            yield from find(v, k)
    elif isinstance(o, list):
        for v in o: yield from find(v, k)
print("; ".join("%s=%s" % (k, json.dumps(v)[:200]) for k, v in find(d, "chunk")))' "$WORK/rt.json" 2>/dev/null | cut -c1-600)"
  result morning "retired_blobs of the tenant" "$(q "SELECT count(*) FROM retired_blobs WHERE tenant_id='$TZP_TENANT'")"
  result morning "tenant_quotas used / heads" "$(q "SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id='$TZP_TENANT'") / $(q "SELECT count(*)||' rows, '||coalesce(sum(size_bytes),0)||' B' FROM object_head_cache WHERE tenant_id='$TZP_TENANT'")"
}

# --- main ------------------------------------------------------------------------------

cleanup() {
  local rc=$?
  if [ -n "${KEY_ID:-}" ]; then
    api DELETE "/api/v1/manage/keys/$KEY_ID" || true
    echo "temp key deleted: HTTP $API_CODE"
  fi
  [ -n "${WORK:-}" ] && rm -rf "$WORK"
  exit "$rc"
}

main() {
  [ $# -ge 1 ] || die "usage: TZP_TENANT=tenant-… $0 STEP…"
  valid_tenant "${TZP_TENANT:-}" || die "TZP_TENANT must name one tenant (tenant-…), never empty or '*'"
  if [ -z "${TZP_ADMIN_BASE:-}" ]; then
    TZP_ADMIN_BASE=http://127.0.0.1:$(cat /opt/vaultaire/ACTIVE_PORT)
  fi
  local steps=("$@")
  case "$1" in
    all) steps=(preflight chunking multipart parallel_get versioned_chunking bench_cleanup modes dogfood transparency) ;;
    evening) steps=(preflight chunking multipart parallel_get versioned_chunking) ;;
    morning-all) steps=(morning bench_cleanup modes dogfood transparency) ;;
  esac

  load_admin_jwt
  WORK=$(mktemp -d); chmod 700 "$WORK"
  trap cleanup EXIT
  USER_EMAIL=$(q "SELECT email FROM tenants WHERE id='$TZP_TENANT'")
  USER_ID=$(q "SELECT id FROM users WHERE email='$USER_EMAIL'")
  [ -n "$USER_ID" ] || die "no user for the tenant"
  jwt_check "$ADMIN_JWT" "$TZP_TENANT" "$USER_ID" "$TZP_JWT_MIN_LEFT" || die "admin JWT"

  local kj
  api POST /api/v1/manage/keys "{\"name\":\"$TZP_KEY_NAME\"}"
  [ "$API_CODE" = 201 ] || [ "$API_CODE" = 200 ] || die "mint temp key: HTTP $API_CODE"
  kj=$(cat "$WORK/body"); rm -f "$WORK/body"
  KEY_ID=$(python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])' <<< "$kj")
  export AWS_SHARED_CREDENTIALS_FILE=$WORK/credentials AWS_CONFIG_FILE=$WORK/config
  python3 -c '
import sys, json
d = json.load(sys.stdin)
print("[default]\naws_access_key_id = %s\naws_secret_access_key = %s" % (d["key"], d["secret"]))' <<< "$kj" > "$AWS_SHARED_CREDENTIALS_FILE"
  unset kj
  printf '[default]\nregion = us-east-1\ns3 =\n  multipart_threshold = 8MB\n  multipart_chunksize = 16MB\n  addressing_style = path\n' > "$AWS_CONFIG_FILE"
  result run "temp key" "minted ($TZP_KEY_NAME); S3 endpoint $TZP_ENDPOINT; API $TZP_ADMIN_BASE"

  local s
  for s in "${steps[@]}"; do
    echo "=== $s $(date -u +%H:%M:%SZ)"
    "step_$s"
  done
  echo
  echo "| step | check | value |"
  echo "|---|---|---|"
  local r
  for r in "${RESULTS[@]}"; do echo "| ${r//|/ | } |"; done
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
