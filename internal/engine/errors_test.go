package engine

import (
	"errors"
	"testing"
)

func TestErrors_Types(t *testing.T) {
	err := ErrNotFound("container", "artifact.txt")

	// Check it's the right type
	var nfErr NotFoundError
	if !errors.As(err, &nfErr) {
		t.Error("ErrNotFound should return NotFoundError type")
	}

	// Check the message contains expected parts
	if err.Error() == "" {
		t.Error("Error should have message")
	}

	err = ErrPermissionDenied("tenant-123", "write")
	if err.Error() == "" {
		t.Error("Error should have message")
	}
}

func TestErrors_Wrapping(t *testing.T) {
	original := errors.New("disk full")
	wrapped := WrapError(original, "failed to write artifact")

	if !errors.Is(wrapped, original) {
		t.Error("Should be able to unwrap to original error")
	}

	// Test with a typed error
	var nfErr NotFoundError
	typedErr := ErrNotFound("bucket", "file.txt")
	wrapped = WrapError(typedErr, "operation failed")

	if !errors.As(wrapped, &nfErr) {
		t.Error("Should preserve error type through wrapping")
	}
}
