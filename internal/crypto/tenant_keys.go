package crypto

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/mlkem"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// TenantKeys is the tenant key store (WP-R8-1): per tenant, numbered
// versions, each holding two RANDOM secrets — the 32-byte chunk key (the
// input of every convergent chunk key) and the ML-KEM-768 seed of the SSE-S3
// decapsulation key — wrapped under ENCRYPTION_MASTER_KEY in
// tenant_key_versions (migration 084).
//
// Why stored and random, not derived: a key derived from the master key
// (the HKDF of KeyManager, which this replaces) can be neither rotated —
// every version is a function of the same master — nor shredded while the
// master lives. Here a version is a row: Rotate adds one, a write uses the
// newest, a read uses the one its blob records, and deleting a tenant's rows
// (account.EraseRows) makes every blob sealed under them unreadable for
// good. Re-keying the master re-wraps the rows; no data is re-encrypted.
type TenantKeys struct {
	db       *sql.DB
	master   []byte
	previous []byte // ENCRYPTION_MASTER_KEY_PREVIOUS, only during a master re-key
	now      func() time.Time

	mu       sync.Mutex
	versions map[versionKey]cachedVersion // (tenant, version) → unwrapped key
	current  map[string]cachedCurrent     // tenant → newest version number
}

// KeyVersion is one unwrapped tenant key version.
type KeyVersion struct {
	Version  int
	ChunkKey [32]byte
	KEM      *mlkem.DecapsulationKey768
	Encap    *mlkem.EncapsulationKey768
}

type versionKey struct {
	tenant  string
	version int
}

type cachedVersion struct {
	kv *KeyVersion
	at time.Time
}

type cachedCurrent struct {
	version int
	at      time.Time
}

const (
	// currentKeyTTL bounds how long a process keeps writing with a version
	// another process has since rotated past. A write in that window records
	// the version it used, so it stays readable; the re-encrypt job moves it.
	currentKeyTTL = 60 * time.Second
	// keyVersionTTL bounds how long unwrapped key material stays in this
	// process after its last load: a version never changes, but an erased
	// tenant's keys should not outlive the erasure in a process's memory
	// (Forget covers the process that ran it).
	keyVersionTTL = time.Hour
	// keyVersionCacheMax bounds the unwrapped-key cache.
	keyVersionCacheMax = 10000

	wrapFormat      byte = 0x01
	purposeChunkKey      = "chunk-key"
	purposeKEMSeed       = "kem-seed"
)

var (
	// ErrNoTenantKey: the tenant has no key of that version — never created,
	// or shredded with the account. Blobs sealed under it are unreadable.
	ErrNoTenantKey = errors.New("tenant key version not found")
	// ErrKeyUnwrap: the row exists but does not unwrap under the master key
	// (nor under ENCRYPTION_MASTER_KEY_PREVIOUS): the wrong master key is
	// configured. Reads fail closed — never ciphertext (R8-04).
	ErrKeyUnwrap = errors.New("tenant key does not unwrap under the configured master key")
)

// NewTenantKeys builds the store. masterHex is ENCRYPTION_MASTER_KEY (64 hex
// characters); previousHex is ENCRYPTION_MASTER_KEY_PREVIOUS, "" when no
// master re-key is in progress.
func NewTenantKeys(db *sql.DB, masterHex, previousHex string) (*TenantKeys, error) {
	master, err := parseMasterKey(masterHex)
	if err != nil {
		return nil, fmt.Errorf("ENCRYPTION_MASTER_KEY: %w", err)
	}
	k := &TenantKeys{
		db:       db,
		master:   master,
		now:      time.Now,
		versions: map[versionKey]cachedVersion{},
		current:  map[string]cachedCurrent{},
	}
	if previousHex != "" {
		prev, err := parseMasterKey(previousHex)
		if err != nil {
			return nil, fmt.Errorf("ENCRYPTION_MASTER_KEY_PREVIOUS: %w", err)
		}
		if bytes.Equal(prev, master) {
			return nil, errors.New("ENCRYPTION_MASTER_KEY_PREVIOUS equals ENCRYPTION_MASTER_KEY")
		}
		k.previous = prev
	}
	return k, nil
}

