package middleware

import (
	"net/http"
	"runtime/debug"

	"go.uber.org/zap"
)

// Recovery catches panics in downstream handlers, logs the stack trace,
// and renders a branded 500 page. The stack is NOT included in the response
// body — it would leak internal structure to the client.
func Recovery(logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic recovered",
						zap.Any("panic", rec),
						zap.String("path", r.URL.Path),
						zap.String("method", r.Method),
						zap.ByteString("stack", debug.Stack()))
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(errorPage500HTML))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// errorPage500HTML is a self-contained HTML page that does NOT depend on
// templates (if the template engine panicked we can't trust it).
const errorPage500HTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>500 — stored.ge</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:#0f172a;color:#e2e8f0;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0}
.card{background:#1e293b;border-radius:12px;padding:2.5rem;max-width:420px;text-align:center}
h1{font-size:3rem;margin:0 0 0.5rem;color:#f87171}
p{color:#94a3b8;line-height:1.6}
a{color:#60a5fa;text-decoration:none}
a:hover{text-decoration:underline}
</style>
</head>
<body>
<div class="card">
<h1>500</h1>
<h2>Something went wrong</h2>
<p>We hit an unexpected error. Please try again in a moment.</p>
<p><a href="/dashboard">Back to Dashboard</a></p>
</div>
</body>
</html>`
