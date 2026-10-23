// Command translog-verify checks the transparency log (keystone K4,
// docs/TRANSPARENCY.md) from outside: the newest checkpoint's signature
// against a PINNED verifier key, the consistency of that checkpoint with the
// one this tool verified last time (saved in -state — the witness's role
// until an external witness exists, K4b), and, with -entry, the inclusion
// of one entry, printed.
//
//	translog-verify -url https://trust.stored.cloud/translog -vkey 'stored.cloud/translog/v1+…' -entry 0
//
// -vkey takes the key or @file. The saved checkpoint moves forward only
// after every check passed; a failure exits 1 and leaves it as it was.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/translog"
)

type options struct {
	url, vkey, state string
	entry            int64
}

func main() {
	home, _ := os.UserHomeDir()
	var o options
	flag.StringVar(&o.url, "url", "https://trust.stored.cloud/translog", "where checkpoint and tile/… are served (the trust host)")
	flag.StringVar(&o.vkey, "vkey", "", "the pinned verifier key, or @file")
	flag.StringVar(&o.state, "state", filepath.Join(home, ".translog-verify", "checkpoint"), "the checkpoint verified last time")
	flag.Int64Var(&o.entry, "entry", -1, "also prove this entry is in the log and print it")
	flag.Parse()
	if err := run(os.Stdout, o); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

func run(out io.Writer, o options) error {
	vkey := o.vkey
	if strings.HasPrefix(vkey, "@") {
		b, err := os.ReadFile(vkey[1:])
		if err != nil {
			return fmt.Errorf("read the verifier key: %w", err)
		}
		vkey = strings.TrimSpace(string(b))
	}
	if vkey == "" {
		return errors.New("-vkey is required: pin the key published outside our infrastructure")
	}
	v, err := translog.NewVerifier(vkey)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := &translog.Client{BaseURL: strings.TrimSuffix(o.url, "/") + "/", Verifier: v}

	cp, msg, err := c.Checkpoint(ctx)
	if err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	_, _ = fmt.Fprintf(out, "checkpoint: %s size %d root %x — signature OK\n", cp.Origin, cp.Size, cp.Root[:])

	prev, err := os.ReadFile(o.state)
	switch {
	case errors.Is(err, os.ErrNotExist):
		_, _ = fmt.Fprintln(out, "consistency: no saved checkpoint (first run) — this one is saved")
	case err != nil:
		return fmt.Errorf("read %s: %w", o.state, err)
	default:
		old, err := translog.OpenCheckpoint(prev, v)
		if err != nil {
			return fmt.Errorf("the saved checkpoint does not open with this key: %w", err)
		}
		if err := c.VerifyConsistency(ctx, old, cp); err != nil {
			return fmt.Errorf("consistency with the saved checkpoint (size %d): %w", old.Size, err)
		}
		_, _ = fmt.Fprintf(out, "consistency: size %d → %d OK\n", old.Size, cp.Size)
	}

	if o.entry >= 0 {
		body, err := c.VerifyInclusion(ctx, cp, o.entry)
		if err != nil {
			return fmt.Errorf("inclusion of entry %d: %w", o.entry, err)
		}
		_, _ = fmt.Fprintf(out, "entry %d: included OK\n%s\n", o.entry, body)
	}

	if err := os.MkdirAll(filepath.Dir(o.state), 0o700); err != nil {
		return err
	}
	tmp := o.state + ".tmp"
	if err := os.WriteFile(tmp, msg, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, o.state)
}
