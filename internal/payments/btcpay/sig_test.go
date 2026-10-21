package btcpay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Every capture of the lab verifies under the lab's secret with the header
// exactly as BTCPay sent it.
func TestBTCPaySig_Valid(t *testing.T) {
	caps := loadCaptures(t)
	require.NotEmpty(t, caps)
	for _, c := range caps {
		assert.NoError(t, VerifySig(labSecret(t), c.Sig(), c.Raw), c.Name)
	}
}

func TestBTCPaySig_WrongSecret_400(t *testing.T) {
	c := loadCapture(t, anyCapture(t))
	assert.ErrorIs(t, VerifySig("not-the-secret", c.Sig(), c.Raw), ErrBadSignature)
	// A body changed by one byte fails under the right secret.
	tampered := append([]byte(nil), c.Raw...)
	tampered[len(tampered)/2] ^= 0x01
	assert.ErrorIs(t, VerifySig(labSecret(t), c.Sig(), tampered), ErrBadSignature)
	// An empty secret verifies nothing, even a header computed with it.
	assert.ErrorIs(t, VerifySig("", sign("", c.Raw), c.Raw), ErrBadSignature)
}

func TestBTCPaySig_MissingHeader_400(t *testing.T) {
	c := loadCapture(t, anyCapture(t))
	assert.ErrorIs(t, VerifySig(labSecret(t), "", c.Raw), ErrNoSignature)
	assert.ErrorIs(t, VerifySig(labSecret(t), "   ", c.Raw), ErrNoSignature)
}

// The comparison never returns early on the header's shape: every
// malformed header (short, long, not hex, no prefix, another algorithm)
// is the same ErrBadSignature, and the source compares a fixed-size buffer
// with hmac.Equal and has no early return before it.
func TestBTCPaySig_ConstantTime(t *testing.T) {
	body := []byte(`{"type":"InvoiceSettled"}`)
	good := sign("s3cret", body)
	for _, h := range []string{
		good[:len(good)-2],                              // short
		good + "00",                                     // long
		"sha256=" + strings.Repeat("zz", 32),            // not hex
		strings.TrimPrefix(good, "sha256="),             // no prefix
		"sha512=" + strings.TrimPrefix(good, "sha256="), // another algorithm
		"sha256=", // empty digest
	} {
		assert.ErrorIs(t, VerifySig("s3cret", h, body), ErrBadSignature, h)
	}
	require.NoError(t, VerifySig("s3cret", good, body))

	// Source guard: in VerifySig, the only return after the blank-header
	// check is the final one — no length/prefix early exit — and the
	// digest comparison is hmac.Equal.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sig.go", nil, 0)
	require.NoError(t, err)
	var returns int
	var usesHMACEqual bool
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "VerifySig" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ReturnStmt:
				returns++
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "hmac" && x.Sel.Name == "Equal" {
					usesHMACEqual = true
				}
			}
			return true
		})
		return false
	})
	assert.True(t, usesHMACEqual)
	assert.Equal(t, 3, returns, "blank header, the one combined verdict, success — nothing else")
}
