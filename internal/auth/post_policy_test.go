package auth

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestParsePostPolicy_TheShapesAndAnUnknownOperator(t *testing.T) {
	p, err := ParsePostPolicy(b64(`{"expiration":"2099-01-01T00:00:00.000Z","conditions":[
		{"bucket":"b"},["eq","$key","k"],["starts-with","$Content-Type","image/"],["content-length-range",1,"100"],
		["content-length-range",10,1000]]}`))
	require.NoError(t, err)
	assert.True(t, p.Expiration.Equal(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)))
	require.Len(t, p.Conditions, 5)
	assert.Equal(t, PolicyCondition{Op: "eq", Field: "bucket", Value: "b"}, p.Conditions[0])
	assert.Equal(t, PolicyCondition{Op: "starts-with", Field: "content-type", Value: "image/"}, p.Conditions[2])
	lo, hi, ok := p.SizeRange()
	assert.True(t, ok)
	assert.Equal(t, [2]int64{10, 100}, [2]int64{lo, hi}, "every range holds: the narrowest")

	for name, doc := range map[string]string{
		"unknown operator":  `{"expiration":"2099-01-01T00:00:00Z","conditions":[["ends-with","$key","x"]]}`,
		"no expiration":     `{"conditions":[{"bucket":"b"}]}`,
		"a bad expiration":  `{"expiration":"tomorrow","conditions":[]}`,
		"a field with no $": `{"expiration":"2099-01-01T00:00:00Z","conditions":[["eq","key","x"]]}`,
		"a reversed range":  `{"expiration":"2099-01-01T00:00:00Z","conditions":[["content-length-range",10,1]]}`,
		"a numeric value":   `{"expiration":"2099-01-01T00:00:00Z","conditions":[{"bucket":7}]}`,
		"two elements":      `{"expiration":"2099-01-01T00:00:00Z","conditions":[["eq","$key"]]}`,
		"not JSON":          `expiration`,
	} {
		_, err := ParsePostPolicy(b64(doc))
		assert.ErrorIs(t, err, ErrPolicyMalformed, name)
	}
	_, err = ParsePostPolicy("%%%not base64")
	assert.ErrorIs(t, err, ErrPolicyMalformed)
}

func TestPostPolicyCheck_EveryFieldNamedEveryConditionHeld(t *testing.T) {
	p, err := ParsePostPolicy(b64(`{"expiration":"2099-01-01T00:00:00Z","conditions":[{"bucket":"b"},["starts-with","$key","u/"],
		["starts-with","$content-type","image/"]]}`))
	require.NoError(t, err)
	base := func() map[string]string {
		return map[string]string{"bucket": "b", "key": "u/x", "content-type": "image/png", "policy": "p", "x-amz-signature": "s", "x-ignore-me": "1"}
	}
	assert.NoError(t, p.check(base()))
	f := base()
	f["content-type"] = "image/png, text/html"
	assert.ErrorIs(t, p.check(f), ErrPolicyCondition, "each comma-separated type must match")
	f = base()
	f["x-amz-meta-extra"] = "1"
	assert.ErrorIs(t, p.check(f), ErrPolicyCondition, "an unnamed field")
	f = base()
	f["key"] = "v/x"
	assert.ErrorIs(t, p.check(f), ErrPolicyCondition)
	noBucket, err := ParsePostPolicy(b64(`{"expiration":"2099-01-01T00:00:00Z","conditions":[["starts-with","$key",""]]}`))
	require.NoError(t, err)
	assert.ErrorIs(t, noBucket.check(map[string]string{"bucket": "b", "key": "k"}), ErrPolicyCondition, "a policy must name its bucket")
}
