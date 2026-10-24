#!/bin/bash
# tenant-zero-proof_test.sh — tests for deploy/scripts/tenant-zero-proof.sh. Run from anywhere:
#   bash deploy/scripts/tenant-zero-proof_test.sh
# CI runs it (ci.yml "Deploy scripts"). Needs shellcheck, openssl and python3; never a
# server, a database or a network: the pure helpers only, and the refusals that keep a
# proof from writing a global flag row.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=$HERE/tenant-zero-proof.sh
FAILED=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; FAILED=1; }
eq() { # eq MESSAGE GOT WANT
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1: got <$2> want <$3>"; fi
}

# --- 1. shellcheck-clean -------------------------------------------------------------
if command -v shellcheck >/dev/null; then
  if shellcheck "$SCRIPT" "$HERE/tenant-zero-proof_test.sh"; then pass "shellcheck clean"; else fail "shellcheck"; fi
else
  fail "shellcheck is not installed"
fi

# shellcheck source=deploy/scripts/tenant-zero-proof.sh
. "$SCRIPT" # defines the helpers; main() does not run when sourced
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# --- 2. flag_body never writes a global row ---------------------------------------------
eq "flag_body for one tenant" "$(flag_body true tenant-4e1b43062db9d685)" '{"enabled":true,"tenant_id":"tenant-4e1b43062db9d685"}'
eq "flag_body false" "$(flag_body false tenant-abc)" '{"enabled":false,"tenant_id":"tenant-abc"}'
for bad in "" "*" "'*'" "tenant-" "tenant-abc\",\"tenant_id\":\"" "TENANT-ABC" "tenant-abc def"; do
  if flag_body true "$bad" > /dev/null 2>&1; then fail "flag_body accepted tenant <$bad>"; else pass "flag_body refuses tenant <$bad>"; fi
done
if flag_body yes tenant-abc > /dev/null 2>&1; then fail "flag_body accepted enabled=yes"; else pass "flag_body refuses enabled=yes"; fi

# main refuses to start without one tenant (before any key, flag or bucket).
for bad in "" "*"; do
  if out=$(TZP_TENANT="$bad" bash "$SCRIPT" preflight 2>&1); then
    fail "main ran with TZP_TENANT=<$bad>"
  elif grep -q "never empty or '\*'" <<< "$out"; then
    pass "main refuses TZP_TENANT=<$bad>"
  else
    fail "main with TZP_TENANT=<$bad> failed for another reason: $out"
  fi
done

# --- 3. the admin JWT is a real sign-in's, never one minted from JWT_SECRET (RP56-5) ----
# Before Prompt 40 the script signed its own 10 min admin JWTs with the box's JWT_SECRET:
# the audit rows and the transparency log's admin reads named the admin with no sign-in
# or MFA behind them. Now it takes the token POST /auth/login answered to the admin's
# password AND MFA code, and checks it names tenant zero's user before any call.
if grep -q 'JWT_SECRET' "$SCRIPT"; then fail "the script still reads JWT_SECRET"; else pass "the script never reads JWT_SECRET"; fi
jwt() { # jwt USER TENANT EXP — an unsigned token of that payload (the server checks the signature)
  python3 -c '
import sys, json, base64
b = lambda x: base64.urlsafe_b64encode(x).rstrip(b"=").decode()
print(b(b"{\"alg\":\"HS256\"}") + "." + b(json.dumps({"user_id": sys.argv[1], "tenant_id": sys.argv[2], "exp": int(sys.argv[3])}).encode()) + ".sig")' "$@"
}
NOW=$(date +%s)
U=4b1c0c47-0000-4000-8000-000000000001
eq "jwt_check: tenant zero's user, 2 h left" "$(jwt_check "$(jwt $U tenant-abc $((NOW + 7200)))" tenant-abc $U 1800 2>&1 && echo ok)" ok
for c in "another tenant|$(jwt $U tenant-other $((NOW + 7200)))" \
         "another user|$(jwt 4b1c0c47-0000-4000-8000-000000000002 tenant-abc $((NOW + 7200)))" \
         "expired|$(jwt $U tenant-abc $((NOW - 5)))" \
         "10 min left of the 30 the run needs|$(jwt $U tenant-abc $((NOW + 600)))" \
         "not a JWT|not-a-jwt" "empty|"; do
  if jwt_check "${c#*|}" tenant-abc $U 1800 > /dev/null 2>&1; then fail "jwt_check accepted: ${c%%|*}"; else pass "jwt_check refuses: ${c%%|*}"; fi
