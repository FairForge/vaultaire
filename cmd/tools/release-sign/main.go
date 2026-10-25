// Command release-sign signs and verifies release artefacts (WP-SUPPLY-1,
// Stage 1a Prompt 41 Part A; docs/RELEASES.md): a detached signed note per
// file (internal/release), by an Ed25519 key named stored.cloud/release/v1.
//
//	release-sign keygen -out /root/release.key > release.vkey
//	release-sign sign vaultaire-linux vaultaire.cdx.json …      → <file>.sig each
//	release-sign verify -vkey @release.vkey vaultaire-linux vaultaire-linux.sig
//	release-sign verify -vkey @release.vkey -log https://trust.stored.cloud/translog -log-vkey @translog.vkey \
//	    -client sdk-wasm e2ee.wasm e2ee.wasm.sig
//
// The SIGNING key is read like the transparency log's (the systemd
// credential release_signing_key, then RELEASE_SIGNING_KEY_FILE, then
// RELEASE_SIGNING_KEY): a source that is set but unusable is a refusal,
// none set is a refusal that says so — signing is an OWNER step on a machine
// that holds the key, never GitHub Actions. verify needs nothing but this
// tool and the public key (-vkey, published on the trust host and in the
// release body); with -log it also proves the artefact's sha256 is a build
// the transparency log lists (a server_build, or the named client's
// client_build) under a checkpoint signed by -log-vkey.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/release"
	"github.com/FairForge/vaultaire/internal/translog"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:], os.Getenv, os.ReadFile)
	case "verify":
		err = verify(os.Stdout, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		err = fmt.Errorf("unknown verb %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "release-sign:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: release-sign keygen -out <file> | sign <artefact>… | verify -vkey <key|@file> [-log <url> -log-vkey <key|@file> [-client <name>]] <artefact> <sig>")
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("out", "", "where to write the signer key (created 0600; must not exist)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" || *out == "-" {
		return errors.New("-out <file> is required: the signer key is never printed")
	}
	skey, vkey, err := release.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	s, err := release.NewSigner(skey)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", *out, err)
	}
	if _, err := fmt.Fprintln(f, skey); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", *out, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", *out, err)
	}
	fmt.Println(vkey)
	fmt.Fprintf(os.Stderr, "release key %s, key ID %08x; signer key in %s (0600) — keep it off every CI machine; the verifier key on stdout is what the trust host and the release body publish (docs/RELEASES.md)\n",
		release.Name, s.KeyHash(), *out)
	return nil
}

// sign writes <artefact>.sig beside each artefact. Nothing is written when
// the key cannot be loaded or any artefact cannot be read: all or none.
func sign(args []string, getenv func(string) string, readFile func(string) ([]byte, error)) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths := fs.Args()
	if len(paths) == 0 {
		return errors.New("sign: name at least one artefact")
	}
	sk, err := release.LoadSigningKey(getenv, readFile)
	if err != nil {
		return err
	}
	if len(sk.Ignored) > 0 {
		fmt.Fprintf(os.Stderr, "release-sign: the key comes from the %s; also set and ignored: %s\n", sk.Source, strings.Join(sk.Ignored, ", "))
	}
	sigs := make([][]byte, len(paths))
	for i, p := range paths {
		if strings.HasSuffix(p, ".sig") {
			return fmt.Errorf("%s: a signature is not signed", p)
		}
		if sigs[i], err = release.Sign(sk.Signer, p); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	for i, p := range paths {
		if err := os.WriteFile(p+".sig", sigs[i], 0o644); err != nil { // #nosec G306 -- a signature is public
			return fmt.Errorf("write %s.sig: %w", p, err)
		}
		fmt.Printf("signed %s → %s.sig (key %08x)\n", p, p, sk.Signer.KeyHash())
	}
	return nil
}

func keyArg(v string) (string, error) {
	if strings.HasPrefix(v, "@") {
		b, err := os.ReadFile(v[1:]) // #nosec G304 -- the operator's key file
		if err != nil {
			return "", fmt.Errorf("read the key file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return v, nil
}

// verify checks one artefact against its signature, offline; with -log the
// artefact's sha256 must also be a build the transparency log lists.
func verify(out io.Writer, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	vkey := fs.String("vkey", "", "the release verifier key, or @file (published on the trust host and in the release body)")
	logURL := fs.String("log", "", "also prove the artefact is a build the transparency log lists: where checkpoint and tile/… are served")
	logVkey := fs.String("log-vkey", "", "with -log: the log's pinned verifier key, or @file")
	client := fs.String("client", "", "with -log: the artefact is this client (sdk-wasm, sdk-js) — its client_build entry; without it, the server's server_build")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("verify: <artefact> <sig>")
	}
	k, err := keyArg(*vkey)
	if err != nil {
		return err
	}
	if k == "" {
		return errors.New("-vkey is required: pin the key published outside the download")
	}
	v, err := release.NewVerifier(k)
	if err != nil {
		return err
	}
	path, sigPath := fs.Arg(0), fs.Arg(1)
	sig, err := os.ReadFile(sigPath) // #nosec G304 -- the reader's own download
	if err != nil {
		return fmt.Errorf("read the signature: %w", err)
	}
	st, err := release.Verify(v, path, sig)
	if err != nil {
		return fmt.Errorf("FAIL %s: %w", path, err)
	}
	_, _ = fmt.Fprintf(out, "OK %s: signed as %s, sha256 %s, %d bytes, by key %08x\n", path, st.Name, st.SHA256, st.Size, v.KeyHash())
	if *logURL == "" {
		return nil
	}
	lk, err := keyArg(*logVkey)
	if err != nil {
		return err
	}
	if lk == "" {
		return errors.New("-log needs -log-vkey: the transparency log's pinned verifier key")
	}
	lv, err := translog.NewVerifier(lk)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := &translog.Client{BaseURL: strings.TrimSuffix(*logURL, "/") + "/", Verifier: lv}
	cp, _, err := c.Checkpoint(ctx)
	if err != nil {
		return fmt.Errorf("log checkpoint: %w", err)
	}
	found, err := translog.FindBuild(ctx, c, cp, st.SHA256, *client)
	if err != nil {
		return fmt.Errorf("FAIL %s: %w", path, err)
	}
	_, _ = fmt.Fprintf(out, "OK %s: %s\n", path, found)
	return nil
}
