package api

import (
	"errors"
	"os"
	"strings"

	"github.com/FairForge/vaultaire/internal/engine"
)

// isObjectMissingErr reports whether an engine operation error means the
// object does not exist, as opposed to the backend failing. Misses arrive in
// different shapes depending on which driver served the request: the engine's
// typed NotFoundError, os.ErrNotExist from the local driver, or AWS-SDK error
// text from S3-class drivers (iDrive, Lyve, s3) — "api error NoSuchKey",
// "api error NotFound", "StatusCode: 404". Handlers must map every shape to
// 404 NoSuchKey: matching only the local driver's strings turned each miss on
// an S3-class backend into a 500 in prod (found via the LET demo, 2026-07-31).
// Mirrors the taxonomy in engine.isBackendFailure — keep the two in sync.
func isObjectMissingErr(err error) bool {
	if err == nil {
		return false
	}
	var nf engine.NotFoundError
	if errors.As(err, &nf) {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{
		"no such file or directory",
		"not found",
		"NoSuchKey",
		"NotFound",
		"StatusCode: 404",
		"status code: 404",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
