package api

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Per-floor accounting (066, dashboard plan Phase 1): a house tenant is
// charged on the floor the write's storage class resolves to, and every
// release lands on the floor the head row recorded.
//
// Invariants under test, per tenant:
//
//	tenant_floor_quotas[f].storage_used_bytes == SUM(object_head_cache.size_bytes WHERE floor = f)
//	tenant_quotas.storage_used_bytes          == SUM over floors

// floors returns (downstairs used, attic used) for the fixture's tenant.
func (f *quotaAccountingFixture) floors(t *testing.T) (int64, int64) {
	t.Helper()
	rows, err := f.qm.GetFloors(context.Background(), f.tenantID)
	require.NoError(t, err)
	var std, vault int64
	for _, r := range rows {
		switch r.Floor {
		case usage.FloorStandard:
			std = r.UsedBytes
		case usage.FloorVault:
			vault = r.UsedBytes
		}
	}
	return std, vault
}

// headFloor returns the floor recorded on bucket/key's head row.
func (f *quotaAccountingFixture) headFloor(t *testing.T, bucket, key string) string {
	t.Helper()
	var floor string
	require.NoError(t, f.db.QueryRow(
		`SELECT floor FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, bucket, key).Scan(&floor))
	return floor
}

// putClass is put with an x-amz-storage-class header on any bucket.
func (f *quotaAccountingFixture) putClass(t *testing.T, bucket, key, class string, body []byte) int {
	t.Helper()
	req := httptest.NewRequest("PUT", "/"+bucket+"/"+key, bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	if class != "" {
		req.Header.Set("x-amz-storage-class", class)
	}
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handlePutObject(w, req, f.s3Req(bucket, key))
	return w.Code
}

// house gives the fixture's tenant a house of the given floor sizes and
// registers an `archive`-tier bucket ("attic-bucket") for it.
func (f *quotaAccountingFixture) house(t *testing.T, stdBytes, vaultBytes int64) {
	t.Helper()
	require.NoError(t, f.qm.SetHouse(context.Background(), f.tenantID,
		usage.House{StdBytes: stdBytes, VaultBytes: vaultBytes}))
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, tier_preference) VALUES ($1, 'attic-bucket', 'archive')
		ON CONFLICT (tenant_id, name) DO UPDATE SET tier_preference = 'archive'`, f.tenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, f.tenantID)
	})
	require.NoError(t, os.MkdirAll(filepath.Join(f.tempDir, f.tenant.NamespaceContainer("attic-bucket")), 0o755))
}

func TestFloorAccounting_ClassPicksTheFloor(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)
	f.house(t, 10<<20, 10<<20)

	require.Equal(t, 200, f.put(t, "down.bin", testBytes(1<<20)))
	require.Equal(t, 200, f.putClass(t, "test-bucket", "up.bin", "GLACIER", testBytes(2<<20)))
	require.Equal(t, 200, f.putClass(t, "attic-bucket", "shelf.bin", "", testBytes(3<<20)))

	assert.Equal(t, usage.FloorStandard, f.headFloor(t, "test-bucket", "down.bin"))
	assert.Equal(t, usage.FloorVault, f.headFloor(t, "test-bucket", "up.bin"), "GLACIER header = attic")
	assert.Equal(t, usage.FloorVault, f.headFloor(t, "attic-bucket", "shelf.bin"), "archive bucket = attic")

	std, vault := f.floors(t)
	assert.Equal(t, int64(1<<20), std)
	assert.Equal(t, int64(5<<20), vault)
	assert.Equal(t, int64(6<<20), f.used(t), "total == sum of floors")
}

func TestFloorAccounting_DeleteReleasesItsFloor(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)
	f.house(t, 10<<20, 10<<20)
	require.Equal(t, 200, f.put(t, "down.bin", testBytes(1<<20)))
	require.Equal(t, 200, f.putClass(t, "test-bucket", "up.bin", "GLACIER", testBytes(2<<20)))

	require.Equal(t, 204, f.del(t, "up.bin"))
	std, vault := f.floors(t)
	assert.Equal(t, int64(1<<20), std)
	assert.Equal(t, int64(0), vault)
	assert.Equal(t, int64(1<<20), f.used(t))

	// Batch delete releases per floor too.
	require.Equal(t, 200, f.putClass(t, "test-bucket", "up2.bin", "GLACIER", testBytes(2<<20)))
	body := `<Delete><Object><Key>down.bin</Key></Object><Object><Key>up2.bin</Key></Object></Delete>`
	req := httptest.NewRequest("POST", "/test-bucket?delete", bytes.NewReader([]byte(body)))
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handleDeleteObjects(w, req, f.s3Req("test-bucket", ""))
	require.Equal(t, 200, w.Code)
	std, vault = f.floors(t)
	assert.Equal(t, int64(0), std)
	assert.Equal(t, int64(0), vault)
	assert.Equal(t, int64(0), f.used(t))
}

func TestFloorAccounting_OverwriteMovesBetweenFloors(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)
	f.house(t, 10<<20, 10<<20)
	require.Equal(t, 200, f.put(t, "x.bin", testBytes(1<<20)))
	// Same key, now put away in the attic: downstairs is released, the attic charged.
	require.Equal(t, 200, f.putClass(t, "test-bucket", "x.bin", "GLACIER", testBytes(3<<20)))

	std, vault := f.floors(t)
	assert.Equal(t, int64(0), std)
	assert.Equal(t, int64(3<<20), vault)
	assert.Equal(t, int64(3<<20), f.used(t))
	assert.Equal(t, usage.FloorVault, f.headFloor(t, "test-bucket", "x.bin"))
}

