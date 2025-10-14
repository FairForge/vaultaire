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

// Mock database for ROPA testing
type mockROPADB struct {
	mu             sync.Mutex
	activities     map[uuid.UUID]*ProcessingActivity
	dataCategories map[uuid.UUID][]DataCategory
	dataSubjects   map[uuid.UUID][]DataSubjectCategory
	recipients     map[uuid.UUID][]Recipient
	reviews        map[uuid.UUID][]*ActivityReview
}

func newMockROPADB() *mockROPADB {
	return &mockROPADB{
		activities:     make(map[uuid.UUID]*ProcessingActivity),
		dataCategories: make(map[uuid.UUID][]DataCategory),
		dataSubjects:   make(map[uuid.UUID][]DataSubjectCategory),
		recipients:     make(map[uuid.UUID][]Recipient),
		reviews:        make(map[uuid.UUID][]*ActivityReview),
	}
}

func (m *mockROPADB) CreateActivity(ctx context.Context, activity *ProcessingActivity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activities[activity.ID] = activity
	return nil
}

func (m *mockROPADB) GetActivity(ctx context.Context, activityID uuid.UUID) (*ProcessingActivity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	activity, ok := m.activities[activityID]
	if !ok {
		return nil, ErrNotFound
	}
	return activity, nil
}

func (m *mockROPADB) UpdateActivity(ctx context.Context, activity *ProcessingActivity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activities[activity.ID] = activity
	return nil
}

func (m *mockROPADB) DeleteActivity(ctx context.Context, activityID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.activities, activityID)
	return nil
}

func (m *mockROPADB) ListActivities(ctx context.Context, filters map[string]interface{}) ([]*ProcessingActivity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*ProcessingActivity
	for _, activity := range m.activities {
		// Apply status filter if provided
		if status, ok := filters["status"].(string); ok {
			if activity.Status != status {
				continue
			}
		}
		result = append(result, activity)
	}
	return result, nil
}

func (m *mockROPADB) AddDataCategories(ctx context.Context, activityID uuid.UUID, categories []DataCategory) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dataCategories[activityID] = categories
	return nil
}

func (m *mockROPADB) GetDataCategories(ctx context.Context, activityID uuid.UUID) ([]DataCategory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	categories, ok := m.dataCategories[activityID]
	if !ok {
		return []DataCategory{}, nil
	}
	return categories, nil
}

func (m *mockROPADB) AddDataSubjects(ctx context.Context, activityID uuid.UUID, subjects []DataSubjectCategory) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dataSubjects[activityID] = subjects
	return nil
}

func (m *mockROPADB) GetDataSubjects(ctx context.Context, activityID uuid.UUID) ([]DataSubjectCategory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	subjects, ok := m.dataSubjects[activityID]
	if !ok {
		return []DataSubjectCategory{}, nil
	}
	return subjects, nil
}

func (m *mockROPADB) AddRecipients(ctx context.Context, activityID uuid.UUID, recipients []Recipient) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recipients[activityID] = recipients
	return nil
}

func (m *mockROPADB) GetRecipients(ctx context.Context, activityID uuid.UUID) ([]Recipient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	recipients, ok := m.recipients[activityID]
	if !ok {
		return []Recipient{}, nil
	}
	return recipients, nil
}

func (m *mockROPADB) CreateReview(ctx context.Context, review *ActivityReview) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reviews[review.ActivityID] = append(m.reviews[review.ActivityID], review)
	return nil
}

func (m *mockROPADB) GetReviews(ctx context.Context, activityID uuid.UUID) ([]*ActivityReview, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	reviews, ok := m.reviews[activityID]
	if !ok {
		return []*ActivityReview{}, nil
	}
	return reviews, nil
}

func (m *mockROPADB) GetROPAStats(ctx context.Context) (*ROPAStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stats := &ROPAStats{
		ActivitiesByLegalBasis: make(map[string]int),
		ActivitiesByStatus:     make(map[string]int),
	}

	for _, activity := range m.activities {
		stats.TotalActivities++
		switch activity.Status {
		case ActivityStatusActive:
			stats.ActiveActivities++
		case ActivityStatusInactive:
			stats.InactiveActivities++
		}
		stats.ActivitiesByLegalBasis[activity.LegalBasis]++
		stats.ActivitiesByStatus[activity.Status]++

		// Check if needs review (> 365 days)
		if activity.LastReviewedAt == nil {
			stats.ActivitiesNeedingReview++
		} else {
			daysSinceReview := time.Since(*activity.LastReviewedAt).Hours() / 24
			if daysSinceReview > 365 {
				stats.ActivitiesNeedingReview++
			}
		}
	}

	return stats, nil
}

