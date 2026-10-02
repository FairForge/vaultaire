package auth

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R12-8: EnableMFA wrote this process's copy before the database row. A
// write the database refused left the account "enabled" in memory only: the
// dashboard said two-factor was on, sign-in asked for a code — until the next
// restart loaded the table and found nothing.
func TestEnableMFA_ADatabaseFailureLeavesTheAccountNotEnrolled(t *testing.T) {
	// Arrange: a database handle that refuses every statement.
	db, err := sql.Open("postgres", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	svc := NewAuthService(nil, nil)
	u, err := svc.CreateUser(context.Background(), "order@stored.ge", "securepass123")
	require.NoError(t, err)
	svc.sqlDB = db

	// Act
	err = svc.EnableMFA(context.Background(), u.ID, "JBSWY3DPEHPK3PXP", []string{"CODE1"})

	// Assert
	require.Error(t, err)
	enabled, _ := svc.IsMFAEnabled(context.Background(), u.ID)
	assert.False(t, enabled, "memory says enrolled, the database does not")
}
