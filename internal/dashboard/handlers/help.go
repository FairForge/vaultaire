package handlers

import (
	"html/template"
	"net/http"
)

type HelpHandler struct{}

func NewHelpHandler() *HelpHandler {
	return &HelpHandler{}
}

func (h *HelpHandler) ShowHelp(w http.ResponseWriter, r *http.Request) {
	tmpl := `<!DOCTYPE html>
<html>
<head>
    <title>Help & Documentation</title>
    <style>
        body { background: #000; color: #0f0; font-family: monospace; }
        .terminal { border: 1px solid #0f0; padding: 20px; margin: 20px; }
        .section { margin: 20px 0; }
        code { background: #111; padding: 2px 5px; }
    </style>
</head>
<body>
    <div class="terminal">
        <h1>Help & Documentation</h1>
        
        <div class="section">
            <h2>S3 API Endpoints</h2>
            <pre>
GET    /                    - List buckets
PUT    /{bucket}            - Create bucket
DELETE /{bucket}            - Delete bucket
GET    /{bucket}            - List objects
PUT    /{bucket}/{key}      - Upload object
GET    /{bucket}/{key}      - Download object
DELETE /{bucket}/{key}      - Delete object
            </pre>
        </div>
        
        <div class="section">
            <h2>Authentication</h2>
            <p>Use AWS Signature Version 4 with your API keys:</p>
            <code>aws s3 ls --endpoint-url http://localhost:8000</code>
        </div>
        
        <div class="section">
            <h2>Pricing</h2>
            <p>Storage: $3.99/TB per month</p>
            <p>Bandwidth: Included</p>
        </div>
        
        <div class="section">
            <h2>Support</h2>
            <p>Email: support@vaultaire.io</p>
            <p>Documentation: https://docs.vaultaire.io</p>
        </div>
    </div>
</body>
</html>`

	t, _ := template.New("help").Parse(tmpl)
	if err := t.Execute(w, nil); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}
