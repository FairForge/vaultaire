package translog

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"

	"filippo.io/mldsa"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	fnote "github.com/transparency-dev/formats/note"
	"golang.org/x/mod/sumdb/note"
)

// K4c-2 (Stage 1a Prompt 34b): cosignatures in the c2sp.org/tlog-cosignature
// form. The expectations here are built from the spec's text, by the test,
// not by the code under test.

// goldenSkey is a fixed ML-DSA-44 cosigner key (test only; never used anywhere).
const goldenSkey = "PRIVATE+KEY+witness.example/golden+89692182+Bub8YUHYoyqbMPSXbBSWJuqg6UYiCEe/KprTv9i6zEjr"

// The spec's example checkpoint and time (c2sp.org/tlog-cosignature § Ed25519).
const (
	specOrigin = "example.com/behind-the-sofa"
	specSize   = 20852163
	specRoot   = "CsUYapGGPo4dkMgIAUqom/Xajj7h2fB2MPA3j2jxq2I="
	specTime   = 1679315147
)

func specCheckpoint() string { return fmt.Sprintf("%s\n%d\n%s\n", specOrigin, specSize, specRoot) }

// sigValue decodes a signature line: key ID ‖ timestamp ‖ signature.
func sigValue(t *testing.T, line, name string) (keyHash uint32, ts uint64, sig []byte) {
	t.Helper()
	require.True(t, strings.HasPrefix(line, "— "+name+" "), "a note signature line: %q", line)
	require.True(t, strings.HasSuffix(line, "\n"))
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(line, "— "+name+" "), "\n"))
	require.NoError(t, err)
	require.Greater(t, len(raw), 12)
	return binary.BigEndian.Uint32(raw), binary.BigEndian.Uint64(raw[4:]), raw[12:]
}

