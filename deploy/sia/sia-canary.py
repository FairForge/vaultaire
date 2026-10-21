#!/usr/bin/env python3
"""sia-canary.py — read canary for the s3d gateways (deploy/sia/README.md).

Every 5 min (sia-canary.timer) a 4 MiB object is read back from each gateway in
SIA_GATEWAYS and its MD5 compared; once a day a 256 MiB object is read too, so
the read budget is exercised as a customer restore would be. The objects are
written once (bucket "canary", PUT then `s3d flush`, so reads come from Sia,
not the local buffer) and kept. Credentials: the gateway's own
~/<instance>/s3-creds.txt. Results go to $SIA_METRICS_DIR/vaultaire_sia_canary.prom:

  vaultaire_sia_canary_success{gateway,size}          1/0, the last read
  vaultaire_sia_canary_seconds{gateway,size}          its duration
  vaultaire_sia_canary_last_success_timestamp_seconds{gateway,size}
"""
import hashlib
import json
import os
import re
import subprocess
import time

HOME = os.path.expanduser("~")
GATEWAYS = [g for g in os.environ.get("SIA_GATEWAYS", "s3d-lab,s3d-lab2").split(",") if g]
OUT_DIR = os.environ.get("SIA_METRICS_DIR", "/opt/vaultaire/metrics")
STATE = os.path.join(HOME, ".config/stored-sia/canary-state.json")
SIZES = {"4m": 4 << 20, "256m": 256 << 20}
LARGE_EVERY = 23 * 3600


def creds(inst):
    txt = open(f"{HOME}/{inst}/s3-creds.txt").read()
    ak = re.search(r"Access Key:\s*(\S+)", txt).group(1)
    sk = re.search(r"Secret Key:\s*(\S+)", txt).group(1)
    return ak, sk


def port(inst):
    return re.search(r"apiAddress:\s*\S+:(\d+)", open(f"{HOME}/{inst}/s3d.yml").read()).group(1)


def aws(inst, *args, timeout=1800):
    ak, sk = creds(inst)
    env = dict(os.environ, AWS_ACCESS_KEY_ID=ak, AWS_SECRET_ACCESS_KEY=sk, AWS_DEFAULT_REGION="us-east-1",
               AWS_CONFIG_FILE="/dev/null", AWS_SHARED_CREDENTIALS_FILE="/dev/null", AWS_MAX_ATTEMPTS="1")
    return subprocess.run(["aws", "--endpoint-url", f"http://127.0.0.1:{port(inst)}", *args],
                          capture_output=True, text=True, env=env, timeout=timeout)


def ensure(inst, name, n, state):
    key = f"{inst}/{name}"
    if key in state:
        return state[key]
    data = os.urandom(n)
    path = os.path.join(HOME, ".config/stored-sia", f".canary-{inst}-{name}")
    with open(path, "wb") as f:
        f.write(data)
    aws(inst, "s3", "mb", "s3://canary")
    r = aws(inst, "s3", "cp", "--only-show-errors", path, f"s3://canary/canary-{name}.bin")
    os.remove(path)
    if r.returncode != 0:
        raise RuntimeError(r.stderr[-200:])
    binary = open(f"{HOME}/.config/stored-sia/{inst}.env").read().split("S3D_BIN=", 1)[1].split()[0]
    subprocess.run([binary, "flush"], env=dict(os.environ, S3D_CONFIG_FILE=f"{HOME}/{inst}/s3d.yml"),
                   capture_output=True, timeout=3600)
    state[key] = hashlib.md5(data).hexdigest()
    return state[key]


def read(inst, name, want):
    out = os.path.join(HOME, ".config/stored-sia", f".canary-read-{inst}-{name}")
    t0 = time.time()
    r = aws(inst, "s3api", "get-object", "--bucket", "canary", "--key", f"canary-{name}.bin", out)
    dt = time.time() - t0
    ok = False
    if r.returncode == 0:
        h = hashlib.md5()
        with open(out, "rb") as f:
            for b in iter(lambda: f.read(1 << 20), b""):
                h.update(b)
        ok = h.hexdigest() == want
    if os.path.exists(out):
        os.remove(out)
    return ok, dt


def main():
    try:
        state = json.load(open(STATE))
    except (OSError, ValueError):
        state = {}
    prev = {}
    try:
        for line in open(os.path.join(OUT_DIR, "vaultaire_sia_canary.prom")):
            if line.startswith("vaultaire_sia_canary_last_success_timestamp_seconds"):
                k, v = line.rsplit(" ", 1)
                prev[k] = v.strip()
    except OSError:
        pass
    lines = []
    for inst in GATEWAYS:
        for name, n in SIZES.items():
            lab = f'gateway="{inst}",size="{name}"'
            ts_key = f"vaultaire_sia_canary_last_success_timestamp_seconds{{{lab}}}"
            if name == "256m" and time.time() - state.get(f"{inst}/last_large", 0) < LARGE_EVERY:
                if ts_key in prev:
                    lines.append(f"{ts_key} {prev[ts_key]}")
                continue
            try:
                want = ensure(inst, name, n, state)
                ok, dt = read(inst, name, want)
            except Exception:
                ok, dt = False, 0.0
            if name == "256m":
                state[f"{inst}/last_large"] = time.time()
            lines.append(f"vaultaire_sia_canary_success{{{lab}}} {int(ok)}")
            lines.append(f"vaultaire_sia_canary_seconds{{{lab}}} {dt:.2f}")
            lines.append(f"{ts_key} {time.time():.0f}" if ok else f"{ts_key} {prev.get(ts_key, '0')}")
    with open(STATE + ".tmp", "w") as f:
        json.dump(state, f)
    os.replace(STATE + ".tmp", STATE)
    tmp = os.path.join(OUT_DIR, ".vaultaire_sia_canary.prom.tmp")
    with open(tmp, "w") as f:
        f.write("\n".join(lines) + "\n")
    os.replace(tmp, os.path.join(OUT_DIR, "vaultaire_sia_canary.prom"))


if __name__ == "__main__":
    main()
