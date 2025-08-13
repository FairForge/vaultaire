#!/bin/bash
echo "=== Vaultaire S3 PUT Test Suite ==="

# Test 1: Upload a text file
echo -n "Test 1 - PUT text file: "
HTTP_CODE=$(curl -X PUT http://localhost:8080/test-bucket/uploaded.txt \
  -d "This file was uploaded through S3 API" \
  -s -o /dev/null -w "%{http_code}")
if [[ "$HTTP_CODE" == "200" ]]; then
    echo "✅ PASS (HTTP $HTTP_CODE)"
else
    echo "❌ FAIL (HTTP $HTTP_CODE)"
fi

# Test 2: Verify the upload by getting it back
echo -n "Test 2 - GET uploaded file: "
RESULT=$(curl -s http://localhost:8080/test-bucket/uploaded.txt)
if [[ "$RESULT" == "This file was uploaded through S3 API" ]]; then
    echo "✅ PASS"
else
    echo "❌ FAIL: Got '$RESULT'"
fi

# Test 3: Upload JSON
echo -n "Test 3 - PUT JSON file: "
HTTP_CODE=$(curl -X PUT http://localhost:8080/test-bucket/test.json \
  -H "Content-Type: application/json" \
  -d '{"test": "data", "number": 42}' \
  -s -o /dev/null -w "%{http_code}")
if [[ "$HTTP_CODE" == "200" ]]; then
    echo "✅ PASS (HTTP $HTTP_CODE)"
else
    echo "❌ FAIL (HTTP $HTTP_CODE)"
fi

# Test 4: Verify JSON upload
echo -n "Test 4 - GET JSON file: "
RESULT=$(curl -s http://localhost:8080/test-bucket/test.json)
if [[ "$RESULT" == '{"test": "data", "number": 42}' ]]; then
    echo "✅ PASS"
else
    echo "❌ FAIL: Got '$RESULT'"
fi

# Test 5: Check files exist on disk
echo -n "Test 5 - Files exist on disk: "
if [[ -f "/tmp/vaultaire/test-bucket/uploaded.txt" ]] && [[ -f "/tmp/vaultaire/test-bucket/test.json" ]]; then
    echo "✅ PASS"
    ls -la /tmp/vaultaire/test-bucket/*.txt /tmp/vaultaire/test-bucket/*.json
else
    echo "❌ FAIL"
fi

echo "=== Tests Complete ==="
