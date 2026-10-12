package crypto

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/hkdf"
)

const (
	testMasterA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testMasterB = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// keyTenant gives a test its own tenant ids; their key rows go with the test.
func keyTenant(t *testing.T, db *sql.DB) string {
	t.Helper()
	id := "tk-" + uuid.NewString()
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tenant_key_versions WHERE tenant_id = $1`, id) })
	return id
}

func newTestKeys(t *testing.T, db *sql.DB, master, previous string) *TenantKeys {
	t.Helper()
	k, err := NewTenantKeys(db, master, previous)
	require.NoError(t, err)
	return k
}

// oldHKDFKey is what KeyManager.DeriveTenantKey(tenant, 1) returned: the
// derived key this WP removes.
func oldHKDFKey(t *testing.T, masterHex, tenantID string) []byte {
	t.Helper()
	master := mustHex(t, masterHex)
	salt := sha256.Sum256([]byte("vaultaire-salt-v1"))
	r := hkdf.New(sha256.New, master, salt[:], []byte(fmt.Sprintf("vaultaire-tenant-key:v%d:%s", 1, tenantID)))
	out := make([]byte, 32)
	_, err := r.Read(out)
	require.NoError(t, err)
	return out
}

func mustHex(t *testing.T, h string) []byte {
	t.Helper()
	b, err := parseMasterKey(h)
	require.NoError(t, err)
	return b
}

func TestTenantKeys_StoredRandomNeverDerived(t *testing.T) {
	db := getTestDB(t)
	ctx := context.Background()
	a, b := keyTenant(t, db), keyTenant(t, db)
	keys := newTestKeys(t, db, testMasterA, "")

	ka, err := keys.Current(ctx, a)
	require.NoError(t, err)
	kb, err := keys.Current(ctx, b)
	require.NoError(t, err)

	assert.Equal(t, 1, ka.Version)
	assert.NotEqual(t, ka.ChunkKey, kb.ChunkKey, "two tenants under one master: unrelated keys")
	assert.False(t, bytes.Equal(ka.ChunkKey[:], oldHKDFKey(t, testMasterA, a)), "never the old HKDF output")
	assert.NotEqual(t, ka.KEM.Bytes(), kb.KEM.Bytes())

	// The same master in a fresh process (no cache) reads the same bytes.
	again, err := newTestKeys(t, db, testMasterA, "").Version(ctx, a, 1)
	require.NoError(t, err)
	assert.Equal(t, ka.ChunkKey, again.ChunkKey, "Version(v) is byte-stable across a cache clear")
	assert.Equal(t, ka.KEM.Bytes(), again.KEM.Bytes())

	// A random key is not a function of the master: the same tenant id on
	// another database row set (a fresh tenant) gets a different key.
	c := keyTenant(t, db)
	kc, err := keys.Current(ctx, c)
	require.NoError(t, err)
	assert.NotEqual(t, ka.ChunkKey, kc.ChunkKey)

	// The wrapped bytes are not the key.
	var ckw []byte
	require.NoError(t, db.QueryRow(`SELECT chunk_key_wrapped FROM tenant_key_versions WHERE tenant_id = $1 AND key_version = 1`, a).Scan(&ckw))
	assert.False(t, bytes.Contains(ckw, ka.ChunkKey[:]), "stored wrapped, never in the clear")
}

func TestTenantKeys_TwoFirstWritersAgree(t *testing.T) {
	db := getTestDB(t)
	ctx := context.Background()
	tenant := keyTenant(t, db)

	// Two processes (two stores, no shared cache), eight first writers each.
	stores := []*TenantKeys{newTestKeys(t, db, testMasterA, ""), newTestKeys(t, db, testMasterA, "")}
	var wg sync.WaitGroup
	got := make([]*KeyVersion, 16)
	errs := make([]error, 16)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = stores[i%2].Current(ctx, tenant)
		}(i)
	}
	wg.Wait()

	for i := range got {
		require.NoError(t, errs[i])
		assert.Equal(t, 1, got[i].Version)
		assert.Equal(t, got[0].ChunkKey, got[i].ChunkKey, "every first writer holds the one stored key")
		assert.Equal(t, got[0].KEM.Bytes(), got[i].KEM.Bytes())
	}
	var rows int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM tenant_key_versions WHERE tenant_id = $1`, tenant).Scan(&rows))
	assert.Equal(t, 1, rows)
}

