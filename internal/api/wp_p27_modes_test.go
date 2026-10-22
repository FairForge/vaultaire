package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/modes"
)

// Stage 1a Prompt 27 Part 0a, RP33-1 (the WP-P26 review, P2 proven): a
// bucket enters Private only when it holds nothing we can read. The door
// (modes.SetBucket) refuses ssec_required false→true over a readable head
// row, a readable retained version or an upload in progress without a key
// (409 bucket_has_readable_objects); every commit of a row with bytes takes
// the bucket-mode lock SHARED and re-reads ssec_required, so a write placed
// before a flip that commits after it is refused (400, as an unkeyed PUT
// into a Private bucket) instead of landing plaintext in a Private bucket.

func (f *versioningFixture) bucketMode(t *testing.T) string {
	t.Helper()
	var mode string
	require.NoError(t, f.db.QueryRow(`SELECT mode FROM buckets WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket).Scan(&mode))
	return mode
}

// readableRows counts the bucket's head rows and retained versions that are
// not sealed with the customer's key.
func (f *versioningFixture) readableRows(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`
		SELECT (SELECT count(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2
		          AND COALESCE(encryption_algorithm, '') NOT LIKE 'AES256-SSE-C%')
		     + (SELECT count(*) FROM object_versions WHERE tenant_id = $1 AND bucket = $2 AND NOT is_delete_marker
		          AND COALESCE(encryption_algorithm, '') NOT LIKE 'AES256-SSE-C%')`,
		f.tenantID, f.bucket).Scan(&n))
	return n
}

func (f *versioningFixture) cleanupUploads(t *testing.T) {
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM multipart_parts WHERE upload_id IN (SELECT upload_id FROM multipart_uploads WHERE tenant_id = $1)`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM multipart_uploads WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM retired_blobs WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, f.tenantID)
	})
}

func TestPrivateMode_RefusedOverReadableObject(t *testing.T) {
	cases := []struct {
		name   string
		seed   func(t *testing.T, f *versioningFixture)
		counts string // the count the refusal names
	}{
		{"an unkeyed object (the review's repro)", func(t *testing.T, f *versioningFixture) {
			require.Equal(t, http.StatusOK, f.s3(t, "PUT", "/"+f.bucket+"/plain.txt", strings.NewReader("readable-by-us"), nil).Code)
		}, `"readable_objects":1`},
		{"a retained unkeyed version behind a delete marker", func(t *testing.T, f *versioningFixture) {
			f.setVersioning(t, "Enabled")
			require.Equal(t, http.StatusOK, f.s3(t, "PUT", "/"+f.bucket+"/plain.txt", strings.NewReader("readable-by-us"), nil).Code)
			require.Equal(t, http.StatusNoContent, f.s3(t, "DELETE", "/"+f.bucket+"/plain.txt", nil, nil).Code)
		}, `"readable_versions":1`},
		{"an upload in progress without a key", func(t *testing.T, f *versioningFixture) {
			f.mpInitiate(t, "big.bin", nil)
		}, `"uploads_without_key":1`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setupVersioningFixture(t)
			f.cleanupUploads(t)
			c.seed(t, f)

			w := f.mgmt(t, "PATCH", "/api/v1/manage/buckets/"+f.bucket, `{"ssec_required": true}`)

			assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), "bucket_has_readable_objects")
			assert.Contains(t, w.Body.String(), c.counts)
			assert.Equal(t, modes.Standard, f.bucketMode(t), "the bucket stays Standard")
		})
	}
	t.Run("the object is still served as before", func(t *testing.T) {
		f := setupVersioningFixture(t)
		require.Equal(t, http.StatusOK, f.s3(t, "PUT", "/"+f.bucket+"/plain.txt", strings.NewReader("readable-by-us"), nil).Code)
		require.Equal(t, http.StatusConflict, f.mgmt(t, "PATCH", "/api/v1/manage/buckets/"+f.bucket, `{"ssec_required": true}`).Code)
		w := f.s3(t, "GET", "/"+f.bucket+"/plain.txt", nil, nil)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "readable-by-us", w.Body.String())
	})
}

