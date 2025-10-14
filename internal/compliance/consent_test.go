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

// Mock database for consent testing
type mockConsentDB struct {
	mu       sync.Mutex
	purposes map[string]*ConsentPurpose
	consents map[string]*ConsentRecord // key: userID+purpose
	audit    []*ConsentAuditEntry
}

func newMockConsentDB() *mockConsentDB {
	return &mockConsentDB{
		purposes: make(map[string]*ConsentPurpose),
		consents: make(map[string]*ConsentRecord),
		audit:    []*ConsentAuditEntry{},
	}
}

func (m *mockConsentDB) CreateConsentPurpose(ctx context.Context, purpose *ConsentPurpose) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purposes[purpose.Name] = purpose
	return nil
}

func (m *mockConsentDB) GetConsentPurpose(ctx context.Context, name string) (*ConsentPurpose, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	purpose, ok := m.purposes[name]
	if !ok {
		return nil, ErrNotFound
	}
	return purpose, nil
}

func (m *mockConsentDB) ListConsentPurposes(ctx context.Context) ([]*ConsentPurpose, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*ConsentPurpose
	for _, p := range m.purposes {
		result = append(result, p)
	}
	return result, nil
}

func (m *mockConsentDB) CreateConsent(ctx context.Context, consent *ConsentRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := consent.UserID.String() + ":" + consent.Purpose
	m.consents[key] = consent
	return nil
}

func (m *mockConsentDB) UpdateConsent(ctx context.Context, consent *ConsentRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := consent.UserID.String() + ":" + consent.Purpose
	m.consents[key] = consent
	return nil
}

func (m *mockConsentDB) GetConsent(ctx context.Context, userID uuid.UUID, purpose string) (*ConsentRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := userID.String() + ":" + purpose
	consent, ok := m.consents[key]
	if !ok {
		return nil, ErrNotFound
	}
	return consent, nil
}

func (m *mockConsentDB) ListUserConsents(ctx context.Context, userID uuid.UUID) ([]*ConsentRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*ConsentRecord
	for _, consent := range m.consents {
		if consent.UserID == userID {
			result = append(result, consent)
		}
	}
	return result, nil
}

func (m *mockConsentDB) CreateConsentAudit(ctx context.Context, entry *ConsentAuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, entry)
	return nil
}

func (m *mockConsentDB) GetConsentHistory(ctx context.Context, userID uuid.UUID) ([]*ConsentAuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*ConsentAuditEntry
	for _, entry := range m.audit {
		if entry.UserID == userID {
			result = append(result, entry)
		}
	}
	return result, nil
}

func setupConsentTest() (*mockConsentDB, *ConsentService) {
	db := newMockConsentDB()

	// Add test purposes
	db.purposes[ConsentPurposeMarketing] = &ConsentPurpose{
		ID:          uuid.New(),
		Name:        ConsentPurposeMarketing,
		Description: "Marketing communications",
		Required:    false,
		LegalBasis:  "consent",
		CreatedAt:   time.Now(),
	}

	db.purposes[ConsentPurposeAnalytics] = &ConsentPurpose{
		ID:          uuid.New(),
		Name:        ConsentPurposeAnalytics,
		Description: "Analytics and performance",
		Required:    false,
		LegalBasis:  "legitimate_interest",
		CreatedAt:   time.Now(),
	}

	service := NewConsentService(db, zap.NewNop())
	return db, service
}

func TestConsentService_GrantConsent(t *testing.T) {
	_, service := setupConsentTest()
	userID := uuid.New()

	t.Run("grants new consent successfully", func(t *testing.T) {
		req := &ConsentRequest{
			UserID:       userID,
			Purpose:      ConsentPurposeMarketing,
			Granted:      true,
			Method:       ConsentMethodUI,
			IPAddress:    "192.168.1.1",
			UserAgent:    "Mozilla/5.0",
			TermsVersion: "1.0",
		}

		consent, err := service.GrantConsent(context.Background(), req)
		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, consent.ID)
		assert.Equal(t, userID, consent.UserID)
		assert.Equal(t, ConsentPurposeMarketing, consent.Purpose)
		assert.True(t, consent.Granted)
		assert.NotNil(t, consent.GrantedAt)
		assert.Nil(t, consent.WithdrawnAt)
	})

	t.Run("updates existing consent", func(t *testing.T) {
		// First grant
		req := &ConsentRequest{
			UserID:    userID,
			Purpose:   ConsentPurposeAnalytics,
			Granted:   true,
			Method:    ConsentMethodUI,
			IPAddress: "192.168.1.1",
		}
		_, err := service.GrantConsent(context.Background(), req)
		require.NoError(t, err)

		// Then withdraw
		req.Granted = false
		consent, err := service.GrantConsent(context.Background(), req)
		require.NoError(t, err)
		assert.False(t, consent.Granted)
		assert.NotNil(t, consent.WithdrawnAt)
	})

	t.Run("validates user ID", func(t *testing.T) {
		req := &ConsentRequest{
			UserID:  uuid.Nil,
			Purpose: ConsentPurposeMarketing,
			Granted: true,
		}
		_, err := service.GrantConsent(context.Background(), req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "user_id required")
	})

	t.Run("validates purpose", func(t *testing.T) {
		req := &ConsentRequest{
			UserID:  userID,
			Purpose: "",
			Granted: true,
		}
		_, err := service.GrantConsent(context.Background(), req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "purpose required")
	})

	t.Run("validates purpose exists", func(t *testing.T) {
		req := &ConsentRequest{
			UserID:  userID,
			Purpose: "invalid_purpose",
			Granted: true,
		}
		_, err := service.GrantConsent(context.Background(), req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid purpose")
	})

	t.Run("creates audit entry", func(t *testing.T) {
		db, svc := setupConsentTest()
		req := &ConsentRequest{
			UserID:    userID,
			Purpose:   ConsentPurposeMarketing,
			Granted:   true,
			Method:    ConsentMethodUI,
			IPAddress: "192.168.1.1",
		}
		_, err := svc.GrantConsent(context.Background(), req)
		require.NoError(t, err)

		db.mu.Lock()
		auditCount := len(db.audit)
		db.mu.Unlock()

		assert.Greater(t, auditCount, 0)
	})
}

