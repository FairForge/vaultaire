package api

// WP-ACCT-1 (Stage 1a Prompt 53, PR B): a tenant's residency
// (tenants.residency: eu | uk | us; NULL = anywhere) is where its bytes may
// be stored at rest. Set at creation, never changed (an admin may set it
// while every bucket complies), inherited by a child and never widened;
// enforced at bucket create, on the mirror leg and on the parity leg.

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
)

func TestResidency_BucketOutsideRefused(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	drivers.SetAvailableIDriveRegions("us-central-1", []string{"eu-west-1", "eu-west-3"})
	t.Cleanup(drivers.ResetAvailableIDriveRegions)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, f.user.TenantID) })
	_, err := f.db.Exec(`UPDATE tenants SET residency = 'eu' WHERE id = $1`, f.user.TenantID)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE tenant_quotas SET tier = 'standard' WHERE tenant_id = $1`, f.user.TenantID)
	require.NoError(t, err) // past the free tier's one bucket
	create := func(name, region string) (int, map[string]any) {
		body := map[string]any{"name": name}
		if region != "" {
			body["region"] = region
		}
		return f.call(http.MethodPost, "/api/v1/manage/buckets", f.jwt, body)
	}

	code, out := create("res-us-"+f.user.TenantID[7:15], "us-central-1")
	assert.Equal(t, http.StatusBadRequest, code, out)
	assert.Equal(t, "region_outside_residency", errCode(out))
	code, out = create("res-uk-"+f.user.TenantID[7:15], "eu-west-3")
	assert.Equal(t, http.StatusBadRequest, code, out)
	assert.Equal(t, "region_outside_residency", errCode(out), "London is the UK, not the EU")
	code, out = create("res-eu-"+f.user.TenantID[7:15], "eu-west-1")
	assert.Equal(t, http.StatusCreated, code, out)
	code, out = create("res-def-"+f.user.TenantID[7:15], "")
	require.Equal(t, http.StatusCreated, code, out)
	assert.Equal(t, "eu-west-1", out["region"], "the default region is the first inside the residency")

	// S3 CreateBucket answers the same refusal in its own words.
	req := httptest.NewRequest(http.MethodPut, "/res-s3-"+f.user.TenantID[7:15], bytes.NewBufferString(
		`<CreateBucketConfiguration><LocationConstraint>us-central-1</LocationConstraint></CreateBucketConfiguration>`))
	w := httptest.NewRecorder()
	f.s.CreateBucket(w, withTenantCtx(req, f.user.TenantID))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "InvalidLocationConstraint")

	// A tenant with no residency is unrestricted, as before.
	_, err = f.db.Exec(`UPDATE tenants SET residency = NULL WHERE id = $1`, f.user.TenantID)
	require.NoError(t, err)
	code, out = create("res-any-"+f.user.TenantID[7:15], "us-central-1")
	assert.Equal(t, http.StatusCreated, code, out)
}

// An admin sets a tenant's residency only while every bucket complies.
func TestResidency_AdminSetsItOnlyWhenEveryBucketComplies(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, f.user.TenantID) })
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, region) VALUES ($1, 'res-admin-us', 'us-central-1')`, f.user.TenantID)
	require.NoError(t, err)
	put := func(v any) (int, map[string]any) {
		return f.call(http.MethodPut, "/api/v1/admin/tenants/"+f.user.TenantID+"/residency", f.adminJWT, map[string]any{"residency": v})
	}
	code, out := put("eu")
	assert.Equal(t, http.StatusConflict, code, out)
	assert.Equal(t, "buckets_outside_residency", errCode(out))
	code, out = put("mars")
	assert.Equal(t, http.StatusBadRequest, code, out)
	code, out = put("us")
	assert.Equal(t, http.StatusOK, code, out)
	var got sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT residency FROM tenants WHERE id = $1`, f.user.TenantID).Scan(&got))
	assert.Equal(t, "us", got.String)
	code, out = f.call(http.MethodPut, "/api/v1/admin/tenants/"+f.user.TenantID+"/residency", f.jwt, map[string]any{"residency": "eu"})
	assert.Equal(t, http.StatusForbidden, code, "an operator's only: %v", out)
}

func TestResidency_ChildNarrowsNeverWidens(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	f.grant()
	child := func(body map[string]any) (int, map[string]any) {
		return f.call(http.MethodPost, "/api/v1/platform/tenants", f.jwt, body)
	}
	residencyOf := func(id string) string {
		var r sql.NullString
		require.NoError(t, f.db.QueryRow(`SELECT residency FROM tenants WHERE id = $1`, id).Scan(&r))
		return r.String
	}

	// A platform with no residency: a child may set one.
	code, out := child(map[string]any{"label": "eu customer", "residency": "eu"})
	require.Equal(t, http.StatusCreated, code, out)
	assert.Equal(t, "eu", residencyOf(out["id"].(string)))
	assert.Equal(t, "eu", out["residency"])
	code, out = child(map[string]any{"label": "bad", "residency": "mars"})
	assert.Equal(t, http.StatusBadRequest, code, out)
	assert.Equal(t, "invalid_residency", errCode(out))

	// A platform in the EU: its children are, and may not leave.
	_, err := f.db.Exec(`UPDATE tenants SET residency = 'eu' WHERE id = $1`, f.user.TenantID)
	require.NoError(t, err)
	code, out = child(map[string]any{"label": "inherits"})
	require.Equal(t, http.StatusCreated, code, out)
	assert.Equal(t, "eu", residencyOf(out["id"].(string)), "inherited")
	code, out = child(map[string]any{"label": "widens", "residency": "us"})
	assert.Equal(t, http.StatusBadRequest, code, out)
	assert.Equal(t, "residency_widens", errCode(out))
	code, out = child(map[string]any{"label": "an app", "kind": "app"})
	require.Equal(t, http.StatusCreated, code, out)
	app := out["id"].(string)
	cleanupDescendants(f)
	code, out = child(map[string]any{"label": "the app's user", "parent_id": app})
	require.Equal(t, http.StatusCreated, code, out)
	assert.Equal(t, "eu", residencyOf(out["id"].(string)), "depth 2 inherits too")
}

func TestResidency_MirrorAndParityLegsStayInside(t *testing.T) {
	// The mirror leg: a tenant's residency bounds it, a leg of no known
	// jurisdiction is outside every residency.
	f := setupEdgeFixture(t)
	t.Setenv("LYVE_REGION", "us-west-1")
	t.Setenv("IDRIVE_REGION", "us-central-1")
	for _, n := range []string{"lyve", "idrive-eu-central-1", "idrive"} {
		d := newEdgeFakeDriver()
		d.name, d.urlHost = n, n+".backend.invalid"
		f.srv.engine.AddDriver(n, d)
	}
	f.srv.mirrorLegs = []string{"lyve", "idrive-eu-central-1", "idrive"}
	assert.Equal(t, "lyve", f.srv.mirrorLegFor("idrive", bucketScope{}), "no residency: as before")
	assert.Equal(t, "lyve", f.srv.mirrorLegFor("idrive", bucketScope{Residency: "us"}))
	assert.Empty(t, f.srv.mirrorLegFor("idrive-eu-west-1", bucketScope{Region: "eu-west-1", Residency: "uk"}), "no UK leg: a single copy")
	assert.Equal(t, "idrive-eu-central-1", f.srv.mirrorLegFor("idrive-eu-west-1", bucketScope{Region: "eu-west-1", Residency: "eu"}))
	f.addMirrorLeg()
	f.srv.mirrorLegs = []string{"edgemirror", "lyve"}
	assert.Equal(t, "edgemirror", f.srv.mirrorLegFor("idrive", bucketScope{}), "a leg of no known region holds an unrestricted bucket's copy")
	assert.Equal(t, "lyve", f.srv.mirrorLegFor("idrive", bucketScope{Residency: "us"}), "but never a resident tenant's")

	// The scope carries the tenant's residency.
	f.exec(`UPDATE tenants SET residency = 'eu' WHERE id = $1`, f.tenantID)
	f.exec(`INSERT INTO buckets (tenant_id, name, visibility, region) VALUES ($1, 'res-mirror', 'private', 'eu-west-1')`, f.tenantID)
	assert.Equal(t, "eu", f.srv.bucketScopeOf(context.Background(), f.tenantID, "res-mirror").Residency)

	// The parity leg: the first of the order inside the residency; none = none.
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	for _, n := range []string{"sync", "permafrost", "lyve"} {
		eng.AddDriver(n, drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	}
	db, err := sql.Open("postgres", "") // never dialled
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	p := NewVaultParity(db, eng, nil, zap.NewNop())
	name, _, ok := p.LegFor("")
	require.True(t, ok)
	assert.Equal(t, "sync", name, "no residency: the order as before")
	name, _, ok = p.LegFor("us")
	require.True(t, ok)
	assert.Equal(t, "lyve", name, "sync and permafrost name no jurisdiction: outside every residency")
	_, _, ok = p.LegFor("eu")
	assert.False(t, ok, "no EU leg: no parity copy, never one outside")
}

// RP86-1 (Stage 1a Prompt 54 Part 0): the GDPR export is the account's whole
// metadata, so its system bucket `_exports` is created inside the tenant's
// residency like any bucket — the first enabled region inside it — and with
// no region inside the export fails with the note no_region_inside, never
// written outside.
func TestResidency_TheExportStaysInside(t *testing.T) {
	f := setupExportFixture(t)
	f.seed()
	drivers.SetAvailableIDriveRegions("us-central-1", []string{"eu-west-1"})
	t.Cleanup(drivers.ResetAvailableIDriveRegions)
	t.Setenv("IDRIVE_REGION", "us-central-1")
	f.eng.AddDriver("idrive-eu-west-1", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	f.exec(`UPDATE buckets SET region = 'eu-west-1' WHERE tenant_id = $1`, f.tenantID)
	f.exec(`UPDATE tenants SET residency = 'eu' WHERE id = $1`, f.tenantID)

	id, err := f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.NoError(t, err)
	f.run()
	st := f.status(id)
	require.Equal(t, "completed", st.Status, st.Error)
	var region, label, backend string
	require.NoError(t, f.db.QueryRow(`SELECT region, data_residency FROM buckets WHERE tenant_id = $1 AND name = $2`,
		f.tenantID, tenant.ExportsBucket).Scan(&region, &label))
	assert.Equal(t, "eu-west-1", region, "the system bucket is made inside the residency")
	assert.Equal(t, "eu", label)
	require.NoError(t, f.db.QueryRow(`SELECT backend_name FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2`,
		f.tenantID, tenant.ExportsBucket).Scan(&backend))
	assert.Equal(t, "idrive-eu-west-1", backend, "the export's bytes are at rest in the EU")

	// No region inside: the export fails with the note, and nothing is
	// written anywhere — not even the bucket row.
	g := setupExportFixture(t)
	g.exec(`INSERT INTO users (id, email, password_hash, status, role) VALUES ($1, $2, 'x', 'active', 'user')`, g.userID, g.email)
	g.exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, residency) VALUES ($1, 'UK Co', $2, $3, $4, 'uk')`,
		g.tenantID, g.email, g.accessKey, g.secretKey)
	g.exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, tier) VALUES ($1, 1000000000, 'standard')`, g.tenantID)
	_, err = g.svc.Request(context.Background(), g.userID, g.tenantID)
	require.NoError(t, err)
	rep := g.run()
	assert.Zero(t, rep.Rows)
	assert.Contains(t, rep.Note, "no_region_inside")
	var n int
	require.NoError(t, g.db.QueryRow(`SELECT COUNT(*) FROM buckets WHERE tenant_id = $1 AND name = $2`, g.tenantID, tenant.ExportsBucket).Scan(&n))
	assert.Zero(t, n, "no system bucket outside the residency")
	require.NoError(t, g.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2`, g.tenantID, tenant.ExportsBucket).Scan(&n))
	assert.Zero(t, n)
}

