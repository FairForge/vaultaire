#!/bin/bash
# Kill any existing servers
pkill vaultaire 2>/dev/null
sleep 1

echo "Starting server with multiple backends..."
export STORAGE_MODE=local
export S3_ACCESS_KEY=test
export S3_SECRET_KEY=test

./bin/vaultaire 2>&1 | tee server.log &
PID=$!
sleep 3

# Test write then read
echo "data" > test.txt
aws s3 cp test.txt s3://bucket/file.txt --endpoint-url http://localhost:8000
aws s3 cp s3://bucket/file.txt - --endpoint-url http://localhost:8000

# Look for selection
echo "=== Backend Selection ==="
grep "backend selection" server.log || echo "No selection yet"

# Clean up
kill $PID
rm test.txt server.log
