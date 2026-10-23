package e2ee

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArgon2id_KnownAnswer pins the KEK's bytes for fixed inputs, recomputed
// by OpenSSL and libsodium (testdata/vectors_check.py): a mix-up of RFC
// 9106's parameters (time ↔ memory, lanes, the version) changes them.
func TestArgon2id_KnownAnswer(t *testing.T) {
	t.Parallel() // Argon2id at 64 MiB: slow under -race
	vs := loadVectors(t).Argon2id
	require.Len(t, vs, 2)
	for _, v := range vs {
		got := deriveKEK(v.Password, v.Salt, KDFParams{v.MemoryKiB, v.Iterations, v.Parallelism})
		assert.Equal(t, hex.EncodeToString(v.KEK), hex.EncodeToString(got), "m=%d t=%d p=%d", v.MemoryKiB, v.Iterations, v.Parallelism)
	}
}

// TestPasswordWrap_GoldenBytes pins the 0x04 wire: the header layout, the
// header in the box's AD, the label. Both directions.
func TestPasswordWrap_GoldenBytes(t *testing.T) {
	t.Parallel() // Argon2id at 64 MiB: slow under -race
	v := loadVectors(t).PasswordWrap
	require.NotNil(t, v)
	p := KDFParams{v.MemoryKiB, v.Iterations, v.Parallelism}
	assert.Equal(t, Interactive, p)
	got, err := wrapMasterKeyWithPassword(v.MasterKey, v.Password, p, v.Salt, v.Nonce)
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(v.Wrapped), hex.EncodeToString(got))
	assert.Len(t, got, headerSize+1+24+KeySize+16)

	params, err := PasswordWrapParams(v.Wrapped)
	require.NoError(t, err)
	assert.Equal(t, p, params)
	mk, err := OpenMasterKeyWithPassword(v.Wrapped, v.Password)
	require.NoError(t, err)
	assert.Equal(t, []byte(v.MasterKey), mk)
}

func TestPasswordWrap_WrongPasswordAndTamper(t *testing.T) {
	t.Parallel() // Argon2id at 64 MiB: slow under -race
	mk := key(t, 9)
	w, err := WrapMasterKeyWithPassword(mk, []byte("a long passphrase"), Interactive)
	require.NoError(t, err)
	w2, err := WrapMasterKeyWithPassword(mk, []byte("a long passphrase"), Interactive)
	require.NoError(t, err)
	assert.NotEqual(t, w[11:27], w2[11:27], "a fresh salt per wrap")

	_, err = OpenMasterKeyWithPassword(w, []byte("a long passphrasE"))
	assert.ErrorIs(t, err, ErrOpen)
	_, err = OpenMasterKeyWithPassword(w, nil)
	assert.ErrorIs(t, err, ErrEmptyPassword)
	_, err = WrapMasterKeyWithPassword(mk, nil, Interactive)
	assert.ErrorIs(t, err, ErrEmptyPassword)

	salted := append([]byte{}, w...)
	salted[20] ^= 1
	_, err = OpenMasterKeyWithPassword(salted, []byte("a long passphrase"))
	assert.ErrorIs(t, err, ErrOpen, "another salt is another KEK")
	_, err = OpenMasterKeyWithPassword(append([]byte{FormatBox}, w[1:]...), []byte("a long passphrase"))
	assert.ErrorIs(t, err, ErrFormat)
	_, err = OpenMasterKeyWithPassword(w[:headerSize+10], []byte("a long passphrase"))
	assert.ErrorIs(t, err, ErrFormat)
}

// reheader builds a well-formed password wrap under parameters the public
// Wrap refuses — what a hostile server could hand a device.
func reheader(t *testing.T, mk, password []byte, p KDFParams) []byte {
	t.Helper()
	salt := make([]byte, saltSize)
	hdr := []byte{FormatPasswordWrap, kdfArgon2id}
	hdr = binary.BigEndian.AppendUint32(hdr, p.MemoryKiB)
	hdr = binary.BigEndian.AppendUint32(hdr, p.Iterations)
	hdr = append(append(hdr, p.Parallelism), salt...)
	var kek []byte
	if p.MemoryKiB <= 64<<10 && p.Iterations <= 3 && p.Parallelism >= 1 && p.MemoryKiB >= 8 {
		kek = deriveKEK(password, salt, p) // cheap enough to really make it
	} else {
		kek = key(t, 0x42) // never derived: the open must refuse first
	}
	box, err := seal(FormatBox, kek, make([]byte, 24), mk, append([]byte(labelMasterWrapKEK), hdr...))
	require.NoError(t, err)
	return append(hdr, box...)
}

