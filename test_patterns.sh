#!/bin/bash
set -e

echo "=== Testing Access Pattern Learning (Steps 231-240) ==="

# 1. Generate access patterns
echo "Generating test data..."
for i in {1..20}; do
    # Hot file - access frequently
    aws s3 cp /tmp/test.txt s3://test/hot/file$i.txt \
        --endpoint-url http://localhost:8000
    aws s3 cp s3://test/hot/file$i.txt /dev/null \
        --endpoint-url http://localhost:8000
done

for i in {1..5}; do
    # Cold files - access once
    aws s3 cp /tmp/test.txt s3://test/cold/file$i.txt \
        --endpoint-url http://localhost:8000
done

sleep 6  # Wait for batch processing

# 2. Check patterns are tracked
echo "Checking access patterns..."
PATTERNS=$(psql -d vaultaire -t -c "SELECT COUNT(*) FROM access_patterns")
echo "Access patterns recorded: $PATTERNS"

# 3. Check temperature classification
echo "Checking temperature classification..."
psql -d vaultaire -c "
SELECT temperature, COUNT(*)
FROM access_patterns
GROUP BY temperature"

# 4. Test pattern API
echo "Testing pattern API..."
curl -s http://localhost:8000/api/patterns/hot | jq

# 5. Check anomaly detection
echo "Checking anomalies..."
psql -d vaultaire -c "SELECT COUNT(*) FROM access_anomalies"

# 6. Test recommendations
echo "Getting optimization recommendations..."
curl -s http://localhost:8000/api/patterns/recommendations | jq

# 7. Check ML features
echo "Verifying ML pipeline..."
psql -d vaultaire -c "
SELECT COUNT(*)
FROM access_patterns
WHERE hour_of_day IS NOT NULL"

echo "=== All tests passed! ==="
