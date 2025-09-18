#!/bin/bash

echo "Testing intelligent backend selection..."

# Start server with multiple backends
export STORAGE_MODE=local
export S3_ACCESS_KEY=test
export S3_SECRET_KEY=test

./bin/vaultaire 2>&1 | tee server.log &
PID=$!
sleep 3

# Upload different types of files to trigger intelligence
echo "Small file" > small.txt
dd if=/dev/zero of=large.bin bs=1M count=150 2>/dev/null

# Upload files
aws s3 cp small.txt s3://test/small.txt --endpoint-url http://localhost:8000
aws s3 cp large.bin s3://test/large.bin --endpoint-url http://localhost:8000

# Access small file multiple times to make it "hot"
for i in {1..12}; do
    aws s3 cp s3://test/small.txt - --endpoint-url http://localhost:8000 >/dev/null 2>&1
    echo "Access $i complete"
done

# Check backend selection logs
echo ""
echo "=== Backend Selection Logs ==="
grep "backend selection" server.log || echo "No backend selection logs found"

# Check database for patterns
echo ""
echo "=== Database Patterns ==="
psql -d vaultaire -c "
SELECT
    artifact_key,
    backend_used,
    access_count,
    temperature,
    size_bytes
FROM access_patterns
WHERE container LIKE '%test%'
ORDER BY access_count DESC
LIMIT 10;"

# Clean up
kill $PID
rm small.txt large.bin server.log
