package api

// WP-R8-3 A.2 (R8-10): a sealed blob copied from key A to key B's address
// under the same tenant decrypted and was served as B — every segment's AAD
// was the stream header alone. Every writer now seals format 0x04 for the
// DESTINATION's (tenant, bucket, key) and every opener opens for the object
// it reads: a blob of another key does not open, before any status line, on
// the GET, the parallel sealed GET, /cdn, a copy source and the re-encrypt.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" // #nosec G501 -- the S3 ETag
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/crypto/hkdf"
)

// swapBlob puts the bytes of from's blob at to's address (the same tenant,
// the same size): what a backend that mixed up two addresses would serve.
func (f *sealedFixture) swapBlob(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(f.blobFile(t, from))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.blobFile(t, to), b, 0o600))
}

func TestSSEIdentity_EveryWriterSeals0x04ForTheDestination(t *testing.T) {
	// Arrange + Act: a PUT, a CopyObject (whole), a CompleteMultipartUpload.
	f := setupSealedFixture(t)
	body := mpBody(5000, 3)
	require.Equal(t, http.StatusOK, f.putWith(t, "put.bin", body, nil).Code)
	cp := httptest.NewRequest("PUT", "/test-bucket/copy.bin", nil)
	cp.Header.Set("x-amz-copy-source", "/test-bucket/put.bin")
	cp = cp.WithContext(s3Ctx(cp.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, cp)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Assert: each blob is 0x04 and opens as its own key only.
	for _, key := range []string{"put.bin", "copy.bin"} {
		raw, err := os.ReadFile(f.blobFile(t, key))
		require.NoError(t, err)
		assert.Equal(t, crypto.StreamFormat, raw[0], key)
		assert.Equal(t, byte(0x04), raw[0], key)
		_, err = f.server.sseService.Open(context.Background(), crypto.StreamIdentity{Tenant: f.tenantID, Bucket: "test-bucket", Key: key}, bytes.NewReader(raw))
		assert.NoError(t, err, "%s opens as itself", key)
		_, err = f.server.sseService.Open(context.Background(), crypto.StreamIdentity{Tenant: f.tenantID, Bucket: "test-bucket", Key: "other.bin"}, bytes.NewReader(raw))
		assert.ErrorIs(t, err, crypto.ErrStreamCorrupt, "%s does not open as another key", key)
	}
	got := f.getWith(t, "copy.bin", "", nil)
	require.Equal(t, http.StatusOK, got.Code)
	assert.Equal(t, body, got.Body.Bytes())
}

func TestSSEIdentity_ABlobCopiedToAnotherKeyIsRefusedBeforeTheStatusLine(t *testing.T) {
	// Arrange: A and B, same tenant, same size; B's address holds A's blob.
	f := setupSealedFixture(t)
	a, b := mpBody(9000, 1), mpBody(9000, 2)
	require.Equal(t, http.StatusOK, f.putWith(t, "a.bin", a, nil).Code)
	require.Equal(t, http.StatusOK, f.putWith(t, "b.bin", b, nil).Code)
	f.swapBlob(t, "a.bin", "b.bin")

	// Act + Assert: whole and ranged GET — 500, none of A's bytes, no cut.
	for _, rng := range []string{"", "bytes=1500-2600"} {
		w, cut := f.getRecovering(t, "b.bin", rng)
		assert.False(t, cut, "refused before the status line (%q)", rng)
		assert.Equal(t, http.StatusInternalServerError, w.Code, "range %q: %s", rng, w.Body.String())
		assert.False(t, bytes.Contains(w.Body.Bytes(), a[1500:1600]), "none of A's bytes")
	}

	// A copy whose source is B: refused, nothing written.
	cp := httptest.NewRequest("PUT", "/test-bucket/c.bin", nil)
	cp.Header.Set("x-amz-copy-source", "/test-bucket/b.bin")
	cp = cp.WithContext(s3Ctx(cp.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, cp)
	assert.GreaterOrEqual(t, w.Code, 500, w.Body.String())
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'c.bin'`, f.tenantID).Scan(&n))
	assert.Zero(t, n)

	// A itself still opens.
	ok := f.getWith(t, "a.bin", "", nil)
	require.Equal(t, http.StatusOK, ok.Code)
	assert.Equal(t, a, ok.Body.Bytes())
}

func TestSSEIdentity_AParallelSealedGetOfAnotherKeysBlobIsRefused(t *testing.T) {
	// Arrange: the parallel sealed reader (Prompt 8 Part B) derives the key
	// once and opens each segment — for the object it reads.
	f := setupSealedParallelFixture(t, true)
	a, b := randomBytes(9<<20+3), randomBytes(9<<20+3)
	b[0] ^= 1
	f.putSealed(t, "a.bin", a, nil)
	f.putSealed(t, "b.bin", b, nil)
	f.drv.mu.Lock()
	var aKey, bKey string
	for k := range f.drv.objects {
		switch {
		case strings.Contains(k, "/a.bin@"):
			aKey = k
		case strings.Contains(k, "/b.bin@"):
			bKey = k
		}
	}
	require.NotEmpty(t, aKey)
	require.NotEmpty(t, bKey)
	f.drv.objects[bKey] = append([]byte(nil), f.drv.objects[aKey]...)
	f.drv.mu.Unlock()

	// Act
	w := f.get(t, "b.bin", nil)

	// Assert
	assert.False(t, f.cut)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.False(t, bytes.Contains(w.Body.Bytes(), a[:256]))
}

// The CDN fixture names its tenant after the test: a short name.
func TestSSEIdentity_CDNOtherKeysBlob(t *testing.T) {
	// Arrange: the public object's address holds a blob sealed for another
	// key of the same bucket.
	f := setupCDNFixture(t)
	sse := sseFor(t, f.db, f.tenantID)
	content := mpBody(3000, 4)
	ct, v := sealedBlob(t, sse, crypto.StreamIdentity{Tenant: f.tenantID, Bucket: f.bucket, Key: "another.txt"}, content)
	ctx := common.WithTenantID(context.Background(), f.tenantID)
	_, err := f.server.engine.Put(ctx, f.tenantID+"_"+f.bucket, f.key, bytes.NewReader(ct))
	require.NoError(t, err)
	sum := md5.Sum(content) // #nosec G401
	_, err = f.db.Exec(`UPDATE object_head_cache SET encryption_algorithm = $4, encryption_key_version = $5, size_bytes = $6, etag = $7
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, f.tenantID, f.bucket, f.key, crypto.SSEAlgorithm, v, len(content), hex.EncodeToString(sum[:]))
	require.NoError(t, err)
	f.server.sseService = sse

	// Act
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil))

	// Assert
	assert.GreaterOrEqual(t, w.Code, 500)
	assert.False(t, bytes.Contains(w.Body.Bytes(), content[:64]))
}

