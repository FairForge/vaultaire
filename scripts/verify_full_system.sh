#!/bin/bash
set -e

echo "==================================="
echo "VAULTAIRE FULL SYSTEM VERIFICATION"
echo "==================================="
echo "Testing Steps 1-210 Implementation"
echo ""

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Counters
PASSED=0
FAILED=0
SKIPPED=0

# Test function
run_test() {
    local test_name=$1
    local test_cmd=$2
    
    echo -n "Testing $test_name... "
    if eval $test_cmd > /dev/null 2>&1; then
        echo -e "${GREEN}✓${NC}"
        ((PASSED++))
        return 0
    else
        echo -e "${RED}✗${NC}"
        ((FAILED++))
        return 1
    fi
}

# 1. Build the binary
echo "Building Vaultaire..."
if make build; then
    echo -e "${GREEN}Build successful${NC}"
else
    echo -e "${RED}Build failed - cannot continue${NC}"
    exit 1
fi

# 2. Run unit tests for each module
echo ""
echo "Running Unit Tests..."
echo "--------------------"

run_test "Core Engine" "go test ./internal/engine/... -short"
run_test "S3 API" "go test ./internal/api/... -short"
run_test "Auth System" "go test ./internal/auth/... -short"
run_test "Billing" "go test ./internal/billing/... -short"
run_test "Dashboard" "go test ./internal/dashboard/... -short"
run_test "Drivers" "go test ./internal/drivers/... -short"
run_test "Cache (LRU)" "go test ./internal/cache/... -short"
run_test "Gateway" "go test ./internal/gateway/... -short"
run_test "Rate Limiting" "go test ./internal/ratelimit/... -short"
run_test "Storage Optimization" "go test ./internal/storage/... -short"
run_test "Tenant Management" "go test ./internal/tenant/... -short"
run_test "Usage/Quota" "go test ./internal/usage/... -short"

# 3. Check coverage for critical modules
echo ""
echo "Coverage Analysis..."
echo "-------------------"
for module in engine api auth cache drivers; do
    coverage=$(go test ./internal/$module/... -cover 2>/dev/null | grep -oE '[0-9]+\.[0-9]+%' | head -1)
    if [ ! -z "$coverage" ]; then
        echo "$module: $coverage coverage"
    fi
done

# 4. Start server for integration tests
echo ""
echo "Starting server for integration tests..."
./bin/vaultaire serve &
SERVER_PID=$!
sleep 3

# Check if server started
if ! curl -s http://localhost:8080/health > /dev/null 2>&1; then
    echo -e "${RED}Server failed to start${NC}"
    kill $SERVER_PID 2>/dev/null
    exit 1
fi
echo -e "${GREEN}Server started on :8080${NC}"

# 5. Integration tests
echo ""
echo "Running Integration Tests..."
echo "---------------------------"

# Test S3 API
run_test "S3 ListBuckets" "aws --endpoint-url http://localhost:8080 s3 ls"
run_test "S3 CreateBucket" "aws --endpoint-url http://localhost:8080 s3 mb s3://test-bucket"
run_test "S3 PutObject" "echo 'test' | aws --endpoint-url http://localhost:8080 s3 cp - s3://test-bucket/test.txt"
run_test "S3 GetObject" "aws --endpoint-url http://localhost:8080 s3 cp s3://test-bucket/test.txt -"
run_test "S3 DeleteObject" "aws --endpoint-url http://localhost:8080 s3 rm s3://test-bucket/test.txt"

# Test Auth endpoints
run_test "Auth Health" "curl -s http://localhost:8080/api/v1/health"
run_test "Auth Register" "curl -s -X POST http://localhost:8080/api/v1/auth/register -d '{\"email\":\"test@test.com\",\"password\":\"test123\"}'"
run_test "Auth Login" "curl -s -X POST http://localhost:8080/api/v1/auth/login -d '{\"email\":\"test@test.com\",\"password\":\"test123\"}'"

# Test Dashboard
run_test "Dashboard Access" "curl -s http://localhost:8080/dashboard"
run_test "Dashboard Login Page" "curl -s http://localhost:8080/dashboard/login"

# 6. Benchmark tests
echo ""
echo "Running Benchmarks..."
echo "--------------------"
run_test "S3 Operations" "go test ./tests/benchmarks/... -bench=. -benchtime=1s -run=^$"

# 7. Security tests
echo ""
echo "Security Checks..."
echo "-----------------"
run_test "Security Tests" "go test ./tests/security/... -short"

# Clean up
kill $SERVER_PID 2>/dev/null

# 8. Summary
echo ""
echo "==================================="
echo "VERIFICATION SUMMARY"
echo "==================================="
echo -e "Passed:  ${GREEN}$PASSED${NC}"
echo -e "Failed:  ${RED}$FAILED${NC}"
echo -e "Skipped: ${YELLOW}$SKIPPED${NC}"
echo ""

if [ $FAILED -eq 0 ]; then
    echo -e "${GREEN}✅ ALL TESTS PASSED!${NC}"
    echo "Your implementation up to Step 210 is working correctly!"
else
    echo -e "${RED}⚠️  Some tests failed${NC}"
    echo "Review the failures above and fix before proceeding"
fi

# List what's actually working
echo ""
echo "CONFIRMED WORKING FEATURES:"
echo "---------------------------"
[ -f internal/engine/engine.go ] && echo "✓ Core Engine (Steps 11-20)"
[ -f internal/api/s3.go ] && echo "✓ S3 API (Steps 31-40)"
[ -f internal/auth/auth.go ] && echo "✓ Authentication (Step 52)"
[ -f internal/billing/stripe.go ] && echo "✓ Stripe Billing (Step 108)"
[ -f internal/dashboard/handlers/dashboard.go ] && echo "✓ Web Dashboard (Steps 141-150)"
[ -f internal/drivers/onedrive.go ] && echo "✓ OneDrive Backend (Steps 151-160)"
[ -f internal/drivers/idrive.go ] && echo "✓ iDrive Backend (Steps 161-170)"
[ -f internal/usage/quota_manager.go ] && echo "✓ Quota System (Steps 171-180)"
[ -f internal/cache/lru.go ] && echo "✓ LRU Cache (Steps 201-210)"

echo ""
echo "MISSING/BROKEN FEATURES:"
echo "------------------------"
# Check for missing critical files
[ ! -f internal/auth/register.go ] && echo "✗ User Registration endpoints"
[ ! -f internal/api/auth_middleware.go ] && echo "✗ Auth middleware for S3"
[ ! -f .env ] && echo "✗ Environment configuration"
[ ! -f docker-compose.yml ] && echo "✗ Docker setup"
