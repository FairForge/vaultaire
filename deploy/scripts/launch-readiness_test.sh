#!/bin/bash
# launch-readiness_test.sh — tests for deploy/scripts/launch-readiness.sh (Stage 1a Prompt
# 43 Part A). Run from anywhere: bash deploy/scripts/launch-readiness_test.sh
# CI runs it (ci.yml "Deploy scripts"). Every source is a fake: curl (the slot's /metrics
# and Alertmanager), psql (LR_PSQL, answers by the query's `lr:<tag>`), journalctl,
# systemctl. Each gate is driven GREEN, RED and — where it reads a source — UNKNOWN when
# that source does not answer (never GREEN by default). An .env holding a sentinel
# secret must never appear in the output.
# shellcheck disable=SC2016 # the bash -c bodies expand in the inner shell
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=$HERE/launch-readiness.sh
FAILED=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; FAILED=1; }
check() { local msg=$1; shift; if "$@"; then pass "$msg"; else fail "$msg"; fi; }

if command -v shellcheck >/dev/null; then
  if shellcheck "$SCRIPT" "$HERE/launch-readiness_test.sh"; then pass "shellcheck clean"; else fail "shellcheck"; fi
else
  fail "shellcheck is not installed"
fi
command -v jq >/dev/null || { fail "jq is not installed"; exit 1; }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
ROOT=$TMP/root; FX=$TMP/fx; BIN=$TMP/bin; METRICS=$ROOT/metrics
mkdir -p "$ROOT/configs" "$FX/sql" "$BIN" "$METRICS"
SENTINEL="SENTINEL-SECRET-d41d8cd98f00b204e9800998ecf8427e"

cat > "$BIN/curl" <<EOF
#!/bin/bash
url=\${*: -1}
case "\$url" in
  */metrics) [ -e "$FX/metrics" ] && cat "$FX/metrics" ;;
  *9093*) [ -e "$FX/alerts" ] && cat "$FX/alerts" ;;
  *) exit 22 ;;
esac
EOF
cat > "$BIN/journalctl" <<EOF
#!/bin/bash
[ ! -e "$FX/journal-fails" ] || exit 1
cat "$FX/journal"
EOF
cat > "$BIN/systemctl" <<EOF
#!/bin/bash
echo "Sun 2026-10-25 20:00:00 UTC"
EOF
# The fake psql answers a query by its tag; 'SELECT 1' is the ping. db-down = no answer.
cat > "$BIN/psql" <<EOF
#!/bin/bash
[ ! -e "$FX/db-down" ] || exit 2
echo "\$1" >> "$FX/sql.log"
case "\$1" in
  'SELECT 1') echo 1 ;;
  *lr:*) tag=\$(printf '%s' "\$1" | sed -n 's#.*/\* lr:\([a-z]*\) \*/.*#\1#p'); cat "$FX/sql/\$tag" ;;
  *) exit 3 ;;
