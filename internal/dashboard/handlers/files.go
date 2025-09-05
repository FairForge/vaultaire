package handlers

import (
	"html/template"
	"net/http"
)

type FilesHandler struct {
	db interface{}
}

func NewFilesHandler(db interface{}) *FilesHandler {
	return &FilesHandler{db: db}
}

func (h *FilesHandler) ListFiles(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = "default"
	}

	tmpl := `<!DOCTYPE html>
<html>
<head>
    <title>File Browser</title>
    <style>
        body { background: #000; color: #0f0; font-family: monospace; }
        .terminal { border: 1px solid #0f0; padding: 20px; margin: 20px; }
        .file { padding: 5px; margin: 2px 0; }
        .file:hover { background: #111; }
    </style>
</head>
<body>
    <div class="terminal">
        <h1>File Browser</h1>
        <h2>Bucket: ` + bucket + `</h2>
        <div class="file">📄 document.pdf (2.5 MB)</div>
        <div class="file">📄 image.jpg (1.2 MB)</div>
        <div class="file">📁 folder/</div>
        <div class="file">📄 test.txt (15 KB)</div>
    </div>
</body>
</html>`

	t, _ := template.New("files").Parse(tmpl)
	if err := t.Execute(w, nil); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (h *FilesHandler) ShowDetails(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")

	tmpl := `<!DOCTYPE html>
<html>
<head>
    <title>File Details</title>
    <style>
        body { background: #000; color: #0f0; font-family: monospace; }
        .terminal { border: 1px solid #0f0; padding: 20px; margin: 20px; }
    </style>
</head>
<body>
    <div class="terminal">
        <h1>File Details</h1>
        <pre>
Bucket:   ` + bucket + `
Key:      ` + key + `
Size:     1.2 MB
Modified: 2025-09-05 12:34:56
Type:     text/plain
        </pre>
    </div>
</body>
</html>`

	t, _ := template.New("details").Parse(tmpl)
	if err := t.Execute(w, nil); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}
