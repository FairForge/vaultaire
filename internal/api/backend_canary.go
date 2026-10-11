package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
)

// The object canary (Prompt 2b.5 F2.1).
//
// A signed HeadBucket proves the key can see the bucket, not that it can
// read or write an object in it. On 2026-10-11 the iDrive key answered
// HeadBucket and ListObjects 200 and every object call 403 for ≥ 9 hours:
// /health said "healthy", vaultaire_backend_health{idrive} stayed 1, and the
// breaker opened only when bench traffic charged it. Each S3-class probe now
// also HEADs one tiny object of ours — what a customer GET does first.
//
// The object is `t-_probe/_probe/<hostname>` on the fixed-bucket drivers: a
// reserved tenant like `_global` (a tenant id is `tenant-<hex>`, never
// `_probe`), written on the first probe when it is absent. A miss
// ((false, nil): a lifecycle rule, an operator's cleanup — or a HEAD 403,
// which is how S3 answers an absent key to a credential without ListBucket)
// is re-written once and asked again; anything else — the HEAD's error, a
// PUT refused, still absent after the PUT — is the probe failing. Never on tape (geyser) or
// the OneDrive fleet (permafrost): they keep their HeadBucket / Graph probe,
// and the WebDAV driver its PROPFIND.
//
// Cost: one HEAD per probe interval (30 s, 60 s with Lyve's console probe)
// per backend ≈ 86k requests a month, under every vendor's free floor.
const (
	probeCanaryTenant    = "_probe"
	probeCanaryContainer = "_probe"
)

// canaryBackend reports the S3-class backends the canary runs on.
func canaryBackend(name string) bool {
	switch name {
	case "idrive", "wasabi", "lyve", "r2", "s3compat":
		return true
	}
	return strings.HasPrefix(name, "idrive-")
}

// driverGetter is the slice of the engine the canary needs; *engine.CoreEngine
// satisfies it.
type driverGetter interface {
	GetDriver(name string) (engine.Driver, bool)
}

// probeCanaryKey is this box's canary: one object per host (both deploy
// slots share it).
func probeCanaryKey() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_' {
			return r
		}
		return '_'
	}, host)
}

// checkObjectCanary HEADs the canary on drv and re-writes it once when it is
// absent. A nil return means an object call on this backend works.
func checkObjectCanary(ctx context.Context, drv engine.Driver, key string) error {
	cctx := common.WithTenantID(ctx, probeCanaryTenant)
	ok, err := drv.Exists(cctx, probeCanaryContainer, key)
	if err != nil && !isForbidden(err) {
		return fmt.Errorf("object canary HEAD %s/%s: %w", probeCanaryContainer, key, err)
	}
	if ok {
		return nil
	}
	// Absent — or a 403, which is also what S3 answers for an ABSENT key
	// to a credential without ListBucket: the PUT tells the two apart (a
	// dead key refuses it too; a scoped key writes it and is healthy).
	headErr := err
	body := []byte("vaultaire probe canary: " + key + "\n")
	if err := drv.Put(cctx, probeCanaryContainer, key, bytes.NewReader(body), engine.WithContentLength(int64(len(body)))); err != nil {
		if headErr != nil {
			return fmt.Errorf("object canary HEAD %s/%s: %w (and its PUT: %w)", probeCanaryContainer, key, headErr, err)
		}
		return fmt.Errorf("object canary absent, PUT %s/%s: %w", probeCanaryContainer, key, err)
	}
	ok, err = drv.Exists(cctx, probeCanaryContainer, key)
	if err != nil {
		return fmt.Errorf("object canary HEAD after PUT %s/%s: %w", probeCanaryContainer, key, err)
	}
	if !ok {
		return errors.New("object canary written but absent on the next HEAD")
	}
	return nil
}

// isForbidden reports an S3 403: AccessDenied, or a bodiless HEAD's
// Forbidden.
func isForbidden(err error) bool {
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusForbidden {
		return true
	}
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && (apiErr.ErrorCode() == "AccessDenied" || apiErr.ErrorCode() == "Forbidden")
}

// withObjectCanary composes the canary after a backend's probe: the
// bucket-level probe first (a dead endpoint or key says so in its own
// words), then the object HEAD. base nil = the canary alone.
func withObjectCanary(base func(context.Context) error, drv engine.Driver, key string) func(context.Context) error {
	return func(ctx context.Context) error {
		if base != nil {
			if err := base(ctx); err != nil {
				return err
			}
		}
		return checkObjectCanary(ctx, drv, key)
	}
}
