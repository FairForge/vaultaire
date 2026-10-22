package modes

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/crypto"
)

// RP33-1 a (Stage 1a Prompt 27): the door refuses Private over anything we
// can read, clause by clause. RP33-4: every mode change through the door
// writes one audit row in its transaction.

func (f *dbFixture) cleanupRows(t *testing.T) {
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, f.tenant)
		_, _ = f.db.Exec(`DELETE FROM object_versions WHERE tenant_id = $1`, f.tenant)
		_, _ = f.db.Exec(`DELETE FROM multipart_uploads WHERE tenant_id = $1`, f.tenant)
		_, _ = f.db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, f.tenant)
	})
}

func (f *dbFixture) head(t *testing.T, bucket, key, enc string, chunked bool) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, encryption_algorithm, is_chunked)
		VALUES ($1, $2, $3, 1, 'e', 'text/plain', $4, $5)`, f.tenant, bucket, key, enc, chunked)
	require.NoError(t, err)
}

func (f *dbFixture) version(t *testing.T, bucket, key, id string, enc *string, marker bool) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO object_versions (tenant_id, bucket, object_key, version_id, size_bytes, etag, content_type, is_latest, is_delete_marker, encryption_algorithm)
		VALUES ($1, $2, $3, $4, 1, 'e', 'text/plain', FALSE, $5, $6)`, f.tenant, bucket, key, id, marker, enc)
	require.NoError(t, err)
}

