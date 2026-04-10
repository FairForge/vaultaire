package handlers

import (
	"net/http"

	"go.uber.org/zap"
)

// HandleNotFound returns a handler that renders a branded 404 page for
// unknown paths under /dashboard or /admin.
func HandleNotFound(logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logger.Debug("dashboard 404", zap.String("path", r.URL.Path))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(errorPage404HTML))
	}
}

// errorPage404HTML is a self-contained HTML page — no template dependency.
const errorPage404HTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>404 — stored.ge</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:#0f172a;color:#e2e8f0;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0}
.card{background:#1e293b;border-radius:12px;padding:2.5rem;max-width:420px;text-align:center}
h1{font-size:3rem;margin:0 0 0.5rem;color:#fbbf24}
p{color:#94a3b8;line-height:1.6}
a{color:#60a5fa;text-decoration:none}
a:hover{text-decoration:underline}
</style>
</head>
<body>
<div class="card">
<h1>404</h1>
<h2>Page not found</h2>
<p>The page you're looking for doesn't exist or has been moved.</p>
<p><a href="/dashboard">Back to Dashboard</a></p>
</div>
</body>
</html>`
