// catalogue-seed adds one PUBLIC artefact to the catalogue (WP-EDGE-4 PR D,
// Stage 1a Prompt 48; plan 10.3): the operator runs it, never the product.
// It cuts the file with the frozen chunker (crypto.DefaultChunker — the
// origin's and the Worker's boundaries), writes the artefact row and one
// public_chunks row per chunk (migration 114), and for every chunk the
// `_public` scope does not hold yet stores the chunk's bytes — plaintext,
// public content — at its one address `_chunks/_public/<plain hash>` on the
// backend named by -backend (through an S3 endpoint: t-_global/_global/ is
// the fixed-bucket drivers' chunk prefix) and its ready, unencrypted
// global_content_index row in the `_public` scope.
//
//	DATABASE_URL=postgres://… AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=… \
//	  catalogue-seed -file noble-server-cloudimg-amd64.img -name "Ubuntu 24.04 cloud image" \
//	    -licence "Ubuntu (Canonical) licence" -source https://cloud-images.ubuntu.com/… \
//	    -backend idrive -endpoint https://s3.us-central-1.idrivee2.com -region us-central-1 -bucket vaultaire
//
// -dry-run cuts and counts only. Seeding a file already seeded adds nothing.
// Contract line 5: `_public` holds only what an operator seeded here.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	_ "github.com/lib/pq"

	"github.com/FairForge/vaultaire/internal/crypto"
)

// artefactMeta is what the operator says about the file.
type artefactMeta struct {
	Name, Source, Licence, AddedBy string
}

// seedResult is one run.
type seedResult struct {
	ID     int64  `json:"id"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Chunks int    `json:"chunks"`
	Stored int    `json:"stored"`
}

// putFunc stores one chunk blob at its full key.
type putFunc func(ctx context.Context, key string, data []byte) error

func main() {
	file := flag.String("file", "", "the artefact (a local file)")
	name := flag.String("name", "", "its name")
	licence := flag.String("licence", "", "its licence")
	source := flag.String("source", "", "where it came from (URL)")
	backend := flag.String("backend", "", "the driver name the blobs are recorded on (idrive, lyve, …)")
	endpoint := flag.String("endpoint", "", "that backend's S3 endpoint")
	region := flag.String("region", "us-east-1", "its region")
	bucket := flag.String("bucket", "", "its bucket (the driver's fixed bucket)")
	dryRun := flag.Bool("dry-run", false, "cut and count only")
	flag.Parse()
	if *file == "" || *name == "" || *licence == "" || *source == "" || (!*dryRun && (*backend == "" || *endpoint == "" || *bucket == "")) {
		flag.Usage()
		os.Exit(2)
	}
	ctx := context.Background()
	f, err := os.Open(*file)
	if err != nil {
		fail(err)
	}
	defer func() { _ = f.Close() }()
	meta := artefactMeta{Name: *name, Source: *source, Licence: *licence, AddedBy: operator()}
	if *dryRun {
		n, chunks, err := cut(f, func(crypto.Chunk) error { return nil })
		if err != nil {
			fail(err)
		}
		fmt.Printf("dry run: %d bytes, %d chunks\n", n, chunks)
		return
	}
	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		fail(err)
	}
	defer func() { _ = db.Close() }()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(*region))
	if err != nil {
		fail(err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) { o.BaseEndpoint = aws.String(*endpoint); o.UsePathStyle = true })
	put := func(ctx context.Context, key string, data []byte) error {
		_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: aws.String(key), Body: bytes.NewReader(data),
			ContentLength: aws.Int64(int64(len(data)))})
		return err
	}
	res, err := seed(ctx, db, f, meta, *backend, put)
	if err != nil {
		fail(err)
	}
	fmt.Printf("seeded artefact %d (%s): %d bytes, %d chunks, %d stored new\n", res.ID, res.SHA256, res.Size, res.Chunks, res.Stored)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "catalogue-seed:", err)
	os.Exit(1)
}

func operator() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "operator"
}

// cut runs the frozen chunker over r, calling each with every chunk in order.
func cut(r io.Reader, each func(crypto.Chunk) error) (int64, int, error) {
	ch, err := crypto.DefaultChunker()
	if err != nil {
		return 0, 0, err
	}
	results, err := ch.Chunk(r)
	if err != nil {
		return 0, 0, err
	}
	var n int64
	count := 0
	var firstErr error
	for res := range results {
		if firstErr != nil {
			continue // drain
		}
		if res.Err != nil {
			firstErr = res.Err
			continue
		}
		n += int64(res.Chunk.Size)
		count++
		firstErr = each(res.Chunk)
	}
	return n, count, firstErr
}

// seed adds the artefact read from r (see the file comment).
func seed(ctx context.Context, db *sql.DB, r io.Reader, meta artefactMeta, backend string, put putFunc) (seedResult, error) {
	var res seedResult
	whole := sha256.New()
	type piece struct {
		hash string
		size int
	}
	var pieces []piece
	n, count, err := cut(io.TeeReader(r, whole), func(c crypto.Chunk) error {
		var stored bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2 AND state = 'ready')`,
			crypto.PublicDedupScope, c.Hash).Scan(&stored); err != nil {
			return err
		}
		pieces = append(pieces, piece{hash: c.Hash, size: c.Size})
		if stored {
			return nil
		}
		key := "_chunks/" + crypto.PublicDedupScope + "/" + c.Hash
		if err := put(ctx, "t-_global/_global/"+key, c.Data); err != nil {
			return fmt.Errorf("store chunk %s: %w", c.Hash[:12], err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO global_content_index (dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes, encrypted, ref_count, state)
			VALUES ($1, $2, $3, $4, $5, FALSE, 0, 'ready')
			ON CONFLICT (dedup_scope, plaintext_hash) DO NOTHING`,
			crypto.PublicDedupScope, c.Hash, backend, key, c.Size); err != nil {
			return fmt.Errorf("index chunk %s: %w", c.Hash[:12], err)
		}
		res.Stored++
		return nil
	})
	if err != nil {
		return res, err
	}
	res.Size, res.Chunks, res.SHA256 = n, count, hex.EncodeToString(whole.Sum(nil))

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()
	err = tx.QueryRowContext(ctx, `
		INSERT INTO public_artefacts (name, source_url, licence, added_by, sha256, size_bytes) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (sha256) DO NOTHING RETURNING id`, meta.Name, meta.Source, meta.Licence, meta.AddedBy, res.SHA256, res.Size).Scan(&res.ID)
	if errors.Is(err, sql.ErrNoRows) {
		// Seeded before: its rows stand.
		if err := tx.QueryRowContext(ctx, `SELECT id FROM public_artefacts WHERE sha256 = $1`, res.SHA256).Scan(&res.ID); err != nil {
			return res, err
		}
		return res, tx.Commit()
	}
	if err != nil {
		return res, err
	}
	for i, p := range pieces {
		if _, err := tx.ExecContext(ctx, `INSERT INTO public_chunks (artefact_id, chunk_index, plain_hash, size_bytes) VALUES ($1, $2, $3, $4)`,
			res.ID, i, p.hash, p.size); err != nil {
			return res, err
		}
	}
	return res, tx.Commit()
}
