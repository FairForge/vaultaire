# Vaultaire Implementation Checklist

## 🔴 EVERY Function MUST Have:
```go
func (h *Handler) Operation(ctx context.Context, ...) (io.Reader, error) {
    // 1. Extract tenant
    tenant := ctx.Value("tenant").(string)

    // 2. Build namespaced key
    key := fmt.Sprintf("tenant/%s/%s", tenant, originalKey)

    // 3. Log event (ML training)
    h.events.Log("operation", key, size)

    // 4. Record metrics
    defer metrics.Record("operation", time.Now())

    // 5. Stream response (never []byte)
    return reader, nil
}
🔴 EVERY Request MUST:

 Pass through tenant middleware
 Use context.Context
 Log to event stream
 Record prometheus metrics
 Return io.Reader (not []byte)
 Handle errors with context

🔴 EVERY Commit MUST:

 Update PROGRESS.md
 Update CONTEXT.md
 Include step number
 Include test verification
 Preserve critical patterns
