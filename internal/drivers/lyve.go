// internal/drivers/lyve.go
package drivers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

type LyveDriver struct {
	client   *s3.Client
	tenantID string // Default tenant, can be overridden by context
	region   string
	logger   *zap.Logger
	// putTimeout is the per-PUT deadline per 64 MiB, with a 10 s floor for a
	// smaller body (s3PutDeadline; FIXED_BUCKET_PUT_TIMEOUT,
	// 0 = off); putClient wraps client with it and the one retry on a fresh
	// connection (put_deadline.go, 2026-10-17: the Lyve primary's PUT
	// non-response).
	putTimeout time.Duration
	putClient  manager.UploadAPIClient
	// hedge sends a HEAD, a DELETE or a listing page again on a fresh
	// connection when it has not answered after DRIVER_HEDGE_AFTER
	// (hedge.go, WP-R6-3); nil = none.
	hedge *s3Hedger
	// lockEnabled: the bucket has Object Lock or a default retention
	// (CheckBucketLock, WP-R7-7) — the driver is then target-only.
	lockEnabled atomic.Bool
}

// setPutTimeout installs the per-PUT deadline (0 = off).
func (d *LyveDriver) setPutTimeout(base time.Duration) {
	d.putTimeout = base
	pc := &deadlinePutClient{Client: d.client, driver: "lyve", region: d.region, base: base,
		fresh: s3FreshHTTPClient(), logger: d.logger}
	d.putClient = pc
	if d.hedge != nil {
		d.hedge.fresh = pc.fresh
	}
}

// uploadClient is what Put uploads through: the deadline client once set.
func (d *LyveDriver) uploadClient() manager.UploadAPIClient {
	if d.putClient != nil {
		return d.putClient
	}
	return d.client
}

// NewLyveDriver builds a driver for one Lyve Cloud 2 region.
//
// Lyve Cloud 2 gotcha (root-caused 2026-07-28): every bucket is homed in the
// region(s) of its replication policy, and ANY regional endpoint transparently
// proxies requests to the bucket's home region (~500ms/op cross-US penalty).
// The stored-<region> bucket this driver targets must therefore be created
// through that same region's endpoint (default policy homes a bucket in the
// creating region). Verify homing with a GET carrying the
// `x-rstor-replication-status: true` header.
func NewLyveDriver(accessKey, secretKey, tenantID, region string, logger *zap.Logger) (*LyveDriver, error) {
	if region == "" {
		// Closest region to the SLC prod box; us-east-1 here once routed
		// every op cross-country and read as a backend degradation.
		region = "us-west-1"
	}

	endpoint := fmt.Sprintf("https://s3.%s.global.lyve.seagate.com", region)

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		),
		config.WithRegion(region),
		// HTTP/1.1 pinned (2026-08-05 ALPN audit): every Lyve gateway
		// currently REFUSES h2 at ALPN, so this changes nothing today — it
		// guards against a vendor-side gateway upgrade silently flipping us
		// onto h2, where Go multiplexes all concurrent transfers onto one
		// TCP connection (measured 9× ingest collapse on Geyser, the one
		// gateway that does speak h2). Response headers within 60 s like the
		// fixed-bucket drivers (Prompt 2b.5 F3.4: the 2b.4 bound missed
		// lyve, prod's primary since 2026-10-11 — a hung GET lasted until
		// the caller's deadline). Since WP-R6-3 the bounds are per request
		// class (s3OpTimeouts: 15 s metadata, 25 s a GET's first byte).
		config.WithHTTPClient(s3ObjectHTTPClient()),
		config.WithRetryMaxAttempts(requestRetryMaxAttempts),
	)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	d := &LyveDriver{
		client:   s3.NewFromConfig(cfg, func(o *s3.Options) { o.BaseEndpoint = aws.String(endpoint); o.UsePathStyle = true }),
		tenantID: tenantID,
		region:   region,
		logger:   logger,
	}
	// The per-PUT deadline and the one retry on a fresh connection, as the
	// fixed-bucket drivers have had since WP-VAULT-1 (put_deadline.go;
	// FIXED_BUCKET_PUT_TIMEOUT covers lyve too since 2026-10-17: us-east-1
	// answered nothing to ~3 % of 1 MiB PUTs after acknowledging the body).
	driverPutRetries.WithLabelValues("lyve") // the series exists at 0
	d.setPutTimeout(fixedBucketPutTimeoutFromEnv(logger))
	d.hedge = newS3Hedger("lyve", driverHedgeAfterFromEnv(logger), d.putClient.(*deadlinePutClient).fresh)
	return d, nil
}

