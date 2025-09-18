#!/bin/bash

echo "===================================="
echo "FUNCTIONAL VERIFICATION (Steps 1-200)"
echo "===================================="
echo ""

# Test if server actually works
echo "1. SERVER STARTUP TEST"
echo "----------------------"
STORAGE_MODE=local S3_ACCESS_KEY=test S3_SECRET_KEY=test ./bin/vaultaire &
SERVER_PID=$!
sleep 2
ps aux | grep vaultaire
kill $SERVER_PID


if ps -p $PID > /dev/null; then
    echo "✓ Server starts"

    # Test S3 operations
    echo ""
    echo "2. S3 OPERATIONS TEST"
    echo "---------------------"

    # Create bucket
    aws s3 mb s3://testbucket --endpoint-url http://localhost:8000 2>/dev/null && echo "✓ Create bucket" || echo "✗ Create bucket"

    # Upload file
    echo "test data" > /tmp/test.txt
    aws s3 cp /tmp/test.txt s3://testbucket/ --endpoint-url http://localhost:8000 2>/dev/null && echo "✓ Upload file" || echo "✗ Upload file"

    # Download file
    aws s3 cp s3://testbucket/test.txt /tmp/test2.txt --endpoint-url http://localhost:8000 2>/dev/null && echo "✓ Download file" || echo "✗ Download file"

    # List objects
    aws s3 ls s3://testbucket/ --endpoint-url http://localhost:8000 2>/dev/null && echo "✓ List objects" || echo "✗ List objects"

    kill $PID 2>/dev/null
else
    echo "✗ Server failed to start"
    tail -10 /tmp/server.log
fi

echo ""
echo "3. DATABASE FEATURES"
echo "--------------------"
psql -d vaultaire -c "SELECT COUNT(*) FROM access_patterns;" 2>/dev/null && echo "✓ Access patterns table" || echo "✗ Access patterns table"
psql -d vaultaire -c "SELECT COUNT(*) FROM tenant_quotas;" 2>/dev/null && echo "✓ Quota system" || echo "✗ Quota system"
psql -d vaultaire -c "SELECT COUNT(*) FROM tenants;" 2>/dev/null && echo "✓ Tenant system" || echo "✗ Tenant system"

echo ""
echo "4. INTELLIGENCE FEATURES"
echo "------------------------"
grep -q "GetRecommendation" internal/intelligence/*.go 2>/dev/null && echo "✓ Backend selection logic" || echo "✗ Backend selection logic"
grep -q "AccessTracker" internal/intelligence/*.go 2>/dev/null && echo "✓ Access tracking" || echo "✗ Access tracking"

echo ""
echo "5. CACHE IMPLEMENTATION"
echo "-----------------------"
test -f internal/cache/lru.go && echo "✓ LRU cache" || echo "✗ LRU cache"
test -f internal/cache/ssd_cache.go && echo "✓ SSD cache layer" || echo "✗ SSD cache layer"
grep -q "TieredCache" internal/engine/*.go 2>/dev/null && echo "✓ Tiered cache wired" || echo "✗ Tiered cache wired"

echo ""
echo "6. MULTIPLE BACKENDS"
echo "--------------------"
ls internal/drivers/*.go | wc -l | xargs -I {} echo "{} driver files found"
grep -l "Put\|Get\|Delete" internal/drivers/*.go | wc -l | xargs -I {} echo "{} functional drivers"

echo ""
echo "7. MISSING CRITICAL MVP FEATURES"
echo "---------------------------------"
psql -d vaultaire -c "SELECT 1 FROM users LIMIT 1;" 2>/dev/null && echo "✓ User table" || echo "✗ User table (NEEDED FOR MVP!)"
test -s internal/auth/jwt.go && echo "✓ JWT implementation" || echo "✗ JWT implementation (NEEDED FOR MVP!)"
grep -q "stripe" go.mod 2>/dev/null && echo "✓ Stripe integration" || echo "✗ Stripe integration (NEEDED FOR MVP!)"
