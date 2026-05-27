package crypto

import (
	"context"
	"crypto/mlkem"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/lib/pq"
)

func testMasterKeyHex() string {
	return "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
}

func TestSSEService_NewSSEService(t *testing.T) {
	t.Run("valid key", func(t *testing.T) {
		svc, err := NewSSEService(nil, testMasterKeyHex())
		require.NoError(t, err)
		require.NotNil(t, svc)
	})

	t.Run("invalid hex", func(t *testing.T) {
		_, err := NewSSEService(nil, "not-hex")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "decode master key")
	})

	t.Run("wrong length", func(t *testing.T) {
		_, err := NewSSEService(nil, "0123456789abcdef")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "32 bytes")
	})
}

func TestSSEService_SeedEncryptDecrypt(t *testing.T) {
	masterKey, _ := hex.DecodeString(testMasterKeyHex())

	seed := make([]byte, mlkem.SeedSize)
	_, err := rand.Read(seed)
	require.NoError(t, err)

	encrypted, err := encryptSeed(masterKey, seed)
	require.NoError(t, err)
	assert.Greater(t, len(encrypted), len(seed))

	decrypted, err := decryptSeed(masterKey, encrypted)
	require.NoError(t, err)
	assert.Equal(t, seed, decrypted)
}

func TestSSEService_SeedDecryptWrongKey(t *testing.T) {
	masterKey, _ := hex.DecodeString(testMasterKeyHex())
	wrongKey := make([]byte, 32)
	_, _ = rand.Read(wrongKey)

	seed := make([]byte, mlkem.SeedSize)
	_, _ = rand.Read(seed)

	encrypted, err := encryptSeed(masterKey, seed)
	require.NoError(t, err)

	_, err = decryptSeed(wrongKey, encrypted)
	require.Error(t, err)
}

func TestSSEService_EncryptDecrypt_RoundTrip(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	svc, err := NewSSEService(db, testMasterKeyHex())
	require.NoError(t, err)

	ctx := context.Background()
	tenantID := fmt.Sprintf("sse-test-%d", os.Getpid())

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM tenant_encryption_keys WHERE tenant_id = $1", tenantID)
	})

	err = svc.EnsureTenantKey(ctx, tenantID)
	require.NoError(t, err)

	plaintext := []byte("hello, post-quantum world!")
	ciphertext, err := svc.EncryptBytes(ctx, tenantID, plaintext)
	require.NoError(t, err)

	decrypted, err := svc.DecryptBytes(ctx, tenantID, ciphertext)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}

func TestSSEService_EncryptedFormat(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	svc, err := NewSSEService(db, testMasterKeyHex())
	require.NoError(t, err)

	ctx := context.Background()
	tenantID := fmt.Sprintf("sse-fmt-%d", os.Getpid())

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM tenant_encryption_keys WHERE tenant_id = $1", tenantID)
	})

	require.NoError(t, svc.EnsureTenantKey(ctx, tenantID))

	plaintext := []byte("format check")
	ct, err := svc.EncryptBytes(ctx, tenantID, plaintext)
	require.NoError(t, err)

	assert.Equal(t, SSEVersion, ct[0])
	assert.Equal(t, mlkem.CiphertextSize768, len(ct[1:1+mlkem.CiphertextSize768]))

	nonceStart := 1 + mlkem.CiphertextSize768
	nonce := ct[nonceStart : nonceStart+12]
	assert.Len(t, nonce, 12)
}

func TestSSEService_EncryptedSizeOverhead(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	svc, err := NewSSEService(db, testMasterKeyHex())
	require.NoError(t, err)

	ctx := context.Background()
	tenantID := fmt.Sprintf("sse-size-%d", os.Getpid())

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM tenant_encryption_keys WHERE tenant_id = $1", tenantID)
	})

	require.NoError(t, svc.EnsureTenantKey(ctx, tenantID))

	for _, size := range []int{0, 1, 100, 4096, 65536} {
		plaintext := make([]byte, size)
		_, _ = rand.Read(plaintext)

		ct, err := svc.EncryptBytes(ctx, tenantID, plaintext)
		require.NoError(t, err)
		assert.Equal(t, size+SSEOverheadBytes, len(ct),
			"overhead mismatch for plaintext size %d", size)
	}
}

