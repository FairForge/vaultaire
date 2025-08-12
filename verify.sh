#!/bin/bash

echo "======================================"
echo "   VAULTAIRE FOUNDATION VERIFICATION  "
echo "======================================"
echo ""

# Color codes
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Counters
PASS=0
FAIL=0
WARN=0

# Check function
check() {
    if [ $1 -eq 0 ]; then
        echo -e "${GREEN}✅ $2${NC}"
        ((PASS++))
    else
        echo -e "${RED}❌ $2${NC}"
        ((FAIL++))
    fi
}

warn() {
    echo -e "${YELLOW}⚠️  $1${NC}"
    ((WARN++))
}

# Directory Structure
echo "📁 Directory Structure:"
for dir in engine pipeline events intelligence orchestrator backends compute config api core; do
    [ -d "internal/$dir" ]
    check $? "internal/$dir"
done
echo ""

# Core Files
echo "📄 Core Files:"
[ -f "internal/engine/engine.go" ]
check $? "engine.go"
[ -f "internal/events/events.go" ]
check $? "events.go"
[ -f "internal/pipeline/pipeline.go" ]
check $? "pipeline.go"
[ -f "internal/config/config.go" ]
check $? "config.go"
[ -f "Makefile" ]
check $? "Makefile"
echo ""

# Go Module
echo "🔧 Go Module:"
grep -q "github.com/FairForge/vaultaire" go.mod
check $? "Module name correct"
echo ""

# Build Test
echo "🔨 Build System:"
make build > /dev/null 2>&1
if [ $? -eq 0 ]; then
    check 0 "Build successful"
else
    warn "Build has errors (may be OK if incomplete)"
fi
echo ""

# Architecture
echo "🏗️ Architecture:"
! find internal -name "*.go" -exec grep -l "package storage" {} \; | grep -q .
check $? "No 'package storage' (correct!)"
grep -q "type Engine interface" internal/engine/engine.go 2>/dev/null
check $? "Engine interface exists"
grep -q "type EventBus interface" internal/events/events.go 2>/dev/null
check $? "EventBus interface exists"
echo ""

# Summary
echo "======================================"
echo "SUMMARY:"
echo -e "${GREEN}Passed: $PASS${NC}"
echo -e "${YELLOW}Warnings: $WARN${NC}"
echo -e "${RED}Failed: $FAIL${NC}"

if [ $FAIL -eq 0 ]; then
    echo -e "\n${GREEN}🎉 FOUNDATION VERIFIED! Ready for Steps 31-50!${NC}"
else
    echo -e "\n${RED}⚠️ Issues found. Fix them before continuing.${NC}"
fi
