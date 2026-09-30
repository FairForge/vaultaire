package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("requires database")
	}
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	if err := db.Ping(); err != nil {
		t.Skip("PostgreSQL not available:", err)
	}
	return db
}

func TestRecord_WritesRowWithRequestContext(t *testing.T) {
	db := openDB(t)
	defer func() { _ = db.Close() }()

	actor := uuid.New().String()
	subject := uuid.New().String()
	tenant := "tenant-audit-test-" + uuid.NewString()[:8]
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, tenant) })
	ctx := WithRequest(context.Background(), "203.0.113.9", "curl/8")
	ctx = WithActor(ctx, actor)

	Record(ctx, db, Entry{
		UserID:   subject,
		TenantID: tenant,
		Action:   "key.revoked",
		Resource: "key:abc",
		Metadata: map[string]any{"key_id": "abc"},
	})

	var (
		userID, performedBy       sql.NullString
		eventType, action, result string
		severity, ip, ua          sql.NullString
		meta                      []byte
	)
	err := db.QueryRow(`SELECT user_id::text, performed_by::text, event_type, action, result, severity, host(ip), user_agent, metadata
		FROM audit_logs WHERE tenant_id = $1 AND resource = 'key:abc'`, tenant).
		Scan(&userID, &performedBy, &eventType, &action, &result, &severity, &ip, &ua, &meta)
	require.NoError(t, err)
	assert.Equal(t, subject, userID.String)
	assert.Equal(t, actor, performedBy.String)
	assert.Equal(t, "key", eventType, "event_type defaults to the action's prefix")
	assert.Equal(t, "key.revoked", action)
	assert.Equal(t, "success", result)
	assert.Equal(t, "info", severity.String)
	assert.Equal(t, "203.0.113.9", ip.String)
	assert.Equal(t, "curl/8", ua.String)
	var m map[string]any
	require.NoError(t, json.Unmarshal(meta, &m))
	assert.Equal(t, "abc", m["key_id"])
}

func TestRecord_FailureCarriesErrorAndNonUUIDIsNull(t *testing.T) {
	db := openDB(t)
	defer func() { _ = db.Close() }()

	tenant := "tenant-audit-test2-" + uuid.NewString()[:8]
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, tenant) })
	Record(context.Background(), db, Entry{
		UserID:   "not-a-uuid",
		TenantID: tenant,
		Action:   "flag.set",
		Error:    errors.New("boom"),
	})

	var userID sql.NullString
	var result, severity, errMsg string
	err := db.QueryRow(`SELECT user_id::text, result, severity, COALESCE(error_msg,'') FROM audit_logs WHERE tenant_id = $1`, tenant).
		Scan(&userID, &result, &severity, &errMsg)
	require.NoError(t, err)
	assert.False(t, userID.Valid, "a non-UUID subject is stored as NULL, never as a failed insert")
	assert.Equal(t, "failure", result)
	assert.Equal(t, "error", severity)
	assert.Equal(t, "boom", errMsg)
}

func TestRecord_SurvivesCancelledRequestContext(t *testing.T) {
	db := openDB(t)
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client hung up before the handler finished

	tenant := "tenant-audit-test3-" + uuid.NewString()[:8]
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, tenant) })
	Record(ctx, db, Entry{TenantID: tenant, Action: "account.deletion_scheduled"})

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1`, tenant).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestRecord_NilDBIsNoop(t *testing.T) {
	assert.NotPanics(t, func() {
		Record(context.Background(), nil, Entry{Action: "x"})
	})
}

func TestList_TenantAndCursor(t *testing.T) {
	db := openDB(t)
	defer func() { _ = db.Close() }()
	tenant := "tenant-audit-list-" + uuid.NewString()[:8]
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id LIKE $1`, tenant+"%") })

	for i := 0; i < 3; i++ {
		Record(context.Background(), db, Entry{TenantID: tenant, Action: "flag.set", Metadata: map[string]any{"i": i}})
		time.Sleep(2 * time.Millisecond)
	}
	Record(context.Background(), db, Entry{TenantID: tenant + "-other", Action: "flag.set"})

	page, err := List(context.Background(), db, Filter{TenantID: tenant, Limit: 2})
	require.NoError(t, err)
	require.Len(t, page.Rows, 2)
	assert.True(t, page.HasMore)
	assert.NotEmpty(t, page.NextCursor)

	page2, err := List(context.Background(), db, Filter{TenantID: tenant, Limit: 2, Cursor: page.NextCursor})
	require.NoError(t, err)
	require.Len(t, page2.Rows, 1)
	assert.False(t, page2.HasMore)
	assert.NotEqual(t, page.Rows[1].ID, page2.Rows[0].ID)

	_, err = List(context.Background(), db, Filter{Cursor: "garbage"})
	assert.ErrorIs(t, err, ErrBadCursor)
}
