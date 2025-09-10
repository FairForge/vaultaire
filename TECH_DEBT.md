# Technical Debt Register

## HIGH PRIORITY
1. **Database Connection Missing**
   - Location: internal/api/server.go:35
   - Impact: Auth system bypassed
   - Fix: Connect PostgreSQL, run migrations

## MEDIUM PRIORITY
2. **Auth Validation Bypassed**
   - Location: internal/api/auth.go:45
   - Reason: No database for API key lookup
   - Fix: After database connected, uncomment original code

## LOW PRIORITY
3. **HEAD Operation Inefficient**
   - Location: internal/api/s3.go:360
   - Issue: Reads entire stream to get size
   - Fix: Store metadata separately

## LINTING ISSUES (to fix)
- Unused functions in auth.go (needed later)
- Unchecked error on reader.Close()
- fmt.Sprintf can be simplified
