package flags

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

// openTestDB returns the shared dev/CI database, skipping when unset.
// CI applies every migration before tests, so feature_flags exists.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := testutil.DSN() // R9: vaultaire_test by default; DATABASE_URL still wins
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())
	return db
}

// testKey returns a flag key unique to the test, and registers cleanup of
// its rows so runs never interfere.
func testKey(t *testing.T, db *sql.DB) string {
	t.Helper()
	key := fmt.Sprintf("test-%s-%d", t.Name(), time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM feature_flags WHERE flag_key = $1", key)
	})
	return key
}

func TestService_NilDB_DefaultsOnly(t *testing.T) {
	t.Run("registered default is served", func(t *testing.T) {
		// Arrange
		svc := New(nil, zap.NewNop())
		svc.Register("chunking", true)
		svc.Register("signups", false)

		// Act + Assert — defaults apply for any tenant, including global.
		assert.True(t, svc.Enabled("chunking", ""))
		assert.True(t, svc.Enabled("chunking", "tenant-abc"))
		assert.False(t, svc.Enabled("signups", ""))
	})

	t.Run("unregistered key is disabled", func(t *testing.T) {
		svc := New(nil, zap.NewNop())
		assert.False(t, svc.Enabled("does-not-exist", "tenant-abc"))
	})

	t.Run("Set errors without a database", func(t *testing.T) {
		svc := New(nil, zap.NewNop())
		svc.Register("chunking", true)
		err := svc.Set(context.Background(), "chunking", GlobalTenant, false, "test")
		require.Error(t, err)
	})

	t.Run("Refresh is a no-op without a database", func(t *testing.T) {
		svc := New(nil, zap.NewNop())
		require.NoError(t, svc.Refresh(context.Background()))
	})
}

func TestService_ResolutionPrecedence(t *testing.T) {
	// Arrange
	db := openTestDB(t)
	ctx := context.Background()
	key := testKey(t, db)

	svc := New(db, zap.NewNop())
	svc.Register(key, false) // in-code default: off
	require.NoError(t, svc.Refresh(ctx))

	// No rows → registered default.
	assert.False(t, svc.Enabled(key, "tenant-a"))
	assert.False(t, svc.Enabled(key, ""))

	// Global row overrides the default for everyone.
	require.NoError(t, svc.Set(ctx, key, GlobalTenant, true, "tester"))
	assert.True(t, svc.Enabled(key, "tenant-a"))
	assert.True(t, svc.Enabled(key, ""))

	// Tenant row overrides the global row — for that tenant only.
	require.NoError(t, svc.Set(ctx, key, "tenant-a", false, "tester"))
	assert.False(t, svc.Enabled(key, "tenant-a"))
	assert.True(t, svc.Enabled(key, "tenant-b"))

	// Unset the tenant override → back to the global row.
	require.NoError(t, svc.Unset(ctx, key, "tenant-a"))
	assert.True(t, svc.Enabled(key, "tenant-a"))

	// Unset the global row → back to the registered default.
	require.NoError(t, svc.Unset(ctx, key, GlobalTenant))
	assert.False(t, svc.Enabled(key, "tenant-a"))
}

func TestService_SetReloadsCacheImmediately(t *testing.T) {
	// Arrange — two service instances sharing the same table, simulating
	// two processes (or the admin flip vs. the serving path).
	db := openTestDB(t)
	ctx := context.Background()
	key := testKey(t, db)

	writer := New(db, zap.NewNop())
	writer.Register(key, false)
	require.NoError(t, writer.Refresh(ctx))

	reader := New(db, zap.NewNop())
	reader.Register(key, false)
	require.NoError(t, reader.Refresh(ctx))

	// Act — flip via the writer. Its own cache must reflect the change
	// with NO background refresh (write-through + immediate reload).
	require.NoError(t, writer.Set(ctx, key, GlobalTenant, true, "tester"))

	// Assert
	assert.True(t, writer.Enabled(key, "any-tenant"),
		"Set must be visible on the writing service immediately")
	assert.False(t, reader.Enabled(key, "any-tenant"),
		"other instances see the change only after their periodic refresh")
	require.NoError(t, reader.Refresh(ctx))
	assert.True(t, reader.Enabled(key, "any-tenant"),
		"refresh must pick up the new row")
}

