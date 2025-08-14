# Vaultaire Context Document
Last Updated: 2025-08-14 10:30
Current Step: 44 of 500

## 🎯 Critical Architecture Decisions (NEVER VIOLATE)
- [ ] Engine pattern (NOT storage)
- [ ] Container/Artifact (NOT bucket/object)  
- [ ] Driver interface (NOT backend)
- [ ] Event logging on EVERY operation
- [ ] Stream everything (io.Reader, never []byte)
- [ ] Context on ALL functions

## 📊 Current Implementation Status

### ✅ Completed (Working)
- S3 GET: internal/api/s3_handler.go (working)
- S3 PUT: internal/api/s3_handler.go (working)
- Local Driver: internal/drivers/local.go
- Event Logging: internal/events/logger.go

### 🔄 In Progress
- S3 DELETE: Need to implement handleDelete()
- S3 LIST: Need XML response format

### ⚠️ Critical TODOs Before Step 100
- [ ] Multi-tenancy: Add TenantID to all requests
- [ ] Metrics: Add prometheus collectors
- [ ] Config: Make backends map[string]interface{}
- [ ] Streaming: Verify no []byte returns
- [ ] Context: Add context.Context to all functions

## 🔥 Code Snippets to Preserve

### Multi-Tenancy Pattern (MUST ADD)
```go
// internal/middleware/tenant.go
func ExtractTenant(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        tenantID := r.Header.Get("X-Tenant-ID")
        if tenantID == "" {
            tenantID = "default"  // For MVP
        }
        ctx := context.WithValue(r.Context(), "tenant", tenantID)
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}
Metrics Pattern (MUST ADD)
go// internal/metrics/collector.go
var (
    requestCount = promauto.NewCounterVec(
        prometheus.CounterOpts{
            Name: "vaultaire_requests_total",
        },
        []string{"method", "tenant", "status"},
    )
)

func RecordRequest(method, tenant, status string) {
    requestCount.WithLabelValues(method, tenant, status).Inc()
}
Event Logging Pattern (MUST USE)
go// Every operation must log
h.events.Log(EventType{
    Operation: "DELETE",
    Container: req.Container,
    Key:       req.Key,
    Tenant:    tenantID,
    Timestamp: time.Now(),
})
