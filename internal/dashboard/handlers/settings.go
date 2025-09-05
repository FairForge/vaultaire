package handlers

import (
	"html/template"
	"net/http"
)

type SettingsHandler struct {
	db interface{}
}

func NewSettingsHandler(db interface{}) *SettingsHandler {
	return &SettingsHandler{db: db}
}

func (h *SettingsHandler) ShowSettings(w http.ResponseWriter, r *http.Request) {
	tmpl := `<!DOCTYPE html>
<html>
<head>
    <title>Settings</title>
    <style>
        body { background: #000; color: #0f0; font-family: monospace; }
        .terminal { border: 1px solid #0f0; padding: 20px; margin: 20px; }
        .setting { margin: 15px 0; }
        input, select { background: #111; color: #0f0; border: 1px solid #0f0; padding: 5px; }
    </style>
</head>
<body>
    <div class="terminal">
        <h1>Settings</h1>
        <form action="/dashboard/settings" method="POST">
            <div class="setting">
                <label>Default Region:</label><br>
                <select name="region">
                    <option>us-east-1</option>
                    <option>us-west-2</option>
                    <option>eu-west-1</option>
                </select>
            </div>
            <div class="setting">
                <label>Storage Class:</label><br>
                <select name="storage_class">
                    <option>STANDARD</option>
                    <option>REDUCED_REDUNDANCY</option>
                </select>
            </div>
            <div class="setting">
                <label>Notification Email:</label><br>
                <input type="email" name="email" value="user@example.com">
            </div>
            <div class="setting">
                <label>Auto-delete after (days):</label><br>
                <input type="number" name="retention" value="30" min="1" max="365">
            </div>
            <button type="submit">Save Settings</button>
        </form>
    </div>
</body>
</html>`

	t, _ := template.New("settings").Parse(tmpl)
	if err := t.Execute(w, nil); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (h *SettingsHandler) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	// In a real implementation, this would save the settings
	// For now, just redirect back
	http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
}
