package btcpay

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The lab's captures (Stage 1a Prompt 24, BTCPay on regtest): each
// testdata/<event>_<case>.json is one webhook delivery exactly as BTCPay
// sent it — its headers and its raw body (base64, the bytes the signature
// covers) — with the invoice and its payment methods as Greenfield
// answered right after it (<name>.invoice.json, <name>.payment-methods.json).
// Never hand-written where a capture exists.

type capture struct {
	Name    string
	Headers map[string]string
	Raw     []byte
}

// Sig is the delivery's BTCPay-Sig header, whatever the capture's casing.
func (c capture) Sig() string {
	for k, v := range c.Headers {
		if strings.EqualFold(k, SigHeader) {
			return v
		}
	}
	return ""
}

// labSecret is the lab store's webhook secret (a throwaway: the regtest
// store and its BTCPay were deleted after the spike).
func labSecret(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "SECRET.txt"))
	require.NoError(t, err)
	return strings.TrimSpace(string(raw))
}

func loadCapture(t *testing.T, name string) capture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	require.NoError(t, err, name)
	var f struct {
		Headers map[string]any `json:"headers"`
		BodyB64 string         `json:"body_b64"`
	}
	require.NoError(t, json.Unmarshal(raw, &f), name)
	body, err := base64.StdEncoding.DecodeString(f.BodyB64)
	require.NoError(t, err, name)
	h := map[string]string{}
	for k, v := range f.Headers {
		switch x := v.(type) {
		case string:
			h[http.CanonicalHeaderKey(k)] = x
		case []any:
			if len(x) > 0 {
				s, _ := x[0].(string)
				h[http.CanonicalHeaderKey(k)] = s
			}
		}
	}
	return capture{Name: name, Headers: h, Raw: body}
}

// captureNames lists every delivery capture (not the .invoice/.payment-
// methods companions), sorted.
func captureNames(t *testing.T) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	require.NoError(t, err)
	var out []string
	for _, p := range m {
		b := strings.TrimSuffix(filepath.Base(p), ".json")
		if strings.Contains(b, ".") {
			continue
		}
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

func loadCaptures(t *testing.T) []capture {
	t.Helper()
	var out []capture
	for _, n := range captureNames(t) {
		out = append(out, loadCapture(t, n))
	}
	return out
}

func anyCapture(t *testing.T) string {
	t.Helper()
	n := captureNames(t)
	require.NotEmpty(t, n)
	return n[0]
}

// companion reads <name>.<kind>.json (invoice, payment-methods).
func companion(t *testing.T, name, kind string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name+"."+kind+".json"))
	require.NoError(t, err, name+"."+kind)
	return raw
}
