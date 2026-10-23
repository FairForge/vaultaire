package translog

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The c2sp.org/tlog-witness add-checkpoint body, both directions.
func TestAddCheckpointBody_RoundTripAndStrictParse(t *testing.T) {
	// Arrange
	cp := []byte("stored.cloud/translog/v1\n7\nAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n\n— stored.cloud/translog/v1 AAAAAAAA\n")
	proof := []Hash{{1}, {2}, {3}}

	// Act
	body := AddCheckpointBody(4, proof, cp)
	old, got, rest, err := ParseAddCheckpoint(body)

	// Assert: the spec's layout, and it reads back.
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(body), "old 4\n"+base64.StdEncoding.EncodeToString(proof[0][:])+"\n"))
	assert.Equal(t, int64(4), old)
	assert.Equal(t, proof, got)
	assert.Equal(t, cp, rest)
	_, got, _, err = ParseAddCheckpoint(AddCheckpointBody(0, nil, cp))
	require.NoError(t, err)
	assert.Empty(t, got)

	h := base64.StdEncoding.EncodeToString(make([]byte, 32))
	for name, bad := range map[string]string{
		"leading zero":         "old 04\n\n" + string(cp),
		"no old line":          "size 4\n\n" + string(cp),
		"no empty line":        "old 0\n" + string(cp),
		"no checkpoint":        "old 0\n\n",
		"a short hash":         "old 1\nAAAA\n\n" + string(cp),
		"non-canonical base64": "old 1\n" + h[:42] + "B=\n\n" + string(cp),
		"64 proof lines":       "old 1\n" + strings.Repeat(h+"\n", 64) + "\n" + string(cp),
		"a negative size":      "old -1\n\n" + string(cp),
	} {
		_, _, _, err := ParseAddCheckpoint([]byte(bad))
		assert.ErrorIs(t, err, ErrBadAddCheckpoint, name)
	}
	_, _, _, err = ParseAddCheckpoint([]byte("old 1\n" + strings.Repeat(h+"\n", 63) + "\n" + string(cp)))
	assert.NoError(t, err, "63 proof lines is the most a client sends")
}
