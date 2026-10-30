package flags

// RP82-4 (Stage 1a Prompt 53, Part 0c): flag inheritance, decided once
// here. A flag registered InheritTighten (a sealing default: privacy and
// safety) is ON for a child when it is ON anywhere up its tree, and a
// child's OFF row under an ancestor's ON is refused (ErrOnlyTightens). A
// flag registered InheritDefault (performance) takes the nearest row up
// the tree as the child's default, which the child's own row overrides.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// treeTenants makes a root, its child and the child's child.
func treeTenants(t *testing.T, svc *Service) (root, child, grandchild string) {
	t.Helper()
	s := uuid.NewString()[:8]
	root, child, grandchild = "flag-root-"+s, "flag-child-"+s, "flag-grand-"+s
	for _, q := range []struct{ id, parent string }{{root, ""}, {child, root}, {grandchild, child}} {
		var parent any
		if q.parent != "" {
			parent = q.parent
		}
		_, err := svc.db.Exec(`INSERT INTO tenants (id, name, email, parent_tenant_id) VALUES ($1, 'f', $2, $3)`, q.id, q.id+"@x.invalid", parent)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		for _, id := range []string{grandchild, child, root} {
			_, _ = svc.db.Exec(`DELETE FROM feature_flags WHERE tenant_id = $1`, id)
			_, _ = svc.db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, id)
			_, _ = svc.db.Exec(`DELETE FROM tenants WHERE id = $1`, id)
		}
	})
	return root, child, grandchild
}

func TestFlags_ChildInheritsSealingDefault(t *testing.T) {
	db := openTestDB(t)
	svc := New(db, zap.NewNop())
	ctx := context.Background()
	sealing := testKey(t, db)
	svc.Register(sealing, false)
	svc.Inherit(sealing, InheritTighten)
	root, child, grandchild := treeTenants(t, svc)
	require.NoError(t, svc.Refresh(ctx))

	assert.False(t, svc.Enabled(sealing, child))
	require.NoError(t, svc.Set(ctx, sealing, root, true, "test"))
	assert.True(t, svc.Enabled(sealing, child), "the platform's sealing default reaches its child")
	assert.True(t, svc.Enabled(sealing, grandchild), "and its child's child")

	err := svc.Set(ctx, sealing, child, false, "test")
	assert.ErrorIs(t, err, ErrOnlyTightens, "a child may not weaken what its parent turned on")
	assert.True(t, svc.Enabled(sealing, child))
	require.NoError(t, svc.Unset(ctx, sealing, root))
	require.NoError(t, svc.Set(ctx, sealing, child, true, "test"), "a child may tighten on its own")
	assert.True(t, svc.Enabled(sealing, grandchild))
	assert.False(t, svc.Enabled(sealing, root), "never upward")
}

func TestFlags_APerformanceFlagInheritsAsADefault(t *testing.T) {
	db := openTestDB(t)
	svc := New(db, zap.NewNop())
	ctx := context.Background()
	perf := testKey(t, db)
	svc.Register(perf, false)
	svc.Inherit(perf, InheritDefault)
	root, child, grandchild := treeTenants(t, svc)
	require.NoError(t, svc.Refresh(ctx))

	require.NoError(t, svc.Set(ctx, perf, root, true, "test"))
	assert.True(t, svc.Enabled(perf, grandchild), "the nearest row up the tree is the default")
	require.NoError(t, svc.Set(ctx, perf, child, false, "test"), "the child may override a performance default")
	assert.False(t, svc.Enabled(perf, child))
	assert.False(t, svc.Enabled(perf, grandchild), "the nearest row wins")
	assert.True(t, svc.Enabled(perf, root))

	plain := testKey(t, db)
	svc.Register(plain, false)
	require.NoError(t, svc.Set(ctx, plain, root, true, "test"))
	assert.False(t, svc.Enabled(plain, child), "a flag registered without inheritance stays per tenant")
}
