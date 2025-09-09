#!/bin/bash

echo "=== CHECKING VAULTAIRE FUNCTIONALITY ==="

# Test 1: Server responds
echo -n "1. Health endpoint: "
curl -s http://localhost:8000/health | grep -q "healthy" && echo "✓ PASS" || echo "✗ FAIL"

# Test 2: S3 API responds (unauthenticated)
echo -n "2. S3 API responds: "
curl -s http://localhost:8000/ | grep -q "ListAllMyBucketsResult" && echo "✓ PASS" || echo "✗ FAIL"

# Test 3: Check for TODO/STUB markers
echo -n "3. Files with TODOs: "
TODO_COUNT=$(find . -name "*.go" -exec grep -l "TODO\|STUB\|PLACEHOLDER" {} \; | wc -l | tr -d ' ')
echo "$TODO_COUNT files"

# Test 4: Try S3 operations without auth
echo "4. Testing S3 operations:"
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test

# Try to create a bucket
echo -n "   - Create bucket: "
aws --endpoint-url=http://localhost:8000 s3 mb s3://test 2>&1 | grep -q "SignatureDoesNotMatch" && echo "✗ Auth broken" || echo "✓ Works"

# Test 5: Check database
echo -n "5. Database tables: "
psql -U postgres -d vaultaire_test -c "\dt" 2>/dev/null | grep -q "tenant_quotas" && echo "✓ Exist" || echo "✗ Missing"

echo ""
echo "=== CRITICAL FILES CHECK ==="
for file in "internal/api/auth.go" "internal/engine/engine.go" "internal/usage/quota_manager.go"; do
    if [ -f "$file" ]; then
        TODOS=$(grep -c "TODO\|STUB" "$file" 2>/dev/null || echo 0)
        LINES=$(wc -l < "$file")
        echo "$file: $LINES lines, $TODOS TODOs"
    else
        echo "$file: MISSING"
    fi
done
