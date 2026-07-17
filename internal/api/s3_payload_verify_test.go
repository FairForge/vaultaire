package api

import (
	"errors"
	"fmt"
	"testing"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/stretchr/testify/assert"
)

// A body-read failure caused by the payload not matching its signed
// x-amz-content-sha256 declaration is the client's fault (400); any other
// read failure stays a 500.
func TestBodyReadErrorCode(t *testing.T) {
	assert.Equal(t, ErrXAmzContentSHA256Mismatch,
		bodyReadErrorCode(auth.ErrContentSHA256Mismatch))
	assert.Equal(t, ErrXAmzContentSHA256Mismatch,
		bodyReadErrorCode(fmt.Errorf("store chunk: %w", auth.ErrContentSHA256Mismatch)))
	assert.Equal(t, ErrInternalError,
		bodyReadErrorCode(errors.New("disk on fire")))
}

func TestXAmzContentSHA256MismatchError_Is400(t *testing.T) {
	assert.Equal(t, 400, errorStatusCodes[ErrXAmzContentSHA256Mismatch])
	assert.Equal(t, 400, errorStatusCodes[ErrInvalidArgument])
	assert.NotEmpty(t, errorMessages[ErrXAmzContentSHA256Mismatch])
}