// getTenantID is the tenant the call is made for: the context's, else the
// driver's own default (tools pass one; the server passes none). With
// neither the call is refused (tenant_ctx.go) — it used to resolve to
// "default".
func (d *LyveDriver) getTenantID(ctx context.Context, op string) (string, error) {
	return requireTenant(ctx, d.Name(), op, d.tenantID, d.logger)
}

// ObjectKey is the key a call would address (engine.KeyAddresser).
func (d *LyveDriver) ObjectKey(ctx context.Context, container, artifact string) string {
	tid := contextTenant(ctx)
	if tid == "" {
		tid = d.tenantID
	}
	return d.buildTenantKey(tid, container, artifact)
}

func (d *LyveDriver) buildTenantKey(tenantID, container, artifact string) string {
	return fmt.Sprintf("t-%s/%s/%s", tenantID, container, artifact)
}

func (d *LyveDriver) getBucket() string {
	return fmt.Sprintf("stored-%s", d.region)
}

func (d *LyveDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	tenantID, tErr := d.getTenantID(ctx, "Get")
	if tErr != nil {
		return nil, tErr
	}
	key := d.buildTenantKey(tenantID, container, artifact)
	bucket := d.getBucket()

	result, err := d.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, sdkCallOptions(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("get object: %w", err)
	}

	return result.Body, nil
}

// GetRange reads a byte range directly from Lyve. Implements engine.RangeGetter.
func (d *LyveDriver) GetRange(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, error) {
	tenantID, tErr := d.getTenantID(ctx, "GetRange")
	if tErr != nil {
		return nil, tErr
	}
	key := d.buildTenantKey(tenantID, container, artifact)
	bucket := d.getBucket()

	input := &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}
	if offset > 0 || length > 0 {
		rangeHeader := fmt.Sprintf("bytes=%d-", offset)
		if length > 0 {
			rangeHeader = fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
		}
		input.Range = aws.String(rangeHeader)
	}

	result, err := d.client.GetObject(ctx, input, sdkCallOptions(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("get range: %w", err)
	}
	return result.Body, nil
}

// GetRangeInfo reads bytes [offset, offset+length) and reports the object's
// ETag and total size from the response (engine.VersionedRangeGetter): the
// parallel ranged GET (flag `parallel_get`) checks every range against the
// others. Until the 2026-10-12 bench only the fixed-bucket driver had it,
// so on the Lyve primary every multipart-uploaded object read back as one
// stream (38–70 MB/s against 124 for a chunked one).
func (d *LyveDriver) GetRangeInfo(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, engine.RangeInfo, error) {
	tenantID, tErr := d.getTenantID(ctx, "GetRangeInfo")
	if tErr != nil {
		return nil, engine.RangeInfo{}, tErr
	}
	if length <= 0 {
		return nil, engine.RangeInfo{}, fmt.Errorf("lyve get range info %s/%s: %w: length %d", container, artifact, engine.ErrInvalidInput, length)
	}
	key := d.buildTenantKey(tenantID, container, artifact)
	result, err := d.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.getBucket()),
		Key:    aws.String(key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)),
	}, sdkCallOptions(ctx)...)
	if err != nil {
		return nil, engine.RangeInfo{}, fmt.Errorf("lyve get range %s/%s: %w", container, artifact, err)
	}
	info := engine.RangeInfo{ETag: strings.Trim(aws.ToString(result.ETag), `"`), Size: -1}
	if cr := aws.ToString(result.ContentRange); cr != "" {
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			if n, perr := strconv.ParseInt(cr[i+1:], 10, 64); perr == nil {
				info.Size = n
			}
		}
	}
	return result.Body, info, nil
}

