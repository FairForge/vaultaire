#!/bin/bash
# health-check.sh — cron every 5 min on the box (crontab of user1); appends OK/FAILED
# to /opt/vaultaire/logs/health-check.log. Installed at /opt/vaultaire/bin/health-check.sh.
# Probes the ACTIVE slot (zero-downtime deploys: :8000 or :8001, /opt/vaultaire/ACTIVE_PORT).
PORT=$(cat /opt/vaultaire/ACTIVE_PORT 2>/dev/null || echo 8000)
if ! curl -sf --max-time 10 "http://localhost:$PORT/health" >/dev/null; then
    echo "$(date): HEALTH CHECK FAILED (:$PORT)" >> /opt/vaultaire/logs/health-check.log
else
    echo "$(date): OK" >> /opt/vaultaire/logs/health-check.log
fi
# Keep log under 1000 lines
tail -1000 /opt/vaultaire/logs/health-check.log > /opt/vaultaire/logs/health-check.log.tmp
mv /opt/vaultaire/logs/health-check.log.tmp /opt/vaultaire/logs/health-check.log
