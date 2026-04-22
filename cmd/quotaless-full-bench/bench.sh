#!/bin/bash
# Quotaless comprehensive benchmark via rclone (bypasses AWS SDK v2 issues).
# Tests S3 + WebDAV protocols across all endpoints.
#
# Usage:
#   export QUOTALESS_ACCESS_KEY="your-token"
#   bash cmd/quotaless-full-bench/bench.sh [quick|full]

set -euo pipefail

MODE="${1:-full}"
OUTDIR="bench-results/quotaless-$(hostname)-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$OUTDIR"
CONF="/tmp/rclone-quotaless-bench.conf"

AK="${QUOTALESS_ACCESS_KEY:?Set QUOTALESS_ACCESS_KEY}"
QS="${QUOTALESS_SECRET_KEY:-gatewaysecret}"
QU="${QUOTALESS_USERNAME:-teamviera}"
QP="${QUOTALESS_PASSWORD:-}"

# --- Generate rclone config for all endpoints ---
cat > "$CONF" << EOF
[srv1]
type = s3
provider = Minio
access_key_id = $AK
secret_access_key = $QS
endpoint = https://srv1.quotaless.cloud:8000
acl = bucket-owner-full-control
no_check_bucket = true
upload_cutoff = 100M
chunk_size = 50M
encoding = Slash,InvalidUtf8,Dot,Percent,Ctl
use_multipart_uploads = true
no_head = false

[srv2]
type = s3
provider = Minio
access_key_id = $AK
secret_access_key = $QS
endpoint = https://srv2.quotaless.cloud:8000
acl = bucket-owner-full-control
no_check_bucket = true
upload_cutoff = 100M
chunk_size = 50M
encoding = Slash,InvalidUtf8,Dot,Percent,Ctl
use_multipart_uploads = true
no_head = false

[us]
type = s3
provider = Minio
access_key_id = $AK
secret_access_key = $QS
endpoint = https://us.quotaless.cloud:8000
acl = bucket-owner-full-control
no_check_bucket = true
upload_cutoff = 100M
chunk_size = 50M
encoding = Slash,InvalidUtf8,Dot,Percent,Ctl
use_multipart_uploads = true
no_head = false

[nl]
type = s3
provider = Minio
access_key_id = $AK
secret_access_key = $QS
endpoint = https://nl.quotaless.cloud:8000
acl = bucket-owner-full-control
no_check_bucket = true
upload_cutoff = 100M
chunk_size = 50M
encoding = Slash,InvalidUtf8,Dot,Percent,Ctl
use_multipart_uploads = true
no_head = false

[sg]
type = s3
provider = Minio
access_key_id = $AK
secret_access_key = $QS
endpoint = https://sg.quotaless.cloud:8000
acl = bucket-owner-full-control
no_check_bucket = true
upload_cutoff = 100M
chunk_size = 50M
encoding = Slash,InvalidUtf8,Dot,Percent,Ctl
use_multipart_uploads = true
no_head = false

[io]
type = s3
provider = Minio
access_key_id = $AK
secret_access_key = $QS
endpoint = https://io.quotaless.cloud:8000
acl = bucket-owner-full-control
no_check_bucket = true
upload_cutoff = 100M
chunk_size = 50M
encoding = Slash,InvalidUtf8,Dot,Percent,Ctl
use_multipart_uploads = false
no_head = true
EOF

# Add WebDAV if password is set
if [ -n "$QP" ]; then
  RCLONE_PASS=$(rclone --config "$CONF" obscure "$QP" 2>/dev/null || echo "")
  if [ -n "$RCLONE_PASS" ]; then
    cat >> "$CONF" << EOF2

[webdav-us]
type = webdav
url = https://us.quotaless.cloud:8080/webdav
user = $QU
pass = $RCLONE_PASS
vendor = owncloud
EOF2
  fi
fi

RC="rclone --config $CONF"
BENCH_PREFIX="data/personal-files/bench-$(date +%s)"

echo "╔══════════════════════════════════════════════════════════════╗"
echo "║          QUOTALESS COMPREHENSIVE BENCHMARK (rclone)         ║"
echo "╠══════════════════════════════════════════════════════════════╣"
echo "  Host:   $(hostname) ($(uname -s)/$(uname -m))"
echo "  Mode:   $MODE"
echo "  Output: $OUTDIR"
echo "  Prefix: $BENCH_PREFIX"
echo "══════════════════════════════════════════════════════════════"

