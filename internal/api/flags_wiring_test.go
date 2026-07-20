package api

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

func openFlagsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())
	return db
}

func TestSignupsDefaultFromEnv(t *testing.T) {
	t.Run("unset means enabled", func(t *testing.T) {
		t.Setenv("SIGNUPS_ENABLED", "")
		assert.True(t, signupsDefaultFromEnv())
	})
	t.Run("false closes signups", func(t *testing.T) {
		t.Setenv("SIGNUPS_ENABLED", "false")
		assert.False(t, signupsDefaultFromEnv())
	})
	t.Run("true opens signups", func(t *testing.T) {
		t.Setenv("SIGNUPS_ENABLED", "true")
		assert.True(t, signupsDefaultFromEnv())
	})
	t.Run("garbage means enabled (parse failure ignored, matching old behavior)", func(t *testing.T) {
		t.Setenv("SIGNUPS_ENABLED", "banana")
		assert.True(t, signupsDefaultFromEnv())
	})
}

// TestSignupsFlag_DBRowOverridesEnv — 1.13 decision of record: the `signups`
// flag's in-code default reads SIGNUPS_ENABLED, so a deploy never reopens
// signups; a DB row then overrides the env in EITHER direction with no
// deploy. Wired exactly as server.go does it.
func TestSignupsFlag_DBRowOverridesEnv(t *testing.T) {
	db := openFlagsTestDB(t)
	ctx := context.Background()

	// The test writes real `signups` rows; restore a clean slate after.
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM feature_flags WHERE flag_key = $1", flagSignups)
	})

	wire := func(t *testing.T) (*flags.Service, *auth.AuthService) {
		t.Helper()
		fs := flags.New(db, zap.NewNop())
		fs.Register(flagSignups, signupsDefaultFromEnv())
		require.NoError(t, fs.Refresh(ctx))
		as := auth.NewAuthService(nil, nil) // in-memory only
		as.SetSignupsEnabledFunc(func() bool { return fs.Enabled(flagSignups, "") })
		return fs, as
	}
	uniqueEmail := func(tag string) string {
		return fmt.Sprintf("flag-%s-%d@example.com", tag, time.Now().UnixNano())
	}

	t.Run("env closed, DB row opens", func(t *testing.T) {
		_, err := db.Exec("DELETE FROM feature_flags WHERE flag_key = $1", flagSignups)
		require.NoError(t, err)
		t.Setenv("SIGNUPS_ENABLED", "false")
		fs, as := wire(t)

		// Env default: closed — a deploy with no DB row keeps signups shut.
		_, _, _, err = as.CreateUserWithTenant(ctx, uniqueEmail("closed"), "pw-123456", "Co")
		require.ErrorIs(t, err, auth.ErrSignupsDisabled)

		// A DB row overrides env with no deploy.
		require.NoError(t, fs.Set(ctx, flagSignups, flags.GlobalTenant, true, "test"))
		_, _, _, err = as.CreateUserWithTenant(ctx, uniqueEmail("opened"), "pw-123456", "Co")
		require.NoError(t, err)
	})

	t.Run("env open, DB row closes", func(t *testing.T) {
		_, err := db.Exec("DELETE FROM feature_flags WHERE flag_key = $1", flagSignups)
		require.NoError(t, err)
		t.Setenv("SIGNUPS_ENABLED", "true")
		fs, as := wire(t)

		_, _, _, err = as.CreateUserWithTenant(ctx, uniqueEmail("open"), "pw-123456", "Co")
		require.NoError(t, err)

		require.NoError(t, fs.Set(ctx, flagSignups, flags.GlobalTenant, false, "test"))
		_, _, _, err = as.CreateUserWithTenant(ctx, uniqueEmail("shut"), "pw-123456", "Co")
		require.ErrorIs(t, err, auth.ErrSignupsDisabled)
	})
}
