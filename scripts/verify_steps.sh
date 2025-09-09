#!/bin/bash
# verify_steps.sh - Verify that completed steps are actually implemented

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

ERRORS=0
WARNINGS=0

echo "🔍 Vaultaire Step Verification Script"
echo "======================================"

# Function to check if a file exists
check_file() {
    if [ -f "$1" ]; then
        echo -e "${GREEN}✓${NC} $2"
        return 0
    else
        echo -e "${RED}✗${NC} $2 - Missing: $1"
        ((ERRORS++))
        return 1
    fi
}

# Function to check if a directory exists
check_dir() {
    if [ -d "$1" ]; then
        echo -e "${GREEN}✓${NC} $2"
        return 0
    else
        echo -e "${RED}✗${NC} $2 - Missing: $1"
        ((ERRORS++))
        return 1
    fi
}

# Function to check if package has tests
check_tests() {
    if ls $1/*_test.go 1> /dev/null 2>&1; then
        echo -e "${GREEN}✓${NC} $2 has tests"
        return 0
    else
        echo -e "${YELLOW}⚠${NC} $2 - No tests found"
        ((WARNINGS++))
        return 1
    fi
}

# Function to check if a function/type exists in Go files
check_go_implementation() {
    if grep -r "$1" --include="*.go" . > /dev/null 2>&1; then
        echo -e "${GREEN}✓${NC} $2"
        return 0
    else
        echo -e "${RED}✗${NC} $2 - Not found: $1"
        ((ERRORS++))
        return 1
    fi
}

echo ""
echo "Phase 1: Foundation (Steps 1-50)"
echo "---------------------------------"

# Steps 1-10: Project Setup
echo "Steps 1-10: Project Setup"
check_file ".gitignore" "Step 1: .gitignore"
check_file "go.mod" "Step 2: go.mod"
check_file "Makefile" "Step 4: Makefile"
check_file ".github/workflows/ci.yml" "Step 5: GitHub Actions CI"
check_file ".pre-commit-config.yaml" "Step 6: Pre-commit hooks"
check_file "README.md" "Step 8: README"
check_file "LICENSE" "Step 9: LICENSE"

# Steps 11-20: Core Interfaces
echo ""
echo "Steps 11-20: Core Interfaces"
check_file "internal/engine/backend.go" "Step 11: Backend interface"
check_go_implementation "type Engine struct" "Step 12: Engine structure"
check_go_implementation "type Container" "Step 13: Container abstraction"
check_go_implementation "type Artifact" "Step 14: Artifact abstraction"
check_file "internal/engine/errors.go" "Step 15: Error types"
check_tests "internal/engine" "Step 20: Interface tests"

# Steps 21-30: LocalBackend
echo ""
echo "Steps 21-30: LocalBackend"
check_file "internal/drivers/local.go" "Step 21: LocalBackend"
check_go_implementation "func.*LocalDriver.*Get" "Step 22: Get method"
check_go_implementation "func.*LocalDriver.*Put" "Step 23: Put method"
check_go_implementation "func.*LocalDriver.*Delete" "Step 24: Delete method"
check_go_implementation "func.*LocalDriver.*List" "Step 25: List method"
check_tests "internal/drivers" "Step 30: LocalBackend tests"

# Steps 31-40: S3 API
echo ""
echo "Steps 31-40: S3 API Foundation"
check_go_implementation "chi.Router" "Step 31: Chi router"
check_file "internal/api/server.go" "Step 32: HTTP server"
check_go_implementation "ListBuckets" "Step 34: ListBuckets"
check_go_implementation "CreateBucket" "Step 35: CreateBucket"
check_go_implementation "DeleteBucket" "Step 36: DeleteBucket"
check_tests "internal/api" "Step 40: S3 tests"

# Steps 41-50: Multi-tenancy
echo ""
echo "Steps 41-50: Multi-Tenancy"
check_go_implementation "GetTenantID" "Step 41: Tenant context"
check_go_implementation "TenantMiddleware" "Step 42: Tenant middleware"
check_file "internal/tenant/tenant.go" "Step 43: Tenant isolation"
check_file "internal/ratelimit/limiter.go" "Step 48: Rate limiting"
check_go_implementation "prometheus" "Step 50: Prometheus metrics"

echo ""
echo "Phase 2: Storage Backends (Steps 51-137)"
echo "-----------------------------------------"

# Steps 51-60: Advanced File Operations
echo "Steps 51-60: Advanced File Operations"
check_go_implementation "Symlink" "Step 51: Symlink support"
check_go_implementation "Chmod" "Step 52: Permission handling"
check_go_implementation "Xattr" "Step 53: Extended attributes"
check_go_implementation "Flock" "Step 54: File locking"

# Steps 61-70: Atomic Operations
echo ""
echo "Steps 61-70: Atomic Operations"
check_go_implementation "AtomicWrite" "Step 61: Atomic writes"
check_go_implementation "Transaction" "Step 62: Transaction support"
check_go_implementation "fsnotify" "Step 66: File watching"

# Steps 71-75: Parallel I/O
echo ""
echo "Steps 71-75: Parallel I/O"
check_go_implementation "ReaderPool" "Step 71: Reader pool"
check_go_implementation "ParallelRead" "Step 72: Parallel reading"

# Steps 76-85: S3 Backend
echo ""
echo "Steps 76-85: S3 Backend"
check_file "internal/drivers/s3.go" "Step 76: S3 driver"
check_go_implementation "aws.Config" "Step 76: AWS SDK v2"
check_go_implementation "MultipartUpload" "Step 81: Multipart upload"

# Steps 86-95: S3 Advanced
echo ""
echo "Steps 86-95: S3 Advanced Features"
check_go_implementation "SignatureV4" "Step 86: AWS Signature V4"
check_go_implementation "PresignedURL" "Step 87: Presigned URLs"
check_go_implementation "CircuitBreaker" "Step 93: Circuit breaker"

# Steps 96-100: Plugin System
echo ""
echo "Steps 96-100: Plugin System"
check_go_implementation "wazero" "Step 96: WASM runtime"
check_file "internal/drivers/wasm.go" "Step 97: Plugin interface"

# Steps 101-110: Billing
echo ""
echo "Steps 101-110: Lyve Integration & Billing"
check_file "internal/drivers/lyve.go" "Step 101: Quotaless wrapper"
check_file "internal/billing/stripe.go" "Step 108: Stripe integration"
check_file "internal/usage/tracker.go" "Step 107: Usage tracking"

# Steps 111-120: Optimization
echo ""
echo "Steps 111-120: Optimization & Reliability"
check_go_implementation "ChunkedWrite" "Step 111: Chunked transfer"
check_go_implementation "ResumableUpload" "Step 112: Resume capability"
check_go_implementation "BandwidthThrottle" "Step 113: Bandwidth throttling"
check_go_implementation "RetryWithBackoff" "Step 116: Retry policies"

# Steps 121-125: Rate Limiting
echo ""
echo "Steps 121-125: Advanced Rate Limiting"
check_go_implementation "PerOperationLimits" "Step 121: Per-operation limits"
check_go_implementation "TenantLimits" "Step 122: Tenant-specific limits"
check_go_implementation "BurstHandler" "Step 123: Burst handling"
check_go_implementation "RateLimitHeaders" "Step 124: Rate limit headers"
check_go_implementation "DistributedLimiter" "Step 125: Distributed limiting"

# Steps 126-130: Storage Optimization
echo ""
echo "Steps 126-130: Storage Optimization"
check_file "internal/storage/dedup.go" "Step 126: Deduplication"
check_file "internal/storage/chunking.go" "Step 127: Content-aware chunking"
check_file "internal/storage/delta.go" "Step 128: Delta encoding"
check_file "internal/storage/tiering.go" "Step 129: Storage tiering"
check_file "internal/storage/garbage_collector.go" "Step 130: Garbage collection"

# Steps 131-137: API Gateway
echo ""
echo "Steps 131-137: API Gateway (Current)"
check_file "internal/gateway/gateway.go" "Step 131: API gateway"
check_file "internal/gateway/router.go" "Step 132: Request routing"
check_file "internal/gateway/versioning.go" "Step 133: API versioning"
check_file "internal/gateway/pipeline.go" "Step 134: Middleware pipeline"
check_file "internal/gateway/apikey.go" "Step 135: API key management"
check_file "internal/gateway/ratelimit_integration.go" "Step 136: Rate limit integration"
check_file "internal/gateway/validation/validator.go" "Step 137: Request validation"

echo ""
echo "======================================"
echo "Verification Summary"
echo "======================================"

# Run tests to verify implementations work
echo ""
echo "Running tests to verify implementations..."
if go test ./... -short > /dev/null 2>&1; then
    echo -e "${GREEN}✓${NC} All tests pass"
else
    echo -e "${RED}✗${NC} Some tests are failing"
    ((ERRORS++))
fi

# Check test coverage
echo ""
echo "Checking test coverage..."
COVERAGE=$(go test ./... -cover 2>/dev/null | grep -oE '[0-9]+\.[0-9]+%' | head -1 | cut -d'%' -f1)
if [ ! -z "$COVERAGE" ]; then
    if (( $(echo "$COVERAGE > 70" | bc -l) )); then
        echo -e "${GREEN}✓${NC} Test coverage: ${COVERAGE}%"
    else
        echo -e "${YELLOW}⚠${NC} Test coverage: ${COVERAGE}% (below 70% target)"
        ((WARNINGS++))
    fi
fi

echo ""
echo "Results:"
echo "--------"
echo -e "Errors: ${RED}${ERRORS}${NC}"
echo -e "Warnings: ${YELLOW}${WARNINGS}${NC}"

if [ $ERRORS -eq 0 ]; then
    echo -e "${GREEN}✅ All verified steps are implemented correctly!${NC}"
    exit 0
else
    echo -e "${RED}❌ Some implementations are missing or incorrect${NC}"
    exit 1
fi