func setupROPATest() (*mockROPADB, *ROPAService) {
	db := newMockROPADB()
	service := NewROPAService(db, zap.NewNop())
	return db, service
}

func TestROPAService_CreateActivity(t *testing.T) {
	db, service := setupROPATest()

	t.Run("creates activity successfully", func(t *testing.T) {
		req := &ProcessingActivityRequest{
			Name:                  "User Registration",
			Description:           "Processing user registration data",
			Purpose:               "Account creation and management",
			LegalBasis:            LegalBasisContract,
			ControllerName:        "Fairforge LLC",
			ControllerContact:     "dpo@fairforge.io",
			RetentionPeriod:       "Until account deletion + 30 days",
			SecurityMeasures:      "Encryption at rest, TLS in transit",
			DataCategories:        []string{"email", "name", "password"},
			DataSubjectCategories: []string{"customers", "users"},
		}

		activity, err := service.CreateActivity(context.Background(), req)
		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, activity.ID)
		assert.Equal(t, "User Registration", activity.Name)
		assert.Equal(t, LegalBasisContract, activity.LegalBasis)
		assert.Equal(t, ActivityStatusActive, activity.Status)

		// Verify data categories were added
		categories, err := db.GetDataCategories(context.Background(), activity.ID)
		require.NoError(t, err)
		assert.Len(t, categories, 3)

		// Verify data subjects were added
		subjects, err := db.GetDataSubjects(context.Background(), activity.ID)
		require.NoError(t, err)
		assert.Len(t, subjects, 2)
	})

	t.Run("validates name", func(t *testing.T) {
		req := &ProcessingActivityRequest{
			Name:       "",
			Purpose:    "Test",
			LegalBasis: LegalBasisConsent,
		}
		_, err := service.CreateActivity(context.Background(), req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "name required")
	})

	t.Run("validates purpose", func(t *testing.T) {
		req := &ProcessingActivityRequest{
			Name:       "Test",
			Purpose:    "",
			LegalBasis: LegalBasisConsent,
		}
		_, err := service.CreateActivity(context.Background(), req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "purpose required")
	})

	t.Run("validates legal basis", func(t *testing.T) {
		req := &ProcessingActivityRequest{
			Name:       "Test",
			Purpose:    "Test purpose",
			LegalBasis: "",
		}
		_, err := service.CreateActivity(context.Background(), req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "legal basis required")
	})

	t.Run("validates legal basis value", func(t *testing.T) {
		req := &ProcessingActivityRequest{
			Name:       "Test",
			Purpose:    "Test purpose",
			LegalBasis: "invalid_basis",
		}
		_, err := service.CreateActivity(context.Background(), req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid legal basis")
	})

	t.Run("creates activity with recipients", func(t *testing.T) {
		req := &ProcessingActivityRequest{
			Name:       "Payment Processing",
			Purpose:    "Process customer payments",
			LegalBasis: LegalBasisContract,
			Recipients: []RecipientRequest{
				{
					Name:       "Stripe",
					Type:       "processor",
					Purpose:    "Payment processing",
					Country:    "USA",
					Safeguards: "Standard Contractual Clauses",
				},
			},
		}

		activity, err := service.CreateActivity(context.Background(), req)
		require.NoError(t, err)

		// Verify recipients were added
		recipients, err := db.GetRecipients(context.Background(), activity.ID)
		require.NoError(t, err)
		assert.Len(t, recipients, 1)
		assert.Equal(t, "Stripe", recipients[0].Name)
	})
}

func TestROPAService_GetActivity(t *testing.T) {
	_, service := setupROPATest()

	t.Run("retrieves activity with details", func(t *testing.T) {
		req := &ProcessingActivityRequest{
			Name:                  "Test Activity",
			Purpose:               "Testing",
			LegalBasis:            LegalBasisConsent,
			DataCategories:        []string{"email"},
			DataSubjectCategories: []string{"users"},
		}

		created, err := service.CreateActivity(context.Background(), req)
		require.NoError(t, err)

		// Retrieve it
		retrieved, err := service.GetActivity(context.Background(), created.ID)
		require.NoError(t, err)
		assert.Equal(t, created.ID, retrieved.ID)
		assert.Equal(t, created.Name, retrieved.Name)
		assert.Len(t, retrieved.DataCategories, 1)
		assert.Len(t, retrieved.DataSubjectCategories, 1)
	})

	t.Run("returns error for non-existent activity", func(t *testing.T) {
		_, err := service.GetActivity(context.Background(), uuid.New())
		assert.Error(t, err)
	})
}

