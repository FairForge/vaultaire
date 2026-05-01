package api

import "net/http"

const llmsTxtContent = `# stored.ge API Reference
> S3-compatible object storage at $3.99/TB

## Authentication

### S3 API (SigV4)
All S3-compatible endpoints use AWS Signature Version 4 authentication.
Access key and secret key are provisioned when you create an account.
Compatible with any S3 SDK (aws-cli, boto3, aws-sdk-go, etc).

### Management API (Bearer JWT)
POST /auth/register — create account, returns JWT
POST /auth/login — authenticate, returns JWT
Include token as: Authorization: Bearer <token>

## S3-Compatible Endpoints

Base URL: https://stored.ge

PUT /<bucket> — create bucket
DELETE /<bucket> — delete empty bucket
GET / — list all buckets
PUT /<bucket>/<key> — upload object (streaming, multipart supported)
GET /<bucket>/<key> — download object
DELETE /<bucket>/<key> — delete object
HEAD /<bucket>/<key> — object metadata (served from cache, ~1ms)
GET /<bucket>?list-type=2 — list objects (V2, supports prefix/delimiter/continuation)
PUT /<bucket>/<key>?uploadId= — multipart upload
POST /<bucket>/<key>?uploads — initiate multipart
POST /<bucket>/<key>?uploadId= — complete multipart

### S3 Features
- Pre-signed URLs (GET and PUT, SigV4 query string auth)
- Object versioning (enable/suspend per bucket)
- Object lock (GOVERNANCE and COMPLIANCE retention modes, legal hold)
- Copy object (x-amz-copy-source header)
- Bucket notifications

## Management API (JSON)

Base URL: https://stored.ge/api/v1/manage
All endpoints require Bearer JWT authentication.

GET  /buckets — list buckets (cursor pagination: ?limit=20&starting_after=name)
POST /buckets — create bucket (body: {"name": "my-bucket"})
GET  /buckets/{name} — get bucket details
DELETE /buckets/{name} — delete bucket

GET /buckets/{name}/objects — list objects (?prefix=, ?limit=, ?starting_after=)

GET  /keys — list API keys
POST /keys — create API key (body: {"name": "my-key"})
DELETE /keys/{id} — revoke API key

GET /usage — current storage and bandwidth usage

### Response Format
Single object: {"object": "bucket", "name": "...", "request_id": "..."}
List: {"object": "list", "data": [...], "has_more": bool, "next_cursor": "...", "total_count": N}
Error: {"error": {"type": "invalid_request_error", "code": "...", "message": "...", "request_id": "..."}}

### Rate Limits
Management API: 100 requests/minute per tenant.
Headers: X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset.

## CDN

GET /cdn/{slug}/{bucket}/{key} — public object access (requires public-read bucket)

## Health & Status

GET /health — service health with backend status
GET /status — HTML status page
GET /metrics — Prometheus-compatible metrics
GET /version — build version

## Docs

GET /docs — interactive Swagger UI
GET /openapi.json — OpenAPI 3.0 specification
GET /llms.txt — this file
`

func (s *Server) handleLlmsTxt(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(llmsTxtContent))
}
