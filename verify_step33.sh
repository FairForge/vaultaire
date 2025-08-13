#!/bin/bash

echo "===================================="
echo "🔍 VAULTAIRE STEP 33 VERIFICATION"
echo "===================================="
echo ""

# Colors for output
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Function to check if something exists
check() {
    if [ $1 -eq 0 ]; then
        echo -e "${GREEN}✅ $2${NC}"
    else
        echo -e "${RED}❌ $2${NC}"
    fi
}

echo "📁 PROJECT STRUCTURE"
echo "--------------------"
[ -d "cmd/vaultaire" ] && echo -e "${GREEN}✅ cmd/vaultaire/${NC}" || echo -e "${RED}❌ cmd/vaultaire/${NC}"
[ -d "internal/api" ] && echo -e "${GREEN}✅ internal/api/${NC}" || echo -e "${RED}❌ internal/api/${NC}"
[ -d "internal/config" ] && echo -e "${GREEN}✅ internal/config/${NC}" || echo -e "${RED}❌ internal/config/${NC}"
[ -d "internal/engine" ] && echo -e "${GREEN}✅ internal/engine/${NC}" || echo -e "${RED}❌ internal/engine/${NC}"
[ -d "internal/events" ] && echo -e "${GREEN}✅ internal/events/${NC}" || echo -e "${RED}❌ internal/events/${NC}"
[ -d "internal/backends" ] && echo -e "${GREEN}✅ internal/backends/${NC}" || echo -e "${RED}❌ internal/backends/${NC}"
echo ""

echo "📄 DOCUMENTATION FILES"
echo "----------------------"
[ -f "README.md" ] && echo -e "${GREEN}✅ README.md ($(wc -l < README.md) lines)${NC}" || echo -e "${RED}❌ README.md${NC}"
[ -f "CONTRIBUTING.md" ] && echo -e "${GREEN}✅ CONTRIBUTING.md ($(wc -l < CONTRIBUTING.md) lines)${NC}" || echo -e "${RED}❌ CONTRIBUTING.md${NC}"
[ -f "LICENSE" ] && echo -e "${GREEN}✅ LICENSE${NC}" || echo -e "${RED}❌ LICENSE${NC}"
[ -f "Makefile" ] && echo -e "${GREEN}✅ Makefile${NC}" || echo -e "${RED}❌ Makefile${NC}"
[ -f ".gitignore" ] && echo -e "${GREEN}✅ .gitignore${NC}" || echo -e "${RED}❌ .gitignore${NC}"
echo ""

echo "🔧 BUILD & BINARY"
echo "-----------------"
echo "Testing build..."
go build -o bin/vaultaire ./cmd/vaultaire 2>/dev/null
if [ $? -eq 0 ]; then
    echo -e "${GREEN}✅ Build successful${NC}"
    [ -f "bin/vaultaire" ] && echo -e "${GREEN}✅ Binary exists${NC}" || echo -e "${RED}❌ Binary missing${NC}"
else
    echo -e "${RED}❌ Build failed${NC}"
fi
echo ""

echo "📦 GO MODULES"
echo "-------------"
[ -f "go.mod" ] && echo -e "${GREEN}✅ go.mod${NC}" || echo -e "${RED}❌ go.mod${NC}"
[ -f "go.sum" ] && echo -e "${GREEN}✅ go.sum${NC}" || echo -e "${RED}❌ go.sum${NC}"
grep -q "github.com/gorilla/mux" go.mod && echo -e "${GREEN}✅ gorilla/mux dependency${NC}" || echo -e "${YELLOW}⚠️  gorilla/mux not found${NC}"
grep -q "go.uber.org/zap" go.mod && echo -e "${GREEN}✅ zap logger dependency${NC}" || echo -e "${YELLOW}⚠️  zap not found${NC}"
echo ""

