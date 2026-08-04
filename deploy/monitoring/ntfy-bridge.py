#!/usr/bin/env python3
"""Alertmanager -> ntfy.sh bridge.

Receives Alertmanager webhook POSTs on 127.0.0.1:9095 and republishes each
alert as a human-readable ntfy.sh notification via the JSON publish endpoint
(UTF-8 safe — header-based publish rejects non-latin-1 chars in alert text).

Stdlib only — no pip dependencies. Runs as a systemd unit (ntfy-bridge) with
NTFY_TOPIC supplied by /etc/default/ntfy-bridge, which stays on the server:
the topic name is the only access control on a public ntfy.sh topic.
"""
import json
import os
import sys
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

NTFY_BASE = os.environ.get("NTFY_BASE", "https://ntfy.sh")
NTFY_TOPIC = os.environ.get("NTFY_TOPIC", "")
SERVER_NAME = os.environ.get("ALERT_SERVER_NAME", "slc-vaultaire-01")


def publish(alert, group_status):
    labels = alert.get("labels", {})
    annotations = alert.get("annotations", {})
    name = labels.get("alertname", "unknown")
    severity = labels.get("severity", "none")
    summary = annotations.get("summary", name)
    description = annotations.get("description", "")
    status = alert.get("status", group_status)

    msg = {
        "topic": NTFY_TOPIC,
        "message": f"{name} [{severity}] on {SERVER_NAME}\n{description}",
    }
    if status == "firing":
        msg["title"] = f"FIRING: {summary}"
        msg["priority"] = 5 if severity == "critical" else 4
        msg["tags"] = ["rotating_light" if severity == "critical" else "warning"]
        # No "email" field: ntfy.sh rejects anonymous email publishing
        # (40053). Email fan-out needs an authenticated ntfy account, or
        # alertmanager email_configs once SMTP exists on this box (seq 5.6).
    else:
        msg["title"] = f"RESOLVED: {summary}"
        msg["priority"] = 3
        msg["tags"] = ["white_check_mark"]

    req = urllib.request.Request(
        NTFY_BASE, data=json.dumps(msg).encode("utf-8"),
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        print(f"published {status} {name} -> ntfy ({resp.status})", flush=True)


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length)
        code = 200
        try:
            payload = json.loads(raw)
            for alert in payload.get("alerts", []):
                try:
                    publish(alert, payload.get("status", "firing"))
                except Exception as e:  # one bad publish shouldn't drop the rest
                    print(f"publish failed: {e}", file=sys.stderr, flush=True)
                    code = 502
        except Exception as e:
            print(f"bad webhook payload: {e}", file=sys.stderr, flush=True)
            code = 400
        self.send_response(code)
        self.end_headers()

    def log_message(self, fmt, *args):
        pass  # journald gets our explicit prints; skip per-request noise


if __name__ == "__main__":
    if not NTFY_TOPIC:
        print("NTFY_TOPIC is not set — refusing to start", file=sys.stderr)
        sys.exit(1)
    print(f"ntfy-bridge listening on 127.0.0.1:9095 -> {NTFY_BASE}/<topic>", flush=True)
    HTTPServer(("127.0.0.1", 9095), Handler).serve_forever()
