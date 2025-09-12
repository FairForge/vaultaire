#!/bin/bash
echo "Testing Vaultaire Auth..."

# Register
RESPONSE=$(curl -s -X POST http://localhost:8000/auth/register)
echo "Registration response: $RESPONSE"

# Extract credentials (if response is valid JSON)
if [[ $RESPONSE == *"accessKeyId"* ]]; then
    ACCESS_KEY=$(echo $RESPONSE | sed 's/.*"accessKeyId":"\([^"]*\)".*/\1/')
    SECRET_KEY=$(echo $RESPONSE | sed 's/.*"secretAccessKey":"\([^"]*\)".*/\1/')
    
    echo "Got credentials:"
    echo "  Access: $ACCESS_KEY"
    echo "  Secret: ${SECRET_KEY:0:20}..."
else
    echo "Registration might have failed or returned unexpected format"
fi
