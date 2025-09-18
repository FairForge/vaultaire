#!/bin/bash
echo "Testing backend selection wiring..."

# Start with multiple backends
export STORAGE_MODE=local
export S3_ACCESS_KEY=mock
export S3_SECRET_KEY=mock

./bin/vaultaire &
PID=$!
sleep 3

# Upload and access to trigger intelligence
aws s3 cp /etc/hosts s3://test/hosts.txt --endpoint-url http://localhost:8000

# Check logs for "backend selection" messages
echo "Check server logs for 'backend selection' entries showing intelligence decisions"

# Query database to see if patterns are recorded
psql -d vaultaire -c "SELECT artifact_key, backend_used, access_count, temperature FROM access_patterns WHERE container LIKE '%test%' ORDER BY last_seen DESC LIMIT 5;"

kill $PID
