#!/bin/bash
export STORAGE_MODE=lyve
export LYVE_ACCESS_KEY="STX1AAB7VF6NEUX3XLODKEP4TROF"
export LYVE_SECRET_KEY="+NS1M9KsonZzrDO3tMVqQx7sJpuAlHYMWBZEtQo4XvS"
export LYVE_REGION="us-east-1"

./bin/vaultaire &
SERVER_PID=$!
sleep 2

echo "Testing with 64MB parts..."
time aws --endpoint-url http://localhost:8000 s3 cp large-file.bin s3://my-bucket/test-64mb-parts.bin

kill $SERVER_PID
