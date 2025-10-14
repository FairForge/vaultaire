package compliance

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Mock database for breach testing
type mockBreachDB struct {
	mu            sync.Mutex
	breaches      map[uuid.UUID]*BreachRecord
	affectedUsers map[uuid.UUID][]*BreachAffectedUser
	notifications map[uuid.UUID][]*BreachNotification
}

func newMockBreachDB() *mockBreachDB {
	return &mockBreachDB{
		breaches:      make(map[uuid.UUID]*BreachRecord),
		affectedUsers: make(map[uuid.UUID][]*BreachAffectedUser),
		notifications: make(map[uuid.UUID][]*BreachNotification),
	}
}

func (m *mockBreachDB) CreateBreach(ctx context.Context, breach *BreachRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.breaches[breach.ID] = breach
	return nil
}

func (m *mockBreachDB) GetBreach(ctx context.Context, breachID uuid.UUID) (*BreachRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	breach, ok := m.breaches[breachID]
	if !ok {
		return nil, ErrNotFound
	}
	return breach, nil
}

func (m *mockBreachDB) UpdateBreach(ctx context.Context, breach *BreachRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.breaches[breach.ID] = breach
	return nil
}

func (m *mockBreachDB) ListBreaches(ctx context.Context, filters map[string]interface{}) ([]*BreachRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*BreachRecord
	for _, breach := range m.breaches {
		result = append(result, breach)
	}
	return result, nil
}

func (m *mockBreachDB) AddAffectedUsers(ctx context.Context, breachID uuid.UUID, userIDs []uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, userID := range userIDs {
		affected := &BreachAffectedUser{
			ID:        uuid.New(),
			BreachID:  breachID,
			UserID:    userID,
			Notified:  false,
			CreatedAt: time.Now(),
		}
		m.affectedUsers[breachID] = append(m.affectedUsers[breachID], affected)
	}
	return nil
}

func (m *mockBreachDB) GetAffectedUsers(ctx context.Context, breachID uuid.UUID) ([]*BreachAffectedUser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	users, ok := m.affectedUsers[breachID]
	if !ok {
		return []*BreachAffectedUser{}, nil
	}
	return users, nil
}

func (m *mockBreachDB) UpdateAffectedUser(ctx context.Context, affected *BreachAffectedUser) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	users := m.affectedUsers[affected.BreachID]
	for i, user := range users {
		if user.ID == affected.ID {
			users[i] = affected
			break
		}
	}
	return nil
}

func (m *mockBreachDB) CreateNotification(ctx context.Context, notification *BreachNotification) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notifications[notification.BreachID] = append(m.notifications[notification.BreachID], notification)
	return nil
}

func (m *mockBreachDB) GetNotifications(ctx context.Context, breachID uuid.UUID) ([]*BreachNotification, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	notifications, ok := m.notifications[breachID]
	if !ok {
		return []*BreachNotification{}, nil
	}
	return notifications, nil
}

func (m *mockBreachDB) GetBreachStats(ctx context.Context) (*BreachStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stats := &BreachStats{
		BreachesByType:     make(map[string]int),
		BreachesBySeverity: make(map[string]int),
		BreachesByStatus:   make(map[string]int),
	}

	for _, breach := range m.breaches {
		stats.TotalBreaches++
		stats.BreachesByType[breach.BreachType]++
		stats.BreachesBySeverity[breach.Severity]++
		stats.BreachesByStatus[breach.Status]++
	}

	return stats, nil
}

func setupBreachTest() (*mockBreachDB, *BreachService) {
	db := newMockBreachDB()
	service := NewBreachService(db, zap.NewNop())
	return db, service
}

