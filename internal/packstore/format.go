package packstore

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// The pack format (version 1). A pack is self-describing: the footer names
// every member, so the index can be rebuilt from the file alone (Recover).
//
//	offset 0        header   8 bytes  "VLTPACK1"
//	offset 8        members  the member bytes back to back, no padding
//	body end        footer   JSON {"version":1,"members":[{tenant,key,offset,length,sha256}…]}
//	size-48         trailer  footer length (uint64 big-endian, 8 bytes)
//	                         sha256 of the footer (32 bytes)
//	                         "VLTPEND1" (8 bytes)
//
// The pack's name is the sha256 of the whole file. A reader finds the footer
// from the trailer at the end; the trailer at exactly `size-48` is also how an
// upload is verified (a short or long file has no trailer there).
const (
	headerMagic  = "VLTPACK1"
	trailerMagic = "VLTPEND1"
	// HeaderSize is the length of the pack header.
	HeaderSize = int64(len(headerMagic))
	// TrailerSize is the length of the fixed trailer at the end of a pack.
	TrailerSize = 8 + sha256.Size + int64(len(trailerMagic))
	// FormatVersion is the footer version this package writes.
	FormatVersion = 1
	// maxFooterBytes bounds a footer a reader will load (64 MiB).
	maxFooterBytes = 64 << 20
)

// ErrBadPack is returned for bytes that are not a well-formed pack.
var ErrBadPack = errors.New("packstore: not a well-formed pack")

// FooterMember is one member as the footer records it.
type FooterMember struct {
	Tenant string `json:"tenant"`
	Key    string `json:"key"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	SHA256 string `json:"sha256"`
}

// Footer is a pack's member index.
type Footer struct {
	Version int            `json:"version"`
	Members []FooterMember `json:"members"`
}

// encodeTail renders the footer and the trailer that follow the members.
func encodeTail(members []FooterMember) ([]byte, error) {
	if members == nil {
		members = []FooterMember{}
	}
	footer, err := json.Marshal(Footer{Version: FormatVersion, Members: members})
	if err != nil {
		return nil, fmt.Errorf("encode pack footer: %w", err)
	}
	// The reader refuses a footer above maxFooterBytes (parseTrailer); never
	// write one it would refuse.
	if uint64(len(footer)) > maxFooterBytes {
		return nil, fmt.Errorf("%w: footer of %d bytes exceeds %d", ErrBadPack, len(footer), uint64(maxFooterBytes))
	}
	sum := sha256.Sum256(footer)
	// The trailer goes on the end of the footer's own fresh slice.
	out := binary.BigEndian.AppendUint64(footer, uint64(len(footer)))
	out = append(out, sum[:]...)
	out = append(out, trailerMagic...)
	return out, nil
}

// parseTrailer reads the footer length and checksum from a trailer.
func parseTrailer(b []byte) (int64, [sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	if int64(len(b)) != TrailerSize || string(b[TrailerSize-int64(len(trailerMagic)):]) != trailerMagic {
		return 0, sum, fmt.Errorf("%w: no trailer", ErrBadPack)
	}
	n := binary.BigEndian.Uint64(b[:8])
	if n > maxFooterBytes {
		return 0, sum, fmt.Errorf("%w: footer of %d bytes", ErrBadPack, n)
	}
	copy(sum[:], b[8:8+sha256.Size])
	return int64(n), sum, nil
}

// parseFooter checks a footer against its checksum and the pack geometry.
func parseFooter(b []byte, sum [sha256.Size]byte, bodyEnd int64) (*Footer, error) {
	if sha256.Sum256(b) != sum {
		return nil, fmt.Errorf("%w: footer checksum mismatch", ErrBadPack)
	}
	var f Footer
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%w: footer: %w", ErrBadPack, err)
	}
	if f.Version != FormatVersion {
		return nil, fmt.Errorf("%w: footer version %d", ErrBadPack, f.Version)
	}
	ms := append([]FooterMember(nil), f.Members...)
	sort.Slice(ms, func(i, j int) bool { return ms[i].Offset < ms[j].Offset })
	next := HeaderSize
	for _, m := range ms {
		if m.Offset < next || m.Length < 0 || m.Offset+m.Length > bodyEnd {
			return nil, fmt.Errorf("%w: member %q at %d+%d is outside the body", ErrBadPack, m.Key, m.Offset, m.Length)
		}
		if _, err := hex.DecodeString(m.SHA256); err != nil || len(m.SHA256) != 2*sha256.Size {
			return nil, fmt.Errorf("%w: member %q has no sha256", ErrBadPack, m.Key)
		}
		next = m.Offset + m.Length
	}
	return &f, nil
}

// ReadIndex reads the footer of a pack held in r (size bytes): a local file,
// or a downloaded pack. The header, the trailer and the footer checksum are
// verified; member bytes are not read.
func ReadIndex(r io.ReaderAt, size int64) (*Footer, error) {
	if size < HeaderSize+TrailerSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrBadPack, size)
	}
	head := make([]byte, HeaderSize)
	if _, err := r.ReadAt(head, 0); err != nil {
		return nil, fmt.Errorf("read pack header: %w", err)
	}
	if string(head) != headerMagic {
		return nil, fmt.Errorf("%w: no header", ErrBadPack)
	}
	tr := make([]byte, TrailerSize)
	if _, err := r.ReadAt(tr, size-TrailerSize); err != nil {
		return nil, fmt.Errorf("read pack trailer: %w", err)
	}
	n, sum, err := parseTrailer(tr)
	if err != nil {
		return nil, err
	}
	bodyEnd := size - TrailerSize - n
	if bodyEnd < HeaderSize {
		return nil, fmt.Errorf("%w: footer longer than the pack", ErrBadPack)
	}
	fb := make([]byte, n)
	if _, err := r.ReadAt(fb, bodyEnd); err != nil {
		return nil, fmt.Errorf("read pack footer: %w", err)
	}
	return parseFooter(fb, sum, bodyEnd)
}

// --- the layout ---------------------------------------------------------------

// Every pack is `<container>/<aa>/<sha256>.pack`, `aa` the first two hex
// characters of its own sha256: 256 first-level folders, created once each
// (the WebDAV driver caches the collections it has made), and a capacity of
// 256 × 50,000 packs before Sync's per-folder limit — ~3 EB at 256 MiB. The
// MaxPerFolder guard refuses a seal into a full folder. On the WebDAV driver
// the full path is `<root>/t-_global/_packs/aa/<64 hex>.pack`: 89 characters
// plus the root, against Sync's 248.

// packFolder is the first-level folder of a pack.
func packFolder(sum string) string { return sum[:2] }

// packName is the artifact name of the pack whose sha256 is sum.
func packName(sum string) string { return packFolder(sum) + "/" + sum + ".pack" }

// validPackName reports a name packName could have produced; GC deletes
// nothing else from the container.
func validPackName(name string) bool {
	folder, file, ok := strings.Cut(name, "/")
	if !ok || !strings.HasSuffix(file, ".pack") {
		return false
	}
	sum := strings.TrimSuffix(file, ".pack")
	if len(sum) != 2*sha256.Size || strings.ToLower(sum) != sum {
		return false
	}
	if _, err := hex.DecodeString(sum); err != nil {
		return false
	}
	return folder == packFolder(sum)
}

// sumOfName is the sha256 a valid pack name carries.
func sumOfName(name string) string {
	_, file, _ := strings.Cut(name, "/")
	return strings.TrimSuffix(file, ".pack")
}