done
# main refuses to start without the token — before any key, flag or bucket — and refuses a
# token file other users can read.
if out=$(TZP_TENANT=tenant-abc TZP_ADMIN_BASE=http://127.0.0.1:9 bash "$SCRIPT" preflight 2>&1); then
  fail "main ran without an admin JWT"
elif grep -q "TZP_ADMIN_JWT" <<< "$out"; then pass "main refuses to start without an admin JWT"
else fail "main without an admin JWT failed for another reason: $out"; fi
jwt $U tenant-abc $((NOW + 7200)) > "$TMP/jwt"; chmod 644 "$TMP/jwt"
if out=$(TZP_TENANT=tenant-abc TZP_ADMIN_BASE=http://127.0.0.1:9 TZP_ADMIN_JWT_FILE="$TMP/jwt" bash "$SCRIPT" preflight 2>&1); then
  fail "main ran with a world-readable token file"
elif grep -q "other users can read" <<< "$out"; then pass "main refuses a token file other users can read"
else fail "main with a 0644 token file failed for another reason: $out"; fi

# --- 4. make_body: exact size, the same bytes for a seed, other bytes for another ------
make_body 3145728 seed-a "$TMP/a1"
make_body 3145728 seed-a "$TMP/a2"
make_body 3145728 seed-b "$TMP/b1"
eq "make_body: exact size" "$(wc -c < "$TMP/a1" | tr -d ' ')" 3145728
eq "make_body: deterministic" "$(sha256_of "$TMP/a1")" "$(sha256_of "$TMP/a2")"
if [ "$(sha256_of "$TMP/a1")" != "$(sha256_of "$TMP/b1")" ]; then pass "make_body: another seed, other bytes"; else fail "make_body: seeds collide"; fi
# Incompressible: a chunk store must not shrink it to nothing (gzip saves < 1 %).
gz=$(gzip -c "$TMP/a1" | wc -c | tr -d ' ')
if [ "$gz" -gt 3100000 ]; then pass "make_body: incompressible"; else fail "make_body: compresses to $gz"; fi

# --- 5. slice = the bytes of a Range GET -------------------------------------------------
printf '0123456789' > "$TMP/ten"
eq "slice: middle" "$(slice "$TMP/ten" 3 4)" "3456"
eq "slice: from 0" "$(slice "$TMP/ten" 0 2)" "01"
eq "slice: tail" "$(slice "$TMP/ten" 8 2)" "89"

# --- 6. metric_sum over Prometheus text --------------------------------------------------
cat > "$TMP/m" <<'EOF'
# HELP vaultaire_large_get_parallel_reads_total x
# TYPE vaultaire_large_get_parallel_reads_total counter
vaultaire_large_get_parallel_reads_total{backend="idrive",path="ranged"} 7
vaultaire_large_get_parallel_reads_total{backend="lyve",path="ranged"} 2
vaultaire_large_get_parallel_reads_total{backend="idrive",path="sealed"} 1
vaultaire_large_get_parallel_reads_total_extra 100
vaultaire_large_get_fallbacks_total{reason="budget"} 3
EOF
eq "metric_sum: every sample" "$(metric_sum vaultaire_large_get_parallel_reads_total < "$TMP/m")" 10
eq "metric_sum: one label" "$(metric_sum vaultaire_large_get_parallel_reads_total 'path="ranged"' < "$TMP/m")" 9
eq "metric_sum: two labels" "$(metric_sum vaultaire_large_get_parallel_reads_total 'backend="idrive"' 'path="ranged"' < "$TMP/m")" 7
eq "metric_sum: absent = 0" "$(metric_sum vaultaire_nope_total < "$TMP/m")" 0
eq "metric_sum: no prefix match" "$(metric_sum vaultaire_large_get_parallel_reads < "$TMP/m")" 0

# --- 7. mbps ------------------------------------------------------------------------------
eq "mbps" "$(mbps 268435456 2)" "134.2"
eq "mbps: no time" "$(mbps 1 0)" "n/a"

if [ "$FAILED" -ne 0 ]; then echo "tenant-zero-proof_test: FAILED"; exit 1; fi
echo "tenant-zero-proof_test: all passed"