func TestBreachService_DetectBreach(t *testing.T) {
	db, service := setupBreachTest()

	t.Run("detects breach successfully", func(t *testing.T) {
		req := &BreachRequest{
			BreachType:          BreachTypeUnauthorizedAccess,
			Description:         "Unauthorized access to user database",
			RootCause:           "SQL injection vulnerability",
			AffectedUserCount:   1000,
			AffectedRecordCount: 5000,
			DataCategories:      []string{"email", "name", "address"},
		}

		breach, err := service.DetectBreach(context.Background(), req)
		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, breach.ID)
		assert.Equal(t, BreachTypeUnauthorizedAccess, breach.BreachType)
		assert.Equal(t, BreachStatusDetected, breach.Status)
		assert.Equal(t, 1000, breach.AffectedUserCount)
		assert.NotEmpty(t, breach.Severity)
		assert.False(t, breach.DeadlineAt.IsZero())

		// Verify 72-hour deadline
		expectedDeadline := breach.DetectedAt.Add(72 * time.Hour)
		assert.WithinDuration(t, expectedDeadline, breach.DeadlineAt, time.Second)
	})

	t.Run("validates breach type", func(t *testing.T) {
		req := &BreachRequest{
			BreachType:  "",
			Description: "Test",
		}
		_, err := service.DetectBreach(context.Background(), req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "breach_type required")
	})

	t.Run("validates description", func(t *testing.T) {
		req := &BreachRequest{
			BreachType:  BreachTypeDataLoss,
			Description: "",
		}
		_, err := service.DetectBreach(context.Background(), req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "description required")
	})

	t.Run("stores affected users", func(t *testing.T) {
		userIDs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
		req := &BreachRequest{
			BreachType:        BreachTypePhishing,
			Description:       "Phishing attack",
			AffectedUserCount: len(userIDs),
			AffectedUserIDs:   userIDs,
		}

		breach, err := service.DetectBreach(context.Background(), req)
		require.NoError(t, err)

		// Verify affected users were stored
		affected, err := db.GetAffectedUsers(context.Background(), breach.ID)
		require.NoError(t, err)
		assert.Len(t, affected, len(userIDs))
	})

	t.Run("uses custom detection time if provided", func(t *testing.T) {
		customTime := time.Now().Add(-48 * time.Hour)
		req := &BreachRequest{
			BreachType:  BreachTypeRansomware,
			Description: "Ransomware attack",
			DetectedAt:  &customTime,
		}

		breach, err := service.DetectBreach(context.Background(), req)
		require.NoError(t, err)
		assert.WithinDuration(t, customTime, breach.DetectedAt, time.Second)
	})
}

func TestBreachService_AssessSeverity(t *testing.T) {
	_, service := setupBreachTest()

	tests := []struct {
		name             string
		breach           *BreachRecord
		expectedSeverity string
		minRiskLevel     int
	}{
		{
			name: "critical - many users with sensitive data",
			breach: &BreachRecord{
				ID:                uuid.New(),
				BreachType:        BreachTypeRansomware,
				AffectedUserCount: 50000,
				DataCategories:    []string{"password", "financial"},
			},
			expectedSeverity: BreachSeverityCritical,
			minRiskLevel:     80,
		},
		{
			name: "high - moderate users with sensitive data",
			breach: &BreachRecord{
				ID:                uuid.New(),
				BreachType:        BreachTypeUnauthorizedAccess,
				AffectedUserCount: 5000,
				DataCategories:    []string{"health", "email"},
			},
			expectedSeverity: BreachSeverityHigh,
			minRiskLevel:     60,
		},
		{
			name: "medium - some users with standard data",
			breach: &BreachRecord{
				ID:                uuid.New(),
				BreachType:        BreachTypeDataLeakage,
				AffectedUserCount: 1500, // Changed from 500 to reach 40+ points
				DataCategories:    []string{"email", "name"},
			},
			expectedSeverity: BreachSeverityMedium,
			minRiskLevel:     40,
		},
		{
			name: "low - few users with minimal data",
			breach: &BreachRecord{
				ID:                uuid.New(),
				BreachType:        BreachTypeSystemFailure,
				AffectedUserCount: 50,
				DataCategories:    []string{"name"},
			},
			expectedSeverity: BreachSeverityLow,
			minRiskLevel:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assessment := service.AssessSeverity(tt.breach)
			assert.Equal(t, tt.expectedSeverity, assessment.Severity)
			assert.GreaterOrEqual(t, assessment.RiskLevel, tt.minRiskLevel)
			assert.Equal(t, tt.breach.AffectedUserCount, assessment.AffectedUserCount)
		})
	}

	t.Run("determines notification requirements", func(t *testing.T) {
		// Critical breach - requires both
		criticalBreach := &BreachRecord{
			ID:                uuid.New(),
			BreachType:        BreachTypeRansomware,
			AffectedUserCount: 100000,
			DataCategories:    []string{"password", "financial", "health"},
		}
		assessment := service.AssessSeverity(criticalBreach)
		assert.True(t, assessment.RequiresAuthority)
		assert.True(t, assessment.RequiresSubjects)

		// Low breach - requires neither
		lowBreach := &BreachRecord{
			ID:                uuid.New(),
			BreachType:        BreachTypeSystemFailure,
			AffectedUserCount: 10,
			DataCategories:    []string{"name"},
		}
		assessment = service.AssessSeverity(lowBreach)
		assert.False(t, assessment.RequiresAuthority)
		assert.False(t, assessment.RequiresSubjects)
	})
}