func TestFloorAccounting_FullFloorRejects(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)
	f.house(t, 10<<20, 1<<20) // a 1 MiB attic

	require.Equal(t, 403, f.putClass(t, "test-bucket", "big.bin", "GLACIER", testBytes(2<<20)),
		"the attic is full even though the total (11 MiB) has room")
	require.Equal(t, 200, f.put(t, "big.bin", testBytes(2<<20)), "downstairs still has room")
	std, vault := f.floors(t)
	assert.Equal(t, int64(2<<20), std)
	assert.Equal(t, int64(0), vault)
	assert.Equal(t, int64(2<<20), f.used(t), "a rejected write reserves nothing")
}

func TestFloorAccounting_NoAtticRefusesArchiveWrites(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)
	f.house(t, 10<<20, 0)

	code := f.putClass(t, "test-bucket", "nope.bin", "GLACIER", testBytes(1<<10))
	assert.Equal(t, 403, code, "no attic was bought")
	assert.Equal(t, int64(0), f.used(t))
}

func TestFloorAccounting_LegacyTenantUnchanged(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 4<<20) // no house: one total quota
	require.Equal(t, 200, f.putClass(t, "test-bucket", "a.bin", "GLACIER", testBytes(3<<20)))
	assert.Equal(t, usage.FloorVault, f.headFloor(t, "test-bucket", "a.bin"), "the floor is still recorded")
	require.Equal(t, 403, f.put(t, "b.bin", testBytes(2<<20)), "the total still applies")
	rows, err := f.qm.GetFloors(context.Background(), f.tenantID)
	require.NoError(t, err)
	assert.Empty(t, rows)
	assert.Equal(t, int64(3<<20), f.used(t))
}

func TestFloorAccounting_CopyIntoTheAttic(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)
	f.house(t, 10<<20, 10<<20)
	const size = 256 << 10
	require.Equal(t, 200, f.put(t, "src.bin", testBytes(size)))

	req := httptest.NewRequest("PUT", "/attic-bucket/copy.bin", nil)
	req.Header.Set("x-amz-copy-source", "/test-bucket/src.bin")
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handleCopyObject(w, req, f.s3Req("attic-bucket", "copy.bin"))
	require.Equal(t, 200, w.Code)

	assert.Equal(t, usage.FloorVault, f.headFloor(t, "attic-bucket", "copy.bin"))
	std, vault := f.floors(t)
	assert.Equal(t, int64(size), std)
	assert.Equal(t, int64(size), vault)
	assert.Equal(t, int64(2*size), f.used(t))
}

func TestFloorAccounting_MultipartIntoTheAttic(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)
	f.house(t, 20<<20, 20<<20)

	initReq := httptest.NewRequest("POST", "/attic-bucket/mp.bin?uploads", nil)
	initReq = initReq.WithContext(f.ctx(initReq.Context()))
	iw := httptest.NewRecorder()
	f.server.handleInitiateMultipartUpload(iw, initReq, "attic-bucket", "mp.bin")
	require.Equal(t, 200, iw.Code)
	var initRes InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(iw.Body.Bytes(), &initRes))

	const partSize = 5 << 20
	for pn := 1; pn <= 2; pn++ {
		part := testBytes(partSize)
		pReq := httptest.NewRequest("PUT",
			fmt.Sprintf("/attic-bucket/mp.bin?uploadId=%s&partNumber=%d", initRes.UploadID, pn),
			bytes.NewReader(part))
		pReq.ContentLength = int64(len(part))
		pReq = pReq.WithContext(f.ctx(pReq.Context()))
		pw := httptest.NewRecorder()
		f.server.handleUploadPart(pw, pReq, "attic-bucket", "mp.bin")
		require.Equal(t, 200, pw.Code)
	}
	cReq := httptest.NewRequest("POST",
		fmt.Sprintf("/attic-bucket/mp.bin?uploadId=%s", initRes.UploadID), nil)
	cReq = cReq.WithContext(f.ctx(cReq.Context()))
	cw := httptest.NewRecorder()
	f.server.handleCompleteMultipartUpload(cw, cReq, "attic-bucket", "mp.bin")
	require.Equal(t, 200, cw.Code)

	assert.Equal(t, usage.FloorVault, f.headFloor(t, "attic-bucket", "mp.bin"))
	std, vault := f.floors(t)
	assert.Equal(t, int64(0), std)
	assert.Equal(t, int64(2*partSize), vault)
	assert.Equal(t, int64(2*partSize), f.used(t))
}

func TestFloorAccounting_ReconcileRepairsFloors(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)
	f.house(t, 10<<20, 10<<20)
	require.Equal(t, 200, f.put(t, "down.bin", testBytes(1<<20)))
	require.Equal(t, 200, f.putClass(t, "test-bucket", "up.bin", "GLACIER", testBytes(2<<20)))
	_, err := f.db.Exec(`UPDATE tenant_floor_quotas SET storage_used_bytes = 12345 WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)

	require.NoError(t, f.qm.ReconcileTenantStorageUsage(context.Background(), f.tenantID))
	std, vault := f.floors(t)
	assert.Equal(t, int64(1<<20), std)
	assert.Equal(t, int64(2<<20), vault)
}
