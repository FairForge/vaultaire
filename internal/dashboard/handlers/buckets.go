package handlers

import (
	"html/template"
	"net/http"
)

type BucketsHandler struct {
	db interface{}
}

func NewBucketsHandler(db interface{}) *BucketsHandler {
	return &BucketsHandler{db: db}
}

func (h *BucketsHandler) ListBuckets(w http.ResponseWriter, r *http.Request) {
	tmpl := `<!DOCTYPE html>
<html>
<head>
    <title>S3 Buckets</title>
    <style>
        body { background: #000; color: #0f0; font-family: monospace; }
        .terminal { border: 1px solid #0f0; padding: 20px; margin: 20px; }
        .bucket { background: #111; padding: 10px; margin: 10px 0; }
    </style>
</head>
<body>
    <div class="terminal">
        <h1>S3 Buckets</h1>
        <div class="bucket">
            test-bucket [Created: 2025-09-05]
        </div>
        <div class="bucket">
            production-data [Created: 2025-09-01]
        </div>
        <button onclick="location.href='/dashboard/buckets/new'">Create New Bucket</button>
    </div>
</body>
</html>`

	t, _ := template.New("buckets").Parse(tmpl)
	if err := t.Execute(w, nil); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (h *BucketsHandler) ShowCreateForm(w http.ResponseWriter, r *http.Request) {
	tmpl := `<!DOCTYPE html>
<html>
<head>
    <title>Create Bucket</title>
    <style>
        body { background: #000; color: #0f0; font-family: monospace; }
        .terminal { border: 1px solid #0f0; padding: 20px; margin: 20px; }
        input { background: #111; color: #0f0; border: 1px solid #0f0; padding: 5px; }
    </style>
</head>
<body>
    <div class="terminal">
        <h1>Create Bucket</h1>
        <form action="/dashboard/buckets/create" method="POST">
            <label>Bucket Name:</label><br>
            <input type="text" name="name" pattern="[a-z0-9-]+" required><br><br>
            <button type="submit">Create</button>
            <button type="button" onclick="location.href='/dashboard/buckets'">Cancel</button>
        </form>
    </div>
</body>
</html>`

	t, _ := template.New("create").Parse(tmpl)
	if err := t.Execute(w, nil); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (h *BucketsHandler) CreateBucket(w http.ResponseWriter, r *http.Request) {
	// In a real implementation, this would create the bucket
	// For now, just redirect back to the list
	http.Redirect(w, r, "/dashboard/buckets", http.StatusSeeOther)
}
