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
package main

import (
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/FairForge/vaultaire/internal/translog"
)

func main() {
	out := flag.String("out", "", "where to write the signer key (created 0600; must not exist)")
	flag.Parse()
	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "translog-keygen:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	if out == "" || out == "-" {
		return errors.New("-out <file> is required: the signer key is never printed")
	}
	skey, vkey, err := translog.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	s, err := translog.NewSigner(skey)
	if err != nil {
		return err
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
	fmt.Fprintf(os.Stderr, "origin %s, key ID %08x; signer key in %s (0600) — seal it with systemd-creds, then shred it\n",
		translog.Origin, s.KeyHash(), out)
	return nil
}
