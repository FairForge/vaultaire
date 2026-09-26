package engine

import (
	"context"
	"database/sql"
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
	"github.com/FairForge/vaultaire/internal/testutil"
)

// openTestDB connects to the test database (vaultaire_test by default,
// DATABASE_URL overrides) or skips.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Skipf("test database unavailable (%v) — run `make test-db`", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

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

// R6-03: an `auto` bucket PUT carries no storage class. The engine used to ask
// the access tracker for a recommendation and APPLY it; with `temperature`
// never written (always 'cold') and access_count < 5, the tracker names
// "lyve" for every object that already has an access_patterns row — i.e. the
// second to fifth PUT of any key in a default bucket landed on the resilient
// tier's backend instead of the primary. Placement is the API layer's decision
// (resolvePutStorageClass); the engine must not re-derive it.
func TestEnginePut_AutoBucketOverwriteStaysOnPrimary(t *testing.T) {
	db := openTestDB(t)
	const tenantID, container, key = "r6-placement-tenant", "r6-placement-tenant_bucket", "overwrite.bin"
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM access_patterns WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM object_locations WHERE tenant_id = $1`, tenantID)
	})
	// What the tracker leaves behind after the first PUT was flushed.
	_, err := db.Exec(`
		INSERT INTO access_patterns (tenant_id, container, artifact_key, operation, access_count, temperature)
		VALUES ($1, $2, $3, 'PUT', 1, 'cold')
		ON CONFLICT (tenant_id, container, artifact_key) DO UPDATE SET access_count = 1, temperature = 'cold'`,
		tenantID, container, key)
	require.NoError(t, err)

	eng := NewEngine(db, nopLogger(), &Config{DefaultBackend: "idrive", EnableML: true})
	idrive := newMemDriver("idrive")
	lyve := newMemDriver("lyve")
	eng.AddDriver("idrive", idrive)
	eng.AddDriver("lyve", lyve)
	eng.SetPrimary("idrive")
	require.NotNil(t, eng.intelligence, "fixture must exercise the tracker path")

	ctx := common.WithTenantID(context.Background(), tenantID)
	backend, err := eng.Put(ctx, container, key, strings.NewReader("v2"))
	require.NoError(t, err)

	assert.Equal(t, "idrive", backend, "an auto-bucket overwrite must stay on the primary")
	assert.True(t, idrive.has(container, key))
	assert.False(t, lyve.has(container, key), "the access tracker must never place customer data")
}

// R6-04: r2 (public store), geyser (tape), permafrost (async second copy) and
// region-pinned idrive-<region> drivers are TARGET-ONLY: they receive a write
// when they are the resolved target or the configured primary, never as a
// silent failover destination for a STANDARD object.
func TestBuildWriteCandidateList_TargetOnlyBackends(t *testing.T) {
	eng := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "idrive"})
	for _, n := range []string{"idrive", "lyve", "s3", "r2", "geyser", "permafrost", "idrive-eu-west-1", "local"} {
		eng.AddDriver(n, newMemDriver(n))
	}
	eng.SetPrimary("idrive")

	t.Run("STANDARD write never falls over to a target-only backend", func(t *testing.T) {
		c := eng.buildWriteCandidateList("idrive")
		assert.Equal(t, "idrive", c[0])
		assert.ElementsMatch(t, []string{"idrive", "lyve", "s3"}, c)
	})
	t.Run("explicit target is honoured first, then general-purpose backends", func(t *testing.T) {
		for _, target := range []string{"r2", "geyser", "permafrost", "idrive-eu-west-1"} {
			c := eng.buildWriteCandidateList(target)
			assert.Equal(t, target, c[0], "target %s", target)
			assert.ElementsMatch(t, []string{target, "idrive", "lyve", "s3"}, c, "target %s", target)
		}
	})
	t.Run("a target-only primary is still writable", func(t *testing.T) {
		g := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "geyser"})
		g.AddDriver("geyser", newMemDriver("geyser"))
		g.AddDriver("idrive", newMemDriver("idrive"))
		g.SetPrimary("geyser")
		assert.Contains(t, g.buildWriteCandidateList("idrive"), "geyser")
	})
}

func TestEnginePut_NeverFailsOverToTargetOnlyBackend(t *testing.T) {
	eng := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "idrive"})
	idrive := newMemDriver("idrive")
	idrive.putErr = fmt.Errorf("dial tcp: connection refused")
	r2, geyser, eu := newMemDriver("r2"), newMemDriver("geyser"), newMemDriver("idrive-eu-west-1")
	eng.AddDriver("idrive", idrive)
	eng.AddDriver("r2", r2)
	eng.AddDriver("geyser", geyser)
	eng.AddDriver("idrive-eu-west-1", eu)
	eng.SetPrimary("idrive")

	_, err := eng.Put(context.Background(), "t_b", "k", strings.NewReader("private bytes"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAllBackendsUnavailable), "must fail loudly, got: %v", err)
	assert.False(t, r2.has("t_b", "k"), "a private object must never land on the public R2 store")
	assert.False(t, geyser.has("t_b", "k"), "a STANDARD object must never land on tape")
	assert.False(t, eu.has("t_b", "k"), "a US object must never land in an EU region")
	assert.EqualValues(t, 1, eng.WriteFailures())
}

// R6-05: Delete resolved the backend from the in-memory map only. After a
// restart the map is cold, the DELETE went to the primary, iDrive said 404,
// the API treated that as an idempotent miss and removed the head row — the
// bytes stayed on the real backend forever. Delete must consult the recorded
// location like Get does.
func TestEngineDelete_UsesRecordedLocationWhenMapIsCold(t *testing.T) {
	db := openTestDB(t)
	const tenantID, container, key = "r6-delete-tenant", "r6-delete-tenant_bucket", "on-lyve.bin"
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM object_locations WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM access_patterns WHERE tenant_id = $1`, tenantID)
	})
	idrive, lyve := newMemDriver("idrive"), newMemDriver("lyve")
	ctx := common.WithTenantID(context.Background(), tenantID)

	before := NewEngine(db, nopLogger(), &Config{DefaultBackend: "idrive"})
	before.AddDriver("idrive", idrive)
	before.AddDriver("lyve", lyve)
	before.SetPrimary("idrive")
	backend, err := before.Put(ctx, container, key, strings.NewReader("resilient bytes"), WithStorageClass("RESILIENT"))
	require.NoError(t, err)
	require.Equal(t, "lyve", backend)
	// Put records the location fire-and-forget; make it durable for the test.
	require.NoError(t, before.locations.RecordLocation(ctx, tenantID, container, key, "lyve", "RESILIENT", 15))

	// "Restart": a fresh engine over the same drivers has an empty routing map.
	after := NewEngine(db, nopLogger(), &Config{DefaultBackend: "idrive"})
	after.AddDriver("idrive", idrive)
	after.AddDriver("lyve", lyve)
	after.SetPrimary("idrive")

	require.NoError(t, after.Delete(ctx, container, key))
	assert.False(t, lyve.has(container, key), "the object must be deleted from the backend that holds it")
	assert.Contains(t, lyve.deleted, container+"/"+key)
}