func TestCosignature_V1_Golden(t *testing.T) {
	t.Run("ML-DSA-44, the witness's own key type", func(t *testing.T) {
		// Arrange: a fixed key and a fixed time.
		name := "witness.example/golden"
		s, err := NewCosigner(goldenSkey, func() time.Time { return time.Unix(specTime, 0) })
		require.NoError(t, err)
		seed, err := base64.StdEncoding.DecodeString(strings.SplitN(goldenSkey, "+", 5)[4])
		require.NoError(t, err)
		require.Equal(t, byte(0x06), seed[0], "c2sp.org/signed-note: 0x06 is ML-DSA-44 cosignature/v1")
		sk, err := mldsa.NewPrivateKey(mldsa.MLDSA44(), seed[1:])
		require.NoError(t, err)
		pub := sk.PublicKey().Bytes()

		// Act
		msg, err := note.Sign(&note.Note{Text: specCheckpoint()}, s)
		require.NoError(t, err)

		// Assert: one signature line after the blank line.
		text, sigs, ok := strings.Cut(string(msg), "\n\n")
		require.True(t, ok)
		assert.Equal(t, specCheckpoint(), text+"\n")
		kh, ts, sig := sigValue(t, sigs, name)
		// The key ID: the first 4 bytes of SHA-256(name ‖ "\n" ‖ 0x06 ‖ public key).
		h := sha256.Sum256(append([]byte(name+"\n\x06"), pub...))
		assert.Equal(t, binary.BigEndian.Uint32(h[:]), kh)
		assert.Equal(t, uint32(0x89692182), kh)
		assert.Equal(t, uint64(specTime), ts, "the timestamp: 8 bytes, big-endian, POSIX seconds")
		assert.Len(t, sig, mldsa.MLDSA44SignatureSize)
		// The signed message: CosignedSubtree{"subtree/v1\n\0", name, timestamp, origin, 0, size, hash}.
		root, err := base64.StdEncoding.DecodeString(specRoot)
		require.NoError(t, err)
		var m []byte
		m = append(m, "subtree/v1\n\x00"...)
		m = append(m, byte(len(name)))
		m = append(m, name...)
		m = binary.BigEndian.AppendUint64(m, specTime)
		m = append(m, byte(len(specOrigin)))
		m = append(m, specOrigin...)
		m = binary.BigEndian.AppendUint64(m, 0)
		m = binary.BigEndian.AppendUint64(m, specSize)
		m = append(m, root...)
		pk, err := mldsa.NewPublicKey(mldsa.MLDSA44(), pub)
		require.NoError(t, err)
		require.NoError(t, mldsa.Verify(pk, m, sig, nil), "the signature is over the spec's CosignedSubtree")
		// A reader with the published verifier key verifies it and reads the time back.
		vkey := fmt.Sprintf("%s+%08x+%s", name, kh, base64.StdEncoding.EncodeToString(append([]byte{0x06}, pub...)))
		v, err := NewCosignatureVerifier(vkey)
		require.NoError(t, err)
		got, err := VerifyAnswer([]byte(specCheckpoint()+"\n"+"— "+specOrigin+" AAAAAAAA\n"), []byte(sigs), v)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, time.Unix(specTime, 0).UTC(), got[0].Time.UTC())
		assert.Equal(t, sigs, got[0].Line)
	})

	t.Run("Ed25519, a third party's key type: the spec's message, byte for byte", func(t *testing.T) {
		// Arrange: a fixed Ed25519 key; the line built from the spec's text.
		priv := ed25519.NewKeyFromSeed(make([]byte, 32))
		pub := priv.Public().(ed25519.PublicKey)
		name := "witness.example/ed25519"
		h := sha256.Sum256(append([]byte(name+"\n\x04"), pub...))
		kh := binary.BigEndian.Uint32(h[:])
		signed := "cosignature/v1\ntime 1679315147\n" + specCheckpoint()
		value := binary.BigEndian.AppendUint32(nil, kh)
		value = binary.BigEndian.AppendUint64(value, specTime)
		value = append(value, ed25519.Sign(priv, []byte(signed))...)
		line := "— " + name + " " + base64.StdEncoding.EncodeToString(value) + "\n"
		// Ed25519 is deterministic: this is the line, for ever.
		assert.Equal(t, "— witness.example/ed25519 Y9ztVQAAAABkGFDLN7VCWowLNFrAx5kFlOq5Gblf9cDN74bbmX0TAuxTFK/0CM4NgMiBvbpFFyRcIVXoHLQV9IBeLzGW/URa5SA/DQ==\n", line)
		vkey := fmt.Sprintf("%s+%08x+%s", name, kh, base64.StdEncoding.EncodeToString(append([]byte{0x04}, pub...)))

		// Act
		v, err := NewCosignatureVerifier(vkey)
		require.NoError(t, err)
		got, err := VerifyAnswer([]byte(specCheckpoint()+"\n"+"— "+specOrigin+" AAAAAAAA\n"), []byte(line), v)

		// Assert
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, int64(specTime), got[0].Time.Unix())
		// The same signature under another time does not verify.
		tampered := binary.BigEndian.AppendUint32(nil, kh)
		tampered = binary.BigEndian.AppendUint64(tampered, specTime+1)
		tampered = append(tampered, value[12:]...)
		_, err = VerifyAnswer([]byte(specCheckpoint()+"\n"+"— "+specOrigin+" AAAAAAAA\n"),
			[]byte("— "+name+" "+base64.StdEncoding.EncodeToString(tampered)+"\n"), v)
		assert.Error(t, err, "the timestamp is signed")
	})
}

func TestNewCosignatureVerifier_RefusesWhatIsNotACosignatureKey(t *testing.T) {
	// A plain note key (0x01) carries no timestamp: refused.
	_, plain, err := note.GenerateKey(nil, "witness.example/plain")
	require.NoError(t, err)
	_, err = NewCosignatureVerifier(plain)
	assert.ErrorContains(t, err, "not a cosignature/v1 key")
	// A key under the log's origin: refused.
	_, v, err := fnote.GenerateMLDSAKey(Origin)
	require.NoError(t, err)
	_, err = NewCosignatureVerifier(v)
	assert.ErrorContains(t, err, "log's origin")
	// A key ID that is not its key's.
	_, v, err = fnote.GenerateMLDSAKey("witness.example/a")
	require.NoError(t, err)
	parts := strings.SplitN(v, "+", 3)
	_, err = NewCosignatureVerifier(parts[0] + "+00000000+" + parts[2])
	assert.ErrorContains(t, err, "does not match")
	// The signer side: a plain key, or the log's own, is no cosigner.
	sk, _, err := GenerateKey(nil)
	require.NoError(t, err)
	_, err = NewCosigner(sk, nil)
	assert.ErrorContains(t, err, "ML-DSA-44")
	_, _, err = GenerateCosignerKey(Origin)
	assert.Error(t, err)
}

