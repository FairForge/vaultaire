package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
)

// fixedBucketDriver keys the way prod's primary does (iDrive; also Lyve,
// Geyser, R2, permafrost): one store, every key
// `t-<tenant>/<container>/<artifact>`, the tenant taken from the CONTEXT of
// the call — and, like the real drivers since WP-R8-7, a call whose context
// names no tenant is refused (drivers.ErrNoTenant), not filed under
// "default". It implements engine.TenantWalker and engine.KeyAddresser and
// can be told to fail the way a backend does.
//
// Every test of anything that addresses a blob runs on this double, not on
// the local driver: local keys by container only, which is why no test saw
// that a chunk was stored under its uploader's prefix (WP-R8-7).
type fixedBucketDriver struct {
	dir string

	failGet       atomic.Bool  // reads fail (what opens the engine's breaker)
	failWalkAfter atomic.Int32 // > 0: the listing fails after handing out this many objects
	failRemove    atomic.Bool  // every delete is refused
	hangRemove    atomic.Bool  // every delete waits for its context to give up
	walks         atomic.Int32 // WalkTenant calls
	removes       atomic.Int32 // deletes attempted
	failExists    atomic.Bool  // Exists fails (a backend that cannot be asked)
	failDelete    atomic.Bool  // Delete fails
	dropPut       atomic.Bool  // Put answers success and writes nothing
	noTenant      atomic.Int32 // calls refused because the context named no tenant
	onObject      func(n int)  // called before object n of a walk is handed out
	// failGetUnder: reads under this tenant's prefix fail (not a miss).
	failGetUnder atomic.Value
}

func (d *fixedBucketDriver) tenant(ctx context.Context) (string, error) {
	if t, ok := ctx.Value(common.TenantIDKey).(string); ok && t != "" {
		return t, nil
	}
	d.noTenant.Add(1)
	return "", fmt.Errorf("fixed: %w", drivers.ErrNoTenant)
}

// ObjectKey implements engine.KeyAddresser.
func (d *fixedBucketDriver) ObjectKey(ctx context.Context, container, artifact string) string {
	t, _ := ctx.Value(common.TenantIDKey).(string)
	return "t-" + t + "/" + container + "/" + artifact
}

func (d *fixedBucketDriver) path(tenant, container, artifact string) string {
	return filepath.Join(d.dir, "t-"+tenant, container, filepath.FromSlash(artifact))
}

func (d *fixedBucketDriver) Name() string { return "fixed" }

func (d *fixedBucketDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	if d.failGet.Load() {
		return nil, errors.New("fixed: connection refused")
	}
	tn, err := d.tenant(ctx)
	if err != nil {
		return nil, err
	}
	if under, _ := d.failGetUnder.Load().(string); under != "" && under == tn {
		return nil, errors.New("fixed: 503 SlowDown")
	}
	f, err := os.Open(d.path(tn, container, artifact))
	if err != nil {
		return nil, engine.ErrNotFound(container, artifact)
	}
	return f, nil
}

func (d *fixedBucketDriver) Put(ctx context.Context, container, artifact string, data io.Reader, _ ...engine.PutOption) error {
	tn, err := d.tenant(ctx)
	if err != nil {
		return err
	}
	if d.dropPut.Load() {
		_, _ = io.Copy(io.Discard, data)
		return nil
	}
	p := d.path(tn, container, artifact)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	b, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// Delete answers like S3: removing a key that is not there is a success.
func (d *fixedBucketDriver) Delete(ctx context.Context, container, artifact string) error {
	tn, err := d.tenant(ctx)
	if err != nil {
		return err
	}
	if d.failDelete.Load() {
		return errors.New("fixed: 503 SlowDown")
	}
	if err := os.Remove(d.path(tn, container, artifact)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (d *fixedBucketDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	var out []string
	tn, err := d.tenant(ctx)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(d.dir, "t-"+tn, container)
	err = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if name := filepath.ToSlash(rel); strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
		return nil
	})
	return out, err
}

func (d *fixedBucketDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	tn, err := d.tenant(ctx)
	if err != nil {
		return false, err
	}
	if d.failExists.Load() {
		return false, errors.New("fixed: 503 SlowDown")
	}
	return fileExists(d.path(tn, container, artifact)), nil
}

// blobs lists every file in the store, as `t-<tenant>/<container>/<artifact>`.
func (d *fixedBucketDriver) blobs() []string {
	var out []string
	_ = filepath.Walk(d.dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(d.dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// s3Ctx is the context of an authenticated S3 request the way s3.go builds
// it: the tenant object the handlers read AND the tenant id the drivers key
// by (handleS3Request sets common.TenantIDKey, then tenant.WithTenant). A
// test that sets only the first exercises a context production never has.
func s3Ctx(ctx context.Context, tn *tenant.Tenant) context.Context {
	return tenant.WithTenant(common.WithTenantID(ctx, tn.ID), tn)
}

func (d *fixedBucketDriver) HealthCheck(context.Context) error { return nil }

func (d *fixedBucketDriver) WalkTenant(ctx context.Context, tenantID string, fn func(engine.TenantObject) error) error {
	d.walks.Add(1)
	if tenantID == "" || strings.Contains(tenantID, "/") {
		return drivers.ErrWalkTenantID
	}
	root := filepath.Join(d.dir, "t-"+tenantID)
	var files []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	for n, p := range files {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("fixed: walk: %w", err)
		}
		if limit := int(d.failWalkAfter.Load()); limit > 0 && n >= limit {
			return errors.New("fixed: walk: 500 InternalError on the next page")
		}
		if d.onObject != nil {
			d.onObject(n)
		}
		rel, _ := filepath.Rel(root, p)
		container, artifact, _ := strings.Cut(filepath.ToSlash(rel), "/")
		err := fn(engine.TenantObject{Container: container, Artifact: artifact, Remove: func(rctx context.Context) error {
			d.removes.Add(1)
			if d.hangRemove.Load() {
				<-rctx.Done()
				return rctx.Err()
			}
			if d.failRemove.Load() {
				return errors.New("fixed: 503 SlowDown")
			}
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}})
		if err != nil {
			return err
		}
	}
	return nil
}