esac
EOF
chmod +x "$BIN"/*

# all_green — every source says the switch is on.
all_green() {
  rm -rf "${FX:?}"/* "${METRICS:?}"/*; mkdir -p "$FX/sql"
  echo 8001 > "$ROOT/ACTIVE_PORT"
  cat > "$ROOT/configs/.env" <<EOF
DB_NAME=vaultaire
DB_PASSWORD="$SENTINEL"
JWT_SECRET=$SENTINEL
VAULTAIRE_ENDPOINT=https://s3.stored.cloud
VAULTAIRE_PUBLIC_HOST="https://stored.cloud"
SIGNUPS_ENABLED=true
EOF
  printf '{"level":"info","msg":"master key: credential"}\n{"level":"warn","msg":"master key: lower-precedence sources are also set and ignored"}\n' > "$FX/journal"
  cat > "$FX/metrics" <<'EOF'
# TYPE vaultaire_tenant_keys_unwrap_failures gauge
vaultaire_tenant_keys_unwrap_failures 0
vaultaire_translog_signing_key_loaded 1
vaultaire_payment_provider_ready{provider="btcpay"} 0
vaultaire_payment_provider_ready{provider="stripe"} 1
vaultaire_synthetic_check_success 1
EOF
  printf 'vaultaire_backup_dump_encrypted 1\n' > "$METRICS/vaultaire_backup_dump_encrypted.prom"
  echo 0 > "$FX/sql/unwrapped"; echo true > "$FX/sql/dropflag"; echo "0 0 0" > "$FX/sql/plaintext"
  echo "0 0" > "$FX/sql/unrotated"; echo true > "$FX/sql/signups"; echo tenant-abc123 > "$FX/sql/tz"; echo 4 > "$FX/sql/tzobjects"
  echo '[]' > "$FX/alerts"
  echo 'stored.cloud/release/v1+0a1b2c3d+AQ' > "$ROOT/configs/release.vkey"
}

OUT=$TMP/out
run() { # run [args] — the script against the fakes; the exit status in $RC, the output in $OUT
  set +e
  env PATH="$BIN:$PATH" LR_ROOT="$ROOT" LR_PSQL="$BIN/psql" LR_CURL="$BIN/curl" LR_JOURNALCTL="$BIN/journalctl" \
    LR_SYSTEMCTL="$BIN/systemctl" bash "$SCRIPT" "$@" > "$OUT" 2>&1
  RC=$?
  set -e
}
line_of() { grep -E "^[A-Z]+ +$1 \[" "$OUT" || true; }
status_of() { line_of "$1" | awk '{print $1}'; }
expect_gate() { # expect_gate CASE GATE STATUS [EVIDENCE_SUBSTRING]
  local got; got=$(status_of "$2")
  if [ "$got" = "$3" ] && { [ -z "${4:-}" ] || line_of "$2" | grep -qF -- "$4"; }; then pass "$1: $2 $3"
  else fail "$1: $2 want $3${4:+ ($4)}, got: $(line_of "$2")"; fi
}
GATES="master_key translog_signing_key dump_sealed credentials_wrapped plaintext_dropped keys_rotated blomp_sealed sia_sealed geyser_sealed stripe_ready signups_open domain_switched synthetic_check tenant_zero_objects alerts_firing release_signing_key"

# 1. everything on: every gate GREEN, exit 0, the gauge says 1 for each.
all_green; run
for g in $GATES; do expect_gate "all on" "$g" GREEN; done
check "all on: exit 0 (got $RC)" [ "$RC" = 0 ]
check "all on: summary 16/16" grep -q 'summary: 16/16 GREEN; launch gates not GREEN: 0' "$OUT"
check "all on: one line per gate" [ "$(grep -cE '^(GREEN|RED|UNKNOWN) ' "$OUT")" = 16 ]
check "all on: the Warn after the Info line is not read as a source" grep -q 'master key: credential (vaultaire@8001' "$OUT"
PROM=$METRICS/vaultaire_launch_gate.prom
check "all on: the gauge has 16 series at 1" [ "$(grep -c '^vaultaire_launch_gate{.*} 1$' "$PROM")" = 16 ]
check "all on: launch class on the security gates" grep -q 'vaultaire_launch_gate{gate="keys_rotated",class="launch"} 1' "$PROM"
check "all on: info class on the rest" grep -q 'vaultaire_launch_gate{gate="alerts_firing",class="info"} 1' "$PROM"
check "all on: the gauge is world-readable, no temp file left" [ "$(stat -c%a "$PROM" 2>/dev/null || stat -f%Lp "$PROM")" = 644 ] && [ -z "$(find "$METRICS" -name '.*' -type f)" ]
check "all on: the database was only SELECTed" bash -c '! grep -viE "^(/\* lr:[a-z]+ \*/ )?(SELECT|WITH) " "$1"' _ "$FX/sql.log"

# 2. NoSecretInOutput: the sentinel in .env never appears, whatever the gates say.
check "NoSecretInOutput: the sentinel secret is not in the output" bash -c '! grep -qF "$1" "$2"' _ "$SENTINEL" "$OUT"
check "NoSecretInOutput: nor in the gauge" bash -c '! grep -qF "$1" "$2"' _ "$SENTINEL" "$PROM"
all_green; printf 'BLOMP_ALLOW_UNSEALED=%s\nSIA_ALLOW_UNSEALED=%s\nVAULTAIRE_ENDPOINT=https://%s.example\n' "$SENTINEL" "$SENTINEL" "$SENTINEL" >> "$ROOT/configs/.env"
sed -i.bak '/^VAULTAIRE_ENDPOINT=https:\/\/s3/d' "$ROOT/configs/.env"; run
check "NoSecretInOutput: values of flagged keys are never printed" bash -c '! grep -qF "$1" "$2"' _ "$SENTINEL" "$OUT"

