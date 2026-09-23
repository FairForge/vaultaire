package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Read-time promotion (5.15.8 PR B): reading a Smart-demoted object brings
// it back hot — a routing flip while the hot copy still exists, an async
// copy-back once it was reclaimed, and an auto-restore (503 + Retry-After
// instead of Glacier's 403) once the cold backend evicted it to tape.

func newPromoter(f *demotionFixture) *SmartPromoter {
	p := NewSmartPromoter(f.db, f.eng, zap.NewNop())
	p.sync = true
	p.now = f.runner.now
	f.runner.Promoter = p
	return p
}

func (f *demotionFixture) demoteNow(bucket, key string) string {
	f.t.Helper()
	res, err := f.runner.RunOnce(context.Background(), false)
	require.NoError(f.t, err)
	require.Equal(f.t, 1, res.Demoted, "%+v", res)
	require.Equal(f.t, "geyser", f.backendOf(bucket, key))
	var etag string
	require.NoError(f.t, f.db.QueryRow(`SELECT etag FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`, f.tenantID, bucket, key).Scan(&etag))
	return etag
}

func TestSmartPromotion_FlipBackInsideGrace(t *testing.T) {
	f := setupDemotionFixture(t, 1*tb, "standard")
	p := newPromoter(f)
	f.object("b", "doc", 100, 30, 20)
	etag := f.demoteNow("b", "doc")
	require.True(t, f.hotExists("b", "doc") && f.coldExists("b", "doc"))

	got := p.OnRead(context.Background(), f.tenantID, "b", "doc", etag)

	assert.Equal(t, "idrive", got, "serve from hot right away")
	assert.Equal(t, "idrive", f.backendOf("b", "doc"))
	assert.True(t, f.hotExists("b", "doc"))
	assert.False(t, f.coldExists("b", "doc"), "cold copy dropped — tape bytes cost money")
	_, _, _, ok := f.ledger("b", "doc")
	assert.False(t, ok, "ledger row gone")
}

func TestSmartPromotion_CopyBackAfterReclaim(t *testing.T) {
	f := setupDemotionFixture(t, 1*tb, "standard")
	p := newPromoter(f)
	f.object("b", "doc", 100, 30, 20)
	etag := f.demoteNow("b", "doc")
	// Reclaim happened: hot copy gone, ledger closed out.
	require.NoError(t, os.Remove(filepath.Join(f.hotDir, f.tenantID+"_b", "doc")))
	_, err := f.db.Exec(`UPDATE smart_demotions SET hot_deleted_at=NOW(), hot_outcome='deleted' WHERE tenant_id=$1`, f.tenantID)
	require.NoError(t, err)

	got := p.OnRead(context.Background(), f.tenantID, "b", "doc", etag)

	assert.Equal(t, "geyser", got, "this read is served from cold")
	// sync promoter already copied back:
	assert.Equal(t, "idrive", f.backendOf("b", "doc"))
	assert.True(t, f.hotExists("b", "doc"))
	assert.False(t, f.coldExists("b", "doc"))
	_, _, _, ok := f.ledger("b", "doc")
	assert.False(t, ok)
	b, err := os.ReadFile(filepath.Join(f.hotDir, f.tenantID+"_b", "doc"))
	require.NoError(t, err)
	assert.Equal(t, "blob:doc", string(b), "bytes round-tripped")
}

func TestSmartPromotion_CopyBackChangedObjectKeepsNewBytes(t *testing.T) {
	// A PUT lands on hot while we copy back: the hot blob at this key is the
	// NEW object — never delete it; just drop the stale ledger row.
	f := setupDemotionFixture(t, 1*tb, "standard")
	p := newPromoter(f)
	f.object("b", "doc", 100, 30, 20)
	etag := f.demoteNow("b", "doc")
	require.NoError(t, os.Remove(filepath.Join(f.hotDir, f.tenantID+"_b", "doc")))
	_, err := f.db.Exec(`UPDATE smart_demotions SET hot_deleted_at=NOW(), hot_outcome='deleted' WHERE tenant_id=$1`, f.tenantID)
	require.NoError(t, err)
	p.beforeFlip = func(bucket, key string) {
		// simulate the concurrent PUT: new bytes on hot, head row re-pointed
		require.NoError(t, os.WriteFile(filepath.Join(f.hotDir, f.tenantID+"_b", "doc"), []byte("NEW"), 0o600))
		_, err := f.db.Exec(`UPDATE object_head_cache SET backend_name='idrive', etag='etag-new' WHERE tenant_id=$1 AND bucket='b' AND object_key='doc'`, f.tenantID)
		require.NoError(t, err)
	}

	_ = p.OnRead(context.Background(), f.tenantID, "b", "doc", etag)

	b, err := os.ReadFile(filepath.Join(f.hotDir, f.tenantID+"_b", "doc"))
	require.NoError(t, err)
	assert.Equal(t, "NEW", string(b), "the newer object's bytes survive")
	assert.Equal(t, "idrive", f.backendOf("b", "doc"))
	_, _, _, ok := f.ledger("b", "doc")
	assert.False(t, ok)
}

