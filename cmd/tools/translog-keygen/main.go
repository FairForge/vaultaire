// Command translog-keygen makes the transparency log's checkpoint signing
// key (keystone K4, docs/TRANSPARENCY.md § The signing key): an Ed25519
// note key named stored.cloud/translog/v1.
//
//	umask 077
//	translog-keygen -out /root/translog.key > translog.vkey
//
// The SIGNER key is written to -out (mode 0600, never an existing file) for
// `systemd-creds encrypt --name=translog_signing_key`; it is never printed.
// The VERIFIER key — the public half, to publish outside our infrastructure
// before the first signed checkpoint — goes to stdout, and the key ID to
// stderr.
//
// A WITNESS's key (cmd/tools/translog-witness) is a cosignature/v1 key
// (K4c-2, c2sp.org/tlog-cosignature): ML-DSA-44, timestamped, under the
// witness's own name — never the log's:
//
//	translog-keygen -cosignature -name witness.<host> -out ~/.translog-witness/witness.key > witness.vkey
//
// (A plain note key under another name, -name without -cosignature, is
// still made — for nothing that cosigns: translog-witness refuses it.)
package main

import (
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"os"

	"golang.org/x/mod/sumdb/note"

	"github.com/FairForge/vaultaire/internal/translog"
)

func main() {
	out := flag.String("out", "", "where to write the signer key (created 0600; must not exist)")
	name := flag.String("name", translog.Origin, "the key's name: the log's origin, or a witness's own name (translog-witness -key)")
	cosig := flag.Bool("cosignature", false, "make a witness's ML-DSA-44 cosignature/v1 key (c2sp.org/tlog-cosignature) named -name")
	flag.Parse()
	if err := run(*out, *name, *cosig); err != nil {
		fmt.Fprintln(os.Stderr, "translog-keygen:", err)
		os.Exit(1)
	}
}

func run(out, name string, cosig bool) error {
	if out == "" || out == "-" {
		return errors.New("-out <file> is required: the signer key is never printed")
	}
	if cosig && name == translog.Origin {
		return errors.New("-cosignature makes a witness's key: give it the witness's own name (-name witness.<host>), never the log's origin")
	}
	var (
		skey, vkey string
		s          note.Signer
		err        error
	)
	switch {
	case cosig:
		if skey, vkey, err = translog.GenerateCosignerKey(name); err == nil {
			s, err = translog.NewCosigner(skey, nil)
		}
	case name == translog.Origin:
		if skey, vkey, err = translog.GenerateKey(rand.Reader); err == nil {
			s, err = translog.NewSigner(skey)
		}
	default:
		// A witness key: any note key name but the log's origin.
		if skey, vkey, err = note.GenerateKey(rand.Reader, name); err == nil {
			s, err = note.NewSigner(skey)
		}
	}
	if err != nil {
		return fmt.Errorf("generate a key named %q: %w", name, err)
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", out, err)
	}
	if _, err := fmt.Fprintln(f, skey); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", out, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", out, err)
	}
	fmt.Println(vkey)
	if cosig {
		fmt.Fprintf(os.Stderr, "witness cosignature/v1 key (ML-DSA-44) %s, key ID %08x; signer key in %s (0600) — keep it on the witness's machine only; the verifier key on stdout goes in the log's TRANSLOG_WITNESSES and readers' translog-verify -witness\n",
			name, s.KeyHash(), out)
		return nil
	}
	if name != translog.Origin {
		fmt.Fprintf(os.Stderr, "note key %s, key ID %08x; signer key in %s (0600) — NOT a witness key (translog-witness wants -cosignature)\n",
			name, s.KeyHash(), out)
		return nil
	}
	fmt.Fprintf(os.Stderr, "origin %s, key ID %08x; signer key in %s (0600) — seal it with systemd-creds, then shred it\n",
		translog.Origin, s.KeyHash(), out)
	return nil
}