// TestPasswordWrap_RefusesParametersUnderTheFloor: a wrap whose header asks
// for less than Interactive is refused before any derivation, even though it
// would open; one that asks for more than the bound is refused too (a
// server cannot exhaust a device).
func TestPasswordWrap_RefusesParametersUnderTheFloor(t *testing.T) {
	t.Parallel() // Argon2id at 64 MiB: slow under -race
	mk, pw := key(t, 5), []byte("password")
	weak := []KDFParams{
		{MemoryKiB: 32 << 10, Iterations: 3, Parallelism: 1},
		{MemoryKiB: 64<<10 - 1, Iterations: 3, Parallelism: 1},
		{MemoryKiB: 64 << 10, Iterations: 2, Parallelism: 1},
		{MemoryKiB: 64 << 10, Iterations: 3, Parallelism: 0},
		{MemoryKiB: MaxMemoryKiB + 1, Iterations: 3, Parallelism: 1},
		{MemoryKiB: 64 << 10, Iterations: MaxIterations + 1, Parallelism: 1},
		{MemoryKiB: 64 << 10, Iterations: 3, Parallelism: MaxParallelism + 1},
	}
	for _, p := range weak {
		w := reheader(t, mk, pw, p)
		_, err := OpenMasterKeyWithPassword(w, pw)
		assert.ErrorIs(t, err, ErrWeakParams, "%+v", p)
		_, err = WrapMasterKeyWithPassword(mk, pw, p)
		assert.ErrorIs(t, err, ErrWeakParams, "%+v", p)
	}
	// The floor itself opens.
	w := reheader(t, mk, pw, Interactive)
	got, err := OpenMasterKeyWithPassword(w, pw)
	require.NoError(t, err)
	assert.Equal(t, mk, got)
	assert.Equal(t, uint32(64<<10), Interactive.MemoryKiB, "Interactive is the 64 MiB floor")
	assert.GreaterOrEqual(t, Sensitive.MemoryKiB, Interactive.MemoryKiB)
}

func TestMnemonic_WordlistIsBIP39(t *testing.T) {
	sum := sha256.Sum256([]byte(bip39English))
	assert.Equal(t, "2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda", hex.EncodeToString(sum[:]))
	require.Len(t, bip39Words, 2048)
	assert.True(t, sort.StringsAreSorted(bip39Words))
	assert.Len(t, bip39Index, 2048, "no duplicates")
}

// TestRecoveryMnemonic_BIP39Vectors: BIP-39's published 256-bit vectors
// (entropy → words → checksum), both directions.
func TestRecoveryMnemonic_BIP39Vectors(t *testing.T) {
	vs := loadVectors(t).BIP39
	require.Len(t, vs, 8)
	for _, v := range vs {
		words, err := RecoveryMnemonic(v.Entropy)
		require.NoError(t, err)
		assert.Equal(t, v.Mnemonic, words)
		k, err := ParseRecoveryMnemonic(v.Mnemonic)
		require.NoError(t, err)
		assert.Equal(t, []byte(v.Entropy), k)
	}
}

func TestRecoveryMnemonic_Refusals(t *testing.T) {
	v := loadVectors(t).BIP39[4] // 68a79eac…: 24 distinct-looking words
	words := strings.Fields(v.Mnemonic)

	// Case and spacing are not the key.
	k, err := ParseRecoveryMnemonic("  " + strings.ToUpper(strings.Join(words, "\n  ")) + " ")
	require.NoError(t, err)
	assert.Equal(t, []byte(v.Entropy), k)

	with := func(f func(w []string)) string {
		w := append([]string{}, words...)
		f(w)
		return strings.Join(w, " ")
	}
	_, err = ParseRecoveryMnemonic(with(func(w []string) { w[5] = "stored" }))
	assert.ErrorIs(t, err, ErrMnemonicWord, "a word outside the list")
	_, err = ParseRecoveryMnemonic(strings.Join(words[:23], " "))
	assert.ErrorIs(t, err, ErrMnemonicWord, "23 words")
	_, err = ParseRecoveryMnemonic(with(func(w []string) { w[23] = bip39Words[(bip39Index[w[23]]+1)%2048] }))
	assert.ErrorIs(t, err, ErrMnemonicChecksum, "a wrong checksum word")
	_, err = ParseRecoveryMnemonic(with(func(w []string) { w[2], w[3] = w[3], w[2] }))
	assert.ErrorIs(t, err, ErrMnemonicChecksum, "two words swapped")
	_, err = ParseRecoveryMnemonic(with(func(w []string) { w[10] = bip39Words[(bip39Index[w[10]]+7)%2048] }))
	assert.ErrorIs(t, err, ErrMnemonicChecksum, "a mistyped word that is in the list")
	_, err = RecoveryMnemonic(make([]byte, 16))
	assert.ErrorIs(t, err, ErrKeySize, "only 256-bit keys: always 24 words")
}