# 3. each gate RED from its source.
red_case() { # red_case GATE EVIDENCE SETUP...
  local g=$1 ev=$2; shift 2
  all_green; "$@"; run
  expect_gate "RED" "$g" RED "$ev"
  case "$g" in
    domain_switched|synthetic_check|tenant_zero_objects|alerts_firing|release_signing_key)
      check "RED $g (info): exit 0 — an info gate never blocks" [ "$RC" = 0 ] ;;
    *) check "RED $g (launch): exit 1" [ "$RC" = 1 ] ;;
  esac
}
put() { printf '%s\n' "$2" > "$1"; }
red_case master_key "no 'master key:' Info line" put "$FX/journal" '{"msg":"boot"}'
red_case master_key "loaded from 'env'" put "$FX/journal" '{"msg":"master key: env"}'
red_case master_key "do not unwrap" sed -i.bak 's/^vaultaire_tenant_keys_unwrap_failures 0/vaultaire_tenant_keys_unwrap_failures 2/' "$FX/metrics"
red_case translog_signing_key "_loaded 0" sed -i.bak 's/signing_key_loaded 1/signing_key_loaded 0/' "$FX/metrics"
red_case dump_sealed "plaintext" put "$METRICS/vaultaire_backup_dump_encrypted.prom" 'vaultaire_backup_dump_encrypted 0'
red_case dump_sealed "no vaultaire_backup_dump_encrypted gauge" rm -f "$METRICS/vaultaire_backup_dump_encrypted.prom"
red_case credentials_wrapped "3 live" put "$FX/sql/unwrapped" 3
red_case plaintext_dropped "is off" put "$FX/sql/dropflag" none
red_case plaintext_dropped "api_keys 0, sts_tokens 1, tenants 2" put "$FX/sql/plaintext" "0 1 2"
red_case keys_rotated "is off" put "$FX/sql/dropflag" false
red_case keys_rotated "5 live key(s) and 1 live STS" put "$FX/sql/unrotated" "5 1"
red_case blomp_sealed "BLOMP_ALLOW_UNSEALED is set" bash -c 'echo BLOMP_ALLOW_UNSEALED=true >> "$1"' _ "$ROOT/configs/.env"
red_case sia_sealed "SIA_ALLOW_UNSEALED is set" bash -c 'echo SIA_ALLOW_UNSEALED=1 >> "$1"' _ "$ROOT/configs/.env"
red_case geyser_sealed "GEYSER_ALLOW_UNSEALED is set" bash -c 'echo GEYSER_ALLOW_UNSEALED=true >> "$1"' _ "$ROOT/configs/.env"
red_case stripe_ready 'provider="stripe"} 0' sed -i.bak 's/provider="stripe"} 1/provider="stripe"} 0/' "$FX/metrics"
red_case signups_open "flag row: off" put "$FX/sql/signups" false
red_case signups_open "SIGNUPS_ENABLED is off" bash -c 'echo none > "$1"; sed -i.bak "s/^SIGNUPS_ENABLED=true/SIGNUPS_ENABLED=false/" "$2"' _ "$FX/sql/signups" "$ROOT/configs/.env"
red_case domain_switched "VAULTAIRE_PUBLIC_HOST is not https://stored.cloud" sed -i.bak 's#^VAULTAIRE_PUBLIC_HOST=.*#VAULTAIRE_PUBLIC_HOST=https://stored.ge#' "$ROOT/configs/.env"
red_case synthetic_check "success 0" sed -i.bak 's/check_success 1/check_success 0/' "$FX/metrics"
red_case tenant_zero_objects "holds no object" put "$FX/sql/tzobjects" 0
red_case alerts_firing "2 alert(s) firing: BackupDumpPlaintext, TranslogWitnessStale" put "$FX/alerts" '[{"labels":{"alertname":"TranslogWitnessStale"}},{"labels":{"alertname":"BackupDumpPlaintext"}}]'
red_case release_signing_key "no stored.cloud/release/v1 verifier key" rm -f "$ROOT/configs/release.vkey"

# Off values of the unsealed flags stay GREEN.
all_green; printf 'BLOMP_ALLOW_UNSEALED=false\nSIA_ALLOW_UNSEALED="0"\nGEYSER_ALLOW_UNSEALED=false\n' >> "$ROOT/configs/.env"; run
expect_gate "off value" blomp_sealed GREEN "set to an off value"
expect_gate "off value" sia_sealed GREEN "set to an off value"
expect_gate "off value" geyser_sealed GREEN "set to an off value"

