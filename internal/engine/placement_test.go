package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/common"
)

// memDriver keeps objects in memory and records deletes; a missing
// object is reported the way the local driver reports it.
type memDriver struct {
	name    string
	mu      sync.Mutex
	data    map[string][]byte
	deleted []string
	putErr  error
}

func newMemDriver(name string) *memDriver {
	return &memDriver{name: name, data: map[string][]byte{}}
}

func (d *memDriver) Name() string { return d.name }
func (d *memDriver) Get(_ context.Context, c, a string) (io.ReadCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.data[c+"/"+a]
	if !ok {
		return nil, NotFoundError{Container: c, Artifact: a}
	}
	return io.NopCloser(strings.NewReader(string(b))), nil
}
func (d *memDriver) Put(_ context.Context, c, a string, r io.Reader, _ ...PutOption) error {
	if d.putErr != nil {
		return d.putErr
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data[c+"/"+a] = b
	return nil
}
func (d *memDriver) Delete(_ context.Context, c, a string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deleted = append(d.deleted, c+"/"+a)
	if _, ok := d.data[c+"/"+a]; !ok {
		return NotFoundError{Container: c, Artifact: a}
	}
	delete(d.data, c+"/"+a)
	return nil
}
func (d *memDriver) List(context.Context, string, string) ([]string, error) { return nil, nil }
func (d *memDriver) Exists(_ context.Context, c, a string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.data[c+"/"+a]
	return ok, nil
}
func (d *memDriver) HealthCheck(context.Context) error { return nil }

func (d *memDriver) has(c, a string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.data[c+"/"+a]
	return ok
}

// R6-03: an `auto` bucket PUT carries no storage class. The engine used to
// ask an access tracker for a recommendation and APPLY it, relocating the
// second to fifth PUT of any key in a default bucket to the resilient tier's
// backend. The tracker was deleted in R15 (WP-R6-5); this pins the rule that
// placement is the API layer's decision and an overwrite stays on the primary.
func TestEnginePut_AutoBucketOverwriteStaysOnPrimary(t *testing.T) {
	const container, key = "r6-placement-tenant_bucket", "overwrite.bin"

	eng := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "idrive"})
	idrive := newMemDriver("idrive")
	lyve := newMemDriver("lyve")
	eng.AddDriver("idrive", idrive)
	eng.AddDriver("lyve", lyve)
	eng.SetPrimary("idrive")

	ctx := common.WithTenantID(context.Background(), "r6-placement-tenant")
	for _, body := range []string{"v1", "v2", "v3", "v4", "v5"} {
		backend, err := eng.Put(ctx, container, key, strings.NewReader(body))
		require.NoError(t, err)
		assert.Equal(t, "idrive", backend, "an auto-bucket overwrite must stay on the primary")
	}
	assert.True(t, idrive.has(container, key))
	assert.False(t, lyve.has(container, key), "nothing but the resolved class may place customer data")
}

func TestBuildWriteCandidateList_TargetOnlyBackends(t *testing.T) {
	eng := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "idrive"})
	for _, n := range []string{"idrive", "lyve", "s3", "r2", "geyser", "permafrost", "sync", "idrive-eu-west-1", "local"} {
		eng.AddDriver(n, newMemDriver(n))
	}
	eng.SetPrimary("idrive")

	t.Run("STANDARD write never falls over to a target-only backend", func(t *testing.T) {
		c := eng.buildWriteCandidateList(eng.view(), "idrive")
		assert.Equal(t, "idrive", c[0])
		assert.ElementsMatch(t, []string{"idrive", "lyve", "s3"}, c)
	})
	t.Run("explicit target is honoured first, then general-purpose backends", func(t *testing.T) {
		for _, target := range []string{"r2", "geyser", "permafrost", "idrive-eu-west-1"} {
			c := eng.buildWriteCandidateList(eng.view(), target)
			assert.Equal(t, target, c[0], "target %s", target)
			assert.ElementsMatch(t, []string{target, "idrive", "lyve", "s3"}, c, "target %s", target)
		}
	})
	t.Run("a strict target is asked alone (Prompt 2b.4 E1.1)", func(t *testing.T) {
		assert.Equal(t, []string{"sync"}, eng.buildWriteCandidateList(eng.view(), "sync"))
		c, err := eng.readCandidates(eng.view(), "sync", "get")
		require.NoError(t, err)
		assert.Equal(t, []string{"sync"}, c)
		// WP-R6-1: every recorded backend is read alone — tape included (it
		// used to keep a read fall-over to every other backend).
		c, err = eng.readCandidates(eng.view(), "geyser", "get")
		require.NoError(t, err)
		assert.Equal(t, []string{"geyser"}, c)
	})
	t.Run("a target-only primary is still writable", func(t *testing.T) {
		g := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "geyser"})
		g.AddDriver("geyser", newMemDriver("geyser"))
		g.AddDriver("idrive", newMemDriver("idrive"))
		g.SetPrimary("geyser")
		assert.Contains(t, g.buildWriteCandidateList(g.view(), "idrive"), "geyser")
	})
}