// RP86-1: the hidden `_exports` row outlives its object (retention deletes
// the object, the row stays), and it made an admin's residency set a 409
// for a bucket the customer can neither see nor delete. An empty system
// bucket never blocks the set: it moves inside in the set's transaction. One
// holding a live export is 409 until the export expires.
func TestResidency_TheSystemBucketNeverBlocksTheSet(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	drivers.SetAvailableIDriveRegions("us-central-1", []string{"eu-west-1"})
	t.Cleanup(drivers.ResetAvailableIDriveRegions)
	t.Setenv("IDRIVE_REGION", "us-central-1")
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, f.user.TenantID)
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, f.user.TenantID)
	})
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility, region, data_residency, metadata)
		VALUES ($1, $2, 'private', 'us-central-1', 'us', '{"system": true}')`, f.user.TenantID, tenant.ExportsBucket)
	require.NoError(t, err)
	put := func(v any) (int, map[string]any) {
		return f.call(http.MethodPut, "/api/v1/admin/tenants/"+f.user.TenantID+"/residency", f.adminJWT, map[string]any{"residency": v})
	}

	code, out := put("eu")
	require.Equal(t, http.StatusOK, code, out)
	var region, label string
	require.NoError(t, f.db.QueryRow(`SELECT region, data_residency FROM buckets WHERE tenant_id = $1 AND name = $2`,
		f.user.TenantID, tenant.ExportsBucket).Scan(&region, &label))
	assert.Equal(t, "eu-west-1", region, "the empty system bucket moved inside with the set")
	assert.Equal(t, "eu", label)

	// A live export in a system bucket outside the asked residency: 409.
	code, out = put(nil)
	require.Equal(t, http.StatusOK, code, out)
	_, err = f.db.Exec(`UPDATE buckets SET region = 'us-central-1', data_residency = 'us' WHERE tenant_id = $1 AND name = $2`, f.user.TenantID, tenant.ExportsBucket)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name)
		VALUES ($1, $2, 'account-export-x.json', 10, 'e', 'idrive')`, f.user.TenantID, tenant.ExportsBucket)
	require.NoError(t, err)
	code, out = put("eu")
	assert.Equal(t, http.StatusConflict, code, out)
	assert.Equal(t, "buckets_outside_residency", errCode(out))
}
