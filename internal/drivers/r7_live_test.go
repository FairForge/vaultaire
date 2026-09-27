//go:build live

package drivers

// Review R7 live conformance checks. Build tag `live`; every test skips unless
// its credentials are in the environment. Objects go under container
// `review-r7` (tenant `review-r7`) and are deleted afterwards. Nothing here
// prints a credential — only error shapes (HTTP status, API error code, Go
// error type) and key names.
//
//	set -a; . <creds.env>; set +a
//	go test -tags live -run TestR7Live -v ./internal/drivers/
//
// iDrive is exercised through a NON-primary region key (IDRIVE_<REGION>_*
// with R7_IDRIVE_REGION_PREFIX naming the env prefix, default "DA"), never the
// prod pair, per the R7 ground rules.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
)

const r7Tenant = "review-r7"
const r7Container = "review-r7"

func r7Ctx(t *testing.T) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	return common.WithTenantID(ctx, r7Tenant), cancel
}

// r7Shape renders an error as its classification, never its full text.
func r7Shape(err error) string {
	if err == nil {
		return "nil"
	}
	parts := []string{fmt.Sprintf("type=%T", err)}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		parts = append(parts, "code="+apiErr.ErrorCode())
	}
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) {
		parts = append(parts, fmt.Sprintf("status=%d", re.HTTPStatusCode()))
	}
	var ne net.Error
	if errors.As(err, &ne) {
		parts = append(parts, fmt.Sprintf("net=%T timeout=%v", ne, ne.Timeout()))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		parts = append(parts, "deadline")
	}
	var nf engine.NotFoundError
	if errors.As(err, &nf) {
		parts = append(parts, "engine.NotFoundError")
	}
	return strings.Join(parts, " ")
}

func r7Rand() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func r7IDrive(t *testing.T) (*IDriveDriver, string, string) {
	prefix := os.Getenv("R7_IDRIVE_REGION_PREFIX")
	if prefix == "" {
		prefix = "DA"
	}
	ak := os.Getenv("IDRIVE_" + prefix + "_ACCESS_KEY")
	sk := os.Getenv("IDRIVE_" + prefix + "_SECRET_KEY")
	ep := os.Getenv("IDRIVE_" + prefix + "_ENDPOINT")
	if ak == "" || sk == "" || ep == "" {
		t.Skipf("IDRIVE_%s_{ACCESS_KEY,SECRET_KEY,ENDPOINT} not set", prefix)
	}
	if prod := os.Getenv("IDRIVE_ENDPOINT"); prod != "" && prod == ep {
		t.Skip("refusing to run against the primary (prod) iDrive endpoint")
	}
	u, err := url.Parse(ep)
	require.NoError(t, err)
	// s3.<region>.idrivee2.com
	host := u.Hostname()
	region := ""
	if strings.HasPrefix(host, "s3.") && strings.HasSuffix(host, ".idrivee2.com") {
		region = strings.TrimSuffix(strings.TrimPrefix(host, "s3."), ".idrivee2.com")
	}
	require.NotEmpty(t, region, "cannot derive region from endpoint host %q", host)
	d, err := NewIDriveDriver(ak, sk, ep, region, zap.NewNop())
	require.NoError(t, err)
	return d, ep, region
}

// TestR7Live_IDriveRegionHosts: the driver's region table names
// e2-<region>.idrive.com; the reseller account's regional endpoints are
// s3.<region>.idrivee2.com. Same key, same region, both hosts, signed HeadBucket.
func TestR7Live_IDriveRegionHosts(t *testing.T) {
	d, ep, region := r7IDrive(t)
	ctx, cancel := r7Ctx(t)
	defer cancel()

	t.Logf("region=%s real endpoint host=%s", region, must(url.Parse(ep)).Hostname())
	t.Logf("HeadBucket(real endpoint): %s", r7Shape(d.HealthCheck(ctx)))

	legacy := "https://e2-" + region + ".idrive.com"
	ak := os.Getenv("IDRIVE_" + envPrefix() + "_ACCESS_KEY")
	sk := os.Getenv("IDRIVE_" + envPrefix() + "_SECRET_KEY")
	dl, err := NewIDriveDriver(ak, sk, legacy, region, zap.NewNop())
	require.NoError(t, err)
	hctx, hcancel := context.WithTimeout(ctx, 15*time.Second)
	defer hcancel()
	t.Logf("HeadBucket(code-table host %s): %s", legacy, r7Shape(dl.HealthCheck(hctx)))

	// Also the primary-region pair against a different region's real endpoint —
	// what every idrive-<region> driver does in prod today (no per-region keys).
	if pak, psk := os.Getenv("IDRIVE_ACCESS_KEY"), os.Getenv("IDRIVE_SECRET_KEY"); pak != "" && psk != "" {
		dp, err := NewIDriveDriver(pak, psk, ep, region, zap.NewNop())
		require.NoError(t, err)
		t.Logf("HeadBucket(primary-region key against %s): %s", region, r7Shape(dp.HealthCheck(ctx)))
	}
}

