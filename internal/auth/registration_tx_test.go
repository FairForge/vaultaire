package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Registration writes users → tenants → api_keys → tenant_quotas in ONE
// transaction and fills the in-memory maps only after the commit (Review
// R10-10 / R5-18). A failure on the third INSERT must leave no row behind
// and no account that can log in until the next restart.
func TestCreateUserWithTenant_IsAtomic(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	const email = "tx-fail@stored.ge"

	// A trigger that rejects the primary api_keys row of exactly this user.
	_, err := db.Exec(`CREATE OR REPLACE FUNCTION r10_reg_fail() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF EXISTS (SELECT 1 FROM users u WHERE u.id = NEW.user_id AND u.email = 'tx-fail@stored.ge') THEN
				RAISE EXCEPTION 'r10: simulated api_keys failure';
			END IF;
			RETURN NEW;
		END $$`)
	require.NoError(t, err)
	_, err = db.Exec(`DROP TRIGGER IF EXISTS r10_reg_fail_trg ON api_keys;
		CREATE TRIGGER r10_reg_fail_trg BEFORE INSERT ON api_keys FOR EACH ROW EXECUTE FUNCTION r10_reg_fail()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS r10_reg_fail_trg ON api_keys`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS r10_reg_fail()`)
		_, _ = db.Exec(`DELETE FROM tenants WHERE email = $1`, email)
		_, _ = db.Exec(`DELETE FROM users WHERE email = $1`, email)
	})

	svc := NewAuthService(nil, db)

	// Act
	_, _, _, err = svc.CreateUserWithTenant(context.Background(), email, "correct horse battery", "Tx Co")

	// Assert: the error surfaces and nothing was left behind — in the
	// database or in memory.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "persist api key")
	var users, tenants int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM users WHERE email = $1`, email).Scan(&users))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM tenants WHERE email = $1`, email).Scan(&tenants))
	assert.Equal(t, 0, users, "the users row rolled back")
	assert.Equal(t, 0, tenants, "the tenants row rolled back")
	_, inMemory := svc.users[email]
	assert.False(t, inMemory, "no phantom in-memory account")

	// With the trigger gone the same registration succeeds and all four
	// rows exist, in order.
	_, err = db.Exec(`DROP TRIGGER IF EXISTS r10_reg_fail_trg ON api_keys`)
	require.NoError(t, err)
	user, tenant, key, err := svc.CreateUserWithTenant(context.Background(), email, "correct horse battery", "Tx Co")
	require.NoError(t, err)
	assert.Equal(t, tenant.AccessKey, key.Key, "the primary key is the tenant's S3 pair")
	var n int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM users u
		JOIN tenants t ON t.id = $2
		JOIN api_keys k ON k.user_id = u.id AND k.key_id = $3
		JOIN tenant_quotas q ON q.tenant_id = t.id
		WHERE u.id = $1`, user.ID, tenant.ID, key.Key).Scan(&n))
	assert.Equal(t, 1, n, "users → tenants → api_keys → tenant_quotas all present")
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, tenant.ID) })
}
