package release

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"golang.org/x/mod/sumdb/note"
)

func keys(t *testing.T) (note.Signer, note.Verifier) {
	t.Helper()
	skey, vkey, err := GenerateKey(rand.Reader)
	require.NoError(t, err)
	s, err := NewSigner(skey)
	require.NoError(t, err)
	v, err := NewVerifier(vkey)
	require.NoError(t, err)
	return s, v
}

func artefact(t *testing.T, name string, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(p, body, 0o600))
	return p
}

func TestSignVerify_RoundTripAndWhatIsRefused(t *testing.T) {
	s, v := keys(t)
	body := []byte(strings.Repeat("vaultaire release bytes ", 1000))
	p := artefact(t, "vaultaire-linux", body)
	sig, err := Sign(s, p)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(sig), Name+"\nname vaultaire-linux\nsha256 "), "the statement is readable: %s", sig)
	assert.Contains(t, string(sig), "\n\n— "+Name+" ", "a signed note by the release key")

	st, err := Verify(v, p, sig)
	require.NoError(t, err)
	assert.Equal(t, Statement{Name: "vaultaire-linux", SHA256: st.SHA256, Size: int64(len(body))}, st)
	assert.Len(t, st.SHA256, 64)

	t.Run("renamed download still verifies (the bytes are checked)", func(t *testing.T) {
		renamed := artefact(t, "vaultaire-linux (1)", body)
		st, err := Verify(v, renamed, sig)
		require.NoError(t, err)
		assert.Equal(t, "vaultaire-linux", st.Name, "the statement names what was signed")
	})
	t.Run("one bit changed", func(t *testing.T) {
		b := append([]byte(nil), body...)
		b[len(b)/2] ^= 0x01
		_, err := Verify(v, artefact(t, "vaultaire-linux", b), sig)
		require.ErrorIs(t, err, ErrMismatch)
	})
	t.Run("one byte appended", func(t *testing.T) {
		_, err := Verify(v, artefact(t, "vaultaire-linux", append(append([]byte(nil), body...), 0)), sig)
		require.ErrorIs(t, err, ErrMismatch)
	})
	t.Run("the wrong key", func(t *testing.T) {
		_, other := keys(t)
		_, err := Verify(other, p, sig)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "signature")
	})
	t.Run("a truncated signature", func(t *testing.T) {
		for _, cut := range []int{len(sig) - 1, len(sig) - 10, len(sig) / 2, len(Name) + 1} {
			_, err := Verify(v, p, sig[:cut])
			assert.Error(t, err, "cut at %d", cut)
		}
	})
	t.Run("a flipped signature byte", func(t *testing.T) {
		// The signature line is "— <name> <base64>": a byte in the middle of
		// the base64 (not its padding, whose spare bits decode the same).
		b := append([]byte(nil), sig...)
		line := bytes.LastIndex(b, []byte("— "+Name+" ")) + len("— "+Name+" ")
		b[line+20] ^= 0x01
		_, err := Verify(v, p, b)
		require.Error(t, err)
	})
	t.Run("a statement of another format under a valid signature", func(t *testing.T) {
		for _, text := range []string{
			"stored.cloud/release/v2\nname x\nsha256 " + st.SHA256 + "\nsize 1\n",
			Name + "\nname x\nsha256 " + st.SHA256 + "\n",
			Name + "\nname x\nsha256 " + st.SHA256 + "\nsize 1\nextra line\n",
			Name + "\nname ../x\nsha256 " + st.SHA256 + "\nsize 1\n",
			Name + "\nname x\nsha256 " + strings.ToUpper(st.SHA256) + "\nsize 1\n",
			Name + "\nname x\nsha256 " + st.SHA256 + "\nsize -1\n",
			Name + "\nname x\nsha256 " + st.SHA256 + "\nsize 01\n",
		} {
			forged, err := note.Sign(&note.Note{Text: text}, s)
			require.NoError(t, err)
			_, err = Open(v, forged)
			assert.ErrorIs(t, err, ErrFormat, "%q", text)
		}
	})
	t.Run("not a signature at all", func(t *testing.T) {
		_, err := Verify(v, p, []byte("-----BEGIN PGP SIGNATURE-----\n"))
		require.ErrorIs(t, err, ErrFormat)
	})
}

func TestKeys_NamedForTheReleaseOnly(t *testing.T) {
	skey, vkey, err := note.GenerateKey(rand.Reader, "stored.cloud/translog/v1")
	require.NoError(t, err)
	_, err = NewSigner(skey)
	require.Error(t, err, "the transparency log's key is not the release key")
	assert.NotContains(t, err.Error(), skey[len(skey)-20:], "the error quotes nothing of the key")
	_, err = NewVerifier(vkey)
	require.Error(t, err)
	_, err = NewSigner("not a key")
	require.Error(t, err)
}

func TestLoadSigningKey_Precedence(t *testing.T) {
	skey, _, err := GenerateKey(rand.Reader)
	require.NoError(t, err)
	other, _, err := GenerateKey(rand.Reader)
	require.NoError(t, err)
	files := map[string][]byte{}
	readFile := func(p string) ([]byte, error) {
		if b, ok := files[p]; ok {
			return b, nil
		}
		return nil, os.ErrNotExist
	}
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	_, err = LoadSigningKey(env(nil), readFile)
	assert.ErrorIs(t, err, ErrNoSigningKey, "no source = dark, a clear message")

	files["/creds/release_signing_key"] = []byte(skey + "\n")
	files["/k/file"] = []byte(other)
	sk, err := LoadSigningKey(env(map[string]string{"CREDENTIALS_DIRECTORY": "/creds", "RELEASE_SIGNING_KEY_FILE": "/k/file", "RELEASE_SIGNING_KEY": other}), readFile)
	require.NoError(t, err)
	assert.Equal(t, SigningKeyFromCredential, sk.Source)
	assert.Equal(t, []string{"RELEASE_SIGNING_KEY_FILE", "RELEASE_SIGNING_KEY"}, sk.Ignored)

	sk, err = LoadSigningKey(env(map[string]string{"RELEASE_SIGNING_KEY_FILE": "/k/file"}), readFile)
	require.NoError(t, err)
	assert.Equal(t, SigningKeyFromFile, sk.Source)

	_, err = LoadSigningKey(env(map[string]string{"RELEASE_SIGNING_KEY_FILE": "/k/missing"}), readFile)
	assert.ErrorIs(t, err, ErrSigningKeySource, "a file source set but unreadable is a refusal, never dark")
	_, err = LoadSigningKey(env(map[string]string{"RELEASE_SIGNING_KEY": "garbage"}), readFile)
	assert.ErrorIs(t, err, ErrSigningKeySource)
	assert.NotContains(t, err.Error(), "garbage", "never a character of the value")
	files["/creds/release_signing_key"] = []byte("stored.cloud/translog/v1+00000000+AAAA")
	_, err = LoadSigningKey(env(map[string]string{"CREDENTIALS_DIRECTORY": "/creds"}), readFile)
	assert.ErrorIs(t, err, ErrSigningKeySource, "a credential named for another purpose is refused")
	var pe *os.PathError
	assert.False(t, errors.As(err, &pe))
}