# --- Generate test files ---
echo ""
echo "Generating test files..."
for sz in 1024 65536 1048576 16777216 67108864; do
  label=$(numfmt --to=iec-i $sz 2>/dev/null || echo "${sz}B")
  dd if=/dev/urandom of="/tmp/q-${sz}.bin" bs=$sz count=1 2>/dev/null
  echo "  $label: $(sha256sum /tmp/q-${sz}.bin | cut -c1-16)..."
done
if [ "$MODE" = "full" ]; then
  dd if=/dev/urandom of="/tmp/q-268435456.bin" bs=1M count=256 2>/dev/null
  echo "  256Mi: $(sha256sum /tmp/q-268435456.bin | cut -c1-16)..."
fi

# --- Helper: benchmark a single upload+download+verify ---
bench_one() {
  local remote=$1 size=$2 label=$3
  local src="/tmp/q-${size}.bin"
  local key="${BENCH_PREFIX}/${remote}-${label}.bin"
  local expected=$(sha256sum "$src" | cut -d' ' -f1)

  # Upload
  local up_start=$(date +%s%N)
  $RC copyto "$src" "${remote}:/${key}" 2>/dev/null
  local up_end=$(date +%s%N)
  local up_ms=$(( (up_end - up_start) / 1000000 ))
  local up_mbps=$(echo "scale=1; $size / 1048576 / ($up_ms / 1000)" | bc 2>/dev/null || echo "?")

  # Download + verify
  local dl_start=$(date +%s%N)
  local actual=$($RC cat "${remote}:/${key}" 2>/dev/null | sha256sum | cut -d' ' -f1)
  local dl_end=$(date +%s%N)
  local dl_ms=$(( (dl_end - dl_start) / 1000000 ))
  local dl_mbps=$(echo "scale=1; $size / 1048576 / ($dl_ms / 1000)" | bc 2>/dev/null || echo "?")

  local integrity="✓"
  [ "$expected" != "$actual" ] && integrity="✗"

  printf "  %-8s %-6s  UP %7sms %7s MB/s  DL %7sms %7s MB/s  %s\n" \
    "$remote" "$label" "$up_ms" "$up_mbps" "$dl_ms" "$dl_mbps" "$integrity"

  echo "${remote},${label},${size},${up_ms},${up_mbps},${dl_ms},${dl_mbps},${integrity}" >> "$OUTDIR/results.csv"

  # Cleanup
  $RC delete "${remote}:/${key}" 2>/dev/null || true
}

# --- Helper: concurrent upload test ---
bench_concurrent() {
  local remote=$1 workers=$2 duration=$3 filesize=$4
  local label="${workers}w×$(numfmt --to=iec-i $filesize 2>/dev/null)"
  local src="/tmp/q-${filesize}.bin"
  [ ! -f "$src" ] && dd if=/dev/urandom of="$src" bs=$filesize count=1 2>/dev/null

  local total_ops=0
  local total_bytes=0
  local errors=0
  local start=$(date +%s)
  local deadline=$((start + duration))
  local pids=""

  for w in $(seq 1 $workers); do
    (
      local ops=0
      while [ $(date +%s) -lt $deadline ]; do
        local key="${BENCH_PREFIX}/conc-${remote}-${w}-${ops}-$(date +%s%N).bin"
        if $RC copyto "$src" "${remote}:/${key}" 2>/dev/null; then
          echo "OK $filesize" >> "/tmp/q-conc-${remote}-${w}.log"
          $RC delete "${remote}:/${key}" 2>/dev/null || true
        else
          echo "ERR" >> "/tmp/q-conc-${remote}-${w}.log"
        fi
        ops=$((ops + 1))
      done
    ) &
    pids="$pids $!"
  done

  wait $pids 2>/dev/null || true
  local elapsed=$(($(date +%s) - start))

  # Aggregate results
  for w in $(seq 1 $workers); do
    local f="/tmp/q-conc-${remote}-${w}.log"
    if [ -f "$f" ]; then
      local ok_c; ok_c=$(grep -c "^OK" "$f" 2>/dev/null) || ok_c=0
      local er_c; er_c=$(grep -c "^ERR" "$f" 2>/dev/null) || er_c=0
      total_ops=$((total_ops + ok_c))
      errors=$((errors + er_c))
      rm -f "$f"
    fi
  done
  total_bytes=$((total_ops * filesize))
  local mbps=$(echo "scale=1; $total_bytes / 1048576 / $elapsed" | bc 2>/dev/null || echo "?")
  local ops_s=$(echo "scale=1; $total_ops / $elapsed" | bc 2>/dev/null || echo "?")

  printf "  %-8s %-12s  %4d ops  %3d err  %7s MB/s  %5s ops/s  (%ds)\n" \
    "$remote" "$label" "$total_ops" "$errors" "$mbps" "$ops_s" "$elapsed"
  echo "${remote},concurrent_${label},${total_bytes},${elapsed}000,${mbps},0,0,${errors}" >> "$OUTDIR/results.csv"
}

