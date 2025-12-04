// internal/integrations/integration_test.go
package integrations

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &IntegrationConfig{
			ID:       "slack-notify",
			Name:     "Slack Notifications",
			Type:     IntegrationTypeWebhook,
			Endpoint: "https://hooks.slack.com/services/xxx",
			Enabled:  true,
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty ID", func(t *testing.T) {
		config := &IntegrationConfig{Name: "test"}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "id")
	})

	t.Run("rejects invalid type", func(t *testing.T) {
		config := &IntegrationConfig{
			ID:   "test",
			Name: "test",
			Type: "invalid",
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "type")
	})
}

func TestNewIntegrationManager(t *testing.T) {
	t.Run("creates manager", func(t *testing.T) {
		manager := NewIntegrationManager(nil)
		assert.NotNil(t, manager)
	})
}

func TestIntegrationManager_Register(t *testing.T) {
	manager := NewIntegrationManager(nil)

	t.Run("registers integration", func(t *testing.T) {
		config := &IntegrationConfig{
			ID:       "test-integration",
			Name:     "Test",
			Type:     IntegrationTypeWebhook,
			TenantID: "tenant-1",
			Enabled:  true,
		}

		err := manager.Register(config)
		assert.NoError(t, err)

		integration := manager.Get("test-integration")
		assert.NotNil(t, integration)
		assert.Equal(t, "Test", integration.Name)
	})

	t.Run("rejects duplicate ID", func(t *testing.T) {
		config := &IntegrationConfig{
			ID:   "dup-id",
			Name: "First",
			Type: IntegrationTypeWebhook,
		}
		_ = manager.Register(config)

		err := manager.Register(config)
		assert.Error(t, err)
	})
}

func TestIntegrationManager_Unregister(t *testing.T) {
	manager := NewIntegrationManager(nil)

	t.Run("unregisters integration", func(t *testing.T) {
		config := &IntegrationConfig{ID: "to-remove", Name: "Remove", Type: IntegrationTypeWebhook}
		_ = manager.Register(config)

		err := manager.Unregister("to-remove")
		assert.NoError(t, err)

		assert.Nil(t, manager.Get("to-remove"))
	})

	t.Run("returns error for unknown", func(t *testing.T) {
		err := manager.Unregister("unknown")
		assert.Error(t, err)
	})
}

func TestIntegrationManager_ListByTenant(t *testing.T) {
	manager := NewIntegrationManager(nil)

	_ = manager.Register(&IntegrationConfig{ID: "int-1", Name: "One", Type: IntegrationTypeWebhook, TenantID: "tenant-1"})
	_ = manager.Register(&IntegrationConfig{ID: "int-2", Name: "Two", Type: IntegrationTypeWebhook, TenantID: "tenant-1"})
	_ = manager.Register(&IntegrationConfig{ID: "int-3", Name: "Three", Type: IntegrationTypeWebhook, TenantID: "tenant-2"})

	t.Run("lists by tenant", func(t *testing.T) {
		integrations := manager.ListByTenant("tenant-1")
		assert.Len(t, integrations, 2)
	})
}

