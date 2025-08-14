# API Reference

## S3-Compatible API

Vaultaire implements a subset of the S3 API for maximum compatibility with existing tools and SDKs.

### Supported Operations

#### Object Operations
- ✅ `GetObject` - Retrieve an object
- ✅ `PutObject` - Store an object
- 🔄 `DeleteObject` - Remove an object (coming soon)
- 🔄 `HeadObject` - Get object metadata (coming soon)
- 🔄 `CopyObject` - Copy an object (planned)

#### Bucket Operations
- 🔄 `ListObjects` - List objects in a bucket (coming soon)
- 🔄 `CreateBucket` - Create a new bucket (planned)
- 🔄 `DeleteBucket` - Remove a bucket (planned)
- 🔄 `HeadBucket` - Check bucket exists (planned)

### Authentication

Vaultaire supports AWS Signature Version 4 for authentication.

```bash
# Example with AWS CLI
aws s3 cp file.txt s3://my-bucket/file.txt \
  --endpoint-url https://api.stored.ge
Request Format
GetObject
httpGET /{bucket}/{key} HTTP/1.1
Host: api.stored.ge
Authorization: AWS4-HMAC-SHA256 ...
PutObject
httpPUT /{bucket}/{key} HTTP/1.1
Host: api.stored.ge
Content-Type: application/octet-stream
Content-Length: 1024
Authorization: AWS4-HMAC-SHA256 ...

[binary data]
Response Format
Success Response
httpHTTP/1.1 200 OK
Content-Type: application/octet-stream
Content-Length: 1024
ETag: "d41d8cd98f00b204e9800998ecf8427e"
x-amz-request-id: 1234567890

[binary data]
Error Response
xml<?xml version="1.0" encoding="UTF-8"?>
<Error>
    <Code>NoSuchKey</Code>
    <Message>The specified key does not exist.</Message>
    <Key>my-object</Key>
    <RequestId>1234567890</RequestId>
</Error>
Error Codes
CodeDescriptionHTTP StatusNoSuchBucketBucket doesn't exist404NoSuchKeyObject doesn't exist404AccessDeniedNo permission403InvalidRequestMalformed request400InternalErrorServer error500
Rate Limits

stored.ge: 1000 requests/minute
stored.cloud: 10000 requests/minute
Vaultaire Core: Unlimited (self-hosted)

SDKs
Any S3-compatible SDK works with Vaultaire:
Python (boto3)
pythonimport boto3

s3 = boto3.client('s3',
    endpoint_url='https://api.stored.ge',
    aws_access_key_id='your-key',
    aws_secret_access_key='your-secret'
)

s3.put_object(Bucket='my-bucket', Key='file.txt', Body=data)
JavaScript (AWS SDK)
javascriptconst { S3Client, PutObjectCommand } = require("@aws-sdk/client-s3");

const s3 = new S3Client({
    endpoint: "https://api.stored.ge",
    credentials: {
        accessKeyId: "your-key",
        secretAccessKey: "your-secret"
    }
});

await s3.send(new PutObjectCommand({
    Bucket: "my-bucket",
    Key: "file.txt",
    Body: data
}));
Go (AWS SDK)
goimport (
    "github.com/aws/aws-sdk-go/aws"
    "github.com/aws/aws-sdk-go/aws/session"
    "github.com/aws/aws-sdk-go/service/s3"
)

sess := session.Must(session.NewSession(&aws.Config{
    Endpoint: aws.String("https://api.stored.ge"),
}))

svc := s3.New(sess)
svc.PutObject(&s3.PutObjectInput{
    Bucket: aws.String("my-bucket"),
    Key:    aws.String("file.txt"),
    Body:   bytes.NewReader(data),
})
