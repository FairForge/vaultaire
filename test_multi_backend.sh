#!/bin/bash

# Export mock credentials (won't work but will register the drivers)
export S3_ACCESS_KEY="test"
export S3_SECRET_KEY="test"
export STORAGE_MODE="local"  # Primary is local but others available

# Start server
./bin/vaultaire &
SERVER_PID=$!
sleep 2

# Check what drivers are registered
echo "=== REGISTERED DRIVERS ==="
curl -s http://localhost:8000/metrics | grep drivers || echo "Metrics endpoint not available"

# Test uploads
aws s3 cp test.txt s3://bucket/test.txt --endpoint-url http://localhost:8000

kill $SERVER_PID
