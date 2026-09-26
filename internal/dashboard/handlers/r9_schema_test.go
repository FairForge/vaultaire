// r9_schema_test.go — Review R9: dashboard queries that referenced columns the
// migrated schema does not have (R9-02 dashboard export, R9-09 admin recent
// users). Real database on purpose — sqlmock cannot catch phantom columns.
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func r9Fixture(t *testing.T, db *sql.DB) (userID, tenantID string) {
	t.Helper()
	ctx := context.Background()
	tenantID = fmt.Sprintf("test-r9-dash-%d", time.Now().UnixNano())
	email := tenantID + "@r9.test"
	userID = uuid.New().String()
	// created_at in the future keeps this user inside the admin "recent users"
	// LIMIT 10 window whatever else the shared test DB holds.
	_, err := db.ExecContext(ctx,
		`INSERT INTO users (id, email, password_hash, company, created_at) VALUES ($1, $2, 'h', 'R9 Co', NOW() + interval '1 day')`, userID, email)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO tenants (id, name, email, access_key, secret_key, plan) VALUES ($1, $1, $2, $1, 's', 'standard')`, tenantID, email)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, updated_at)
		 VALUES ($1, 'b1', 'k1', 123, 'e', 'text/plain', NOW())`, tenantID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO bandwidth_usage_daily (tenant_id, date, ingress_bytes, egress_bytes, requests_count)
		 VALUES ($1, CURRENT_DATE, 1, 2, 7)`, tenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM bandwidth_usage_daily WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, userID)
	})
	return userID, tenantID
}

// R9-09: the admin overview joined tenants on users.tenant_id, a column that
// does not exist, so the "recent users" panel was always empty.
func TestQueryRecentUsers_MigratedColumns(t *testing.T) {
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	_, tenantID := r9Fixture(t, db)

	users := queryRecentUsers(context.Background(), db, zap.NewNop())
	require.NotEmpty(t, users, "recent users must not be empty on a populated database")
	var found *recentUser
	for i := range users {
		if users[i].Email == tenantID+"@r9.test" {
			found = &users[i]
		}
	}
	require.NotNil(t, found, "the newest user must be listed")
	assert.Equal(t, "R9 Co", found.Company)
	assert.Equal(t, "standard", found.Plan, "plan must come from the tenant joined by email")
}

// R9-02: the dashboard data export selected object_head_cache.size /
// last_modified and bandwidth_usage_daily.requests — none exist — and, because
// the errors were swallowed, exported empty sections.
func TestCollectExportData_MigratedColumns(t *testing.T) {
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	userID, tenantID := r9Fixture(t, db)

	req := httptest.NewRequest("POST", "/dashboard/settings/export", nil)
	out := collectExportData(req, db, userID, tenantID, zap.NewNop())

	var data map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &data))
	var objects []map[string]interface{}
	require.NoError(t, json.Unmarshal(data["objects"], &objects))
	require.Len(t, objects, 1, "export must include the tenant's objects")
	assert.EqualValues(t, 123, objects[0]["size"])
	var bw []map[string]interface{}
	require.NoError(t, json.Unmarshal(data["bandwidth_usage"], &bw))
	require.Len(t, bw, 1, "export must include bandwidth usage")
	assert.EqualValues(t, 7, bw[0]["requests"])
}
