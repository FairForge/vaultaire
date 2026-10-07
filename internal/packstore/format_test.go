package packstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildPack assembles a pack in memory the way the Writer lays it out on disk.
func buildPack(t *testing.T, members map[string][]byte, order []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString(headerMagic)
	var idx []FooterMember
	for _, k := range order {
		b := members[k]
		sum := sha256.Sum256(b)
		idx = append(idx, FooterMember{Tenant: "t1", Key: k, Offset: int64(buf.Len()), Length: int64(len(b)), SHA256: hex.EncodeToString(sum[:])})
		buf.Write(b)
	}
	tail, err := encodeTail(idx)
	require.NoError(t, err)
	buf.Write(tail)
	return buf.Bytes()
}

func TestFormat_FooterRoundTrip(t *testing.T) {
	// Arrange
	members := map[string][]byte{"a": []byte("alpha"), "b": {}, "c": bytes.Repeat([]byte{7}, 1000)}
	pack := buildPack(t, members, []string{"a", "b", "c"})

	// Act
	f, err := ReadIndex(bytes.NewReader(pack), int64(len(pack)))

	// Assert
	require.NoError(t, err)
	require.Len(t, f.Members, 3)
	assert.Equal(t, FormatVersion, f.Version)
	for _, m := range f.Members {
		assert.Equal(t, members[m.Key], pack[m.Offset:m.Offset+m.Length], m.Key)
	}
}

func TestFormat_DamagedFooterIsRefused(t *testing.T) {
	pack := buildPack(t, map[string][]byte{"a": []byte("alpha")}, []string{"a"})

	// a byte of the footer flipped: its sha256 in the trailer no longer matches
	bad := append([]byte(nil), pack...)
	bad[len(bad)-int(TrailerSize)-3] ^= 0xFF
	_, err := ReadIndex(bytes.NewReader(bad), int64(len(bad)))
	assert.ErrorIs(t, err, ErrBadPack)

	// a truncated pack has no trailer at its end
	_, err = ReadIndex(bytes.NewReader(pack[:len(pack)-1]), int64(len(pack)-1))
	assert.ErrorIs(t, err, ErrBadPack)

	// not a pack at all
	junk := []byte(strings.Repeat("x", 200))
	_, err = ReadIndex(bytes.NewReader(junk), int64(len(junk)))
	assert.ErrorIs(t, err, ErrBadPack)
}

func TestFormat_MembersOutsideTheBodyAreRefused(t *testing.T) {
	tail, err := encodeTail([]FooterMember{{Tenant: "t", Key: "k", Offset: 0, Length: 4, SHA256: strings.Repeat("0", 64)}})
	require.NoError(t, err)
	pack := append([]byte(headerMagic+"abcd"), tail...)
	_, err = ReadIndex(bytes.NewReader(pack), int64(len(pack)))
	assert.ErrorIs(t, err, ErrBadPack, "offset 0 overlaps the header")
}

// The layout: a two-level, content-addressed name. 256 first-level folders
// under the container; every pack path stays far under Sync's 248-character
// limit (we hold ourselves to 200 including the driver's tenant prefix).
func TestLayout_NamesAndPathLengths(t *testing.T) {
	sum := sha256.Sum256([]byte("x"))
	hexsum := hex.EncodeToString(sum[:])

	name := packName(hexsum)
	assert.Equal(t, hexsum[:2]+"/"+hexsum+".pack", name)
	assert.Equal(t, hexsum[:2], packFolder(hexsum))
	assert.True(t, validPackName(name))
	other := "zz/" + hexsum + ".pack"
	assert.False(t, validPackName(other), "folder must be the hash's own prefix")

	full := "t-" + addressTenant + "/" + DefaultContainer + "/" + name
	assert.LessOrEqual(t, len(full), 200, full)
	// With a generous WebDAV root on top, still under 248.
	assert.Less(t, len("/vaultaire-sync-root-folder/"+full), 248)

	// folders: the first level has at most 256 entries.
	folders := map[string]bool{}
	for i := 0; i < 20000; i++ {
		s := sha256.Sum256([]byte{byte(i), byte(i >> 8)})
		folders[packFolder(hex.EncodeToString(s[:]))] = true
	}
	assert.LessOrEqual(t, len(folders), 256)
}

func TestEncodeTail_RefusesAFooterTheReaderWouldRefuse(t *testing.T) {
	// One member whose key alone exceeds the footer limit.
	big := strings.Repeat("k", int(maxFooterBytes)+1)
	_, err := encodeTail([]FooterMember{{Tenant: "t", Key: big, Offset: 0, Length: 1, SHA256: "x"}})
	require.ErrorIs(t, err, ErrBadPack)
}
