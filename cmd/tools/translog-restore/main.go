// Command translog-restore puts back, byte for byte, the transparency-log
// entries a database restore lost (keystone K4b, RP38-6; the recipe is
// docs/TRANSPARENCY.md § After a database restore, case 2). It is the ONLY
// way to keep the log's origin after a restore behind the high-water file:
// the tree at the file's size must have the file's root, so the entries
// M … N-1 must be the very bytes the key signed — read from a witness's
// cache (translog-witness -dir), never re-encoded.
//
//	sudo systemctl stop 'vaultaire@*'      # no slot appends meanwhile
//	DATABASE_URL=postgres://… translog-restore -from ~/.translog-witness -highwater /opt/vaultaire/data/translog/highwater
//
// It reads the database's size M and the file's checkpoint N, loads
// entries M … N-1 from the cached entry bundles (full, and the partial one
// at the witness's newest checkpoint), and appends them in one transaction
// that commits only when the recomputed root at N is the file's
// (logstore.ReAppend). It refuses — nothing written — when M ≥ N, when a
// bundle it needs is missing (named), or when the root differs. It never
// signs: the checkpoint job does, once the slots are started again.
// Operator tool: never linked into cmd/vaultaire.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	_ "github.com/lib/pq" // the postgres driver

	"github.com/FairForge/vaultaire/internal/translog/logstore"
	"github.com/FairForge/vaultaire/internal/translog/witness"
)

type options struct {
	from, highwater string
}

func main() {
	var o options
	flag.StringVar(&o.from, "from", "", "a translog-witness directory (its cache/ holds the entry bundles)")
	flag.StringVar(&o.highwater, "highwater", "", "the high-water file: the checkpoint the restored log must reach")
	flag.Parse()
	if err := main1(o); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

func main1(o options) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return run(ctx, os.Stdout, o, db)
}

func run(ctx context.Context, out io.Writer, o options, db *sql.DB) error {
	if o.from == "" || o.highwater == "" {
		return errors.New("-from <witness dir> and -highwater <file> are required")
	}
	hw, ok, err := logstore.ReadHighwater(o.highwater)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no high-water file at %s", o.highwater)
	}
	s := logstore.New(db, nil)
	m, err := s.Size(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "database: %d entries; high-water: %s size %d root %x\n", m, hw.Origin, hw.Size, hw.Root[:])
	if m >= hw.Size {
		return fmt.Errorf("the database already holds %d entries (the high-water is %d): nothing to re-append — start the slots and let the checkpoint job prove it (case 1)", m, hw.Size)
	}
	bodies, err := witness.OpenCache(o.from).Entries(m, hw.Size)
	if err != nil {
		return fmt.Errorf("entries %d…%d from the witness cache: %w", m, hw.Size-1, err)
	}
	if err := s.ReAppend(ctx, bodies, hw); err != nil {
		return err
	}
	root, err := s.Root(ctx, hw.Size)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "re-appended entries %d…%d: the root at %d is %x = the high-water's\n", m, hw.Size-1, hw.Size, root[:])
	_, _ = fmt.Fprintln(out, "next: start the slots, wait for an ok translog_checkpoint run, then append the log_restored ceremony with new_origin false")
	return nil
}