func TestROPAService_UpdateActivity(t *testing.T) {
	_, service := setupROPATest()

	t.Run("updates activity successfully", func(t *testing.T) {
		// Create activity
		req := &ProcessingActivityRequest{
			Name:            "Original Name",
			Purpose:         "Original Purpose",
			LegalBasis:      LegalBasisConsent,
			RetentionPeriod: "1 year",
		}

		activity, err := service.CreateActivity(context.Background(), req)
		require.NoError(t, err)

		// Update it
		updates := map[string]interface{}{
			"name":             "Updated Name",
			"retention_period": "2 years",
		}

		err = service.UpdateActivity(context.Background(), activity.ID, updates)
		require.NoError(t, err)

		// Verify updates
		updated, err := service.GetActivity(context.Background(), activity.ID)
		require.NoError(t, err)
		assert.Equal(t, "Updated Name", updated.Name)
		assert.Equal(t, "2 years", updated.RetentionPeriod)
	})
}

func TestROPAService_DeleteActivity(t *testing.T) {
	_, service := setupROPATest()

	t.Run("marks activity as inactive", func(t *testing.T) {
		req := &ProcessingActivityRequest{
			Name:       "To Delete",
			Purpose:    "Testing deletion",
			LegalBasis: LegalBasisConsent,
		}

		activity, err := service.CreateActivity(context.Background(), req)
		require.NoError(t, err)
		assert.Equal(t, ActivityStatusActive, activity.Status)

		// Delete it
		err = service.DeleteActivity(context.Background(), activity.ID)
		require.NoError(t, err)

		// Verify it's inactive
		deleted, err := service.GetActivity(context.Background(), activity.ID)
		require.NoError(t, err)
		assert.Equal(t, ActivityStatusInactive, deleted.Status)
	})
}

func TestROPAService_ListActivities(t *testing.T) {
	_, service := setupROPATest()

	t.Run("lists all activities", func(t *testing.T) {
		// Create multiple activities
		for i := 0; i < 3; i++ {
			req := &ProcessingActivityRequest{
				Name:       "Activity " + string(rune(i)),
				Purpose:    "Testing",
				LegalBasis: LegalBasisConsent,
			}
			_, err := service.CreateActivity(context.Background(), req)
			require.NoError(t, err)
		}

		activities, err := service.ListActivities(context.Background(), nil)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(activities), 3)
	})

	t.Run("filters by status", func(t *testing.T) {
		// Create and delete one
		req := &ProcessingActivityRequest{
			Name:       "To Filter",
			Purpose:    "Testing",
			LegalBasis: LegalBasisConsent,
		}
		activity, err := service.CreateActivity(context.Background(), req)
		require.NoError(t, err)

		err = service.DeleteActivity(context.Background(), activity.ID)
		require.NoError(t, err)

		// List only active
		active, err := service.ListActivities(context.Background(), map[string]interface{}{
			"status": ActivityStatusActive,
		})
		require.NoError(t, err)

		// Verify deleted one is not in active list
		for _, a := range active {
			assert.NotEqual(t, activity.ID, a.ID)
		}
	})
}

func TestROPAService_ReviewActivity(t *testing.T) {
	db, service := setupROPATest()

	t.Run("creates review successfully", func(t *testing.T) {
		req := &ProcessingActivityRequest{
			Name:       "To Review",
			Purpose:    "Testing reviews",
			LegalBasis: LegalBasisConsent,
		}

		activity, err := service.CreateActivity(context.Background(), req)
		require.NoError(t, err)
		assert.Nil(t, activity.LastReviewedAt)

		// Review it
		reviewerID := uuid.New()
		err = service.ReviewActivity(context.Background(), activity.ID, reviewerID, "Annual review completed")
		require.NoError(t, err)

		// Verify review was recorded
		reviewed, err := service.GetActivity(context.Background(), activity.ID)
		require.NoError(t, err)
		assert.NotNil(t, reviewed.LastReviewedAt)
		assert.NotNil(t, reviewed.ReviewedBy)
		assert.Equal(t, reviewerID, *reviewed.ReviewedBy)

		// Verify review entry
		reviews, err := db.GetReviews(context.Background(), activity.ID)
		require.NoError(t, err)
		assert.Len(t, reviews, 1)
		assert.Equal(t, "Annual review completed", reviews[0].Notes)
	})
}