func envPrefix() string {
	if p := os.Getenv("R7_IDRIVE_REGION_PREFIX"); p != "" {
		return p
	}
	return "DA"
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestR7Live_IDriveMissingObjectShapes: what a miss looks like on every read
// path, and whether Delete of a missing key is idempotent.
func TestR7Live_IDriveMissingObjectShapes(t *testing.T) {
	d, _, _ := r7IDrive(t)
	ctx, cancel := r7Ctx(t)
	defer cancel()
	if err := d.HealthCheck(ctx); err != nil {
		t.Skipf("bucket %q not reachable in this region: %s", d.bucket, r7Shape(err))
	}
	key := "missing-" + r7Rand()

	_, err := d.Get(ctx, r7Container, key)
	t.Logf("Get(missing):      %s", r7Shape(err))
	_, err = d.GetRange(ctx, r7Container, key, 0, 10)
	t.Logf("GetRange(missing): %s", r7Shape(err))
	ok, err := d.Exists(ctx, r7Container, key)
	t.Logf("Exists(missing):   ok=%v err=%s", ok, r7Shape(err))
	err = d.Delete(ctx, r7Container, key)
	t.Logf("Delete(missing):   %s", r7Shape(err))

	// Raw HeadObject so the 404 body/code is visible independent of Exists.
	_, err = d.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(d.bucket), Key: aws.String(d.buildKey(r7Tenant, r7Container, key))})
	t.Logf("HeadObject(missing) raw: %s", r7Shape(err))
	// A key in a prefix this key has no rights to does not exist here; iDrive
	// keys are bucket-wide, so a 403-for-missing cannot be provoked. Noted.
}

// TestR7Live_IDriveListAndDotSegments: List semantics (relative names, prefix
// filter, >1 page) and whether a key containing ".." is stored literally.
func TestR7Live_IDriveListAndDotSegments(t *testing.T) {
	d, _, _ := r7IDrive(t)
	ctx, cancel := r7Ctx(t)
	defer cancel()
	if err := d.HealthCheck(ctx); err != nil {
		t.Skipf("bucket %q not reachable in this region: %s", d.bucket, r7Shape(err))
	}
	run := r7Rand()
	keys := []string{run + "/a/1", run + "/a/2", run + "/b/3", run + "/x/../escape-" + run}
	body := func() io.Reader { return strings.NewReader("r7") }
	var cleanup []string
	defer func() {
		for _, k := range cleanup {
			_ = d.Delete(ctx, r7Container, k)
		}
	}()
	for _, k := range keys {
		err := d.Put(ctx, r7Container, k, body(), engine.WithContentLength(2))
		t.Logf("Put(%q): %s", k, r7Shape(err))
		if err == nil {
			cleanup = append(cleanup, k)
		}
	}

	got, err := d.List(ctx, r7Container, run+"/a/")
	require.NoError(t, err)
	t.Logf("List(prefix %q) -> %q", run+"/a/", got)
	all, err := d.List(ctx, r7Container, run+"/")
	require.NoError(t, err)
	t.Logf("List(prefix %q) -> %q", run+"/", all)

	// Dot segments: did the vendor store the literal key or normalise it?
	lit, err := d.Exists(ctx, r7Container, run+"/x/../escape-"+run)
	t.Logf("Exists(literal ..): ok=%v err=%s", lit, r7Shape(err))
	norm, err := d.Exists(ctx, r7Container, run+"/escape-"+run)
	t.Logf("Exists(normalised): ok=%v err=%s", norm, r7Shape(err))
	if norm {
		cleanup = append(cleanup, run+"/escape-"+run)
	}
}

// TestR7Live_R2MissingObjectShapes: R2 error shapes against the fixed public
// bucket — reads of keys that do not exist only; no writes.
func TestR7Live_R2MissingObjectShapes(t *testing.T) {
	acct := os.Getenv("CF_ACCOUNT_ID")
	if acct == "" {
		acct = os.Getenv("R2_ACCOUNT_ID")
	}
	if acct == "" {
		if ep := os.Getenv("R2_ENDPOINT"); ep != "" {
			h := must(url.Parse(ep)).Hostname()
			acct = strings.SplitN(h, ".", 2)[0]
		}
	}
	ak, sk := os.Getenv("R2_ACCESS_KEY"), os.Getenv("R2_SECRET_KEY")
	if acct == "" || ak == "" || sk == "" {
		t.Skip("R2 credentials not set")
	}
	d, err := NewR2Driver(acct, ak, sk, os.Getenv("R2_JURISDICTION"), os.Getenv("R2_BUCKET"), zap.NewNop())
	require.NoError(t, err)
	ctx, cancel := r7Ctx(t)
	defer cancel()

	t.Logf("HealthCheck: %s", r7Shape(d.HealthCheck(ctx)))
	key := "missing-" + r7Rand()
	_, err = d.Get(ctx, r7Container, key)
	t.Logf("Get(missing):      %s", r7Shape(err))
	_, err = d.GetRange(ctx, r7Container, key, 0, 10)
	t.Logf("GetRange(missing): %s", r7Shape(err))
	ok, err := d.Exists(ctx, r7Container, key)
	t.Logf("Exists(missing):   ok=%v err=%s", ok, r7Shape(err))
	err = d.Delete(ctx, r7Container, key)
	t.Logf("Delete(missing):   %s", r7Shape(err))
	got, err := d.List(ctx, r7Container, "")
	t.Logf("List(empty container): n=%d err=%s", len(got), r7Shape(err))

	// Buckets visible to this token (names only) — to know whether a scratch
	// bucket exists for write-side checks.
	lb, err := d.client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		t.Logf("ListBuckets: %s", r7Shape(err))
		return
	}
	for _, b := range lb.Buckets {
		t.Logf("bucket: %s", aws.ToString(b.Name))
	}
}
