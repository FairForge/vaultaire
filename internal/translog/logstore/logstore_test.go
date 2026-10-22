package logstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/FairForge/vaultaire/internal/translog"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/lib/pq"
)

// The log is global (no tenant): other packages and earlier runs append to
// the same tables on the shared test database. Tests check their own
// entries and the whole log's invariants, never an absolute size.

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	return db
}

// uniqueBuild is a server_build no other run has appended.
func uniqueBuild(t *testing.T) translog.ServerBuild {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return translog.ServerBuild{GitSHA: "dev", BinarySHA256: hex.EncodeToString(b), BuildTime: "unknown", GoVersion: "go1.26.0"}
}

func ceremony(t *testing.T, statement string) translog.KeyCeremony {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return translog.KeyCeremony{Ceremony: translog.CeremonyTranslogKeyInstall, KeyFingerprint: "sha256:" + hex.EncodeToString(b),
		Participants: 1, Statement: statement}
}

// recomputeRoot rebuilds the tree of the first size entries from their
// bodies alone, in a fresh in-memory log.
func recomputeRoot(t *testing.T, db *sql.DB, size int64) translog.Hash {
	t.Helper()
	rows, err := db.Query(`SELECT idx, body FROM translog_entries WHERE idx < $1 ORDER BY idx`, size)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	m := &translog.Memory{}
	var want int64
	for rows.Next() {
		var idx int64
		var body []byte
		require.NoError(t, rows.Scan(&idx, &body))
		require.Equal(t, want, idx, "entries are contiguous")
		want++
		_, err := m.Append(body)
		require.NoError(t, err)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, size, want)
	root, err := translog.Root(size, m)
	require.NoError(t, err)
	return root
}

func TestTranslog_ConcurrentAppends_Contiguous(t *testing.T) {
	db := openDB(t)
	s := New(db, nil)
	ctx := context.Background()

	const n = 24
	var wg sync.WaitGroup
	idxs := make([]int64, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			idxs[i], _, errs[i] = s.Append(ctx, ceremony(t, fmt.Sprintf("concurrent append %d", i)))
		}(i)
	}
	wg.Wait()
	seen := map[int64]bool{}
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		assert.False(t, seen[idxs[i]], "index %d handed out twice", idxs[i])
		seen[idxs[i]] = true
	}

	size, err := s.Size(ctx)
	require.NoError(t, err)
	for _, x := range idxs {
		assert.Less(t, x, size)
	}
	// The whole table has no gap, its stored hashes are exactly the count
	// tlog stores for that size, and the root from the stored hashes is the
	// root recomputed from the bodies alone.
	var hashes int64
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM translog_hashes WHERE idx < $1`, translog.StoredHashCount(size)).Scan(&hashes))
	assert.Equal(t, translog.StoredHashCount(size), hashes)
	stored, err := s.Root(ctx, size)
	require.NoError(t, err)
	assert.Equal(t, recomputeRoot(t, db, size), stored)

	// Each of our entries is provably in the tree.
	for i, x := range idxs {
		var body []byte
		require.NoError(t, db.QueryRow(`SELECT body FROM translog_entries WHERE idx = $1`, x).Scan(&body))
		require.Contains(t, string(body), fmt.Sprintf("concurrent append %d", i))
		p, err := s.InclusionProof(ctx, x, size)
		require.NoError(t, err)
		require.NoError(t, translog.VerifyInclusion(p, x, size, translog.LeafHash(body), stored))
	}
}

func TestTranslog_AppendDedupKey(t *testing.T) {
	db := openDB(t)
	s := New(db, nil)
	ctx := context.Background()
	b := uniqueBuild(t)
	first, appended, err := s.Append(ctx, b)
	require.NoError(t, err)
	assert.True(t, appended)
	again, appended, err := s.Append(ctx, b)
	require.NoError(t, err)
	assert.False(t, appended, "the same build twice is one entry")
	assert.Equal(t, first, again)
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM translog_entries WHERE kind = 'server_build' AND dedup_key = $1`, b.DedupKey()).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestTranslog_AppendRefusedEntryCountsAFailure(t *testing.T) {
	db := openDB(t)
	s := New(db, nil)
	before := promtest.ToFloat64(appendFailures.WithLabelValues(translog.KindKeyCeremony))
	_, _, err := s.Append(context.Background(), ceremony(t, "for alice@example.com"))
	require.Error(t, err)
	assert.Equal(t, before+1, promtest.ToFloat64(appendFailures.WithLabelValues(translog.KindKeyCeremony)))
}