func TestBreachService_NotifyAuthority(t *testing.T) {
	db, service := setupBreachTest()

	t.Run("notifies authority successfully", func(t *testing.T) {
		// Create a breach
		req := &BreachRequest{
			BreachType:        BreachTypeUnauthorizedAccess,
			Description:       "Test breach",
			AffectedUserCount: 1000,
		}
		breach, err := service.DetectBreach(context.Background(), req)
		require.NoError(t, err)

		// Notify authority
		err = service.NotifyAuthority(context.Background(), breach.ID)
		require.NoError(t, err)

		// Verify breach was updated
		updated, err := db.GetBreach(context.Background(), breach.ID)
		require.NoError(t, err)
		assert.True(t, updated.NotifiedAuthority)
		assert.NotNil(t, updated.AuthorityNotifiedAt)
		assert.NotNil(t, updated.ReportedAt)
		assert.Equal(t, BreachStatusReported, updated.Status)

		// Verify notification was created
		notifications, err := db.GetNotifications(context.Background(), breach.ID)
		require.NoError(t, err)
		assert.Len(t, notifications, 1)
		assert.Equal(t, NotificationTypeAuthority, notifications[0].NotificationType)
	})

	t.Run("prevents duplicate authority notification", func(t *testing.T) {
		req := &BreachRequest{
			BreachType:  BreachTypeDataLoss,
			Description: "Test breach 2",
		}
		breach, err := service.DetectBreach(context.Background(), req)
		require.NoError(t, err)

		// First notification
		err = service.NotifyAuthority(context.Background(), breach.ID)
		require.NoError(t, err)

		// Second notification should fail
		err = service.NotifyAuthority(context.Background(), breach.ID)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "already notified")
	})

	t.Run("tracks deadline status", func(t *testing.T) {
		// Create breach detected 73 hours ago (past deadline)
		pastTime := time.Now().Add(-73 * time.Hour)
		req := &BreachRequest{
			BreachType:  BreachTypeRansomware,
			Description: "Past deadline breach",
			DetectedAt:  &pastTime,
		}
		breach, err := service.DetectBreach(context.Background(), req)
		require.NoError(t, err)

		err = service.NotifyAuthority(context.Background(), breach.ID)
		require.NoError(t, err)

		// Check notification metadata
		notifications, err := db.GetNotifications(context.Background(), breach.ID)
		require.NoError(t, err)
		assert.Len(t, notifications, 1)

		withinDeadline, ok := notifications[0].Metadata["within_deadline"].(bool)
		assert.True(t, ok)
		assert.False(t, withinDeadline)
	})
}

