package compliance

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Mock database for testing
type mockPortabilityDB struct {
	mu       sync.Mutex
	requests map[uuid.UUID]*PortabilityRequest
	users    map[uuid.UUID]*User
	apiKeys  map[uuid.UUID][]*APIKey
	files    map[uuid.UUID][]*FileMetadata
}

func newMockPortabilityDB() *mockPortabilityDB {
	return &mockPortabilityDB{
		requests: make(map[uuid.UUID]*PortabilityRequest),
		users:    make(map[uuid.UUID]*User),
		apiKeys:  make(map[uuid.UUID][]*APIKey),
		files:    make(map[uuid.UUID][]*FileMetadata),
	}
}

func (m *mockPortabilityDB) CreatePortabilityRequest(ctx context.Context, req *PortabilityRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[req.ID] = req
	return nil
}

func (m *mockPortabilityDB) GetPortabilityRequest(ctx context.Context, id uuid.UUID) (*PortabilityRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	req, ok := m.requests[id]
	if !ok {
		return nil, ErrNotFound
	}
	return req, nil
}

func (m *mockPortabilityDB) UpdatePortabilityRequest(ctx context.Context, req *PortabilityRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[req.ID] = req
	return nil
}

func (m *mockPortabilityDB) ListPortabilityRequests(ctx context.Context, userID uuid.UUID) ([]*PortabilityRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var results []*PortabilityRequest
	for _, req := range m.requests {
		if req.UserID == userID {
			results = append(results, req)
		}
	}
	return results, nil
}

func (m *mockPortabilityDB) GetUser(ctx context.Context, userID uuid.UUID) (*User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	user, ok := m.users[userID]
	if !ok {
		return nil, ErrNotFound
	}
	return user, nil
}

func (m *mockPortabilityDB) ListAPIKeys(ctx context.Context, userID uuid.UUID) ([]*APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.apiKeys[userID], nil
}

func (m *mockPortabilityDB) GetUsageRecords(ctx context.Context, userID uuid.UUID) ([]UsageRecord, error) {
	return []UsageRecord{
		{
			Date:          time.Now().AddDate(0, 0, -1),
			BytesStored:   1024 * 1024 * 100, // 100MB
			BytesTransfer: 1024 * 1024 * 50,  // 50MB
			APIRequests:   150,
		},
	}, nil
}

func (m *mockPortabilityDB) ListFiles(ctx context.Context, userID uuid.UUID) ([]*FileMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.files[userID], nil
}

func (m *mockPortabilityDB) ListContainers(ctx context.Context, userID uuid.UUID) ([]*Container, error) {
	return []*Container{
		{
			ID:        uuid.New(),
			UserID:    userID,
			Name:      "my-bucket",
			CreatedAt: time.Now().AddDate(0, -1, 0),
		},
	}, nil
}

// Mock storage backend
type mockStorage struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMockStorage() *mockStorage {
	return &mockStorage{
		data: make(map[string][]byte),
	}
}

func (m *mockStorage) Put(ctx context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = data
	return nil
}

func (m *mockStorage) Get(ctx context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.data[key]
	if !ok {
		return nil, ErrNotFound
	}
	return data, nil
}

func (m *mockStorage) GeneratePresignedURL(ctx context.Context, key string, duration time.Duration) (string, error) {
	return "https://example.com/exports/" + key + "?expires=7d", nil
}

func TestPortabilityService_CreateExportRequest(t *testing.T) {
	db := newMockPortabilityDB()
	storage := newMockStorage()
	service := NewPortabilityService(db, storage, zap.NewNop())

	userID := uuid.New()

	t.Run("creates export request successfully", func(t *testing.T) {
		req, err := service.CreateExportRequest(context.Background(), userID, "json")
		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, req.ID)
		assert.Equal(t, userID, req.UserID)
		assert.Equal(t, "json", req.Format)
		assert.Equal(t, StatusPending, req.Status)
		assert.False(t, req.ExpiresAt.IsZero())

		// Wait a bit for goroutine to finish
		time.Sleep(100 * time.Millisecond)
	})

	t.Run("validates user ID", func(t *testing.T) {
		_, err := service.CreateExportRequest(context.Background(), uuid.Nil, "json")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "user_id required")
	})

	t.Run("validates format", func(t *testing.T) {
		_, err := service.CreateExportRequest(context.Background(), userID, "invalid")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid format")
	})

	t.Run("accepts valid formats", func(t *testing.T) {
		formats := []string{"json", "archive", "s3"}
		for _, format := range formats {
			_, err := service.CreateExportRequest(context.Background(), userID, format)
			assert.NoError(t, err)
		}

		// Wait for goroutines
		time.Sleep(100 * time.Millisecond)
	})
}

