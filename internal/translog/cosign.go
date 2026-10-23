package translog

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/transparency-dev/formats/log"
	fnote "github.com/transparency-dev/formats/note"
	"golang.org/x/mod/sumdb/note"
)

// Cosignatures (K4c-2, Stage 1a Prompt 34b; c2sp.org/tlog-cosignature): a
// witness's timestamped statement that a checkpoint is consistent with
// every view of the log it saw before, and the largest one it saw as of
// that time. None of the signature code is ours: transparency-dev/formats/note
// signs and verifies (cosignature/v1), golang.org/x/mod/sumdb/note puts the
// lines on the note.

// The cosignature/v1 key types (c2sp.org/signed-note § Signature types).
const (
	cosigEd25519 = 0x04 // Ed25519 cosignature/v1 (checkpoint cosignatures only)
	cosigMLDSA44 = 0x06 // ML-DSA-44 cosignature/v1 — "SHOULD be used for new deployments"
)

// GenerateCosignerKey makes an ML-DSA-44 cosignature/v1 key pair named name
// (a witness's own name, never the log's origin): skey is the signer key
// (PRIVATE+KEY+…, the secret), vkey the verifier key to publish.
func GenerateCosignerKey(name string) (skey, vkey string, err error) {
	if name == Origin {
		return "", "", fmt.Errorf("translog: a cosigner key cannot be named %q, the log's origin", Origin)
	}
	skey, vkey, err = fnote.GenerateMLDSAKey(name)
	if err != nil {
		return "", "", fmt.Errorf("translog: generate a cosigner key named %q: %w", name, err)
	}
	return skey, vkey, nil
}

// NewCosigner parses an ML-DSA-44 cosignature/v1 signer key. Its Sign
// takes a checkpoint note's text and returns the cosignature value
// (timestamp ‖ signature) over (origin, size, root) at now(), so note.Sign
// puts a cosignature line on the note. A plain note key, an Ed25519 one, or
// one named for the log's origin is refused. Errors never quote the key.
func NewCosigner(skey string, now func() time.Time) (note.Signer, error) {
	s, err := fnote.NewMLDSASigner(skey)
	if err != nil {
		return nil, errors.New("translog: the cosigner key is not an ML-DSA-44 cosignature/v1 signer key (translog-keygen -cosignature)")
	}
	if s.Name() == Origin {
		return nil, fmt.Errorf("translog: the cosigner key is named %q, the log's origin", Origin)
	}
	if now == nil {
		now = time.Now
	}
	return &cosigner{s: s, now: now}, nil
}

type cosigner struct {
	s   *fnote.SubtreeSigner
	now func() time.Time
}

func (c *cosigner) Name() string    { return c.s.Name() }
func (c *cosigner) KeyHash() uint32 { return c.s.KeyHash() }

// Sign is the checkpoint cosignature of msg (a checkpoint body, the note's
// text): the subtree [0, size) of the origin at the timestamp now()
// (c2sp.org/tlog-cosignature § ML-DSA-44).
func (c *cosigner) Sign(msg []byte) ([]byte, error) {
	var cp log.Checkpoint
	if _, err := cp.Unmarshal(msg); err != nil {
		return nil, fmt.Errorf("translog: cosign: %w", err)
	}
	t := c.now().Unix()
	if t <= 0 {
		return nil, errors.New("translog: cosign: the clock is before 1970")
	}
	return c.s.SignSubtree(uint64(t), cp.Origin, 0, cp.Size, cp.Hash)
}