func TestPrivateMode_SSECObjectsOnly_Allowed(t *testing.T) {
	f := setupVersioningFixture(t)
	f.cleanupUploads(t)
	f.setVersioning(t, "Enabled")
	// Two keyed writes of a key (a retained keyed version), a keyed object
	// behind a delete marker (markers hold no bytes) and a keyed upload in
	// progress: nothing we can read.
	require.Equal(t, http.StatusOK, f.s3(t, "PUT", "/"+f.bucket+"/a.txt", strings.NewReader("one"), ssecRequiredKeyHeaders()).Code)
	require.Equal(t, http.StatusOK, f.s3(t, "PUT", "/"+f.bucket+"/a.txt", strings.NewReader("two"), ssecRequiredKeyHeaders()).Code)
	require.Equal(t, http.StatusOK, f.s3(t, "PUT", "/"+f.bucket+"/b.txt", strings.NewReader("three"), ssecRequiredKeyHeaders()).Code)
	require.Equal(t, http.StatusNoContent, f.s3(t, "DELETE", "/"+f.bucket+"/b.txt", nil, nil).Code)
	f.mpInitiate(t, "big.bin", ssecRequiredKeyHeaders())

	w := f.mgmt(t, "PATCH", "/api/v1/manage/buckets/"+f.bucket, `{"ssec_required": true}`)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, modes.Private, f.bucketMode(t))
	assert.Zero(t, f.readableRows(t))
}

