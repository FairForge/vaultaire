package btcpay

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
)

// SigHeader is the header BTCPay signs a webhook delivery with.
const SigHeader = "BTCPay-Sig"

// sigPrefix: the header is "sha256=" + the lowercase hex of
// HMAC-SHA256(UTF-8 secret, the raw body) — no timestamp in the scheme, so
// replay protection is the dedup table (payments.ClaimEvent).
const sigPrefix = "sha256="

var (
	// ErrNoSignature: the delivery carries no BTCPay-Sig (400).
	ErrNoSignature = errors.New("btcpay webhook: no BTCPay-Sig header")
	// ErrBadSignature: the signature does not verify (400).
	ErrBadSignature = errors.New("btcpay webhook: signature mismatch")
)

// VerifySig checks header against HMAC-SHA256(secret, body) in constant
// time: the header's hex is decoded (a malformed one compares as garbage of
// the same length, never returns early on its length or content) and
// compared with hmac.Equal. An empty secret verifies nothing.
func VerifySig(secret, header string, body []byte) error {
	if strings.TrimSpace(header) == "" {
		return ErrNoSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)

	got := make([]byte, sha256.Size)
	hexPart, hasPrefix := strings.CutPrefix(strings.TrimSpace(header), sigPrefix)
	decoded, decErr := hex.DecodeString(hexPart)
	// Copy what decoded into a fixed-size buffer: the comparison below
	// always runs over sha256.Size bytes whatever the header held.
	copy(got, decoded)
	lengthOK := subtle.ConstantTimeEq(int32(len(decoded)), sha256.Size)
	equal := hmac.Equal(got, want)
	if secret == "" || !hasPrefix || decErr != nil || lengthOK != 1 || !equal {
		return ErrBadSignature
	}
	return nil
}