// NewCosignatureVerifier parses a witness's cosignature/v1 verifier key:
// Ed25519 (type 0x04) or ML-DSA-44 (0x06). A plain note key (0x01) is
// refused — its signatures carry no timestamp and say nothing about
// consistency — and so is a key named for the log's origin.
func NewCosignatureVerifier(vkey string) (note.Verifier, error) {
	vkey = strings.TrimSpace(vkey)
	parts := strings.SplitN(vkey, "+", 3)
	if len(parts) != 3 {
		return nil, errors.New("translog: a cosignature verifier key is <name>+<key ID>+<base64>")
	}
	raw, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil || len(raw) == 0 {
		return nil, errors.New("translog: the cosignature verifier key's key is not base64")
	}
	if raw[0] != cosigEd25519 && raw[0] != cosigMLDSA44 {
		return nil, fmt.Errorf("translog: verifier key type 0x%02x is not a cosignature/v1 key (0x04 Ed25519, 0x06 ML-DSA-44)", raw[0])
	}
	if parts[0] == Origin {
		return nil, fmt.Errorf("translog: a witness key cannot be named %q, the log's origin", Origin)
	}
	v, err := fnote.NewVerifierForCosignatureV1(vkey)
	if err != nil {
		return nil, fmt.Errorf("translog: cosignature verifier key: %w", err)
	}
	if want, err := strconv.ParseUint(parts[1], 16, 32); err != nil || len(parts[1]) != 8 || uint32(want) != v.KeyHash() {
		return nil, errors.New("translog: the cosignature verifier key's key ID does not match its key")
	}
	return v, nil
}

// KeyID names a key in one string: "<name>+<8 hex key ID>".
func KeyID(name string, hash uint32) string { return fmt.Sprintf("%s+%08x", name, hash) }

// Cosignature is one verified cosignature on a checkpoint.
type Cosignature struct {
	Name    string
	KeyHash uint32
	// Time is the cosignature's own timestamp.
	Time time.Time
	// Line is the signature line exactly as on the note, "— <name> <base64>\n".
	Line string
}

// KeyID is the cosigner's KeyID.
func (c Cosignature) KeyID() string { return KeyID(c.Name, c.KeyHash) }

// OpenCosigned verifies msg — a checkpoint of this log signed by logV,
// carrying any number of cosignature lines — and returns the checkpoint and
// the cosignatures by witnesses that verified. A line by a witness that is
// not listed is ignored (c2sp.org/signed-note); a line whose key name and ID
// are a listed witness's but whose signature does not verify — forged, or
// copied from another checkpoint — makes the whole note malformed (an
// error), never "one cosignature fewer".
func OpenCosigned(msg []byte, logV note.Verifier, witnesses ...note.Verifier) (Checkpoint, []Cosignature, error) {
	cp, err := OpenCheckpoint(msg, logV)
	if err != nil {
		return Checkpoint{}, nil, err
	}
	known := make(map[string]bool, len(witnesses))
	for _, w := range witnesses {
		if w.Name() == logV.Name() && w.KeyHash() == logV.KeyHash() {
			return Checkpoint{}, nil, errors.New("translog: the log's own key is not a witness")
		}
		known[KeyID(w.Name(), w.KeyHash())] = true
	}
	if len(witnesses) == 0 {
		return cp, nil, nil
	}
	n, err := note.Open(msg, note.VerifierList(append([]note.Verifier{logV}, witnesses...)...))
	if err != nil {
		var inv *note.InvalidSignatureError
		if errors.As(err, &inv) {
			return Checkpoint{}, nil, fmt.Errorf("translog: the cosignature by %s+%08x does not verify over this checkpoint (forged, or another checkpoint's)", inv.Name, inv.Hash)
		}
		return Checkpoint{}, nil, fmt.Errorf("translog: open the cosigned checkpoint: %w", err)
	}
	var out []Cosignature
	for _, s := range n.Sigs {
		if !known[KeyID(s.Name, s.Hash)] {
			continue // the log's own signature
		}
		t, err := fnote.CoSigV1Timestamp(s)
		if err != nil {
			return Checkpoint{}, nil, fmt.Errorf("translog: the cosignature by %s carries no timestamp: %w", s.Name, err)
		}
		out = append(out, Cosignature{Name: s.Name, KeyHash: s.Hash, Time: t, Line: "— " + s.Name + " " + s.Base64 + "\n"})
	}
	return cp, out, nil
}

// WitnessPolicy is what a reader requires of a checkpoint's cosignatures
// (translog-verify -witness … -min N).
type WitnessPolicy struct {
	Witnesses []note.Verifier
	// Min is how many distinct witnesses must have cosigned.
	Min int
	// MaxAge: a cosignature older than this is stale and does not count
	// (0 = no limit). One in the future by more than MaxSkew never counts.
	MaxAge, MaxSkew time.Duration
	Now             func() time.Time
}

