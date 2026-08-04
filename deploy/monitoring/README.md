# deploy/monitoring

Alert delivery for the SLC production box (launch sequence 5.2). Prometheus
already evaluates the rules in `/etc/prometheus/rules/vaultaire-alerts.yml`
(VaultaireDown, VaultaireHighErrorRate, disk/memory/CPU, NodeDown); these
files make the alerts actually reach a human.

## Pipeline

```
Prometheus (:9090)
  └─ alerting → Alertmanager (127.0.0.1:9093, clustering disabled)
       └─ webhook → ntfy-bridge (127.0.0.1:9095, python3 stdlib)
            └─ ntfy.sh JSON publish → phone/browser push on the topic
```

The ntfy topic name is the only access control on a public ntfy.sh topic, so
it is **not** committed. It lives in `/etc/default/ntfy-bridge` on the server
(`NTFY_TOPIC=...`), which systemd injects via `EnvironmentFile`.

## Files

| File | Installs to | Purpose |
|------|-------------|---------|
| `alertmanager.yml` | `/etc/prometheus/alertmanager.yml` | route → ntfy webhook receiver, send_resolved, critical-inhibits-warning |
| `ntfy-bridge.py` | `/opt/vaultaire/monitoring/ntfy-bridge.py` | Alertmanager webhook → readable ntfy push (UTF-8-safe JSON publish) |
| `ntfy-bridge.service` | `/etc/systemd/system/ntfy-bridge.service` | sandboxed systemd unit (DynamicUser) |

## Install (already done on slc-vaultaire-01, 2026-08-03)

```bash
apt-get install -y prometheus-alertmanager
# Single-node box with only public IPs: gossip clustering must be disabled
# or Alertmanager exits at boot ("no private IP found").
cat > /etc/default/prometheus-alertmanager <<'EOF'
ARGS="--cluster.listen-address= --web.listen-address=127.0.0.1:9093"
EOF
install -m 0755 ntfy-bridge.py /opt/vaultaire/monitoring/ntfy-bridge.py
install -m 0644 ntfy-bridge.service /etc/systemd/system/ntfy-bridge.service
install -m 0644 alertmanager.yml /etc/prometheus/alertmanager.yml
printf 'NTFY_TOPIC=%s\n' "vaultaire-slc-$(openssl rand -hex 6)" > /etc/default/ntfy-bridge
chmod 600 /etc/default/ntfy-bridge
systemctl daemon-reload
systemctl enable --now ntfy-bridge
systemctl restart prometheus-alertmanager
```

Prometheus needs no change — `alerting: alertmanagers: localhost:9093` was
already in `/etc/prometheus/prometheus.yml`.

## Subscribing (the human end)

Install the ntfy app (Android/iOS/desktop, https://ntfy.sh) and subscribe to
the topic from `/etc/default/ntfy-bridge` on the server, or watch it in a
browser at `https://ntfy.sh/<topic>`.

## Testing

Synthetic alert through the full Alertmanager → bridge → ntfy chain:

```bash
curl -XPOST http://localhost:9093/api/v2/alerts -H 'Content-Type: application/json' \
  -d '[{"labels":{"alertname":"AlertDeliveryTest","severity":"critical"},
        "annotations":{"summary":"delivery test","description":"ignore"}}]'
```

Real end-to-end (VaultaireDown; ~90s of prod downtime):
`systemctl stop vaultaire`, wait for the push (10s scrape + 1m `for:` +
10s group_wait ≈ 85s), `systemctl start vaultaire`. Verified 2026-08-03:
delivered at ~85s, RESOLVED notice followed after restart.

## Known limits

- **Push only.** ntfy.sh rejects anonymous email publishing, and the box has
  no SMTP (seq 5.6). When an email provider lands, add an `email_configs`
  receiver alongside the webhook — or pay for ntfy and set the `email` field
  in the bridge.
- Alertmanager web UI is loopback-only; reach it via SSH tunnel
  (`ssh -L 9093:localhost:9093 vaultaire-slc`).