// TestKeyWraps_GoldenOpen: each role's wrap opens through its public
// function and only under its own label.
func TestKeyWraps_GoldenOpen(t *testing.T) {
	kw := loadVectors(t).KeyWraps
	openers := map[string]func(k, w []byte) ([]byte, error){
		"master_under_recovery": OpenMasterKeyWithRecovery,
		"recovery_under_master": OpenRecoveryKey,
		"recovery_under_vault":  OpenRecoveryKeyFromVault,
	}
	require.Len(t, kw, len(openers))
	for name, opener := range openers {
		v := kw[name]
		require.NotNil(t, v, name)
		got, err := opener(v.WrappingKey, v.Wrapped)
		require.NoError(t, err, name)
		assert.Equal(t, []byte(v.Key), got, name)
		for other, o := range openers {
			if other != name {
				_, err := o(v.WrappingKey, v.Wrapped)
				assert.ErrorIs(t, err, ErrOpen, "%s does not open as %s", name, other)
			}
		}
	}
}

// TestAccount_PasswordChangeAndRecoveryKeepEverySpace: the master key, and
// so every key under it, survives a password change, a recovery by the 24
// words and a recovery through the vault key; only the one wrap changes.
func TestAccount_PasswordChangeAndRecoveryKeepEverySpace(t *testing.T) {
	t.Parallel() // Argon2id at 64 MiB: slow under -race
	mk, err := RandomKey()
	require.NoError(t, err)
	rk, err := NewRecoveryKey()
	require.NoError(t, err)
	vk, err := NewVaultKey()
	require.NoError(t, err)
	assert.LessOrEqual(t, VaultSecretSize, 128, "fits the recovery vault's secret bound (K5-PROBE: ≤ 128 bytes)")
	assert.Len(t, vk, VaultSecretSize)

	spaces := map[string][]byte{}
	wrappedSpaces := map[string][]byte{}
	for _, id := range []string{"photos", "documents", "backups"} {
		sk, err := RandomKey()
		require.NoError(t, err)
		spaces[id] = sk
		ad := "stored.cloud/e2ee/v1/space-key " + id
		wrappedSpaces[id], err = Seal(mk, sk, []byte(ad))
		require.NoError(t, err)
	}
	everySpaceOpens := func(master []byte) {
		t.Helper()
		for id, w := range wrappedSpaces {
			got, err := Open(master, w, []byte("stored.cloud/e2ee/v1/space-key "+id))
			require.NoError(t, err, id)
			assert.Equal(t, spaces[id], got, id)
		}
	}

	byPassword, err := WrapMasterKeyWithPassword(mk, []byte("first password"), Interactive)
	require.NoError(t, err)
	byRecovery, err := WrapMasterKeyWithRecovery(rk, mk)
	require.NoError(t, err)
	recoveryShown, err := WrapRecoveryKey(mk, rk)
	require.NoError(t, err)
	byVault, err := WrapRecoveryKeyForVault(vk, rk)
	require.NoError(t, err)
	words, err := RecoveryMnemonic(rk)
	require.NoError(t, err)

	// Password change: open with the old one, wrap under the new one.
	got, err := OpenMasterKeyWithPassword(byPassword, []byte("first password"))
	require.NoError(t, err)
	byPassword, err = WrapMasterKeyWithPassword(got, []byte("second password"), Interactive)
	require.NoError(t, err)
	_, err = OpenMasterKeyWithPassword(byPassword, []byte("first password"))
	assert.ErrorIs(t, err, ErrOpen, "the old password no longer opens it")
	got, err = OpenMasterKeyWithPassword(byPassword, []byte("second password"))
	require.NoError(t, err)
	assert.Equal(t, mk, got)
	everySpaceOpens(got)

	// Signed in: the words can be shown again.
	shown, err := OpenRecoveryKey(got, recoveryShown)
	require.NoError(t, err)
	assert.Equal(t, rk, shown)

	// Recovery by the 24 words, then a new password.
	fromWords, err := ParseRecoveryMnemonic(words)
	require.NoError(t, err)
	got, err = OpenMasterKeyWithRecovery(fromWords, byRecovery)
	require.NoError(t, err)
	assert.Equal(t, mk, got)
	byPassword, err = WrapMasterKeyWithPassword(got, []byte("third password"), Interactive)
	require.NoError(t, err)
	got, err = OpenMasterKeyWithPassword(byPassword, []byte("third password"))
	require.NoError(t, err)
	everySpaceOpens(got)

	// Recovery through the vault: the realms release the vault key.
	fromVault, err := OpenRecoveryKeyFromVault(vk, byVault)
	require.NoError(t, err)
	got, err = OpenMasterKeyWithRecovery(fromVault, byRecovery)
	require.NoError(t, err)
	everySpaceOpens(got)
	_, err = OpenRecoveryKeyFromVault(rk, byVault)
	assert.ErrorIs(t, err, ErrOpen)
}
