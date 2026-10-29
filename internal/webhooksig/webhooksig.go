// Package webhooksig is the webhook signature (WP-R11-3, Stripe's shape):
// `t=<unix>,v1=<hex HMAC-SHA256(secret, "<t>." + body)>`. The server's
// deliveries (internal/api) and `stored listen`'s forwards (cmd/stored)
// sign with it; a receiver checks v1 and refuses a t more than Tolerance
// from its clock (docs/API.md).
package webhooksig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Tolerance is how far t may be from the receiver's clock.
const Tolerance = 5 * time.Minute

// Sign is the X-Webhook-Signature value of body at at.
func Sign(body []byte, secret string, at time.Time) string {
	t := strconv.FormatInt(at.Unix(), 10)
	return "t=" + t + ",v1=" + hex.EncodeToString(mac(body, secret, t))
}

func mac(body []byte, secret, t string) []byte {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(t))
	h.Write([]byte("."))
	h.Write(body)
	return h.Sum(nil)
}

// ErrInvalid: the header does not sign body with secret within Tolerance.
var ErrInvalid = errors.New("webhook signature does not verify")

// Verify checks header against body, secret and now.
func Verify(header string, body []byte, secret string, now time.Time) error {
	var t, v1 string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "t":
			t = v
		case "v1":
			v1 = v
		}
	}
	sec, err := strconv.ParseInt(t, 10, 64)
	if err != nil || v1 == "" {
		return ErrInvalid
	}
	if d := now.Sub(time.Unix(sec, 0)); d > Tolerance || d < -Tolerance {
		return ErrInvalid
	}
	got, err := hex.DecodeString(v1)
	if err != nil || !hmac.Equal(got, mac(body, secret, t)) {
		return ErrInvalid
	}
	return nil
}
