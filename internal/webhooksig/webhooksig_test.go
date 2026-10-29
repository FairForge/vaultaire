package webhooksig

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSign_VerifiesAndRefusesTamperAndAge(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	h := Sign([]byte(`{"a":1}`), "whsec_x", now)
	assert.Regexp(t, `^t=1800000000,v1=[0-9a-f]{64}$`, h)
	assert.NoError(t, Verify(h, []byte(`{"a":1}`), "whsec_x", now.Add(time.Minute)))
	assert.ErrorIs(t, Verify(h, []byte(`{"a":2}`), "whsec_x", now), ErrInvalid)
	assert.ErrorIs(t, Verify(h, []byte(`{"a":1}`), "whsec_y", now), ErrInvalid)
	assert.ErrorIs(t, Verify(h, []byte(`{"a":1}`), "whsec_x", now.Add(6*time.Minute)), ErrInvalid)
}
