package drivers

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/FairForge/vaultaire/internal/engine"
)

// engine.PageLister for the S3-class drivers that hold chunk blobs (WP-R8-2
// B.2: the chunk orphan sweep walks `_global/_chunks/` a page at a time,
// resuming from the last key of its previous run).
var (
	_ engine.PageLister = (*IDriveDriver)(nil)
	_ engine.PageLister = (*LyveDriver)(nil)
)

// maxListPage is S3's ListObjectsV2 page ceiling.
const maxListPage = 1000

// s3ListPage is one ListObjectsV2 page of keys under keyPrefix (the
// driver's `t-<tenant>/<container>/` + the caller's prefix), after
// startAfter (an artifact name, relative to base = `t-<tenant>/<container>/`).
// Keys come back relative to base. One page is bounded like a tenant walk's
// (tenantWalkPageTimeout).
func s3ListPage(ctx context.Context, client s3.ListObjectsV2APIClient, bucket, base, prefix, startAfter string, limit int) ([]engine.ListedObject, bool, error) {
	if limit <= 0 || limit > maxListPage {
		limit = maxListPage
	}
	in := &s3.ListObjectsV2Input{
		Bucket:  aws.String(bucket),
		Prefix:  aws.String(base + prefix),
		MaxKeys: aws.Int32(int32(limit)), // #nosec G115 -- bounded by maxListPage above
	}
	if startAfter != "" {
		in.StartAfter = aws.String(base + startAfter)
	}
	pctx, cancel := context.WithTimeout(ctx, tenantWalkPageTimeout)
	defer cancel()
	page, err := client.ListObjectsV2(pctx, in)
	if err != nil {
		return nil, false, fmt.Errorf("list page %s after %q: %w", base+prefix, startAfter, err)
	}
	out := make([]engine.ListedObject, 0, len(page.Contents))
	for _, obj := range page.Contents {
		key := aws.ToString(obj.Key)
		rel, ok := strings.CutPrefix(key, base)
		if !ok {
			return nil, false, fmt.Errorf("list page %s: the backend returned key %q from outside the prefix", base+prefix, key)
		}
		out = append(out, engine.ListedObject{Artifact: rel, Size: aws.ToInt64(obj.Size), LastModified: aws.ToTime(obj.LastModified)})
	}
	return out, aws.ToBool(page.IsTruncated), nil
}

// ListPage implements engine.PageLister.
func (d *IDriveDriver) ListPage(ctx context.Context, container, prefix, startAfter string, limit int) ([]engine.ListedObject, bool, error) {
	tenantID, err := d.getTenantID(ctx, "ListPage")
	if err != nil {
		return nil, false, err
	}
	objs, more, err := s3ListPage(ctx, d.client, d.bucket, d.buildKey(tenantID, container, ""), prefix, startAfter, limit)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", d.name, err)
	}
	return objs, more, nil
}

// ListPage implements engine.PageLister.
func (d *LyveDriver) ListPage(ctx context.Context, container, prefix, startAfter string, limit int) ([]engine.ListedObject, bool, error) {
	tenantID, err := d.getTenantID(ctx, "ListPage")
	if err != nil {
		return nil, false, err
	}
	objs, more, err := s3ListPage(ctx, d.client, d.getBucket(), d.buildTenantKey(tenantID, container, ""), prefix, startAfter, limit)
	if err != nil {
		return nil, false, fmt.Errorf("lyve: %w", err)
	}
	return objs, more, nil
}
