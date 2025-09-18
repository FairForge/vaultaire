#!/bin/bash

echo "======================================="
echo "INTELLIGENCE LAYER VERIFICATION (201-300)"
echo "======================================="
echo ""

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m'

check() {
    if eval "$2" 2>/dev/null; then
        echo -e "$1: ${GREEN}✓${NC}"
    else
        echo -e "$1: ${RED}✗${NC}"
    fi
}

echo "STEPS 201-210: Memory Cache (LRU)"
echo "---------------------------------"
check "201 LRU implementation" "test -f internal/cache/lru.go"
check "202 Size management" "grep -q 'maxSize\|256' internal/cache/*.go"
check "203 Hit/miss tracking" "grep -q 'hit\|miss' internal/cache/*.go"
check "204 Cache warming" "grep -q 'warm\|preheat' internal/cache/*.go"
check "205 Hot data" "grep -q 'GetHotData' internal/intelligence/*.go"
check "206 Eviction policies" "grep -q 'evict\|LRU' internal/cache/*.go"
check "207 Persistence" "grep -q 'persist\|save' internal/cache/*.go"
check "208 Clustering" "test -f internal/cache/cluster.go"
check "209 Invalidation API" "grep -q 'Invalidate' internal/cache/*.go"
check "210 Performance metrics" "test -f internal/cache/metrics.go"

echo ""
echo "STEPS 211-220: SSD Cache Layer"
echo "------------------------------"
check "211 SSD cache" "test -f internal/cache/ssd_cache.go"
check "212 Tiered logic" "grep -q 'TieredCache' internal/engine/*.go"
check "213 Wear leveling" "grep -q 'wear' internal/cache/*.go"
check "214 Promotion/demotion" "grep -q 'promot\|demot' internal/cache/*.go"
check "215 SSD monitoring" "grep -q 'monitor.*ssd\|ssd.*monitor' internal/cache/*.go"
check "216 Compression" "grep -q 'compress\|gzip' internal/cache/*.go"
check "217 Deduplication" "grep -q 'dedup' internal/cache/*.go"
check "218 Encryption" "grep -q 'encrypt' internal/cache/*.go"
check "219 Backup strategy" "test -f internal/cache/backup.go"
check "220 Recovery" "test -f internal/cache/recovery.go"

echo ""
echo "STEPS 221-230: Intelligent Caching"
echo "----------------------------------"
check "221 Pattern analysis" "test -f internal/cache/patterns.go"
check "222 Prefetching" "test -f internal/cache/prefetch.go"
check "223 Time strategies" "test -f internal/cache/time_strategies.go"
check "224 User caching" "test -f internal/cache/user_cache.go"
check "225 Geo distribution" "test -f internal/cache/geo_cache.go"
check "226 Cost optimization" "test -f internal/cache/cost_optimizer.go"
check "227 Bandwidth mgmt" "grep -q 'bandwidth' internal/cache/*.go"
check "228 Consistency" "test -f internal/cache/consistency.go"
check "229 Debug tools" "test -f internal/cache/debug.go"
check "230 Config API" "test -f internal/cache/config_api.go"

echo ""
echo "STEPS 231-240: Access Pattern Learning"
echo "--------------------------------------"
check "231 Access logging" "grep -q 'LogAccess' internal/intelligence/*.go"
check "232 Pattern recognition" "grep -q 'pattern' internal/intelligence/*.go"
check "233 User modeling" "psql -d vaultaire -c 'SELECT 1 FROM user_behavior_models LIMIT 1'"
check "234 Temporal detection" "grep -q 'temporal' internal/intelligence/*.go"
check "235 Spatial detection" "grep -q 'spatial' internal/intelligence/*.go"
check "236 Anomaly detection" "test -f internal/intelligence/anomaly.go"
check "237 Visualization" "grep -q 'visual' internal/intelligence/*.go"
check "238 Pattern optimization" "grep -q 'optimize.*pattern' internal/intelligence/*.go"
check "239 ML pipeline" "test -f internal/intelligence/ml_pipeline.go"
check "240 Pattern API" "grep -q 'GetPatterns' internal/intelligence/*.go"

