package api

// RP82-4 (Stage 1a Prompt 53, Part 0c): which flags read the account tree.
// The rule is internal/flags'; this pins the classification and the admin
// API's refusal.

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/flags"
)

// Every flag's inheritance is decided here, once: the sealing defaults
// tighten, the edge's performance flags are defaults, every other flag
// stays per tenant. A new flag that should follow a child's tree is added
// to one of the two lists, deliberately.
func TestFlags_TheTreeInheritanceOfEveryFlag(t *testing.T) {
	fl := flags.New(nil, zap.NewNop())
	registerFlags(fl)
	assert.Equal(t, map[string]flags.Inheritance{
		"edge_codec":              flags.InheritTighten,
		"standard_sealed_default": flags.InheritTighten,
		"edge_ingest":             flags.InheritDefault,
		"edge_mirror":             flags.InheritDefault,
		"parallel_get":            flags.InheritDefault,
		"data_plane_slc":          flags.InheritDefault,
		"edge_rs":                 flags.InheritDefault,
	}, fl.Inheritances())
}

// A child's OFF row of a sealing flag its platform turned on is 409
// flag_only_tightens on the admin API; the child reads it ON.
func TestAdminFlags_AChildCannotTurnOffItsPlatformsSealing(t *testing.T) {
	f := setupAdminFlagsFixture(t)
	f.svc.Inherit(f.key, flags.InheritTighten)
	s := uuid.NewString()[:8]
	root, child := "flagapi-root-"+s, "flagapi-child-"+s
	_, err := f.db.Exec(`INSERT INTO tenants (id, name, email, platform) VALUES ($1, 'r', $2, TRUE)`, root, root+"@x.invalid")
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO tenants (id, name, email, parent_tenant_id) VALUES ($1, 'c', $2, $3)`, child, child+"@x.invalid", root)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM feature_flags WHERE tenant_id IN ($1, $2)`, root, child)
		_, _ = f.db.Exec(`DELETE FROM audit_logs WHERE tenant_id IN ($1, $2)`, root, child)
		_, _ = f.db.Exec(`DELETE FROM tenants WHERE id = $1`, child)
		_, _ = f.db.Exec(`DELETE FROM tenants WHERE id = $1`, root)
	})
	require.NoError(t, f.svc.Set(context.Background(), f.key, root, true, "test"))

	w := f.do(t, f.adminID, http.MethodPut, "/api/v1/admin/flags/"+f.key, map[string]any{"tenant_id": child, "enabled": false})
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "flag_only_tightens")
	assert.True(t, f.svc.Enabled(f.key, child))
}
