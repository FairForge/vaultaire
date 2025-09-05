package handlers

import (
	"html/template"
	"net/http"
)

type ActivityHandler struct {
	db interface{}
}

func NewActivityHandler(db interface{}) *ActivityHandler {
	return &ActivityHandler{db: db}
}

func (h *ActivityHandler) ShowActivityLog(w http.ResponseWriter, r *http.Request) {
	activityType := r.URL.Query().Get("type")
	filter := ""
	if activityType != "" {
		filter = " (Filtered: " + activityType + ")"
	}

	tmpl := `<!DOCTYPE html>
<html>
<head>
    <title>Activity Log</title>
    <style>
        body { background: #000; color: #0f0; font-family: monospace; }
        .terminal { border: 1px solid #0f0; padding: 20px; margin: 20px; }
        .log-entry { padding: 5px; margin: 5px 0; border-left: 2px solid #0f0; }
        .timestamp { color: #999; }
    </style>
</head>
<body>
    <div class="terminal">
        <h1>Activity Log` + filter + `</h1>
        <div class="log-entry">
            <span class="timestamp">[2025-09-05 14:23:45]</span> Upload: file.txt to test-bucket
        </div>
        <div class="log-entry">
            <span class="timestamp">[2025-09-05 14:22:10]</span> Delete: old-file.pdf from production
        </div>
        <div class="log-entry">
            <span class="timestamp">[2025-09-05 14:20:33]</span> Create Bucket: test-bucket
        </div>
        <div class="log-entry">
            <span class="timestamp">[2025-09-05 14:18:22]</span> API Key Generated: vlt_abc123
        </div>
        <div class="log-entry">
            <span class="timestamp">[2025-09-05 14:15:11]</span> Login: user@example.com
        </div>
    </div>
</body>
</html>`

	t, _ := template.New("activity").Parse(tmpl)
	if err := t.Execute(w, nil); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}
