package adminread

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/sumdb/note"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
	"github.com/FairForge/vaultaire/internal/translog/logstore/logstoretest"
)

// privateStore is a store on its own log (migration 103) plus migration
// 104's translog_subjects, with a clock the test moves.
func privateStore(t *testing.T, signer note.Signer) (*Store, *sql.DB, *time.Time) {
	t.Helper()
	db := logstoretest.PrivateLog(t)
	_, self, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(self), "..", "..", "database", "migrations", "104_translog_subjects.sql")) // #nosec G304 -- the repository's own migration
	require.NoError(t, err)
	_, err = db.Exec(string(raw))
	require.NoError(t, err)
	now := time.Date(2026, 10, 23, 12, 10, 0, 0, time.UTC)
	s := New(db, logstore.New(db, signer))
	s.now = func() time.Time { return now }
	return s, db, &now
}

func body(t *testing.T, db *sql.DB, idx int64) translog.AdminRead {
	t.Helper()
	var b []byte
	require.NoError(t, db.QueryRow(`SELECT body FROM translog_entries WHERE idx = $1`, idx).Scan(&b))
	d, err := translog.Decode(b)
	require.NoError(t, err)
	return d.Entry.(translog.AdminRead)
}

func TestAdminRead_SubjectUnlinkableWithoutSalt(t *testing.T) {
	// Arrange: two reads of tenant A (two operators) and one of tenant B.
	ctx := context.Background()
	s, db, _ := privateStore(t, nil)
	a, b := "tenant-a-"+uuid.NewString(), "tenant-b-"+uuid.NewString()

	i1, _, err := s.Record(ctx, "op-1", translog.ScopeTenantDetail, a)
	require.NoError(t, err)
	i2, _, err := s.Record(ctx, "op-2", translog.ScopeTenantDetail, a)
	require.NoError(t, err)
	i3, _, err := s.Record(ctx, "op-1", translog.ScopeTenantDetail, b)
	require.NoError(t, err)

	// Assert: three public bodies that share nothing a reader could join
	// on — a fresh nonce each, commitments unrelated without the salt.
	e1, e2, e3 := body(t, db, i1), body(t, db, i2), body(t, db, i3)
	assert.NotEqual(t, e1.Nonce, e2.Nonce)
	assert.NotEqual(t, e1.Commitment, e2.Commitment, "two reads of one tenant are not linkable")
	for _, e := range []translog.AdminRead{e1, e2, e3} {
		assert.NotContains(t, e.Commitment+e.Nonce, hex.EncodeToString([]byte(a))[:16])
	}
	// With the salt they are A's; under another salt — B's, or a guess — no.
	saltA, err := s.salt(ctx, a, false)
	require.NoError(t, err)
	saltB, err := s.salt(ctx, b, false)
	require.NoError(t, err)
	guess := make([]byte, 32)
	_, _ = rand.Read(guess)
	for _, e := range []translog.AdminRead{e1, e2} {
		nonce, _ := hex.DecodeString(e.Nonce)
		assert.Equal(t, e.Commitment, hex.EncodeToString(Commitment(saltA, nonce)))
		assert.NotEqual(t, e.Commitment, hex.EncodeToString(Commitment(saltB, nonce)))
		assert.NotEqual(t, e.Commitment, hex.EncodeToString(Commitment(guess, nonce)))
	}
	found, err := s.Find(ctx, a)
	require.NoError(t, err)
	require.Len(t, found, 2)
	assert.Equal(t, []int64{i1, i2}, []int64{found[0].Index, found[1].Index})

	// After the erasure (the salt deleted), nobody — the server included —
	// can link them: Find finds nothing, the bodies are unchanged.
	_, err = db.Exec(`DELETE FROM translog_subjects WHERE tenant_id = $1`, a)
	require.NoError(t, err)
	found, err = s.Find(ctx, a)
	require.NoError(t, err)
	assert.Empty(t, found)
	var keys []string
	rows, err := db.Query(`SELECT dedup_key FROM translog_entries WHERE kind = 'admin_read' ORDER BY idx`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		keys = append(keys, k)
		assert.NotContains(t, k, a, "the coalescing key names no tenant")
		assert.NotContains(t, k, "op-", "nor an operator")
	}
	require.NoError(t, rows.Err())
	assert.Len(t, keys, 3)
}

func TestAdminRead_RefreshCoalesced(t *testing.T) {
	ctx := context.Background()
	s, db, now := privateStore(t, nil)
	tenant := "tenant-" + uuid.NewString()
	count := func() int {
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM translog_entries WHERE kind = 'admin_read'`).Scan(&n))
		return n
	}

	first, appended, err := s.Record(ctx, "op-1", translog.ScopeTenantDetail, tenant)
	require.NoError(t, err)
	require.True(t, appended)
	// A refresh, ten minutes later: nothing appended, the same index.
	*now = now.Add(10 * time.Minute)
	again, appended, err := s.Record(ctx, "op-1", translog.ScopeTenantDetail, tenant)
	require.NoError(t, err)
	assert.False(t, appended)
	assert.Equal(t, first, again)
	assert.Equal(t, 1, count())

	// Another scope, another operator, another hour: each is its own entry.
	_, appended, err = s.Record(ctx, "op-1", translog.ScopeTenantPayments, tenant)
	require.NoError(t, err)
	assert.True(t, appended)
	_, appended, err = s.Record(ctx, "op-2", translog.ScopeTenantDetail, tenant)
	require.NoError(t, err)
	assert.True(t, appended)
	*now = now.Add(time.Hour)
	_, appended, err = s.Record(ctx, "op-1", translog.ScopeTenantDetail, tenant)
	require.NoError(t, err)
	assert.True(t, appended)
	assert.Equal(t, 4, count())

	// A list scope coalesces the same way, and names no tenant.
	_, appended, err = s.Record(ctx, "op-1", translog.ScopeTenantList, "")
	require.NoError(t, err)
	assert.True(t, appended)
	_, appended, err = s.Record(ctx, "op-1", translog.ScopeTenantList, "")
	require.NoError(t, err)
	assert.False(t, appended)
	_, _, err = s.Record(ctx, "op-1", translog.ScopeTenantList, tenant)
	assert.Error(t, err, "a list names no tenant")
	_, _, err = s.Record(ctx, "op-1", translog.ScopeTenantDetail, "")
	assert.ErrorIs(t, err, ErrNoSubject)
}

func TestFind_NamesTheCheckpointThatIncludesEach(t *testing.T) {
	ctx := context.Background()
	skey, _, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := translog.NewSigner(skey)
	require.NoError(t, err)
	s, _, now := privateStore(t, signer)
	tenant := "tenant-" + uuid.NewString()
	i1, _, err := s.Record(ctx, "op-1", translog.ScopeTenantDetail, tenant)
	require.NoError(t, err)
	res, err := s.log.Checkpoint(ctx)
	require.NoError(t, err)
	require.True(t, res.Signed)
	*now = now.Add(time.Hour)
	_, _, err = s.Record(ctx, "op-1", translog.ScopeTenantDetail, tenant)
	require.NoError(t, err)

	found, err := s.Find(ctx, tenant)

	require.NoError(t, err)
	require.Len(t, found, 2)
	require.NotNil(t, found[0].CheckpointSize)
	assert.Equal(t, i1+1, *found[0].CheckpointSize)
	assert.Nil(t, found[1].CheckpointSize, "not signed yet")
	assert.Equal(t, translog.ScopeTenantDetail, found[0].Scope)
}
