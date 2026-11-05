package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RP96-20: the bucket page's line is computed at most once a minute per
// (tenant, bucket); another bucket or tenant is its own entry.
func TestDashboard_BucketLineIsCachedAMinute(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	prior := verifiedLineNow
	verifiedLineNow = func() time.Time { return now }
	t.Cleanup(func() { verifiedLineNow = prior })

	flagOff := func() {
		mock.ExpectQuery(`SELECT enabled, updated_at FROM feature_flags`).WillReturnRows(sqlmock.NewRows([]string{"enabled", "updated_at"}).AddRow(false, now))
	}
	flagOff()
	assert.Empty(t, CachedBucketVerifiedLine(context.Background(), db, "t-cache", "b"))
	assert.Empty(t, CachedBucketVerifiedLine(context.Background(), db, "t-cache", "b"), "within the minute: no query")
	require.NoError(t, mock.ExpectationsWereMet())

	flagOff()
	assert.Empty(t, CachedBucketVerifiedLine(context.Background(), db, "t-cache", "other"), "another bucket is its own entry")
	require.NoError(t, mock.ExpectationsWereMet())

	now = now.Add(61 * time.Second)
	flagOff()
	assert.Empty(t, CachedBucketVerifiedLine(context.Background(), db, "t-cache", "b"), "past the minute: computed again")
	require.NoError(t, mock.ExpectationsWereMet())
}
