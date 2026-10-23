# K4c-3 Rekor probe: can a Rekor v2 entry carry the log's checkpoint signature?

*Stage 1a Prompt 35, PR 3 (2026-10-23). Lane: Trust. Plan 43.6 and F-16 ask for "signed statements + OpenTimestamps + a Rekor mirror". The prompt's rule: "Preferred: an entry that reuses the note's existing log signature (artifact = the note text, the log's verifier key) — no new signing domain. If Rekor requires our log key to sign something new, do NOT reuse it: write D-P35-* and stop at a probe note (a dedicated anchor key becomes an owner ceremony item)." This is that note. Contract rule 7 (the log) and rule 8 (claim nothing we do not do): no Rekor anchor is built, and nothing says there is one.*

## Answer

**No.** Rekor v2 verifies every entry with **Ed25519ph (or ECDSA / RSA) over a digest**. The log signs a checkpoint with **pure Ed25519 over the note text**. Anchoring in Rekor therefore needs the log's key to make a second kind of signature: a new signing domain. Per the rule, **D-P35-2 stops here**. The way forward is a dedicated anchor key, which is an owner ceremony.

## The sources, quoted

All quotes are from `github.com/sigstore/rekor-tiles`: `main` at `a7ec779` (2026-10-23) and the release `v2.3.0`.

- **Entry types** (`CLIENTS.md`): "Rekor v2 only supports `hashedrekord` (`HashedRekordLogEntryV002`), dropping" `rekord`, `intoto` and `dsse`; "Rekor v2 will not support a DSSE entry type." The request is `POST /api/v2/log/entries` with `hashedRekordRequestV002 {digest, signature {content, verifier {publicKey {rawBytes}, keyDetails}}}`.
- **Algorithms** (`internal/algorithmregistry/algorithmregistry.go`, v2.3.0): `AllowedClientSigningAlgorithms` lists RSA PKCS1v15 2048/3072/4096 SHA-256, ECDSA P-256/P-384/P-521, `PKIX_ED25519` and `PKIX_ED25519_PH`.
- **Verification** (`pkg/types/hashedrekord/hashedrekord.go`, v2.3.0 `verifySignature`): `signature.LoadVerifierWithOpts(v.PublicKey(), options.WithED25519ph())`, then `VerifySignature(…, options.WithDigest(hr.Digest), options.WithCryptoSignerOpts(hashAlg))`. Every Ed25519 key is checked as **Ed25519ph over the submitted digest**. The artifact itself is never sent.
- **`main`, the same day** (`ToLogEntry`): "hashedrekord only permits signing algorithms that prehash the data. Pure Ed25519 (PKIX_ED25519) does not, so GetHashType returns crypto.Hash(0); reject it here rather than calling Size() on an unset hash." In v2.3.0 that `Size()` call is reached, and it panics (below).
- **Response** (`CLIENTS.md`): "Rekor will no longer return SignedEntryTimestamps or include integrated time in the response". Timestamps come from an RFC 3161 authority. The answer is an inclusion proof and a checkpoint.
- **Instances:**
  - production is `https://log2025-1.rekor.sigstore.dev` ("We strongly advise against hardcoding this URL"). Its own checkpoint key is `PKIX_ED25519`, in `root-signing` `trusted_root.json`;
  - no Rekor v2 **staging** URL is published: not in the README, `CLIENTS.md`, `tests/README.md` or `signing_config.v0.2.json`, which lists only the v1 `rekor.sigstore.dev`;
  - nothing was sent to either instance.

## The probe

The probe runs offline, with Docker not running on the Mac, through rekor-tiles **v2.3.0**'s own admission code, `hashedrekord.ToLogEntry`, the function the server calls on every `POST /api/v2/log/entries`. Its inputs are a real lab checkpoint and its real signature:
- the lab log `vaultaire_p35lab`;
- the lab key `stored.cloud/translog/v1+ed69882b`;
- checkpoint 3, signed by the `translog_checkpoint` job.

Its source is below, and it builds with `GOTOOLCHAIN=go1.26.10` against `github.com/sigstore/rekor-tiles/v2 v2.3.0`.

```
$ ./probe cp3.note log.vkey
pure Ed25519 over the note text verifies locally: true
PKIX_ED25519, digest sha256(text): PANIC in ToLogEntry: crypto: Size of unknown hash function
PKIX_ED25519_PH, digest sha512(text): REFUSED: verifying signature: failed to verify signature: ed25519: invalid signature
```

1. **`PKIX_ED25519`, the signature as it is:** v2.3.0 panics on the unset hash, and `main` refuses it with the error quoted above. Either way the entry is not admitted. How a running v2.3.0 server answers the panic was not observed: nothing was sent to one.
2. **`PKIX_ED25519_PH` with the note's SHA-512:** refused. The log's signature is not an Ed25519ph signature, and it never will be: Ed25519ph is RFC 8032's "dom2" variant, a different signing domain over a digest.

The only way in is a signature that does not exist today: the log's key, or another key, signing the digest as Ed25519ph, ECDSA or RSA.

## Why not sign with the log's key anyway

Ed25519ph is domain-separated from pure Ed25519, so a second signature by the same key could not be mistaken for a checkpoint signature. The prompt's rule stands regardless.

The log's key signs **checkpoints only**. Readers pin it for exactly that meaning, and the Witness Network asks that the origin and key "never change". A key that also signs Rekor entries:
- widens what a compromise of it can forge;
- makes its published meaning two things.

A dedicated key keeps each key to one meaning and can be rotated without touching the log.

## The way forward (owner)

1. **A dedicated anchor key**, as an owner ceremony beside the log's signing key (`docs/TRANSPARENCY.md` § The signing key). Use ECDSA P-256 (the Sigstore default) or Ed25519ph. Seal it the same way (systemd credential), publish its verifier key outside our infrastructure, and append its `key_generated` / `key_installed` entries (the `key_role` list gains `anchor_key`).
2. **The job's second kind.** Sign SHA-256 (P-256) or SHA-512 (Ed25519ph) of the note with that key. `POST` the `hashedrekord` request to the instance named in Sigstore's TUF `signing_config` (never a hard-coded URL, per the README). Keep the answer — the `TransparencyLogEntry` with its inclusion proof and checkpoint — as a `translog_anchors` row of `kind = 'rekor'`. Migration 106 already admits that kind.
3. **A lab first**, against a local `rekor-tiles` (`docker compose up --build --wait`, HTTP on :3003). Production Rekor is never used from a lab.

Until then, `TRANSLOG_ANCHORS` refuses a `rekor` line (naming D-P35-2), and `translog-verify -anchors` prints `anchor rekor: not built`.

## The probe's source

```go
// Rekor v2 probe (WP-P35, D-P35-2): can a Rekor v2 hashedrekord entry carry
// the transparency log's own checkpoint signature (pure Ed25519 over the
// note text), verified with the log's key? Runs rekor-tiles' own
// hashedrekord.ToLogEntry — the server's admission code — offline.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	v1 "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	pb "github.com/sigstore/rekor-tiles/v2/pkg/generated/protobuf"
	"github.com/sigstore/rekor-tiles/v2/pkg/types/hashedrekord"
	"github.com/sigstore/sigstore/pkg/signature"
)

func main() {
	note, _ := os.ReadFile(os.Args[1]) // the checkpoint note as the log signed it
	vkey, _ := os.ReadFile(os.Args[2]) // the log's verifier key
	text, sigs, _ := bytes.Cut(note, []byte("\n\n"))
	text = append(text, '\n')
	line := strings.TrimSpace(string(sigs))
	raw, _ := base64.StdEncoding.DecodeString(line[strings.LastIndexByte(line, ' ')+1:])
	sig := raw[4:] // key hash, then the Ed25519 signature
	kraw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.SplitN(string(vkey), "+", 3)[2]))
	pub := ed25519.PublicKey(kraw[1:]) // 0x01 = Ed25519
	fmt.Printf("pure Ed25519 over the note text verifies locally: %v\n", ed25519.Verify(pub, text, sig))
	der, _ := x509.MarshalPKIXPublicKey(pub)
	reg, err := signature.NewAlgorithmRegistryConfig(algs())
	if err != nil {
		panic(err)
	}
	s256, s512 := sha256.Sum256(text), sha512.Sum512(text)
	for _, c := range []struct {
		name   string
		kd     v1.PublicKeyDetails
		digest []byte
	}{
		{"PKIX_ED25519, digest sha256(text)", v1.PublicKeyDetails_PKIX_ED25519, s256[:]},
		{"PKIX_ED25519_PH, digest sha512(text)", v1.PublicKeyDetails_PKIX_ED25519_PH, s512[:]},
	} {
		hr := &pb.HashedRekordRequestV002{Digest: c.digest, Signature: &pb.Signature{Content: sig,
			Verifier: &pb.Verifier{Verifier: &pb.Verifier_PublicKey{PublicKey: &pb.PublicKey{RawBytes: der}}, KeyDetails: c.kd}}}
		fmt.Printf("%s: %s\n", c.name, admit(hr, reg))
	}
}

func admit(hr *pb.HashedRekordRequestV002, reg *signature.AlgorithmRegistryConfig) (out string) {
	defer func() {
		if p := recover(); p != nil {
			out = fmt.Sprintf("PANIC in ToLogEntry: %v", p)
		}
	}()
	if _, err := hashedrekord.ToLogEntry(hr, reg); err != nil {
		return "REFUSED: " + err.Error()
	}
	return "ADMITTED"
}

func algs() []v1.PublicKeyDetails {
	return []v1.PublicKeyDetails{v1.PublicKeyDetails_PKIX_ECDSA_P256_SHA_256, v1.PublicKeyDetails_PKIX_ED25519, v1.PublicKeyDetails_PKIX_ED25519_PH}
}
```