func TestSSEService_DecryptWrongTenant(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	svc, err := NewSSEService(db, testMasterKeyHex())
	require.NoError(t, err)

	ctx := context.Background()
	tenant1 := fmt.Sprintf("sse-t1-%d", os.Getpid())
	tenant2 := fmt.Sprintf("sse-t2-%d", os.Getpid())

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM tenant_encryption_keys WHERE tenant_id IN ($1, $2)", tenant1, tenant2)
	})

	require.NoError(t, svc.EnsureTenantKey(ctx, tenant1))
	require.NoError(t, svc.EnsureTenantKey(ctx, tenant2))

	ct, err := svc.EncryptBytes(ctx, tenant1, []byte("secret data"))
	require.NoError(t, err)

	_, err = svc.DecryptBytes(ctx, tenant2, ct)
	require.Error(t, err)
}

func TestSSEService_DecryptCorruptedData(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	svc, err := NewSSEService(db, testMasterKeyHex())
	require.NoError(t, err)

	ctx := context.Background()
	tenantID := fmt.Sprintf("sse-corrupt-%d", os.Getpid())

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM tenant_encryption_keys WHERE tenant_id = $1", tenantID)
	})

	require.NoError(t, svc.EnsureTenantKey(ctx, tenantID))

	ct, err := svc.EncryptBytes(ctx, tenantID, []byte("original"))
	require.NoError(t, err)

	tampered := make([]byte, len(ct))
	copy(tampered, ct)
	tampered[len(tampered)-1] ^= 0xff

	_, err = svc.DecryptBytes(ctx, tenantID, tampered)
	require.Error(t, err)
}

func TestSSEService_DecryptBadVersion(t *testing.T) {
	data := make([]byte, 1+mlkem.CiphertextSize768+12+32)
	data[0] = 0xFF

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	svc, err := NewSSEService(db, testMasterKeyHex())
	require.NoError(t, err)

	tenantID := fmt.Sprintf("sse-ver-%d", os.Getpid())
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM tenant_encryption_keys WHERE tenant_id = $1", tenantID)
	})
	require.NoError(t, svc.EnsureTenantKey(context.Background(), tenantID))

	_, err = svc.DecryptBytes(context.Background(), tenantID, data)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported SSE version")
}

func TestSSEService_DecryptTooShort(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	svc, err := NewSSEService(db, testMasterKeyHex())
	require.NoError(t, err)

	tenantID := fmt.Sprintf("sse-short-%d", os.Getpid())
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM tenant_encryption_keys WHERE tenant_id = $1", tenantID)
	})
	require.NoError(t, svc.EnsureTenantKey(context.Background(), tenantID))

	_, err = svc.DecryptBytes(context.Background(), tenantID, []byte("too short"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too short")
}

func TestSSEService_EnsureTenantKey_Idempotent(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	svc, err := NewSSEService(db, testMasterKeyHex())
	require.NoError(t, err)

	ctx := context.Background()
	tenantID := fmt.Sprintf("sse-idem-%d", os.Getpid())

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM tenant_encryption_keys WHERE tenant_id = $1", tenantID)
	})

	require.NoError(t, svc.EnsureTenantKey(ctx, tenantID))
	require.NoError(t, svc.EnsureTenantKey(ctx, tenantID))

	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM tenant_encryption_keys WHERE tenant_id = $1", tenantID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestSSEService_EmptyPlaintext(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	svc, err := NewSSEService(db, testMasterKeyHex())
	require.NoError(t, err)

	ctx := context.Background()
	tenantID := fmt.Sprintf("sse-empty-%d", os.Getpid())

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM tenant_encryption_keys WHERE tenant_id = $1", tenantID)
	})

	require.NoError(t, svc.EnsureTenantKey(ctx, tenantID))

	ct, err := svc.EncryptBytes(ctx, tenantID, []byte{})
	require.NoError(t, err)
	assert.Equal(t, SSEOverheadBytes, len(ct))

	pt, err := svc.DecryptBytes(ctx, tenantID, ct)
	require.NoError(t, err)
	assert.Empty(t, pt)
}

func TestSSEOverheadConstant(t *testing.T) {
	assert.Equal(t, 1117, SSEOverheadBytes)
}

func TestDeriveSSEKey(t *testing.T) {
	key1, err := deriveSSEKey(make([]byte, 32))
	require.NoError(t, err)
	assert.Len(t, key1, 32)

	key2, err := deriveSSEKey(make([]byte, 32))
	require.NoError(t, err)
	assert.Equal(t, key1, key2, "same input should produce same output")

	differentInput := make([]byte, 32)
	differentInput[0] = 1
	key3, err := deriveSSEKey(differentInput)
	require.NoError(t, err)
	assert.NotEqual(t, key1, key3)
}