# 4. UNKNOWN when a source does not answer — never GREEN, and a launch gate blocks.
all_green; touch "$FX/db-down"; run
for g in credentials_wrapped plaintext_dropped keys_rotated signups_open tenant_zero_objects; do expect_gate "database down" "$g" UNKNOWN "did not answer"; done
check "database down: exit 1" [ "$RC" = 1 ]
all_green; rm -f "$FX/metrics"; run
for g in translog_signing_key stripe_ready synthetic_check; do expect_gate "metrics down" "$g" UNKNOWN "did not answer"; done
expect_gate "metrics down" master_key UNKNOWN "unwrap failures unknown"
check "metrics down: exit 1" [ "$RC" = 1 ]
all_green; grep -v signing_key_loaded "$FX/metrics" > "$FX/m2"; mv "$FX/m2" "$FX/metrics"; run
expect_gate "metric absent" translog_signing_key UNKNOWN "absent from /metrics"
all_green; touch "$FX/journal-fails"; run
expect_gate "journal unreadable" master_key UNKNOWN "cannot read the journal"
all_green; rm -f "$ROOT/ACTIVE_PORT"; run
expect_gate "no active port" master_key UNKNOWN "no active port"
expect_gate "no active port" stripe_ready UNKNOWN
all_green; rm -f "$FX/alerts"; run
expect_gate "Alertmanager down" alerts_firing UNKNOWN "did not answer"
all_green; chmod 0000 "$ROOT/configs/.env"
if [ -r "$ROOT/configs/.env" ]; then echo "SKIPPED .env unreadable: running as root"; else
  run
  for g in blomp_sealed sia_sealed geyser_sealed domain_switched; do expect_gate ".env unreadable" "$g" UNKNOWN "not readable"; done
fi
chmod 0600 "$ROOT/configs/.env"
all_green; chmod 0000 "$METRICS"
if [ -r "$METRICS" ]; then echo "SKIPPED metrics dir unreadable: running as root"; else
  run --no-metrics; expect_gate "textfile dir unreadable" dump_sealed UNKNOWN "not readable"
fi
chmod 0755 "$METRICS"
all_green; put "$FX/sql/tz" ''; run
expect_gate "no single admin" tenant_zero_objects UNKNOWN "set LR_TENANT_ZERO"
all_green; set +e; env PATH="$BIN:$PATH" LR_ROOT="$ROOT" LR_PSQL="$BIN/psql" LR_CURL="$BIN/curl" LR_JOURNALCTL="$BIN/journalctl" \
  LR_SYSTEMCTL="$BIN/systemctl" LR_TENANT_ZERO="x'; DROP TABLE users; --" bash "$SCRIPT" > "$OUT" 2>&1; set -e
expect_gate "a tenant id that is not one" tenant_zero_objects UNKNOWN "not a tenant id"
check "a tenant id that is not one never reaches SQL" bash -c '! grep -q "DROP TABLE" "$1"' _ "$FX/sql.log"

# 5. UNKNOWN gauge is 0, and LR_LAUNCH_GATES moves a gate between classes.
all_green; touch "$FX/db-down"; run
check "UNKNOWN is 0 in the gauge" grep -q 'vaultaire_launch_gate{gate="credentials_wrapped",class="launch"} 0' "$PROM"
all_green; put "$FX/sql/tzobjects" 0; set +e
env PATH="$BIN:$PATH" LR_ROOT="$ROOT" LR_PSQL="$BIN/psql" LR_CURL="$BIN/curl" LR_JOURNALCTL="$BIN/journalctl" \
  LR_SYSTEMCTL="$BIN/systemctl" LR_LAUNCH_GATES=tenant_zero_objects bash "$SCRIPT" > "$OUT" 2>&1; RC=$?; set -e
check "LR_LAUNCH_GATES: tenant zero is a launch gate, its RED blocks (exit $RC)" [ "$RC" = 1 ]
check "LR_LAUNCH_GATES: master_key is info then" grep -q '^GREEN   master_key \[info\]' "$OUT"
all_green; rm -f "$PROM"; run --no-metrics
check "--no-metrics writes no gauge" [ ! -e "$PROM" ]
run --bogus
check "a bad argument is a usage error (exit 2)" [ "$RC" = 2 ]

exit $FAILED
