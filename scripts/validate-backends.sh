#!/usr/bin/env bash
# scripts/validate-backends.sh
#
# End-to-end product validation — spins up Vaultaire against each backend
# and runs the full S3 compatibility suite. Produces a per-backend report.
#
# Usage:
#   source .env.bench && ./scripts/validate-backends.sh              # all backends
#   source .env.bench && ./scripts/validate-backends.sh idrive       # one backend
#   source .env.bench && ./scripts/validate-backends.sh --live       # test live stored.ge
#   source .env.bench && ./scripts/validate-backends.sh --fast       # skip slow tests
#
# Prerequisites:
#   - source .env.bench (backend credentials)
#   - PostgreSQL running (for local tests)
#   - For --live: stored.ge configured with real backends

set -euo pipefail

PORT=8002
FAST=""
LIVE=""
BACKENDS=()
VAULTAIRE_PID=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --fast|-f) FAST="--skip-slow"; shift ;;
        --live|-l) LIVE="true"; shift ;;
        *) BACKENDS+=("$1"); shift ;;
    esac
done

if [[ ${#BACKENDS[@]} -eq 0 ]]; then
    BACKENDS=("local" "idrive" "geyser")
fi

cleanup() {
    if [[ -n "$VAULTAIRE_PID" ]]; then
        kill "$VAULTAIRE_PID" 2>/dev/null || true
        wait "$VAULTAIRE_PID" 2>/dev/null || true
        VAULTAIRE_PID=""
    fi
}
trap cleanup EXIT

wait_for_health() {
    local attempts=0
    while [[ $attempts -lt 30 ]]; do
        if curl -sf "http://localhost:$PORT/health/live" >/dev/null 2>&1; then
            return 0
        fi
        sleep 0.5
        attempts=$((attempts + 1))
    done
    return 1
}

map_idrive_creds() {
    if [[ -n "${IDRIVE_ACCESS_KEY:-}" ]]; then return; fi
    if [[ -n "${IDRIVE_US_CENTRAL_1_ACCESS_KEY:-}" ]]; then
        export IDRIVE_ACCESS_KEY="$IDRIVE_US_CENTRAL_1_ACCESS_KEY"
        export IDRIVE_SECRET_KEY="$IDRIVE_US_CENTRAL_1_SECRET_KEY"
        export IDRIVE_ENDPOINT="https://s3.us-central-1.idrivee2.com"
        export IDRIVE_REGION="us-central-1"
    elif [[ -n "${IDRIVE_US_WEST_1_ACCESS_KEY:-}" ]]; then
        export IDRIVE_ACCESS_KEY="$IDRIVE_US_WEST_1_ACCESS_KEY"
        export IDRIVE_SECRET_KEY="$IDRIVE_US_WEST_1_SECRET_KEY"
        export IDRIVE_ENDPOINT="https://s3.us-west-1.idrivee2.com"
        export IDRIVE_REGION="us-west-1"
    fi
}

isolate_backend_env() {
    local backend="$1"
    # Clear all backend creds, then set only the target
    unset LYVE_ACCESS_KEY LYVE_SECRET_KEY 2>/dev/null || true
    unset QUOTALESS_ACCESS_KEY QUOTALESS_SECRET_KEY QUOTALESS_ENDPOINT 2>/dev/null || true
    unset GEYSER_ACCESS_KEY GEYSER_SECRET_KEY GEYSER_BUCKET GEYSER_ENDPOINT 2>/dev/null || true
    unset IDRIVE_ACCESS_KEY IDRIVE_SECRET_KEY IDRIVE_ENDPOINT IDRIVE_REGION 2>/dev/null || true
    unset ONEDRIVE_CLIENT_ID ONEDRIVE_CLIENT_SECRET ONEDRIVE_TENANT_ID 2>/dev/null || true

    source .env.bench 2>/dev/null || true

    case "$backend" in
        local)
            export STORAGE_MODE=local
            ;;
        idrive)
            map_idrive_creds
            export STORAGE_MODE=idrive
            unset LYVE_ACCESS_KEY LYVE_SECRET_KEY
            unset GEYSER_ACCESS_KEY GEYSER_SECRET_KEY
            ;;
        geyser)
            export STORAGE_MODE=geyser
            export GEYSER_BUCKET="${GEYSER_LA_BUCKET:-vaultaire-la}"
            export GEYSER_ENDPOINT="${GEYSER_ENDPOINT:-https://geyser-la.example.com}"
            unset LYVE_ACCESS_KEY LYVE_SECRET_KEY
            unset IDRIVE_ACCESS_KEY IDRIVE_SECRET_KEY
            ;;
        lyve)
            export STORAGE_MODE=lyve
            unset IDRIVE_ACCESS_KEY IDRIVE_SECRET_KEY
            unset GEYSER_ACCESS_KEY GEYSER_SECRET_KEY
            ;;
        onedrive)
            export STORAGE_MODE=onedrive
            unset IDRIVE_ACCESS_KEY IDRIVE_SECRET_KEY
            unset GEYSER_ACCESS_KEY GEYSER_SECRET_KEY
            unset LYVE_ACCESS_KEY LYVE_SECRET_KEY
            ;;
    esac
}

