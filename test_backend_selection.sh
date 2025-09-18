#!/bin/bash

# Start server if not running
./bin/vaultaire &
SERVER_PID=$!
sleep 2

# Upload files of different sizes
echo "Small file" > small.txt
dd if=/dev/zero of=large.bin bs=1M count=150 2>/dev/null
echo "Archive me" > cold.txt

# Upload and access to create patterns
aws s3 cp small.txt s3://test/small.txt --endpoint-url http://localhost:8000

# Access multiple times to make it "hot"
for i in {1..15}; do
    aws s3 cp s3://test/small.txt - --endpoint-url http://localhost:8000 >/dev/null 2>&1
done

# Upload large file (should go to S3 if wired)
aws s3 cp large.bin s3://test/large.bin --endpoint-url http://localhost:8000

# Upload cold file (access rarely)
aws s3 cp cold.txt s3://test/cold.txt --endpoint-url http://localhost:8000

# Check what backends were used
psql -d vaultaire -c "
SELECT artifact_key, backend_used, access_count, temperature
FROM access_patterns
WHERE container LIKE '%test%'
ORDER BY access_count DESC;
"

# Cleanup
kill $SERVER_PID
rm small.txt large.bin cold.txt