// legacySSEV3Blob is a 0x03 SSE-S3 blob exactly as the writers of WP-R8-5
// made it — built from its own primitives (the format's spec in
// internal/crypto/stream.go), so the compatibility is tested against the old
// bytes, not against the new code: AAD = the header alone.
func legacySSEV3Blob(t *testing.T, keys *crypto.TenantKeys, tenantID string, pt []byte, seg int) ([]byte, int) {
	t.Helper()
	kv, err := keys.Current(context.Background(), tenantID)
	require.NoError(t, err)
	shared, kemCT := kv.Encap.Encapsulate()
	header := []byte{0x03, crypto.StreamKindSSES3}
	header = binary.BigEndian.AppendUint32(header, uint32(kv.Version)) // #nosec G115 -- a test's version
	header = append(header, kemCT...)
	header = binary.BigEndian.AppendUint32(header, uint32(seg)) // #nosec G115 -- a test's segment size
	prefix := make([]byte, 7)
	_, err = rand.Read(prefix)
	require.NoError(t, err)
	header = append(header, prefix...)
	key := make([]byte, 32)
	_, err = io.ReadFull(hkdf.New(sha256.New, shared, nil, append([]byte("vaultaire-sse-stream-v3"), header...)), key)
	require.NoError(t, err)
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	out := append([]byte(nil), header...)
	n := max(1, (len(pt)+seg-1)/seg)
	for i := 0; i < n; i++ {
		end := min((i+1)*seg, len(pt))
		nonce := make([]byte, 12)
		copy(nonce, prefix)
		binary.BigEndian.PutUint32(nonce[7:], uint32(i)) // #nosec G115 -- a test's segment count
		if i == n-1 {
			nonce[11] = 1
		}
		out = gcm.Seal(out, nonce, pt[i*seg:end], header)
	}
	return out, kv.Version
}