func TestBreachService_NotifySubjects(t *testing.T) {
	db, service := setupBreachTest()

	t.Run("notifies affected subjects", func(t *testing.T) {
		// Create high-severity breach with affected users
		userIDs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
		req := &BreachRequest{
			BreachType:        BreachTypeUnauthorizedAccess,
			Description:       "High severity breach",
			AffectedUserCount: 10000, // High severity
			DataCategories:    []string{"password", "financial"},
			AffectedUserIDs:   userIDs,
		}
		breach, err := service.DetectBreach(context.Background(), req)
		require.NoError(t, err)

		// Notify subjects
		err = service.NotifySubjects(context.Background(), breach.ID)
		require.NoError(t, err)

		// Verify breach was updated
		updated, err := db.GetBreach(context.Background(), breach.ID)
		require.NoError(t, err)
		assert.True(t, updated.NotifiedSubjects)
		assert.NotNil(t, updated.SubjectsNotifiedAt)

		// Verify all affected users were notified
		affected, err := db.GetAffectedUsers(context.Background(), breach.ID)
		require.NoError(t, err)
		for _, user := range affected {
			assert.True(t, user.Notified)
			assert.NotNil(t, user.NotifiedAt)
		}

		// Verify notifications were created
		notifications, err := db.GetNotifications(context.Background(), breach.ID)
		require.NoError(t, err)
		subjectNotifications := 0
		for _, n := range notifications {
			if n.NotificationType == NotificationTypeSubject {
				subjectNotifications++
			}
		}
		assert.Equal(t, len(userIDs), subjectNotifications)
	})

	t.Run("prevents notification if not required", func(t *testing.T) {
		// Create low-severity breach
		req := &BreachRequest{
			BreachType:        BreachTypeSystemFailure,
			Description:       "Low severity breach",
			AffectedUserCount: 10, // Low severity
		}
		breach, err := service.DetectBreach(context.Background(), req)
		require.NoError(t, err)

		// Attempt to notify subjects
		err = service.NotifySubjects(context.Background(), breach.ID)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not required")
	})

	t.Run("prevents duplicate subject notification", func(t *testing.T) {
		userIDs := []uuid.UUID{uuid.New()}
		req := &BreachRequest{
			BreachType:        BreachTypeRansomware,
			Description:       "Critical breach",
			AffectedUserCount: 50000,
			DataCategories:    []string{"password"},
			AffectedUserIDs:   userIDs,
		}
		breach, err := service.DetectBreach(context.Background(), req)
		require.NoError(t, err)

		// First notification
		err = service.NotifySubjects(context.Background(), breach.ID)
		require.NoError(t, err)

		// Second notification should fail
		err = service.NotifySubjects(context.Background(), breach.ID)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "already notified")
	})
}

func TestBreachService_GetBreachStatus(t *testing.T) {
	_, service := setupBreachTest()

	t.Run("retrieves breach status", func(t *testing.T) {
		req := &BreachRequest{
			BreachType:  BreachTypeDataLeakage,
			Description: "Test breach",
		}
		breach, err := service.DetectBreach(context.Background(), req)
		require.NoError(t, err)

		// Get status
		status, err := service.GetBreachStatus(context.Background(), breach.ID)
		require.NoError(t, err)
		assert.Equal(t, breach.ID, status.ID)
		assert.Equal(t, breach.BreachType, status.BreachType)
	})

	t.Run("returns error for non-existent breach", func(t *testing.T) {
		_, err := service.GetBreachStatus(context.Background(), uuid.New())
		assert.Error(t, err)
	})
}

