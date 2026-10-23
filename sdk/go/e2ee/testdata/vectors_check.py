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
import base64
import hashlib
import hmac
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


def lp(b):
    return struct.pack(">H", len(b)) + b


def hkdf(ikm, info, n=32):
    """RFC 5869 with an empty salt (= HashLen zero bytes), as Go's crypto/hkdf with salt nil."""
    prk = hmac.new(b"\x00" * 32, ikm, hashlib.sha256).digest()
    out, t, i = b"", b"", 1
    while len(out) < n:
        t = hmac.new(prk, t + info + bytes([i]), hashlib.sha256).digest()
        out, i = out + t, i + 1
    return out[:n]


def seal_chunk(space_key, chunk):
    digest = hashlib.sha256(chunk).digest()
    key = hkdf(space_key, LABEL + b"chunk-key" + digest)
    nonce = hmac.new(key, LABEL + b"chunk-nonce" + chunk, hashlib.sha256).digest()[:24]
    cid = hmac.new(hkdf(space_key, LABEL + b"chunk-id"), digest, hashlib.sha256).digest()
    return digest, key, cid, box(key, nonce, chunk, b"", fmt=0x02)


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

    for name, iv in sorted(v["identities"].items()):
        seed = h(iv["sig_private"])
        assert seed[0] == 0x01
        pk, sk = bindings.crypto_sign_seed_keypair(seed[1:])
        checks.append((f"identities.{name}.sig_public", b"\x01" + pk == h(iv["sig_public"])))
        fp = hashlib.sha256(LABEL + b"fingerprint" + lp(h(iv["enc_public"])) + lp(h(iv["sig_public"]))).digest()
        checks.append((f"identities.{name}.fingerprint", fp == h(iv["fingerprint"])))
        text = base64.b32encode(fp[:20]).decode().lower()
        checks.append((f"identities.{name}.fingerprint_string", " ".join(text[i:i + 4] for i in range(0, 32, 4)) == iv["fingerprint_string"]))
        msg = lp(LABEL + b"sig/" + iv["sign_context"].encode()) + h(iv["sign_message"])
        sig = bindings.crypto_sign(msg, sk)[:64]
        checks.append((f"identities.{name}.signature", b"\x01" + sig == h(iv["signature"])))
        mk = h(iv["master_key"])
        checks.append((f"identities.{name}.wrapped_enc", box(mk, h(iv["nonce_enc"]), h(iv["enc_private"]), LABEL + b"identity/enc") == h(iv["wrapped_enc"])))
        checks.append((f"identities.{name}.wrapped_sig", box(mk, h(iv["nonce_sig"]), seed, LABEL + b"identity/sig") == h(iv["wrapped_sig"])))

    c = v["chunk_id"]
    digest, _, cid, _ = seal_chunk(h(c["space_key"]), h(c["chunk"]))
    checks.append(("chunk_id.hash", digest == h(c["chunk_hash"])))
    checks.append(("chunk_id", cid == h(c["chunk_id"])))
    checks.append(("chunk_id is not the hash", cid != digest))

    bw = v["bound_wraps"]
    w = bw["space_key_owner"]
    ad = LABEL + b"space-key" + lp(h(w["space_id"]))
    checks.append(("bound_wraps.space_key_owner.ad", ad == h(w["ad"])))
    checks.append(("bound_wraps.space_key_owner", box(h(w["wrapping_key"]), h(w["nonce"]), h(w["key"]), ad) == h(w["wrapped"])))
    w = bw["object_key"]
    ad = LABEL + b"object-key" + lp(h(w["space_id"])) + lp(h(w["object_id"]))
    checks.append(("bound_wraps.object_key.ad", ad == h(w["ad"])))
    checks.append(("bound_wraps.object_key", box(h(w["wrapping_key"]), h(w["nonce"]), h(w["key"]), ad) == h(w["wrapped"])))

    o = v["object"]
    sk, sid, oid = h(o["space_key"]), h(o["space_id"]), h(o["object_id"])
    prev = b"\x00" * 32
    for mv in o["versions"]:
        ver = mv["version"]
        checks.append((f"object.v{ver}.previous", h(mv["previous"]) == prev))
        refs, total = b"", 0
        for i, (chunk, sealed) in enumerate(zip(mv["chunks"], mv["sealed_chunks"])):
            digest, key, cid, sc = seal_chunk(sk, h(chunk))
            checks.append((f"object.v{ver}.chunk[{i}]", sc == h(sealed)))
            refs += cid + digest + key + struct.pack(">II", len(h(chunk)), len(sc))
            total += len(h(chunk))
        pt = (b"\x01" + lp(sid) + lp(oid) + struct.pack(">Q", ver) + prev + b"\x00" + struct.pack(">QI", total, len(mv["chunks"])) + refs)
        ad = LABEL + b"manifest" + lp(sid) + lp(oid) + struct.pack(">Q", ver)
        sealed_manifest = box(h(o["object_key"]), h(mv["nonce"]), pt, ad)
        checks.append((f"object.v{ver}.manifest", sealed_manifest == h(mv["manifest"])))
        checks.append((f"object.v{ver}.hash", hashlib.sha256(sealed_manifest).digest() == h(mv["hash"])))
        prev = hashlib.sha256(sealed_manifest).digest()

    sh = v["share"]
    share = h(sh["share"])
    sender, recipient = v["identities"][sh["sender"]], v["identities"][sh["recipient"]]
    n = struct.unpack(">I", share[65:69])[0]
    checks.append(("share.format", share[0] == 0x05))
    checks.append(("share.sender_fp", share[1:33] == h(sender["fingerprint"])))
    checks.append(("share.recipient_fp", share[33:65] == h(recipient["fingerprint"])))
    checks.append(("share.sealed_is_0x03", share[69] == 0x03 and n == 1 + 1120 + 32 + 16))
    sig = share[69 + n:]
    signed = lp(LABEL + b"sig/share") + lp(h(sh["space_id"])) + share[:69 + n]
    try:
        bindings.crypto_sign_open(sig[1:] + signed, h(sender["sig_public"])[1:])
        checks.append(("share.signature", sig[0] == 0x01 and len(sig) == 65))
    except Exception:
        checks.append(("share.signature", False))

    bad = [name for name, ok in checks if not ok]
    for name, ok in checks:
        print(("ok  " if ok else "BAD ") + name)
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
