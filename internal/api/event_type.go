package api

// Event represents an event in the system
type Event struct {
    Type      string                 `json:"type"`
    Container string                 `json:"container"`
    Artifact  string                 `json:"artifact"`
    Operation string                 `json:"operation"`
    TenantID  string                 `json:"tenant_id"`
    Data      map[string]interface{} `json:"data"`
}
