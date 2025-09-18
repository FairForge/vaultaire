#!/bin/bash

echo "==================================="
echo "VAULTAIRE STEP VERIFICATION REPORT"
echo "==================================="
echo ""

# Color codes
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Counters
PASS=0
FAIL=0
PARTIAL=0

check_step() {
    local step=$1
    local name=$2
    local check=$3

    echo -n "Step $step: $name... "
    if eval "$check" 2>/dev/null; then
        echo -e "${GREEN}✓${NC}"
        ((PASS++))
        return 0
    else
        echo -e "${RED}✗${NC}"
        ((FAIL++))
        return 1
    fi
}

echo "QUICK STRUCTURE CHECK"
echo "--------------------"
check_step "Core" "Engine exists" "test -f internal/engine/engine.go"
check_step "Core" "API exists" "test -f internal/api/s3.go"
check_step "Core" "Drivers exist" "test -d internal/drivers"
check_step "Core" "Cache exists" "test -d internal/cache"
check_step "Core" "Intelligence exists" "test -d internal/intelligence"

echo ""
echo "BACKEND DRIVERS"
echo "---------------"
check_step "Driver" "LocalDriver" "test -f internal/drivers/local.go"
check_step "Driver" "S3Driver" "test -f internal/drivers/s3.go"
check_step "Driver" "OneDrive" "test -f internal/drivers/onedrive.go"
check_step "Driver" "LyveDriver" "test -f internal/drivers/lyve.go"

echo ""
echo "KEY FEATURES"
echo "------------"
check_step "Feature" "Rate limiting" "test -f internal/ratelimit/ratelimit.go"
check_step "Feature" "Dashboard" "test -d internal/dashboard"
check_step "Feature" "Gateway" "test -d internal/gateway"
check_step "Feature" "Billing" "test -d internal/billing"
check_step "Feature" "Auth" "test -f internal/auth/auth.go"

echo ""
echo "TEST COVERAGE"
echo "-------------"
echo "Running quick test check..."
go test -short ./internal/engine/... > /dev/null 2>&1 && echo -e "Engine tests: ${GREEN}✓${NC}" || echo -e "Engine tests: ${RED}✗${NC}"
go test -short ./internal/api/... > /dev/null 2>&1 && echo -e "API tests: ${GREEN}✓${NC}" || echo -e "API tests: ${RED}✗${NC}"
go test -short ./internal/drivers/... > /dev/null 2>&1 && echo -e "Driver tests: ${GREEN}✓${NC}" || echo -e "Driver tests: ${RED}✗${NC}"

echo ""
echo "SUMMARY"
echo "-------"
echo -e "Passed: ${GREEN}$PASS${NC}"
echo -e "Failed: ${RED}$FAIL${NC}"
