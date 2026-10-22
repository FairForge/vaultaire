package secret

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func files(m map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if v, ok := m[p]; ok {
			if v == "<unreadable>" {
				return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrPermission}
			}
			return []byte(v), nil
		}
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
}

func TestLoad_Precedence(t *testing.T) {
	v, err := Load(env(map[string]string{"CREDENTIALS_DIRECTORY": "/cred", "K_FILE": "/k", "K": "from-env"}),
		files(map[string]string{"/cred/k_cred": " from-credential\n", "/k": "from-file"}), "k_cred", "K")
	require.NoError(t, err)
	assert.Equal(t, "from-credential", string(v.Bytes))
	assert.Equal(t, FromCredential, v.Source)
	assert.Equal(t, []string{"K_FILE", "K"}, v.Ignored)

	v, err = Load(env(map[string]string{"CREDENTIALS_DIRECTORY": "/cred", "K_FILE": "/k"}), files(map[string]string{"/k": "from-file"}), "k_cred", "K")
	require.NoError(t, err)
	assert.Equal(t, FromFile, v.Source, "a unit with other credentials falls through")

	v, err = Load(env(map[string]string{"K": "from-env"}), files(nil), "k_cred", "K")
	require.NoError(t, err)
	assert.Equal(t, FromEnv, v.Source)

	v, err = Load(env(nil), files(nil), "k_cred", "K")
	require.NoError(t, err)
	assert.Nil(t, v.Bytes, "nothing set is none, not an error")
}

func TestLoad_SetButUnusableIsAnError(t *testing.T) {
	for name, tc := range map[string]struct {
		env   map[string]string
		files map[string]string
	}{
		"credential unreadable": {map[string]string{"CREDENTIALS_DIRECTORY": "/cred"}, map[string]string{"/cred/k_cred": "<unreadable>"}},
		"file missing":          {map[string]string{"K_FILE": "/nope"}, nil},
		"empty value":           {map[string]string{"K_FILE": "/k"}, map[string]string{"/k": "  \n"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(tc.env), files(tc.files), "k_cred", "K")
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrSource))
			assert.NotContains(t, err.Error(), "/nope", "never a path")
		})
	}
}
