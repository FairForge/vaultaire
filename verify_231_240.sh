#!/bin/bash

echo "=== Verifying Steps 231-240 Implementation ==="

# Database tables
echo "Checking database tables..."
for table in access_patterns pattern_aggregations user_behavior_models access_anomalies; do
    psql -d vaultaire -c "\d $table" > /dev/null 2>&1 && \
        echo "✅ $table exists" || \
        echo "❌ $table missing"
done

# Code integration
echo "Checking code integration..."
grep -q "intelligence.AccessTracker" internal/engine/engine.go && \
    echo "✅ Intelligence integrated in engine" || \
    echo "❌ Intelligence not integrated"

# API endpoints
echo "Testing API endpoints..."
curl -f http://localhost:8000/api/patterns > /dev/null 2>&1 && \
    echo "✅ Pattern API working" || \
    echo "❌ Pattern API not responding"

# Real data flow
echo "Checking data flow..."
COUNT=$(psql -d vaultaire -t -c "SELECT COUNT(*) FROM access_patterns")
if [ $COUNT -gt 0 ]; then
    echo "✅ Access patterns being recorded ($COUNT entries)"
else
    echo "❌ No access patterns recorded"
fi

# ML features
echo "Checking ML features..."
ML_COUNT=$(psql -d vaultaire -t -c "SELECT COUNT(*) FROM access_patterns WHERE hour_of_day IS NOT NULL")
if [ $ML_COUNT -gt 0 ]; then
    echo "✅ ML features being extracted"
else
    echo "❌ ML features not working"
fi

echo "=== Verification complete ==="