func TestROPAService_ValidateCompliance(t *testing.T) {
	_, service := setupROPATest()

	t.Run("validates complete activity", func(t *testing.T) {
		req := &ProcessingActivityRequest{
			Name:                  "Complete Activity",
			Purpose:               "Full compliance test",
			LegalBasis:            LegalBasisContract,
			RetentionPeriod:       "5 years",
			SecurityMeasures:      "Encryption, access controls",
			DataCategories:        []string{"email"},
			DataSubjectCategories: []string{"users"},
		}

		activity, err := service.CreateActivity(context.Background(), req)
		require.NoError(t, err)

		check, err := service.ValidateCompliance(context.Background(), activity.ID)
		require.NoError(t, err)
		assert.True(t, check.IsCompliant)
		assert.Empty(t, check.Issues)
	})

	t.Run("identifies missing fields", func(t *testing.T) {
		req := &ProcessingActivityRequest{
			Name:       "Incomplete Activity",
			Purpose:    "", // Missing
			LegalBasis: LegalBasisConsent,
		}

		activity, err := service.CreateActivity(context.Background(), req)
		// This should fail validation in CreateActivity
		assert.Error(t, err)
		assert.Nil(t, activity)
	})

	t.Run("warns about missing review", func(t *testing.T) {
		req := &ProcessingActivityRequest{
			Name:            "Never Reviewed",
			Purpose:         "Testing review warnings",
			LegalBasis:      LegalBasisConsent,
			RetentionPeriod: "1 year",
		}

		activity, err := service.CreateActivity(context.Background(), req)
		require.NoError(t, err)

		check, err := service.ValidateCompliance(context.Background(), activity.ID)
		require.NoError(t, err)
		assert.Contains(t, check.Warnings, "Activity has never been reviewed")
	})
}

func TestROPAService_GenerateROPAReport(t *testing.T) {
	_, service := setupROPATest()

	t.Run("generates complete report", func(t *testing.T) {
		// Create multiple activities
		for i := 0; i < 3; i++ {
			req := &ProcessingActivityRequest{
				Name:       "Activity " + string(rune(i)),
				Purpose:    "Report testing",
				LegalBasis: LegalBasisConsent,
			}
			_, err := service.CreateActivity(context.Background(), req)
			require.NoError(t, err)
		}

		report, err := service.GenerateROPAReport(context.Background(), "Fairforge LLC")
		require.NoError(t, err)
		assert.Equal(t, "Fairforge LLC", report.OrganizationName)
		assert.GreaterOrEqual(t, report.TotalActivities, 3)
		assert.NotZero(t, report.GeneratedAt)
		assert.NotZero(t, report.NextReviewDue)
	})
}

func TestROPAService_GetROPAStats(t *testing.T) {
	_, service := setupROPATest()

	t.Run("retrieves statistics", func(t *testing.T) {
		// Create activities with different legal bases
		bases := []string{LegalBasisConsent, LegalBasisContract, LegalBasisConsent}
		for _, basis := range bases {
			req := &ProcessingActivityRequest{
				Name:       "Stats Test",
				Purpose:    "Testing stats",
				LegalBasis: basis,
			}
			_, err := service.CreateActivity(context.Background(), req)
			require.NoError(t, err)
		}

		stats, err := service.GetROPAStats(context.Background())
		require.NoError(t, err)
		assert.GreaterOrEqual(t, stats.TotalActivities, 3)
		assert.NotEmpty(t, stats.ActivitiesByLegalBasis)
		assert.NotEmpty(t, stats.ActivitiesByStatus)
	})
}

func TestROPAService_ConcurrentAccess(t *testing.T) {
	_, service := setupROPATest()

	t.Run("handles concurrent activity creation", func(t *testing.T) {
		var wg sync.WaitGroup
		numGoroutines := 10

		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := &ProcessingActivityRequest{
					Name:       "Concurrent Test",
					Purpose:    "Concurrency testing",
					LegalBasis: LegalBasisConsent,
				}
				_, err := service.CreateActivity(context.Background(), req)
				assert.NoError(t, err)
			}()
		}

		wg.Wait()

		// Verify all were created
		activities, err := service.ListActivities(context.Background(), nil)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(activities), numGoroutines)
	})
}