func TestConsentService_WithdrawConsent(t *testing.T) {
	db, service := setupConsentTest()
	userID := uuid.New()

	// First grant consent
	req := &ConsentRequest{
		UserID:    userID,
		Purpose:   ConsentPurposeMarketing,
		Granted:   true,
		Method:    ConsentMethodUI,
		IPAddress: "192.168.1.1",
	}
	_, err := service.GrantConsent(context.Background(), req)
	require.NoError(t, err)

	t.Run("withdraws consent successfully", func(t *testing.T) {
		withdrawReq := &ConsentRequest{
			Method:    ConsentMethodUI,
			IPAddress: "192.168.1.1",
		}
		err := service.WithdrawConsent(context.Background(), userID, ConsentPurposeMarketing, withdrawReq)
		require.NoError(t, err)

		// Verify consent is withdrawn
		consent, err := db.GetConsent(context.Background(), userID, ConsentPurposeMarketing)
		require.NoError(t, err)
		assert.False(t, consent.Granted)
		assert.NotNil(t, consent.WithdrawnAt)
	})

	t.Run("validates user ID", func(t *testing.T) {
		err := service.WithdrawConsent(context.Background(), uuid.Nil, ConsentPurposeMarketing, &ConsentRequest{})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "user_id required")
	})

	t.Run("validates purpose", func(t *testing.T) {
		err := service.WithdrawConsent(context.Background(), userID, "", &ConsentRequest{})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "purpose required")
	})

	t.Run("returns error for non-existent consent", func(t *testing.T) {
		err := service.WithdrawConsent(context.Background(), uuid.New(), ConsentPurposeMarketing, &ConsentRequest{})
		assert.Error(t, err)
	})
}

func TestConsentService_GetConsentStatus(t *testing.T) {
	_, service := setupConsentTest()
	userID := uuid.New()

	// Grant multiple consents
	purposes := []string{ConsentPurposeMarketing, ConsentPurposeAnalytics}
	for _, purpose := range purposes {
		req := &ConsentRequest{
			UserID:    userID,
			Purpose:   purpose,
			Granted:   true,
			Method:    ConsentMethodUI,
			IPAddress: "192.168.1.1",
		}
		_, err := service.GrantConsent(context.Background(), req)
		require.NoError(t, err)
	}

	t.Run("retrieves all user consents", func(t *testing.T) {
		status, err := service.GetConsentStatus(context.Background(), userID)
		require.NoError(t, err)
		assert.Equal(t, userID, status.UserID)
		assert.Len(t, status.Consents, 2)
		assert.Contains(t, status.Consents, ConsentPurposeMarketing)
		assert.Contains(t, status.Consents, ConsentPurposeAnalytics)
	})

	t.Run("validates user ID", func(t *testing.T) {
		_, err := service.GetConsentStatus(context.Background(), uuid.Nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "user_id required")
	})

	t.Run("returns empty status for user with no consents", func(t *testing.T) {
		status, err := service.GetConsentStatus(context.Background(), uuid.New())
		require.NoError(t, err)
		assert.Empty(t, status.Consents)
	})
}

