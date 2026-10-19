package api

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RP16-5: checkMFADelete read a failing SELECT of mfa_delete_enabled as
// "MFA Delete off", so a database that could not answer that one query let
// a DELETE, a DeleteObjects and a versioning suspend through without a code
// on a bucket that requires one. Now the read failing is 503
// ServiceUnavailable + Retry-After on all three, and nothing is touched.

// poisonedSQLDriver wraps lib/pq and fails every query whose text contains
// poison — the database "fails" for exactly the statement under test.
type poisonedSQLDriver struct {
	inner  driver.Driver
	poison string
}

func (d *poisonedSQLDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &poisonedConn{Conn: c, poison: d.poison}, nil
}

type poisonedConn struct {
	driver.Conn
	poison string
}

var errPoisoned = errors.New("poisoned: connection reset by peer")

func (c *poisonedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, c.poison) {
		return nil, errPoisoned
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *poisonedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(query, c.poison) {
		return nil, errPoisoned
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *poisonedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if strings.Contains(query, c.poison) {
		return nil, errPoisoned
	}
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
}

func (c *poisonedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *poisonedConn) CheckNamedValue(nv *driver.NamedValue) error {
	return c.Conn.(driver.NamedValueChecker).CheckNamedValue(nv)
}

var mfaPoisonOnce sync.Once

// poisonMFAFlag points the fixture's server at a pool on which every read of
// mfa_delete_enabled fails; f.db (the test's own handle) still works.
func (f *mfaDeleteFixture) poisonMFAFlag(t *testing.T) {
	t.Helper()
	mfaPoisonOnce.Do(func() {
		sql.Register("postgres-poison-mfa", &poisonedSQLDriver{inner: &pq.Driver{}, poison: "mfa_delete_enabled"})
	})
	db, err := sql.Open("postgres-poison-mfa", testutil.DSN())
	require.NoError(t, err)
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	f.server.db = db
}

func assertMFAUnknown503(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), ErrServiceUnavailable)
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
}

func TestMFADelete_FlagReadFailureIs503OnDelete(t *testing.T) {
	f := setupMFADeleteFixture(t)
	f.putObject(t, "keep.txt", "x")
	f.enableMFADelete(t)
	f.poisonMFAFlag(t)

	req := httptest.NewRequest("DELETE", "/"+f.bucket+"/keep.txt", nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleDeleteObject(w, req, &S3Request{Bucket: f.bucket, Object: "keep.txt", TenantID: f.tenantID})

	assertMFAUnknown503(t, w)
	assert.Equal(t, 1, f.headRows(t, "keep.txt"), "the object is untouched")
}

func TestMFADelete_FlagReadFailureIs503OnTheBatch(t *testing.T) {
	f := setupMFADeleteFixture(t)
	for _, k := range batchMFAKeys {
		f.putObject(t, k, "x")
	}
	f.enableMFADelete(t)
	f.poisonMFAFlag(t)

	w := f.batchDelete(t, "", batchMFAKeys...)

	assertMFAUnknown503(t, w)
	assert.Equal(t, len(batchMFAKeys), f.headRows(t, batchMFAKeys...), "no key deleted")
}

func TestMFADelete_FlagReadFailureIs503OnVersioningSuspend(t *testing.T) {
	f := setupMFADeleteFixture(t)
	f.enableMFADelete(t)
	_, err := f.db.Exec(`UPDATE buckets SET versioning_status = 'Enabled' WHERE tenant_id = $1 AND name = $2`,
		f.tenantID, f.bucket)
	require.NoError(t, err)
	f.poisonMFAFlag(t)

	body := `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Suspended</Status></VersioningConfiguration>`
	req := httptest.NewRequest("PUT", "/"+f.bucket+"?versioning", bytes.NewReader([]byte(body)))
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handlePutBucketVersioning(w, req, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})

	assertMFAUnknown503(t, w)
	var status string
	require.NoError(t, f.db.QueryRow(`SELECT versioning_status FROM buckets WHERE tenant_id = $1 AND name = $2`,
		f.tenantID, f.bucket).Scan(&status))
	assert.Equal(t, "Enabled", status, "versioning was not suspended")
}

func TestMFADelete_ABucketWithNoRowStaysUngated(t *testing.T) {
	f := setupMFADeleteFixture(t)
	req := httptest.NewRequest("DELETE", "/no-row-bucket/k", nil)

	err := checkMFADelete(context.Background(), f.db, f.authSvc, f.mfaSvc, f.tenantID, "no-row-bucket", req)

	assert.NoError(t, err, "no bucket row = MFA Delete not enabled (ErrNoRows stays nil)")
}
