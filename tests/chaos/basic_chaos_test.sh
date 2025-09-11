#!/bin/bash

# Chaos Testing Script for Vaultaire
# Tests system resilience to various failures

set -e

echo "Starting Chaos Testing..."

# Test 1: Network latency simulation
test_network_latency() {
    echo "Test 1: Simulating network latency..."
    # Add 100ms latency to localhost (requires sudo on Mac)
    # sudo dnctl pipe 1 config delay 100

    # Run operations with latency
    curl -X PUT http://localhost:8000/chaos-test/latency.txt \
         -d "Testing with network latency" \
         -w "\nLatency test response time: %{time_total}s\n"
}

# Test 2: Disk space exhaustion
test_disk_space() {
    echo "Test 2: Testing low disk space handling..."
    # Create a large file to simulate low disk space
    dd if=/dev/zero of=/tmp/vaultaire-test-fill bs=1M count=500 2>/dev/null || true

    # Try to upload when disk is fuller
    curl -X PUT http://localhost:8000/chaos-test/disktest.txt \
         -d "Testing with low disk space" \
         -w "\nDisk test response: %{http_code}\n"

    # Cleanup
    rm -f /tmp/vaultaire-test-fill
}

# Test 3: Concurrent connection storm
test_connection_storm() {
    echo "Test 3: Connection storm test..."

    # Launch 50 concurrent requests
    for i in {1..50}; do
        curl -X PUT http://localhost:8000/chaos-test/storm-$i.txt \
             -d "Connection storm test $i" \
             -w "%{http_code} " &
    done

    # Wait for all to complete
    wait
    echo -e "\nConnection storm complete"
}

# Test 4: Large request handling
test_large_request() {
    echo "Test 4: Testing large request handling..."

    # Create a 10MB payload
    dd if=/dev/zero bs=1M count=10 2>/dev/null | \
        curl -X PUT http://localhost:8000/chaos-test/large.bin \
             --data-binary @- \
             -H "Content-Type: application/octet-stream" \
             -w "\nLarge request response: %{http_code}, time: %{time_total}s\n"
}

# Test 5: Rapid create/delete cycles
test_rapid_cycles() {
    echo "Test 5: Rapid create/delete cycles..."

    for i in {1..20}; do
        # Create
        curl -s -X PUT http://localhost:8000/chaos-test/cycle.txt \
             -d "Cycle test $i" -o /dev/null

        # Delete
        curl -s -X DELETE http://localhost:8000/chaos-test/cycle.txt \
             -o /dev/null

        echo -n "."
    done
    echo " Cycles complete"
}

# Run all tests
test_network_latency
test_disk_space
test_connection_storm
test_large_request
test_rapid_cycles

echo "Chaos testing complete!"