# --- CSV header ---
echo "remote,workload,bytes,up_ms,up_mbps,dl_ms,dl_mbps,integrity" > "$OUTDIR/results.csv"

# ===== PHASE 1: Single-file throughput on each S3 endpoint =====
echo ""
echo "═══ PHASE 1: Single-file throughput (S3 endpoints) ═══"

if [ "$MODE" = "quick" ]; then
  S3_ENDPOINTS="srv1 us io"
  SIZES="1048576 16777216"
else
  S3_ENDPOINTS="srv1 srv2 us nl sg io"
  SIZES="1024 65536 1048576 16777216 67108864"
fi

for ep in $S3_ENDPOINTS; do
  echo ""
  echo "--- $ep ---"
  for sz in $SIZES; do
    label=$(numfmt --to=iec-i $sz 2>/dev/null || echo "${sz}B")
    bench_one "$ep" "$sz" "$label"
  done
  if [ "$MODE" = "full" ]; then
    bench_one "$ep" "268435456" "256Mi"
  fi
done

# ===== PHASE 2: Concurrent upload stress =====
echo ""
echo "═══ PHASE 2: Concurrent upload stress (20s) ═══"

for ep in srv1 us io; do
  echo ""
  echo "--- $ep ---"
  bench_concurrent "$ep" 4 20 1048576
  bench_concurrent "$ep" 8 20 1048576
  if [ "$MODE" = "full" ]; then
    bench_concurrent "$ep" 16 20 1048576
    bench_concurrent "$ep" 4 20 16777216
  fi
done

# ===== PHASE 3: Cross-server consistency =====
echo ""
echo "═══ PHASE 3: Cross-server read-after-write consistency ═══"
echo "  Upload to srv1, immediately read from srv2, us, io..."
dd if=/dev/urandom of="/tmp/q-raw.bin" bs=1M count=1 2>/dev/null
EXPECTED=$(sha256sum /tmp/q-raw.bin | cut -d' ' -f1)
RAW_KEY="${BENCH_PREFIX}/consistency-test.bin"

$RC copyto "/tmp/q-raw.bin" "srv1:/${RAW_KEY}" 2>/dev/null
echo "  Uploaded to srv1. SHA: ${EXPECTED:0:16}"

for reader in srv1 srv2 us nl sg io; do
  ACTUAL=$($RC cat "${reader}:/${RAW_KEY}" 2>/dev/null | sha256sum | cut -d' ' -f1)
  if [ "$EXPECTED" = "$ACTUAL" ]; then
    echo "  Read from $reader: ✓ MATCH"
  else
    echo "  Read from $reader: ✗ MISMATCH (got ${ACTUAL:0:16})"
  fi
done
$RC delete "srv1:/${RAW_KEY}" 2>/dev/null || true

# ===== PHASE 4: WebDAV (if configured) =====
if $RC lsf webdav-us:/ 2>/dev/null | head -1 > /dev/null 2>&1; then
  echo ""
  echo "═══ PHASE 4: WebDAV throughput ═══"
  for sz in 1048576 16777216; do
    label=$(numfmt --to=iec-i $sz 2>/dev/null)
    bench_one "webdav-us" "$sz" "$label"
  done
  if [ "$MODE" = "full" ]; then
    bench_one "webdav-us" "67108864" "64Mi"
  fi
else
  echo ""
  echo "═══ PHASE 4: WebDAV — skipped (set QUOTALESS_PASSWORD to test) ═══"
fi

echo ""
echo "══════════════════════════════════════════════════════════════"
echo "✅ Done. Results: $OUTDIR/results.csv"
echo "══════════════════════════════════════════════════════════════"