register_bench_tenant() {
    if [[ -n "${VAULTAIRE_BENCH_ACCESS_KEY:-}" ]]; then
        return 0
    fi
    echo "  Registering bench tenant..."
    local resp
    resp=$(curl -sf "http://localhost:$PORT/auth/register" \
        -H "Content-Type: application/json" \
        -d '{"email":"bench@validate.local","password":"BenchPass123!","company":"Validation"}' 2>/dev/null) || true
    # Extract keys from response if available
    if echo "$resp" | grep -q "access_key"; then
        export VAULTAIRE_BENCH_ACCESS_KEY=$(echo "$resp" | grep -o '"access_key":"[^"]*"' | cut -d'"' -f4)
        export VAULTAIRE_BENCH_SECRET_KEY=$(echo "$resp" | grep -o '"secret_key":"[^"]*"' | cut -d'"' -f4)
    fi
}

# ── Main ─────────────────────────────────────────────────────────────────────

echo ""
echo "╔══════════════════════════════════════════════════════════════════╗"
echo "║       stored.ge — Product Validation Suite                      ║"
echo "╠══════════════════════════════════════════════════════════════════╣"
echo "║  Tests S3 compatibility, integrity, performance per backend     ║"
echo "╚══════════════════════════════════════════════════════════════════╝"
echo ""

# Build validate binary
echo "Building validation binary..."
go build -o bin/validate ./cmd/validate/
echo ""

if [[ -n "$LIVE" ]]; then
    echo "MODE: Testing against live endpoint"
    EP="${VAULTAIRE_LOAD_ENDPOINT:-https://s3.stored.ge}"
    echo "Endpoint: $EP"
    echo ""
    ./bin/validate \
        -endpoint "$EP" \
        -access-key "${VAULTAIRE_LOAD_ACCESS_KEY:-$VAULTAIRE_BENCH_ACCESS_KEY}" \
        -secret-key "${VAULTAIRE_LOAD_SECRET_KEY:-$VAULTAIRE_BENCH_SECRET_KEY}" \
        $FAST
    exit $?
fi

echo "MODE: Per-backend local validation"
echo "Backends: ${BACKENDS[*]}"
echo ""

OVERALL_EXIT=0

for backend in "${BACKENDS[@]}"; do
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  BACKEND: $backend"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""

    isolate_backend_env "$backend"

    echo "  Starting Vaultaire (port $PORT, storage=$backend)..."
    STORAGE_MODE="$backend" PORT="$PORT" JWT_SECRET="validate-secret-key" \
        ./bin/vaultaire >/tmp/vaultaire-validate-$backend.log 2>&1 &
    VAULTAIRE_PID=$!

    if ! wait_for_health; then
        echo "  FAILED: Vaultaire didn't start with $backend"
        echo "  Log tail:"
        tail -10 /tmp/vaultaire-validate-$backend.log 2>/dev/null || true
        cleanup
        OVERALL_EXIT=1
        continue
    fi

    echo "  Vaultaire healthy on $backend."
    echo ""

    # Register tenant if needed
    register_bench_tenant

    # Run validation
    ./bin/validate \
        -endpoint "http://localhost:$PORT" \
        -access-key "${VAULTAIRE_BENCH_ACCESS_KEY}" \
        -secret-key "${VAULTAIRE_BENCH_SECRET_KEY}" \
        -bucket "validate-$backend" \
        $FAST || OVERALL_EXIT=1

    cleanup
    sleep 1
done

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
if [[ $OVERALL_EXIT -eq 0 ]]; then
    echo "  ALL BACKENDS VALIDATED SUCCESSFULLY"
else
    echo "  SOME BACKENDS FAILED — check output above"
fi
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

exit $OVERALL_EXIT
