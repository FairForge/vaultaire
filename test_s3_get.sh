#!/bin/bash
echo "=== Vaultaire S3 GET Test Suite ==="

# Test 1: GET existing file
echo -n "Test 1 - GET hello.txt: "
RESULT=$(curl -s http://localhost:8080/test-bucket/hello.txt)
if [[ "$RESULT" == "Hello from Vaultaire!" ]]; then
    echo "✅ PASS"
else
    echo "❌ FAIL: Got '$RESULT'"
fi

# Test 2: GET JSON file
echo -n "Test 2 - GET data.json: "
RESULT=$(curl -s http://localhost:8080/test-bucket/data.json)
if [[ "$RESULT" == '{"message": "JSON test"}' ]]; then
    echo "✅ PASS"
else
    echo "❌ FAIL"
fi

# Test 3: Non-existent file
echo -n "Test 3 - Non-existent file: "
RESULT=$(curl -s http://localhost:8080/test-bucket/missing.txt)
if [[ "$RESULT" == *"NoSuchKey"* ]]; then
    echo "✅ PASS (returns NoSuchKey error)"
else
    echo "❌ FAIL"
fi

echo "=== Tests Complete ==="
