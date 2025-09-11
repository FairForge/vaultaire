#!/bin/bash

echo "Testing process crash recovery..."

# Function to start server
start_server() {
    echo "Starting server..."
    cd ../..
    go run cmd/vaultaire/main.go > /tmp/vaultaire.log 2>&1 &
    SERVER_PID=$!
    sleep 3
    echo "Server started with PID: $SERVER_PID"
}

# Function to kill server
kill_server() {
    echo "Killing server (PID: $SERVER_PID)..."
    kill -9 $SERVER_PID 2>/dev/null || true
    sleep 1
}

# Start server
start_server

# Upload some data
echo "Uploading data before crash..."
for i in {1..5}; do
    curl -X PUT http://localhost:8000/crash-test/file-$i.txt \
         -d "Data before crash $i" \
         -s -o /dev/null
done

# Kill server abruptly (simulate crash)
kill_server

# Restart server
start_server

# Try to read the data
echo "Reading data after recovery..."
SUCCESS=0
for i in {1..5}; do
    RESULT=$(curl -s http://localhost:8000/crash-test/file-$i.txt)
    if [[ "$RESULT" == "Data before crash $i" ]]; then
        ((SUCCESS++))
    fi
done

echo "Recovery test: $SUCCESS/5 files recovered"

# Cleanup
kill_server
echo "Crash recovery test complete"