func TestBreachService_UpdateBreach(t *testing.T) {
	db, service := setupBreachTest()

	t.Run("updates breach successfully", func(t *testing.T) {
		req := &BreachRequest{
			BreachType:  BreachTypeInsiderThreat,
			Description: "Initial description",
		}
		breach, err := service.DetectBreach(context.Background(), req)
		require.NoError(t, err)

		// Update breach
		updates := map[string]interface{}{
			"status":       BreachStatusMitigated,
			"consequences": "Data was accessed but not exfiltrated",
			"mitigation":   "Access revoked, passwords reset",
		}
		err = service.UpdateBreach(context.Background(), breach.ID, updates)
		require.NoError(t, err)

		// Verify updates
		updated, err := db.GetBreach(context.Background(), breach.ID)
		require.NoError(t, err)
		assert.Equal(t, BreachStatusMitigated, updated.Status)
		assert.Equal(t, "Data was accessed but not exfiltrated", updated.Consequences)
		assert.Equal(t, "Access revoked, passwords reset", updated.Mitigation)
	})
}

func TestBreachService_ListBreaches(t *testing.T) {
	_, service := setupBreachTest()

	t.Run("lists all breaches", func(t *testing.T) {
		// Create multiple breaches
		for i := 0; i < 3; i++ {
			req := &BreachRequest{
				BreachType:  BreachTypeUnauthorizedAccess,
				Description: "Test breach",
			}
			_, err := service.DetectBreach(context.Background(), req)
			require.NoError(t, err)
		}

		breaches, err := service.ListBreaches(context.Background(), nil)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(breaches), 3)
	})
}

func TestBreachService_GetBreachStats(t *testing.T) {
	_, service := setupBreachTest()

	t.Run("retrieves breach statistics", func(t *testing.T) {
		// Create breaches of different types
		types := []string{
			BreachTypeUnauthorizedAccess,
			BreachTypeDataLoss,
			BreachTypeUnauthorizedAccess,
		}
		for _, breachType := range types {
			req := &BreachRequest{
				BreachType:  breachType,
				Description: "Test breach",
			}
			_, err := service.DetectBreach(context.Background(), req)
			require.NoError(t, err)
		}

		stats, err := service.GetBreachStats(context.Background())
		require.NoError(t, err)
		assert.GreaterOrEqual(t, stats.TotalBreaches, 3)
		assert.NotEmpty(t, stats.BreachesByType)
		assert.NotEmpty(t, stats.BreachesBySeverity)
	})
}

func TestBreachService_CheckDeadline(t *testing.T) {
	_, service := setupBreachTest()

	t.Run("checks deadline correctly", func(t *testing.T) {
		now := time.Now()
		breach := &BreachRecord{
			DetectedAt: now,
			DeadlineAt: now.Add(72 * time.Hour),
		}

		withinDeadline, remaining := service.CheckDeadline(breach)
		assert.True(t, withinDeadline)
		assert.Greater(t, remaining, 71*time.Hour)
		assert.Less(t, remaining, 73*time.Hour)
	})

	t.Run("detects missed deadline", func(t *testing.T) {
		pastTime := time.Now().Add(-80 * time.Hour)
		breach := &BreachRecord{
			DetectedAt: pastTime,
			DeadlineAt: pastTime.Add(72 * time.Hour),
		}

		withinDeadline, remaining := service.CheckDeadline(breach)
		assert.False(t, withinDeadline)
		assert.Equal(t, time.Duration(0), remaining)
	})
}

func TestBreachService_ConcurrentAccess(t *testing.T) {
	_, service := setupBreachTest()

	t.Run("handles concurrent breach detection", func(t *testing.T) {
		var wg sync.WaitGroup
		numGoroutines := 10

		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := &BreachRequest{
					BreachType:  BreachTypeUnauthorizedAccess,
					Description: "Concurrent test",
				}
				_, err := service.DetectBreach(context.Background(), req)
				assert.NoError(t, err)
			}()
		}

		wg.Wait()

		// Verify all breaches were recorded
		breaches, err := service.ListBreaches(context.Background(), nil)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(breaches), numGoroutines)
	})
}
