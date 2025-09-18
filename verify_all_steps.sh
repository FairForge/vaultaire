#!/bin/bash
set -e

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
    if eval "$check" > /dev/null 2>&1; then
        echo -e "${GREEN}✓${NC}"
        ((PASS++))
        return 0
    else
        echo -e "${RED}✗${NC}"
        ((FAIL++))
        return 1
    fi
}

check_partial() {
    local step=$1
    local name=$2
    local check=$3

    echo -n "Step $step: $name... "
    if eval "$check" > /dev/null 2>&1; then
        echo -e "${YELLOW}~${NC} (partial)"
        ((PARTIAL++))
        return 0
    else
        echo -e "${RED}✗${NC}"
        ((FAIL++))
        return 1
    fi
}

echo "PHASE 1: Core Architecture (11-50)"
echo "-----------------------------------"
check_step "11-20" "Core Interfaces" "test -f internal/storage/backend.go && grep -q 'type Backend interface' internal/storage/backend.go"
check_step "21-30" "LocalBackend" "test -f internal/drivers/local.go && grep -q 'func.*Get' internal/drivers/local.go"
check_step "31-40" "S3 API" "test -f internal/api/s3.go && grep -q 'ListBuckets' internal/api/s3.go"
check_step "41-50" "Multi-tenancy" "test -f internal/tenant/tenant.go"

echo ""
echo "PHASE 2: Storage Backends (51-200)"
echo "-----------------------------------"
check_step "51-60" "Advanced File Ops" "grep -q 'symlink\|xattr' internal/drivers/local.go"
check_step "61-70" "Atomic Operations" "test -f internal/drivers/atomic.go"
check_step "71-75" "Parallel I/O" "grep -q 'ReaderPool\|WriterPool' internal/drivers/*.go"
check_step "76-85" "S3 Backend" "test -f internal/drivers/s3.go && grep -q 'aws-sdk' go.mod"
check_step "86-95" "S3 Advanced" "grep -q 'retry\|circuit' internal/drivers/*.go"
check_step "96-100" "Plugin System" "test -d internal/plugins"
check_partial "101-110" "Lyve/Billing" "test -f internal/drivers/lyve.go"
check_step "111-120" "Optimization" "grep -q 'chunk\|compress' internal/drivers/*.go"
check_step "121-125" "Rate Limiting" "test -f internal/ratelimit/ratelimit.go"
check_step "126-130" "Storage Opt" "test -f internal/storage/dedup.go"
check_step "131-140" "API Gateway" "test -f internal/gateway/gateway.go"
check_step "141-150" "Dashboard" "test -d internal/dashboard"
check_step "151-160" "OneDrive" "test -f internal/drivers/onedrive.go"
check_step "161-170" "iDrive" "test -f internal/drivers/idrive.go"
check_step "171-180" "Quota System" "psql -d vaultaire -c 'SELECT 1 FROM tenant_quotas LIMIT 1' 2>/dev/null"
check_step "181-190" "Backend Features" "test -f internal/engine/health.go"
check_step "191-200" "Testing" "test -d tests/integration"

echo ""
echo "DATABASE VERIFICATION"
echo "--------------------"
echo -n "PostgreSQL Connection... "
if psql -d vaultaire -c '\dt' > /dev/null 2>&1; then
    echo -e "${GREEN}✓${NC}"
    echo "Tables found:"
    psql -d vaultaire -c "\dt" 2>/dev/null | grep -E "tenants|quotas|access_patterns" | sed 's/^/  /'
else
    echo -e "${RED}✗${NC}"
fi

echo ""
echo "SERVER TEST"
echo "-----------"
echo -n "Starting test server... "
timeout 3 ./bin/vaultaire > /tmp/server.log 2>&1 &
PID=$!
sleep 2

if ps -p $PID > /dev/null 2>&1; then
    echo -e "${GREEN}✓${NC}"

    echo -n "S3 API responds... "
    if curl -s http://localhost:8000/ > /dev/null 2>&1; then
        echo -e "${GREEN}✓${NC}"
    else
        echo -e "${RED}✗${NC}"
    fi

    kill $PID 2>/dev/null || true
else
    echo -e "${RED}✗${NC}"
    echo "Server failed to start. Last logs:"
    tail -5 /tmp/server.log
fi

echo ""
echo "SUMMARY"
echo "-------"
echo -e "Passed: ${GREEN}$PASS${NC}"
echo -e "Partial: ${YELLOW}$PARTIAL${NC}"
echo -e "Failed: ${RED}$FAIL${NC}"

if [ $FAIL -gt 0 ]; then
    echo ""
    echo "To see details for failed steps, run:"
    echo "  ./debug_step.sh <step_range>"
fi
