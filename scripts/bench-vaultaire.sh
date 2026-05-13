#!/usr/bin/env bash
# scripts/bench-vaultaire.sh
#
# End-to-end Vaultaire benchmark — measures S3 layer overhead per backend.
# Cycles through backends, starts Vaultaire on port 8001, runs bench-compare.
#
# Usage:
#   source .env.bench && ./scripts/bench-vaultaire.sh              # all backends
#   source .env.bench && ./scripts/bench-vaultaire.sh local idrive  # specific backends
#   source .env.bench && ./scripts/bench-vaultaire.sh -smoke local  # quick mode
#
# Prerequisites:
#   - source .env.bench (backend credentials)
#   - PostgreSQL running locally (Mac) or accessible via DB_* env vars (SLC)
#   - For SLC: pre-compiled binaries in same directory

set -euo pipefail

BENCH_PORT=8001
RESULTS_DIR="bench-results/vaultaire-e2e"
SMOKE=""
BACKENDS=()
VAULTAIRE_PID=""

# Parse flags
while [[ $# -gt 0 ]]; do
    case "$1" in
        -smoke|--smoke) SMOKE="-smoke"; shift ;;
        *) BACKENDS+=("$1"); shift ;;
    esac
done

# Default: all backends
if [[ ${#BACKENDS[@]} -eq 0 ]]; then
    BACKENDS=("local" "idrive" "lyve" "geyser" "onedrive")
fi

# Map iDrive per-region creds to single-region format for Vaultaire main.go.
# us-central-1 = best from SLC benchmarks (250 MB/s MPU).
map_idrive_creds() {
    if [[ -n "${IDRIVE_ACCESS_KEY:-}" ]]; then
        return  # already set
    fi
    if [[ -n "${IDRIVE_US_CENTRAL_1_ACCESS_KEY:-}" ]]; then
        export IDRIVE_ACCESS_KEY="$IDRIVE_US_CENTRAL_1_ACCESS_KEY"
        export IDRIVE_SECRET_KEY="$IDRIVE_US_CENTRAL_1_SECRET_KEY"
        export IDRIVE_ENDPOINT="https://s3.us-central-1.idrivee2.com"
        export IDRIVE_REGION="us-central-1"
        echo "  iDrive: mapped us-central-1 creds"
    elif [[ -n "${IDRIVE_US_WEST_1_ACCESS_KEY:-}" ]]; then
        export IDRIVE_ACCESS_KEY="$IDRIVE_US_WEST_1_ACCESS_KEY"
        export IDRIVE_SECRET_KEY="$IDRIVE_US_WEST_1_SECRET_KEY"
        export IDRIVE_ENDPOINT="https://s3.us-west-1.idrivee2.com"
        export IDRIVE_REGION="us-west-1"
        echo "  iDrive: mapped us-west-1 creds (fallback)"
    else
        echo "  iDrive: no per-region creds found, skipping idrive backend"
    fi
}

# Build binaries (skip on Linux where we use pre-compiled)
build_binaries() {
    if command -v go &>/dev/null; then
        echo "Building binaries..."
        go build -o bin/vaultaire ./cmd/vaultaire/
        go build -o bin/bench-compare ./cmd/bench-compare/
        VAULTAIRE_BIN="./bin/vaultaire"
        BENCH_BIN="./bin/bench-compare"
    else
        # Pre-compiled binaries (SLC deployment)
        VAULTAIRE_BIN="./vaultaire-linux"
        BENCH_BIN="./bench-compare-linux"
        if [[ ! -x "$VAULTAIRE_BIN" ]] || [[ ! -x "$BENCH_BIN" ]]; then
            echo "ERROR: No Go compiler and no pre-compiled binaries found"
            exit 1
        fi
    fi
}

cleanup() {
    if [[ -n "$VAULTAIRE_PID" ]] && kill -0 "$VAULTAIRE_PID" 2>/dev/null; then
        kill "$VAULTAIRE_PID" 2>/dev/null
        wait "$VAULTAIRE_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

# Isolate: only export creds for the backend being tested.
# Without this, the engine wires ALL backends and Delete tries them all,
# causing 6s+ per delete when Quotaless is locked or OneDrive 404s.
isolate_backend_env() {
    local backend=$1
    # Save bench creds (always needed)
    local bench_ak="${VAULTAIRE_BENCH_ACCESS_KEY:-}"
    local bench_sk="${VAULTAIRE_BENCH_SECRET_KEY:-}"
    # Save target backend creds
    local save_idrive_ak="${IDRIVE_ACCESS_KEY:-}" save_idrive_sk="${IDRIVE_SECRET_KEY:-}"
    local save_idrive_ep="${IDRIVE_ENDPOINT:-}" save_idrive_rg="${IDRIVE_REGION:-}"
    local save_lyve_ak="${LYVE_ACCESS_KEY:-}" save_lyve_sk="${LYVE_SECRET_KEY:-}" save_lyve_rg="${LYVE_REGION:-}"
    local save_geyser_ak="${GEYSER_ACCESS_KEY:-}" save_geyser_sk="${GEYSER_SECRET_KEY:-}"
    local save_geyser_bucket="${GEYSER_BUCKET:-}" save_geyser_ep="${GEYSER_ENDPOINT:-}"
    local save_tenant1="${TENANT_1_ID:-}"

    # Unset ALL backend creds so only target backend gets wired
    unset S3_ACCESS_KEY S3_SECRET_KEY
    unset LYVE_ACCESS_KEY LYVE_SECRET_KEY LYVE_REGION
    unset QUOTALESS_ACCESS_KEY QUOTALESS_SECRET_KEY QUOTALESS_ENDPOINT
    unset GEYSER_ACCESS_KEY GEYSER_SECRET_KEY GEYSER_BUCKET GEYSER_ENDPOINT
    unset IDRIVE_ACCESS_KEY IDRIVE_SECRET_KEY IDRIVE_ENDPOINT IDRIVE_REGION
    for i in $(seq 1 15); do
        unset "TENANT_${i}_ID" "TENANT_${i}_CLIENT_ID" "TENANT_${i}_SECRET" "TENANT_${i}_USER"
    done

    # Restore only the target backend
    case "$backend" in
        local) ;; # No backend creds needed
        idrive)
            export IDRIVE_ACCESS_KEY="$save_idrive_ak" IDRIVE_SECRET_KEY="$save_idrive_sk"
            export IDRIVE_ENDPOINT="$save_idrive_ep" IDRIVE_REGION="$save_idrive_rg"
            export IDRIVE_BUCKET="vaultaire-bench"
            ;;
        lyve)
            export LYVE_ACCESS_KEY="$save_lyve_ak" LYVE_SECRET_KEY="$save_lyve_sk"
            export LYVE_REGION="${save_lyve_rg:-us-east-1}"
            ;;
        geyser)
            export GEYSER_ACCESS_KEY="$save_geyser_ak" GEYSER_SECRET_KEY="$save_geyser_sk"
            [[ -n "$save_geyser_bucket" ]] && export GEYSER_BUCKET="$save_geyser_bucket"
            [[ -n "$save_geyser_ep" ]] && export GEYSER_ENDPOINT="$save_geyser_ep"
            ;;
        onedrive)
            # Re-source to get all TENANT_N_* vars back
            source .env.bench 2>/dev/null || true
            # Then unset non-OneDrive backends again
            unset S3_ACCESS_KEY S3_SECRET_KEY LYVE_ACCESS_KEY LYVE_SECRET_KEY
            unset QUOTALESS_ACCESS_KEY QUOTALESS_SECRET_KEY
            unset GEYSER_ACCESS_KEY GEYSER_SECRET_KEY
            unset IDRIVE_ACCESS_KEY IDRIVE_SECRET_KEY
            ;;
    esac

    # Restore bench creds
    export VAULTAIRE_BENCH_ACCESS_KEY="$bench_ak"
    export VAULTAIRE_BENCH_SECRET_KEY="$bench_sk"
}