func (d *LyveDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	tenantID, tErr := d.getTenantID(ctx, "Put")
	if tErr != nil {
		return tErr
	}
	key := d.buildTenantKey(tenantID, container, artifact)
	bucket := d.getBucket()

	// Apply options
	options := &engine.PutOptions{}
	for _, opt := range opts {
		opt(options)
	}

	input := &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   data,
	}
	// Pass the known size through: objects that fit in one part then skip the
	// three-round-trip multipart path (see s3upload.go).
	if options.ContentLength > 0 {
		input.ContentLength = aws.Int64(options.ContentLength)
	}

	// Apply metadata from options
	if options.ContentType != "" {
		input.ContentType = aws.String(options.ContentType)
	}
	if options.CacheControl != "" {
		input.CacheControl = aws.String(options.CacheControl)
	}
	if options.ContentEncoding != "" {
		input.ContentEncoding = aws.String(options.ContentEncoding)
	}
	if options.ContentLanguage != "" {
		input.ContentLanguage = aws.String(options.ContentLanguage)
	}
	if len(options.UserMetadata) > 0 {
		input.Metadata = options.UserMetadata
	}

	// Parallel multipart for large objects, single PutObject for small ones —
	// same path as iDrive/s3compat. Lyve requires multipart uploads to be
	// completed in the DC where they were initiated; that holds here because
	// the driver only ever talks to one regional endpoint. Through the
	// deadline client: a PutObject or UploadPart with no answer by its
	// deadline is sent once more on a fresh connection (put_deadline.go).
	if err := s3ParallelUploadInput(ctx, d.uploadClient(), input); err != nil {
		return fmt.Errorf("put object: %w", err)
	}

	return nil
}

func (d *LyveDriver) Delete(ctx context.Context, container, artifact string) error {
	tenantID, tErr := d.getTenantID(ctx, "Delete")
	if tErr != nil {
		return tErr
	}
	key := d.buildTenantKey(tenantID, container, artifact)
	bucket := d.getBucket()

	in := &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	_, err := hedged(ctx, d.hedge, "delete", func(ctx context.Context, o ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
		return d.client.DeleteObject(ctx, in, o...)
	})
	if err != nil {
		return fmt.Errorf("delete object: %w", err)
	}

	return nil
}