func TestWitnessPolicy_ForgedOtherAndStaleLines(t *testing.T) {
	// Arrange: a checkpoint of a log, cosigned by two witnesses.
	skey, vkey, err := GenerateKey(nil)
	require.NoError(t, err)
	ls, err := NewSigner(skey)
	require.NoError(t, err)
	lv, err := NewVerifier(vkey)
	require.NoError(t, err)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	w := func(name string, at time.Time) (note.Signer, note.Verifier) {
		sk, vk, err := GenerateCosignerKey(name)
		require.NoError(t, err)
		s, err := NewCosigner(sk, func() time.Time { return at })
		require.NoError(t, err)
		v, err := NewCosignatureVerifier(vk)
		require.NoError(t, err)
		return s, v
	}
	sa, va := w("witness.example/a", now.Add(-time.Minute))
	sb, vb := w("witness.example/b", now.Add(-48*time.Hour))
	cp6, err := SignCheckpoint(ls, 6, Hash{6})
	require.NoError(t, err)
	n6, err := note.Open(cp6, note.VerifierList(lv))
	require.NoError(t, err)
	both, err := note.Sign(n6, sa, sb)
	require.NoError(t, err)
	p := WitnessPolicy{Witnesses: []note.Verifier{va, vb}, Min: 2, MaxAge: 24 * time.Hour, Now: func() time.Time { return now }}

	// b's cosignature is two days old: stale, not counted.
	_, fresh, skipped, err := p.Verify(both, lv)
	assert.ErrorIs(t, err, ErrTooFewCosignatures)
	assert.Len(t, fresh, 1)
	require.Len(t, skipped, 1)
	assert.Contains(t, skipped[0], "stale")
	p.Min = 1
	_, fresh, _, err = p.Verify(both, lv)
	require.NoError(t, err)
	assert.Equal(t, "witness.example/a", fresh[0].Name)

	// a's line copied from the checkpoint of 5 onto the checkpoint of 6: refused outright.
	cp5, err := SignCheckpoint(ls, 5, Hash{5})
	require.NoError(t, err)
	n5, err := note.Open(cp5, note.VerifierList(lv))
	require.NoError(t, err)
	on5, err := note.Sign(n5, sa)
	require.NoError(t, err)
	lineOf5 := CosignatureLines([]byte(on5), sa.Name(), sa.KeyHash())
	require.Len(t, lineOf5, 1)
	_, _, _, err = p.Verify(append(append([]byte(nil), cp6...), lineOf5[0]...), lv)
	assert.ErrorContains(t, err, "does not verify over this checkpoint")

	// A forged line: a's name and key ID, someone else's bytes.
	good := CosignatureLines(both, sa.Name(), sa.KeyHash())[0]
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(good, "— "+sa.Name()+" "), "\n"))
	require.NoError(t, err)
	raw[len(raw)-1] ^= 1
	forged := "— " + sa.Name() + " " + base64.StdEncoding.EncodeToString(raw) + "\n"
	_, _, _, err = p.Verify(append(append([]byte(nil), cp6...), forged...), lv)
	assert.ErrorContains(t, err, "does not verify")

	// A cosignature dated in the future does not count.
	sf, vf := w("witness.example/f", now.Add(time.Hour))
	fut, err := note.Sign(n6, sf)
	require.NoError(t, err)
	_, _, skipped, err = WitnessPolicy{Witnesses: []note.Verifier{vf}, Min: 1, Now: func() time.Time { return now }}.Verify(fut, lv)
	assert.ErrorIs(t, err, ErrTooFewCosignatures)
	assert.Contains(t, strings.Join(skipped, " "), "future")
}
