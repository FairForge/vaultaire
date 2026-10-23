package e2ee

import (
	"crypto/subtle"
	"encoding/binary"
	"errors"
)

// Spaces and the signed share (RP37-15a). A space key is 32 random bytes:
// wrapped under the owner's master key, and given to each member by a
// SHARE — the space key sealed to the member's X-Wing key and signed by the
// sender. HPKE base mode alone authenticates nobody: the server, which holds
// every public key, could seal a key it chose to a member. The signature
// under a signing key the member already trusts (compared by fingerprint, out
// of band or through key transparency) is what makes a share the sender's.
//
//	owner wrap  0x01 box under the master key, AD = label ‖ "space-key" ‖ lp(space id)
//	0x05 share  05 ‖ sender fingerprint(32) ‖ recipient fingerprint(32) ‖ be32 n ‖ sealed(n) ‖ signature
//	            sealed    = SealTo(recipient X-Wing key, space key,
//	                        info = "share" ‖ lp(space id), ad = sender fp ‖ recipient fp)
//	            signature = Sign(sender, "share", lp(space id) ‖ everything before it)

const (
	// FormatShare leads a signed share.
	FormatShare = byte(0x05)
	// MaxIDSize bounds a space or object id (opaque bytes, chosen by the
	// client; never a name).
	MaxIDSize = 255

	labelSpaceKey = label + "space-key"
	shareInfo     = "share"
	shareContext  = "share"
)

var (
	// ErrShareUnsigned: a share with no signature.
	ErrShareUnsigned = errors.New("e2ee: share is not signed")
	// ErrShareSignature: a share not signed by the sender the caller
	// trusts (another key signed it, or it names another sender).
	ErrShareSignature = errors.New("e2ee: share is not signed by the trusted sender")
	// ErrShareRecipient: a share addressed to another fingerprint.
	ErrShareRecipient = errors.New("e2ee: share is addressed to another identity")
	// ErrID: a space or object id that is empty or longer than MaxIDSize.
	ErrID = errors.New("e2ee: an id is 1 to 255 bytes")
)

func checkID(id []byte) error {
	if len(id) == 0 || len(id) > MaxIDSize {
		return ErrID
	}
	return nil
}

func spaceKeyAD(spaceID []byte) []byte { return append([]byte(labelSpaceKey), lp(spaceID)...) }

// NewSpaceKey is a fresh space key.
func NewSpaceKey() ([]byte, error) { return RandomKey() }

// WrapSpaceKey wraps a space key under the owner's master key, bound to its
// space id.
func WrapSpaceKey(masterKey, spaceID, spaceKey []byte) ([]byte, error) {
	if err := checkID(spaceID); err != nil {
		return nil, err
	}
	return Seal(masterKey, spaceKey, spaceKeyAD(spaceID))
}

// OpenSpaceKey opens the owner's wrap of a space key.
func OpenSpaceKey(masterKey, spaceID, wrapped []byte) ([]byte, error) {
	if err := checkID(spaceID); err != nil {
		return nil, err
	}
	return open(FormatBox, masterKey, wrapped, spaceKeyAD(spaceID))
}

// NewShare gives spaceKey to recipient: sealed to its X-Wing key, bound to
// the space id and both fingerprints, signed by sender.
func NewShare(sender *Identity, recipient PublicIdentity, spaceID, spaceKey []byte) ([]byte, error) {
	if err := checkID(spaceID); err != nil {
		return nil, err
	}
	if len(spaceKey) != KeySize {
		return nil, ErrKeySize
	}
	senderFP, err := Fingerprint(sender.Public())
	if err != nil {
		return nil, err
	}
	recipientFP, err := Fingerprint(recipient)
	if err != nil {
		return nil, err
	}
	sealed, err := SealTo(recipient.EncPublic, spaceKey, append([]byte(shareInfo), lp(spaceID)...), append(append([]byte{}, senderFP...), recipientFP...))
	if err != nil {
		return nil, err
	}
	if len(sealed) > 1<<16 { // 1 + 1120 + 32 + 16 bytes: never
		return nil, ErrFormat
	}
	out := make([]byte, 0, 1+2*FingerprintSize+4+len(sealed)+65)
	out = append(out, FormatShare)
	out = append(out, senderFP...)
	out = append(out, recipientFP...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(sealed)))
	out = append(out, sealed...)
	sig, err := Sign(sender.SigPrivate, shareContext, append(lp(spaceID), out...))
	if err != nil {
		return nil, err
	}
	return append(out, sig...), nil
}

// OpenShare opens a share addressed to me for the space spaceID, signed by
// sender — the identity the caller already trusts (by fingerprint). It
// refuses a share addressed to another fingerprint (ErrShareRecipient), one
// without a signature (ErrShareUnsigned), and one not signed by sender's
// signing key (ErrShareSignature), before it opens anything.
func OpenShare(me *Identity, share []byte, sender PublicIdentity, spaceID []byte) ([]byte, error) {
	if err := checkID(spaceID); err != nil {
		return nil, err
	}
	const head = 1 + 2*FingerprintSize + 4
	if len(share) < head || share[0] != FormatShare {
		return nil, ErrFormat
	}
	n := int(binary.BigEndian.Uint32(share[head-4 : head]))
	if n < 1+EncapsulationSize+16 || n > len(share)-head {
		return nil, ErrFormat
	}
	senderFP, recipientFP := share[1:1+FingerprintSize], share[1+FingerprintSize:1+2*FingerprintSize]
	signedPart, sig := share[:head+n], share[head+n:]

	myFP, err := Fingerprint(me.Public())
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(recipientFP, myFP) != 1 {
		return nil, ErrShareRecipient
	}
	if len(sig) == 0 {
		return nil, ErrShareUnsigned
	}
	trustedFP, err := Fingerprint(sender)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(senderFP, trustedFP) != 1 {
		return nil, ErrShareSignature
	}
	if err := Verify(sender.SigPublic, shareContext, append(lp(spaceID), signedPart...), sig); err != nil {
		return nil, ErrShareSignature
	}
	key, err := OpenSealed(me.EncPrivate, share[head:head+n], append([]byte(shareInfo), lp(spaceID)...), share[1:1+2*FingerprintSize])
	if err != nil {
		return nil, err
	}
	if len(key) != KeySize {
		return nil, ErrFormat
	}
	return key, nil
}