func (d *LyveDriver) List(ctx context.Context, container string, prefix string) ([]string, error) {
	tenantID, tErr := d.getTenantID(ctx, "List")
	if tErr != nil {
		return nil, tErr
	}
	keyPrefix := fmt.Sprintf("t-%s/%s/", tenantID, container)
	if prefix != "" {
		keyPrefix = fmt.Sprintf("t-%s/%s/%s", tenantID, container, prefix)
	}
	bucket := d.getBucket()

	// Strip the tenant/container prefix when returning keys
	// Use the tenantID we actually used, not d.tenantID
	tenantPrefix := fmt.Sprintf("t-%s/%s/", tenantID, container)
	var keys []string
	err := listPages(ctx, d.hedge, d.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(keyPrefix),
	}, func(page *s3.ListObjectsV2Output) error {
		for _, obj := range page.Contents {
			if obj.Key != nil {
				// Remove the tenant/container prefix to return just the artifact name
				keys = append(keys, strings.TrimPrefix(*obj.Key, tenantPrefix))
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list objects: %w", err)
	}
	return keys, nil
}

func (d *LyveDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	tenantID, tErr := d.getTenantID(ctx, "Exists")
	if tErr != nil {
		return false, tErr
	}
	key := d.buildTenantKey(tenantID, container, artifact)
	bucket := d.getBucket()

	in := &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	_, err := hedged(ctx, d.hedge, "head_object", func(ctx context.Context, o ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
		return d.client.HeadObject(ctx, in, o...)
	})
	if err != nil {
		if s3IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("lyve exists: %w", err)
	}
	return true, nil
}

func (d *LyveDriver) Name() string {
	return "lyve"
}

// HealthCheck is the probe: a signed HeadBucket, hedged (hedge.go).
func (d *LyveDriver) HealthCheck(ctx context.Context) error {
	bucket := d.getBucket()
	in := &s3.HeadBucketInput{Bucket: aws.String(bucket)}
	_, err := hedged(ctx, d.hedge, "head_bucket", func(ctx context.Context, o ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
		return d.client.HeadBucket(ctx, in, o...)
	})
	if err != nil {
		return fmt.Errorf("lyve health check (%s): %w", bucket, err)
	}
	return nil
}

// The lock guard (WP-R7-7, R7-16). Lyve is the only general-purpose
// fail-over leg: a fail-over PUT lands in stored-<region> under the
// tenant's prefix. With Object Lock — or a default retention — on that
// bucket every such write would become undeletable until the retention
// lapsed: the SDK answers the PUT 200 and the later DeleteObject
// AccessDenied (a breaker charge), and an erasure request cannot be
// honoured. Both prod buckets have no lock and versioning off (R7-16, and
// ObjectLockConfigurationNotFoundError on 2026-10-17); this keeps it a
// decision rather than one console click.

// ErrLyveLockUnreadable: the key may not read the bucket's lock
// configuration (a scoped service user). The guard cannot decide; the boot
// says so at Warn.
var ErrLyveLockUnreadable = errors.New("lyve: the bucket's object-lock configuration cannot be read with this key")

var lyveBucketLockEnabled = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "vaultaire_lyve_bucket_lock_enabled",
	Help: "1 when the Lyve driver's stored-<region> bucket has Object Lock enabled or a default retention (read at boot): the driver is then target-only — never the primary, never a fail-over destination (WP-R7-7). No series when the key cannot read the configuration.",
}, []string{"region"})

// CheckBucketLock reads stored-<region>'s object-lock configuration once
// (the server calls it at boot): enabled — ObjectLockEnabled, or a default
// retention rule — makes the driver target-only (TargetOnly) and sets
// vaultaire_lyve_bucket_lock_enabled{region} 1. No configuration is no lock
// (0). A 403 is ErrLyveLockUnreadable; anything else an error — both leave
// the driver as it was.
func (d *LyveDriver) CheckBucketLock(ctx context.Context) (bool, error) {
	out, err := d.client.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String(d.getBucket())})
	if err != nil {
		var apiErr smithy.APIError
		switch {
		case errors.As(err, &apiErr) && apiErr.ErrorCode() == "ObjectLockConfigurationNotFoundError":
			d.lockEnabled.Store(false)
			lyveBucketLockEnabled.WithLabelValues(d.region).Set(0)
			return false, nil
		case s3IsForbidden(err):
			return false, fmt.Errorf("%w (%s): %w", ErrLyveLockUnreadable, d.getBucket(), err)
		}
		return false, fmt.Errorf("lyve object-lock configuration of %s: %w", d.getBucket(), err)
	}
	locked := false
	if c := out.ObjectLockConfiguration; c != nil {
		locked = c.ObjectLockEnabled == types.ObjectLockEnabledEnabled || (c.Rule != nil && c.Rule.DefaultRetention != nil)
	}
	d.lockEnabled.Store(locked)
	v := 0.0
	if locked {
		v = 1
	}
	lyveBucketLockEnabled.WithLabelValues(d.region).Set(v)
	return locked, nil
}

// TargetOnly is engine.TargetOnlyDeclarer: true once CheckBucketLock found
// the bucket locked.
func (d *LyveDriver) TargetOnly() bool { return d.lockEnabled.Load() }

// Region is the Lyve region the driver stores into.
func (d *LyveDriver) Region() string { return d.region }
