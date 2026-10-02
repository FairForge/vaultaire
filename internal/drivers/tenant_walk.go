package drivers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/FairForge/vaultaire/internal/engine"
)

// engine.TenantWalker for every driver that keys objects under the tenant
// (WP-R10-3c). One listing reaches everything a tenant holds on a backend,
// including containers no table remembers; each object comes with a Remove
// bound to the exact key the listing returned.
//
// Who implements it:
//
//	idrive (+ every idrive-<region>), lyve, geyser, r2   t-<tenant>/…  in one fixed bucket
//	s3compat, quotaless                                  <root>/<tenant>_<bucket>/…
//	local                                                <base>/<tenant>_<bucket>/…
//
// Who cannot: the plain `s3` driver (a container is a real bucket — nothing to
// list by prefix) and permafrost (a folder tree on fifteen accounts; its List
// is one level deep). The erasure sweep falls back to List + Delete per
// bucket for those.

var (
	_ engine.TenantWalker = (*IDriveDriver)(nil)
	_ engine.TenantWalker = (*LyveDriver)(nil)
	_ engine.TenantWalker = (*GeyserDriver)(nil)
	_ engine.TenantWalker = (*R2Driver)(nil)
	_ engine.TenantWalker = (*S3CompatDriver)(nil)
	_ engine.TenantWalker = (*QuotalessDriver)(nil)
	_ engine.TenantWalker = (*LocalDriver)(nil)
)

// tenantWalkPageTimeout bounds one listing page: a backend that stops
// answering fails the walk instead of holding the job.
const tenantWalkPageTimeout = 60 * time.Second

// ErrWalkTenantID is returned for a tenant id a walk must never run with.
var ErrWalkTenantID = errors.New("tenant walk refused: empty tenant id or one containing '/'")

// checkWalkTenant refuses the ids that would widen a listing: "" collapses
// the prefix to every tenant's (`t-/`, or `_` on the container-only drivers),
// and a '/' makes the "tenant" a path inside another tenant's prefix.
func checkWalkTenant(tenantID string) error {
	if tenantID == "" || strings.Contains(tenantID, "/") {
		return ErrWalkTenantID
	}
	return nil
}

