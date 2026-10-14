package api

// WP-VM-1 Part D: the gate lifted, dark first. With the tenant's
// `versioned_chunking` flag on, a PUT, a CompleteMultipartUpload and the
// copy's manifest branch chunk into Enabled and Suspended buckets; off (the
// default) they store whole as before. Reads never depend on the flag.
// "Before" = these tests on WP-VM-1 Parts A–C.

import (
	"context"
	"net/http"
	"testing"

	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// versionedChunkingFor turns the flag on for the fixture's tenant only (a
// per-tenant row: a global one would reach every package on the database).
func versionedChunkingFor(t *testing.T, f *mpChunkFixture, on bool) {
	t.Helper()
	svc := flags.New(f.db, zap.NewNop())
	registerFlags(svc)
	require.NoError(t, svc.Set(context.Background(), "versioned_chunking", f.tenantID, on, "test"))
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM feature_flags WHERE flag_key = 'versioned_chunking' AND tenant_id = $1`, f.tenantID)
	})
	f.server.flags = svc
}

func TestVM1_D_VersionedChunking_ThreeChunkedVersionsShareTheirChunks(t *testing.T) {
	for _, status := range []string{"Enabled", "Suspended"} {
		t.Run(status, func(t *testing.T) {
			f := setupMPChunkFixture(t, false)
			f.versioning(t, status)
			versionedChunkingFor(t, f, true)
			bodies := [][]byte{vbody(f, 8192, 1), vbody(f, 8192, 2), vbody(f, 8192, 3)}
			var vids []string
			for _, b := range bodies {
				w := f.req(t, "PUT", "/test-bucket/v.bin", b, nil)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				vids = append(vids, w.Header().Get("x-amz-version-id"))
				chunked, _ := f.head(t, "v.bin")
				assert.True(t, chunked, "a versioned PUT chunks with the flag on")
			}
			assert.Equal(t, bodies[2], f.getBytes(t, "v.bin", ""))
			if status == "Enabled" {
				for i, vid := range vids {
					assert.Equal(t, bodies[i], f.getBytes(t, "v.bin", vid), "version %d", i)
				}
				assert.Equal(t, int64(3*8192), f.used(t), "every version billed at its logical size")
			} else {
				assert.Equal(t, int64(8192), f.used(t), "Suspended: the null version is rewritten")
			}
			f.ledgerAgrees(t, "after three versions")
		})
	}
}

func TestVM1_D_VersionedChunking_CompleteAndCopyChunkIntoAVersionedBucket(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	f.versioning(t, "Enabled")
	versionedChunkingFor(t, f, true)
	body := vbody(f, 12<<10, 4)
	f.multipart(t, "mp.bin", body, 3, nil)
	chunked, _ := f.head(t, "mp.bin")
	assert.True(t, chunked, "Complete is placed as a PUT (WP-R3-1)")

	w := f.req(t, "PUT", "/test-bucket/cp.bin", nil, map[string]string{"x-amz-copy-source": "/test-bucket/mp.bin"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	chunked, _ = f.head(t, "cp.bin")
	assert.True(t, chunked, "a manifest copy into a versioned bucket")
	assert.NotEmpty(t, w.Header().Get("x-amz-version-id"))
	assert.Equal(t, body, f.getBytes(t, "cp.bin", ""))
	f.ledgerAgrees(t, "after the complete and the copy")
}

func TestVM1_D_VersionedChunking_DefaultOff_ReadsSurviveTurningItOff(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	f.versioning(t, "Enabled")
	// Off by default: whole, as before.
	require.Equal(t, http.StatusOK, f.req(t, "PUT", "/test-bucket/off.bin", vbody(f, 8192, 5), nil).Code)
	chunked, _ := f.head(t, "off.bin")
	assert.False(t, chunked)

	versionedChunkingFor(t, f, true)
	a := vbody(f, 8192, 6)
	w := f.req(t, "PUT", "/test-bucket/on.bin", a, nil)
	require.Equal(t, http.StatusOK, w.Code)
	v1 := w.Header().Get("x-amz-version-id")
	require.Equal(t, http.StatusOK, f.req(t, "PUT", "/test-bucket/on.bin", vbody(f, 8192, 7), nil).Code)

	versionedChunkingFor(t, f, false)
	assert.Equal(t, a, f.getBytes(t, "on.bin", v1), "a chunked version written while the flag was on reads after it goes off")
	assert.Equal(t, vbody(f, 8192, 7), f.getBytes(t, "on.bin", ""))
}