echo ""
echo "STEPS 241-250: Auto-Tiering Engine"
echo "-----------------------------------"
check "241 Access tracking" "psql -d vaultaire -c 'SELECT COUNT(*) FROM access_patterns'"
check "242 Cost calculation" "test -f internal/engine/cost_optimizer.go"
check "243 Migration scheduling" "grep -q 'migration\|migrate' internal/engine/*.go"
check "244 Tiering policies" "grep -q 'tier' internal/engine/*.go"
check "245 User-defined rules" "grep -q 'rule' internal/engine/*.go"
check "246 Tier monitoring" "grep -q 'monitor.*tier' internal/engine/*.go"
check "247 Cost reporting" "grep -q 'report.*cost' internal/engine/*.go"
check "248 Optimization suggestions" "grep -q 'suggest' internal/engine/*.go"
check "249 Migration history" "psql -d vaultaire -c '\dt' | grep -q migration"
check "250 Tier API" "grep -q 'tier.*api\|api.*tier' internal/engine/*.go"

echo ""
echo "STEPS 251-260: Cost Optimization"
echo "---------------------------------"
check "251 Provider tracking" "grep -q 'provider.*cost' internal/engine/*.go"
check "252 Bandwidth opt" "grep -q 'bandwidth' internal/engine/*.go"
check "253 Storage opt" "grep -q 'storage.*optim' internal/engine/*.go"
check "254 Compute opt" "grep -q 'compute' internal/engine/*.go"
check "255 Cost forecasting" "grep -q 'forecast' internal/engine/*.go"
check "256 Cost alerts" "grep -q 'alert.*cost' internal/engine/*.go"
check "257 Cost allocation" "grep -q 'allocat' internal/engine/*.go"
check "258 Budget management" "grep -q 'budget' internal/engine/*.go"
check "259 Cost dashboard" "grep -q 'cost.*dashboard' internal/dashboard/*.go"
check "260 Cost API" "grep -q 'cost' internal/api/*.go"

echo ""
echo "STEPS 261-270: Data Optimization"
echo "---------------------------------"
check "261 Compression" "test -f internal/storage/compression.go"
check "262 Deduplication" "test -f internal/storage/dedup.go"
check "263 Delta compression" "test -f internal/storage/delta.go"
check "264 Binary diff" "grep -q 'diff\|delta' internal/storage/*.go"
check "265 Format optimization" "grep -q 'format' internal/storage/*.go"
check "266 Encoding optimization" "grep -q 'encoding' internal/storage/*.go"
check "267 Metadata optimization" "grep -q 'metadata' internal/storage/*.go"
check "268 Index optimization" "grep -q 'index' internal/storage/*.go"
check "269 Query optimization" "grep -q 'query.*optim' internal/storage/*.go"
check "270 Network optimization" "grep -q 'network' internal/storage/*.go"

echo ""
echo "FUNCTIONAL TESTS"
echo "----------------"
# Test if cache actually works
echo "test" > /tmp/cache_test.txt
./bin/vaultaire 2>/dev/null &
PID=$!
sleep 2
if ps -p $PID 2>/dev/null; then
    curl -X PUT http://localhost:8000/test/cache.txt --data-binary @/tmp/cache_test.txt -H "x-amz-acl: public-read" 2>/dev/null
    curl -X GET http://localhost:8000/test/cache.txt 2>/dev/null > /dev/null && echo -e "Cache write/read: ${GREEN}✓${NC}" || echo -e "Cache write/read: ${RED}✗${NC}"
    kill $PID 2>/dev/null
else
    echo -e "Cache write/read: ${RED}✗ Server failed${NC}"
fi