func TestPortabilityService_ExportJSON(t *testing.T) {
	db := newMockPortabilityDB()
	storage := newMockStorage()
	service := NewPortabilityService(db, storage, zap.NewNop())

	userID := uuid.New()

	// Set up mock data
	db.mu.Lock()
	db.users[userID] = &User{
		ID:        userID,
		Email:     "test@example.com",
		Name:      "Test User",
		CreatedAt: time.Now().AddDate(-1, 0, 0),
	}

	db.apiKeys[userID] = []*APIKey{
		{
			ID:        uuid.New(),
			UserID:    userID,
			Name:      "Test Key",
			Key:       "vk_1234567890abcdef",
			CreatedAt: time.Now().AddDate(0, -1, 0),
		},
	}

	db.files[userID] = []*FileMetadata{
		{
			ID:          uuid.New(),
			UserID:      userID,
			Path:        "documents/test.pdf",
			Size:        1024 * 1024,
			Container:   "my-bucket",
			ContentType: "application/pdf",
			CreatedAt:   time.Now().AddDate(0, 0, -7),
			ModifiedAt:  time.Now().AddDate(0, 0, -7),
		},
	}
	db.mu.Unlock()

	t.Run("exports user data as JSON", func(t *testing.T) {
		url, err := service.exportJSON(context.Background(), userID)
		require.NoError(t, err)
		assert.NotEmpty(t, url)
		assert.Contains(t, url, "exports")

		// Verify JSON was created
		storage.mu.Lock()
		var foundKey string
		for key := range storage.data {
			if key != "" {
				foundKey = key
				break
			}
		}
		assert.NotEmpty(t, foundKey)

		// Parse and validate JSON
		var export UserDataExport
		err = json.Unmarshal(storage.data[foundKey], &export)
		storage.mu.Unlock()

		require.NoError(t, err)
		assert.Equal(t, "json", export.Format)
		assert.Equal(t, "1.0", export.Version)
		assert.NotNil(t, export.PersonalData)
		assert.Equal(t, "test@example.com", export.PersonalData.Email)
		assert.Len(t, export.APIKeys, 1)
		assert.Contains(t, export.APIKeys[0].Masked, "****")
		assert.Len(t, export.Files, 1)
		assert.Equal(t, "documents/test.pdf", export.Files[0].Path)
	})
}

func TestPortabilityService_GetExportRequest(t *testing.T) {
	db := newMockPortabilityDB()
	storage := newMockStorage()
	service := NewPortabilityService(db, storage, zap.NewNop())

	userID := uuid.New()
	requestID := uuid.New()

	// Create a request
	req := &PortabilityRequest{
		ID:        requestID,
		UserID:    userID,
		Status:    StatusReady,
		ExportURL: "https://example.com/export.json",
	}
	db.mu.Lock()
	db.requests[requestID] = req
	db.mu.Unlock()

	t.Run("retrieves existing request", func(t *testing.T) {
		retrieved, err := service.GetExportRequest(context.Background(), requestID)
		require.NoError(t, err)
		assert.Equal(t, requestID, retrieved.ID)
		assert.Equal(t, StatusReady, retrieved.Status)
		assert.Equal(t, "https://example.com/export.json", retrieved.ExportURL)
	})

	t.Run("returns error for non-existent request", func(t *testing.T) {
		_, err := service.GetExportRequest(context.Background(), uuid.New())
		assert.Error(t, err)
	})
}

func TestMaskKey(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "masks long key",
			input:    "vk_1234567890abcdef",
			expected: "vk_1****cdef",
		},
		{
			name:     "masks short key",
			input:    "short",
			expected: "****",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := maskKey(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGenerateTempCredentials(t *testing.T) {
	t.Run("generates valid access key", func(t *testing.T) {
		key := generateTempAccessKey()
		assert.NotEmpty(t, key)
		assert.Contains(t, key, "TEMP")
	})

	t.Run("generates valid secret key", func(t *testing.T) {
		key := generateTempSecretKey()
		assert.NotEmpty(t, key)
		// Should be valid UUID format
		_, err := uuid.Parse(key)
		assert.NoError(t, err)
	})
}
