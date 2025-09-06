package handlers

import (
	"encoding/json"
	"html/template"
	"net/http"
)

// UsageStats represents storage usage statistics
type UsageStats struct {
	StorageGB   float64 `json:"storage_gb"`
	BandwidthGB float64 `json:"bandwidth_gb"`
	Requests    int64   `json:"requests"`
	Cost        float64 `json:"cost"`
	Period      string  `json:"period"`
}
type UsageHandler struct {
	// Will add database connection later
	db interface{}
}

func NewUsageHandler(db interface{}) *UsageHandler {
	return &UsageHandler{db: db}
}

func (h *UsageHandler) GetUsage(w http.ResponseWriter, r *http.Request) {
	stats := UsageStats{
		StorageGB:   125.5,
		BandwidthGB: 450.2,
		Requests:    15234,
		Cost:        0.50, // $3.99 per TB, using 125.5GB
		Period:      "2025-09",
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(stats); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (h *UsageHandler) UsageDashboard(w http.ResponseWriter, r *http.Request) {
	tmpl := `<!DOCTYPE html>
<html>
<head>
    <title>Usage Statistics</title>
    <style>
        body { background: #000; color: #0f0; font-family: monospace; }
        .terminal { border: 1px solid #0f0; padding: 20px; margin: 20px; }
    </style>
</head>
<body>
    <div class="terminal">
        <h1>Usage Statistics</h1>
        <pre>
Storage:    125.5 GB
Bandwidth:  450.2 GB
Requests:   15,234
Cost:       $0.50
        </pre>
    </div>
</body>
</html>`

	t, _ := template.New("usage").Parse(tmpl)
	if err := t.Execute(w, nil); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

type BillingHandler struct {
	db interface{}
}

func NewBillingHandler(db interface{}) *BillingHandler {
	return &BillingHandler{db: db}
}

func (h *BillingHandler) ShowBilling(w http.ResponseWriter, r *http.Request) {
	tmpl := `<!DOCTYPE html>
<html>
<head>
    <title>Billing</title>
    <style>
        body { background: #000; color: #0f0; font-family: monospace; }
        .terminal { border: 1px solid #0f0; padding: 20px; margin: 20px; }
    </style>
</head>
<body>
    <div class="terminal">
        <h1>Billing Information</h1>
        <pre>
Plan:       Experimental Edge Storage
Price:      $3.99/TB
Current:    125.5 GB used
Charge:     $0.50
        </pre>
    </div>
</body>
</html>`

	t, _ := template.New("billing").Parse(tmpl)
	if err := t.Execute(w, nil); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}