wait_for_health() {
    local max_wait=30
    for i in $(seq 1 $max_wait); do
        if curl -sf "http://localhost:$BENCH_PORT/health" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    echo "ERROR: Vaultaire did not become healthy in ${max_wait}s"
    return 1
}

# Register bench tenant if VAULTAIRE_BENCH_ACCESS_KEY is not set
register_bench_tenant() {
    if [[ -n "${VAULTAIRE_BENCH_ACCESS_KEY:-}" ]] && [[ -n "${VAULTAIRE_BENCH_SECRET_KEY:-}" ]]; then
        echo "  Using existing bench creds: ${VAULTAIRE_BENCH_ACCESS_KEY:0:8}..."
        return 0
    fi

    echo "  Registering bench tenant..."
    local resp
    resp=$(curl -sf -X POST "http://localhost:$BENCH_PORT/auth/register" \
        -H "Content-Type: application/json" \
        -d '{"email":"bench@vaultaire.local","password":"bench-e2e-2026","company":"Bench"}' 2>&1) || {
        echo "  Registration failed (user may already exist). Trying login..."
        # If registration fails, the user probably exists already.
        # We need the creds from a previous run. Check if they're in .env.bench.
        echo "  ERROR: Set VAULTAIRE_BENCH_ACCESS_KEY and VAULTAIRE_BENCH_SECRET_KEY manually"
        return 1
    }

    export VAULTAIRE_BENCH_ACCESS_KEY
    export VAULTAIRE_BENCH_SECRET_KEY
    VAULTAIRE_BENCH_ACCESS_KEY=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['accessKeyId'])" 2>/dev/null) || {
        echo "  Failed to parse registration response: $resp"
        return 1
    }
    VAULTAIRE_BENCH_SECRET_KEY=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['secretAccessKey'])" 2>/dev/null)

    echo "  Bench tenant registered!"
    echo "  Access Key: $VAULTAIRE_BENCH_ACCESS_KEY"
    echo "  Secret Key: ${VAULTAIRE_BENCH_SECRET_KEY:0:8}..."
    echo ""
    echo "  Save these to .env.bench for future runs:"
    echo "  export VAULTAIRE_BENCH_ACCESS_KEY=\"$VAULTAIRE_BENCH_ACCESS_KEY\""
    echo "  export VAULTAIRE_BENCH_SECRET_KEY=\"$VAULTAIRE_BENCH_SECRET_KEY\""
    echo ""
}

