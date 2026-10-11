package api

import (
	"github.com/FairForge/vaultaire/internal/engine"
)

// isObjectMissingErr reports whether an engine operation error means the
// object does not exist, as opposed to the backend failing. It is
// engine.IsObjectMiss — one predicate, so the breaker (which never charges a
// miss), the engine's Delete (which drops the route of a miss) and the
// handlers (which answer 404 NoSuchKey) can never disagree.
//
// Misses arrive typed from every driver: the engine's NotFoundError (WebDAV/
// Sync, permafrost), os.ErrNotExist from the local driver, or an aws-sdk-go-v2
// chain from the S3-class drivers (iDrive, Lyve, R2, Geyser, s3compat) —
// types.NoSuchKey, types.NotFound, the GenericAPIError of an empty-body 404,
// any HTTP 404 but NoSuchBucket (R6-01). Every handler must map each shape to
// 404 NoSuchKey: matching only the local driver's strings turned each miss on
// an S3-class backend into a 500 in prod (found via the LET demo, 2026-07-31).
//
// Never by text (Prompt 2b.5 F1.1): the drivers and the engine put the key in
// every message, so `errors/page not found.html` on a backend answering 403
// read as "does not exist" — 404 on GET, 204 on DELETE with the head row
// dropped while the bytes stayed. An error wrapping
// engine.ErrAllBackendsUnavailable or ErrPartiallyUnavailable is NEVER a miss,
// whatever a fallback backend said inside it (R6-02): handlers answer 503.
func isObjectMissingErr(err error) bool {
	return engine.IsObjectMiss(err)
}
