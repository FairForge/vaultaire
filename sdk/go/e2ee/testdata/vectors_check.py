#!/usr/bin/env python3
"""Recompute the deterministic sections of vectors.json without Go (K2b).

An independent implementation of every layout the Go package pins: PyNaCl
(libsodium) for XChaCha20-Poly1305, Argon2id and Ed25519, and the Python
standard library for SHA-256, HMAC and HKDF (RFC 5869). Run it by hand
before committing a new section; it is not part of CI (CI has no PyNaCl).

    python3 -I sdk/go/e2ee/testdata/vectors_check.py

The 0x03 sealed box and the share's HPKE layer cannot be recomputed here
(no X-Wing in libsodium); their open side is pinned by the Go tests.
"""
import json
import pathlib
import struct
import subprocess
import sys

from nacl import bindings

LABEL = b"stored.cloud/e2ee/v1/"


def box(key, nonce, plaintext, ad, fmt=0x01):
    ct = bindings.crypto_aead_xchacha20poly1305_ietf_encrypt(plaintext, bytes([fmt]) + ad, nonce, key)
    return bytes([fmt]) + nonce + ct


def argon2id_openssl(password, salt, mem_kib, t, p):
    """OpenSSL 3.2+'s ARGON2ID KDF: a second implementation (lanes > 1 too)."""
    out = subprocess.run(
        ["openssl", "kdf", "-keylen", "32", "-kdfopt", "hexpass:" + password.hex(), "-kdfopt", "hexsalt:" + salt.hex(),
         "-kdfopt", f"iter:{t}", "-kdfopt", f"memcost:{mem_kib}", "-kdfopt", f"lanes:{p}",
         "ARGON2ID"], check=True, capture_output=True, text=True).stdout
    return bytes.fromhex(out.strip().replace(":", ""))


def argon2id_sodium(password, salt, mem_kib, t):
    """libsodium's crypto_pwhash (Argon2id v1.3, one lane only)."""
    return bindings.crypto_pwhash_alg(32, password, salt, t, mem_kib * 1024, bindings.crypto_pwhash_ALG_ARGON2ID13)


def main():
    v = json.loads(pathlib.Path(__file__).with_name("vectors.json").read_text())
    h = bytes.fromhex
    checks = []

    b = v["box"]
    checks.append(("box", box(h(b["key"]), h(b["nonce"]), h(b["plaintext"]), h(b["ad"])) == h(b["msg"])))

    for i, a in enumerate(v["argon2id"]):
        args = (h(a["password"]), h(a["salt"]), a["memory_kib"], a["iterations"], a["parallelism"])
        checks.append((f"argon2id[{i}] openssl", argon2id_openssl(*args) == h(a["kek"])))
        if a["parallelism"] == 1:
            checks.append((f"argon2id[{i}] libsodium", argon2id_sodium(*args[:4]) == h(a["kek"])))

    w = v["password_wrap"]
    hdr = bytes([0x04, 0x01]) + struct.pack(">IIB", w["memory_kib"], w["iterations"], w["parallelism"]) + h(w["salt"])
    kek = argon2id_sodium(h(w["password"]), h(w["salt"]), w["memory_kib"], w["iterations"])
    want = hdr + box(kek, h(w["nonce"]), h(w["master_key"]), LABEL + b"master-wrap/kek" + hdr)
    checks.append(("password_wrap", want == h(w["wrapped"])))

    for name, kw in sorted(v["key_wraps"].items()):
        want = box(h(kw["wrapping_key"]), h(kw["nonce"]), h(kw["key"]), kw["ad"].encode())
        checks.append((f"key_wraps.{name}", want == h(kw["wrapped"])))

    bad = [name for name, ok in checks if not ok]
    for name, ok in checks:
        print(("ok  " if ok else "BAD ") + name)
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