// s3TenantWalkAPI is what a walk needs from an S3 client.
type s3TenantWalkAPI interface {
	s3.ListObjectsV2APIClient
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// s3WalkPrefix lists every key of `bucket` under `prefix` and hands each to
// fn. The part of the key after the prefix is `<rest of container>/<artifact>`;
// containerLead is put back in front of the container (the container-only
// drivers list by `<root>/<tenant>_`, so the lead is `<tenant>_`).
//
// It runs its own page loop instead of s3ListPaginator: that one STOPS on a
// repeated continuation token, which is right for a customer listing and
// wrong here — a listing cut short would be reported as a finished sweep.
func s3WalkPrefix(ctx context.Context, client s3TenantWalkAPI, bucket, prefix, containerLead string, fn func(engine.TenantObject) error) error {
	var token *string
	for {
		pctx, cancel := context.WithTimeout(ctx, tenantWalkPageTimeout)
		page, err := client.ListObjectsV2(pctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		cancel()
		if err != nil {
			return fmt.Errorf("walk %s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			rest, ok := strings.CutPrefix(key, prefix)
			if !ok {
				return fmt.Errorf("walk %s: the backend returned key %q from outside the prefix", prefix, key)
			}
			container, artifact, hasSep := strings.Cut(rest, "/")
			if !hasSep {
				// No container segment: nothing this code writes. Hand it
				// over unaddressable; Remove still deletes the listed key.
				container, artifact = "", rest
			} else {
				container = containerLead + container
			}
			err := fn(engine.TenantObject{
				Container: container,
				Artifact:  artifact,
				Remove: func(ctx context.Context) error {
					if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
						Bucket: aws.String(bucket),
						Key:    aws.String(key),
					}); err != nil && !s3IsNotFound(err) {
						return fmt.Errorf("delete %s: %w", key, err)
					}
					return nil
				},
			})
			if err != nil {
				return err
			}
		}
		if !aws.ToBool(page.IsTruncated) {
			return nil
		}
		next := aws.ToString(page.NextContinuationToken)
		if next == "" || (token != nil && next == *token) {
			return fmt.Errorf("walk %s: the listing is truncated and its continuation token did not advance", prefix)
		}
		token = aws.String(next)
	}
}

// tenantKeyPrefix is the prefix of everything a tenant holds in a
// fixed-bucket backend. The trailing slash is the boundary: `t-ab/` is not a
// prefix of `t-abc/…`.
func tenantKeyPrefix(tenantID string) string { return "t-" + tenantID + "/" }

// WalkTenant implements engine.TenantWalker.
func (d *IDriveDriver) WalkTenant(ctx context.Context, tenantID string, fn func(engine.TenantObject) error) error {
	if err := checkWalkTenant(tenantID); err != nil {
		return err
	}
	if err := s3WalkPrefix(ctx, d.client, d.bucket, tenantKeyPrefix(tenantID), "", fn); err != nil {
		return fmt.Errorf("idrive: %w", err)
	}
	return nil
}

// WalkTenant implements engine.TenantWalker.
func (d *LyveDriver) WalkTenant(ctx context.Context, tenantID string, fn func(engine.TenantObject) error) error {
	if err := checkWalkTenant(tenantID); err != nil {
		return err
	}
	if err := s3WalkPrefix(ctx, d.client, d.getBucket(), tenantKeyPrefix(tenantID), "", fn); err != nil {
		return fmt.Errorf("lyve: %w", err)
	}
	return nil
}

// WalkTenant implements engine.TenantWalker. A delete of an object that has
// gone to tape is an ordinary DeleteObject (geyser_README: "Safe on archived
// objects: DELETE"); the bucket's versioning is suspended, so it leaves no
// marker behind.
func (d *GeyserDriver) WalkTenant(ctx context.Context, tenantID string, fn func(engine.TenantObject) error) error {
	if err := checkWalkTenant(tenantID); err != nil {
		return err
	}
	if err := s3WalkPrefix(ctx, d.client, d.bucket, tenantKeyPrefix(tenantID), "", fn); err != nil {
		return fmt.Errorf("geyser: %w", err)
	}
	return nil
}

// WalkTenant implements engine.TenantWalker.
func (d *R2Driver) WalkTenant(ctx context.Context, tenantID string, fn func(engine.TenantObject) error) error {
	if err := checkWalkTenant(tenantID); err != nil {
		return err
	}
	if err := s3WalkPrefix(ctx, d.client, d.bucket, tenantKeyPrefix(tenantID), "", fn); err != nil {
		return fmt.Errorf("r2: %w", err)
	}
	return nil
}

// WalkTenant implements engine.TenantWalker. Keys are
// `<root>/<tenant>_<bucket>/<artifact>`: the `_` after the tenant id is the
// boundary (`tenant-ab_` is not a prefix of `tenant-abc_…`).
func (d *S3CompatDriver) WalkTenant(ctx context.Context, tenantID string, fn func(engine.TenantObject) error) error {
	if err := checkWalkTenant(tenantID); err != nil {
		return err
	}
	lead := tenantID + "_"
	if err := s3WalkPrefix(ctx, d.client, d.bucket, d.prefix+"/"+lead, lead, fn); err != nil {
		return fmt.Errorf("s3compat: %w", err)
	}
	return nil
}

// WalkTenant implements engine.TenantWalker (same key shape as s3compat, in
// the fixed `data` bucket under rootPath).
func (d *QuotalessDriver) WalkTenant(ctx context.Context, tenantID string, fn func(engine.TenantObject) error) error {
	if err := checkWalkTenant(tenantID); err != nil {
		return err
	}
	lead := tenantID + "_"
	if err := s3WalkPrefix(ctx, d.client, "data", d.rootPath+"/"+lead, lead, fn); err != nil {
		return fmt.Errorf("quotaless: %w", err)
	}
	return nil
}

// WalkTenant implements engine.TenantWalker: every file under every
// `<base>/<tenant>_*` directory. Unlike List it reports walk errors (a
// directory it cannot read is bytes it cannot vouch for) and it includes the
// files List hides — AtomicWrite temp files and legacy `.meta` sidecars are
// bytes of the tenant too. Directories are left; they hold nothing.
func (d *LocalDriver) WalkTenant(ctx context.Context, tenantID string, fn func(engine.TenantObject) error) error {
	if err := checkWalkTenant(tenantID); err != nil {
		return err
	}
	base := filepath.Clean(d.basePath)
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("local: walk %s: %w", tenantID, err)
	}
	lead := tenantID + "_"
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), lead) {
			continue
		}
		container := e.Name()
		root := filepath.Join(base, container)
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil // removed while walking
				}
				return err
			}
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			if info.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			return fn(engine.TenantObject{
				Container: container,
				Artifact:  filepath.ToSlash(rel),
				Remove: func(context.Context) error {
					if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
						return fmt.Errorf("local: delete %s/%s: %w", container, filepath.ToSlash(rel), err)
					}
					return nil
				},
			})
		})
		if err != nil {
			return fmt.Errorf("local: walk %s: %w", container, err)
		}
	}
	return nil
}
