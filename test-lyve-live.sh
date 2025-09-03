#!/bin/bash

# Export your Lyve credentials
export STORAGE_MODE=lyve
export LYVE_ACCESS_KEY="your_actual_key"
export LYVE_SECRET_KEY="your_actual_secret"
export LYVE_REGION="us-east-1"

# Start the server
echo "Starting Vaultaire with Lyve backend..."
go run cmd/vaultaire/main.go &
SERVER_PID=$!

# Wait for server to start
sleep 3

# Test with curl (S3 PUT)
echo "Testing PUT operation..."
curl -X PUT \
  -H "Content-Type: text/plain" \
  -d "Hello Lyve Cloud from Vaultaire!" \
  http://localhost:8000/test-bucket/test.txt

# Test GET
echo "Testing GET operation..."
curl http://localhost:8000/test-bucket/test.txt

# Kill server
kill $SERVER_PID
