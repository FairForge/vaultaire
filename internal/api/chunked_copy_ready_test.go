package api

// RP13-4 (WP-P12 post-merge review): the chunked CopyObject took its
// references with `SELECT increment_chunk_ref($1, $2)` — no `state = 'ready'`
// filter and no row count. A source chunk whose row is pending (a belt: a
// manifest names only ready rows) or gone was referenced silently and the
// copy answered 200 with a manifest naming a chunk no ready row holds. It
// takes them through IncrementRefTx now: ready rows only, one row each, or
// the install fails whole. (A row a manifest names cannot vanish: the
// tenant_chunk_refs foreign key refuses the delete — so pending is the one
// shape a manifest's chunk can be in that is not ready.)

import (
	"crypto/rand"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChunkedCopy_ASourceChunkThatIsNotReadyIsNeverReferenced(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ string
	}{
		{"pending", `UPDATE global_content_index SET state = 'pending', pending_since = NOW(), ref_count = 0 WHERE dedup_scope = $1 AND plaintext_hash = $2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange: a chunked source; one of its chunks' rows not ready.
			f := setupChunkedCopyFixture(t)
			content := make([]byte, 3<<20)
			_, err := rand.Read(content)
			require.NoError(t, err)
			f.seedChunked(t, "src.bin", content)
			var scope, hash string
			require.NoError(t, f.db.QueryRow(`SELECT dedup_scope, plaintext_hash FROM tenant_chunk_refs
				WHERE tenant_id = $1 AND bucket_name = 'test-bucket' ORDER BY chunk_index LIMIT 1`, f.tenantID).Scan(&scope, &hash))
			_, err = f.db.Exec(tc.break_, scope, hash)
			require.NoError(t, err)

			// Act
			w := f.copyObject(t, "src.bin", "dest-bucket", "dst.bin")

			// Assert: refused, nothing installed, no reference taken.
			assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
			var n int
			require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = 'dest-bucket'`, f.tenantID).Scan(&n))
			assert.Zero(t, n, "no destination row")
			require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id = $1 AND bucket_name = 'dest-bucket'`, f.tenantID).Scan(&n))
			assert.Zero(t, n, "no destination manifest")
			var refs int
			require.NoError(t, f.db.QueryRow(`SELECT ref_count FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`, scope, hash).Scan(&refs))
			assert.Zero(t, refs, "a pending row holds no reference")
		})
	}
}