func TestSmartPromotion_NotDemotedObjectsUntouched(t *testing.T) {
	f := setupDemotionFixture(t, 1*tb, "standard")
	p := newPromoter(f)
	f.object("vault", "archive.bin", 100, 30, 20, onBackend("geyser")) // a Vault object, never demoted by us

	assert.Equal(t, "geyser", p.OnRead(context.Background(), f.tenantID, "vault", "archive.bin", "etag-vault-archive.bin"))
	assert.False(t, p.OnArchived(context.Background(), f.tenantID, "vault", "archive.bin"), "Vault keeps Glacier semantics")
	assert.Equal(t, "geyser", f.backendOf("vault", "archive.bin"))
}

func TestSmartPromotion_EvictedObjectAutoRestoresThenCopiesBack(t *testing.T) {
	f := setupDemotionFixture(t, 1*tb, "standard")
	p := newPromoter(f)
	// Swap the cold backend for the archive stub so eviction can be simulated.
	stub := newStubArchiveDriver()
	f.eng.AddDriver("geyser", stub)
	f.object("b", "doc", 100, 30, 20)
	etag := f.demoteNow("b", "doc")
	require.NoError(t, os.Remove(filepath.Join(f.hotDir, f.tenantID+"_b", "doc")))
	_, err := f.db.Exec(`UPDATE smart_demotions SET hot_deleted_at=NOW(), hot_outcome='deleted' WHERE tenant_id=$1`, f.tenantID)
	require.NoError(t, err)
	stub.archived = true

	// Read → copy-back attempt hits ErrArchived → restore requested.
	assert.Equal(t, "geyser", p.OnRead(context.Background(), f.tenantID, "b", "doc", etag))
	assert.Equal(t, 1, stub.restoreCalls)
	assert.Equal(t, int32(7), stub.lastDays)
	var restoreReq, promoteReq bool
	require.NoError(t, f.db.QueryRow(`SELECT restore_requested_at IS NOT NULL, promote_requested_at IS NOT NULL FROM smart_demotions WHERE tenant_id=$1`, f.tenantID).Scan(&restoreReq, &promoteReq))
	assert.True(t, restoreReq)
	assert.True(t, promoteReq)

	// The GET path's ErrArchived branch consults OnArchived: ours → true,
	// and a second call must not re-submit the restore.
	assert.True(t, p.OnArchived(context.Background(), f.tenantID, "b", "doc"))
	assert.Equal(t, 1, stub.restoreCalls, "restore submitted once")

	// Restore lands; the daily job's PromotePending copies it back.
	stub.archived = false
	res, err := f.runner.RunOnce(context.Background(), false)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Promoted, "%+v", res)
	assert.Equal(t, "idrive", f.backendOf("b", "doc"))
	assert.True(t, f.hotExists("b", "doc"))
	_, _, _, ok := f.ledger("b", "doc")
	assert.False(t, ok)
}

func TestSmartPromotion_HandleGetServesAndFlips(t *testing.T) {
	f := setupDemotionFixture(t, 1*tb, "standard")
	p := newPromoter(f)
	f.object("b", "doc", 100, 30, 20)
	_ = f.demoteNow("b", "doc")
	adapter := NewS3ToEngine(f.eng, f.db, zap.NewNop())
	adapter.smartPromoter = p
	tn := &tenant.Tenant{ID: f.tenantID, Namespace: "tenant/" + f.tenantID + "/"}

	req := httptest.NewRequest(http.MethodGet, "/b/doc", nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), tn))
	w := httptest.NewRecorder()
	adapter.HandleGet(w, req, "b", "doc")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	body, _ := io.ReadAll(w.Body)
	assert.Equal(t, "blob:doc", string(body))
	assert.Equal(t, "idrive", f.backendOf("b", "doc"), "flipped back by the read")
}

func TestSmartPromotion_HandleGetEvictedAnswers503NotForbidden(t *testing.T) {
	f := setupDemotionFixture(t, 1*tb, "standard")
	p := newPromoter(f)
	stub := newStubArchiveDriver()
	f.eng.AddDriver("geyser", stub)
	f.object("b", "doc", 100, 30, 20)
	_ = f.demoteNow("b", "doc")
	require.NoError(t, os.Remove(filepath.Join(f.hotDir, f.tenantID+"_b", "doc")))
	_, err := f.db.Exec(`UPDATE smart_demotions SET hot_deleted_at=NOW(), hot_outcome='deleted' WHERE tenant_id=$1`, f.tenantID)
	require.NoError(t, err)
	stub.archived = true
	adapter := NewS3ToEngine(f.eng, f.db, zap.NewNop())
	adapter.smartPromoter = p
	tn := &tenant.Tenant{ID: f.tenantID, Namespace: "tenant/" + f.tenantID + "/"}

	req := httptest.NewRequest(http.MethodGet, "/b/doc", nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), tn))
	w := httptest.NewRecorder()
	adapter.HandleGet(w, req, "b", "doc")

	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "120", w.Header().Get("Retry-After"))
	assert.GreaterOrEqual(t, stub.restoreCalls, 1, "restore submitted on the reader's behalf")
	_ = time.Second
}
