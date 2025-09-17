#!/bin/bash
# Basic S3 operations test

echo "Testing S3 Operations..."

# Test upload
echo "test data" > test.txt
aws s3 cp test.txt s3://testbucket/test.txt --endpoint-url http://localhost:8000

# Test download
aws s3 cp s3://testbucket/test.txt downloaded.txt --endpoint-url http://localhost:8000

# Verify content
if grep -q "test data" downloaded.txt; then
    echo "✅ Upload/Download working"
else
    echo "❌ Upload/Download failed"
fi

# Test listing
aws s3 ls s3://testbucket/ --endpoint-url http://localhost:8000

# Clean up
rm test.txt downloaded.txt