func parseMasterKey(h string) ([]byte, error) {
	key, err := hex.DecodeString(h)
	if err != nil {
		return nil, fmt.Errorf("decode master key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(key))
	}
	if bytes.Equal(key, make([]byte, 32)) {
		return nil, errors.New("master key is all zeros")
	}
	return key, nil
}

// HasPrevious reports whether a master re-key is in progress.
func (k *TenantKeys) HasPrevious() bool { return k.previous != nil }

// Current returns the tenant's newest key version, creating version 1 on the
// first call for a tenant. The number is cached for currentKeyTTL.
func (k *TenantKeys) Current(ctx context.Context, tenantID string) (*KeyVersion, error) {
	if tenantID == "" {
		return nil, errors.New("tenant key: tenant ID required")
	}
	k.mu.Lock()
	c, ok := k.current[tenantID]
	k.mu.Unlock()
	if ok && k.now().Sub(c.at) < currentKeyTTL {
		return k.Version(ctx, tenantID, c.version)
	}
	v, err := k.CurrentVersion(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if v == 0 {
		if v, err = k.createFirst(ctx, tenantID); err != nil {
			return nil, err
		}
	}
	k.mu.Lock()
	k.current[tenantID] = cachedCurrent{version: v, at: k.now()}
	k.mu.Unlock()
	return k.Version(ctx, tenantID, v)
}

// CurrentVersion reads the tenant's newest version number from the table,
// past the cache (0 = the tenant has no key yet). The re-encrypt job uses it:
// it must never "re-encrypt" to a version another process has rotated past.
func (k *TenantKeys) CurrentVersion(ctx context.Context, tenantID string) (int, error) {
	var v int
	if err := k.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(key_version), 0) FROM tenant_key_versions WHERE tenant_id = $1`,
		tenantID).Scan(&v); err != nil {
		return 0, fmt.Errorf("read current key version of %s: %w", tenantID, err)
	}
	return v, nil
}

// lockTenantKeys serialises key creation and rotation for one tenant. The
// two-key form, with a namespace of its own: the one-key space is the jobs'
// and the chunk lock's (hashtext(scope), hashtext(hash)) would need a scope
// whose hash equals the namespace's to meet it.
const lockTenantKeys = `SELECT pg_advisory_xact_lock(hashtext('tenant_key_versions'), hashtext($1))`

// createFirst creates version 1 under the tenant's lock. Two first writers
// on two processes agree: the loser's INSERT does nothing and both read the
// winner's row back — never their own freshly generated secrets.
func (k *TenantKeys) createFirst(ctx context.Context, tenantID string) (int, error) {
	tx, err := k.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("create tenant key: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, lockTenantKeys, tenantID); err != nil {
		return 0, fmt.Errorf("create tenant key: lock: %w", err)
	}
	if err := k.insertVersion(ctx, tx, tenantID, 1); err != nil {
		return 0, err
	}
	var v int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(key_version), 0) FROM tenant_key_versions WHERE tenant_id = $1`,
		tenantID).Scan(&v); err != nil {
		return 0, fmt.Errorf("create tenant key: re-read: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("create tenant key: commit: %w", err)
	}
	return v, nil
}

// Rotate adds the tenant's next version and retires the one before it. New
// writes use the new version at once in this process and within
// currentKeyTTL everywhere else; reads keep using the version each blob
// records. A tenant with no key gets version 1.
func (k *TenantKeys) Rotate(ctx context.Context, tenantID string) (int, error) {
	if tenantID == "" {
		return 0, errors.New("rotate tenant key: tenant ID required")
	}
	tx, err := k.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("rotate tenant key: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, lockTenantKeys, tenantID); err != nil {
		return 0, fmt.Errorf("rotate tenant key: lock: %w", err)
	}
	var cur int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(key_version), 0) FROM tenant_key_versions WHERE tenant_id = $1`,
		tenantID).Scan(&cur); err != nil {
		return 0, fmt.Errorf("rotate tenant key: read current: %w", err)
	}
	next := cur + 1
	if err := k.insertVersion(ctx, tx, tenantID, next); err != nil {
		return 0, err
	}
	if cur > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE tenant_key_versions SET retired_at = NOW()
			 WHERE tenant_id = $1 AND key_version = $2 AND retired_at IS NULL`, tenantID, cur); err != nil {
			return 0, fmt.Errorf("rotate tenant key: retire v%d: %w", cur, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("rotate tenant key: commit: %w", err)
	}
	k.mu.Lock()
	k.current[tenantID] = cachedCurrent{version: next, at: k.now()}
	k.mu.Unlock()
	return next, nil
}