func TestEnginePut_NeverFailsOverToTargetOnlyBackend(t *testing.T) {
	eng := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "idrive"})
	idrive := newMemDriver("idrive")
	idrive.putErr = fmt.Errorf("dial tcp: connection refused")
	r2, geyser, eu := newMemDriver("r2"), newMemDriver("geyser"), newMemDriver("idrive-eu-west-1")
	syncDrv := newMemDriver("sync")
	eng.AddDriver("sync", syncDrv)
	eng.AddDriver("idrive", idrive)
	eng.AddDriver("r2", r2)
	eng.AddDriver("geyser", geyser)
	eng.AddDriver("idrive-eu-west-1", eu)
	eng.SetPrimary("idrive")

	_, err := eng.Put(tenantCtx(), "t_b", "k", strings.NewReader("private bytes"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAllBackendsUnavailable), "must fail loudly, got: %v", err)
	assert.False(t, r2.has("t_b", "k"), "a private object must never land on the public R2 store")
	assert.False(t, geyser.has("t_b", "k"), "a STANDARD object must never land on tape")
	assert.False(t, eu.has("t_b", "k"), "a US object must never land in an EU region")
	assert.False(t, syncDrv.has("t_b", "k"), "customer data reaches Sync.com only for a flagged tenant's bucket (the ToS)")
	assert.EqualValues(t, 1, eng.WriteFailures())
}

// R6-05: Delete resolved the backend from the in-memory map only. After a
// restart the map was cold, the DELETE went to the primary, iDrive said 404,
// the API treated that as an idempotent miss and removed the head row — the
// bytes stayed on the real backend forever. Since WP-R6-1 the engine keeps
// no route at all: the caller names the head row's backend on the context,
// the same after a restart as before it.
func TestEngineDelete_UsesTheRecordedBackendAfterARestart(t *testing.T) {
	const tenantID, container, key = "r6-delete-tenant", "r6-delete-tenant_bucket", "on-lyve.bin"
	idrive, lyve := newMemDriver("idrive"), newMemDriver("lyve")
	ctx := common.WithTenantID(context.Background(), tenantID)

	before := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "idrive"})
	before.AddDriver("idrive", idrive)
	before.AddDriver("lyve", lyve)
	before.SetPrimary("idrive")
	backend, err := before.Put(ctx, container, key, strings.NewReader("resilient bytes"), WithStorageClass("RESILIENT"))
	require.NoError(t, err)
	require.Equal(t, "lyve", backend) // what the head row records

	// "Restart": a fresh engine over the same drivers.
	after := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "idrive"})
	after.AddDriver("idrive", idrive)
	after.AddDriver("lyve", lyve)
	after.SetPrimary("idrive")

	require.NoError(t, after.Delete(WithRecordedBackend(ctx, backend), container, key))
	assert.False(t, lyve.has(container, key), "the object must be deleted from the backend that holds it")
	assert.Contains(t, lyve.deleted, container+"/"+key)
}
