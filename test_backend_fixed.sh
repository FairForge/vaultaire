#!/bin/bash
set -e

echo "Starting Vaultaire with multiple backends..."
export STORAGE_MODE=local
export S3_ACCESS_KEY=test
export S3_SECRET_KEY=test

# Start server and capture output
./bin/vaultaire > server.log 2>&1 &
SERVER_PID=$!

# Wait for server to start
sleep 3

# Check if server is running
if ! ps -p $SERVER_PID > /dev/null; then
    echo "Server failed to start. Last 10 lines of log:"
    tail -10 server.log
    exit 1
fi

echo "Server started with PID $SERVER_PID"

# Test upload
echo "test data" > testfile.txt
aws s3 cp testfile.txt s3://test/testfile.txt --endpoint-url http://localhost:8000

# Check logs
echo "=== Backend Selection Logs ==="
grep "backend selection" server.log | tail -5 || echo "No selection logs found"

echo "=== Registered Drivers ==="
grep "driver added" server.log

# Cleanup
kill $SERVER_PID
rm -f testfile.txt server.log

echo "Test complete!"
