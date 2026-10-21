#!/usr/bin/env python3
"""sia-metrics.py — textfile metrics for the s3d gateways and their Litestream
replication, for node_exporter's textfile collector (deploy/sia/README.md).

Runs every minute (sia-metrics.timer) as the gateways' owner. For each gateway
instance named in SIA_GATEWAYS (default "s3d-lab,s3d-lab2"; the instance is
~/<instance>/ and its binary is S3D_BIN in ~/.config/stored-sia/<instance>.env)
it exports the unit state, `s3d status`, the age of the oldest buffered object
(a PUT's 200 is only on local disk until its upload), and counters of the SDK
events our build logs (not-enough-funds refusals, refill waits, chunk give-ups,
cut bodies, failed shard writes). Litestream's own /metrics (127.0.0.1:9097)
is folded in per gateway. Written atomically to
$SIA_METRICS_DIR/vaultaire_sia.prom (default /opt/vaultaire/metrics).
"""
import os
import re
import subprocess
import time
import urllib.request

HOME = os.path.expanduser("~")
GATEWAYS = [g for g in os.environ.get("SIA_GATEWAYS", "s3d-lab,s3d-lab2").split(",") if g]
OUT_DIR = os.environ.get("SIA_METRICS_DIR", "/opt/vaultaire/metrics")
LITESTREAM = os.environ.get("SIA_LITESTREAM_METRICS", "http://127.0.0.1:9097/metrics")

UNITS = {"B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}
# s3d status line -> metric name (counts and sizes)
STATUS = {
    "Pending Objects": "pending_objects",
    "Pending Size": "pending_bytes",
    "Uploaded Objects": "uploaded_objects",
    "Uploaded Size": "uploaded_bytes",
    "Unpinned Objects": "unpinned_objects",
    "Failed Uploads": "failed_uploads",
    "Orphaned Objects": "orphaned_objects",
    "Multipart Uploads": "multipart_uploads",
}
# substring in the gateway's log -> event label (our build's warn lines)
EVENTS = {
    "not enough funds": "funds_refusal",
    "waiting for account refill": "refill_wait",
    "chunk recovery failed, giving up": "chunk_giveup",
    "failed to write object body": "body_cut",
    "shard write failed": "shard_write_failed",
}


def size(v, unit):
    return float(v) * UNITS.get(unit, 1)


def env_file(path):
    out = {}
    try:
        for line in open(path):
            if "=" in line and not line.lstrip().startswith("#"):
                k, v = line.strip().split("=", 1)
                out[k] = v
    except OSError:
        pass
    return out


def gateway(inst, lines):
    lab = f'gateway="{inst}"'
    up = subprocess.run(["systemctl", "--user", "is-active", f"s3d@{inst}.service"],
                        capture_output=True, text=True).stdout.strip() == "active"
    lines.append(f"vaultaire_sia_gateway_up{{{lab}}} {int(up)}")

    binary = env_file(f"{HOME}/.config/stored-sia/{inst}.env").get("S3D_BIN", "")
    ok = 0
    if up and binary:
        r = subprocess.run([binary, "status"], capture_output=True, text=True, timeout=30,
                           env=dict(os.environ, S3D_CONFIG_FILE=f"{HOME}/{inst}/s3d.yml"))
        if r.returncode == 0:
            ok = 1
            for raw in r.stdout.splitlines():
                key, _, val = raw.strip().partition(":")
                val = val.strip()
                if key in STATUS:
                    m = re.match(r"([\d.]+)\s*([KMGT]?i?B)?", val)
                    if m:
                        n = size(m.group(1), m.group(2)) if m.group(2) else float(m.group(1))
                        lines.append(f"vaultaire_sia_{STATUS[key]}{{{lab}}} {n:.0f}")
                elif key == "Used":
                    # "0 B / 10.00 GiB, 0.0%"
                    m = re.match(r"([\d.]+)\s*([KMGT]?i?B)\s*/\s*([\d.]+)\s*([KMGT]?i?B)", val)
                    if m:
                        lines.append(f"vaultaire_sia_buffer_used_bytes{{{lab}}} {size(m.group(1), m.group(2)):.0f}")
                        lines.append(f"vaultaire_sia_buffer_limit_bytes{{{lab}}} {size(m.group(3), m.group(4)):.0f}")
    lines.append(f"vaultaire_sia_status_ok{{{lab}}} {ok}")

    oldest = 0.0
    up_dir = f"{HOME}/{inst}/data/uploads"
    try:
        mtimes = [os.stat(os.path.join(up_dir, e)).st_mtime for e in os.listdir(up_dir)]
        if mtimes:
            oldest = max(0.0, time.time() - min(mtimes))
    except OSError:
        pass
    lines.append(f"vaultaire_sia_oldest_pending_seconds{{{lab}}} {oldest:.0f}")

    counts = dict.fromkeys(EVENTS.values(), 0)
    try:
        with open(f"{HOME}/{inst}/data/s3d.log", errors="replace") as f:
            for raw in f:
                for needle, ev in EVENTS.items():
                    if needle in raw:
                        counts[ev] += 1
    except OSError:
        pass
    for ev, n in counts.items():
        lines.append(f'vaultaire_sia_log_events_total{{{lab},event="{ev}"}} {n}')


def litestream(lines):
    try:
        body = urllib.request.urlopen(LITESTREAM, timeout=5).read().decode()
    except Exception:
        lines.append("vaultaire_sia_litestream_up 0")
        return
    lines.append("vaultaire_sia_litestream_up 1")
    for raw in body.splitlines():
        m = re.match(r'litestream_(sync_count|sync_error_count|disk_full)\{db="([^"]+)"\}\s+(\S+)', raw)
        if not m:
            continue
        db = m.group(2)
        inst = next((g for g in GATEWAYS if db.startswith(f"{HOME}/{g}/")), None)
        if inst is None:
            continue
        name = {"sync_count": "litestream_syncs_total", "sync_error_count": "litestream_sync_errors_total",
                "disk_full": "litestream_disk_full"}[m.group(1)]
        lines.append(f'vaultaire_sia_{name}{{gateway="{inst}"}} {float(m.group(3)):.0f}')


def main():
    lines = []
    for inst in GATEWAYS:
        gateway(inst, lines)
    litestream(lines)
    lines.append(f"vaultaire_sia_metrics_last_run_timestamp_seconds {time.time():.0f}")
    os.makedirs(OUT_DIR, exist_ok=True)
    tmp = os.path.join(OUT_DIR, ".vaultaire_sia.prom.tmp")
    with open(tmp, "w") as f:
        f.write("\n".join(lines) + "\n")
    os.replace(tmp, os.path.join(OUT_DIR, "vaultaire_sia.prom"))


if __name__ == "__main__":
    main()