# Check if a backend has the required credentials
backend_ready() {
    local backend=$1
    case "$backend" in
        local)    return 0 ;;
        idrive)   [[ -n "${IDRIVE_ACCESS_KEY:-}" ]] ;;
        lyve)     [[ -n "${LYVE_ACCESS_KEY:-}" ]] ;;
        geyser)   [[ -n "${GEYSER_ACCESS_KEY:-}" ]] ;;
        onedrive) [[ -n "${TENANT_1_ID:-}" ]] ;;
        *)        return 1 ;;
    esac
}

# Run benchmark for one backend
bench_backend() {
    local backend=$1
    local host
    host=$(hostname 2>/dev/null || echo "unknown")
    local outfile="$RESULTS_DIR/${host}-vaultaire-${backend}.json"

    echo ""
    echo "═══════════════════════════════════════════════════════════════"
    echo "  BACKEND: $backend"
    echo "═══════════════════════════════════════════════════════════════"

    if ! backend_ready "$backend"; then
        echo "  SKIPPED — no credentials for $backend"
        return 0
    fi

    # Isolate: only wire the target backend to avoid cross-backend delete retries
    isolate_backend_env "$backend"

    # Start Vaultaire with this backend
    echo "  Starting Vaultaire (port $BENCH_PORT, storage=$backend)..."
    STORAGE_MODE="$backend" PORT="$BENCH_PORT" "$VAULTAIRE_BIN" &
    VAULTAIRE_PID=$!

    if ! wait_for_health; then
        echo "  FAILED — Vaultaire didn't start"
        cleanup
        VAULTAIRE_PID=""
        return 1
    fi

    echo "  Vaultaire healthy. Running bench-compare..."
    echo ""

    # Run bench-compare against Vaultaire
    "$BENCH_BIN" -only vaultaire -out "$outfile" $SMOKE || true

    echo ""
    echo "  Results: $outfile"

    # Stop Vaultaire
    cleanup
    VAULTAIRE_PID=""
    sleep 1
}

# ── Main ─────────────────────────────────────────────────────────────────────

echo ""
echo "╔══════════════════════════════════════════════════════════════╗"
echo "║          Vaultaire End-to-End Benchmark Suite               ║"
echo "╠══════════════════════════════════════════════════════════════╣"
echo "║  Measures S3 layer overhead vs direct-to-backend            ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""

build_binaries
map_idrive_creds
mkdir -p "$RESULTS_DIR"

echo ""
echo "Backends: ${BACKENDS[*]}"
echo "Port:     $BENCH_PORT"
echo "Output:   $RESULTS_DIR/"
echo "Mode:     ${SMOKE:-full}"
echo ""

# Start with local to register bench tenant
echo "── Phase 1: Register bench tenant ──────────────────────────────"
STORAGE_MODE=local PORT="$BENCH_PORT" "$VAULTAIRE_BIN" &
VAULTAIRE_PID=$!
if wait_for_health; then
    register_bench_tenant
fi
cleanup
VAULTAIRE_PID=""
sleep 1

if [[ -z "${VAULTAIRE_BENCH_ACCESS_KEY:-}" ]]; then
    echo "ERROR: No bench credentials. Cannot continue."
    exit 1
fi

# Run each backend (re-source .env.bench before each to restore all creds)
echo ""
echo "── Phase 2: Benchmark each backend ─────────────────────────────"
for backend in "${BACKENDS[@]}"; do
    source .env.bench 2>/dev/null || true
    map_idrive_creds
    bench_backend "$backend"
done

echo ""
echo "═══════════════════════════════════════════════════════════════"
echo "  ALL DONE"
echo "  Results in: $RESULTS_DIR/"
echo "═══════════════════════════════════════════════════════════════"
