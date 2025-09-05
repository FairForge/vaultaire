package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"html/template"
	"net/http"
)

type APIKeysHandler struct {
	db interface{}
}

func NewAPIKeysHandler(db interface{}) *APIKeysHandler {
	return &APIKeysHandler{db: db}
}

func (h *APIKeysHandler) ListKeys(w http.ResponseWriter, r *http.Request) {
	tmpl := `<!DOCTYPE html>
<html>
<head>
    <title>API Keys</title>
    <style>
        body { background: #000; color: #0f0; font-family: monospace; }
        .terminal { border: 1px solid #0f0; padding: 20px; margin: 20px; }
        .key { background: #111; padding: 10px; margin: 10px 0; }
    </style>
</head>
<body>
    <div class="terminal">
        <h1>API Keys</h1>
        <div class="key">
            vlt_abc123def456 [Active]
        </div>
        <div class="key">
            vlt_xyz789ghi012 [Active]
        </div>
        <button onclick="location.href='/dashboard/apikeys/generate'">Generate New Key</button>
    </div>
</body>
</html>`

	t, _ := template.New("apikeys").Parse(tmpl)
	if err := t.Execute(w, nil); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (h *APIKeysHandler) GenerateKey(w http.ResponseWriter, r *http.Request) {
	// Generate a random key with vlt_ prefix
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	key := "vlt_" + hex.EncodeToString(b)

	tmpl := `<!DOCTYPE html>
<html>
<head>
    <title>New API Key</title>
    <style>
        body { background: #000; color: #0f0; font-family: monospace; }
        .terminal { border: 1px solid #0f0; padding: 20px; margin: 20px; }
        .key { background: #111; padding: 20px; margin: 20px 0; font-size: 18px; }
        .warning { color: #ff0; }
    </style>
</head>
<body>
    <div class="terminal">
        <h1>New API Key Generated</h1>
        <div class="key">` + key + `</div>
        <p class="warning">Save this key now. You won't be able to see it again!</p>
        <button onclick="location.href='/dashboard/apikeys'">Back to API Keys</button>
    </div>
</body>
</html>`

	t, _ := template.New("newkey").Parse(tmpl)
	if err := t.Execute(w, nil); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (h *APIKeysHandler) RevokeKey(w http.ResponseWriter, r *http.Request) {
	// In a real implementation, this would delete the key from the database
	// For now, just redirect back to the list
	http.Redirect(w, r, "/dashboard/apikeys", http.StatusSeeOther)
}