// insertVersion generates a version's secrets and stores them wrapped.
func (k *TenantKeys) insertVersion(ctx context.Context, tx *sql.Tx, tenantID string, version int) error {
	var chunkKey [32]byte
	if _, err := io.ReadFull(rand.Reader, chunkKey[:]); err != nil {
		return fmt.Errorf("generate chunk key: %w", err)
	}
	dk, err := mlkem.GenerateKey768()
	if err != nil {
		return fmt.Errorf("generate ML-KEM key: %w", err)
	}
	ckw, err := wrapKey(k.master, tenantID, version, purposeChunkKey, chunkKey[:])
	if err != nil {
		return err
	}
	ksw, err := wrapKey(k.master, tenantID, version, purposeKEMSeed, dk.Bytes())
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenant_key_versions (tenant_id, key_version, chunk_key_wrapped, kem_seed_wrapped, kem_public_key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, key_version) DO NOTHING`,
		tenantID, version, ckw, ksw, dk.EncapsulationKey().Bytes()); err != nil {
		return fmt.Errorf("store tenant key v%d: %w", version, err)
	}
	return nil
}

// Version returns one version of the tenant's key, unwrapped. A version
// never changes, so it is cached (bounded, keyVersionTTL).
func (k *TenantKeys) Version(ctx context.Context, tenantID string, version int) (*KeyVersion, error) {
	if version < 1 {
		return nil, fmt.Errorf("%w: %s v%d", ErrNoTenantKey, tenantID, version)
	}
	vk := versionKey{tenantID, version}
	k.mu.Lock()
	c, ok := k.versions[vk]
	k.mu.Unlock()
	if ok && k.now().Sub(c.at) < keyVersionTTL {
		return c.kv, nil
	}

	var ckw, ksw, pub []byte
	err := k.db.QueryRowContext(ctx, `
		SELECT chunk_key_wrapped, kem_seed_wrapped, kem_public_key
		  FROM tenant_key_versions WHERE tenant_id = $1 AND key_version = $2`,
		tenantID, version).Scan(&ckw, &ksw, &pub)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s v%d", ErrNoTenantKey, tenantID, version)
	}
	if err != nil {
		return nil, fmt.Errorf("load tenant key %s v%d: %w", tenantID, version, err)
	}
	kv, _, err := k.unwrapVersion(tenantID, version, ckw, ksw, pub)
	if err != nil {
		return nil, err
	}

	k.mu.Lock()
	if len(k.versions) >= keyVersionCacheMax {
		n := 0
		for key := range k.versions {
			delete(k.versions, key)
			if n++; n >= keyVersionCacheMax/2 {
				break
			}
		}
	}
	k.versions[vk] = cachedVersion{kv: kv, at: k.now()}
	k.mu.Unlock()
	return kv, nil
}

// unwrapVersion opens a row's two wrapped secrets under the master key, else
// under the previous one (usedPrevious = true: the row awaits its re-wrap).
func (k *TenantKeys) unwrapVersion(tenantID string, version int, ckw, ksw, pub []byte) (*KeyVersion, bool, error) {
	for i, master := range [][]byte{k.master, k.previous} {
		if master == nil {
			continue
		}
		chunkKey, err := unwrapKey(master, tenantID, version, purposeChunkKey, ckw)
		if err != nil {
			continue
		}
		seed, err := unwrapKey(master, tenantID, version, purposeKEMSeed, ksw)
		if err != nil {
			continue
		}
		kv, err := newKeyVersion(version, chunkKey, seed, pub)
		if err != nil {
			return nil, false, fmt.Errorf("tenant key %s v%d: %w", tenantID, version, err)
		}
		return kv, i == 1, nil
	}
	return nil, false, fmt.Errorf("%w: %s v%d", ErrKeyUnwrap, tenantID, version)
}

func newKeyVersion(version int, chunkKey, seed, pub []byte) (*KeyVersion, error) {
	if len(chunkKey) != 32 {
		return nil, fmt.Errorf("chunk key is %d bytes", len(chunkKey))
	}
	dk, err := mlkem.NewDecapsulationKey768(seed)
	if err != nil {
		return nil, fmt.Errorf("reconstruct decapsulation key: %w", err)
	}
	if !bytes.Equal(dk.EncapsulationKey().Bytes(), pub) {
		return nil, errors.New("stored public key does not match the seed")
	}
	kv := &KeyVersion{Version: version, KEM: dk, Encap: dk.EncapsulationKey()}
	copy(kv.ChunkKey[:], chunkKey)
	return kv, nil
}

// Forget drops everything this process holds for a tenant (the erasure: the
// rows are gone, the unwrapped copies go with them).
func (k *TenantKeys) Forget(tenantID string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.current, tenantID)
	for vk := range k.versions {
		if vk.tenant == tenantID {
			delete(k.versions, vk)
		}
	}
}

// RewrapResult is what one pass of RewrapAll found.
type RewrapResult struct {
	Rows      int // rows examined
	Rewrapped int // opened under the previous master key, re-wrapped under the current
	// Failed rows unwrap under neither key: their tenant's encrypted reads
	// fail (500, never ciphertext) until the right master key is configured.
	Failed        []string // "tenant vN"
	RewrapErrored int      // rows that unwrapped but could not be written back
}

// RewrapAll is the boot pass of a master re-key: every row that opens under
// ENCRYPTION_MASTER_KEY_PREVIOUS and not under ENCRYPTION_MASTER_KEY is
// re-wrapped under the latter. Without a previous key it only checks that
// every row opens. No data is re-encrypted: the wrapped secrets are the same
// bytes before and after.
func (k *TenantKeys) RewrapAll(ctx context.Context) (RewrapResult, error) {
	var res RewrapResult
	type row struct {
		tenant        string
		version       int
		ckw, ksw, pub []byte
	}
	rows, err := k.db.QueryContext(ctx, `
		SELECT tenant_id, key_version, chunk_key_wrapped, kem_seed_wrapped, kem_public_key
		  FROM tenant_key_versions ORDER BY tenant_id, key_version`)
	if err != nil {
		return res, fmt.Errorf("read tenant keys: %w", err)
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.tenant, &r.version, &r.ckw, &r.ksw, &r.pub); err != nil {
			_ = rows.Close()
			return res, fmt.Errorf("scan tenant key: %w", err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return res, fmt.Errorf("read tenant keys: %w", err)
	}
	_ = rows.Close()

	for _, r := range all {
		res.Rows++
		kv, usedPrevious, err := k.unwrapVersion(r.tenant, r.version, r.ckw, r.ksw, r.pub)
		if err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s v%d", r.tenant, r.version))
			continue
		}
		if !usedPrevious {
			continue
		}
		ckw, err := wrapKey(k.master, r.tenant, r.version, purposeChunkKey, kv.ChunkKey[:])
		if err != nil {
			res.RewrapErrored++
			continue
		}
		ksw, err := wrapKey(k.master, r.tenant, r.version, purposeKEMSeed, kv.KEM.Bytes())
		if err != nil {
			res.RewrapErrored++
			continue
		}
		// Guarded on the old bytes: a second process re-wrapping the same
		// row at the same boot writes once.
		if _, err := k.db.ExecContext(ctx, `
			UPDATE tenant_key_versions SET chunk_key_wrapped = $3, kem_seed_wrapped = $4
			 WHERE tenant_id = $1 AND key_version = $2 AND chunk_key_wrapped = $5 AND kem_seed_wrapped = $6`,
			r.tenant, r.version, ckw, ksw, r.ckw, r.ksw); err != nil {
			res.RewrapErrored++
			continue
		}
		res.Rewrapped++
	}
	return res, nil
}

// wrapKey seals a tenant secret under a master key. The AAD binds the blob to
// its tenant, version and purpose, so a wrapped value copied to another row
// — another tenant's, another version's, or the other column — does not
// open (encryptSeed of 037 had no AAD).
//
// Format: [0x01][nonce 12][AES-256-GCM ciphertext + tag].
func wrapKey(master []byte, tenantID string, version int, purpose string, secret []byte) ([]byte, error) {
	gcm, err := masterGCM(master)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("wrap nonce: %w", err)
	}
	out := make([]byte, 0, 1+len(nonce)+len(secret)+gcm.Overhead())
	out = append(out, wrapFormat)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, secret, wrapAAD(tenantID, version, purpose)), nil
}

func unwrapKey(master []byte, tenantID string, version int, purpose string, wrapped []byte) ([]byte, error) {
	gcm, err := masterGCM(master)
	if err != nil {
		return nil, err
	}
	if len(wrapped) < 1+gcm.NonceSize()+gcm.Overhead() || wrapped[0] != wrapFormat {
		return nil, errors.New("wrapped key: bad format")
	}
	nonce := wrapped[1 : 1+gcm.NonceSize()]
	secret, err := gcm.Open(nil, nonce, wrapped[1+gcm.NonceSize():], wrapAAD(tenantID, version, purpose))
	if err != nil {
		return nil, fmt.Errorf("wrapped key: %w", err)
	}
	return secret, nil
}

// wrapAAD = "vaultaire-tenant-key" ‖ 0 ‖ tenant ‖ 0 ‖ version (uint32 BE) ‖ purpose.
func wrapAAD(tenantID string, version int, purpose string) []byte {
	aad := make([]byte, 0, 32+len(tenantID)+len(purpose))
	aad = append(aad, "vaultaire-tenant-key"...)
	aad = append(aad, 0)
	aad = append(aad, tenantID...)
	aad = append(aad, 0)
	aad = binary.BigEndian.AppendUint32(aad, uint32(version))
	return append(aad, purpose...)
}

func masterGCM(master []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(master)
	if err != nil {
		return nil, fmt.Errorf("master key cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("master key GCM: %w", err)
	}
	return gcm, nil
}

// GenerateMasterKey creates a new random master key.
func GenerateMasterKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("failed to generate master key: %w", err)
	}
	return key, nil
}

// GenerateMasterKeyHex creates a new random master key as a hex string.
func GenerateMasterKeyHex() (string, error) {
	key, err := GenerateMasterKey()
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(key), nil
}
