package handlers

import (
	"html/template"
	"net/http"
)

type DashboardHandler struct {
	templates *template.Template
}

func NewDashboardHandler() *DashboardHandler {
	// For now, use inline template to pass tests
	tmplStr := `<!DOCTYPE html>
<html>
<head>
    <title>VAULTAIRE CONTROL PANEL</title>
    <style>
        body {
            background: #000;
            color: #0f0;
            font-family: 'Courier New', monospace;
        }
        .terminal {
            border: 1px solid #0f0;
            padding: 20px;
        }
    </style>
</head>
<body>
    <div class="terminal">
        <h1>VAULTAIRE CONTROL PANEL</h1>
    </div>
</body>
</html>`

	tmpl, _ := template.New("dashboard").Parse(tmplStr)
	return &DashboardHandler{templates: tmpl}
}

func (h *DashboardHandler) Home(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.templates.Execute(w, nil); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
}
