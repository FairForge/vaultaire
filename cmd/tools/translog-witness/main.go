// Command translog-witness is the transparency log's witness (keystone
// K4b/K4c-2, docs/TRANSPARENCY.md § The witness): run it OFF the box. It
// trusts nothing of ours but the pinned verifier key of the log, and
// cosigns with its OWN cosignature/v1 key (c2sp.org/tlog-cosignature,
// ML-DSA-44, timestamped).
//
//	translog-keygen -cosignature -name witness.<host> -out ~/.translog-witness/witness.key > witness.vkey
//	translog-witness -vkey @translog.vkey -key @~/.translog-witness/witness.key          # one pull pass (cron)
//	translog-witness serve -listen 127.0.0.1:7380 -vkey @translog.vkey -key @…           # c2sp.org/tlog-witness
//
// A pull pass (internal/translog/witness Pass): the newest checkpoint's
// signature, its consistency with the checkpoint accepted last, a byte-exact
// mirror of every entry bundle and full level-0 tile under -dir/cache (each
// entry proven against the checkpoint), then the checkpoint cosigned in
// -dir/cosigned (+ one line per new note in -dir/cosigned.history). A
// checkpoint already cosigned is not signed again.
//
// serve answers POST /add-checkpoint (the log pushes each new checkpoint
// with a consistency proof from the size the witness holds) and GET
// /<sha256(origin)>/checkpoint (the monitor view). It does not mirror: run
// the pull pass from cron beside it to keep the byte cache translog-restore
// reads. Both share -dir/checkpoint under one lock (-dir/lock). Any failure
// of a pull pass exits 1 and the accepted checkpoint stays as it was.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/mod/sumdb/note"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/witness"
)

type options struct {
	url, vkey, dir, key string
	once                bool
	interval            time.Duration
	listen              string
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if err := serveMain(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "FAIL:", err)
			os.Exit(1)
		}
		return
	}
	home, _ := os.UserHomeDir()
	var o options
	flag.StringVar(&o.url, "url", "https://trust.stored.cloud/translog", "where checkpoint and tile/… are served (the trust host)")
	flag.StringVar(&o.vkey, "vkey", "", "the pinned verifier key of the LOG, or @file")
	flag.StringVar(&o.dir, "dir", filepath.Join(home, ".translog-witness"), "the witness's state, cosigned notes and byte cache")
	flag.StringVar(&o.key, "key", "", "the WITNESS's own cosignature/v1 signer key (translog-keygen -cosignature -name …), or @file")
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

// keys reads the log's pinned verifier key and the witness's cosigner key.
func keys(o options) (note.Verifier, note.Signer, error) {
	vkey, err := keyArg(o.vkey, "verifier key")
	if err != nil {
		return nil, nil, err
	}
	if vkey == "" {
		return nil, nil, errors.New("-vkey is required: pin the log key published outside our infrastructure")
	}
	v, err := translog.NewVerifier(vkey)
	if err != nil {
		return nil, nil, err
	}
	skey, err := keyArg(o.key, "witness key")
	if err != nil {
		return nil, nil, err
	}
	if skey == "" {
		return nil, nil, errors.New("-key is required: the witness cosigns with its own key (translog-keygen -cosignature -name witness.<host> -out …)")
	}
	s, err := translog.NewCosigner(skey, nil)
	if err != nil {
		return nil, nil, err // never quotes the key
	}
	return v, s, nil
}

func run(ctx context.Context, out io.Writer, o options) error {
	v, s, err := keys(o)
	if err != nil {
		return err
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
	_, _ = fmt.Fprintf(out, "%s %s size %d root %x — signature OK, %s, %d bundle(s) mirrored, cosigned by %s (%08x)\n",
		time.Now().UTC().Format(time.RFC3339), translog.Origin, res.Size, res.Root[:], from, res.Bundles, w.Signer.Name(), w.Signer.KeyHash())
	return nil
}

// serveMain is `translog-witness serve`: the c2sp.org/tlog-witness HTTP
// side on -listen until interrupted.
func serveMain(args []string) error {
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var o options
	fs.StringVar(&o.listen, "listen", "127.0.0.1:7380", "address to serve add-checkpoint on (put TLS — a reverse proxy, a bastion — in front of it)")
	fs.StringVar(&o.vkey, "vkey", "", "the pinned verifier key of the LOG, or @file")
	fs.StringVar(&o.dir, "dir", filepath.Join(home, ".translog-witness"), "the witness's state and cosigned notes (shared with the pull pass)")
	fs.StringVar(&o.key, "key", "", "the WITNESS's own cosignature/v1 signer key, or @file")
	floor := fs.Duration("refresh-floor", witness.DefaultRefreshFloor, "a request for the checkpoint already cosigned is answered with that cosignature, signing and writing nothing, while it is younger than this; keep it below the log's 6 h refresh (RP46-1)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *floor <= 0 || *floor >= 6*time.Hour {
		return fmt.Errorf("-refresh-floor %s: want more than 0 and less than the log's 6 h refresh", *floor)
	}
	v, s, err := keys(o)
	if err != nil {
		return err
	}
	srv := &witness.Server{Dir: o.dir, Log: v, Signer: s, RefreshFloor: *floor,
		Logf: func(format string, a ...any) {
			fmt.Fprintln(os.Stderr, time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, a...))
		}}
	hs := &http.Server{Addr: o.listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	fmt.Fprintf(os.Stderr, "translog-witness serve: %s for %s, cosigning as %s (%08x), state in %s\n", o.listen, v.Name(), s.Name(), s.KeyHash(), o.dir)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