func TestWebhookIntegration(t *testing.T) {
	received := make(chan map[string]interface{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&data)
		received <- data
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	manager := NewIntegrationManager(nil)

	t.Run("sends webhook", func(t *testing.T) {
		config := &IntegrationConfig{
			ID:       "webhook-test",
			Name:     "Webhook",
			Type:     IntegrationTypeWebhook,
			Endpoint: server.URL,
			Enabled:  true,
		}
		_ = manager.Register(config)

		event := &IntegrationEvent{
			Type:    "object.created",
			Payload: map[string]interface{}{"bucket": "test", "key": "file.txt"},
		}

		err := manager.Trigger(context.Background(), "webhook-test", event)
		assert.NoError(t, err)

		select {
		case data := <-received:
			assert.Equal(t, "object.created", data["type"])
		case <-time.After(time.Second):
			t.Fatal("webhook not received")
		}
	})

	t.Run("skips disabled integration", func(t *testing.T) {
		config := &IntegrationConfig{
			ID:       "disabled-webhook",
			Name:     "Disabled",
			Type:     IntegrationTypeWebhook,
			Endpoint: server.URL,
			Enabled:  false,
		}
		_ = manager.Register(config)

		event := &IntegrationEvent{Type: "test"}
		err := manager.Trigger(context.Background(), "disabled-webhook", event)
		assert.NoError(t, err) // No error, just skipped
	})
}

func TestCustomHandler(t *testing.T) {
	manager := NewIntegrationManager(nil)

	handlerCalled := false
	customHandler := func(ctx context.Context, event *IntegrationEvent) error {
		handlerCalled = true
		return nil
	}

	t.Run("executes custom handler", func(t *testing.T) {
		config := &IntegrationConfig{
			ID:      "custom-handler",
			Name:    "Custom",
			Type:    IntegrationTypeCustom,
			Enabled: true,
		}
		_ = manager.Register(config)
		manager.SetHandler("custom-handler", customHandler)

		event := &IntegrationEvent{Type: "custom.event"}
		err := manager.Trigger(context.Background(), "custom-handler", event)
		assert.NoError(t, err)
		assert.True(t, handlerCalled)
	})
}

func TestS3EventIntegration(t *testing.T) {
	manager := NewIntegrationManager(nil)

	t.Run("formats S3 event", func(t *testing.T) {
		event := NewS3Event("ObjectCreated:Put", "my-bucket", "path/to/file.txt", 1024)
		assert.Equal(t, "s3:ObjectCreated:Put", event.Type)
		assert.Equal(t, "my-bucket", event.Payload["bucket"])
		assert.Equal(t, "path/to/file.txt", event.Payload["key"])
		assert.Equal(t, int64(1024), event.Payload["size"])
	})

	t.Run("triggers on S3 event type", func(t *testing.T) {
		received := make(chan bool, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			received <- true
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		config := &IntegrationConfig{
			ID:         "s3-events",
			Name:       "S3 Events",
			Type:       IntegrationTypeWebhook,
			Endpoint:   server.URL,
			Enabled:    true,
			EventTypes: []string{"s3:ObjectCreated:*"},
		}
		_ = manager.Register(config)

		event := NewS3Event("ObjectCreated:Put", "bucket", "key", 100)
		err := manager.TriggerByEventType(context.Background(), "tenant-1", event)
		assert.NoError(t, err)
	})
}

func TestIntegrationAuth(t *testing.T) {
	manager := NewIntegrationManager(nil)

	t.Run("adds auth header", func(t *testing.T) {
		receivedAuth := ""
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		config := &IntegrationConfig{
			ID:       "auth-test",
			Name:     "Auth Test",
			Type:     IntegrationTypeWebhook,
			Endpoint: server.URL,
			Enabled:  true,
			Auth: &AuthConfig{
				Type:  AuthTypeBearer,
				Token: "secret-token-123",
			},
		}
		_ = manager.Register(config)

		event := &IntegrationEvent{Type: "test"}
		_ = manager.Trigger(context.Background(), "auth-test", event)

		assert.Equal(t, "Bearer secret-token-123", receivedAuth)
	})

	t.Run("adds basic auth", func(t *testing.T) {
		receivedAuth := ""
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		config := &IntegrationConfig{
			ID:       "basic-auth-test",
			Name:     "Basic Auth Test",
			Type:     IntegrationTypeWebhook,
			Endpoint: server.URL,
			Enabled:  true,
			Auth: &AuthConfig{
				Type:     AuthTypeBasic,
				Username: "user",
				Password: "pass",
			},
		}
		_ = manager.Register(config)

		event := &IntegrationEvent{Type: "test"}
		_ = manager.Trigger(context.Background(), "basic-auth-test", event)

		assert.Contains(t, receivedAuth, "Basic")
	})
}

func TestIntegrationRetry(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	manager := NewIntegrationManager(&ManagerConfig{
		MaxRetries: 3,
		RetryDelay: 10 * time.Millisecond,
	})

	t.Run("retries on failure", func(t *testing.T) {
		config := &IntegrationConfig{
			ID:       "retry-test",
			Name:     "Retry Test",
			Type:     IntegrationTypeWebhook,
			Endpoint: server.URL,
			Enabled:  true,
		}
		_ = manager.Register(config)

		event := &IntegrationEvent{Type: "test"}
		err := manager.Trigger(context.Background(), "retry-test", event)
		assert.NoError(t, err)
		assert.Equal(t, 3, attempts)
	})
}

func TestIntegrationTypes(t *testing.T) {
	t.Run("defines integration types", func(t *testing.T) {
		assert.Equal(t, "webhook", IntegrationTypeWebhook)
		assert.Equal(t, "lambda", IntegrationTypeLambda)
		assert.Equal(t, "sns", IntegrationTypeSNS)
		assert.Equal(t, "sqs", IntegrationTypeSQS)
		assert.Equal(t, "custom", IntegrationTypeCustom)
	})
}

func TestAuthTypes(t *testing.T) {
	t.Run("defines auth types", func(t *testing.T) {
		assert.Equal(t, "none", AuthTypeNone)
		assert.Equal(t, "basic", AuthTypeBasic)
		assert.Equal(t, "bearer", AuthTypeBearer)
		assert.Equal(t, "api_key", AuthTypeAPIKey)
	})
}

func TestIntegrationStatus(t *testing.T) {
	manager := NewIntegrationManager(nil)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	config := &IntegrationConfig{
		ID:       "status-test",
		Name:     "Status Test",
		Type:     IntegrationTypeWebhook,
		Endpoint: server.URL,
		Enabled:  true,
	}
	_ = manager.Register(config)

	event := &IntegrationEvent{Type: "test"}
	_ = manager.Trigger(context.Background(), "status-test", event)

	t.Run("tracks integration status", func(t *testing.T) {
		status := manager.GetStatus("status-test")
		require.NotNil(t, status)
		assert.True(t, status.LastSuccess.After(time.Time{}))
		assert.Equal(t, int64(1), status.SuccessCount)
	})
}