func TestConsentService_CheckConsent(t *testing.T) {
	_, service := setupConsentTest()
	userID := uuid.New()

	t.Run("returns true for granted consent", func(t *testing.T) {
		req := &ConsentRequest{
			UserID:    userID,
			Purpose:   ConsentPurposeMarketing,
			Granted:   true,
			Method:    ConsentMethodUI,
			IPAddress: "192.168.1.1",
		}
		_, err := service.GrantConsent(context.Background(), req)
		require.NoError(t, err)

		granted, err := service.CheckConsent(context.Background(), userID, ConsentPurposeMarketing)
		require.NoError(t, err)
		assert.True(t, granted)
	})

	t.Run("returns false for withdrawn consent", func(t *testing.T) {
		// First grant
		req := &ConsentRequest{
			UserID:    userID,
			Purpose:   ConsentPurposeAnalytics,
			Granted:   true,
			Method:    ConsentMethodUI,
			IPAddress: "192.168.1.1",
		}
		_, err := service.GrantConsent(context.Background(), req)
		require.NoError(t, err)

		// Then withdraw
		err = service.WithdrawConsent(context.Background(), userID, ConsentPurposeAnalytics, &ConsentRequest{
			Method:    ConsentMethodUI,
			IPAddress: "192.168.1.1",
		})
		require.NoError(t, err)

		granted, err := service.CheckConsent(context.Background(), userID, ConsentPurposeAnalytics)
		require.NoError(t, err)
		assert.False(t, granted)
	})

	t.Run("returns false for non-existent consent", func(t *testing.T) {
		granted, err := service.CheckConsent(context.Background(), uuid.New(), ConsentPurposeMarketing)
		require.NoError(t, err)
		assert.False(t, granted)
	})
}

func TestConsentService_GetConsentHistory(t *testing.T) {
	_, service := setupConsentTest()
	userID := uuid.New()

	// Create some consent actions
	req := &ConsentRequest{
		UserID:    userID,
		Purpose:   ConsentPurposeMarketing,
		Granted:   true,
		Method:    ConsentMethodUI,
		IPAddress: "192.168.1.1",
	}
	_, err := service.GrantConsent(context.Background(), req)
	require.NoError(t, err)

	// Withdraw
	err = service.WithdrawConsent(context.Background(), userID, ConsentPurposeMarketing, &ConsentRequest{
		Method:    ConsentMethodUI,
		IPAddress: "192.168.1.1",
	})
	require.NoError(t, err)

	t.Run("retrieves consent history", func(t *testing.T) {
		history, err := service.GetConsentHistory(context.Background(), userID)
		require.NoError(t, err)
		assert.Len(t, history, 2) // Grant + Withdraw
	})

	t.Run("validates user ID", func(t *testing.T) {
		_, err := service.GetConsentHistory(context.Background(), uuid.Nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "user_id required")
	})
}

func TestConsentService_ListPurposes(t *testing.T) {
	_, service := setupConsentTest()

	t.Run("lists all purposes", func(t *testing.T) {
		purposes, err := service.ListPurposes(context.Background())
		require.NoError(t, err)
		assert.Len(t, purposes, 2) // Marketing + Analytics from setup
	})
}

func TestConsentService_VerifyAge(t *testing.T) {
	_, service := setupConsentTest()

	tests := []struct {
		name       string
		birthDate  time.Time
		minimumAge int
		expected   bool
	}{
		{
			name:       "adult over 18",
			birthDate:  time.Now().AddDate(-25, 0, 0),
			minimumAge: 18,
			expected:   true,
		},
		{
			name:       "exactly 18",
			birthDate:  time.Now().AddDate(-18, 0, 0),
			minimumAge: 18,
			expected:   true,
		},
		{
			name:       "under 18",
			birthDate:  time.Now().AddDate(-15, 0, 0),
			minimumAge: 18,
			expected:   false,
		},
		{
			name:       "child under 13",
			birthDate:  time.Now().AddDate(-10, 0, 0),
			minimumAge: 13,
			expected:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := service.VerifyAge(tt.birthDate, tt.minimumAge)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestConsentService_RequiresParentalConsent(t *testing.T) {
	_, service := setupConsentTest()

	tests := []struct {
		name      string
		birthDate time.Time
		expected  bool
	}{
		{
			name:      "adult 18+",
			birthDate: time.Now().AddDate(-20, 0, 0),
			expected:  false,
		},
		{
			name:      "exactly 16",
			birthDate: time.Now().AddDate(-16, 0, 0),
			expected:  false,
		},
		{
			name:      "15 years old",
			birthDate: time.Now().AddDate(-15, 0, 0),
			expected:  true,
		},
		{
			name:      "child 12 years old",
			birthDate: time.Now().AddDate(-12, 0, 0),
			expected:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := service.RequiresParentalConsent(tt.birthDate)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestConsentService_ConcurrentAccess(t *testing.T) {
	db, service := setupConsentTest()
	userID := uuid.New()

	t.Run("handles concurrent consent grants", func(t *testing.T) {
		var wg sync.WaitGroup
		numGoroutines := 10

		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				req := &ConsentRequest{
					UserID:    userID,
					Purpose:   ConsentPurposeMarketing,
					Granted:   i%2 == 0, // Alternate grant/deny
					Method:    ConsentMethodAPI,
					IPAddress: "192.168.1.1",
				}
				_, err := service.GrantConsent(context.Background(), req)
				assert.NoError(t, err)
			}(i)
		}

		wg.Wait()

		// Verify final state is consistent
		consent, err := db.GetConsent(context.Background(), userID, ConsentPurposeMarketing)
		require.NoError(t, err)
		assert.NotNil(t, consent)
	})
}