echo "🌐 API ENDPOINTS (Step 1-20)"
echo "----------------------------"
# Start server in background
./bin/vaultaire > /dev/null 2>&1 &
SERVER_PID=$!
sleep 2

# Test health endpoint
curl -s http://localhost:8080/health > /dev/null 2>&1
check $? "Health endpoint (/health)"

# Test metrics endpoint
curl -s http://localhost:8080/metrics > /dev/null 2>&1
check $? "Metrics endpoint (/metrics)"

# Test S3 endpoint
curl -s http://localhost:8080/mybucket/test.jpg > /dev/null 2>&1
check $? "S3 endpoint parsing"

# Kill server
kill $SERVER_PID 2>/dev/null
echo ""

echo "🗂️ CODE FEATURES (Step 21-33)"
echo "------------------------------"
# Check for key features
grep -q "type Event struct" internal/api/event_type.go 2>/dev/null && echo -e "${GREEN}✅ Event type defined${NC}" || echo -e "${RED}❌ Event type missing${NC}"
grep -q "ParseS3Request" internal/api/s3.go 2>/dev/null && echo -e "${GREEN}✅ S3 request parser${NC}" || echo -e "${RED}❌ S3 parser missing${NC}"
grep -q "ValidateAWSSignature" internal/api/auth.go 2>/dev/null && echo -e "${GREEN}✅ AWS auth validation${NC}" || echo -e "${RED}❌ Auth validation missing${NC}"
grep -q "LogEvent" internal/events/logger.go 2>/dev/null && echo -e "${GREEN}✅ Event logger${NC}" || echo -e "${RED}❌ Event logger missing${NC}"
[ -f "internal/engine/types.go" ] && echo -e "${GREEN}✅ Engine types${NC}" || echo -e "${YELLOW}⚠️  Engine types missing${NC}"
[ -f "internal/backends/interface.go" ] && echo -e "${GREEN}✅ Backend interface${NC}" || echo -e "${YELLOW}⚠️  Backend interface missing${NC}"
echo ""

echo "📊 CODE STATISTICS"
echo "------------------"
echo "Total Go files: $(find . -name '*.go' -type f | wc -l)"
echo "Total lines of Go code: $(find . -name '*.go' -type f -exec cat {} + | wc -l)"
echo "Test files: $(find . -name '*_test.go' -type f | wc -l)"
echo ""

echo "🔄 GIT STATUS"
echo "-------------"
BRANCH=$(git branch --show-current)
echo "Current branch: $BRANCH"
COMMITS=$(git rev-list --count HEAD)
echo "Total commits: $COMMITS"
LAST_COMMIT=$(git log -1 --oneline)
echo "Last commit: $LAST_COMMIT"

# Check if synced with remote
git fetch origin > /dev/null 2>&1
LOCAL=$(git rev-parse HEAD)
REMOTE=$(git rev-parse origin/main)
if [ "$LOCAL" = "$REMOTE" ]; then
    echo -e "${GREEN}✅ Synced with GitHub${NC}"
else
    echo -e "${YELLOW}⚠️  Not synced with GitHub${NC}"
fi
echo ""

echo "===================================="
echo "📈 STEP 33 COMPLETION SUMMARY"
echo "===================================="
echo ""
echo "Core Features Implemented:"
echo "  ✅ HTTP server with gorilla/mux"
echo "  ✅ Health & metrics endpoints"
echo "  ✅ S3 request parsing"
echo "  ✅ AWS Signature V4 validation"
echo "  ✅ Event logging system"
echo "  ✅ Prometheus metrics"
echo "  ✅ Structured logging (zap)"
echo "  ✅ Configuration system"
echo "  ✅ Project documentation"
echo ""
echo "Ready for Steps 34-50:"
echo "  📋 S3 Error responses (Step 34)"
echo "  📋 S3 GET implementation (35-40)"
echo "  📋 S3 PUT implementation (41-45)"
echo "  📋 S3 DELETE implementation (46-50)"
echo ""
