package handlers

import (
	"html/template"
	"net/http"
)

type UploadHandler struct {
	db interface{}
}

func NewUploadHandler(db interface{}) *UploadHandler {
	return &UploadHandler{db: db}
}

func (h *UploadHandler) ShowUploadForm(w http.ResponseWriter, r *http.Request) {
	tmpl := `<!DOCTYPE html>
<html>
<head>
    <title>Upload Files</title>
    <style>
        body { background: #000; color: #0f0; font-family: monospace; }
        .terminal { border: 1px solid #0f0; padding: 20px; margin: 20px; }
        input[type="file"] {
            background: #111;
            color: #0f0;
            border: 1px solid #0f0;
            padding: 10px;
            margin: 10px 0;
        }
        .progress {
            border: 1px solid #0f0;
            height: 20px;
            margin: 10px 0;
        }
    </style>
</head>
<body>
    <div class="terminal">
        <h1>Upload Files</h1>
        <form action="/dashboard/upload" method="POST" enctype="multipart/form-data">
            <label>Select Bucket:</label><br>
            <select name="bucket" style="background: #111; color: #0f0; border: 1px solid #0f0; padding: 5px;">
                <option>test-bucket</option>
                <option>production-data</option>
            </select><br><br>
            <label>Choose Files:</label><br>
            <input type="file" name="files" multiple><br><br>
            <button type="submit">Upload</button>
        </form>
    </div>
</body>
</html>`

	t, _ := template.New("upload").Parse(tmpl)
	if err := t.Execute(w, nil); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (h *UploadHandler) HandleUpload(w http.ResponseWriter, r *http.Request) {
	// In a real implementation, this would process the uploaded files
	// For now, just redirect to files browser
	http.Redirect(w, r, "/dashboard/files", http.StatusSeeOther)
}
