package compliance

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBreachPgStore_CreateAndGet(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	store := NewBreachPgStore(db)
	ctx := context.Background()

	breach := &BreachRecord{
		ID:                uuid.New(),
		BreachType:        BreachTypeDataLeakage,
		Severity:          BreachSeverityHigh,
		Status:            BreachStatusDetected,
		DetectedAt:        time.Now(),
		AffectedUserCount: 42,
		DataCategories:    []string{"email", "health"},
		Description:       "test breach",
		RootCause:         "misconfiguration",
		DeadlineAt:        time.Now().Add(72 * time.Hour),
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}

	// Expect INSERT
	mock.ExpectExec(`INSERT INTO breach_records`).
		WithArgs(
			breach.ID, breach.BreachType, breach.Severity, breach.Status,
			breach.DetectedAt, breach.ReportedAt,
			breach.AffectedUserCount, breach.AffectedRecordCount,
			sqlmock.AnyArg(), // categories JSON
			breach.Description, breach.RootCause, breach.Consequences, breach.Mitigation,
			breach.NotifiedAuthority, breach.NotifiedSubjects,
			breach.AuthorityNotifiedAt, breach.SubjectsNotifiedAt,
			breach.DeadlineAt, sqlmock.AnyArg(), // metadata
			breach.CreatedAt, breach.UpdatedAt,
		).WillReturnResult(sqlmock.NewResult(0, 1))

	err = store.CreateBreach(ctx, breach)
	require.NoError(t, err)

	// Expect SELECT
	rows := sqlmock.NewRows([]string{
		"id", "breach_type", "severity", "status", "detected_at", "reported_at",
		"affected_user_count", "affected_record_count", "data_categories",
		"description", "root_cause", "consequences", "mitigation",
		"notified_authority", "notified_subjects", "authority_notified_at",
		"subjects_notified_at", "deadline_at", "metadata", "created_at", "updated_at",
	}).AddRow(
		breach.ID, breach.BreachType, breach.Severity, breach.Status,
		breach.DetectedAt, breach.ReportedAt,
		breach.AffectedUserCount, breach.AffectedRecordCount,
		[]byte(`["email","health"]`),
		breach.Description, breach.RootCause, breach.Consequences, breach.Mitigation,
		breach.NotifiedAuthority, breach.NotifiedSubjects,
		breach.AuthorityNotifiedAt, breach.SubjectsNotifiedAt,
		breach.DeadlineAt, nil, breach.CreatedAt, breach.UpdatedAt,
	)
	mock.ExpectQuery(`SELECT .+ FROM breach_records WHERE id = \$1`).
		WithArgs(breach.ID).WillReturnRows(rows)

	got, err := store.GetBreach(ctx, breach.ID)
	require.NoError(t, err)
	assert.Equal(t, breach.ID, got.ID)
	assert.Equal(t, BreachTypeDataLeakage, got.BreachType)
	assert.Equal(t, BreachSeverityHigh, got.Severity)
	assert.Equal(t, 42, got.AffectedUserCount)
	assert.Equal(t, []string{"email", "health"}, got.DataCategories)

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBreachPgStore_AddAffectedUsers(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	store := NewBreachPgStore(db)
	ctx := context.Background()

	breachID := uuid.New()
	userIDs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}

	for range userIDs {
		mock.ExpectExec(`INSERT INTO breach_affected_users`).
			WithArgs(sqlmock.AnyArg(), breachID, sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}

	err = store.AddAffectedUsers(ctx, breachID, userIDs)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBreachPgStore_ListBreaches(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	store := NewBreachPgStore(db)
	ctx := context.Background()

	now := time.Now()
	id1 := uuid.New()
	id2 := uuid.New()

	rows := sqlmock.NewRows([]string{
		"id", "breach_type", "severity", "status", "detected_at", "reported_at",
		"affected_user_count", "affected_record_count", "data_categories",
		"description", "root_cause", "consequences", "mitigation",
		"notified_authority", "notified_subjects", "authority_notified_at",
		"subjects_notified_at", "deadline_at", "metadata", "created_at", "updated_at",
	}).
		AddRow(id1, "data_leakage", "high", "detected", now, nil,
			10, 100, []byte(`["email"]`), "desc1", "", "", "",
			false, false, nil, nil, now.Add(72*time.Hour), nil, now, now).
		AddRow(id2, "phishing", "medium", "detected", now, nil,
			5, 50, []byte(`[]`), "desc2", "", "", "",
			false, false, nil, nil, now.Add(72*time.Hour), nil, now, now)

	mock.ExpectQuery(`SELECT .+ FROM breach_records WHERE 1=1 AND status = \$1`).
		WithArgs("detected").WillReturnRows(rows)

	breaches, err := store.ListBreaches(ctx, map[string]interface{}{"status": "detected"})
	require.NoError(t, err)
	assert.Len(t, breaches, 2)
	assert.Equal(t, id1, breaches[0].ID)
	assert.Equal(t, id2, breaches[1].ID)

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBreachPgStore_UpdateStatus(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	store := NewBreachPgStore(db)
	ctx := context.Background()

	breach := &BreachRecord{
		ID:             uuid.New(),
		BreachType:     BreachTypeRansomware,
		Severity:       BreachSeverityCritical,
		Status:         BreachStatusMitigated,
		DetectedAt:     time.Now(),
		DataCategories: []string{"financial"},
		Description:    "ransomware attack",
		DeadlineAt:     time.Now().Add(72 * time.Hour),
		UpdatedAt:      time.Now(),
	}

	mock.ExpectExec(`UPDATE breach_records SET`).
		WithArgs(
			breach.ID, breach.BreachType, breach.Severity, breach.Status,
			breach.ReportedAt, breach.AffectedUserCount, breach.AffectedRecordCount,
			sqlmock.AnyArg(), // categories
			breach.Description, breach.RootCause,
			breach.Consequences, breach.Mitigation,
			breach.NotifiedAuthority, breach.NotifiedSubjects,
			breach.AuthorityNotifiedAt, breach.SubjectsNotifiedAt,
			sqlmock.AnyArg(), // metadata
			breach.UpdatedAt,
		).WillReturnResult(sqlmock.NewResult(0, 1))

	err = store.UpdateBreach(ctx, breach)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBreachPgStore_GetNotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	store := NewBreachPgStore(db)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT .+ FROM breach_records WHERE id = \$1`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(nil))

	_, err = store.GetBreach(ctx, uuid.New())
	assert.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}
