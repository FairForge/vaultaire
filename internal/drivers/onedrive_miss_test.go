package drivers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
)

// Prompt 2b.5 F1.1: permafrost's "absent" is a typed answer. Absent on every
// account is an engine.NotFoundError; any other Graph status is an error —
// whatever the key is called (the error text carries the path, and the miss
// predicates used to read it).
func TestOneDrive_AbsenceIsTypedAndOtherStatusesAreNeverMisses(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")

	t.Run("absent everywhere is a typed miss", func(t *testing.T) {
		d, _ := stubFleet(3, zap.NewNop())
		_, err := d.Get(ctx, "bkt", "plain.txt")
		require.Error(t, err)
		assert.True(t, engine.IsObjectMiss(err), "GET: %v", err)
		err = d.Delete(ctx, "bkt", "plain.txt")
		require.Error(t, err)
		assert.True(t, engine.IsObjectMiss(err), "DELETE: %v", err)
	})

	for _, key := range []string{"NotFound.txt", "404.html", "errors/page not found.html", "itemNotFound"} {
		t.Run("403 on "+key, func(t *testing.T) {
			d, stubs := stubFleet(3, zap.NewNop())
			path := d.ObjectKey(ctx, "bkt", key)
			for i, tn := range d.tenants {
				stubs[i].routes["GET "+itemURL(tn, path)] = odRoute{403, `{"error":{"code":"accessDenied"}}`}
			}
			_, err := d.Get(ctx, "bkt", key)
			require.Error(t, err)
			assert.False(t, engine.IsObjectMiss(err), "GET: %v", err)
			err = d.Delete(ctx, "bkt", key)
			require.Error(t, err)
			assert.False(t, engine.IsObjectMiss(err), "DELETE: %v", err)
		})
	}
}
