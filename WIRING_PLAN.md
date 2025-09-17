# Wiring Existing Features

## Current State
- ✅ Intelligence tracking records patterns
- ✅ Multiple backends registered (local, s3, lyve)
- ✅ Cache structures initialized
- ❌ But nothing actually uses these features!

## Priority 1: Backend Selection (Day 1)
- [ ] Add backend selection logic in engine.selectBackend()
- [ ] Use intelligence recommendations for backend choice
- [ ] Test S3 backend actually works
- [ ] Test Lyve backend connectivity

## Priority 2: Cache Integration (Day 2)
- [ ] Wire cache.Get() before backend.Get()
- [ ] Wire cache.Set() after successful gets
- [ ] Use intelligence hot data for cache warming
- [ ] Verify cache hits actually happen

## Priority 3: Compression (Day 3)
- [ ] Add compression decision logic
- [ ] Wire compression wrapper in Put flow
- [ ] Wire decompression in Get flow
- [ ] Test file sizes actually reduce

## Priority 4: Rate Limiting (Day 4)
- [ ] Wire rate limiter into middleware stack
- [ ] Return 429 when limits exceeded
- [ ] Test concurrent requests get limited
- [ ] Add rate limit headers to responses

## Testing Plan
After each wiring:
1. Unit test the specific feature
2. Integration test the full flow
3. Verify with real S3 commands
4. Check metrics/logs confirm it's working