func TestSSEIdentity_A0x03BlobStillOpensAndTheReencryptMigratesItTo0x04(t *testing.T) {
	// Arrange: an object whose blob is in the 0x03 format (written before
	// this change — labs and tests only: prod has no master key).
	f := setupSealedFixture(t)
	keys := testTenantKeys(t, f.db, keyTestMasterA)
	f.server.sseService = crypto.NewSSEService(keys) // one key store for the server and the rotation
	f.server.sseService.SetSegmentSize(sealedSeg)
	countSSEOpens(f.server.sseService)
	body := mpBody(4000, 6)
	require.Equal(t, http.StatusOK, f.putWith(t, "old.bin", body, nil).Code)
	v3, _ := legacySSEV3Blob(t, keys, f.tenantID, body, sealedSeg)
	require.NoError(t, os.WriteFile(f.blobFile(t, "old.bin"), v3, 0o600))
	opened := promtest.ToFloat64(sseBlobsOpened.WithLabelValues("0x03"))

	// Act + Assert: GET whole and by range.
	w := f.getWith(t, "old.bin", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, body, w.Body.Bytes())
	r := f.getWith(t, "old.bin", "bytes=1000-2999", nil)
	require.Equal(t, http.StatusPartialContent, r.Code)
	assert.Equal(t, body[1000:3000], r.Body.Bytes())
	assert.GreaterOrEqual(t, promtest.ToFloat64(sseBlobsOpened.WithLabelValues("0x03")), opened+2)

	// The whole-object re-encrypt re-seals it for its own key: 0x04.
	_, err := keys.Rotate(context.Background(), f.tenantID)
	require.NoError(t, err)
	runner := NewKeyReencryptRunner(f.db, f.server.engine, f.server.gci, crypto.NewChunkEncryptionService(keys), zap.NewNop())
	require.NotNil(t, runner)
	runner.sse = f.server.sseService
	runner.onlyScope = f.tenantID
	runner.JobName = "test_key_reencrypt_p13_" + f.tenantID[:8]
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM job_runs WHERE job = $1`, runner.JobName) })
	res, err := runner.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.ObjectsReencrypted, "%+v", res)
	raw, err := os.ReadFile(f.blobFile(t, "old.bin"))
	require.NoError(t, err)
	assert.Equal(t, crypto.StreamFormat, raw[0], "migrated to 0x04 by the re-encrypt")
	again := f.getWith(t, "old.bin", "", nil)
	require.Equal(t, http.StatusOK, again.Code)
	assert.Equal(t, body, again.Body.Bytes())
}

func TestSSEIdentity_ARetainedVersionOpensAsItsKey(t *testing.T) {
	// The identity is the KEY, never the address: a retained version's blob
	// sits at its own address and was sealed for the key.
	f := setupSealedFixture(t)
	v1, v2 := mpBody(3000, 7), mpBody(3100, 8)
	w := f.putWith(t, "doc.bin", v1, nil)
	require.Equal(t, http.StatusOK, w.Code)
	vid := w.Header().Get("x-amz-version-id")
	require.NotEmpty(t, vid)
	require.Equal(t, http.StatusOK, f.putWith(t, "doc.bin", v2, nil).Code)

	r := httptest.NewRequest("GET", "/test-bucket/doc.bin?versionId="+vid, nil)
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	got := httptest.NewRecorder()
	f.server.handleS3Request(got, r)

	require.Equal(t, http.StatusOK, got.Code, got.Body.String())
	assert.Equal(t, v1, got.Body.Bytes())
}

func TestSSEPaths_EveryOpenerAndSealerNamesTheObject(t *testing.T) {
	// Every sseS3Opener / sseCOpener literal sets its identity, and every
	// crypto.StreamIdentity literal names all three fields — the seal and
	// open refuse an incomplete one at run time (crypto.ErrStreamIdentity);
	// this keeps a caller from shipping one.
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	var violations []string
	seen := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			typ := ""
			switch tt := lit.Type.(type) {
			case *ast.Ident:
				typ = tt.Name
			case *ast.SelectorExpr:
				typ = tt.Sel.Name
			}
			keys := map[string]bool{}
			for _, el := range lit.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok {
						keys[id.Name] = true
					}
				}
			}
			switch typ {
			case "sseS3Opener", "sseCOpener":
				seen++
				if !keys["id"] {
					violations = append(violations, fset.Position(lit.Pos()).String()+": "+typ+" without id")
				}
			case "StreamIdentity":
				seen++
				if !keys["Tenant"] || !keys["Bucket"] || !keys["Key"] {
					violations = append(violations, fset.Position(lit.Pos()).String()+": StreamIdentity missing a field")
				}
			}
			return true
		})
	}
	assert.Positive(t, seen)
	assert.Empty(t, violations)
}
