---
name: verify
description: Build, launch, and drive a local Vaultaire server end-to-end with aws-cli against the local PostgreSQL dev DB.
---

# Verify Vaultaire changes end-to-end

Build and launch (local storage backend, shared dev DB):

```bash
make build   # -> bin/vaultaire
PORT=8099 DB_HOST=localhost DB_PORT=5432 DB_NAME=vaultaire DB_USER=viera DB_PASSWORD= \
  DATA_PATH=<scratch>/data JWT_SECRET=verify-secret STORAGE_MODE=local \
  ./bin/vaultaire > <scratch>/server.log 2>&1 &
curl -s http://127.0.0.1:8099/health   # expect {"status":"healthy",...}
```

Get S3 credentials (registration persists users → tenants → api_keys → tenant_quotas):

```bash
curl -s -X POST http://127.0.0.1:8099/auth/register -H "Content-Type: application/json" \
  -d '{"email":"verify@test.local","password":"verify-pass-123","company":"Verify"}'
# returns accessKeyId (VK...) + secretAccessKey (SK...)
```

Drive with aws-cli (`--endpoint-url http://127.0.0.1:8099 --region us-east-1`):
`aws s3 mb/cp/rm`, `aws s3api copy-object`, multipart via
`aws configure set default.s3.multipart_threshold 5MB`.

Inspect state: `psql "postgres://viera@localhost:5432/vaultaire"` — key tables
`tenant_quotas` (storage_used_bytes), `object_head_cache`, `multipart_uploads`.
Force quota conditions by UPDATEing `storage_limit_bytes` directly.

Gotchas:
- The dev DB is shared — clean up your test tenant afterwards
  (quota_usage_events → tenant_quotas → object_head_cache → multipart_parts/uploads
  → buckets → api_keys → tenants → users, in that order for FKs).
- Tenant ID lookup: `SELECT id FROM tenants WHERE access_key='VK...'`.
- SigV4 enforcement is on; aws-cli signs correctly, raw curl S3 calls will 403.
