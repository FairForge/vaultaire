// Command translog-witness is the transparency log's witness (keystone
// K4b, docs/TRANSPARENCY.md § The witness): run it OFF the box, from a cron,
// against the public read side. It trusts nothing of ours but the pinned
// verifier key of the log.
//
//	translog-keygen -name witness.<host> -out ~/.translog-witness/witness.key > witness.vkey
//	translog-witness -vkey @translog.vkey -key @~/.translog-witness/witness.key
//
// One pass (internal/translog/witness): the newest checkpoint's signature,
// its consistency with the checkpoint accepted last, a byte-exact mirror of
// every entry bundle and full level-0 tile under -dir/cache (each entry
// proven against the checkpoint), then the checkpoint co-signed with the
// witness's own key in -dir/cosigned (+ one line per new note in
// -dir/cosigned.history). Any failure exits 1 and the accepted checkpoint
// stays as it was. With the cache, entries a database restore lost can be
// put back byte for byte (translog-restore -from -dir).
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

	"golang.org/x/mod/sumdb/note"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/witness"
)

type options struct {
	url, vkey, dir, key string
	once                bool
	interval            time.Duration
}

func main() {
	home, _ := os.UserHomeDir()
	var o options
	flag.StringVar(&o.url, "url", "https://trust.stored.cloud/translog", "where checkpoint and tile/… are served (the trust host)")
	flag.StringVar(&o.vkey, "vkey", "", "the pinned verifier key of the LOG, or @file")
	flag.StringVar(&o.dir, "dir", filepath.Join(home, ".translog-witness"), "the witness's state, cosigned notes and byte cache")
	flag.StringVar(&o.key, "key", "", "the WITNESS's own signer key (translog-keygen -name …), or @file")
	flag.BoolVar(&o.once, "once", true, "one pass and exit (for cron); false = a pass every -interval")
	flag.DurationVar(&o.interval, "interval", 10*time.Minute, "time between passes with -once=false")
	flag.Parse()
	if err := run(context.Background(), os.Stdout, o); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

// keyArg is a key given inline or as @file.
func keyArg(v, what string) (string, error) {
	if strings.HasPrefix(v, "@") {
		b, err := os.ReadFile(v[1:]) // #nosec G304 -- the operator's own key file
		if err != nil {
			return "", fmt.Errorf("read the %s: %w", what, err)
		}
		v = string(b)
	}
	return strings.TrimSpace(v), nil
}

func run(ctx context.Context, out io.Writer, o options) error {
	vkey, err := keyArg(o.vkey, "verifier key")
	if err != nil {
		return err
	}
	if vkey == "" {
		return errors.New("-vkey is required: pin the log key published outside our infrastructure")
	}
	v, err := translog.NewVerifier(vkey)
	if err != nil {
		return err
	}
	skey, err := keyArg(o.key, "witness key")
	if err != nil {
		return err
	}
	if skey == "" {
		return errors.New("-key is required: the witness co-signs with its own key (translog-keygen -name witness.<host> -out …)")
	}
	s, err := note.NewSigner(skey)
	if err != nil {
		return errors.New("-key is not a note signer key") // never quote the key
	}
	w := &witness.Witness{
		Dir:    o.dir,
		Client: &translog.Client{BaseURL: strings.TrimSuffix(o.url, "/") + "/", Verifier: v},
		Signer: s,
	}
	for {
		if err := pass(ctx, out, w); err != nil || o.once {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.interval):
		}
	}
}

func pass(ctx context.Context, out io.Writer, w *witness.Witness) error {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	res, err := w.Pass(pctx)
	if err != nil {
		return err
	}
	from := "first pass"
	if res.PrevSize >= 0 {
		from = fmt.Sprintf("consistent with %d", res.PrevSize)
	}
	_, _ = fmt.Fprintf(out, "%s %s size %d root %x — signature OK, %s, %d bundle(s) mirrored, co-signed by %s (%08x)\n",
		time.Now().UTC().Format(time.RFC3339), translog.Origin, res.Size, res.Root[:], from, res.Bundles, w.Signer.Name(), w.Signer.KeyHash())
	return nil
}
