package logstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
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
	"github.com/FairForge/vaultaire/internal/translog/logstore/logstoretest"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/sumdb/note"

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

func ceremony(t *testing.T) translog.KeyCeremony {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return translog.KeyCeremony{Action: translog.ActionKeyGenerated, KeyRole: translog.KeyTranslogSigningKey,
		KeyFingerprint: "sha256:" + hex.EncodeToString(b), Participants: 1}
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
	entries := make([]translog.KeyCeremony, n)
	for i := range entries {
		entries[i] = ceremony(t)
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			idxs[i], _, errs[i] = s.Append(ctx, entries[i])
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
		require.Contains(t, string(body), entries[i].KeyFingerprint)
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
	bad := ceremony(t)
	bad.Reference = "https://example.com/alice"
	_, _, err := s.Append(context.Background(), bad)
	require.Error(t, err)
	assert.Equal(t, before+1, promtest.ToFloat64(appendFailures.WithLabelValues(translog.KindKeyCeremony)))
}

func TestTranslog_TablesAreAppendOnly(t *testing.T) {
	db := openDB(t)
	s := New(db, nil)
	ctx := context.Background()
	idx, _, err := s.Append(ctx, ceremony(t))
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

func testSigner(t *testing.T) (note.Signer, note.Verifier) {
	t.Helper()
	skey, vkey, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := translog.NewSigner(skey)
	require.NoError(t, err)
	verifier, err := translog.NewVerifier(vkey)
	require.NoError(t, err)
	return signer, verifier
}

// A test that signs runs on its own log (RP37-13): on the shared one another
// package could sign the same size first, with another key.
func TestTranslog_CheckpointSignsWhenTheTreeGrew(t *testing.T) {
	db := logstoretest.PrivateLog(t)
	ctx := context.Background()
	signer, verifier := testSigner(t)
	hw := filepath.Join(t.TempDir(), "translog", "highwater")
	s := New(db, signer).WithHighwater(hw)

	for i := 0; i < 3; i++ {
		_, _, err := s.Append(ctx, ceremony(t))
		require.NoError(t, err)
	}
	res, err := s.Checkpoint(ctx)
	require.NoError(t, err)
	require.True(t, res.Signed, "%+v", res)
	assert.Equal(t, int64(3), res.Size)
	assert.Contains(t, res.Note, "first run", "no high-water file yet")
	latest, ok, err := s.LatestCheckpoint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	cp, err := translog.OpenCheckpoint([]byte(latest.Note), verifier)
	require.NoError(t, err)
	assert.Equal(t, int64(3), cp.Size)
	assert.Equal(t, recomputeRoot(t, db, cp.Size), cp.Root)

	// The high-water file holds that checkpoint, 0600.
	st, err := os.Stat(hw)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	raw, err := os.ReadFile(hw)
	require.NoError(t, err)
	assert.Equal(t, strings.SplitN(latest.Note, "\n\n", 2)[0]+"\n", string(raw), "the checkpoint body as signed")

	// Nothing appended since: the next run signs nothing; one more entry is signed.
	res, err = New(db, signer).WithHighwater(hw).Checkpoint(ctx)
	require.NoError(t, err)
	assert.False(t, res.Signed)
	assert.Equal(t, "unchanged", res.Reason)
	_, _, err = s.Append(ctx, ceremony(t))
	require.NoError(t, err)
	res, err = s.Checkpoint(ctx)
	require.NoError(t, err)
	assert.True(t, res.Signed)
	assert.Equal(t, int64(4), res.HighwaterSize)
}

// restoreOf is a database restored from a dump taken when the log held the
// first m entries of src and src's checkpoints up to m: the same rows,
// byte for byte (the dump), in a fresh log.
func restoreOf(t *testing.T, src *sql.DB, m int64) *sql.DB {
	t.Helper()
	ctx := context.Background()
	dst := logstoretest.PrivateLog(t)
	copyRows := func(query, insert string, arg int64, cols int) {
		rows, err := src.QueryContext(ctx, query, arg)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			vals := make([]any, cols)
			ptrs := make([]any, cols)
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			require.NoError(t, rows.Scan(ptrs...))
			_, err := dst.ExecContext(ctx, insert, vals...)
			require.NoError(t, err)
		}
		require.NoError(t, rows.Err())
	}
	copyRows(`SELECT idx, kind, body, dedup_key FROM translog_entries WHERE idx < $1 ORDER BY idx`,
		`INSERT INTO translog_entries (idx, kind, body, dedup_key) VALUES ($1, $2, $3, $4)`, m, 4)
	copyRows(`SELECT idx, hash FROM translog_hashes WHERE idx < $1`,
		`INSERT INTO translog_hashes (idx, hash) VALUES ($1, $2)`, translog.StoredHashCount(m), 2)
	copyRows(`SELECT size, root, note, signed_at FROM translog_checkpoints WHERE size <= $1`,
		`INSERT INTO translog_checkpoints (size, root, note, signed_at) VALUES ($1, $2, $3, $4)`, m, 4)
	return dst
}

// RP37-11: a database restored behind the newest tree the key signed makes
// the job refuse — first because it holds fewer entries than the high-water
// (behind_highwater), then, once new entries reach and pass that size,
// because they are a different tree (rewritten). Never a second signature at
// a size already signed. Without the file the restored database's own
// checkpoint is consistent and the job would sign the fork.
func TestCheckpoint_DatabaseRestoredBehindHighwater_RefusesToSign(t *testing.T) {
	ctx := context.Background()
	signer, _ := testSigner(t)
	hw := filepath.Join(t.TempDir(), "highwater")
	live := logstoretest.PrivateLog(t)
	s := New(live, signer).WithHighwater(hw)
	for i := 0; i < 2; i++ {
		_, _, err := s.Append(ctx, ceremony(t))
		require.NoError(t, err)
	}
	res, err := s.Checkpoint(ctx) // the 03:00 dump is taken after this: size 2, signed
	require.NoError(t, err)
	require.True(t, res.Signed)
	for i := 0; i < 3; i++ {
		_, _, err := s.Append(ctx, ceremony(t))
		require.NoError(t, err)
	}
	res, err = s.Checkpoint(ctx) // signed at 5 and published; then the disk dies
	require.NoError(t, err)
	require.True(t, res.Signed)
	require.Equal(t, int64(5), res.HighwaterSize)

	restored := restoreOf(t, live, 2)
	r := New(restored, signer).WithHighwater(hw)
	behind := promtest.ToFloat64(checkpointRefusals.WithLabelValues(RefusedBehindHighwater))
	rewritten := promtest.ToFloat64(checkpointRefusals.WithLabelValues(RefusedRewritten))
	signedRows := func() int {
		var n int
		require.NoError(t, restored.QueryRow(`SELECT COUNT(*) FROM translog_checkpoints WHERE size > 2`).Scan(&n))
		return n
	}

	// Restored to 2 entries, then new (different) entries up to 4: behind.
	// Since RP42-1 the gated store refuses those appends (freeze_test.go);
	// they come here from an appender without the file — a build before the
	// gate, or SQL by hand — and the job still refuses: defence in depth.
	ungated := New(restored, nil)
	for i := 0; i < 2; i++ {
		_, _, err := ungated.Append(ctx, ceremony(t))
		require.NoError(t, err)
	}
	res, err = r.Checkpoint(ctx)
	require.Error(t, err)
	assert.Equal(t, RefusedBehindHighwater, res.Refused)
	assert.Equal(t, behind+1, promtest.ToFloat64(checkpointRefusals.WithLabelValues(RefusedBehindHighwater)))
	assert.Equal(t, 0, signedRows())

	// Grown to 5 and past it with different entries: not the tree the key signed.
	for _, want := range []int64{5, 6} {
		_, _, err := ungated.Append(ctx, ceremony(t))
		require.NoError(t, err)
		res, err = r.Checkpoint(ctx)
		require.Error(t, err, "size %d", want)
		assert.Equal(t, RefusedRewritten, res.Refused)
		assert.Contains(t, err.Error(), "does not extend checkpoint 5")
	}
	assert.Equal(t, rewritten+2, promtest.ToFloat64(checkpointRefusals.WithLabelValues(RefusedRewritten)))
	assert.Equal(t, 0, signedRows(), "the key never signed a second tree at a size it had signed")
}

// RP37-11: a high-water file that exists but cannot be read, or is not a
// checkpoint of this log, refuses — never "first run".
func TestCheckpoint_HighwaterUnreadable_Refuses(t *testing.T) {
	ctx := context.Background()
	signer, _ := testSigner(t)
	db := logstoretest.PrivateLog(t)
	s0 := New(db, signer)
	_, _, err := s0.Append(ctx, ceremony(t))
	require.NoError(t, err)
	dir := t.TempDir()
	cases := map[string]func(path string){
		"garbage": func(p string) { require.NoError(t, os.WriteFile(p, []byte("not a checkpoint\n"), 0o600)) },
		"another origin": func(p string) {
			require.NoError(t, os.WriteFile(p, []byte("example.com/log\n1\n"+strings.Repeat("A", 43)+"=\n"), 0o600))
		},
		"empty":       func(p string) { require.NoError(t, os.WriteFile(p, nil, 0o600)) },
		"a directory": func(p string) { require.NoError(t, os.Mkdir(p, 0o700)) },
	}
	if os.Geteuid() != 0 {
		cases["unreadable"] = func(p string) { require.NoError(t, os.WriteFile(p, []byte("x"), 0o000)) }
	}
	for name, mk := range cases {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
		mk(p)
		before := promtest.ToFloat64(checkpointRefusals.WithLabelValues(RefusedHighwaterUnreadable))
		res, err := New(db, signer).WithHighwater(p).Checkpoint(ctx)
		require.Error(t, err, name)
		assert.Equal(t, RefusedHighwaterUnreadable, res.Refused, name)
		assert.False(t, res.Signed, name)
		assert.Equal(t, before+1, promtest.ToFloat64(checkpointRefusals.WithLabelValues(RefusedHighwaterUnreadable)), name)
	}
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM translog_checkpoints`).Scan(&n))
	assert.Equal(t, 0, n)
}

// Every refusal reason is a series from boot (at 0).
func TestCheckpointRefusals_PreCreated(t *testing.T) {
	assert.Equal(t, len(RefusalReasons()), promtest.CollectAndCount(checkpointRefusals), "one series per reason, from init")
	assert.ElementsMatch(t, []string{"rewritten", "behind_highwater", "highwater_unreadable"}, RefusalReasons())
}

func TestTranslog_CheckpointWithoutAKeySignsNothing(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	s := New(db, nil)
	_, _, err := s.Append(ctx, ceremony(t))
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
	// The log's three tables. translog_subjects (104, K4c-1) is tenant data
	// — the admin-read salts — written by internal/translog/adminread and
	// erased by internal/account; it is not the log.
	write := regexp.MustCompile(`(?is)\b(INSERT\s+INTO|UPDATE|DELETE\s+FROM|TRUNCATE(\s+TABLE)?)\s+translog_(entries|hashes|checkpoints)\b`)
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