func TestTenantKeys_RotateAddsAVersionAndRetiresThePrevious(t *testing.T) {
	db := getTestDB(t)
	ctx := context.Background()
	tenant := keyTenant(t, db)
	keys := newTestKeys(t, db, testMasterA, "")

	v1, err := keys.Current(ctx, tenant)
	require.NoError(t, err)
	n, err := keys.Rotate(ctx, tenant)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	cur, err := keys.Current(ctx, tenant)
	require.NoError(t, err)
	assert.Equal(t, 2, cur.Version, "this process writes with the new version at once")
	assert.NotEqual(t, v1.ChunkKey, cur.ChunkKey)
	old, err := keys.Version(ctx, tenant, 1)
	require.NoError(t, err)
	assert.Equal(t, v1.ChunkKey, old.ChunkKey, "the old version still reads")

	var retired1, retired2 sql.NullTime
	require.NoError(t, db.QueryRow(`SELECT
		(SELECT retired_at FROM tenant_key_versions WHERE tenant_id = $1 AND key_version = 1),
		(SELECT retired_at FROM tenant_key_versions WHERE tenant_id = $1 AND key_version = 2)`, tenant).Scan(&retired1, &retired2))
	assert.True(t, retired1.Valid)
	assert.False(t, retired2.Valid)

	_, err = keys.Version(ctx, tenant, 3)
	assert.ErrorIs(t, err, ErrNoTenantKey)

	// A tenant with no key: rotate gives it version 1.
	fresh := keyTenant(t, db)
	n, err = keys.Rotate(ctx, fresh)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// The AAD binds a wrapped secret to its tenant, version and purpose.
func TestTenantKeys_WrappedSecretsCannotBeMoved(t *testing.T) {
	db := getTestDB(t)
	ctx := context.Background()
	a, b := keyTenant(t, db), keyTenant(t, db)
	keys := newTestKeys(t, db, testMasterA, "")
	_, err := keys.Current(ctx, a)
	require.NoError(t, err)
	_, err = keys.Current(ctx, b)
	require.NoError(t, err)

	// b's row gets a's wrapped chunk key: b's key no longer opens.
	_, err = db.Exec(`UPDATE tenant_key_versions SET chunk_key_wrapped = (SELECT chunk_key_wrapped FROM tenant_key_versions WHERE tenant_id = $1 AND key_version = 1) WHERE tenant_id = $2`, a, b)
	require.NoError(t, err)
	_, err = newTestKeys(t, db, testMasterA, "").Version(ctx, b, 1)
	assert.ErrorIs(t, err, ErrKeyUnwrap, "another tenant's wrapped key does not open")

	// The purposes are not interchangeable either.
	w, err := wrapKey(mustHex(t, testMasterA), a, 1, purposeChunkKey, make([]byte, 32))
	require.NoError(t, err)
	_, err = unwrapKey(mustHex(t, testMasterA), a, 1, purposeKEMSeed, w)
	assert.Error(t, err)
	_, err = unwrapKey(mustHex(t, testMasterA), a, 2, purposeChunkKey, w)
	assert.Error(t, err)
	_, err = unwrapKey(mustHex(t, testMasterA), a, 1, purposeChunkKey, w)
	assert.NoError(t, err)
}

func TestMasterKeyRewrap_PreviousKeyToCurrent(t *testing.T) {
	db := getTestDB(t)
	ctx := context.Background()
	tenant := keyTenant(t, db)
	before := newTestKeys(t, db, testMasterA, "")
	k1, err := before.Current(ctx, tenant)
	require.NoError(t, err)
	_, err = before.Rotate(ctx, tenant)
	require.NoError(t, err)
	k2, err := before.Version(ctx, tenant, 2)
	require.NoError(t, err)

	// The new master alone cannot open the rows.
	_, err = newTestKeys(t, db, testMasterB, "").Version(ctx, tenant, 1)
	require.ErrorIs(t, err, ErrKeyUnwrap)

	// Boot with both: every row is re-wrapped, the secrets unchanged.
	// (Scoped to the test's tenant: the table is shared with other
	// packages, whose rows a re-wrap under B would make unreadable to them.)
	during := newTestKeys(t, db, testMasterB, testMasterA)
	res, err := during.RewrapTenant(ctx, tenant)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Rewrapped)
	assert.NotContains(t, res.Failed, tenant+" v1")
	res, err = during.RewrapTenant(ctx, tenant)
	require.NoError(t, err)
	assert.NotContains(t, strings.Join(res.Failed, ","), tenant)

	after := newTestKeys(t, db, testMasterB, "") // PREVIOUS removed
	got1, err := after.Version(ctx, tenant, 1)
	require.NoError(t, err)
	got2, err := after.Version(ctx, tenant, 2)
	require.NoError(t, err)
	assert.Equal(t, k1.ChunkKey, got1.ChunkKey, "no data is re-encrypted: the same secrets")
	assert.Equal(t, k2.KEM.Bytes(), got2.KEM.Bytes())

	_, err = newTestKeys(t, db, testMasterA, "").Version(ctx, tenant, 1)
	assert.ErrorIs(t, err, ErrKeyUnwrap, "the old master opens nothing any more")
}

