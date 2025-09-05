#!/bin/bash

echo "Quick Vaultaire Verification"
echo "============================"

# Count Go files
echo ""
echo "📊 Project Statistics:"
echo "  Go files: $(find . -name "*.go" -not -path "./vendor/*" | wc -l)"
echo "  Test files: $(find . -name "*_test.go" -not -path "./vendor/*" | wc -l)"
echo "  Packages: $(go list ./... 2>/dev/null | wc -l)"

# Check key implementations
echo ""
echo "✅ Key Implementations:"
[ -f "internal/engine/backend.go" ] && echo "  ✓ Backend interface" || echo "  ✗ Backend interface"
[ -f "internal/drivers/local.go" ] && echo "  ✓ LocalDriver" || echo "  ✗ LocalDriver"
[ -f "internal/drivers/s3.go" ] && echo "  ✓ S3Driver" || echo "  ✗ S3Driver"
[ -f "internal/api/server.go" ] && echo "  ✓ API Server" || echo "  ✗ API Server"
[ -f "internal/tenant/tenant.go" ] && echo "  ✓ Multi-tenancy" || echo "  ✗ Multi-tenancy"
[ -f "internal/ratelimit/limiter.go" ] && echo "  ✓ Rate limiting" || echo "  ✗ Rate limiting"
[ -f "internal/gateway/validation/validator.go" ] && echo "  ✓ Request validation (Step 137)" || echo "  ✗ Request validation"

# Test status
echo ""
echo "🧪 Test Status:"
if go test ./... -short > /dev/null 2>&1; then
    echo "  ✓ All tests passing"
else
    echo "  ✗ Some tests failing"
    echo "  Run 'go test ./...' to see details"
fi

# Latest implementations
echo ""
echo "📦 Latest Features (Steps 131-137):"
[ -f "internal/gateway/gateway.go" ] && echo "  ✓ API Gateway" || echo "  ✗ API Gateway"
[ -f "internal/gateway/router.go" ] && echo "  ✓ Request routing" || echo "  ✗ Request routing"
[ -f "internal/gateway/versioning.go" ] && echo "  ✓ API versioning" || echo "  ✗ API versioning"
[ -f "internal/gateway/pipeline.go" ] && echo "  ✓ Middleware pipeline" || echo "  ✗ Middleware pipeline"
[ -f "internal/gateway/apikey.go" ] && echo "  ✓ API key management" || echo "  ✗ API key management"
[ -f "internal/gateway/validation/validator.go" ] && echo "  ✓ Request validation" || echo "  ✗ Request validation"

echo ""
echo "Next Step: 138 - Response caching layer"