func (f *dbFixture) upload(t *testing.T, bucket, id, status, keyMD5 string) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO multipart_uploads (upload_id, tenant_id, bucket, object_key, status, ssec_key_md5)
		VALUES ($1, $2, $3, 'big.bin', $4, $5)`, id+"-"+f.tenant, f.tenant, bucket, status, keyMD5)
	require.NoError(t, err)
}

func TestSetBucket_PrivateRefusedOverEachReadableKind(t *testing.T) {
	ssec := crypto.SSECAlgorithmV3
	cases := []struct {
		name      string
		seed      func(t *testing.T, f *dbFixture, b string)
		readable  ReadableObjectsError
		allowsKey bool // nothing readable: Private allowed
	}{
		{"plaintext head row", func(t *testing.T, f *dbFixture, b string) { f.head(t, b, "a", "", false) },
			ReadableObjectsError{Objects: 1}, false},
		{"SSE-S3 head row (we hold the key)", func(t *testing.T, f *dbFixture, b string) { f.head(t, b, "a", "AES256", false) },
			ReadableObjectsError{Objects: 1}, false},
		{"chunked head row", func(t *testing.T, f *dbFixture, b string) { f.head(t, b, "a", "", true) },
			ReadableObjectsError{Objects: 1}, false},
		{"every SSE-C format", func(t *testing.T, f *dbFixture, b string) {
			for i, alg := range SSECAlgorithms {
				f.head(t, b, string(rune('a'+i)), alg, false)
			}
		}, ReadableObjectsError{}, true},
		{"unkeyed retained version", func(t *testing.T, f *dbFixture, b string) { f.version(t, b, "a", "v1", nil, false) },
			ReadableObjectsError{Versions: 1}, false},
		{"keyed version and a delete marker", func(t *testing.T, f *dbFixture, b string) {
			f.version(t, b, "a", "v1", &ssec, false)
			f.version(t, b, "a", "v2", nil, true)
		}, ReadableObjectsError{}, true},
		{"unkeyed upload in progress", func(t *testing.T, f *dbFixture, b string) { f.upload(t, b, "u1", "active", "") },
			ReadableObjectsError{Uploads: 1}, false},
		{"keyed upload, and an unkeyed one completed or aborted", func(t *testing.T, f *dbFixture, b string) {
			f.upload(t, b, "u1", "active", "md5==")
			f.upload(t, b, "u2", "completed", "")
			f.upload(t, b, "u3", "aborted", "")
		}, ReadableObjectsError{}, true},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newDB(t)
			f.cleanupRows(t)
			b := "door-" + string(rune('a'+i))
			f.bucket(t, b)
			c.seed(t, f, b)

			got, err := SetBucket(context.Background(), f.db, f.tenant, b, Change{SSECRequired: ptr(true)})

			if c.allowsKey {
				require.NoError(t, err)
				assert.Equal(t, Private, got)
				return
			}
			var r *ReadableObjectsError
			require.ErrorAs(t, err, &r)
			assert.ErrorIs(t, err, ErrReadableObjects)
			assert.Equal(t, c.readable.Objects, r.Objects)
			assert.Equal(t, c.readable.Versions, r.Versions)
			assert.Equal(t, c.readable.Uploads, r.Uploads)
			_, ssecOn, mode := f.columns(t, b)
			assert.False(t, ssecOn, "nothing written")
			assert.Equal(t, Standard, mode)
		})
	}
}

// Taking a key requirement OFF, and saving a Private bucket again, never
// asks what the bucket holds.
func TestSetBucket_LeavingPrivateIsNeverRefused(t *testing.T) {
	f := newDB(t)
	f.cleanupRows(t)
	f.bucket(t, "b")
	_, err := SetBucket(context.Background(), f.db, f.tenant, "b", Change{SSECRequired: ptr(true)})
	require.NoError(t, err)
	f.head(t, "b", "legacy", "", false) // e.g. a row written before 096 by an old slot

	got, err := SetBucket(context.Background(), f.db, f.tenant, "b", Change{SSECRequired: ptr(true)})
	require.NoError(t, err, "already Private: no false→true change")
	assert.Equal(t, Private, got)
	got, err = SetBucket(context.Background(), f.db, f.tenant, "b", Change{SSECRequired: ptr(false)})
	require.NoError(t, err)
	assert.Equal(t, Standard, got)
}

func TestBucketModeChange_Audited(t *testing.T) {
	f := newDB(t)
	f.cleanupRows(t)
	f.bucket(t, "b")
	const actor = "6f1c1d1e-0000-4000-8000-000000000027"
	ctx := audit.WithActor(audit.WithRequest(context.Background(), "198.51.100.27", "p27-agent"), actor)

	steps := []struct {
		c        Change
		from, to string
	}{
		{Change{Visibility: ptr(VisibilityPublic)}, Standard, Public},
		{Change{Visibility: ptr(VisibilityPrivate)}, Public, Standard},
		{Change{SSECRequired: ptr(true)}, Standard, Private},
		{Change{SSECRequired: ptr(true)}, "", ""}, // no change: no row
		{Change{SSECRequired: ptr(false)}, Private, Standard},
	}
	for _, s := range steps {
		_, err := SetBucket(ctx, f.db, f.tenant, "b", s.c)
		require.NoError(t, err)
	}

	rows, err := f.db.Query(`SELECT action, resource, host(ip), user_agent, performed_by::text, metadata FROM audit_logs
		WHERE tenant_id = $1 ORDER BY timestamp, created_at`, f.tenant)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var got []map[string]any
	for rows.Next() {
		var action, resource, ip, ua, by string
		var meta []byte
		require.NoError(t, rows.Scan(&action, &resource, &ip, &ua, &by, &meta))
		assert.Equal(t, ActionModeChanged, action)
		assert.Equal(t, "b", resource)
		assert.Equal(t, "198.51.100.27", ip)
		assert.Equal(t, "p27-agent", ua)
		assert.Equal(t, actor, by)
		var m map[string]any
		require.NoError(t, json.Unmarshal(meta, &m))
		got = append(got, m)
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 4, "one row per change, none for a save that changes nothing")
	want := [][2]string{{Standard, Public}, {Public, Standard}, {Standard, Private}, {Private, Standard}}
	for i, w := range want {
		assert.Equal(t, w[0], got[i]["from"], "row %d", i)
		assert.Equal(t, w[1], got[i]["to"], "row %d", i)
		assert.Equal(t, "b", got[i]["bucket"])
		assert.Contains(t, got[i], "visibility")
		assert.Contains(t, got[i], "ssec_required")
	}
	assert.Equal(t, "public-read", got[0]["visibility"])
	assert.Equal(t, true, got[2]["ssec_required"])
}

// The audit row commits with the change or neither does: a refused change
// leaves none.
func TestBucketModeChange_RefusedLeavesNoAuditRow(t *testing.T) {
	f := newDB(t)
	f.cleanupRows(t)
	f.bucket(t, "b")
	f.head(t, "b", "a", "", false)
	_, err := SetBucket(context.Background(), f.db, f.tenant, "b", Change{SSECRequired: ptr(true)})
	require.ErrorIs(t, err, ErrReadableObjects)
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE tenant_id = $1`, f.tenant).Scan(&n))
	assert.Zero(t, n)
}
