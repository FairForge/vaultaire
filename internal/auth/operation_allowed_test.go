package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// WP-R3-2: UploadPartCopy writes a part of the destination's upload — a key
// that may UploadPart may run it (aws-cli and rclone switch to it on their
// own for large copies). Its source is the dispatcher's copy gate (R5-02).
func TestOperationAllowed_UploadPartCopyOnUploadPart(t *testing.T) {
	assert.True(t, OperationAllowed([]string{OpUploadPart}, OpUploadPartCopy))
	assert.True(t, OperationAllowed([]string{OpUploadPartCopy}, OpUploadPartCopy))
	assert.True(t, OperationAllowed([]string{"*"}, OpUploadPartCopy))
	assert.False(t, OperationAllowed([]string{OpPutObject, OpGetObject}, OpUploadPartCopy))
	assert.False(t, OperationAllowed([]string{OpUploadPartCopy}, OpUploadPart), "the copy grants no plain part upload")
	assert.True(t, OperationAllowed([]string{OpGetObject}, OpGetObject))
	assert.False(t, OperationAllowed([]string{OpUploadPart}, OpPutObject))
}