// The barrier the review asked for: the write is placed (its placement read
// ssec_required = false), then the door commits Private, then the write
// commits — refused at the commit, nothing readable lands in the bucket and
// the blob is queued as an orphan, exactly as a failed commit.
func TestPrivateMode_UnkeyedCommitAfterFlip_Refused(t *testing.T) {
	writers := []struct {
		name  string
		seed  func(t *testing.T, f *versioningFixture)
		write func(t *testing.T, f *versioningFixture) *httptest.ResponseRecorder
	}{
		{"PutObject", nil, func(t *testing.T, f *versioningFixture) *httptest.ResponseRecorder {
			return (f.s3(t, "PUT", "/"+f.bucket+"/late.txt", strings.NewReader("placed before the flip"), nil))
		}},
		{"CopyObject from another bucket", func(t *testing.T, f *versioningFixture) {
			f.secondBucket(t, "p27-src")
			require.Equal(t, http.StatusOK, f.s3(t, "PUT", "/p27-src/src.txt", strings.NewReader("source"), nil).Code)
		}, func(t *testing.T, f *versioningFixture) *httptest.ResponseRecorder {
			return (f.s3(t, "PUT", "/"+f.bucket+"/late.txt", nil, map[string]string{"x-amz-copy-source": "/p27-src/src.txt"}))
		}},
	}
	for _, wr := range writers {
		t.Run(wr.name, func(t *testing.T) {
			f := setupVersioningFixture(t)
			f.cleanupUploads(t)
			if wr.seed != nil {
				wr.seed(t, f)
			}
			var flipErr error
			flipped := false
			prev := blobCommitStartHook
			blobCommitStartHook = func(tenantID, bucket, key string) {
				if tenantID != f.tenantID || bucket != f.bucket || flipped {
					return
				}
				flipped = true
				_, flipErr = modes.SetBucket(context.Background(), f.db, f.tenantID, f.bucket, modes.Change{SSECRequired: ptrTo(true)})
			}
			t.Cleanup(func() { blobCommitStartHook = prev })

			w := wr.write(t, f)

			require.True(t, flipped, "the hook ran")
			require.NoError(t, flipErr, "the bucket was empty: the door let it in")
			assertSSECRequired(t, w)
			assert.Equal(t, modes.Private, f.bucketMode(t))
			assert.Zero(t, f.readableRows(t), "nothing readable in a Private bucket")
			var orphans int
			require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM retired_blobs WHERE tenant_id = $1 AND reason = 'orphan_write'`, f.tenantID).Scan(&orphans))
			assert.Equal(t, 1, orphans, "the blob is queued for the collector, as any failed commit's")
		})
	}
}

// The other order: a write holds its commit (inside the flip's transaction)
// while the door runs. The door waits for it and then counts what it wrote —
// whatever the timing, the bucket never ends Private over plaintext.
func TestPrivateMode_FlipDuringCommit_NeverPrivateOverPlaintext(t *testing.T) {
	f := setupVersioningFixture(t)
	f.cleanupUploads(t)
	done := make(chan error, 1)
	started := false
	prev := headFlipHook
	headFlipHook = func(tenantID, bucket, key string) error {
		if tenantID == f.tenantID && bucket == f.bucket && !started {
			started = true
			go func() {
				_, err := modes.SetBucket(context.Background(), f.db, f.tenantID, f.bucket, modes.Change{SSECRequired: ptrTo(true)})
				done <- err
			}()
			select { // the door either finishes now (no lock) or waits for this commit
			case err := <-done:
				done <- err
			case <-time.After(500 * time.Millisecond):
			}
		}
		return nil
	}
	t.Cleanup(func() { headFlipHook = prev })

	w := f.s3(t, "PUT", "/"+f.bucket+"/racing.txt", strings.NewReader("racing"), nil)

	var flipErr error
	select {
	case flipErr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the door never returned")
	}
	if f.bucketMode(t) == modes.Private {
		assert.Zero(t, f.readableRows(t), "Private over plaintext (PUT %d, door %v)", w.Code, flipErr)
	} else {
		assert.ErrorIs(t, flipErr, modes.ErrReadableObjects, "the door saw the committed write")
		assert.Equal(t, http.StatusOK, w.Code)
	}
}

// RP33-1 c — the plan's "set at creation through the SDK/API" (line 1699):
// POST /buckets with "mode": "private" creates the bucket Private in one
// transaction through the door; "public" stays the dashboard's (F-32).
func TestCreateBucket_ModePrivate(t *testing.T) {
	f := setupVersioningFixture(t)
	f.cleanupUploads(t)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1 AND name LIKE 'p27-%'`, f.tenantID)
	})
	create := func(body string) (*httptest.ResponseRecorder, map[string]any) {
		w := f.mgmtBucketCall(t, "POST", "", "", body, f.server.handleMgmtCreateBucket)
		var got map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		return (w), got
	}
	modeOf := func(name string) string {
		var m string
		require.NoError(t, f.db.QueryRow(`SELECT mode FROM buckets WHERE tenant_id = $1 AND name = $2`, f.tenantID, name).Scan(&m))
		return m
	}

	w, got := create(`{"name": "p27-private", "mode": "private"}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, modes.Private, got["mode"])
	assert.Equal(t, modes.Private, modeOf("p27-private"))
	var audited int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'bucket.mode_changed' AND resource = 'p27-private'`, f.tenantID).Scan(&audited))
	assert.Equal(t, 1, audited, "the create's mode change is audited like any other")

	w, got = create(`{"name": "p27-standard", "mode": "standard"}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, modes.Standard, got["mode"])
	assert.Equal(t, modes.Standard, modeOf("p27-standard"))

	w, _ = create(`{"name": "p27-default"}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, modes.Standard, modeOf("p27-default"), "omitted = Standard, as before")

	for _, bad := range []string{"public", "vault", "PRIVATE"} {
		w, _ = create(`{"name": "p27-bad", "mode": "` + bad + `"}`)
		assert.Equal(t, http.StatusBadRequest, w.Code, bad)
		assert.Contains(t, w.Body.String(), "invalid_mode", bad)
	}
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM buckets WHERE tenant_id = $1 AND name = 'p27-bad'`, f.tenantID).Scan(&n))
	assert.Zero(t, n, "nothing created on a refused mode")
}

// secondBucket registers another bucket of the fixture's tenant.
func (f *versioningFixture) secondBucket(t *testing.T, name string) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, $2, 'private') ON CONFLICT DO NOTHING`, f.tenantID, name)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(f.tempDir, f.tenant.NamespaceContainer(name)), 0o755))
}

func ptrTo[T any](v T) *T { return &v }