// WP-R8-1 follow-up A3: the two-slot switch keeps the OLD slot (old master
// only) serving after the new slot's boot check ran. A tenant's first
// encrypted write on the old slot in that window wraps v1 under the old
// master; the new slot opens it through PREVIOUS and nothing shows — until
// PREVIOUS is removed and no key opens the row. The new slot must re-wrap
// it at first use.
func TestMasterKeyRewrap_RowCreatedAfterTheBootCheck_IsRewrappedAtFirstUse(t *testing.T) {
	db := getTestDB(t)
	ctx := context.Background()
	tenant := keyTenant(t, db)

	// The new slot boots with B + previous A; its check finds nothing of
	// this tenant (scoped: the table is shared with other packages).
	newSlot := newTestKeys(t, db, testMasterB, testMasterA)
	var rewraps int
	newSlot.OnRewrap(func() { rewraps++ })
	res, err := newSlot.RewrapTenant(ctx, tenant)
	require.NoError(t, err)
	require.Zero(t, res.Rows)

	// The old slot (A only) is still serving: the tenant's first encrypted
	// write creates v1 under A.
	oldSlot := newTestKeys(t, db, testMasterA, "")
	created, err := oldSlot.Current(ctx, tenant)
	require.NoError(t, err)

	// The new slot reads it (through PREVIOUS) — and leaves it under B.
	got, err := newSlot.Version(ctx, tenant, 1)
	require.NoError(t, err)
	assert.Equal(t, created.ChunkKey, got.ChunkKey)
	assert.Equal(t, 1, rewraps, "one row re-wrapped at first use")

	// PREVIOUS removed: a third start with B alone opens it.
	third := newTestKeys(t, db, testMasterB, "")
	again, err := third.Version(ctx, tenant, 1)
	require.NoError(t, err, "before: ErrKeyUnwrap — the tenant's every encrypted read and write 500")
	assert.Equal(t, created.ChunkKey, again.ChunkKey, "the same secret: nothing re-encrypted")
	assert.Equal(t, created.KEM.Bytes(), again.KEM.Bytes())

	// A second use re-wraps nothing; the boot pass finds nothing left.
	_, err = newTestKeys(t, db, testMasterB, testMasterA).Version(ctx, tenant, 1)
	require.NoError(t, err)
	res, err = newSlot.RewrapTenant(ctx, tenant)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Rows)
	assert.Zero(t, res.Rewrapped)
}

// Two processes opening the same old-master row at once: both read the
// secret, one re-wrap lands (the UPDATE is guarded on the old bytes), the
// row opens under the new master after.
func TestMasterKeyRewrap_ConcurrentFirstUse_OneWriteLands(t *testing.T) {
	db := getTestDB(t)
	ctx := context.Background()
	tenant := keyTenant(t, db)
	created, err := newTestKeys(t, db, testMasterA, "").Current(ctx, tenant)
	require.NoError(t, err)

	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k := newTestKeys(t, db, testMasterB, testMasterA)
			k.OnRewrap(func() { mu.Lock(); total++; mu.Unlock() })
			kv, err := k.Version(ctx, tenant, 1)
			assert.NoError(t, err)
			if err == nil {
				assert.Equal(t, created.ChunkKey, kv.ChunkKey)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, total, "the guarded UPDATE lands once")
	_, err = newTestKeys(t, db, testMasterB, "").Version(ctx, tenant, 1)
	require.NoError(t, err)
}

func TestMasterKeyRewrap_UnwrapsUnderNeither(t *testing.T) {
	db := getTestDB(t)
	ctx := context.Background()
	tenant := keyTenant(t, db)
	_, err := newTestKeys(t, db, testMasterA, "").Current(ctx, tenant)
	require.NoError(t, err)

	third := "1111111111111111111111111111111111111111111111111111111111111111"
	wrong := newTestKeys(t, db, testMasterB, third)
	res, err := wrong.RewrapTenant(ctx, tenant)
	require.NoError(t, err)
	assert.Contains(t, res.Failed, tenant+" v1", "a row neither key opens is reported")

	_, err = wrong.Current(ctx, tenant)
	assert.ErrorIs(t, err, ErrKeyUnwrap, "and its reads fail closed")

	// It was left as it was: the right master still opens it.
	_, err = newTestKeys(t, db, testMasterA, "").Version(ctx, tenant, 1)
	assert.NoError(t, err)
}

func TestNewTenantKeys_RejectsBadMasterKeys(t *testing.T) {
	for name, tc := range map[string][2]string{
		"not hex":            {"not-hex", ""},
		"short":              {"0123456789abcdef", ""},
		"all zeros":          {strings.Repeat("00", 32), ""},
		"bad previous":       {testMasterA, "zz"},
		"previous = current": {testMasterA, testMasterA},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewTenantKeys(nil, tc[0], tc[1])
			assert.Error(t, err)
		})
	}
}
