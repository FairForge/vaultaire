#!/bin/bash
# verify_implementation.sh

echo "=== VAULTAIRE IMPLEMENTATION VERIFICATION ==="
echo "Checking Steps 11-180 for actual implementation..."
echo ""

# Color codes
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Counters
IMPLEMENTED=0
STUBS=0
MISSING=0

check_file() {
    local file=$1
    local component=$2

    if [ -f "$file" ]; then
        # Check if file has actual implementation (more than 50 lines, no TODO/STUB markers)
        lines=$(wc -l < "$file")
        todos=$(grep -c "TODO\|STUB\|PLACEHOLDER\|NotImplemented" "$file" 2>/dev/null || echo 0)

        if [[[ $lines -gt 50 ]] && [[[ $todos -eq 0 ]]; then
            echo -e "${GREEN}✓${NC} $component - IMPLEMENTED ($lines lines)"
            ((IMPLEMENTED++))
        elif [[ $todos -gt 0 ]; then
            echo -e "${YELLOW}⚠${NC} $component - PARTIAL (has $todos TODOs)"
            ((STUBS++))
        else
            echo -e "${YELLOW}⚠${NC} $component - STUB ($lines lines)"
            ((STUBS++))
        fi
    else
        echo -e "${RED}✗${NC} $component - MISSING"
        ((MISSING++))
    fi
}

echo "=== CORE INTERFACES (Steps 11-20) ==="
check_file "internal/storage/backend.go" "Backend interface"
check_file "internal/engine/engine.go" "Engine structure"
check_file "internal/storage/container.go" "Container abstraction"
check_file "internal/storage/artifact.go" "Artifact abstraction"
check_file "internal/engine/errors.go" "Error types"

echo ""
echo "=== STORAGE BACKENDS (Steps 21-30, 51-75) ==="
check_file "internal/drivers/local.go" "LocalBackend"
check_file "internal/drivers/local_pool.go" "Parallel I/O pools"
check_file "internal/drivers/local_atomic.go" "Atomic operations"
check_file "internal/drivers/local_watch.go" "File watching"

echo ""
echo "=== S3 BACKEND (Steps 31-40, 76-95) ==="
check_file "internal/api/s3_handlers.go" "S3 API handlers"
check_file "internal/drivers/s3.go" "S3 backend"
check_file "internal/drivers/s3_multipart.go" "Multipart upload"
check_file "internal/drivers/s3_signer.go" "AWS Signature V4"
check_file "internal/api/s3_auth.go" "S3 authentication"

echo ""
echo "=== MULTI-TENANCY (Steps 41-50) ==="
check_file "internal/tenant/manager.go" "Tenant manager"
check_file "internal/api/middleware.go" "Tenant middleware"
check_file "internal/ratelimit/limiter.go" "Rate limiting"

echo ""
echo "=== PLUGIN SYSTEM (Steps 96-100) ==="
check_file "internal/drivers/wasm.go" "WASM runtime"
check_file "internal/drivers/wasm_plugin.go" "Plugin interface"

echo ""
echo "=== BILLING & QUOTAS (Steps 101-110, 171-180) ==="
check_file "internal/billing/stripe.go" "Stripe integration"
check_file "internal/usage/quota_manager.go" "Quota manager"
check_file "internal/usage/tracker.go" "Usage tracking"
check_file "internal/usage/reporting.go" "Usage reporting"
check_file "internal/usage/billing_integration.go" "Billing integration"

echo ""
echo "=== OPTIMIZATION (Steps 111-130) ==="
check_file "internal/drivers/throttle.go" "Bandwidth throttling"
check_file "internal/drivers/retry.go" "Retry policies"
check_file "internal/drivers/queue.go" "Request queuing"
check_file "internal/storage/dedup.go" "Deduplication"
check_file "internal/storage/chunking.go" "Content-aware chunking"

echo ""
echo "=== API GATEWAY (Steps 131-140) ==="
check_file "internal/gateway/gateway.go" "API gateway"
check_file "internal/gateway/router.go" "Request routing"
check_file "internal/gateway/cache/cache.go" "Response caching"
check_file "internal/gateway/validation/validator.go" "Request validation"
check_file "internal/docs/openapi.go" "API documentation"

echo ""
echo "=== WEB DASHBOARD (Steps 141-150) ==="
check_file "internal/dashboard/server.go" "Dashboard server"
check_file "internal/dashboard/auth/auth.go" "Dashboard auth"
check_file "internal/dashboard/handlers/handlers.go" "Dashboard handlers"
check_file "internal/dashboard/static/index.html" "Dashboard UI"

echo ""
echo "=== CLOUD INTEGRATIONS (Steps 151-170) ==="
check_file "internal/drivers/onedrive.go" "OneDrive integration"
check_file "internal/drivers/onedrive_auth.go" "OneDrive auth"
check_file "internal/drivers/idrive.go" "iDrive integration"

echo ""
echo "=== TESTS VERIFICATION ==="
echo "Running test coverage check..."
go test -coverprofile=coverage.out ./... 2>/dev/null
if [ -f coverage.out ]; then
    coverage=$(go tool cover -func=coverage.out | grep total | awk '{print $3}' | sed 's/%//')
    if (( $(echo "$coverage > 60" | bc -l) )); then
        echo -e "${GREEN}✓${NC} Test coverage: ${coverage}%"
    elif (( $(echo "$coverage > 40" | bc -l) )); then
        echo -e "${YELLOW}⚠${NC} Test coverage: ${coverage}% (should be >60%)"
    else
        echo -e "${RED}✗${NC} Test coverage: ${coverage}% (too low)"
    fi
    rm coverage.out
fi

echo ""
echo "=== DATABASE SCHEMA CHECK ==="
# Check if all required tables exist
tables=(
    "tenant_quotas"
    "quota_usage_events"
    "billing_charges"
    "billing_credits"
    "usage_reports"
    "grace_periods"
    "upgrade_triggers"
)

for table in "${tables[@]}"; do
    if grep -q "CREATE TABLE.*$table" internal/usage/*.go internal/billing/*.go 2>/dev/null; then
        echo -e "${GREEN}✓${NC} Table: $table"
    else
        echo -e "${RED}✗${NC} Table: $table - not found"
    fi
done

echo ""
echo "=== INTEGRATION CHECK ==="
# Check if main server integrates all components
check_file "cmd/vaultaire/main.go" "Main server"
check_file "internal/api/server.go" "API server"
check_file "internal/config/config.go" "Configuration"

echo ""
echo "=== SUMMARY ==="
echo "Implemented: $IMPLEMENTED"
echo "Partial/Stubs: $STUBS"
echo "Missing: $MISSING"
TOTAL=$((IMPLEMENTED + STUBS + MISSING))
PERCENT=$((IMPLEMENTED * 100 / TOTAL))

echo ""
if [[ $PERCENT -gt 80 ]; then
    echo -e "${GREEN}System is ${PERCENT}% implemented - Ready for production testing${NC}"
elif [[ $PERCENT -gt 60 ]; then
    echo -e "${YELLOW}System is ${PERCENT}% implemented - Mostly complete but needs work${NC}"
else
    echo -e "${RED}System is only ${PERCENT}% implemented - Significant work needed${NC}"
fi

# Run actual functionality test
echo ""
echo "=== FUNCTIONAL TEST ==="
echo "Starting server for functional test..."

# Start server in background
go run cmd/vaultaire/main.go &
SERVER_PID=$!
sleep 3

# Test basic endpoints
echo "Testing health endpoint..."
curl -s http://localhost:8080/health | grep -q "healthy" && echo -e "${GREEN}✓${NC} Health check passed" || echo -e "${RED}✗${NC} Health check failed"

echo "Testing S3 API..."
curl -s http://localhost:8080/ | grep -q "ListAllMyBuckets" && echo -e "${GREEN}✓${NC} S3 API responding" || echo -e "${RED}✗${NC} S3 API not responding"

# Kill server
kill $SERVER_PID 2>/dev/null

echo ""
echo "=== RECOMMENDATIONS ==="
if [[ $MISSING -gt 5 ]; then
    echo "- Several components are missing, need implementation"
fi
if [[ $STUBS -gt 10 ]; then
    echo "- Many components are stubs, need completion"
fi
echo "- Run 'go test ./...' to verify all tests pass"
echo "- Check 'go build ./...' for compilation errors"
