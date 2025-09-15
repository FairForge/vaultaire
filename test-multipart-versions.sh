#!/bin/bash

# Configure AWS CLI
aws configure set aws_access_key_id AKIAIOSFODNN7EXAMPLE
aws configure set aws_secret_access_key wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY

# Clean start
pkill -f vaultaire 2>/dev/null

echo "Starting Original Version..."
STORAGE_MODE=lyve \
LYVE_ACCESS_KEY="STX1AAB7VF6NEUX3XLODKEP4TROF" \
LYVE_SECRET_KEY="+NS1M9KsonZzrDO3tMVqQx7sJpuAlHYMWBZEtQo4XvS" \
LYVE_REGION="us-east-1" \
./bin/vaultaire > original.log 2>&1 &

sleep 3
echo "=== Original Version ==="
time aws --endpoint-url http://localhost:8000 s3 cp large-file.bin s3://my-bucket/test-original.bin

pkill -f vaultaire
sleep 2

echo "Starting Optimized Version..."
OPTIMIZED_MULTIPART=true \
STORAGE_MODE=lyve \
LYVE_ACCESS_KEY="STX1AAB7VF6NEUX3XLODKEP4TROF" \
LYVE_SECRET_KEY="+NS1M9KsonZzrDO3tMVqQx7sJpuAlHYMWBZEtQo4XvS" \
LYVE_REGION="us-east-1" \
./bin/vaultaire > optimized.log 2>&1 &

sleep 3
echo "=== Optimized Version ==="
time aws --endpoint-url http://localhost:8000 s3 cp large-file.bin s3://my-bucket/test-optimized.bin

pkill -f vaultaire

echo "Check logs for timing details:"
echo "grep 'assembly' original.log"
echo "grep 'assembly' optimized.log"
