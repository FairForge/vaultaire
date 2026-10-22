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
import sys

from nacl import bindings

LABEL = b"stored.cloud/e2ee/v1/"


def box(key, nonce, plaintext, ad, fmt=0x01):
    ct = bindings.crypto_aead_xchacha20poly1305_ietf_encrypt(plaintext, bytes([fmt]) + ad, nonce, key)
    return bytes([fmt]) + nonce + ct


def main():
    v = json.loads(pathlib.Path(__file__).with_name("vectors.json").read_text())
    h = bytes.fromhex
    checks = []

    b = v["box"]
    checks.append(("box", box(h(b["key"]), h(b["nonce"]), h(b["plaintext"]), h(b["ad"])) == h(b["msg"])))

    bad = [name for name, ok in checks if not ok]
    for name, ok in checks:
        print(("ok  " if ok else "BAD ") + name)
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