func TestTranslog_TablesAreAppendOnly(t *testing.T) {
	db := openDB(t)
	s := New(db, nil)
	ctx := context.Background()
	idx, _, err := s.Append(ctx, ceremony(t, "append-only check"))
	require.NoError(t, err)
	for _, q := range []string{
		`UPDATE translog_entries SET body = 'x' WHERE idx = $1`,
		`DELETE FROM translog_entries WHERE idx = $1`,
		`DELETE FROM translog_hashes WHERE idx = $1`,
	} {
		_, err := db.ExecContext(ctx, q, idx)
		require.Error(t, err, q)
		assert.Contains(t, err.Error(), "append-only", q)
	}
	_, err = db.ExecContext(ctx, `TRUNCATE translog_checkpoints`)
	require.Error(t, err)
}

func TestTranslog_CheckpointSignsWhenTheTreeGrew(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	skey, vkey, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := translog.NewSigner(skey)
	require.NoError(t, err)
	verifier, err := translog.NewVerifier(vkey)
	require.NoError(t, err)
	s := New(db, signer)

	_, _, err = s.Append(ctx, ceremony(t, "checkpoint test"))
	require.NoError(t, err)
	res, err := s.Checkpoint(ctx)
	require.NoError(t, err)
	// Another package's run may have signed this size first (shared DB):
	// either way the newest checkpoint is at least our size and opens with
	// a key — ours when we signed it.
	size, err := s.Size(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, size, res.Size)
	if res.Signed {
		latest, ok, err := s.LatestCheckpoint(ctx)
		require.NoError(t, err)
		require.True(t, ok)
		cp, err := translog.OpenCheckpoint([]byte(latest.Note), verifier)
		require.NoError(t, err)
		assert.Equal(t, res.Size, cp.Size)
		assert.Equal(t, recomputeRoot(t, db, cp.Size), cp.Root)
	}

	// Nothing appended since: the next run signs nothing.
	s2 := New(db, signer)
	_, err = s2.Checkpoint(ctx)
	require.NoError(t, err)
	res3, err := s2.Checkpoint(ctx)
	require.NoError(t, err)
	if size2, _ := s2.Size(ctx); size2 == res3.CheckpointSize {
		assert.False(t, res3.Signed, "an unchanged tree is not signed again")
	}
}

func TestTranslog_CheckpointWithoutAKeySignsNothing(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	s := New(db, nil)
	_, _, err := s.Append(ctx, ceremony(t, "no key"))
	require.NoError(t, err)
	res, err := s.Checkpoint(ctx)
	require.NoError(t, err)
	assert.False(t, res.Signed)
	assert.Equal(t, "no signing key", res.Reason)
	assert.Greater(t, res.Size, int64(0))
}

// A log whose stored history no longer extends its newest checkpoint must
// not get a new signature: the job refuses and says so.
func TestTranslog_CheckpointRefusesARewrittenHistory(t *testing.T) {
	skey, _, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := translog.NewSigner(skey)
	require.NoError(t, err)
	err = checkExtends(translog.Checkpoint{Size: 3, Root: translog.Hash{1}}, 5, translog.Hash{2}, func(old, n int64) ([]translog.Hash, error) {
		m := &translog.Memory{}
		for i := 0; i < 5; i++ {
			_, _ = m.Append([]byte{byte(i)})
		}
		return translog.ConsistencyProof(old, n, m)
	})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "does not extend"), err.Error())
	_ = signer
}

func TestTranslogStateCollector(t *testing.T) {
	db := openDB(t)
	c := NewStateCollector(db, nil)
	c.ttl = 0
	n := promtest.CollectAndCount(c)
	assert.Equal(t, 3, n, "size, checkpoint size, checkpoint time")
	_ = time.Second
}

// TestTranslog_OnlyTheStoreWritesTheLogTables: append-only by structure —
// no non-test Go file outside this package writes a translog_ table (the
// database trigger refuses UPDATE/DELETE/TRUNCATE; this keeps INSERTs here,
// under the lock).
func TestTranslog_OnlyTheStoreWritesTheLogTables(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	write := regexp.MustCompile(`(?is)\b(INSERT\s+INTO|UPDATE|DELETE\s+FROM|TRUNCATE(\s+TABLE)?)\s+translog_`)
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if filepath.Dir(path) == "." || strings.HasSuffix(filepath.ToSlash(filepath.Dir(path)), "internal/translog/logstore") {
			return nil
		}
		b, err := os.ReadFile(path) // #nosec G304 -- the repository's own files
		if err != nil {
			return err
		}
		if write.Match(b) {
			offenders = append(offenders, path)
		}
		return nil
	})
	require.NoError(t, err)
	assert.Empty(t, offenders, "only internal/translog/logstore writes the transparency log's tables")
}
