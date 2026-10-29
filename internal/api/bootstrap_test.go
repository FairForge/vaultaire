package api

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/testutil"
)

// WP-DX-1 PR C: the local image's first-boot tenant. VAULTAIRE_BOOTSTRAP_EMAIL
// makes one account at the first boot and prints its key pair ONCE; every
// later boot finds it and prints no secret.
func TestBootstrapTenant_MadeOnceKeysPrintedOnce(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())
	email := "bootstrap-" + uuid.NewString()[:8] + "@stored.ge"
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM api_keys WHERE user_id IN (SELECT id FROM users WHERE email = $1)`, email)
		_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id IN (SELECT id FROM tenants WHERE email = $1)`, email)
		_, _ = db.Exec(`DELETE FROM tenants WHERE email = $1`, email)
		_, _ = db.Exec(`DELETE FROM users WHERE email = $1`, email)
	})
	svc := auth.NewAuthService(nil, db)
	var out bytes.Buffer
	made, err := bootstrapTenant(context.Background(), svc, zap.NewNop(), email, &out)
	require.NoError(t, err)
	assert.True(t, made)
	assert.Contains(t, out.String(), "AWS_ACCESS_KEY_ID=VK")
	assert.Contains(t, out.String(), "AWS_SECRET_ACCESS_KEY=")
	assert.Contains(t, out.String(), "shown once")

	out.Reset()
	fresh := auth.NewAuthService(nil, db)
	require.NoError(t, fresh.LoadFromDB(context.Background()))
	made, err = bootstrapTenant(context.Background(), fresh, zap.NewNop(), email, &out)
	require.NoError(t, err)
	assert.False(t, made, "idempotent: the next boot finds it")
	assert.NotContains(t, out.String(), "AWS_SECRET_ACCESS_KEY", "the secret is never printed twice")
	assert.True(t, strings.Contains(out.String(), "already"))
}
