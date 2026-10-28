package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"sync"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/testutil"
)

func TestSeed_RowsTheIndexAndOneBlobPerChunk(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable (%v)", err)
	}
	body := crypto.GoldenBody(91, 6<<20)
	whole := sha256.Sum256(body)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM global_content_index WHERE dedup_scope = '_public' AND plaintext_hash IN (SELECT plain_hash FROM public_chunks c JOIN public_artefacts a ON a.id = c.artefact_id WHERE a.sha256 = $1)`, hex.EncodeToString(whole[:]))
		_, _ = db.Exec(`DELETE FROM public_artefacts WHERE sha256 = $1`, hex.EncodeToString(whole[:]))
	})
	var mu sync.Mutex
	blobs := map[string][]byte{}
	put := func(_ context.Context, key string, data []byte) error {
		mu.Lock()
		defer mu.Unlock()
		blobs[key] = bytes.Clone(data)
		return nil
	}
	meta := artefactMeta{Name: "golden-91", Source: "https://example.test/golden", Licence: "CC0-1.0", AddedBy: "test"}
	res, err := seed(context.Background(), db, bytes.NewReader(body), meta, "idrive", put)
	require.NoError(t, err)
	assert.Equal(t, int64(len(body)), res.Size)
	assert.Equal(t, hex.EncodeToString(whole[:]), res.SHA256)
	require.Greater(t, res.Chunks, 1)
	assert.Equal(t, res.Chunks, res.Stored, "every chunk new")
	assert.Len(t, blobs, res.Chunks)

	var chunks, ready int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM public_chunks WHERE artefact_id = $1`, res.ID).Scan(&chunks))
	assert.Equal(t, res.Chunks, chunks)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM global_content_index g JOIN public_chunks c ON g.plaintext_hash = c.plain_hash
		WHERE c.artefact_id = $1 AND g.dedup_scope = '_public' AND g.state = 'ready' AND NOT g.encrypted AND g.backend_id = 'idrive'
		  AND g.storage_key = '_chunks/_public/' || c.plain_hash`, res.ID).Scan(&ready))
	assert.Equal(t, res.Chunks, ready, "one ready, plaintext _public index row per chunk at its one address")
	for k, b := range blobs {
		s := sha256.Sum256(b)
		assert.Equal(t, "t-_global/_global/_chunks/_public/"+hex.EncodeToString(s[:]), k, "the blob's address names its plain hash")
	}

	// Seeding the same file again adds nothing.
	res2, err := seed(context.Background(), db, bytes.NewReader(body), meta, "idrive", put)
	require.NoError(t, err)
	assert.Equal(t, res.ID, res2.ID)
	assert.Zero(t, res2.Stored)
}
