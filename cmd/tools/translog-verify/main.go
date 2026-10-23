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
//
// With -witness (K4c-2, c2sp.org/tlog-cosignature), repeatable, each a
// witness's cosignature/v1 verifier key or @file, the checkpoint is accepted
// only with -min of them cosigned (default 1): a line that claims one of
// those keys and does not verify — forged, or copied from another
// checkpoint — fails the run, and a cosignature older than -max-age (or
// dated in the future) does not count.
//
//	translog-verify -vkey @translog.vkey -witness @witness.vkey -min 1
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
	witnesses        []string
	min              int
	maxAge           time.Duration
	now              func() time.Time
}

// keyList is a repeatable -witness flag.
type keyList []string

func (k *keyList) String() string     { return strings.Join(*k, ",") }
func (k *keyList) Set(v string) error { *k = append(*k, v); return nil }

func main() {
	home, _ := os.UserHomeDir()
	var o options
	flag.StringVar(&o.url, "url", "https://trust.stored.cloud/translog", "where checkpoint and tile/… are served (the trust host)")
	flag.StringVar(&o.vkey, "vkey", "", "the pinned verifier key, or @file")
	flag.StringVar(&o.state, "state", filepath.Join(home, ".translog-verify", "checkpoint"), "the checkpoint verified last time")
	flag.Int64Var(&o.entry, "entry", -1, "also prove this entry is in the log and print it")
	var ws keyList
	flag.Var(&ws, "witness", "a witness's cosignature/v1 verifier key, or @file (repeatable)")
	flag.IntVar(&o.min, "min", -1, "how many of the -witness keys must have cosigned (default: 1 with any -witness, else 0)")
	flag.DurationVar(&o.maxAge, "max-age", 24*time.Hour, "a cosignature older than this does not count (0 = no limit)")
	flag.Parse()
	o.witnesses = ws
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
	policy, err := witnessPolicy(o)
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
	if len(policy.Witnesses) > 0 || policy.Min > 0 {
		_, fresh, skipped, err := policy.Verify(msg, v)
		for _, s := range skipped {
			_, _ = fmt.Fprintf(out, "cosignature not counted: %s\n", s)
		}
		if err != nil {
			return fmt.Errorf("cosignatures: %w", err)
		}
		for _, f := range fresh {
			_, _ = fmt.Fprintf(out, "cosignature: %s at %s — OK\n", f.KeyID(), f.Time.UTC().Format(time.RFC3339))
		}
		_, _ = fmt.Fprintf(out, "cosignatures: %d of the %d required\n", len(fresh), policy.Min)
	}

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

// witnessPolicy builds the -witness / -min / -max-age policy.
func witnessPolicy(o options) (translog.WitnessPolicy, error) {
	p := translog.WitnessPolicy{Min: o.min, MaxAge: o.maxAge, Now: o.now}
	for _, w := range o.witnesses {
		k := w
		if strings.HasPrefix(k, "@") {
			b, err := os.ReadFile(k[1:])
			if err != nil {
				return p, fmt.Errorf("read the witness key: %w", err)
			}
			k = string(b)
		}
		wv, err := translog.NewCosignatureVerifier(k)
		if err != nil {
			return p, fmt.Errorf("-witness: %w", err)
		}
		p.Witnesses = append(p.Witnesses, wv)
	}
	if p.Min < 0 {
		p.Min = 0
		if len(p.Witnesses) > 0 {
			p.Min = 1
		}
	}
	if p.Min > len(p.Witnesses) {
		return p, fmt.Errorf("-min %d: only %d -witness key(s) given", p.Min, len(p.Witnesses))
	}
	return p, nil
}
