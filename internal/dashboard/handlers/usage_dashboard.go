// internal/dashboard/handlers/usage_dashboard.go
package handlers

import (
	"html/template"
	"net/http"
)

const usageDashboardHTML = `
<!DOCTYPE html>
<html>
<head>
    <title>Usage Dashboard - Vaultaire</title>
    <style>
        body { font-family: monospace; background: #000; color: #0f0; padding: 20px; }
        .container { max-width: 800px; margin: 0 auto; }
        .usage-bar { background: #111; border: 1px solid #0f0; height: 30px; margin: 20px 0; }
        .usage-fill { background: #0f0; height: 100%; transition: width 0.3s; }
        .usage-fill.warning { background: #ff0; }
        .usage-fill.critical { background: #f00; }
        .stats { display: grid; grid-template-columns: 1fr 1fr; gap: 20px; }
        .stat-card { border: 1px solid #0f0; padding: 15px; background: #111; }
        .alerts { margin-top: 30px; }
        .alert { padding: 10px; margin: 10px 0; border-left: 3px solid; }
        .alert.info { border-color: #00f; }
        .alert.warning { border-color: #ff0; color: #ff0; }
        .alert.critical { border-color: #f00; color: #f00; }
    </style>
</head>
<body>
    <div class="container">
        <h1>[ USAGE DASHBOARD ]</h1>

        <div class="usage-bar">
            <div class="usage-fill" id="usage-bar" style="width: 0%"></div>
        </div>
        <p id="usage-text">Loading...</p>

        <div class="stats">
            <div class="stat-card">
                <h3>Storage Used</h3>
                <p id="storage-used">--</p>
            </div>
            <div class="stat-card">
                <h3>Storage Limit</h3>
                <p id="storage-limit">--</p>
            </div>
        </div>

        <div class="alerts">
            <h2>[ ALERTS ]</h2>
            <div id="alerts-container"></div>
        </div>
    </div>

    <script>
        async function loadUsageStats() {
            try {
                const statsResp = await fetch('/api/v1/usage/stats');
                const stats = await statsResp.json();

                // Update UI
                document.getElementById('storage-used').textContent = formatBytes(stats.storage_used);
                document.getElementById('storage-limit').textContent = formatBytes(stats.storage_limit);
                document.getElementById('usage-text').textContent = stats.usage_percent.toFixed(1) + '% used';

                // Update bar
                const bar = document.getElementById('usage-bar');
                bar.style.width = stats.usage_percent + '%';
                bar.className = 'usage-fill';
                if (stats.usage_percent >= 90) bar.className += ' critical';
                else if (stats.usage_percent >= 80) bar.className += ' warning';

                // Load alerts
                const alertsResp = await fetch('/api/v1/usage/alerts');
                const alerts = await alertsResp.json();
                const alertsContainer = document.getElementById('alerts-container');
                alertsContainer.innerHTML = '';

                if (alerts.length === 0) {
                    alertsContainer.innerHTML = '<p>No alerts</p>';
                } else {
                    alerts.forEach(alert => {
                        const div = document.createElement('div');
                        div.className = 'alert ' + alert.level.toLowerCase();
                        div.textContent = alert.message + ' (Currently: ' + alert.current.toFixed(1) + '%)';
                        alertsContainer.appendChild(div);
                    });
                }
            } catch (err) {
                console.error('Failed to load usage stats:', err);
            }
        }

        function formatBytes(bytes) {
            const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
            if (bytes === 0) return '0 B';
            const i = Math.floor(Math.log(bytes) / Math.log(1024));
            return (bytes / Math.pow(1024, i)).toFixed(2) + ' ' + sizes[i];
        }

        // Load on page load and refresh every 30 seconds
        loadUsageStats();
        setInterval(loadUsageStats, 30000);
    </script>
</body>
</html>
`

func HandleUsageDashboard(w http.ResponseWriter, r *http.Request) {
	tmpl := template.Must(template.New("usage").Parse(usageDashboardHTML))
	if err := tmpl.Execute(w, nil); err != nil {
		http.Error(w, "failed to render template", http.StatusInternalServerError)
	}
}