func TestService_Resolved(t *testing.T) {
	// Arrange
	db := openTestDB(t)
	ctx := context.Background()
	key := testKey(t, db)

	svc := New(db, zap.NewNop())
	svc.Register(key, true)
	require.NoError(t, svc.Refresh(ctx))
	require.NoError(t, svc.Set(ctx, key, GlobalTenant, false, "admin@test"))
	require.NoError(t, svc.Set(ctx, key, "tenant-x", true, "admin@test"))

	// Act
	resolved := svc.Resolved()

	// Assert — find our key in the resolved list.
	var found *Flag
	for i := range resolved {
		if resolved[i].Key == key {
			found = &resolved[i]
			break
		}
	}
	require.NotNil(t, found, "resolved list must include registered flag %s", key)
	assert.True(t, found.Default)
	assert.True(t, found.Registered)
	assert.True(t, found.HasGlobal)
	assert.False(t, found.Global, "global row is off")
	assert.False(t, found.Enabled, "effective global state follows the global row")
	require.Len(t, found.Overrides, 1)
	assert.Equal(t, "tenant-x", found.Overrides[0].TenantID)
	assert.True(t, found.Overrides[0].Enabled)
	assert.Equal(t, "admin@test", found.Overrides[0].UpdatedBy)
}

func TestService_StartBackgroundRefresh(t *testing.T) {
	// Arrange
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := testKey(t, db)

	svc := New(db, zap.NewNop())
	svc.Register(key, false)
	svc.refreshInterval = 20 * time.Millisecond
	svc.Start(ctx)

	// Act — write a row behind the service's back (another process).
	other := New(db, zap.NewNop())
	other.Register(key, false)
	require.NoError(t, other.Set(context.Background(), key, GlobalTenant, true, "tester"))

	// Assert — the background loop picks it up.
	require.Eventually(t, func() bool {
		return svc.Enabled(key, "tenant-a")
	}, 2*time.Second, 10*time.Millisecond,
		"background refresh must pick up rows written by other processes")
}

// Stage 1a Prompt 34a (RP44-2): the write guard runs BEFORE the row is
// written, and a refusal writes nothing; an OnWins flag's tenant row can
// only turn it on; a GlobalOnly flag takes no tenant row.
func TestWriteGuard_RunsBeforeTheRow_RefusalWritesNothing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	key, tenant := testKey(t, db), "tenant-p34a"
	s := New(db, nil)
	s.Register(key, false)
	var sawRow []bool
	refuse := false
	s.SetWriteGuard(func(_ context.Context, c Change) error {
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM feature_flags WHERE flag_key = $1 AND tenant_id = $2 AND enabled = $3`,
			c.Key, c.TenantID, c.Enabled).Scan(&n))
		sawRow = append(sawRow, n > 0)
		if refuse {
			return errors.New("the log is frozen")
		}
		return nil
	})

	// Act + Assert: the guard sees no row; then the row is there.
	require.NoError(t, s.Set(ctx, key, tenant, true, "test"))
	assert.Equal(t, []bool{false}, sawRow)
	assert.True(t, s.Enabled(key, tenant))

	// A refusal: neither a set nor a clear is written.
	refuse = true
	err := s.Set(ctx, key, tenant, false, "test")
	assert.ErrorIs(t, err, ErrChangeRefused)
	assert.True(t, s.Enabled(key, tenant), "a refused set was written")
	assert.ErrorIs(t, s.Unset(ctx, key, tenant), ErrChangeRefused)
	require.NoError(t, s.Refresh(ctx))
	assert.True(t, s.Enabled(key, tenant), "a refused clear was written")
}

func TestOnWins_TenantRowOnlyTurnsItOn(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	key, tenant := testKey(t, db), "tenant-p34a"
	// Global ON by the default (no '*' row on the shared database).
	on := New(db, nil)
	on.Register(key, true)
	on.OnWins(key)

	// A tenant OFF row is refused; written behind the door, it changes nothing.
	assert.ErrorIs(t, on.Set(ctx, key, tenant, false, "test"), ErrNotExemptible)
	_, err := db.Exec(`INSERT INTO feature_flags (flag_key, tenant_id, enabled, updated_by) VALUES ($1, $2, false, 'sql')`, key, tenant)
	require.NoError(t, err)
	require.NoError(t, on.Refresh(ctx))
	assert.True(t, on.Enabled(key, tenant), "ON wins")

	// Global OFF: the tenant row turns it on for that tenant only.
	off := New(db, nil)
	off.Register(key, false)
	off.OnWins(key)
	require.NoError(t, off.Set(ctx, key, tenant, true, "test"))
	assert.True(t, off.Enabled(key, tenant))
	assert.False(t, off.Enabled(key, "another-"+tenant))
	require.NoError(t, off.Set(ctx, key, tenant, false, "test"), "an OFF row is no exemption while the flag is off globally")
	assert.False(t, off.Enabled(key, tenant))

	// GlobalOnly: no tenant row.
	g := New(db, nil)
	g.Register(key, false)
	g.GlobalOnly(key)
	assert.ErrorIs(t, g.Set(ctx, key, tenant, true, "test"), ErrGlobalOnly)
}