// ErrTooFewCosignatures: fewer than Min fresh cosignatures by the listed witnesses.
var ErrTooFewCosignatures = errors.New("translog: too few fresh cosignatures")

// Verify opens msg under the policy: the log's signature, every listed
// witness's line verified (OpenCosigned), and at least Min of them fresh.
// It returns the checkpoint, the fresh cosignatures, and why the others
// did not count.
func (p WitnessPolicy) Verify(msg []byte, logV note.Verifier) (Checkpoint, []Cosignature, []string, error) {
	cp, cos, err := OpenCosigned(msg, logV, p.Witnesses...)
	if err != nil {
		return Checkpoint{}, nil, nil, err
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	skew := p.MaxSkew
	if skew <= 0 {
		skew = 5 * time.Minute
	}
	t := now()
	var fresh []Cosignature
	var skipped []string
	for _, c := range cos {
		switch {
		case c.Time.After(t.Add(skew)):
			skipped = append(skipped, fmt.Sprintf("%s: timestamp %s is in the future", c.KeyID(), c.Time.UTC().Format(time.RFC3339)))
		case p.MaxAge > 0 && t.Sub(c.Time) > p.MaxAge:
			skipped = append(skipped, fmt.Sprintf("%s: stale, cosigned %s (more than %s ago)", c.KeyID(), c.Time.UTC().Format(time.RFC3339), p.MaxAge))
		default:
			fresh = append(fresh, c)
		}
	}
	if len(fresh) < p.Min {
		return cp, fresh, skipped, fmt.Errorf("%w: %d of the %d required (%d listed witness line(s) on the checkpoint)", ErrTooFewCosignatures, len(fresh), p.Min, len(cos))
	}
	return cp, fresh, skipped, nil
}

// CosignatureLines are the lines of sigs (a note's signature lines, or a
// witness's add-checkpoint answer) made by signer s: what a witness answers
// and what the log keeps.
func CosignatureLines(sigs []byte, name string, hash uint32) []string {
	var out []string
	for _, l := range bytes.SplitAfter(sigs, []byte("\n")) {
		rest, ok := bytes.CutPrefix(l, []byte("— "+name+" "))
		if !ok || !bytes.HasSuffix(rest, []byte("\n")) {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(string(bytes.TrimSuffix(rest, []byte("\n"))))
		if err != nil || len(raw) < 4 || uint32(raw[0])<<24|uint32(raw[1])<<16|uint32(raw[2])<<8|uint32(raw[3]) != hash {
			continue
		}
		out = append(out, string(l))
	}
	return out
}

// ErrNoCosignature: a witness's answer carries no line its pinned key verifies.
var ErrNoCosignature = errors.New("translog: no cosignature by the witness's key")

// VerifyAnswer checks a witness's add-checkpoint answer (signature lines)
// against checkpoint (the note the log submitted) and the witness's pinned
// key w: the answer is concatenated to the note and opened with w alone
// (the spec's "the client MAY concatenate it to the checkpoint"). Lines by
// other keys are ignored; a line that claims w's key and does not verify is
// an error; no line by w is ErrNoCosignature.
func VerifyAnswer(checkpoint, answer []byte, w note.Verifier) ([]Cosignature, error) {
	if len(answer) == 0 || answer[len(answer)-1] != '\n' {
		return nil, ErrNoCosignature
	}
	n, err := note.Open(append(append([]byte(nil), checkpoint...), answer...), note.VerifierList(w))
	if err != nil {
		var unverified *note.UnverifiedNoteError
		if errors.As(err, &unverified) {
			return nil, ErrNoCosignature
		}
		return nil, fmt.Errorf("translog: the witness's answer: %w", err)
	}
	var out []Cosignature
	for _, s := range n.Sigs {
		t, err := fnote.CoSigV1Timestamp(s)
		if err != nil {
			return nil, fmt.Errorf("translog: the cosignature by %s carries no timestamp: %w", s.Name, err)
		}
		out = append(out, Cosignature{Name: s.Name, KeyHash: s.Hash, Time: t, Line: "— " + s.Name + " " + s.Base64 + "\n"})
	}
	return out, nil
}
