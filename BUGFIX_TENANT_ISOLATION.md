# Critical Fix: Multipart Upload Tenant Isolation

## Problem
Multipart uploads were storing files under wrong tenant namespace:
- Expected: `t-user-001/user-001_my-bucket/file.bin`
- Actual: `t-default/user-001_my-bucket/file.bin`

## Root Cause
Context keys were defined separately in each package:
- `api` package had its own `contextKey` type
- `drivers` package had its own `contextKey` type
- Even with same string value, Go treats them as different types

## Solution
1. Created shared `internal/common/context.go`:
   - Single `ContextKey` type
   - Exported `TenantIDKey` constant
2. Updated all packages to import and use `common.TenantIDKey`
3. Ensured context propagation through entire call chain

## Verification
Successfully uploaded file via multipart to correct location on Lyve Cloud.

## Lessons Learned
- Go context keys must be the exact same type, not just same value
- Shared constants belong in a common package
- Always verify multi-backend systems with actual backends, not just local

## Future Impact
All new backends (OneDrive, Quotaless, etc.) must:
1. Import `internal/common`
2. Use `common.TenantIDKey` for tenant context
3. Build paths with `t-{tenantID}/` prefix
