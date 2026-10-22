package translog

import (
	"errors"
	"fmt"
	"io"

	"github.com/transparency-dev/formats/log"
	"golang.org/x/mod/sumdb/note"
)

// Checkpoint is a signed statement of the log's size and root
// (c2sp.org/tlog-checkpoint): "<origin>\n<size>\n<base64 root>\n".
type Checkpoint struct {
	Origin string
	Size   int64
	Root   Hash
}

// GenerateKey makes an Ed25519 note key pair named Origin: skey is the
// signer key (PRIVATE+KEY+…, the secret), vkey the verifier key to publish.
func GenerateKey(rand io.Reader) (skey, vkey string, err error) {
	skey, vkey, err = note.GenerateKey(rand, Origin)
	if err != nil {
		return "", "", fmt.Errorf("translog: generate key: %w", err)
	}
	return skey, vkey, nil
}

// NewSigner parses a signer key; one named for another origin is refused.
// Errors never quote the key.
func NewSigner(skey string) (note.Signer, error) {
	s, err := note.NewSigner(skey)
	if err != nil {
		return nil, errors.New("translog: the signing key is not a note signer key")
	}
	if s.Name() != Origin {
		return nil, fmt.Errorf("translog: the signing key is named %q, not %q", s.Name(), Origin)
	}
	return s, nil
}

// NewVerifier parses a verifier key named Origin.
func NewVerifier(vkey string) (note.Verifier, error) {
	v, err := note.NewVerifier(vkey)
	if err != nil {
		return nil, fmt.Errorf("translog: verifier key: %w", err)
	}
	if v.Name() != Origin {
		return nil, fmt.Errorf("translog: the verifier key is named %q, not %q", v.Name(), Origin)
	}
	return v, nil
}

// SignCheckpoint signs the checkpoint of a tree of size entries with root.
func SignCheckpoint(s note.Signer, size int64, root Hash) ([]byte, error) {
	if size < 0 {
		return nil, ErrBadSize
	}
	body := log.Checkpoint{Origin: Origin, Size: uint64(size), Hash: root[:]}.Marshal()
	msg, err := note.Sign(&note.Note{Text: string(body)}, s)
	if err != nil {
		return nil, fmt.Errorf("translog: sign checkpoint %d: %w", size, err)
	}
	return msg, nil
}

// OpenCheckpoint verifies msg's signature by v and parses it; the origin
// must be Origin.
func OpenCheckpoint(msg []byte, v note.Verifier) (Checkpoint, error) {
	cp, _, _, err := log.ParseCheckpoint(msg, Origin, v)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("translog: open checkpoint: %w", err)
	}
	if len(cp.Hash) != HashSize || cp.Size > 1<<62 {
		return Checkpoint{}, errors.New("translog: open checkpoint: malformed size or root")
	}
	var root Hash
	copy(root[:], cp.Hash)
	return Checkpoint{Origin: cp.Origin, Size: int64(cp.Size), Root: root}, nil
}
